package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testUploader(t *testing.T, kind string) *blobUploader {
	t.Helper()
	u, err := newBlobUploader(t.TempDir(), kind, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("newBlobUploader: %v", err)
	}
	return u
}

func TestSafeBlobName(t *testing.T) {
	cases := map[string]bool{
		"git2_example.tar.gz":                    true, // flat download
		"66/b6/sstate:v86d:x-poky-linux:h_t.zst": true, // nested sstate, colons and all
		"66/b6/sstate:v86d:h_t.tar.zst.siginfo":  true, // sidecar: mid-name dots are fine
		"66/b6/sstate:v86d:h_t.tar.zst.sig":      true, // sidecar (signed sstate)
		"":                  false,
		"/etc/passwd":       false, // leading slash -> empty first segment
		"../escape":         false, // ".." segment
		"66/../../etc/pwn":  false, // traversal mid-path
		"a//b":              false, // empty middle segment
		"trailing/":         false, // empty last segment
		".hidden":           false, // dotfile (staging-invariant)
		"66/.staging.token": false, // dotfile segment
	}
	for name, want := range cases {
		if got := safeBlobName(name); got != want {
			t.Errorf("safeBlobName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestBlobPutGetRoundTrip(t *testing.T) {
	u := testUploader(t, "sstate")
	// A real nested sstate name: <aa>/<bb>/sstate:<spec>:<unihash>_<task>.tar.zst.
	name := "66/b6/sstate:v86d:qemux86_64-poky-linux:0.1.10:r0:qemux86_64:14:" +
		"66b661d93ad58574c83267db8b48d961effd62348a49dbad3da33ee8378b983e_deploy_source_date_epoch.tar.zst"
	body := "BLOBDATA"

	put := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/sstate/"+name, strings.NewReader(body))
		req.ContentLength = int64(len(body))
		u.put(rec, req)
		return rec
	}
	if rec := put(); rec.Code != http.StatusCreated {
		t.Fatalf("put status = %d, want 201", rec.Code)
	}
	// Re-upload is idempotent (atomic rename over the existing blob).
	if rec := put(); rec.Code != http.StatusCreated {
		t.Fatalf("re-put status = %d, want 201", rec.Code)
	}

	// GET returns the bytes.
	rec := httptest.NewRecorder()
	u.get(rec, httptest.NewRequest(http.MethodGet, "/sstate/"+name, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != body {
		t.Errorf("get body = %q, want %q", got, body)
	}

	// HEAD returns headers but no body (bitbake HEADs before fetching).
	rec = httptest.NewRecorder()
	u.get(rec, httptest.NewRequest(http.MethodHead, "/sstate/"+name, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("head status = %d, want 200", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("head body = %q, want empty", rec.Body.String())
	}

	// A path that was never uploaded is a miss -> 404 (void fallback).
	rec = httptest.NewRecorder()
	u.get(rec, httptest.NewRequest(http.MethodGet, "/sstate/66/b6/absent.tar.zst", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get miss status = %d, want 404", rec.Code)
	}
}

func TestPutRejectsTraversal(t *testing.T) {
	u := testUploader(t, "sstate")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/sstate/placeholder", strings.NewReader("evil"))
	// Set the path directly to a traversal that an encoded "%2f" would smuggle
	// past ServeMux's path cleaning — safeBlobName is the backstop.
	req.URL.Path = "/sstate/../../../etc/pwn"
	u.put(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("traversal put status = %d, want 400", rec.Code)
	}
}
