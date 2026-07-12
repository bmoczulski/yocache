package main

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/dustin/go-humanize"
)

// CategoryStats returns the file count and cumulative size in bytes for all
// inventory-tracked blobs of the given kind ("downloads" or "sstate").
func (b *blobInventory) CategoryStats(kind string) (files, sizeBytes int64, err error) {
	err = b.db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(size), 0) FROM blobs WHERE kind = ?`, kind,
	).Scan(&files, &sizeBytes)
	if err != nil {
		return 0, 0, fmt.Errorf("inventory category stats %s: %w", kind, err)
	}
	return files, sizeBytes, nil
}

// SstateStats returns sstate's file count, cumulative size in bytes, and the
// deduplicated recipe count: a cached task output is stored as a .tar.zst plus
// a .siginfo sidecar that share the same content hash in their filename, so
// they count as one recipe rather than two files.
func (b *blobInventory) SstateStats() (files, recipes, sizeBytes int64, err error) {
	rows, err := b.db.Query(`SELECT path, size FROM blobs WHERE kind = 'sstate'`)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("inventory sstate stats: %w", err)
	}
	defer rows.Close()

	seen := make(map[string]struct{})
	for rows.Next() {
		var path string
		var size int64
		if err := rows.Scan(&path, &size); err != nil {
			return 0, 0, 0, fmt.Errorf("inventory sstate stats scan: %w", err)
		}
		files++
		sizeBytes += size
		seen[sstateChecksum(path)] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return 0, 0, 0, fmt.Errorf("inventory sstate stats: %w", err)
	}
	return files, int64(len(seen)), sizeBytes, nil
}

// sstateChecksum extracts the content-hash field from a stored sstate blob
// path, e.g. "37/00/sstate:ninja-native::1.13.2:r0::14:37001365…ba34_patch.tar.zst.siginfo"
// -> "37001365…ba34". That hash is the dedup key SstateStats uses to count a
// task's .tar.zst and its .siginfo sidecar as a single recipe.
func sstateChecksum(path string) string {
	base := filepath.Base(path)
	if i := strings.LastIndex(base, ":"); i != -1 {
		base = base[i+1:]
	}
	if i := strings.IndexByte(base, '_'); i != -1 {
		base = base[:i]
	}
	return base
}

// logStartupStats emits a single log line summarizing what's already in the
// blob stores at startup: file counts, the deduplicated sstate recipe count,
// and cumulative size per category (exact bytes plus a human-readable form).
func logStartupStats(log *slog.Logger, inv *blobInventory) error {
	dlFiles, dlBytes, err := inv.CategoryStats("downloads")
	if err != nil {
		return err
	}
	ssFiles, ssRecipes, ssBytes, err := inv.SstateStats()
	if err != nil {
		return err
	}
	log.Info("cache inventory",
		"downloads_files", dlFiles,
		"downloads_bytes", dlBytes, "downloads_size", humanize.Bytes(uint64(dlBytes)),
		"sstate_files", ssFiles,
		"sstate_recipes", ssRecipes,
		"sstate_bytes", ssBytes, "sstate_size", humanize.Bytes(uint64(ssBytes)),
	)
	return nil
}
