package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// --- EvictionManager tests ---

type stubPolicy struct {
	name   string
	toFree int64
	called bool
}

func (s *stubPolicy) Name() string { return s.name }
func (s *stubPolicy) Evict(_ int64) (int64, error) {
	s.called = true
	return s.toFree, nil
}

func TestEvictionManagerNil(t *testing.T) {
	var m *EvictionManager
	freed, err := m.TryFree(100)
	if err != nil {
		t.Errorf("nil manager: unexpected error: %v", err)
	}
	if freed != 0 {
		t.Errorf("nil manager: freed = %d, want 0", freed)
	}
}

func TestEvictionManagerEmpty(t *testing.T) {
	m := &EvictionManager{log: slog.Default()}
	freed, err := m.TryFree(100)
	if err != nil {
		t.Errorf("empty manager: unexpected error: %v", err)
	}
	if freed != 0 {
		t.Errorf("empty manager: freed = %d, want 0", freed)
	}
}

func TestEvictionManagerSinglePolicy(t *testing.T) {
	p := &stubPolicy{name: "stub", toFree: 200}
	m := &EvictionManager{policies: []EvictionPolicy{p}, log: slog.Default()}
	freed, err := m.TryFree(100)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if freed != 200 {
		t.Errorf("freed = %d, want 200", freed)
	}
}

func TestEvictionManagerChained(t *testing.T) {
	// First policy frees 30, second frees 80 — together they cover 100.
	p1 := &stubPolicy{name: "first", toFree: 30}
	p2 := &stubPolicy{name: "second", toFree: 80}
	m := &EvictionManager{policies: []EvictionPolicy{p1, p2}, log: slog.Default()}
	freed, err := m.TryFree(100)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if freed != 110 {
		t.Errorf("freed = %d, want 110", freed)
	}
	if !p1.called || !p2.called {
		t.Errorf("expected both policies called: p1=%v p2=%v", p1.called, p2.called)
	}
}

func TestEvictionManagerEarlyExit(t *testing.T) {
	// First policy frees enough — second must not be called.
	p1 := &stubPolicy{name: "first", toFree: 150}
	p2 := &stubPolicy{name: "second", toFree: 100}
	m := &EvictionManager{policies: []EvictionPolicy{p1, p2}, log: slog.Default()}
	m.TryFree(100)
	if p2.called {
		t.Error("second policy called even though first already freed enough")
	}
}

// --- LRUPolicy tests ---

func newTestLRUPolicy(t *testing.T, storeDir string, qt *quotaTracker) (*LRUPolicy, *blobInventory) {
	t.Helper()
	inv, _ := newTestInventory(t)
	p := &LRUPolicy{
		inventory: inv,
		stores:    map[string]string{"sstate": storeDir},
		quota:     qt,
		ledger:    nil, // nil ledger is a no-op
		log:       slog.Default(),
	}
	return p, inv
}

func writeTestBlob(t *testing.T, dir, name string, content []byte) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestLRUPolicyEvictBasic(t *testing.T) {
	dir := t.TempDir()
	qt := &quotaTracker{limit: 10000}

	writeTestBlob(t, dir, "a.tgz", make([]byte, 100))
	writeTestBlob(t, dir, "b.tgz", make([]byte, 200))
	writeTestBlob(t, dir, "c.tgz", make([]byte, 300))

	p, inv := newTestLRUPolicy(t, dir, qt)
	_, db := newTestInventory(t) // only used via inv returned above — reuse the same inv
	_ = db

	// Seed inventory: a is oldest, then b, then c.
	insertBlobAt(t, inv.db, "sstate", "a.tgz", 100, 100)
	insertBlobAt(t, inv.db, "sstate", "b.tgz", 200, 200)
	insertBlobAt(t, inv.db, "sstate", "c.tgz", 300, 300)
	qt.used.Store(600)

	// Need 250 bytes — LRU evicts a (100) then b (200) = 300 freed.
	freed, err := p.Evict(250)
	if err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if freed != 300 {
		t.Errorf("freed = %d, want 300", freed)
	}
	if qt.Used() != 300 {
		t.Errorf("quota used = %d, want 300", qt.Used())
	}

	// a and b removed from disk.
	if _, err := os.Stat(filepath.Join(dir, "a.tgz")); !os.IsNotExist(err) {
		t.Error("a.tgz still on disk after eviction")
	}
	if _, err := os.Stat(filepath.Join(dir, "b.tgz")); !os.IsNotExist(err) {
		t.Error("b.tgz still on disk after eviction")
	}
	// c untouched.
	if _, err := os.Stat(filepath.Join(dir, "c.tgz")); err != nil {
		t.Errorf("c.tgz unexpectedly gone: %v", err)
	}

	// a and b gone from inventory.
	cands, _ := inv.LRUCandidates(10)
	if len(cands) != 1 || cands[0].Path != "c.tgz" {
		t.Errorf("inventory after eviction: %v, want [c.tgz]", cands)
	}
}

