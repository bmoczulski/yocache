package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
)

// newTestStore opens a throwaway DB in the test's temp dir, applies migrations,
// and returns a hashEquivStore backed by it. The DB is closed on test cleanup.
func newTestStore(t *testing.T) *hashEquivStore {
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
	return &hashEquivStore{db: db}
}

func TestUnihashRoundTripAndFirstWriteWins(t *testing.T) {
	s := newTestStore(t)

	if _, ok, err := s.getEquivalent("m", "task1"); err != nil || ok {
		t.Fatalf("empty store: got ok=%v err=%v, want ok=false err=nil", ok, err)
	}

	got, err := s.insertUnihash("m", "task1", "uniA")
	if err != nil || got != "uniA" {
		t.Fatalf("first insert: got %q err=%v, want uniA", got, err)
	}

	// First-write-wins: a second report for the same (method, taskhash) keeps the
	// original unihash.
	got, err = s.insertUnihash("m", "task1", "uniB")
	if err != nil || got != "uniA" {
		t.Fatalf("conflicting insert: got %q err=%v, want uniA", got, err)
	}

	u, ok, err := s.getEquivalent("m", "task1")
	if err != nil || !ok || u != "uniA" {
		t.Fatalf("getEquivalent: got %q ok=%v err=%v, want uniA/true", u, ok, err)
	}

	// Method is part of the key.
	if _, ok, _ := s.getEquivalent("other", "task1"); ok {
		t.Fatal("getEquivalent matched across methods")
	}
}

func TestUnihashExists(t *testing.T) {
	s := newTestStore(t)

	if ok, err := s.unihashExists("uniA"); err != nil || ok {
		t.Fatalf("unknown unihash: got ok=%v err=%v, want false", ok, err)
	}
	if _, err := s.insertUnihash("m", "task1", "uniA"); err != nil {
		t.Fatalf("insertUnihash: %v", err)
	}
	if ok, err := s.unihashExists("uniA"); err != nil || !ok {
		t.Fatalf("reported unihash: got ok=%v err=%v, want true", ok, err)
	}
}

func TestOuthashKeepsEveryReport(t *testing.T) {
	s := newTestStore(t)

	if _, ok, err := s.getOuthash("m", "out1", true); err != nil || ok {
		t.Fatalf("empty store: got ok=%v err=%v, want false", ok, err)
	}
	for _, r := range []struct{ task, uni string }{{"task1", "uniA"}, {"task2", "uniB"}} {
		if _, err := s.insertUnihash("m", r.task, r.uni); err != nil {
			t.Fatalf("insertUnihash: %v", err)
		}
		inserted, err := s.insertOuthash(outhashRecord{Method: "m", Outhash: "out1", Taskhash: r.task})
		if err != nil || !inserted {
			t.Fatalf("insertOuthash %s: inserted=%v err=%v, want new row", r.task, inserted, err)
		}
	}
	// Repeating a known (taskhash, outhash) report adds nothing.
	inserted, err := s.insertOuthash(outhashRecord{Method: "m", Outhash: "out1", Taskhash: "task2"})
	if err != nil || inserted {
		t.Fatalf("repeat report: inserted=%v err=%v, want no new row", inserted, err)
	}
	if n := countOuthashRows(t, s, "out1"); n != 2 {
		t.Fatalf("out1 has %d rows, want 2 (one per reporter)", n)
	}

	// get-outhash answers with the oldest reporter and its current unihash.
	got, ok, err := s.getOuthash("m", "out1", true)
	got.Created = checkCreated(t, got.Created)
	want := outhashRecord{Method: "m", Outhash: "out1", Taskhash: "task1", Unihash: "uniA"}
	if err != nil || !ok || got != want {
		t.Fatalf("getOuthash: got %+v ok=%v err=%v, want %+v", got, ok, err, want)
	}
}

