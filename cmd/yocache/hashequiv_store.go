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
//   - unihashes: (method, taskhash) -> unihash, first-write-wins. The decision:
//     which unihash a taskhash should use.
//   - outhashes_v2: one row per (method, taskhash, outhash) report, with its
//     creation time. The evidence: which taskhashes produced an output. Cross-
//     output equivalence picks the oldest earlier reporter of the same output
//     and takes that taskhash's unihash from unihashes, as bitbake's
//     get_equivalent_for_outhash does.
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
	"time"
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
		DELETE FROM outhashes_v2
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
		DELETE FROM outhashes_v2 WHERE NOT EXISTS (
			SELECT 1 FROM unihashes u
			WHERE u.method = outhashes_v2.method AND u.taskhash = outhashes_v2.taskhash
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

// isoTimestamp formats t as bitbake hashserv stores outhashes_v2.created
// (Python's datetime.isoformat() in UTC). Fixed width, so the column sorts
// chronologically as text.
func isoTimestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000000+00:00")
}

// nullIfEmpty maps "" (field not sent) to SQL NULL.
func nullIfEmpty(v string) sql.NullString {
	return sql.NullString{String: v, Valid: v != ""}
}

// outhashColumns is the optional part of an outhashes_v2 row, in the order
// scanOuthashColumns reads it. created is cast to TEXT because the driver
// parses a DATETIME column into a time.Time and re-formats it on Scan, which
// would not hand back the timestamp as stored.
const outhashColumns = `CAST(o.created AS TEXT), o.owner, o.PN, o.PV, o.PR, o.task, o.outhash_siginfo`

// scanOuthashColumns returns Scan destinations for outhashColumns, and a
// function that copies the scanned values into rec ("" for NULL).
func scanOuthashColumns(rec *outhashRecord) ([]any, func()) {
	var vals [7]sql.NullString
	dest := make([]any, len(vals))
	for i := range vals {
		dest[i] = &vals[i]
	}
	return dest, func() {
		rec.Created, rec.Owner, rec.PN, rec.PV = vals[0].String, vals[1].String, vals[2].String, vals[3].String
		rec.PR, rec.Task, rec.OuthashSiginfo = vals[4].String, vals[5].String, vals[6].String
	}
}

// insertOuthash records that rec.Taskhash produced rec.Outhash (INSERT OR
// IGNORE on the (method, taskhash, outhash) triple). inserted is false when the
// same triple was already on record — as in bitbake's hashserv, the caller only
// looks for an equivalent output on a new row.
func (s *hashEquivStore) insertOuthash(rec outhashRecord) (inserted bool, err error) {
	res, err := s.db.Exec(
		`INSERT OR IGNORE INTO outhashes_v2
		   (method, taskhash, outhash, created, owner, PN, PV, PR, task, outhash_siginfo)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.Method, rec.Taskhash, rec.Outhash, isoTimestamp(time.Now()),
		nullIfEmpty(rec.Owner), nullIfEmpty(rec.PN), nullIfEmpty(rec.PV), nullIfEmpty(rec.PR),
		nullIfEmpty(rec.Task), nullIfEmpty(rec.OuthashSiginfo),
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
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
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM outhashes_v2`).Scan(&st.Outhashes); err != nil {
		return nil, fmt.Errorf("hashequiv stats outhashes: %w", err)
	}
	return st, nil
}

// equivalentForOuthash returns the unihash in effect for the oldest other
// taskhash that reported (method, outhash) — the cross-output equivalence
// target for taskhash, as in bitbake's get_equivalent_for_outhash. With
// requireSstate, reporters whose unihash has no stored archive are skipped, so
// the next-oldest one with an archive wins; if none has one, ok is false and
// the caller keeps its own unihash. id breaks ties between equal timestamps
// (and between rows copied from the old table, which have none).
func (s *hashEquivStore) equivalentForOuthash(method, outhash, taskhash string) (string, bool, error) {
	var u string
	err := s.db.QueryRow(
		`SELECT u.unihash FROM outhashes_v2 o
		 JOIN unihashes u ON u.method = o.method AND u.taskhash = o.taskhash
		 WHERE o.method = ? AND o.outhash = ? AND o.taskhash != ?`+
			s.backedFilter("u.unihash")+`
		 ORDER BY o.created ASC, o.id ASC
		 LIMIT 1`,
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

// getOuthash returns the oldest report of (method, outhash). With withUnihash
// (bitbake's get_unihash_by_outhash) it only considers reports whose taskhash
// has a unihash, fills rec.Unihash with it, and with requireSstate skips those
// whose archive is gone — another hash server using this one as its upstream
// adopts that unihash. Without it (bitbake's get_outhash), it is the plain
// oldest report and rec.Unihash stays empty.
func (s *hashEquivStore) getOuthash(method, outhash string, withUnihash bool) (outhashRecord, bool, error) {
	rec := outhashRecord{Method: method, Outhash: outhash}
	optional, fill := scanOuthashColumns(&rec)
	var err error
	if withUnihash {
		err = s.db.QueryRow(
			`SELECT o.taskhash, u.unihash, `+outhashColumns+` FROM outhashes_v2 o
			 JOIN unihashes u ON u.method = o.method AND u.taskhash = o.taskhash
			 WHERE o.method = ? AND o.outhash = ?`+
				s.backedFilter("u.unihash")+`
			 ORDER BY o.created ASC, o.id ASC
			 LIMIT 1`,
			method, outhash,
		).Scan(append([]any{&rec.Taskhash, &rec.Unihash}, optional...)...)
	} else {
		err = s.db.QueryRow(
			`SELECT o.taskhash, `+outhashColumns+` FROM outhashes_v2 o
			 WHERE o.method = ? AND o.outhash = ?
			 ORDER BY o.created ASC, o.id ASC
			 LIMIT 1`,
			method, outhash,
		).Scan(append([]any{&rec.Taskhash}, optional...)...)
	}
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return outhashRecord{}, false, nil
	case err != nil:
		return outhashRecord{}, false, err
	}
	fill()
	return rec, true, nil
}
