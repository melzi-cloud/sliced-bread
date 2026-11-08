// Package version is the SlicedBread version of this build.
package version

import "runtime/debug"

// Version is set by release builds (-ldflags "-X .../internal/version.Version=v0.1.0").
var Version = ""

// Get returns the version: the release stamp, the module version of a `go install`ed
// build (go install github.com/melzi-cloud/sliced-bread/cmd/sb@v0.1.0), or "dev".
func Get() string {
	if Version != "" {
		return Version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}
