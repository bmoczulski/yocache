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

// redirectRootToUI handles the bare "/" so a browser visitor lands on the
// dashboard rather than a blob-store 404. Registered as "GET /{$}" so it
// only matches the exact path — anything with a path segment still routes
// through the catch-all blob handler.
func redirectRootToUI(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/ui/", http.StatusMovedPermanently)
}
