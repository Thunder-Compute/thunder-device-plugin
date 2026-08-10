// Package version reports the build version of the Thunder device plugin and
// derives the User-Agent both components send to the Thunder API.
package version

import (
	"runtime/debug"
	"strings"
)

// Version is the release version. It is injected at link time:
//
//	go build -ldflags "-X github.com/Thunder-Compute/thunder-device-plugin/internal/version.Version=v1.2.3"
//
// When it is empty the version is read from the embedded build info, so
// `go install`ed binaries still report something useful.
var Version = ""

// Commit is the git revision, injected at link time the same way. When empty
// it falls back to the VCS stamp Go records in the build info.
var Commit = ""

const unknown = "dev"

// Get returns the build version, never an empty string.
func Get() string {
	if v := strings.TrimSpace(Version); v != "" {
		return v
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := strings.TrimSpace(info.Main.Version); v != "" && v != "(devel)" {
			return v
		}
	}
	return unknown
}

// Revision returns the git commit the binary was built from, or "unknown".
func Revision() string {
	if c := strings.TrimSpace(Commit); c != "" {
		return c
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" && setting.Value != "" {
				return setting.Value
			}
		}
	}
	return "unknown"
}

// UserAgent identifies a component of this build to the Thunder API. The
// Thunder SDK otherwise reports a generic "thunder-sdk/dev".
func UserAgent(component string) string {
	return "thunder-device-plugin/" + Get() + " (" + component + ")"
}
