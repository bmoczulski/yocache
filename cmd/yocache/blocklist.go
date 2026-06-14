package main

import (
	"path/filepath"
	"strings"
)

// recipeBlockList is a set of recipe base-names (PN) whose sstate blobs the
// server will unconditionally reject — reads and writes alike — to protect the
// cache from recipes known to produce non-reproducible or otherwise broken
// artifacts.
//
// Downloads blobs have no recipe identity in their filenames and are never
// matched by this mechanism.
type recipeBlockList map[string]struct{}

func newRecipeBlockList(names []string) recipeBlockList {
	bl := make(recipeBlockList, len(names))
	for _, n := range names {
		if s := strings.TrimSpace(n); s != "" {
			bl[s] = struct{}{}
		}
	}
	return bl
}

// blocked reports whether the named blob belongs to a blocked recipe.
// Only sstate blobs are matched: the base filename follows the bitbake pattern
// "sstate:<PN>:<arch>:<PV>:…" and the second colon-delimited field is the PN.
// For example "sstate:quilt:cortexa53-poky-linux:0.67:…" → PN is "quilt".
func (bl recipeBlockList) blocked(kind, name string) bool {
	if len(bl) == 0 || kind != "sstate" {
		return false
	}
	base := filepath.Base(name)
	if !strings.HasPrefix(base, "sstate:") {
		return false
	}
	// "sstate:<PN>:<arch>:…" — we need only the first two fields.
	parts := strings.SplitN(base, ":", 3)
	if len(parts) < 2 {
		return false
	}
	_, ok := bl[parts[1]]
	return ok
}
