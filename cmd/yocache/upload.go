package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// blobUploader is the write side of the cache for one path space (the build-side
// counterpart is meta-yocache/lib/yocache/uploader.py). A PUT /<kind>/<name>
// streams the request body to <dir>/<name>.
//
// It is crash- and reader-safe by construction. The body is written to a hidden
// staging file — a leading "." plus a random suffix — and only atomically
// rename(2)d onto the final name once the whole payload has landed and been
// fsync'd. Consequences:
//
//   - A reader (the future GET side, today still the void handler) never observes
//     a partial blob: it sees either no file or the complete one. The leading dot
//     also keeps staging files out of any directory listing a reader walks.
//   - A client that disconnects mid-upload leaves only a dotfile, removed
//     immediately on the failure path here and, as a backstop for a hard kill,
//     swept at startup by sweepTempUploads.
//   - Two builds uploading the same artifact concurrently each get their own
//     random-suffixed staging file, so they can't trample each other; whichever
//     renames last wins, and either way the published file is whole.
type blobUploader struct {
	dir       string       // blob store directory (e.g. the --downloads path)
	kind      string       // leading path segment, e.g. "downloads"; stripped to get the name
	log       *slog.Logger
	ledger    *Ledger // mutation events: artifact.added, artifact.evicted
	accessLog *Ledger // access events: artifact.fetched, artifact.missed
}

// newBlobUploader prepares the on-disk store for one path space and returns its
// handler: it creates dir and sweeps any staging files left behind by an upload
// an earlier run didn't finish (the startup backstop to put's per-request
// cleanup). A bad dir is returned as an error — the caller treats it as fatal,
// since upload to that path space would otherwise be permanently broken.
func newBlobUploader(dir, kind string, log *slog.Logger, ledger, accessLog *Ledger) (*blobUploader, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create %s store dir %q: %w", kind, dir, err)
	}
	sweepTempUploads(dir, log)
	log.Info(kind+" store ready", "path", dir)
	return &blobUploader{dir: dir, kind: kind, log: log, ledger: ledger, accessLog: accessLog}, nil
}