func TestLRUPolicyEvictPartial(t *testing.T) {
	dir := t.TempDir()
	qt := &quotaTracker{limit: 10000}

	writeTestBlob(t, dir, "only.tgz", make([]byte, 50))
	p, inv := newTestLRUPolicy(t, dir, qt)
	insertBlobAt(t, inv.db, "sstate", "only.tgz", 50, 1000)
	qt.used.Store(50)

	// Ask for 200 but only 50 available.
	freed, err := p.Evict(200)
	if err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if freed != 50 {
		t.Errorf("freed = %d, want 50 (partial)", freed)
	}
	if qt.Used() != 0 {
		t.Errorf("quota used = %d, want 0", qt.Used())
	}
}

func TestLRUPolicyEvictStaleInventoryEntry(t *testing.T) {
	dir := t.TempDir()
	qt := &quotaTracker{limit: 10000}

	// Write b on disk but not a — a is a stale inventory entry (external delete).
	writeTestBlob(t, dir, "b.tgz", make([]byte, 200))

	p, inv := newTestLRUPolicy(t, dir, qt)
	insertBlobAt(t, inv.db, "sstate", "a.tgz", 100, 100) // stale: file is gone
	insertBlobAt(t, inv.db, "sstate", "b.tgz", 200, 200)
	qt.used.Store(200) // only b is real

	// Need 150. a is stale (ErrNotExist tolerated, not counted as freed).
	// b is evicted for 200 freed.
	freed, err := p.Evict(150)
	if err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if freed != 200 {
		t.Errorf("freed = %d, want 200", freed)
	}

	// Both removed from inventory.
	cands, _ := inv.LRUCandidates(10)
	if len(cands) != 0 {
		t.Errorf("inventory not empty after eviction: %v", cands)
	}
}

func TestLRUPolicyKindScopedName(t *testing.T) {
	p := &LRUPolicy{kind: "sstate"}
	if got := p.Name(); got != "lru-sstate" {
		t.Errorf("Name() = %q, want lru-sstate", got)
	}
	all := &LRUPolicy{}
	if got := all.Name(); got != "lru" {
		t.Errorf("Name() = %q, want lru", got)
	}
}

