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
	"sync"
	"testing"
	"testing/iotest"
)

func testUploader(t *testing.T, kind string) *blobUploader {
	t.Helper()
	return testUploaderWithQuota(t, kind, 0)
}

func testUploaderWithQuota(t *testing.T, kind string, limit int64) *blobUploader {
	t.Helper()
	qt := &quotaTracker{limit: limit}
	u, err := newBlobUploader(t.TempDir(), kind, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, qt)
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

	// Second PUT of the same bytes must be rejected with 412 (already have it).
	// Use identical content so the size matches and we get 412, not 409 —
	// the conflict path (size mismatch) is covered by TestPutConflictSizeMismatch.
	rec = httptest.NewRecorder()
	u.put(rec, putReq(t, "sstate", name, original))
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

// TestPutConflictSizeMismatch verifies that a PUT whose Content-Length differs
// from a stored blob of the same name returns 409 Conflict for plain downloads
// (content-addressed by URL, so a size mismatch is a real conflict).
func TestPutConflictSizeMismatch(t *testing.T) {
	u := testUploader(t, "downloads")
	name := "busybox-1.36.1.tar.bz2" // plain download, not a git mirror tarball
	original := "original content"

	rec := httptest.NewRecorder()
	u.put(rec, putReq(t, "downloads", name, original))
	if rec.Code != http.StatusCreated {
		t.Fatalf("initial put status = %d, want 201", rec.Code)
	}

	// Re-upload with a different Content-Length must be flagged as a conflict.
	rec = httptest.NewRecorder()
	u.put(rec, putReq(t, "downloads", name, "different — longer content"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("conflict put status = %d, want 409", rec.Code)
	}

	// The stored blob must be unchanged.
	got, err := os.ReadFile(filepath.Join(u.dir, name))
	if err != nil {
		t.Fatalf("reading stored blob: %v", err)
	}
	if string(got) != original {
		t.Errorf("stored blob = %q, want %q (conflict must not overwrite)", got, original)
	}
}

// TestPutGrowingVCSTarballLargerReplaces verifies that VCS mirror tarballs
// whose names do not encode a revision (git2_*, gitshallow_*, hg_*, repo_*)
// accept a larger upload and replace the stored snapshot.
func TestPutGrowingVCSTarballLargerReplaces(t *testing.T) {
	names := []string{
		"git2_github.com.poky.tar.gz",
		"gitshallow_github.com.poky.tar.gz",
		"hg_mymodule_hg.example.com_.repos.project.tar.gz",
		"repo_android.googlesource.com.platform.manifest_default.xml_main.tar.gz",
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			u := testUploader(t, "downloads")
			older := "older and shorter"
			newer := "newer and definitely longer content"

			rec := httptest.NewRecorder()
			u.put(rec, putReq(t, "downloads", name, older))
			if rec.Code != http.StatusCreated {
				t.Fatalf("initial put status = %d, want 201", rec.Code)
			}

			rec = httptest.NewRecorder()
			u.put(rec, putReq(t, "downloads", name, newer))
			if rec.Code != http.StatusCreated {
				t.Fatalf("larger re-put status = %d, want 201", rec.Code)
			}

			got, err := os.ReadFile(filepath.Join(u.dir, name))
			if err != nil {
				t.Fatalf("reading stored blob: %v", err)
			}
			if string(got) != newer {
				t.Errorf("stored blob = %q, want newer content %q", got, newer)
			}
		})
	}
}

