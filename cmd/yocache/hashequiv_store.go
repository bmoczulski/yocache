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
//
// The *sql.DB is opened and closed by main (db.go); this type is a pure
// query-method carrier with no lifecycle responsibility of its own.

import (
	"database/sql"
	"errors"
	"fmt"
)

// hashEquivStore is the SQLite-backed hash-equivalence database. database/sql's
// connection pool plus SQLite WAL handle concurrent access; the type is safe for
// use by many goroutines at once.
type hashEquivStore struct {
	db *sql.DB
}

// DB returns the underlying connection pool so other stores (e.g. blobInventory)
// can share the same SQLite file without opening a second connection.
func (s *hashEquivStore) DB() *sql.DB { return s.db }

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

// Stats returns counts describing the hash-equivalence store: TaskHashes is
// the number of recorded taskhash->unihash mappings, Unihashes is how many
// distinct unihashes those collapse to (the dedup signal — TaskHashes minus
// Unihashes is how many taskhashes were spared a rebuild), and Outhashes is
// the number of recorded outhash records.
func (s *hashEquivStore) Stats() (*hashEquivStats, error) {
	st := &hashEquivStats{}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM unihashes`).Scan(&st.TaskHashes); err != nil {
		return nil, fmt.Errorf("hashequiv stats taskhashes: %w", err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(DISTINCT unihash) FROM unihashes`).Scan(&st.Unihashes); err != nil {
		return nil, fmt.Errorf("hashequiv stats unihashes: %w", err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM outhashes`).Scan(&st.Outhashes); err != nil {
		return nil, fmt.Errorf("hashequiv stats outhashes: %w", err)
	}
	return st, nil
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