func TestLRUPolicyEvictKindScoped(t *testing.T) {
	// sstate is trimmed before downloads: a kind-scoped "lru-sstate"
	// policy must evict sstate blobs and never touch downloads, even when
	// downloads are older and would otherwise be evicted first.
	sstateDir := t.TempDir()
	downloadsDir := t.TempDir()
	qt := &quotaTracker{limit: 10000}

	writeTestBlob(t, sstateDir, "s.tgz", make([]byte, 100))
	writeTestBlob(t, downloadsDir, "d.tar.gz", make([]byte, 500))

	inv, _ := newTestInventory(t)
	p := &LRUPolicy{
		inventory: inv,
		stores:    map[string]string{"sstate": sstateDir},
		quota:     qt,
		ledger:    nil,
		log:       slog.Default(),
		kind:      "sstate",
	}

	// downloads entry is oldest (would be first in unscoped LRU order) but
	// must be excluded entirely.
	insertBlobAt(t, inv.db, "downloads", "d.tar.gz", 500, 100)
	insertBlobAt(t, inv.db, "sstate", "s.tgz", 100, 200)
	qt.used.Store(600)

	freed, err := p.Evict(50)
	if err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if freed != 100 {
		t.Errorf("freed = %d, want 100 (only sstate evicted)", freed)
	}
	if qt.Used() != 500 {
		t.Errorf("quota used = %d, want 500 (downloads untouched)", qt.Used())
	}

	// sstate blob gone from disk and inventory.
	if _, err := os.Stat(filepath.Join(sstateDir, "s.tgz")); !os.IsNotExist(err) {
		t.Error("s.tgz still on disk after eviction")
	}
	// downloads blob untouched on disk and still in inventory.
	if _, err := os.Stat(filepath.Join(downloadsDir, "d.tar.gz")); err != nil {
		t.Errorf("d.tar.gz unexpectedly gone: %v", err)
	}
	cands, _ := inv.LRUCandidates(10)
	if len(cands) != 1 || cands[0].Path != "d.tar.gz" {
		t.Errorf("inventory after eviction: %v, want [d.tar.gz]", cands)
	}
}

func TestLRUPolicyEvictUnknownKind(t *testing.T) {
	dir := t.TempDir()
	qt := &quotaTracker{limit: 10000}

	writeTestBlob(t, dir, "a.tgz", make([]byte, 100))

	p, inv := newTestLRUPolicy(t, dir, qt)
	// Seed a blob under a kind the policy doesn't manage.
	insertBlobAt(t, inv.db, "ghost", "a.tgz", 100, 100)
	// Seed a normal blob so eviction has something to do.
	insertBlobAt(t, inv.db, "sstate", "a.tgz", 100, 200)
	qt.used.Store(100)

	freed, err := p.Evict(50)
	if err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if freed != 100 {
		t.Errorf("freed = %d, want 100", freed)
	}

	// Ghost entry cleaned up from inventory.
	cands, _ := inv.LRUCandidates(10)
	if len(cands) != 0 {
		t.Errorf("inventory not empty after eviction: %v", cands)
	}
}

// --- Group eviction + hash-equiv cleanup tests ---

func TestLRUPolicyEvictGroupTogether(t *testing.T) {
	dir := t.TempDir()
	qt := &quotaTracker{limit: 10000}

	archivePath := "sstate:ninja-native::1.13.2:r0::14:37001365f620ee00a3177d608f4c5a428edd973c714942c7fea891040660ba34_patch.tar.zst"
	siginfoPath := archivePath + ".siginfo"
	writeTestBlob(t, dir, archivePath, make([]byte, 1000))
	writeTestBlob(t, dir, siginfoPath, make([]byte, 20))

	p, inv := newTestLRUPolicy(t, dir, qt)
	// The siginfo is much colder than the archive — under plain per-row LRU
	// it alone would be picked first, orphaning the archive. Grouped
	// eviction must take both together, in one pass, for a small ask.
	insertBlobAt(t, inv.db, "sstate", siginfoPath, 20, 50)
	insertBlobAt(t, inv.db, "sstate", archivePath, 1000, 500)
	qt.used.Store(1020)

	freed, err := p.Evict(10)
	if err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if freed != 1020 {
		t.Errorf("freed = %d, want 1020 (both siblings evicted together)", freed)
	}
	if _, err := os.Stat(filepath.Join(dir, archivePath)); !os.IsNotExist(err) {
		t.Error("archive still on disk — group eviction left a sibling behind")
	}
	if _, err := os.Stat(filepath.Join(dir, siginfoPath)); !os.IsNotExist(err) {
		t.Error("siginfo still on disk")
	}
}

