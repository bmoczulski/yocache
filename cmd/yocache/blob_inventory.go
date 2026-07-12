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
func (b *blobInventory) Upsert(kind, path string, size int64) error {
	now := time.Now().Unix()
	_, err := b.db.Exec(`
		INSERT INTO blobs (kind, path, size, added_at, accessed_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (kind, path) DO UPDATE SET
			size        = excluded.size,
			accessed_at = excluded.accessed_at`,
		kind, path, size, now, now,
	)
	if err != nil {
		return fmt.Errorf("inventory upsert %s/%s: %w", kind, path, err)
	}
	return nil
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
