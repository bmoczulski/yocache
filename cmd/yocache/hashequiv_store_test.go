package main

import (
	"path/filepath"
	"testing"
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

func TestOuthashRoundTripAndFirstWriteWins(t *testing.T) {
	s := newTestStore(t)

	if _, ok, err := s.getOuthash("m", "out1"); err != nil || ok {
		t.Fatalf("empty store: got ok=%v err=%v, want false", ok, err)
	}

	rec := outhashRecord{Method: "m", Outhash: "out1", Taskhash: "task1", Unihash: "uniA"}
	if err := s.insertOuthash(rec); err != nil {
		t.Fatalf("insertOuthash: %v", err)
	}
	// First-write-wins on (method, outhash): a later differing report is ignored.
	if err := s.insertOuthash(outhashRecord{Method: "m", Outhash: "out1", Taskhash: "task2", Unihash: "uniB"}); err != nil {
		t.Fatalf("insertOuthash conflict: %v", err)
	}

	got, ok, err := s.getOuthash("m", "out1")
	if err != nil || !ok {
		t.Fatalf("getOuthash: ok=%v err=%v, want true", ok, err)
	}
	if got != rec {
		t.Fatalf("getOuthash: got %+v, want %+v", got, rec)
	}
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
	if err := s.insertOuthash(outhashRecord{Method: "m", Outhash: "out1", Taskhash: "task1", Unihash: "uniA"}); err != nil {
		t.Fatalf("insertOuthash task1: %v", err)
	}
	if err := s.insertOuthash(outhashRecord{Method: "m", Outhash: "out2", Taskhash: "task2", Unihash: "uniA"}); err != nil {
		t.Fatalf("insertOuthash task2: %v", err)
	}
	// An unrelated taskhash/unihash that must survive.
	if _, err := s.insertUnihash("m", "task3", "uniB"); err != nil {
		t.Fatalf("insertUnihash task3: %v", err)
	}
	if err := s.insertOuthash(outhashRecord{Method: "m", Outhash: "out3", Taskhash: "task3", Unihash: "uniB"}); err != nil {
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
	if _, ok, _ := s.getOuthash("m", "out1"); ok {
		t.Error("out1 outhash survived DeleteByUnihash")
	}
	if _, ok, _ := s.getOuthash("m", "out2"); ok {
		t.Error("out2 outhash survived DeleteByUnihash")
	}

	// Unrelated unihash/outhash must survive.
	if u, ok, _ := s.getEquivalent("m", "task3"); !ok || u != "uniB" {
		t.Errorf("task3 unihash = %q ok=%v, want uniB/true", u, ok)
	}
	if _, ok, _ := s.getOuthash("m", "out3"); !ok {
		t.Error("out3 outhash should survive DeleteByUnihash")
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
	if err := s.insertOuthash(outhashRecord{Method: "m", Outhash: "out1", Taskhash: "task1", Unihash: "uniA"}); err != nil {
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
