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
//   - outhashes: (method, outhash)  -> the taskhash/unihash that produced it,
//     also first-write-wins, which is what makes it usable for cross-output
//     equivalence: the one surviving row for an outhash is always the oldest
//     taskhash that produced it, exactly what hashequiv.go's handleReport needs
//     to unify a later, differently-hashed taskhash onto it.
// unihashExists is answered from the unihashes table (a unihash "exists" if some
// taskhash maps to it), so we don't keep a separate set.
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

	// requireSstate limits every answer to unihashes whose sstate archive this
	// server stores (per the blobs inventory). Without it, an entry whose
	// archive is gone (evicted before hash-equivalence GC existed, deleted by
	// hand, or never uploaded because the PUT failed — bitbake reports hash
	// equivalence before it even creates the archive) still redirects a build
	// onto that unihash. Bitbake then rehashes and rechecks everything
	// downstream of the task, only to miss on all of it and rebuild anyway.
	requireSstate bool
}

// sstateBacked is an SQL condition that holds when the unihash in column col
// has its sstate archive in the blobs inventory. A group left with only its
// .siginfo/.sig sidecars doesn't count: bitbake can't restore from those.
func sstateBacked(col string) string {
	return `EXISTS (SELECT 1 FROM blobs b WHERE b.kind = 'sstate' AND b.checksum = ` + col +
		` AND b.path NOT LIKE '%.siginfo' AND b.path NOT LIKE '%.sig')`
}

// backedFilter returns " AND <sstateBacked(col)>" when requireSstate is set,
// else "".
func (s *hashEquivStore) backedFilter(col string) string {
	if !s.requireSstate {
		return ""
	}
	return " AND " + sstateBacked(col)
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
		`SELECT unihash FROM unihashes WHERE method = ? AND taskhash = ?`+
			s.backedFilter("unihashes.unihash"),
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

// insertUnihash records (method, taskhash) -> unihash first-write-wins, then
// reads back the value now in effect — the just-inserted one, or the earlier
// winner if another report got there first. With requireSstate, an earlier
// winner whose archive is gone loses to the new value instead: keeping it
// would hand the build a unihash it can't restore from. The read-after-insert
// needs no wrapping transaction: a row only changes away from an unbacked
// unihash, so whatever it reads is the durable answer.
func (s *hashEquivStore) insertUnihash(method, taskhash, unihash string) (string, error) {
	onConflict := `DO NOTHING`
	if s.requireSstate {
		onConflict = `DO UPDATE SET unihash = excluded.unihash WHERE NOT ` + sstateBacked("unihashes.unihash")
	}
	if _, err := s.db.Exec(
		`INSERT INTO unihashes (method, taskhash, unihash) VALUES (?, ?, ?)
		 ON CONFLICT (method, taskhash) `+onConflict,
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

// DeleteByUnihash removes every unihashes/outhashes row tied to unihash.
// Called when the sstate blob(s) backing that unihash have been fully
// evicted: an equivalence entry pointing at a blob we no longer have would
// otherwise cost bitbake a wasted round trip instead of an immediate,
// correct fallback to its own taskhash.
func (s *hashEquivStore) DeleteByUnihash(unihash string) (unihashRows, outhashRows int64, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()

	// outhashes first, while the unihashes rows it joins against still exist.
	res, err := tx.Exec(`
		DELETE FROM outhashes
		WHERE (method, taskhash) IN (
			SELECT method, taskhash FROM unihashes WHERE unihash = ?
		)`, unihash,
	)
	if err != nil {
		return 0, 0, err
	}
	outhashRows, _ = res.RowsAffected()

	res, err = tx.Exec(`DELETE FROM unihashes WHERE unihash = ?`, unihash)
	if err != nil {
		return 0, outhashRows, err
	}
	unihashRows, _ = res.RowsAffected()

	return unihashRows, outhashRows, tx.Commit()
}

// PruneUnbacked deletes every unihashes row whose sstate archive isn't in the
// blobs inventory, then every outhashes row left without a unihashes row for
// its taskhash. Lookups already ignore such entries when requireSstate is set;
// this keeps them from piling up. Meant for startup, after the inventory has
// been reconciled against disk and before any build can be mid-upload: at
// runtime an entry legitimately has no archive for the moment between
// bitbake's report and its PUT landing.
func (s *hashEquivStore) PruneUnbacked() (unihashRows, outhashRows int64, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`DELETE FROM unihashes WHERE NOT ` + sstateBacked("unihashes.unihash"))
	if err != nil {
		return 0, 0, err
	}
	unihashRows, _ = res.RowsAffected()

	res, err = tx.Exec(`
		DELETE FROM outhashes WHERE NOT EXISTS (
			SELECT 1 FROM unihashes u
			WHERE u.method = outhashes.method AND u.taskhash = outhashes.taskhash
		)`)
	if err != nil {
		return unihashRows, 0, err
	}
	outhashRows, _ = res.RowsAffected()

	return unihashRows, outhashRows, tx.Commit()
}

// unihashExists reports whether any taskhash maps to this unihash (backs
// exists-stream).
func (s *hashEquivStore) unihashExists(unihash string) (bool, error) {
	var exists bool
	err := s.db.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM unihashes WHERE unihash = ?`+
			s.backedFilter("unihashes.unihash")+`)`,
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

// unifiedForOuthash returns the unihash in effect for the earlier, different
// taskhash that first reported (method, outhash) — the cross-output
// equivalence target for taskhash, as in bitbake's get_equivalent_for_outhash.
// It reads that taskhash's current unihashes row rather than the unihash
// stored on the outhash row, so it follows any later repair of that row. With
// requireSstate the target must have its archive stored; otherwise ok is false
// and the caller keeps its own unihash.
func (s *hashEquivStore) unifiedForOuthash(method, outhash, taskhash string) (string, bool, error) {
	var u string
	err := s.db.QueryRow(
		`SELECT u.unihash FROM outhashes o
		 JOIN unihashes u ON u.method = o.method AND u.taskhash = o.taskhash
		 WHERE o.method = ? AND o.outhash = ? AND o.taskhash != ?`+
			s.backedFilter("u.unihash"),
		method, outhash, taskhash,
	).Scan(&u)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, err
	}
	return u, true, nil
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
