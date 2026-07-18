package main

import (
	"flag"
	"reflect"
	"testing"
	"time"
)

// newTestFlagSet builds a flag.FlagSet shaped like main()'s, so
// applyEnvDefaults is exercised against the same flag names/types it runs
// against in production.
func newTestFlagSet() (fs *flag.FlagSet, addr, dataDir *string, buildStatsTTL *time.Duration, evict, blockRecipe *[]string) {
	fs = flag.NewFlagSet("test", flag.ContinueOnError)
	addr = fs.String("addr", ":6768", "")
	dataDir = fs.String("data-dir", "var", "")
	buildStatsTTL = fs.Duration("build-stats-ttl", 720*time.Hour, "")
	evict = &[]string{}
	fs.Func("evict", "", func(v string) error {
		*evict = append(*evict, v)
		return nil
	})
	blockRecipe = &[]string{}
	fs.Func("block-recipe", "", func(v string) error {
		*blockRecipe = append(*blockRecipe, v)
		return nil
	})
	return fs, addr, dataDir, buildStatsTTL, evict, blockRecipe
}

func TestApplyEnvDefaultsScalarFlag(t *testing.T) {
	t.Setenv("YOCACHE_ADDR", "127.0.0.1:9999")
	fs, addr, _, _, _, _ := newTestFlagSet()

	if err := applyEnvDefaults(fs); err != nil {
		t.Fatalf("applyEnvDefaults: %v", err)
	}
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if *addr != "127.0.0.1:9999" {
		t.Errorf("addr = %q, want env override", *addr)
	}
}

func TestApplyEnvDefaultsMultiValueFlag(t *testing.T) {
	t.Setenv("YOCACHE_EVICT", "lru,lru-sstate")
	fs, _, _, _, evict, _ := newTestFlagSet()

	if err := applyEnvDefaults(fs); err != nil {
		t.Fatalf("applyEnvDefaults: %v", err)
	}
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []string{"lru", "lru-sstate"}
	if !reflect.DeepEqual(*evict, want) {
		t.Errorf("evict = %v, want %v", *evict, want)
	}
}

func TestApplyEnvDefaultsCLIWinsOverEnv(t *testing.T) {
	t.Setenv("YOCACHE_ADDR", "127.0.0.1:9999")
	fs, addr, _, _, _, _ := newTestFlagSet()

	if err := applyEnvDefaults(fs); err != nil {
		t.Fatalf("applyEnvDefaults: %v", err)
	}
	if err := fs.Parse([]string{"-addr", ":1234"}); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if *addr != ":1234" {
		t.Errorf("addr = %q, want explicit CLI flag to win over env", *addr)
	}
}

func TestApplyEnvDefaultsScalarNotSplitOnComma(t *testing.T) {
	// A scalar flag's value (e.g. a path) may legitimately contain a comma;
	// only flags in multiValueFlags get split.
	t.Setenv("YOCACHE_DATA_DIR", "/mnt/a,b/yocache")
	fs, _, dataDir, _, _, _ := newTestFlagSet()

	if err := applyEnvDefaults(fs); err != nil {
		t.Fatalf("applyEnvDefaults: %v", err)
	}
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if *dataDir != "/mnt/a,b/yocache" {
		t.Errorf("dataDir = %q, want comma left intact", *dataDir)
	}
}

func TestApplyEnvDefaultsInvalidValue(t *testing.T) {
	t.Setenv("YOCACHE_BUILD_STATS_TTL", "not-a-duration")
	fs, _, _, _, _, _ := newTestFlagSet()

	if err := applyEnvDefaults(fs); err == nil {
		t.Fatal("applyEnvDefaults: want error for invalid duration, got nil")
	}
}

func TestApplyEnvDefaultsNoEnvLeavesDefault(t *testing.T) {
	fs, addr, _, _, _, _ := newTestFlagSet()

	if err := applyEnvDefaults(fs); err != nil {
		t.Fatalf("applyEnvDefaults: %v", err)
	}
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if *addr != ":6768" {
		t.Errorf("addr = %q, want compiled-in default when no env/CLI set", *addr)
	}
}
