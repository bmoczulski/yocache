package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRecipeBlockListBlocked(t *testing.T) {
	bl := newRecipeBlockList([]string{"quilt", "broken-recipe"})

	cases := []struct {
		kind string
		name string
		want bool
	}{
		// blocked recipe, nested sstate path
		{"sstate", "ab/cd/sstate:quilt:cortexa53-poky-linux:0.67:r0:cortexa53:14:abcd_do_compile.tar.zst", true},
		// blocked hyphenated recipe name
		{"sstate", "ab/cd/sstate:broken-recipe:x86_64-poky-linux:1.0:r0:x86_64:14:abcd_do_install.tar.zst", true},
		// flat sstate path (no subdirectory prefix)
		{"sstate", "sstate:quilt:cortexa53-poky-linux:0.67:r0:cortexa53:14:abcd_do_compile.tar.zst", true},
		// siginfo sidecar of a blocked recipe
		{"sstate", "ab/cd/sstate:quilt:cortexa53-poky-linux:0.67:r0:cortexa53:14:abcd.tar.zst.siginfo", true},
		// non-blocked recipe
		{"sstate", "ab/cd/sstate:init-ifupdown:qemux86_64-poky-linux:1.0:r0:qemux86_64:14:abcd.tar.zst", false},
		// downloads kind never blocked, even if filename would match
		{"downloads", "sstate:quilt:cortexa53-poky-linux:0.67:r0:cortexa53:14:abcd_do_compile.tar.zst", false},
		// non-sstate filename in sstate store
		{"sstate", "ab/cd/somefile.tar.gz", false},
	}
	for _, c := range cases {
		t.Run(c.kind+":"+c.name, func(t *testing.T) {
			if got := bl.blocked(c.kind, c.name); got != c.want {
				t.Errorf("blocked(%q, %q) = %v, want %v", c.kind, c.name, got, c.want)
			}
		})
	}
}

func TestRecipeBlockListEmpty(t *testing.T) {
	bl := newRecipeBlockList(nil)
	if bl.blocked("sstate", "ab/cd/sstate:quilt:any:arch.tar.zst") {
		t.Error("empty block list must never block")
	}
}

func TestRecipeBlockListTrimsWhitespace(t *testing.T) {
	bl := newRecipeBlockList([]string{"  quilt  ", "\tbroken-recipe\t"})
	if !bl.blocked("sstate", "ab/cd/sstate:quilt:any:0.67:arch.tar.zst") {
		t.Error("leading/trailing whitespace in recipe name must be trimmed on insert")
	}
}

// TestBlockedRecipeHTTP verifies that the block list guard wired into the blob
// handler returns 403 Forbidden for blocked recipes and does not call through to
// the actual uploader. This mirrors the guard in main.go exactly.
func TestBlockedRecipeHTTP(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	bl := newRecipeBlockList([]string{"quilt"})
	u := testUploader(t, "sstate")
	blobStores := map[string]*blobUploader{"sstate": u}

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, kind, blobName, ok := parseIdentityPath(r.URL.Path)
		if ok && bl.blocked(kind, blobName) {
			log.Warn("blocked recipe", "kind", kind, "name", blobName)
			http.Error(w, "recipe blocked", http.StatusForbidden)
			return
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			blobStores[kind].serveBlob(w, r, blobName, "", "")
		case http.MethodPut:
			blobStores[kind].put(w, r)
		}
	})

	blobName := "ab/cd/sstate:quilt:cortexa53-poky-linux:0.67:r0:cortexa53:14:abcd_do_compile.tar.zst"
	allowedBlobName := "ab/cd/sstate:curl::8.19.0:r0::14:abcd_patch.tar.zst"

	for _, tc := range []struct {
		method   string
		path     string
		wantCode int
	}{
		{http.MethodGet, "/sstate/" + blobName, http.StatusForbidden},
		{http.MethodHead, "/sstate/" + blobName, http.StatusForbidden},
		{http.MethodPut, "/sstate/" + blobName, http.StatusForbidden},
		// non-blocked recipe must not be affected
		{http.MethodGet, "/sstate/" + allowedBlobName, http.StatusNotFound},
	} {
		t.Run(tc.method+"_"+tc.path, func(t *testing.T) {
			var body io.Reader
			req := httptest.NewRequest(tc.method, tc.path, body)
			if tc.method == http.MethodPut {
				req = httptest.NewRequest(tc.method, tc.path, strings.NewReader("data"))
				req.ContentLength = 4
				req.Header.Set("If-None-Match", "*")
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.wantCode {
				t.Errorf("%s %s: status = %d, want %d", tc.method, tc.path, rec.Code, tc.wantCode)
			}
		})
	}
}
