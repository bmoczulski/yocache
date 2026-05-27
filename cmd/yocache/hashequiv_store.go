package main

// Persistent backing store for the hash-equivalence server (hashequiv.go).
//
// This is the SQLite slice that replaces the original in-memory map store: a
// single file survives a yocache restart, so unihashes a build reported earlier
// are still in effect afterwards. That matters because a volatile store can shift
// unihashes across a restart, changing dependent taskhashes and tripping
// bitbake's StaleSetSceneTasks ("Removing N stale sstate objects") mid-campaign.
//
// The schema mirrors the subset of bitbake's hashserv tables a build exercises:
//   - unihashes: (method, taskhash) -> unihash, first-write-wins
//   - outhashes: (method, outhash)  -> the taskhash/unihash that produced it
// Semantics are unchanged from the in-memory store — still first-write-wins with
// NO cross-output equivalence dedup yet. unihashExists is answered from the
// unihashes table (a unihash "exists" if some taskhash maps to it), so we don't
// keep a separate set.
//
// Concurrency is left to SQLite: WAL mode plus a busy_timeout let the read pool
// and the single writer coexist without explicit locking, so the Go-side mutex
// the map store needed is gone. An in-memory read cache in front of this is a
// deliberate follow-up — add it once we know how big entries get per build.

import (
	"database/sql"
	"errors"
	"fmt"

	_ "modernc.org/sqlite"
)

// hashEquivStore is the SQLite-backed hash-equivalence database. database/sql's
// connection pool plus SQLite WAL handle concurrent access; the type is safe for
// use by many connection goroutines at once.
type hashEquivStore struct {
	db *sql.DB
}

// hashEquivSchema is the table set, created idempotently on open.
const hashEquivSchema = `
CREATE TABLE IF NOT EXISTS unihashes (
	method   TEXT NOT NULL,
	taskhash TEXT NOT NULL,
	unihash  TEXT NOT NULL,
	PRIMARY KEY (method, taskhash)
);
CREATE INDEX IF NOT EXISTS unihashes_by_unihash ON unihashes (unihash);
CREATE TABLE IF NOT EXISTS outhashes (
	method   TEXT NOT NULL,
	outhash  TEXT NOT NULL,
	taskhash TEXT NOT NULL,
	unihash  TEXT NOT NULL,
	PRIMARY KEY (method, outhash)
);`

// openHashEquivStore opens (creating if absent) the SQLite database at path and
// applies the schema. WAL + busy_timeout are set per-connection via the DSN so
// they apply to every connection the pool opens. The caller owns Close.
func openHashEquivStore(path string) (*hashEquivStore, error) {
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}
	// sql.Open is lazy; Ping forces a real connection so a bad path or DSN fails
	// here rather than on the first request.
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect sqlite %q: %w", path, err)
	}
	if _, err := db.Exec(hashEquivSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply hashequiv schema: %w", err)
	}
	return &hashEquivStore{db: db}, nil
}

func (s *hashEquivStore) Close() error { return s.db.Close() }

// getEquivalent returns the unihash recorded for (method, taskhash). The bool is
// false on a clean miss; an error is a real query failure, which callers degrade
// to a miss so a broken DB can't stall a build.
func (s *hashEquivStore) getEquivalent(method, taskhash string) (string, bool, error) {
	var u string
	err := s.db.QueryRow(
		`SELECT unihash FROM unihashes WHERE method = ? AND taskhash = ?`,
		method, taskhash,
	).Scan(&u)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, err
	}
	return u, true, nil
}

// insertUnihash records (method, taskhash) -> unihash first-write-wins (INSERT OR
// IGNORE), then reads back the value now in effect — the just-inserted one, or
// the earlier winner if another report got there first. Rows are never updated or
// deleted, so the read-after-insert always returns the durable winner without a
// wrapping transaction.
func (s *hashEquivStore) insertUnihash(method, taskhash, unihash string) (string, error) {
	if _, err := s.db.Exec(
		`INSERT INTO unihashes (method, taskhash, unihash) VALUES (?, ?, ?)
		 ON CONFLICT (method, taskhash) DO NOTHING`,
		method, taskhash, unihash,
	); err != nil {
		return "", err
	}
	var inEffect string
	if err := s.db.QueryRow(
		`SELECT unihash FROM unihashes WHERE method = ? AND taskhash = ?`,
		method, taskhash,
	).Scan(&inEffect); err != nil {
		return "", err
	}
	return inEffect, nil
}

// unihashExists reports whether any taskhash maps to this unihash (backs
// exists-stream).
func (s *hashEquivStore) unihashExists(unihash string) (bool, error) {
	var exists bool
	err := s.db.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM unihashes WHERE unihash = ?)`,
		unihash,
	).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists, nil
}

// insertOuthash records an outhash -> (taskhash, unihash) mapping, first-write-
// wins per (method, outhash).
func (s *hashEquivStore) insertOuthash(rec outhashRecord) error {
	_, err := s.db.Exec(
		`INSERT INTO outhashes (method, outhash, taskhash, unihash) VALUES (?, ?, ?, ?)
		 ON CONFLICT (method, outhash) DO NOTHING`,
		rec.Method, rec.Outhash, rec.Taskhash, rec.Unihash,
	)
	return err
}

// getOuthash returns the record stored for (method, outhash).
func (s *hashEquivStore) getOuthash(method, outhash string) (outhashRecord, bool, error) {
	rec := outhashRecord{Method: method, Outhash: outhash}
	err := s.db.QueryRow(
		`SELECT taskhash, unihash FROM outhashes WHERE method = ? AND outhash = ?`,
		method, outhash,
	).Scan(&rec.Taskhash, &rec.Unihash)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return outhashRecord{}, false, nil
	case err != nil:
		return outhashRecord{}, false, err
	}
	return rec, true, nil
}