// put handles PUT /<kind>/<name>.
func (u *blobUploader) put(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/"+u.kind+"/")
	if !safeBlobName(name) {
		u.log.Warn("upload: rejected name", "kind", u.kind, "path", r.URL.Path, "remote", r.RemoteAddr)
		http.Error(w, "invalid artifact name", http.StatusBadRequest)
		return
	}
	final := filepath.Join(u.dir, name)

	// Require If-None-Match so callers declare their intent (RFC 6585 §3 /
	// RFC 7232 §6). Accepting unconditional PUTs would risk silently overwriting
	// a complete blob — wrong for a content-addressed store where URL presence
	// already implies identity (sstate URLs encode the unihash).
	if r.Header.Get("If-None-Match") == "" {
		u.log.Warn("upload: missing If-None-Match",
			"kind", u.kind, "name", name, "remote", r.RemoteAddr)
		http.Error(w, "If-None-Match required", http.StatusPreconditionRequired)
		return
	}
	// If-None-Match: * means "only create if absent". Check before doing any
	// filesystem work so we don't race to mkdir for a blob we'll reject.
	if stored, statErr := os.Stat(final); statErr == nil {
		if r.ContentLength >= 0 && stored.Size() != r.ContentLength {
			if r.ContentLength > stored.Size() && isGrowingVCSTarball(name) {
				// VCS mirror tarballs whose names don't encode a revision grow
				// monotonically as the upstream repository accumulates history.
				// A larger incoming file is a fresher snapshot — let it fall
				// through and replace the stored one.
				u.log.Info("upload: replacing with larger VCS mirror tarball",
					"kind", u.kind, "name", name,
					"stored_bytes", stored.Size(), "incoming_bytes", r.ContentLength,
					"remote", r.RemoteAddr)
			} else {
				// All other size mismatches are a conflict: two objects
				// claiming the same identity, or a VCS tarball that is
				// inexplicably smaller than what we already hold.
				u.log.Warn("upload: conflict — size mismatch",
					"kind", u.kind, "name", name,
					"stored_bytes", stored.Size(), "incoming_bytes", r.ContentLength,
					"remote", r.RemoteAddr)
				http.Error(w, "conflict: stored blob has different size", http.StatusConflict)
				return
			}
		} else {
			// Same size (or unknown incoming length): assume identical and skip.
			u.log.Info("upload: already exists, skipping",
				"kind", u.kind, "name", name, "remote", r.RemoteAddr)
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
	}

	// A nested name (sstate's <aa>/<bb>/<file>) needs its parent created before
	// we can stage there. filepath.Join cleaned the name, and safeBlobName
	// guaranteed it can't climb out of u.dir, so this only ever makes dirs under
	// the store.
	parent := filepath.Dir(final)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		u.log.Error("upload: cannot create blob dir", "dir", parent, "err", err, "remote", r.RemoteAddr)
		http.Error(w, "cannot stage upload", http.StatusInternalServerError)
		return
	}

	// Stage in the SAME directory as the final file so the rename is
	// same-filesystem (hence atomic). The leading dot goes on the basename (not
	// some ancestor segment) so the staging file sits beside its target and the
	// dotfile invariant/sweep still apply.
	tmp := filepath.Join(parent, "."+filepath.Base(name)+"."+randomToken())
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		u.log.Error("upload: cannot create staging file", "tmp", tmp, "err", err, "remote", r.RemoteAddr)
		http.Error(w, "cannot stage upload", http.StatusInternalServerError)
		return
	}

	// Until the rename commits, every exit path must take the staging file with
	// it — a client disconnect mid-copy arrives here as an io.Copy error.
	committed := false
	defer func() {
		f.Close()
		if !committed {
			if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
				u.log.Warn("upload: leftover staging file not removed", "tmp", tmp, "err", err)
			}
		}
	}()

	n, err := io.Copy(f, r.Body)
	if err != nil {
		u.log.Warn("upload: body copy failed (client gone?)",
			"kind", u.kind, "name", name, "bytes", n, "err", err, "remote", r.RemoteAddr)
		http.Error(w, "upload interrupted", http.StatusBadRequest)
		return
	}
	// A short-but-clean stream (the client promised more via Content-Length than
	// it sent) must not be published as a complete blob.
	if r.ContentLength >= 0 && n != r.ContentLength {
		u.log.Warn("upload: short body",
			"kind", u.kind, "name", name, "got", n, "want", r.ContentLength, "remote", r.RemoteAddr)
		http.Error(w, "incomplete upload", http.StatusBadRequest)
		return
	}
	// Flush data to disk before the rename. rename(2) is atomic for the name, but
	// without this an ext4-style crash could make the rename durable while the
	// data blocks are not — exposing a named-but-empty file to a future reader.
	if err := f.Sync(); err != nil {
		u.log.Error("upload: fsync failed", "tmp", tmp, "err", err, "remote", r.RemoteAddr)
		http.Error(w, "cannot persist upload", http.StatusInternalServerError)
		return
	}
	// Atomic publish. rename(2) replaces the name in one step so a reader
	// never observes a partially-written file (the If-None-Match check above
	// prevents us reaching this point for blobs that already exist).
	if err := os.Rename(tmp, final); err != nil {
		u.log.Error("upload: rename failed", "tmp", tmp, "final", final, "err", err, "remote", r.RemoteAddr)
		http.Error(w, "cannot publish upload", http.StatusInternalServerError)
		return
	}
	committed = true

	u.log.Info("cache upload stored", "kind", u.kind, "name", name, "bytes", n, "remote", r.RemoteAddr)
	u.ledger.RecordArtifactAdded(u.kind, name, n, "")
	w.WriteHeader(http.StatusCreated)
}

// get handles GET (and HEAD — Go's ServeMux routes HEAD to a GET handler)
// /<kind>/<name>: serve the stored blob, or 404 on a miss so bitbake's mirror
// fetch falls back to upstream and the build still completes. http.ServeContent
// does the rest from the open file — HEAD (headers, no body), Range, the
// Content-Length/Last-Modified headers, and conditional requests — which matters
// because bitbake HEADs an sstate object before it GETs it.
func (u *blobUploader) get(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/"+u.kind+"/")
	if !safeBlobName(name) {
		u.miss(w, r, name)
		return
	}
	// safeBlobName guaranteed name can't climb out of u.dir, so this stays inside
	// the store. A staging dotfile can't be served: its basename starts with "."
	// which safeBlobName already rejected.
	f, err := os.Open(filepath.Join(u.dir, name))
	if err != nil {
		u.miss(w, r, name) // absent or unreadable — both are cache misses
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		u.miss(w, r, name)
		return
	}

	// It's a blob, not text/HTML to sniff; say so up front so ServeContent skips
	// content sniffing. bitbake ignores the type, but octet-stream is correct.
	w.Header().Set("Content-Type", "application/octet-stream")
	u.log.Info("cache hit", "kind", u.kind, "name", name, "bytes", fi.Size(),
		"method", r.Method, "remote", r.RemoteAddr)
	u.accessLog.RecordArtifactFetched(u.kind, name, "")
	http.ServeContent(w, r, fi.Name(), fi.ModTime(), f)
}

