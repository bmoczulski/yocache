package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
)

func TestSstateChecksum(t *testing.T) {
	cases := []struct {
		name, path, want string
	}{
		{
			name: "artifact",
			path: "37/00/sstate:ninja-native::1.13.2:r0::14:37001365f620ee00a3177d608f4c5a428edd973c714942c7fea891040660ba34_patch.tar.zst",
			want: "37001365f620ee00a3177d608f4c5a428edd973c714942c7fea891040660ba34",
		},
		{
			name: "siginfo sidecar shares the artifact's checksum",
			path: "37/00/sstate:ninja-native::1.13.2:r0::14:37001365f620ee00a3177d608f4c5a428edd973c714942c7fea891040660ba34_patch.tar.zst.siginfo",
			want: "37001365f620ee00a3177d608f4c5a428edd973c714942c7fea891040660ba34",
		},
		{
			name: "no directory prefix",
			path: "sstate:curl::8.19.0:r0::14:372038f5e66ef6eaf2d5f847a0f07ace84cd0e69ab59ba61d30993a5bfda910c_patch.tar.zst",
			want: "372038f5e66ef6eaf2d5f847a0f07ace84cd0e69ab59ba61d30993a5bfda910c",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sstateChecksum(c.path); got != c.want {
				t.Errorf("sstateChecksum(%q) = %q, want %q", c.path, got, c.want)
			}
		})
	}
}

func TestCategoryStats(t *testing.T) {
	inv, db := newTestInventory(t)
	insertBlobAt(t, db, "downloads", "a.tar.gz", 100, 1)
	insertBlobAt(t, db, "downloads", "b.tar.gz", 250, 2)
	insertBlobAt(t, db, "sstate", "37/00/sstate:foo::1:r0::14:abc_patch.tar.zst", 10, 3)

	files, size, err := inv.CategoryStats("downloads")
	if err != nil {
		t.Fatalf("CategoryStats: %v", err)
	}
	if files != 2 || size != 350 {
		t.Errorf("CategoryStats(downloads) = (%d, %d), want (2, 350)", files, size)
	}
}

func TestSstateStatsDedupesByChecksum(t *testing.T) {
	inv, db := newTestInventory(t)
	// Same recipe/hash, two sidecar files: counts as one recipe, two files.
	insertBlobAt(t, db, "sstate",
		"37/00/sstate:ninja-native::1.13.2:r0::14:37001365f620ee00a3177d608f4c5a428edd973c714942c7fea891040660ba34_patch.tar.zst",
		1000, 1)
	insertBlobAt(t, db, "sstate",
		"37/00/sstate:ninja-native::1.13.2:r0::14:37001365f620ee00a3177d608f4c5a428edd973c714942c7fea891040660ba34_patch.tar.zst.siginfo",
		20, 2)
	// A different recipe/hash.
	insertBlobAt(t, db, "sstate",
		"3a/38/sstate:perl:x86-64-v3-poky-linux:5.42.0:r0:x86-64-v3:14:3a387d44c6d044b39daa846af139bb9f0996e654cff18677c6fac53f03312469_package_qa.tar.zst",
		2000, 3)

	files, recipes, size, err := inv.SstateStats()
	if err != nil {
		t.Fatalf("SstateStats: %v", err)
	}
	if files != 3 {
		t.Errorf("files = %d, want 3", files)
	}
	if recipes != 2 {
		t.Errorf("recipes = %d, want 2", recipes)
	}
	if size != 3020 {
		t.Errorf("size = %d, want 3020", size)
	}
}

func TestStatsHandler(t *testing.T) {
	inv, db := newTestInventory(t)
	insertBlobAt(t, db, "downloads", "a.tar.gz", 100, 1)
	insertBlobAt(t, db, "sstate",
		"37/00/sstate:foo::1:r0::14:37001365f620ee00a3177d608f4c5a428edd973c714942c7fea891040660ba34_patch.tar.zst",
		10, 2)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	req := httptest.NewRequest("GET", "/api/stats", nil)
	rec := httptest.NewRecorder()
	statsHandler(inv, log)(rec, req)

	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var got cacheStats
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	want := cacheStats{
		DownloadsFiles: 1, DownloadsBytes: 100, DownloadsSize: "100 B",
		SstateFiles: 1, SstateRecipes: 1, SstateBytes: 10, SstateSize: "10 B",
	}
	if got != want {
		t.Errorf("stats = %+v, want %+v", got, want)
	}
}
