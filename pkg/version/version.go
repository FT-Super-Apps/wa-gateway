// Package version exposes build metadata injected at compile time via
// -ldflags "-X wa-gateway/pkg/version.Version=..." (see Dockerfile and
// scripts/version-ldflags.sh). Defaults describe a local, untagged build.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
)

var (
	// Version is the semantic version from the VERSION file (e.g. "1.1.0").
	Version = "0.0.0-dev"
	// Commit is the short git SHA the binary was built from.
	Commit = "unknown"
	// BuildTime is the UTC build timestamp (RFC 3339).
	BuildTime = "unknown"
	// BuildNumber is the CI run number; "0" for local builds.
	BuildNumber = "0"
)

// Info is the JSON-serialisable view of the build metadata.
type Info struct {
	Version     string `json:"version"`
	Commit      string `json:"commit"`
	BuildTime   string `json:"build_time"`
	BuildNumber string `json:"build_number"`
	GoVersion   string `json:"go_version"`
}

func init() {
	// Fall back to VCS info stamped by `go build` when ldflags were not given
	// (typical for `go run` during development).
	if Commit != "unknown" {
		return
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if len(s.Value) >= 7 {
				Commit = s.Value[:7]
			}
		case "vcs.time":
			BuildTime = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				Commit += "-dirty"
			}
		}
	}
}

// Get returns the current build metadata.
func Get() Info {
	return Info{
		Version:     Version,
		Commit:      Commit,
		BuildTime:   BuildTime,
		BuildNumber: BuildNumber,
		GoVersion:   runtime.Version(),
	}
}

// String renders "v1.1.0 (build 128, abc1234)".
func String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "v%s", Version)
	if BuildNumber != "0" {
		fmt.Fprintf(&b, " (build %s, %s)", BuildNumber, Commit)
	} else {
		fmt.Fprintf(&b, " (%s)", Commit)
	}
	return b.String()
}
