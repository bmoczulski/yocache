package main

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
)

func testUploader(t *testing.T, kind string) *blobUploader {
	t.Helper()
	u, err := newBlobUploader(t.TempDir(), kind, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("newBlobUploader: %v", err)
	}
	return u
}

// putReq builds a PUT request with If-None-Match: * already set.
func putReq(t *testing.T, kind, name, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/"+kind+"/"+name, strings.NewReader(body))
	req.ContentLength = int64(len(body))
	req.Header.Set("If-None-Match", "*")
	return req
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
		u.put(rec, putReq(t, "sstate", name, body))
		return rec
	}
	if rec := put(); rec.Code != http.StatusCreated {
		t.Fatalf("put status = %d, want 201", rec.Code)
	}
	// Re-upload of the same name is rejected: server already has the blob.
	if rec := put(); rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("re-put status = %d, want 412", rec.Code)
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

// TestPutRequiresIfNoneMatch verifies that a PUT without the If-None-Match
// header is rejected with 428 Precondition Required (RFC 6585 §3).
func TestPutRequiresIfNoneMatch(t *testing.T) {
	u := testUploader(t, "downloads")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/downloads/blob.tar.gz", strings.NewReader("data"))
	req.ContentLength = 4
	// Deliberately omit If-None-Match.
	u.put(rec, req)
	if rec.Code != http.StatusPreconditionRequired {
		t.Fatalf("status = %d, want 428", rec.Code)
	}
}

// TestPutIfNoneMatchExistingBlob verifies that uploading a name that already
// exists returns 412 Precondition Failed and leaves the stored blob untouched.
func TestPutIfNoneMatchExistingBlob(t *testing.T) {
	u := testUploader(t, "sstate")
	name := "ab/cd/sstate:quilt-native:x86_64:1.0:r0:x86_64:14:abcd_do_compile.tar.zst"
	original := "original content"

	rec := httptest.NewRecorder()
	u.put(rec, putReq(t, "sstate", name, original))
	if rec.Code != http.StatusCreated {
		t.Fatalf("initial put status = %d, want 201", rec.Code)
	}

	// Second PUT with If-None-Match: * must be rejected.
	rec = httptest.NewRecorder()
	u.put(rec, putReq(t, "sstate", name, "different content"))
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("re-put status = %d, want 412", rec.Code)
	}

	// The stored blob must be unchanged.
	got, err := os.ReadFile(filepath.Join(u.dir, name))
	if err != nil {
		t.Fatalf("reading stored blob: %v", err)
	}
	if string(got) != original {
		t.Errorf("stored blob = %q, want %q", got, original)
	}
}

// TestPutInterruptedDiscardsPartial verifies that a client disconnecting
// mid-upload leaves no staging dotfile and no partial blob committed.
func TestPutInterruptedDiscardsPartial(t *testing.T) {
	u := testUploader(t, "downloads")
	name := "big.tar.gz"

	// A reader that delivers a few bytes then returns an error, simulating a
	// dropped connection.
	body := io.MultiReader(
		strings.NewReader("partial"),
		iotest.ErrReader(errors.New("connection reset by peer")),
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/downloads/"+name, body)
	req.Header.Set("If-None-Match", "*")
	req.ContentLength = 1024 // promise more bytes than we'll deliver
	u.put(rec, req)

	if rec.Code == http.StatusCreated {
		t.Fatal("interrupted upload must not return 201")
	}

	// Walk the whole store: no dotfile staging remnant should remain.
	err := filepath.WalkDir(u.dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasPrefix(d.Name(), ".") {
			t.Errorf("staging file not cleaned up: %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking store: %v", err)
	}

	// The final blob must not have been committed.
	if _, err := os.Stat(filepath.Join(u.dir, name)); err == nil {
		t.Error("partial blob must not be committed to the store")
	}
}
