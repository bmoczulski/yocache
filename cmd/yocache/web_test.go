package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The redirect must stay relative so a reverse proxy that strips a path
// prefix (/cache1/ -> /) still lands the browser on /cache1/ui/.
func TestRedirectToUIIsRelative(t *testing.T) {
	for _, path := range []string{"/", "/ui"} {
		rec := httptest.NewRecorder()
		redirectToUI(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusMovedPermanently {
			t.Errorf("%s: status = %d, want %d", path, rec.Code, http.StatusMovedPermanently)
		}
		if got := rec.Header().Get("Location"); got != "ui/" {
			t.Errorf("%s: Location = %q, want %q", path, got, "ui/")
		}
	}
}
