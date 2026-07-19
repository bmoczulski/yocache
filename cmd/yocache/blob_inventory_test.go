package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
)

func newTestInventory(t *testing.T) (*blobInventory, *sql.DB) {
	t.Helper()
	db, err := openOperationalDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("openOperationalDB: %v", err)
	}
	if err := migrateDB(db); err != nil {
		db.Close()
		t.Fatalf("migrateDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return openBlobInventory(db), db
}

// insertBlobAt inserts a blob with explicit timestamps, bypassing Upsert's
// time.Now() so tests can control LRU ordering without sleeping. checksum is
// still derived the way Upsert derives it, so sstate rows that share a real
// checksum (e.g. an archive plus its .siginfo sidecar) dedup the same way
// they would via the real write path.
func insertBlobAt(t *testing.T, db *sql.DB, kind, path string, size, ts int64) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO blobs (kind, path, size, added_at, accessed_at, checksum) VALUES (?, ?, ?, ?, ?, ?)`,
		kind, path, size, ts, ts, groupChecksum(kind, path),
	)
	if err != nil {
		t.Fatalf("insertBlobAt(%s/%s): %v", kind, path, err)
	}
}

func queryBlobRow(t *testing.T, db *sql.DB, kind, path string) (size, addedAt, accessedAt int64, found bool) {
	t.Helper()
	err := db.QueryRow(
		`SELECT size, added_at, accessed_at FROM blobs WHERE kind = ? AND path = ?`,
		kind, path,
	).Scan(&size, &addedAt, &accessedAt)
	if err == sql.ErrNoRows {
		return 0, 0, 0, false
	}
	if err != nil {
		t.Fatalf("queryBlobRow(%s/%s): %v", kind, path, err)
	}
	return size, addedAt, accessedAt, true
}

func TestBlobInventoryUpsertNew(t *testing.T) {
	inv, db := newTestInventory(t)

	if err := inv.Upsert("downloads", "foo.tar.gz", 1234, "", -1); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	size, addedAt, accessedAt, found := queryBlobRow(t, db, "downloads", "foo.tar.gz")
	if !found {
		t.Fatal("blob not found after Upsert")
	}
	if size != 1234 {
		t.Errorf("size = %d, want 1234", size)
	}
	if addedAt == 0 || accessedAt == 0 {
		t.Errorf("timestamps not set: added_at=%d accessed_at=%d", addedAt, accessedAt)
	}
	if addedAt != accessedAt {
		t.Errorf("added_at (%d) != accessed_at (%d) on first insert", addedAt, accessedAt)
	}
}

func TestBlobInventoryUpsertReplace(t *testing.T) {
	inv, db := newTestInventory(t)

	// Seed an old record with a known added_at.
	insertBlobAt(t, db, "sstate", "task.tgz", 100, 1000)

	// Upsert with a new size — simulates a VCS tarball that grew.
	if err := inv.Upsert("sstate", "task.tgz", 200, "", -1); err != nil {
		t.Fatalf("Upsert replace: %v", err)
	}

	size, addedAt, _, found := queryBlobRow(t, db, "sstate", "task.tgz")
	if !found {
		t.Fatal("blob not found after replace Upsert")
	}
	if size != 200 {
		t.Errorf("size = %d, want 200 after replace", size)
	}
	// added_at must not change — it marks first-ever introduction to the store.
	if addedAt != 1000 {
		t.Errorf("added_at = %d, want 1000 (must be preserved on replace)", addedAt)
	}
}

func TestBlobInventoryTouch(t *testing.T) {
	inv, db := newTestInventory(t)

	// Insert with an old accessed_at so Touch has something to advance.
	insertBlobAt(t, db, "downloads", "a.tar.gz", 512, 1000)

	if err := inv.Touch("downloads", "a.tar.gz"); err != nil {
		t.Fatalf("Touch: %v", err)
	}

	_, _, accessedAt, _ := queryBlobRow(t, db, "downloads", "a.tar.gz")
	if accessedAt <= 1000 {
		t.Errorf("accessed_at = %d, want > 1000 after Touch", accessedAt)
	}
}

func TestBlobInventoryRemove(t *testing.T) {
	inv, db := newTestInventory(t)

	insertBlobAt(t, db, "sstate", "gone.tgz", 64, 1000)

	if err := inv.Remove("sstate", "gone.tgz"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	_, _, _, found := queryBlobRow(t, db, "sstate", "gone.tgz")
	if found {
		t.Error("blob still present after Remove")
	}
}

func TestBlobInventoryLRUCandidatesOrdering(t *testing.T) {
	inv, db := newTestInventory(t)

	// Three blobs accessed at t=300, t=100, t=200 — LRU order should be B, C, A.
	insertBlobAt(t, db, "downloads", "a.tar.gz", 10, 300)
	insertBlobAt(t, db, "downloads", "b.tar.gz", 20, 100)
	insertBlobAt(t, db, "downloads", "c.tar.gz", 30, 200)

	cands, err := inv.LRUCandidates(10)
	if err != nil {
		t.Fatalf("LRUCandidates: %v", err)
	}
	if len(cands) != 3 {
		t.Fatalf("got %d candidates, want 3", len(cands))
	}
	if cands[0].Path != "b.tar.gz" || cands[1].Path != "c.tar.gz" || cands[2].Path != "a.tar.gz" {
		t.Errorf("LRU order = %v %v %v, want b c a",
			cands[0].Path, cands[1].Path, cands[2].Path)
	}
	if cands[0].Size != 20 {
		t.Errorf("cands[0].Size = %d, want 20", cands[0].Size)
	}
}

func TestBlobInventoryLRUCandidatesLimit(t *testing.T) {
	inv, db := newTestInventory(t)

	for i := int64(0); i < 5; i++ {
		insertBlobAt(t, db, "sstate", filepath.Join("x", string(rune('a'+i))), 1, i)
	}

	cands, err := inv.LRUCandidates(3)
	if err != nil {
		t.Fatalf("LRUCandidates: %v", err)
	}
	if len(cands) != 3 {
		t.Errorf("got %d candidates, want 3 (limit respected)", len(cands))
	}
}

func TestBlobInventoryLRUCandidatesByKind(t *testing.T) {
	inv, db := newTestInventory(t)

	// Interleaved kinds; sstate order by access time should be s-old then s-new,
	// downloads entries must never appear.
	insertBlobAt(t, db, "downloads", "d-old.tar.gz", 10, 100)
	insertBlobAt(t, db, "sstate", "s-old.tgz", 20, 200)
	insertBlobAt(t, db, "downloads", "d-new.tar.gz", 30, 300)
	insertBlobAt(t, db, "sstate", "s-new.tgz", 40, 400)

	cands, err := inv.LRUCandidatesByKind("sstate", 10)
	if err != nil {
		t.Fatalf("LRUCandidatesByKind: %v", err)
	}
	if len(cands) != 2 {
		t.Fatalf("got %d candidates, want 2", len(cands))
	}
	if cands[0].Path != "s-old.tgz" || cands[1].Path != "s-new.tgz" {
		t.Errorf("LRU order = %v %v, want s-old s-new", cands[0].Path, cands[1].Path)
	}
	for _, c := range cands {
		if c.Kind != "sstate" {
			t.Errorf("candidate kind = %q, want sstate", c.Kind)
		}
	}
}

func TestBlobInventoryRetrofit(t *testing.T) {
	inv, db := newTestInventory(t)
	dir := t.TempDir()

	// Write two real files.
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "world.txt"), []byte("world!"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Dot-prefixed staging file must be skipped.
	if err := os.WriteFile(filepath.Join(dir, ".staging"), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := inv.Retrofit(map[string]string{"downloads": dir}); err != nil {
		t.Fatalf("Retrofit: %v", err)
	}

	// Both real files seeded.
	if _, _, _, found := queryBlobRow(t, db, "downloads", "hello.txt"); !found {
		t.Error("hello.txt not seeded")
	}
	if _, _, _, found := queryBlobRow(t, db, "downloads", filepath.Join("sub", "world.txt")); !found {
		t.Error("sub/world.txt not seeded")
	}
	// Dot file skipped.
	if _, _, _, found := queryBlobRow(t, db, "downloads", ".staging"); found {
		t.Error(".staging was seeded but should be skipped")
	}
}

// TestMigrationBackfillsSstateChecksum confirms migration 0004's backfill
// UPDATE (pure SQL) agrees with sstateChecksum (Go) on rows that predate the
// checksum column — the case a fresh-DB test can't exercise, since goose
// applies every migration in one shot before any row exists to backfill. It
// stops short of migration 4, seeds rows the way they'd have looked before
// the column existed, then applies just that migration and checks the result.
func TestMigrationBackfillsSstateChecksum(t *testing.T) {
	paths := []string{
		"37/00/sstate:ninja-native::1.13.2:r0::14:37001365f620ee00a3177d608f4c5a428edd973c714942c7fea891040660ba34_patch.tar.zst",
		"37/00/sstate:ninja-native::1.13.2:r0::14:37001365f620ee00a3177d608f4c5a428edd973c714942c7fea891040660ba34_patch.tar.zst.siginfo",
		"sstate:curl::8.19.0:r0::14:372038f5e66ef6eaf2d5f847a0f07ace84cd0e69ab59ba61d30993a5bfda910c_patch.tar.zst",
	}

	db, err := openOperationalDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("openOperationalDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatalf("goose dialect: %v", err)
	}
	if err := goose.UpTo(db, "migrations", 3); err != nil {
		t.Fatalf("UpTo(3): %v", err)
	}

	for i, p := range paths {
		if _, err := db.Exec(
			`INSERT INTO blobs (kind, path, size, added_at, accessed_at) VALUES (?, ?, ?, ?, ?)`,
			"sstate", p, 100, 1000, 1000+int64(i),
		); err != nil {
			t.Fatalf("seed pre-existing row %q: %v", p, err)
		}
	}

	if err := goose.UpTo(db, "migrations", 4); err != nil {
		t.Fatalf("UpTo(4): %v", err)
	}

	for _, p := range paths {
		var checksum sql.NullString
		if err := db.QueryRow(`SELECT checksum FROM blobs WHERE path = ?`, p).Scan(&checksum); err != nil {
			t.Fatalf("query checksum for %q: %v", p, err)
		}
		want := sstateChecksum(p)
		if !checksum.Valid || checksum.String != want {
			t.Errorf("backfilled checksum for %q = %+v, want %q", p, checksum, want)
		}
	}
}

func TestBlobInventoryRetrofitSkipsExisting(t *testing.T) {
	inv, db := newTestInventory(t)
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "blob.tgz"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Pre-seed with a known accessed_at so we can verify Retrofit didn't touch it.
	insertBlobAt(t, db, "sstate", "blob.tgz", 999, 42)

	if err := inv.Retrofit(map[string]string{"sstate": dir}); err != nil {
		t.Fatalf("Retrofit: %v", err)
	}

	size, _, accessedAt, _ := queryBlobRow(t, db, "sstate", "blob.tgz")
	if size != 999 || accessedAt != 42 {
		t.Errorf("Retrofit overwrote existing record: size=%d accessedAt=%d, want 999/42", size, accessedAt)
	}
}