func TestLRUPolicyEvictCleansUpHashEquiv(t *testing.T) {
	dir := t.TempDir()
	qt := &quotaTracker{limit: 10000}
	inv, db := newTestInventory(t)
	hstore := &hashEquivStore{db: db}

	archivePath := "sstate:ninja-native::1.13.2:r0::14:37001365f620ee00a3177d608f4c5a428edd973c714942c7fea891040660ba34_patch.tar.zst"
	checksum := sstateChecksum(archivePath)
	writeTestBlob(t, dir, archivePath, make([]byte, 100))
	insertBlobAt(t, db, "sstate", archivePath, 100, 100)
	qt.used.Store(100)

	if _, err := hstore.insertUnihash("m", "task1", checksum); err != nil {
		t.Fatalf("insertUnihash: %v", err)
	}

	p := &LRUPolicy{
		inventory: inv,
		stores:    map[string]string{"sstate": dir},
		quota:     qt,
		ledger:    nil,
		hashEquiv: hstore,
		log:       slog.Default(),
		kind:      "sstate",
	}

	if _, err := p.Evict(50); err != nil {
		t.Fatalf("Evict: %v", err)
	}

	if _, ok, _ := hstore.getEquivalent("m", "task1"); ok {
		t.Error("hash-equiv entry survived eviction of its backing blob")
	}
}

// TestLRUPolicyEvictPartialFailureLeavesGroupAndHashEquivIntact makes one
// sibling in a group undeletable (a non-empty directory in its place, so
// os.Remove fails with a real error, not ErrNotExist) and confirms the
// group is treated as not-fully-evicted: the removable sibling is still
// freed individually, but the hash-equiv entry is left alone rather than
// pointing at a blob that (from bitbake's perspective) may still exist.
func TestLRUPolicyEvictPartialFailureLeavesGroupAndHashEquivIntact(t *testing.T) {
	dir := t.TempDir()
	qt := &quotaTracker{limit: 10000}
	inv, db := newTestInventory(t)
	hstore := &hashEquivStore{db: db}

	archivePath := "sstate:ninja-native::1.13.2:r0::14:37001365f620ee00a3177d608f4c5a428edd973c714942c7fea891040660ba34_patch.tar.zst"
	siginfoPath := archivePath + ".siginfo"
	checksum := sstateChecksum(archivePath)

	writeTestBlob(t, dir, siginfoPath, make([]byte, 20))
	// Stand in for the archive with a non-empty directory: os.Remove on it
	// fails (ENOTEMPTY), unlike a missing file (ErrNotExist).
	stuckDir := filepath.Join(dir, archivePath)
	if err := os.MkdirAll(stuckDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stuckDir, "inner"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	insertBlobAt(t, db, "sstate", siginfoPath, 20, 100)
	insertBlobAt(t, db, "sstate", archivePath, 1000, 100)
	qt.used.Store(1020)

	if _, err := hstore.insertUnihash("m", "task1", checksum); err != nil {
		t.Fatalf("insertUnihash: %v", err)
	}

	p := &LRUPolicy{
		inventory: inv,
		stores:    map[string]string{"sstate": dir},
		quota:     qt,
		ledger:    nil,
		hashEquiv: hstore,
		log:       slog.Default(),
		kind:      "sstate",
	}

	freed, err := p.Evict(2000)
	if err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if freed != 20 {
		t.Errorf("freed = %d, want 20 (only the removable sibling)", freed)
	}
	if qt.Used() != 1000 {
		t.Errorf("quota used = %d, want 1000 (only siginfo released)", qt.Used())
	}
	if _, ok, _ := hstore.getEquivalent("m", "task1"); !ok {
		t.Error("hash-equiv entry deleted even though a sibling failed to be removed")
	}
	cands, _ := inv.LRUCandidatesByKind("sstate", 10)
	if len(cands) != 1 || cands[0].Path != archivePath {
		t.Errorf("inventory after partial eviction = %v, want [%s] still present", cands, archivePath)
	}
}