// countOuthashRows counts the reports stored for outhash, independent of
// whether their taskhashes still have a unihash.
func countOuthashRows(t *testing.T, s *hashEquivStore, outhash string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM outhashes_v2 WHERE outhash = ?`, outhash).Scan(&n); err != nil {
		t.Fatalf("count outhashes_v2: %v", err)
	}
	return n
}

func TestDeleteByUnihash(t *testing.T) {
	s := newTestStore(t)

	// Two taskhashes unified onto the same unihash (cross-output equivalence),
	// each with its own outhash provenance row.
	if _, err := s.insertUnihash("m", "task1", "uniA"); err != nil {
		t.Fatalf("insertUnihash task1: %v", err)
	}
	if _, err := s.insertUnihash("m", "task2", "uniA"); err != nil {
		t.Fatalf("insertUnihash task2: %v", err)
	}
	if _, err := s.insertOuthash(outhashRecord{Method: "m", Outhash: "out1", Taskhash: "task1"}); err != nil {
		t.Fatalf("insertOuthash task1: %v", err)
	}
	if _, err := s.insertOuthash(outhashRecord{Method: "m", Outhash: "out2", Taskhash: "task2"}); err != nil {
		t.Fatalf("insertOuthash task2: %v", err)
	}
	// An unrelated taskhash/unihash that must survive.
	if _, err := s.insertUnihash("m", "task3", "uniB"); err != nil {
		t.Fatalf("insertUnihash task3: %v", err)
	}
	if _, err := s.insertOuthash(outhashRecord{Method: "m", Outhash: "out3", Taskhash: "task3"}); err != nil {
		t.Fatalf("insertOuthash task3: %v", err)
	}

	unihashRows, outhashRows, err := s.DeleteByUnihash("uniA")
	if err != nil {
		t.Fatalf("DeleteByUnihash: %v", err)
	}
	if unihashRows != 2 {
		t.Errorf("unihashRows = %d, want 2", unihashRows)
	}
	if outhashRows != 2 {
		t.Errorf("outhashRows = %d, want 2", outhashRows)
	}

	if _, ok, _ := s.getEquivalent("m", "task1"); ok {
		t.Error("task1 unihash survived DeleteByUnihash")
	}
	if _, ok, _ := s.getEquivalent("m", "task2"); ok {
		t.Error("task2 unihash survived DeleteByUnihash")
	}
	if n := countOuthashRows(t, s, "out1"); n != 0 {
		t.Errorf("out1: %d outhash rows survived DeleteByUnihash", n)
	}
	if n := countOuthashRows(t, s, "out2"); n != 0 {
		t.Errorf("out2: %d outhash rows survived DeleteByUnihash", n)
	}

	// Unrelated unihash/outhash must survive.
	if u, ok, _ := s.getEquivalent("m", "task3"); !ok || u != "uniB" {
		t.Errorf("task3 unihash = %q ok=%v, want uniB/true", u, ok)
	}
	if n := countOuthashRows(t, s, "out3"); n != 1 {
		t.Errorf("out3: %d outhash rows, want 1 to survive DeleteByUnihash", n)
	}
}

func TestDeleteByUnihashNoMatch(t *testing.T) {
	s := newTestStore(t)
	unihashRows, outhashRows, err := s.DeleteByUnihash("nonexistent")
	if err != nil {
		t.Fatalf("DeleteByUnihash: %v", err)
	}
	if unihashRows != 0 || outhashRows != 0 {
		t.Errorf("DeleteByUnihash on empty store = (%d, %d), want (0, 0)", unihashRows, outhashRows)
	}
}

// TestPersistAcrossReopen is the whole point of the change: data written before a
// restart is still in effect after one.
func TestPersistAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "persist.db")

	db1, err := openOperationalDB(path)
	if err != nil {
		t.Fatalf("open #1: %v", err)
	}
	if err := migrateDB(db1); err != nil {
		db1.Close()
		t.Fatalf("migrate #1: %v", err)
	}
	s1 := &hashEquivStore{db: db1}
	if _, err := s1.insertUnihash("m", "task1", "uniA"); err != nil {
		t.Fatalf("insertUnihash: %v", err)
	}
	if err := db1.Close(); err != nil {
		t.Fatalf("close #1: %v", err)
	}

	db2, err := openOperationalDB(path)
	if err != nil {
		t.Fatalf("open #2: %v", err)
	}
	if err := migrateDB(db2); err != nil {
		db2.Close()
		t.Fatalf("migrate #2: %v", err)
	}
	defer db2.Close()
	s2 := &hashEquivStore{db: db2}
	u, ok, err := s2.getEquivalent("m", "task1")
	if err != nil || !ok || u != "uniA" {
		t.Fatalf("after reopen: got %q ok=%v err=%v, want uniA/true", u, ok, err)
	}
}

func TestStats(t *testing.T) {
	s := newTestStore(t)

	if _, err := s.insertUnihash("m", "task1", "uniA"); err != nil {
		t.Fatalf("insertUnihash: %v", err)
	}
	// A second taskhash that happens to produce the same unihash: two
	// taskhashes, but only one distinct unihash.
	if _, err := s.insertUnihash("m", "task2", "uniA"); err != nil {
		t.Fatalf("insertUnihash: %v", err)
	}
	if _, err := s.insertOuthash(outhashRecord{Method: "m", Outhash: "out1", Taskhash: "task1"}); err != nil {
		t.Fatalf("insertOuthash: %v", err)
	}

	got, err := s.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	want := &hashEquivStats{TaskHashes: 2, Unihashes: 1, Outhashes: 1}
	if *got != *want {
		t.Errorf("Stats() = %+v, want %+v", *got, *want)
	}
}

// addSstateBlob records an sstate blob for unihash in the inventory the store
// shares its DB with. suffix picks the group member: ".tar.zst" for the
// archive, ".tar.zst.siginfo" for a sidecar.
func addSstateBlob(t *testing.T, s *hashEquivStore, unihash, suffix string) {
	t.Helper()
	path := "ab/cd/sstate:pn:arch:1.0:r0:arch:14:" + unihash + "_populate_sysroot" + suffix
	if err := openBlobInventory(s.db).Upsert("sstate", path, 1, "", -1); err != nil {
		t.Fatalf("Upsert %s: %v", path, err)
	}
}

func TestRequireSstateGatesLookups(t *testing.T) {
	s := newTestStore(t)
	s.requireSstate = true

	if _, err := s.insertUnihash("m", "task1", "uniA"); err != nil {
		t.Fatalf("insertUnihash: %v", err)
	}
	if _, ok, err := s.getEquivalent("m", "task1"); err != nil || ok {
		t.Fatalf("no archive: getEquivalent ok=%v err=%v, want miss", ok, err)
	}
	if ok, err := s.unihashExists("uniA"); err != nil || ok {
		t.Fatalf("no archive: unihashExists ok=%v err=%v, want false", ok, err)
	}

	// A sidecar alone can't be restored from, so it doesn't count.
	addSstateBlob(t, s, "uniA", ".tar.zst.siginfo")
	if _, ok, err := s.getEquivalent("m", "task1"); err != nil || ok {
		t.Fatalf("siginfo only: getEquivalent ok=%v err=%v, want miss", ok, err)
	}

	addSstateBlob(t, s, "uniA", ".tar.zst")
	if u, ok, err := s.getEquivalent("m", "task1"); err != nil || !ok || u != "uniA" {
		t.Fatalf("archive stored: getEquivalent %q ok=%v err=%v, want uniA", u, ok, err)
	}
	if ok, err := s.unihashExists("uniA"); err != nil || !ok {
		t.Fatalf("archive stored: unihashExists ok=%v err=%v, want true", ok, err)
	}
}

func TestRequireSstateInsertReplacesUnbackedWinner(t *testing.T) {
	s := newTestStore(t)
	s.requireSstate = true

	if _, err := s.insertUnihash("m", "task1", "uniA"); err != nil {
		t.Fatalf("insertUnihash: %v", err)
	}
	// uniA has no archive, so a later report's unihash takes over.
	got, err := s.insertUnihash("m", "task1", "uniB")
	if err != nil || got != "uniB" {
		t.Fatalf("unbacked winner: got %q err=%v, want uniB", got, err)
	}
	// Once uniB's archive is stored, first-write-wins applies again.
	addSstateBlob(t, s, "uniB", ".tar.zst")
	got, err = s.insertUnihash("m", "task1", "uniC")
	if err != nil || got != "uniB" {
		t.Fatalf("backed winner: got %q err=%v, want uniB", got, err)
	}
}

func TestEquivalentForOuthash(t *testing.T) {
	s := newTestStore(t)

	if _, err := s.insertUnihash("m", "task1", "uniA"); err != nil {
		t.Fatalf("insertUnihash: %v", err)
	}
	if _, err := s.insertOuthash(outhashRecord{Method: "m", Outhash: "out1", Taskhash: "task1"}); err != nil {
		t.Fatalf("insertOuthash: %v", err)
	}

	// The taskhash that owns the outhash row has nothing to unify with.
	if _, ok, err := s.equivalentForOuthash("m", "out1", "task1"); err != nil || ok {
		t.Fatalf("own row: ok=%v err=%v, want miss", ok, err)
	}
	if u, ok, err := s.equivalentForOuthash("m", "out1", "task2"); err != nil || !ok || u != "uniA" {
		t.Fatalf("other taskhash: %q ok=%v err=%v, want uniA", u, ok, err)
	}

	s.requireSstate = true
	if _, ok, err := s.equivalentForOuthash("m", "out1", "task2"); err != nil || ok {
		t.Fatalf("target without archive: ok=%v err=%v, want miss", ok, err)
	}
	addSstateBlob(t, s, "uniA", ".tar.zst")
	if u, ok, err := s.equivalentForOuthash("m", "out1", "task2"); err != nil || !ok || u != "uniA" {
		t.Fatalf("target with archive: %q ok=%v err=%v, want uniA", u, ok, err)
	}
}

func TestPruneUnbacked(t *testing.T) {
	s := newTestStore(t)

	// task1 -> uniA has its archive; task2 -> uniB has only a sidecar;
	// task3 -> uniC has nothing. Each also owns an outhash row.
	for _, r := range []struct{ task, uni, out string }{
		{"task1", "uniA", "out1"},
		{"task2", "uniB", "out2"},
		{"task3", "uniC", "out3"},
	} {
		if _, err := s.insertUnihash("m", r.task, r.uni); err != nil {
			t.Fatalf("insertUnihash: %v", err)
		}
		if _, err := s.insertOuthash(outhashRecord{Method: "m", Outhash: r.out, Taskhash: r.task}); err != nil {
			t.Fatalf("insertOuthash: %v", err)
		}
	}
	addSstateBlob(t, s, "uniA", ".tar.zst")
	addSstateBlob(t, s, "uniB", ".tar.zst.siginfo")

	uni, out, err := s.PruneUnbacked()
	if err != nil {
		t.Fatalf("PruneUnbacked: %v", err)
	}
	if uni != 2 || out != 2 {
		t.Fatalf("PruneUnbacked removed %d unihashes, %d outhashes; want 2, 2", uni, out)
	}
	if u, ok, _ := s.getEquivalent("m", "task1"); !ok || u != "uniA" {
		t.Fatalf("backed entry: got %q ok=%v, want uniA kept", u, ok)
	}
	if n := countOuthashRows(t, s, "out1"); n != 1 {
		t.Fatalf("backed entry: %d outhash rows, want 1", n)
	}
	for _, task := range []string{"task2", "task3"} {
		if _, ok, _ := s.getEquivalent("m", task); ok {
			t.Fatalf("%s: unbacked entry survived", task)
		}
	}
	for _, out := range []string{"out2", "out3"} {
		if n := countOuthashRows(t, s, out); n != 0 {
			t.Fatalf("%s: %d outhash rows of unbacked entry survived", out, n)
		}
	}
}

func TestEquivalentForOuthashFallsBackToOldestBacked(t *testing.T) {
	s := newTestStore(t)
	s.requireSstate = true

	// task1 and task2 share uniA (unified), task3 has its own uniC; all
	// produced out1, in that order.
	for _, r := range []struct{ task, uni string }{{"task1", "uniA"}, {"task2", "uniA"}, {"task3", "uniC"}} {
		if _, err := s.insertUnihash("m", r.task, r.uni); err != nil {
			t.Fatalf("insertUnihash: %v", err)
		}
		if _, err := s.insertOuthash(outhashRecord{Method: "m", Outhash: "out1", Taskhash: r.task}); err != nil {
			t.Fatalf("insertOuthash: %v", err)
		}
	}
	addSstateBlob(t, s, "uniA", ".tar.zst")
	addSstateBlob(t, s, "uniC", ".tar.zst")
	if u, ok, err := s.equivalentForOuthash("m", "out1", "task4"); err != nil || !ok || u != "uniA" {
		t.Fatalf("all backed: %q ok=%v err=%v, want oldest uniA", u, ok, err)
	}

	// uniA's archive goes; the next-oldest reporter with an archive wins.
	if _, err := s.db.Exec(`DELETE FROM blobs WHERE checksum = 'uniA'`); err != nil {
		t.Fatalf("delete blob: %v", err)
	}
	if u, ok, err := s.equivalentForOuthash("m", "out1", "task4"); err != nil || !ok || u != "uniC" {
		t.Fatalf("oldest unbacked: %q ok=%v err=%v, want uniC", u, ok, err)
	}
}

func TestMigrationCopiesOuthashesToV2(t *testing.T) {
	db, err := openOperationalDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("openOperationalDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// A database as v0.1.13 left it: schema up to 0004, one outhashes row.
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatalf("goose dialect: %v", err)
	}
	if err := goose.UpTo(db, "migrations", 4); err != nil {
		t.Fatalf("migrate to 4: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO unihashes VALUES ('m', 'task1', 'uniA')`); err != nil {
		t.Fatalf("seed unihashes: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO outhashes VALUES ('m', 'out1', 'task1', 'uniA')`); err != nil {
		t.Fatalf("seed outhashes: %v", err)
	}

	if err := migrateDB(db); err != nil {
		t.Fatalf("migrateDB: %v", err)
	}
	s := &hashEquivStore{db: db}
	got, ok, err := s.getOuthash("m", "out1", true)
	want := outhashRecord{Method: "m", Outhash: "out1", Taskhash: "task1", Unihash: "uniA"}
	if err != nil || !ok || got != want {
		t.Fatalf("getOuthash after migration: got %+v ok=%v err=%v, want %+v", got, ok, err, want)
	}

	// A copied row has no timestamp, so it stays the oldest reporter.
	if _, err := s.insertUnihash("m", "task2", "uniB"); err != nil {
		t.Fatalf("insertUnihash: %v", err)
	}
	if _, err := s.insertOuthash(outhashRecord{Method: "m", Outhash: "out1", Taskhash: "task2"}); err != nil {
		t.Fatalf("insertOuthash: %v", err)
	}
	if u, ok, err := s.equivalentForOuthash("m", "out1", "task3"); err != nil || !ok || u != "uniA" {
		t.Fatalf("equivalentForOuthash: %q ok=%v err=%v, want copied row's uniA", u, ok, err)
	}
}