// TestPutGrowingVCSTarballSmallerConflicts verifies that a growing-VCS tarball
// that is smaller than the stored one returns 409: a shrinking repo is
// suspicious (force-push / history rewrite) and must not replace a more
// complete snapshot.
func TestPutGrowingVCSTarballSmallerConflicts(t *testing.T) {
	u := testUploader(t, "downloads")
	name := "git2_github.com.poky.tar.gz"
	larger := "a longer blob that was stored first"

	rec := httptest.NewRecorder()
	u.put(rec, putReq(t, "downloads", name, larger))
	if rec.Code != http.StatusCreated {
		t.Fatalf("initial put status = %d, want 201", rec.Code)
	}

	rec = httptest.NewRecorder()
	u.put(rec, putReq(t, "downloads", name, "short"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("smaller re-put status = %d, want 409", rec.Code)
	}

	got, err := os.ReadFile(filepath.Join(u.dir, name))
	if err != nil {
		t.Fatalf("reading stored blob: %v", err)
	}
	if string(got) != larger {
		t.Errorf("stored blob = %q, want original larger content", got)
	}
}

// TestPutContentAddressedVCSTarballConflicts verifies that VCS tarballs whose
// names embed a revision (svn, perforce, clearcase) are treated as
// content-addressed and any size mismatch is still a 409 Conflict.
func TestPutContentAddressedVCSTarballConflicts(t *testing.T) {
	// These filenames mirror the real bitbake output patterns. The revision
	// is part of the name, so same-name + different-size = genuine conflict.
	names := []string{
		"busybox_git.buildroot.net_git_busybox.git_abc1234_0.tar.gz", // svn pattern
		"depot.example.com_.tools_mymod_12345.tar.gz",                // perforce pattern
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			u := testUploader(t, "downloads")
			original := "original content"

			rec := httptest.NewRecorder()
			u.put(rec, putReq(t, "downloads", name, original))
			if rec.Code != http.StatusCreated {
				t.Fatalf("initial put status = %d, want 201", rec.Code)
			}

			rec = httptest.NewRecorder()
			u.put(rec, putReq(t, "downloads", name, "different — longer content"))
			if rec.Code != http.StatusConflict {
				t.Fatalf("re-put status = %d, want 409", rec.Code)
			}
		})
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

// --- quotaTracker unit tests ---

func TestQuotaUnlimited(t *testing.T) {
	q := &quotaTracker{limit: 0}
	for range 3 {
		if !q.reserve(1 << 30) {
			t.Fatal("unlimited quota must always reserve")
		}
	}
}

func TestQuotaReserveFitsExactly(t *testing.T) {
	q := &quotaTracker{limit: 100}
	if !q.reserve(100) {
		t.Fatal("reserve exactly at limit should succeed")
	}
	if got := q.Used(); got != 100 {
		t.Errorf("used = %d, want 100", got)
	}
}

func TestQuotaReserveExceedsLimit(t *testing.T) {
	q := &quotaTracker{limit: 100}
	if q.reserve(101) {
		t.Fatal("reserve over limit must fail")
	}
	if got := q.Used(); got != 0 {
		t.Errorf("used = %d after failed reserve, want 0", got)
	}
}

func TestQuotaReservePartialThenFull(t *testing.T) {
	q := &quotaTracker{limit: 10}
	q.reserve(7)
	// 7 used, 3 remaining — 4 bytes should be refused
	if q.reserve(4) {
		t.Error("reserve that would exceed limit must fail")
	}
	if got := q.Used(); got != 7 {
		t.Errorf("used = %d, want 7 (failed reserve must not change counter)", got)
	}
}

func TestQuotaReleaseRestoresSpace(t *testing.T) {
	q := &quotaTracker{limit: 10}
	q.reserve(10)
	q.release(10)
	if got := q.Used(); got != 0 {
		t.Errorf("used = %d after release, want 0", got)
	}
	if !q.reserve(10) {
		t.Error("quota should be fully available after release")
	}
}

func TestQuotaSeed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "blob"), []byte("1234567890"), 0o644); err != nil {
		t.Fatal(err)
	}
	q := &quotaTracker{limit: 100}
	q.seed(dir)
	if got := q.Used(); got != 10 {
		t.Errorf("seeded used = %d, want 10", got)
	}
}

func TestQuotaSeedExcludesDotfiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "blob"), []byte("hello"), 0o644)
	os.WriteFile(filepath.Join(dir, ".staging.tmp"), []byte("partial"), 0o644)
	q := &quotaTracker{limit: 100}
	q.seed(dir)
	if got := q.Used(); got != 5 {
		t.Errorf("seeded used = %d, want 5 (dotfiles excluded)", got)
	}
}

// --- PUT integration tests ---

func TestPutRequiresContentLength(t *testing.T) {
	u := testUploader(t, "downloads")
	req := httptest.NewRequest(http.MethodPut, "/downloads/blob.tar.gz", strings.NewReader("data"))
	req.Header.Set("If-None-Match", "*")
	req.ContentLength = -1
	rec := httptest.NewRecorder()
	u.put(rec, req)
	if rec.Code != http.StatusLengthRequired {
		t.Fatalf("status = %d, want 411", rec.Code)
	}
}

func TestPutQuotaExceeded(t *testing.T) {
	u := testUploaderWithQuota(t, "downloads", 4)
	rec := httptest.NewRecorder()
	u.put(rec, putReq(t, "downloads", "big.tar.gz", "12345")) // 5 bytes > 4 limit
	if rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("status = %d, want 507", rec.Code)
	}
	if got := u.quota.Used(); got != 0 {
		t.Errorf("used = %d after rejected upload, want 0", got)
	}
}

func TestPutQuotaFitsUpdatesCounter(t *testing.T) {
	u := testUploaderWithQuota(t, "downloads", 10)
	rec := httptest.NewRecorder()
	u.put(rec, putReq(t, "downloads", "small.tar.gz", "hello")) // 5 bytes
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	if got := u.quota.Used(); got != 5 {
		t.Errorf("used = %d after upload, want 5", got)
	}
}

func TestPutQuotaReleasedOnInterrupt(t *testing.T) {
	u := testUploaderWithQuota(t, "downloads", 1024)
	body := io.MultiReader(
		strings.NewReader("partial"),
		iotest.ErrReader(errors.New("connection reset by peer")),
	)
	req := httptest.NewRequest(http.MethodPut, "/downloads/big.tar.gz", body)
	req.Header.Set("If-None-Match", "*")
	req.ContentLength = 512
	rec := httptest.NewRecorder()
	u.put(rec, req)
	if rec.Code == http.StatusCreated {
		t.Fatal("interrupted upload must not return 201")
	}
	if got := u.quota.Used(); got != 0 {
		t.Errorf("used = %d after interrupted upload, want 0 (quota must be released)", got)
	}
}

func TestPutQuotaConcurrentExclusion(t *testing.T) {
	// Two concurrent uploads of 6 bytes each against a 10-byte quota — exactly
	// one must succeed. The CAS loop in reserve() ensures they can't both pass.
	const payload = "123456" // 6 bytes
	qt := &quotaTracker{limit: 10}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		codes   []int
	)
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Each goroutine gets its own store dir so they don't collide on
			// the filename; the shared quota is the only contention point.
			u, err := newBlobUploader(
				t.TempDir(), "downloads",
				slog.New(slog.NewTextHandler(io.Discard, nil)),
				nil, nil, qt,
			)
			if err != nil {
				t.Errorf("newBlobUploader: %v", err)
				return
			}
			rec := httptest.NewRecorder()
			u.put(rec, putReq(t, "downloads", "blob.tar.gz", payload))
			mu.Lock()
			codes = append(codes, rec.Code)
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	created := 0
	for _, code := range codes {
		switch code {
		case http.StatusCreated:
			created++
		case http.StatusInsufficientStorage:
			// expected for the loser
		default:
			t.Errorf("unexpected status %d (want 201 or 507)", code)
		}
	}
	if created != 1 {
		t.Errorf("exactly 1 upload should succeed, got %d; codes: %v", created, codes)
	}
	if got := qt.Used(); got != int64(len(payload)) {
		t.Errorf("used = %d after concurrent uploads, want %d", got, len(payload))
	}
}
