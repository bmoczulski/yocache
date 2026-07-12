package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// blobInventory tracks per-blob metadata (size, add time, last-access time) in
// the shared operational SQLite database. It is the data source for eviction
// policies: LRU queries LRUCandidates ordered by accessed_at ascending.
//
// The *sql.DB is owned by main; this type has no lifecycle responsibility.
type blobInventory struct {
	db *sql.DB
}

func openBlobInventory(db *sql.DB) *blobInventory {
	return &blobInventory{db: db}
}

// blobRecord is a row returned by LRUCandidates.
type blobRecord struct {
	Kind string
	Path string
	Size int64
}

// Upsert records a blob after a successful PUT. For new blobs both added_at and
// accessed_at are set to now. For replacements (e.g. a growing VCS tarball) size
// and accessed_at are updated but added_at is preserved — it marks when this URL
// was first seen, which eviction bookkeeping may use later.
//
// buildname and buildMS attribute the blob to the build that produced it and
// how long (in milliseconds) that build's task took to produce it (empty
// buildname and negative buildMS mean "not provided" and are stored as NULL)
// — buildMS is only ever sent for sstate uploads. Milliseconds, not whole
// seconds: a fast (sub-second) task would otherwise truncate to 0, and
// summing many already-zeroed per-task values later would undercount even
// the aggregate.
func (b *blobInventory) Upsert(kind, path string, size int64, buildname string, buildMS int64) error {
	now := time.Now().Unix()
	var bn sql.NullString
	if buildname != "" {
		bn = sql.NullString{String: buildname, Valid: true}
	}
	var bs sql.NullInt64
	if buildMS >= 0 {
		bs = sql.NullInt64{Int64: buildMS, Valid: true}
	}
	_, err := b.db.Exec(`
		INSERT INTO blobs (kind, path, size, added_at, accessed_at, buildname, build_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (kind, path) DO UPDATE SET
			size        = excluded.size,
			accessed_at = excluded.accessed_at,
			buildname   = excluded.buildname,
			build_ms    = excluded.build_ms`,
		kind, path, size, now, now, bn, bs,
	)
	if err != nil {
		return fmt.Errorf("inventory upsert %s/%s: %w", kind, path, err)
	}
	return nil
}

// BuildMS returns the build_ms recorded for a blob at upload time (sstate
// only). ok is false if the blob has no inventory record, or has one but no
// recorded build time (downloads, or an sstate upload from before this
// tracking existed).
func (b *blobInventory) BuildMS(kind, path string) (ms int64, ok bool, err error) {
	var ns sql.NullInt64
	err = b.db.QueryRow(
		`SELECT build_ms FROM blobs WHERE kind = ? AND path = ?`, kind, path,
	).Scan(&ns)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("inventory build ms %s/%s: %w", kind, path, err)
	}
	return ns.Int64, ns.Valid, nil
}

// Touch updates accessed_at to now for a blob that was served on a cache hit.
// It is best-effort: the caller logs but does not fail on error.
func (b *blobInventory) Touch(kind, path string) error {
	_, err := b.db.Exec(
		`UPDATE blobs SET accessed_at = ? WHERE kind = ? AND path = ?`,
		time.Now().Unix(), kind, path,
	)
	if err != nil {
		return fmt.Errorf("inventory touch %s/%s: %w", kind, path, err)
	}
	return nil
}

// Remove deletes a blob's inventory record after it has been evicted from disk.
func (b *blobInventory) Remove(kind, path string) error {
	_, err := b.db.Exec(
		`DELETE FROM blobs WHERE kind = ? AND path = ?`,
		kind, path,
	)
	if err != nil {
		return fmt.Errorf("inventory remove %s/%s: %w", kind, path, err)
	}
	return nil
}

