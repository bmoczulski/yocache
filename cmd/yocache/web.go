package main

import (
	"embed"
	"io/fs"
	"net/http"
)

// Built Svelte SPA lives under web/dist (produced by ./build-web.sh, or
// npm ci && npm run build inside cmd/yocache/web/). Missing or empty at
// compile time is an error by design — we never ship a dashboard-less binary.
//
//go:embed all:web/dist
var webDistFS embed.FS

// webUIHandler serves the built dashboard at /ui/*. Wired up before the
// blob catch-all in main.go so paths like /sstate/*, /downloads/*, and
// /machine/*/... fall through untouched.
func webUIHandler() http.Handler {
	sub, err := fs.Sub(webDistFS, "web/dist")
	if err != nil {
		// //go:embed above guarantees web/dist is present, so Sub cannot fail.
		panic(err)
	}
	return http.StripPrefix("/ui", http.FileServer(http.FS(sub)))
}

// redirectToUI sends a browser visitor of "/" or "/ui" to the dashboard
// rather than a blob-store 404. Registered as "GET /{$}" and "GET /ui", so it
// only matches those exact paths — anything else still routes through the
// catch-all blob handler.
//
// Location is relative and set by hand: http.Redirect would resolve it
// against the request path into an absolute "/ui/", dropping any prefix a
// reverse proxy stripped before forwarding (e.g. /cache1 -> /cache1/ui/).
// Both "/" and "/ui" resolve "ui/" to the same <prefix>/ui/.
func redirectToUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Location", "ui/")
	w.WriteHeader(http.StatusMovedPermanently)
}