// miss logs a lookup that found nothing and returns 404 — the "void" outcome
// that lets bitbake fall back to upstream. Mirrors the catch-all's log shape so
// hits and misses read alike.
func (u *blobUploader) miss(w http.ResponseWriter, r *http.Request, name string) {
	u.log.Info("cache miss", "kind", u.kind, "name", name,
		"method", r.Method, "ua", r.UserAgent(), "remote", r.RemoteAddr)
	u.accessLog.RecordArtifactMissed(u.kind, name, "")
	http.NotFound(w, r)
}

// isGrowingVCSTarball reports whether name is a VCS mirror tarball whose
// content grows monotonically as the upstream repository accumulates history,
// meaning a larger incoming file is a fresher snapshot and should replace a
// smaller stored one rather than being treated as a conflict.
//
// Growing (no revision in filename — URL-derived name only):
//   - git2_*        bitbake git fetcher, full bare-clone tarball
//   - gitshallow_*  bitbake git fetcher, shallow-clone tarball
//   - hg_*          bitbake hg fetcher; same structure and guard as git
//   - repo_*        Android repo fetcher; includes branch, not a pinned hash
//
// NOT growing (revision is embedded in the filename — content-addressed):
//   - svn:      <module>_<host>_<path>_<revision>_<pegrev>.tar.gz
//   - perforce: <host>_<path>_<module>_<revision>.tar.gz
//   - clearcase: <identifier>.tar.gz (identifier includes label/revision)
//
// The "not growing" assumption for svn/perforce/clearcase is based on source
// inspection; real-world builds may yet prove it wrong — treat any 409 reports
// for those as a signal to revisit.
func isGrowingVCSTarball(name string) bool {
	for _, pfx := range []string{"git2_", "gitshallow_", "hg_", "repo_"} {
		if strings.HasPrefix(name, pfx) {
			return true
		}
	}
	return false
}

// safeBlobName accepts a relative path that may be nested and rejects anything
// that could escape the store or break the staging invariant. sstate is not a
// flat namespace: bitbake lays it out as <hash[:2]>/<hash[2:4]>/<file> and
// SSTATE_MIRRORS fetches it at that exact path, so the upload must preserve the
// subdirectories (downloads stay single-segment and pass this unchanged).
//
// Every segment must be a plain name: no "" (empty — leading/trailing/double
// slash), and no leading dot, which also rules out "." and ".." (traversal).
// The leading-dot rule keeps the invariant that *every* dotfile under a blob dir
// is dead staging state, which is what lets sweepTempUploads remove them blindly.
func safeBlobName(name string) bool {
	if name == "" {
		return false
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" || strings.HasPrefix(seg, ".") {
			return false
		}
	}
	return true
}

// randomToken returns a short random hex string used to make staging filenames
// unique. crypto/rand.Read effectively never fails on Linux; if it somehow did,
// the zero token still works — the O_EXCL open just fails on the (astronomically
// unlikely) collision and that one upload returns 500, never corrupting a blob.
func randomToken() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// sweepTempUploads removes leftover staging files (leading-dot blobs from an
// interrupted upload) anywhere under dir. The per-request defer in put handles
// the normal disconnect case; this is the startup backstop for a hard kill or
// crash that skipped it, keeping the "every dotfile is dead staging state"
// invariant true. It walks recursively because sstate stages into nested
// <aa>/<bb>/ subdirs, not just the top level.
func sweepTempUploads(dir string, log *slog.Logger) {
	removed := 0
	err := filepath.WalkDir(dir, func(p string, e os.DirEntry, err error) error {
		if err != nil {
			log.Warn("upload sweep: cannot walk", "path", p, "err", err)
			return nil // skip this entry, keep sweeping the rest
		}
		if e.IsDir() || !strings.HasPrefix(e.Name(), ".") {
			return nil
		}
		if err := os.Remove(p); err != nil {
			log.Warn("upload sweep: cannot remove", "path", p, "err", err)
			return nil
		}
		removed++
		return nil
	})
	if err != nil {
		log.Warn("upload sweep: walk failed", "dir", dir, "err", err)
	}
	if removed > 0 {
		log.Info("upload sweep: removed stale staging files", "dir", dir, "count", removed)
	}
}
