package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

// envPrefix namespaces every flag's env var equivalent, e.g. --data-dir
// becomes YOCACHE_DATA_DIR.
const envPrefix = "YOCACHE_"

// multiValueFlags names flags built with flag.Func's repeat-to-accumulate
// pattern (--evict, --block-recipe) rather than a plain scalar. Their env var
// is comma-split into one Set call per element, mirroring repeated CLI use.
// Every other flag's env var is passed through unsplit, since a scalar value
// like --data-dir may legitimately contain a comma.
var multiValueFlags = map[string]bool{
	"evict":        true,
	"block-recipe": true,
}

// applyEnvDefaults lets YOCACHE_<FLAG> env vars set flag defaults before CLI
// parsing, so precedence ends up CLI flag > env var > compiled-in default.
// Must run after every flag is declared but before fs.Parse().
func applyEnvDefaults(fs *flag.FlagSet) error {
	var firstErr error
	fs.VisitAll(func(f *flag.Flag) {
		if firstErr != nil {
			return
		}
		name := envPrefix + strings.ToUpper(strings.ReplaceAll(f.Name, "-", "_"))
		v, ok := os.LookupEnv(name)
		if !ok {
			return
		}
		values := []string{v}
		if multiValueFlags[f.Name] {
			values = strings.Split(v, ",")
		}
		for _, part := range values {
			if err := f.Value.Set(part); err != nil {
				firstErr = fmt.Errorf("invalid %s=%q: %w", name, part, err)
				return
			}
		}
	})
	return firstErr
}