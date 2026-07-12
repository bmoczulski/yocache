package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
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

// cacheStats is the file-count/size summary shared by the startup log line
// and the /api/stats endpoint. Sizes are duplicated as exact bytes and a
// human-readable form so either can be consumed without reformatting.
type cacheStats struct {
	DownloadsFiles int64  `json:"downloads_files"`
	DownloadsBytes int64  `json:"downloads_bytes"`
	DownloadsSize  string `json:"downloads_size,omitempty"`
	SstateFiles    int64  `json:"sstate_files"`
	SstateRecipes  int64  `json:"sstate_recipes"`
	SstateBytes    int64  `json:"sstate_bytes"`
	SstateSize     string `json:"sstate_size,omitempty"`
}

// computeCacheStats queries the inventory DB live, so it reflects uploads and
// evictions that happened since startup, not just the state at boot.
func computeCacheStats(inv *blobInventory) (cacheStats, error) {
	dlFiles, dlBytes, err := inv.CategoryStats("downloads")
	if err != nil {
		return cacheStats{}, err
	}
	ssFiles, ssRecipes, ssBytes, err := inv.SstateStats()
	if err != nil {
		return cacheStats{}, err
	}
	return cacheStats{
		DownloadsFiles: dlFiles,
		DownloadsBytes: dlBytes,
		DownloadsSize:  humanize.Bytes(uint64(dlBytes)),
		SstateFiles:    ssFiles,
		SstateRecipes:  ssRecipes,
		SstateBytes:    ssBytes,
		SstateSize:     humanize.Bytes(uint64(ssBytes)),
	}, nil
}

// logStartupStats emits a single log line summarizing what's already in the
// blob stores at startup: file counts, the deduplicated sstate recipe count,
// and cumulative size per category (exact bytes plus a human-readable form).
func logStartupStats(log *slog.Logger, inv *blobInventory) error {
	s, err := computeCacheStats(inv)
	if err != nil {
		return err
	}
	log.Info("cache inventory",
		"downloads_files", s.DownloadsFiles,
		"downloads_bytes", s.DownloadsBytes, "downloads_size", s.DownloadsSize,
		"sstate_files", s.SstateFiles,
		"sstate_recipes", s.SstateRecipes,
		"sstate_bytes", s.SstateBytes, "sstate_size", s.SstateSize,
	)
	return nil
}

// statsHandler serves the same summary as logStartupStats as JSON, computed
// fresh per request so it stays current between startups.
func statsHandler(inv *blobInventory, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, err := computeCacheStats(inv)
		if err != nil {
			log.Error("stats handler failed", "err", err, "remote", r.RemoteAddr)
			http.Error(w, "stats unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(s); err != nil {
			log.Error("stats handler: encode failed", "err", err, "remote", r.RemoteAddr)
		}
	}
}