// TestDownReconcilesManyReportersToOne exercises 0005's Down migration, which
// must collapse every outhashes_v2 reporter of a given (method, outhash) back
// onto the single row the pre-0005 outhashes table had room for -- picking
// whichever reporter is earliest by created, NULL (a row copied forward by
// Up, rather than reported since) sorting first regardless of insertion
// order.
func TestDownReconcilesManyReportersToOne(t *testing.T) {
	db, err := openOperationalDB(filepath.Join(t.TempDir(), "reconcile.db"))
	if err != nil {
		t.Fatalf("openOperationalDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatalf("goose dialect: %v", err)
	}
	if err := goose.UpTo(db, "migrations", 5); err != nil {
		t.Fatalf("UpTo 5: %v", err)
	}

	type reporter struct {
		taskhash, unihash, created string // created == "" means NULL
	}
	type group struct {
		method, outhash string
		reporters       []reporter
		wantTaskhash    string
		wantUnihash     string
	}

	groups := []group{
		{
			// Three real timestamps, inserted out of chronological order --
			// the middle one first, then the latest, then the earliest --
			// so this also checks created beats insertion order (id).
			method: "m", outhash: "outX",
			reporters: []reporter{
				{"task5", "uniE", "2026-03-01T00:00:00Z"},
				{"task8", "uniH", "2026-06-01T00:00:00Z"},
				{"task2", "uniB", "2026-01-15T00:00:00Z"},
			},
			wantTaskhash: "task2", wantUnihash: "uniB",
		},
		{
			// A NULL (copied-forward) reporter arrives after two
			// timestamped ones -- NULL must still win as "earliest"
			// regardless of insertion order.
			method: "m", outhash: "outY",
			reporters: []reporter{
				{"task3", "uniC", "2026-02-01T00:00:00Z"},
				{"task4", "uniD", "2026-01-01T00:00:00Z"},
				{"task1", "uniA", ""},
			},
			wantTaskhash: "task1", wantUnihash: "uniA",
		},
		{
			// Two reporters with an identical timestamp: the tie-break must
			// be deterministic (lowest id, i.e. whichever was reported
			// first) rather than ambiguous.
			method: "m", outhash: "outZ",
			reporters: []reporter{
				{"task10", "uniJ", "2026-04-01T00:00:00Z"},
				{"task11", "uniK", "2026-04-01T00:00:00Z"},
			},
			wantTaskhash: "task10", wantUnihash: "uniJ",
		},
		{
			// Same outhash text as the first group, but a different
			// method -- must reconcile independently, not merge with "m".
			method: "n", outhash: "outX",
			reporters: []reporter{
				{"task20", "uniT", "2026-05-01T00:00:00Z"},
				{"task21", "uniU", "2026-01-01T00:00:00Z"},
			},
			wantTaskhash: "task21", wantUnihash: "uniU",
		},
		{
			// A single reporter: must survive as-is.
			method: "m", outhash: "outSingle",
			reporters:    []reporter{{"task30", "uniSingle", "2026-01-01T00:00:00Z"}},
			wantTaskhash: "task30", wantUnihash: "uniSingle",
		},
	}

	// Insert interleaved across groups (round-robin by reporter index) so
	// the reconciliation query can't accidentally rely on one outhash's
	// rows being contiguous by id.
	maxReporters := 0
	for _, g := range groups {
		if len(g.reporters) > maxReporters {
			maxReporters = len(g.reporters)
		}
	}
	seenUnihash := map[string]bool{}
	for i := 0; i < maxReporters; i++ {
		for _, g := range groups {
			if i >= len(g.reporters) {
				continue
			}
			r := g.reporters[i]
			key := g.method + "/" + r.taskhash
			if !seenUnihash[key] {
				if _, err := db.Exec(`INSERT INTO unihashes VALUES (?, ?, ?)`, g.method, r.taskhash, r.unihash); err != nil {
					t.Fatalf("seed unihashes %s/%s: %v", g.method, r.taskhash, err)
				}
				seenUnihash[key] = true
			}
			var createdArg interface{}
			if r.created != "" {
				createdArg = r.created
			}
			if _, err := db.Exec(`INSERT INTO outhashes_v2 (method, taskhash, outhash, created) VALUES (?, ?, ?, ?)`,
				g.method, r.taskhash, g.outhash, createdArg); err != nil {
				t.Fatalf("seed outhashes_v2 %s/%s/%s: %v", g.method, r.taskhash, g.outhash, err)
			}
		}
	}

	if err := goose.DownTo(db, "migrations", 4); err != nil {
		t.Fatalf("DownTo 4: %v", err)
	}

	for _, g := range groups {
		var taskhash, unihash string
		err := db.QueryRow(`SELECT taskhash, unihash FROM outhashes WHERE method = ? AND outhash = ?`, g.method, g.outhash).
			Scan(&taskhash, &unihash)
		if err != nil {
			t.Fatalf("outhashes %s/%s: %v", g.method, g.outhash, err)
		}
		if taskhash != g.wantTaskhash || unihash != g.wantUnihash {
			t.Fatalf("outhashes %s/%s: got taskhash=%s unihash=%s, want taskhash=%s unihash=%s",
				g.method, g.outhash, taskhash, unihash, g.wantTaskhash, g.wantUnihash)
		}
	}

	var total int
	if err := db.QueryRow(`SELECT count(*) FROM outhashes`).Scan(&total); err != nil {
		t.Fatalf("count outhashes: %v", err)
	}
	if total != len(groups) {
		t.Fatalf("outhashes row count: got %d, want %d (one per outhash group, reporters collapsed)", total, len(groups))
	}
}

func TestGetOuthashModes(t *testing.T) {
	s := newTestStore(t)
	s.requireSstate = true

	// task1 (uniA, no archive) reported out1 before task2 (uniB, archive).
	for _, r := range []struct{ task, uni string }{{"task1", "uniA"}, {"task2", "uniB"}} {
		if _, err := s.insertUnihash("m", r.task, r.uni); err != nil {
			t.Fatalf("insertUnihash: %v", err)
		}
		if _, err := s.insertOuthash(outhashRecord{Method: "m", Outhash: "out1", Taskhash: r.task}); err != nil {
			t.Fatalf("insertOuthash: %v", err)
		}
	}
	addSstateBlob(t, s, "uniB", ".tar.zst")

	got, ok, err := s.getOuthash("m", "out1", false)
	got.Created = checkCreated(t, got.Created)
	want := outhashRecord{Method: "m", Outhash: "out1", Taskhash: "task1"}
	if err != nil || !ok || got != want {
		t.Fatalf("without unihash: got %+v ok=%v err=%v, want oldest report %+v", got, ok, err, want)
	}
	got, ok, err = s.getOuthash("m", "out1", true)
	got.Created = checkCreated(t, got.Created)
	want = outhashRecord{Method: "m", Outhash: "out1", Taskhash: "task2", Unihash: "uniB"}
	if err != nil || !ok || got != want {
		t.Fatalf("with unihash: got %+v ok=%v err=%v, want oldest backed %+v", got, ok, err, want)
	}
}

// checkCreated fails the test unless created is an ISO 8601 UTC timestamp in
// the form bitbake's hashserv writes, then returns "" so the caller can
// compare the rest of a record by value.
func checkCreated(t *testing.T, created string) string {
	t.Helper()
	if _, err := time.Parse("2006-01-02T15:04:05.000000+00:00", created); err != nil {
		t.Fatalf("created = %q, want an isoformat() UTC timestamp: %v", created, err)
	}
	return ""
}

func TestOuthashReportFields(t *testing.T) {
	s := newTestStore(t)

	if _, err := s.insertUnihash("m", "task1", "uniA"); err != nil {
		t.Fatalf("insertUnihash: %v", err)
	}
	sent := outhashRecord{
		Method: "m", Outhash: "out1", Taskhash: "task1",
		Owner: "builder", PN: "zlib", PV: "1.3", PR: "r0", Task: "populate_sysroot",
		OuthashSiginfo: "OEOuthashBasic\n",
	}
	if _, err := s.insertOuthash(sent); err != nil {
		t.Fatalf("insertOuthash: %v", err)
	}
	got, ok, err := s.getOuthash("m", "out1", true)
	if err != nil || !ok {
		t.Fatalf("getOuthash: ok=%v err=%v", ok, err)
	}
	got.Created = checkCreated(t, got.Created)
	want := sent
	want.Unihash = "uniA"
	if got != want {
		t.Fatalf("getOuthash: got %+v, want %+v", got, want)
	}

	// Fields bitbake didn't send go out as JSON null, as from its own server.
	fields := outhashRecord{Owner: "builder"}.reportFields()
	if fields["owner"] != "builder" || fields["PN"] != nil || fields["created"] != nil {
		t.Fatalf("reportFields = %v, want owner set and the rest nil", fields)
	}
}
