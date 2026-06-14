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
	"sync/atomic"
)

// quotaTracker tracks disk usage across all blob stores and enforces an
// operator-configured storage limit. limit == 0 means unlimited.
//
// reserve() atomically claims space via a CAS retry loop — two concurrent
// uploads cannot both succeed when only one fits. The counter is seeded from
// a directory walk at startup and stays accurate thereafter.
type quotaTracker struct {
	limit int64
	used  atomic.Int64
}

// Used returns the current tracked usage in bytes.
func (q *quotaTracker) Used() int64 { return q.used.Load() }

// seed walks dirs and initialises the usage counter from actual on-disk sizes.
// Call once after all stores are created, before the server starts accepting
// requests. Staging dotfiles are excluded.
func (q *quotaTracker) seed(dirs ...string) {
	var total int64
	for _, dir := range dirs {
		_ = filepath.WalkDir(dir, func(_ string, e os.DirEntry, err error) error {
			if err != nil || e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				return nil
			}
			if fi, err := e.Info(); err == nil {
				total += fi.Size()
			}
			return nil
		})
	}
	q.used.Store(total)
}

// reserve atomically claims net bytes of quota space using a CAS retry loop.
// Returns false (and leaves used unchanged) if adding net would exceed the
// limit. Always updates used — even when quota is unlimited — so release()
// never produces a negative counter and used always reflects actual disk usage.
// Callers must call release(net) if the upload subsequently fails.
func (q *quotaTracker) reserve(net int64) bool {
	if net <= 0 {
		return true
	}
	if q.limit == 0 {
		q.used.Add(net)
		return true
	}
	for {
		cur := q.used.Load()
		if cur+net > q.limit {
			return false
		}
		if q.used.CompareAndSwap(cur, cur+net) {
			return true
		}
		// CAS lost to a concurrent update; retry with the fresh value.
	}
}

// release returns n bytes to the quota. Call on upload failure after a
// successful reserve to keep the counter accurate.
func (q *quotaTracker) release(n int64) {
	if n > 0 {
		q.used.Add(-n)
	}
}

// blobUploader is the write side of the cache for one path space (the build-side
// counterpart is meta-yocache/lib/yocache/uploader.py). A PUT /<kind>/<name>
// streams the request body to <dir>/<name>.
//
// It is crash- and reader-safe by construction. The body is written to a private
// staging directory (<dir>/.uploads/<randomToken>/<basename>) and only atomically
// rename(2)d onto the final name once the whole payload has landed and been
// fsync'd. Consequences:
//
//   - A reader never observes a partial blob: it sees either no file or the
//     complete one.  The .uploads subtree is unreachable via the HTTP API
//     (safeBlobName rejects names with a leading dot).
//   - A client that disconnects mid-upload leaves only a subdirectory under
//     .uploads, removed immediately on the failure path here and, as a backstop
//     for a hard kill, wiped entirely at startup by wipeUploadStaging.
//   - Two builds uploading the same artifact concurrently each get their own
//     randomToken subdirectory, so they can't trample each other; whichever
//     renames last wins, and either way the published file is whole.
//   - Staging in a subdirectory of dir (same filesystem as the final path) keeps
//     the rename atomic and avoids appending a suffix to a name that may already
//     be near the filesystem's 255-byte limit.
type blobUploader struct {
	dir       string           // blob store directory (e.g. the --downloads path)
	uploadDir string           // staging area: dir/.uploads; per-request tmpDirs live here
	kind      string           // leading path segment, e.g. "downloads"; stripped to get the name
	log       *slog.Logger
	ledger    *Ledger          // mutation events: artifact.added, artifact.evicted
	accessLog *Ledger          // access events: artifact.fetched, artifact.missed
	quota     *quotaTracker    // shared across all stores; enforces total storage limit
	inventory *blobInventory   // per-blob metadata for eviction ordering; nil disables tracking
	eviction  *EvictionManager // policy chain consulted when quota is full; nil disables eviction
}

