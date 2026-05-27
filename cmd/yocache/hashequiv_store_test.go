package main

import (
	"path/filepath"
	"testing"
)

// newTestStore opens a throwaway store in the test's temp dir.
func newTestStore(t *testing.T) *hashEquivStore {
	t.Helper()
	s, err := openHashEquivStore(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatalf("openHashEquivStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
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

// TestPersistAcrossReopen is the whole point of the change: data written before a
// restart is still in effect after one.
func TestPersistAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "persist.sqlite")

	s1, err := openHashEquivStore(path)
	if err != nil {
		t.Fatalf("open #1: %v", err)
	}
	if _, err := s1.insertUnihash("m", "task1", "uniA"); err != nil {
		t.Fatalf("insertUnihash: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("close #1: %v", err)
	}

	s2, err := openHashEquivStore(path)
	if err != nil {
		t.Fatalf("open #2: %v", err)
	}
	defer s2.Close()
	u, ok, err := s2.getEquivalent("m", "task1")
	if err != nil || !ok || u != "uniA" {
		t.Fatalf("after reopen: got %q ok=%v err=%v, want uniA/true", u, ok, err)
	}
}