// LRUCandidates returns up to limit blobs ordered by accessed_at ascending
// (least-recently accessed first) — the eviction order for an LRU policy.
func (b *blobInventory) LRUCandidates(limit int) ([]blobRecord, error) {
	rows, err := b.db.Query(
		`SELECT kind, path, size FROM blobs ORDER BY accessed_at ASC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("inventory lru candidates: %w", err)
	}
	defer rows.Close()
	return scanBlobRecords(rows)
}

// LRUCandidatesByKind is LRUCandidates restricted to a single store kind — the
// eviction order for a kind-scoped LRU policy (e.g. lru-sstate), which must
// never surface or touch blobs of other kinds.
func (b *blobInventory) LRUCandidatesByKind(kind string, limit int) ([]blobRecord, error) {
	rows, err := b.db.Query(
		`SELECT kind, path, size FROM blobs WHERE kind = ? ORDER BY accessed_at ASC LIMIT ?`,
		kind, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("inventory lru candidates by kind: %w", err)
	}
	defer rows.Close()
	return scanBlobRecords(rows)
}

func scanBlobRecords(rows *sql.Rows) ([]blobRecord, error) {
	var out []blobRecord
	for rows.Next() {
		var r blobRecord
		if err := rows.Scan(&r.Kind, &r.Path, &r.Size); err != nil {
			return nil, fmt.Errorf("inventory lru scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Retrofit seeds the inventory for blobs already on disk that have no DB record
// yet — typically blobs present before this tracking was introduced. Blobs
// already tracked are left untouched so their accurate accessed_at is preserved.
//
// mtime is used for both added_at and accessed_at: a conservative approximation
// when the filesystem atime is unavailable or unreliable (noatime mounts).
func (b *blobInventory) Retrofit(stores map[string]string) error {
	for kind, dir := range stores {
		if err := b.retrofitStore(kind, dir); err != nil {
			return err
		}
	}
	return nil
}

// RecordBuildDownload attributes a cache hit to the requesting build.
// artifactKey is the dedup key for the fetched artifact (sstateChecksum(path)
// for sstate, path for downloads). bytes and ms are captured only on the
// first insert for a given (buildname, kind, artifactKey) — a build that
// re-fetches the same artifact (e.g. its .siginfo sidecar, or a retry) doesn't
// inflate "bytes downloaded" or double-count "time saved"; fetch_count still
// increments so repeat activity isn't silently lost.
func (b *blobInventory) RecordBuildDownload(buildname, kind, artifactKey string, bytes, ms int64) error {
	now := time.Now().Unix()
	_, err := b.db.Exec(`
		INSERT INTO build_downloads (buildname, kind, artifact_key, bytes, ms, fetch_count, first_seen, last_seen)
		VALUES (?, ?, ?, ?, ?, 1, ?, ?)
		ON CONFLICT (buildname, kind, artifact_key) DO UPDATE SET
			fetch_count = fetch_count + 1,
			last_seen   = excluded.last_seen`,
		buildname, kind, artifactKey, bytes, ms, now, now,
	)
	if err != nil {
		return fmt.Errorf("inventory record build download %s/%s/%s: %w", buildname, kind, artifactKey, err)
	}
	return nil
}

// GCBuildDownloads deletes build_downloads rows whose last_seen is older than
// ttl, keeping the table from growing forever. Returns the number of rows
// removed. Uses last_seen rather than first_seen so a long-running build's
// early downloads aren't GC'd out from under it before the build finishes.
func (b *blobInventory) GCBuildDownloads(ttl time.Duration) (int64, error) {
	cutoff := time.Now().Add(-ttl).Unix()
	res, err := b.db.Exec(`DELETE FROM build_downloads WHERE last_seen < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("inventory gc build downloads: %w", err)
	}
	return res.RowsAffected()
}

func (b *blobInventory) retrofitStore(kind, dir string) error {
	return filepath.WalkDir(dir, func(p string, e os.DirEntry, err error) error {
		if err != nil || e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			return nil
		}
		fi, err := e.Info()
		if err != nil {
			return nil // skip unreadable entry
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return nil
		}
		mtime := fi.ModTime().Unix()
		_, execErr := b.db.Exec(`
			INSERT INTO blobs (kind, path, size, added_at, accessed_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (kind, path) DO NOTHING`,
			kind, rel, fi.Size(), mtime, mtime,
		)
		return execErr
	})
}
