package main

import (
	"database/sql"
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

// BuildUploadStats returns one build's own upload footprint: how many
// artifacts it contributed (deduplicating sstate's pkg+.siginfo(+.sig)
// sidecars by content hash, the same way SstateStats does for the global
// count), their cumulative size, and — sstate only — the total build time
// invested in milliseconds (each distinct hash's build_ms counted once, not
// once per sidecar file). Milliseconds, not seconds: summed here before any
// rounding, so fast (sub-second) tasks don't get truncated to 0 and vanish
// from the aggregate — computeBuildStats converts to whole seconds once, at
// the very end, for display.
func (b *blobInventory) BuildUploadStats(buildname string) (dlFiles, dlBytes, ssFiles, ssRecipes, ssBytes, ssMS int64, err error) {
	dlFiles, dlBytes, err = b.buildUploadCategoryStats(buildname, "downloads")
	if err != nil {
		return 0, 0, 0, 0, 0, 0, err
	}

	rows, err := b.db.Query(
		`SELECT path, size, build_ms FROM blobs WHERE kind = 'sstate' AND buildname = ?`, buildname,
	)
	if err != nil {
		return 0, 0, 0, 0, 0, 0, fmt.Errorf("inventory build upload stats %s: %w", buildname, err)
	}
	defer rows.Close()

	seen := make(map[string]struct{})
	for rows.Next() {
		var path string
		var size int64
		var ms sql.NullInt64
		if err := rows.Scan(&path, &size, &ms); err != nil {
			return 0, 0, 0, 0, 0, 0, fmt.Errorf("inventory build upload stats %s scan: %w", buildname, err)
		}
		ssFiles++
		ssBytes += size
		if csum := sstateChecksum(path); csum != "" {
			if _, dup := seen[csum]; !dup {
				seen[csum] = struct{}{}
				if ms.Valid {
					ssMS += ms.Int64
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return 0, 0, 0, 0, 0, 0, fmt.Errorf("inventory build upload stats %s: %w", buildname, err)
	}
	return dlFiles, dlBytes, ssFiles, int64(len(seen)), ssBytes, ssMS, nil
}

func (b *blobInventory) buildUploadCategoryStats(buildname, kind string) (files, sizeBytes int64, err error) {
	err = b.db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(size), 0) FROM blobs WHERE kind = ? AND buildname = ?`, kind, buildname,
	).Scan(&files, &sizeBytes)
	if err != nil {
		return 0, 0, fmt.Errorf("inventory build upload category stats %s/%s: %w", kind, buildname, err)
	}
	return files, sizeBytes, nil
}

// BuildDownloadStats returns one build's own download footprint, already
// deduplicated at write time by RecordBuildDownload's primary key. ssMS is
// milliseconds, summed before rounding — see BuildUploadStats.
func (b *blobInventory) BuildDownloadStats(buildname string) (dlFiles, dlBytes, ssFiles, ssBytes, ssMS int64, err error) {
	rows, err := b.db.Query(
		`SELECT kind, COUNT(*), COALESCE(SUM(bytes), 0), COALESCE(SUM(ms), 0)
		 FROM build_downloads WHERE buildname = ? GROUP BY kind`, buildname,
	)
	if err != nil {
		return 0, 0, 0, 0, 0, fmt.Errorf("inventory build download stats %s: %w", buildname, err)
	}
	defer rows.Close()

	for rows.Next() {
		var kind string
		var count, bytes, ms int64
		if err := rows.Scan(&kind, &count, &bytes, &ms); err != nil {
			return 0, 0, 0, 0, 0, fmt.Errorf("inventory build download stats %s scan: %w", buildname, err)
		}
		switch kind {
		case "downloads":
			dlFiles, dlBytes = count, bytes
		case "sstate":
			ssFiles, ssBytes, ssMS = count, bytes, ms
		}
	}
	if err := rows.Err(); err != nil {
		return 0, 0, 0, 0, 0, fmt.Errorf("inventory build download stats %s: %w", buildname, err)
	}
	return dlFiles, dlBytes, ssFiles, ssBytes, ssMS, nil
}

// buildDirectionStats is one direction (uploads or downloads) split by
// category, mirroring cacheStats' downloads/sstate split.
type buildDirectionStats struct {
	Downloads directionCount `json:"downloads"`
	Sstate    directionCount `json:"sstate"`
}

// directionCount is one category's count/bytes/seconds within a build's
// upload or download footprint. Seconds is omitted for the downloads
// category, which has no build-time concept.
type directionCount struct {
	Count   int64 `json:"count"`
	Bytes   int64 `json:"bytes"`
	Seconds int64 `json:"seconds,omitempty"`
}

// buildStats is the JSON shape of GET /api/build-stats/{buildname}.
type buildStats struct {
	BuildName string              `json:"build_name"`
	Uploads   buildDirectionStats `json:"uploads"`
	Downloads buildDirectionStats `json:"downloads"`
}

// computeBuildStats queries the inventory DB live for one build's upload and
// download footprint.
func computeBuildStats(inv *blobInventory, buildname string) (buildStats, error) {
	upDlFiles, upDlBytes, _, upSsRecipes, upSsBytes, upSsMS, err := inv.BuildUploadStats(buildname)
	if err != nil {
		return buildStats{}, err
	}
	dnDlFiles, dnDlBytes, dnSsFiles, dnSsBytes, dnSsMS, err := inv.BuildDownloadStats(buildname)
	if err != nil {
		return buildStats{}, err
	}
	return buildStats{
		BuildName: buildname,
		Uploads: buildDirectionStats{
			Downloads: directionCount{Count: upDlFiles, Bytes: upDlBytes},
			Sstate:    directionCount{Count: upSsRecipes, Bytes: upSsBytes, Seconds: roundMSToSeconds(upSsMS)},
		},
		Downloads: buildDirectionStats{
			Downloads: directionCount{Count: dnDlFiles, Bytes: dnDlBytes},
			Sstate:    directionCount{Count: dnSsFiles, Bytes: dnSsBytes, Seconds: roundMSToSeconds(dnSsMS)},
		},
	}, nil
}

// roundMSToSeconds converts milliseconds to the nearest whole second. Applied
// once, after all per-task millisecond values have already been summed —
// rounding any earlier would let fast (sub-second) tasks vanish from the
// aggregate instead of just from their own individual display.
func roundMSToSeconds(ms int64) int64 {
	return (ms + 500) / 1000
}

// buildStatsHandler serves GET /api/build-stats/{buildname}: one build's own
// upload and download footprint, computed fresh per request.
func buildStatsHandler(inv *blobInventory, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		buildname := r.PathValue("buildname")
		if buildname == "" {
			http.Error(w, "buildname required", http.StatusBadRequest)
			return
		}
		s, err := computeBuildStats(inv, buildname)
		if err != nil {
			log.Error("build stats handler failed", "err", err, "build_name", buildname, "remote", r.RemoteAddr)
			http.Error(w, "stats unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(s); err != nil {
			log.Error("build stats handler: encode failed", "err", err, "remote", r.RemoteAddr)
		}
	}
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
