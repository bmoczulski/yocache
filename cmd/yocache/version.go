package main

import (
	"encoding/json"
	"net/http"
	"runtime/debug"
)

type versionInfo struct {
	Version  string `json:"version"`
	Revision string `json:"revision,omitempty"`
	Time     string `json:"time,omitempty"`
	Modified bool   `json:"modified,omitempty"`
}

// buildVersionInfo reads VCS stamps embedded by the Go toolchain at build time.
// Version is the module version from build info (a semver tag for tagged commits,
// a pseudo-version otherwise), falling back to a short commit hash when the
// module version is unavailable (e.g. go run).
func buildVersionInfo() versionInfo {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return versionInfo{Version: "unknown"}
	}

	var revision, vcsTime string
	var modified bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.time":
			vcsTime = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}

	version := info.Main.Version
	if version == "" || version == "(devel)" {
		if revision != "" {
			if len(revision) > 12 {
				version = revision[:12]
			} else {
				version = revision
			}
		} else {
			version = "unknown"
		}
	}

	return versionInfo{
		Version:  version,
		Revision: revision,
		Time:     vcsTime,
		Modified: modified,
	}
}

func versionHandler(ver versionInfo) http.HandlerFunc {
	body, _ := json.Marshal(ver)
	body = append(body, '\n')
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}
}