// newBlobUploader prepares the on-disk store for one path space and returns its
// handler: it creates dir and wipes the staging area left behind by any uploads
// a previous run didn't finish (the startup backstop to put's per-request
// cleanup). A bad dir is returned as an error — the caller treats it as fatal,
// since upload to that path space would otherwise be permanently broken.
func newBlobUploader(dir, kind string, log *slog.Logger, ledger, accessLog *Ledger, quota *quotaTracker, inv *blobInventory, eviction *EvictionManager) (*blobUploader, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create %s store dir %q: %w", kind, dir, err)
	}
	uploadDir := filepath.Join(dir, ".uploads")
	if err := wipeUploadStaging(uploadDir, log); err != nil {
		return nil, fmt.Errorf("wipe %s staging dir %q: %w", kind, uploadDir, err)
	}
	log.Info(kind+" store ready", "path", dir)
	return &blobUploader{dir: dir, uploadDir: uploadDir, kind: kind, log: log, ledger: ledger, accessLog: accessLog, quota: quota, inventory: inv, eviction: eviction}, nil
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
	// Content-Length is mandatory: our uploader always provides it (uploader.py
	// stats the file before sending), and without it quota accounting is impossible.
	if r.ContentLength < 0 {
		u.log.Warn("upload: missing Content-Length", "kind", u.kind, "name", name, "remote", r.RemoteAddr)
		http.Error(w, "Content-Length required", http.StatusLengthRequired)
		return
	}
	// If-None-Match: * means "only create if absent". Check before doing any
	// filesystem work so we don't race to mkdir for a blob we'll reject.
	existingSize := int64(0)
	if stored, statErr := os.Stat(final); statErr == nil {
		existingSize = stored.Size()
		if stored.Size() != r.ContentLength {
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
			// Same size: assume identical (sstate URLs encode the unihash, DL names
			// are stable) and skip — no need to re-transfer what we already hold.
			u.log.Info("upload: already exists, skipping",
				"kind", u.kind, "name", name, "remote", r.RemoteAddr)
			if u.inventory != nil {
				if err := u.inventory.Touch(u.kind, name); err != nil {
					u.log.Warn("upload: already exists: inventory touch failed",
						"kind", u.kind, "name", name, "err", err)
				}
			}
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
	}

	// Atomically claim quota space for the net bytes this upload adds to the store
	// (net accounts for replacements: a growing VCS tarball contributes only the
	// delta). Early exits before the defer is registered must release manually.
	net := r.ContentLength - existingSize

	// Fast-fail before any I/O: a blob larger than the entire quota can never fit
	// regardless of eviction — refuse without destroying cached data.
	if u.quota.limit > 0 && net > u.quota.limit {
		u.log.Warn("upload: blob exceeds total quota, refusing without eviction",
			"kind", u.kind, "name", name,
			"quota_bytes", u.quota.limit, "incoming_bytes", r.ContentLength,
			"remote", r.RemoteAddr)
		u.ledger.RecordQuotaExceeded(u.kind, name, u.quota.limit, u.quota.Used(), r.ContentLength)
		http.Error(w, "storage quota exceeded", http.StatusInsufficientStorage)
		return
	}

	reserved := u.quota.reserve(net)
	for !reserved {
		// Recompute deficit each iteration: a concurrent upload may steal
		// freshly-freed space, and each eviction round should only request what
		// is still missing rather than the original net bytes.
		deficit := u.quota.Used() + net - u.quota.limit
		freed, err := u.eviction.TryFree(deficit)
		if err != nil {
			u.log.Warn("upload: eviction error", "kind", u.kind, "name", name, "err", err)
		}
		if freed == 0 {
			break // store exhausted or no eviction policy configured
		}
		reserved = u.quota.reserve(net)
	}
	if !reserved {
		u.log.Warn("upload: quota exceeded",
			"kind", u.kind, "name", name,
			"quota_bytes", u.quota.limit, "used_bytes", u.quota.Used(),
			"incoming_bytes", r.ContentLength, "remote", r.RemoteAddr)
		u.ledger.RecordQuotaExceeded(u.kind, name, u.quota.limit, u.quota.Used(), r.ContentLength)
		http.Error(w, "storage quota exceeded", http.StatusInsufficientStorage)
		return
	}

	// A nested name (sstate's <aa>/<bb>/<file>) needs its parent created before
	// we can rename into it. filepath.Join cleaned the name, and safeBlobName
	// guaranteed it can't climb out of u.dir, so this only ever makes dirs under
	// the store.
	parent := filepath.Dir(final)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		u.quota.release(net)
		u.log.Error("upload: cannot create blob dir", "dir", parent, "err", err, "remote", r.RemoteAddr)
		http.Error(w, "cannot stage upload", http.StatusInternalServerError)
		return
	}

	// Stage in a private subdirectory of u.uploadDir (which lives inside u.dir,
	// same filesystem as final) so the rename is atomic. Each upload gets its own
	// random subdirectory; the file inside carries the plain basename with no
	// extra suffix, avoiding any 255-byte filename limit overflow for long sstate
	// names.
	tmpDir := filepath.Join(u.uploadDir, randomToken())
	if err := os.Mkdir(tmpDir, 0o755); err != nil {
		u.quota.release(net)
		u.log.Error("upload: cannot create staging dir", "dir", tmpDir, "err", err, "remote", r.RemoteAddr)
		http.Error(w, "cannot stage upload", http.StatusInternalServerError)
		return
	}
	tmp := filepath.Join(tmpDir, filepath.Base(name))
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		_ = os.Remove(tmpDir)
		u.quota.release(net)
		u.log.Error("upload: cannot create staging file", "tmp", tmp, "err", err, "remote", r.RemoteAddr)
		http.Error(w, "cannot stage upload", http.StatusInternalServerError)
		return
	}

	// Until the rename commits, every exit path must clean up the staging dir —
	// a client disconnect mid-copy arrives here as an io.Copy error.
	// release(net) undoes the quota reservation on any failure; on success
	// reserve() already credited the bytes so no further adjustment is needed.
	committed := false
	defer func() {
		f.Close()
		if !committed {
			if err := os.RemoveAll(tmpDir); err != nil {
				u.log.Warn("upload: staging dir not cleaned up", "dir", tmpDir, "err", err)
			}
			u.quota.release(net)
		} else {
			// tmpDir is empty after the rename; remove it.  Failure is cosmetic —
			// startup wipe clears any leftovers next time.
			_ = os.Remove(tmpDir)
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
	if n != r.ContentLength {
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

	machine := r.Header.Get("X-BitBake-var-MACHINE")
	distro := r.Header.Get("X-BitBake-var-DISTRO")
	buildName := r.Header.Get("X-BitBake-var-BUILDNAME")

	logAttrs := []any{"kind", u.kind, "name", name, "bytes", n, "remote", r.RemoteAddr}
	if machine != "" {
		logAttrs = append(logAttrs, "machine", machine)
	}
	if distro != "" {
		logAttrs = append(logAttrs, "distro", distro)
	}
	u.log.Info("cache upload stored", logAttrs...)
	u.ledger.RecordArtifactAdded(u.kind, name, n, "", machine, distro, buildName)
	if u.inventory != nil {
		if err := u.inventory.Upsert(u.kind, name, n); err != nil {
			u.log.Warn("upload: inventory upsert failed", "kind", u.kind, "name", name, "err", err)
		}
	}
	w.WriteHeader(http.StatusCreated)
}

// serveBlob serves a single blob by pre-parsed name. machine and buildName come
// from the identity path prefix (empty for direct /sstate/ or /downloads/ requests).
func (u *blobUploader) serveBlob(w http.ResponseWriter, r *http.Request, name, machine, buildName string) {
	if !safeBlobName(name) {
		u.miss(w, r, name, machine, buildName)
		return
	}
	// safeBlobName guaranteed name can't climb out of u.dir, so this stays inside
	// the store. A staging dotfile can't be served: its basename starts with "."
	// which safeBlobName already rejected.
	f, err := os.Open(filepath.Join(u.dir, name))
	if err != nil {
		u.miss(w, r, name, machine, buildName) // absent or unreadable — both are cache misses
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		u.miss(w, r, name, machine, buildName)
		return
	}

	// It's a blob, not text/HTML to sniff; say so up front so ServeContent skips
	// content sniffing. bitbake ignores the type, but octet-stream is correct.
	w.Header().Set("Content-Type", "application/octet-stream")
	logAttrs := []any{"kind", u.kind, "name", name, "bytes", fi.Size(), "method", r.Method, "remote", r.RemoteAddr}
	if machine != "" {
		logAttrs = append(logAttrs, "machine", machine)
	}
	u.log.Info("cache hit", logAttrs...)
	u.accessLog.RecordArtifactFetched(u.kind, name, "", machine, buildName)
	if u.inventory != nil {
		if err := u.inventory.Touch(u.kind, name); err != nil {
			u.log.Warn("cache hit: inventory touch failed", "kind", u.kind, "name", name, "err", err)
		}
	}
	http.ServeContent(w, r, fi.Name(), fi.ModTime(), f)
}

// miss logs a lookup that found nothing and returns 404 — the "void" outcome
// that lets bitbake fall back to upstream. Mirrors the catch-all's log shape so
// hits and misses read alike.
func (u *blobUploader) miss(w http.ResponseWriter, r *http.Request, name, machine, buildName string) {
	logAttrs := []any{"kind", u.kind, "name", name, "method", r.Method, "ua", r.UserAgent(), "remote", r.RemoteAddr}
	if machine != "" {
		logAttrs = append(logAttrs, "machine", machine)
	}
	u.log.Info("cache miss", logAttrs...)
	u.accessLog.RecordArtifactMissed(u.kind, name, "", machine, buildName)
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

// parseIdentityPath parses a request URL path that may carry an optional
// key/value identity prefix before the kind segment. Two URL forms are accepted:
//
//	/<kind>/<blob-path>                              (no identity)
//	/key1/val1/key2/val2/.../<kind>/<blob-path>      (identity prefix)
//
// The kind sentinel is the first path segment that equals "sstate" or
// "downloads". Everything before it is the identity k/v list (must be an even
// number of segments); everything after is the blob name. ok is false for
// malformed inputs (odd-length identity list, missing kind, empty blob name).
func parseIdentityPath(path string) (identity map[string]string, kind, blobName string, ok bool) {
	// Strip leading slash and split into segments.
	path = strings.TrimPrefix(path, "/")
	segs := strings.SplitN(path, "/", -1)

	// Find the kind sentinel.
	kindIdx := -1
	for i, s := range segs {
		if s == "sstate" || s == "downloads" {
			kindIdx = i
			break
		}
	}
	if kindIdx < 0 {
		return nil, "", "", false
	}

	// Identity k/v pairs precede the kind; must be even count.
	prefix := segs[:kindIdx]
	if len(prefix)%2 != 0 {
		return nil, "", "", false
	}
	identity = make(map[string]string, len(prefix)/2)
	for i := 0; i < len(prefix); i += 2 {
		identity[prefix[i]] = prefix[i+1]
	}

	kind = segs[kindIdx]
	blobName = strings.Join(segs[kindIdx+1:], "/")
	if blobName == "" {
		return nil, "", "", false
	}
	return identity, kind, blobName, true
}

// safeBlobName accepts a relative path that may be nested and rejects anything
// that could escape the store or break the staging invariant. sstate is not a
// flat namespace: bitbake lays it out as <hash[:2]>/<hash[2:4]>/<file> and
// SSTATE_MIRRORS fetches it at that exact path, so the upload must preserve the
// subdirectories (downloads stay single-segment and pass this unchanged).
//
// Every segment must be a plain name: no "" (empty — leading/trailing/double
// slash), and no leading dot, which also rules out "." and ".." (traversal).
// The leading-dot rule keeps .uploads/ (the staging subtree) unreachable via
// the HTTP API, so GET can never serve a partial blob.
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

// wipeUploadStaging removes dir wholesale and recreates it empty — the startup
// backstop for staging subdirs left behind by a hard kill or crash that skipped
// the per-request defer in put. Wiping the whole tree is safe because every
// entry under dir is a randomToken subdirectory owned by exactly one in-flight
// upload; nothing under here is meant to survive a restart.
func wipeUploadStaging(dir string, log *slog.Logger) error {
	entries, _ := os.ReadDir(dir) // ignore error — dir may not exist yet
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if len(entries) > 0 {
		log.Info("upload staging: wiped stale staging dirs", "dir", dir, "count", len(entries))
	}
	return nil
}
