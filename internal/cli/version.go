package cli

import (
	"fmt"
	"runtime"
)

// version/commit/date are injected at build time via -ldflags
// (see .github/workflows/release.yml, and the README's "Building"
// section for the flags a local build can pass the same way) — "dev"/
// "unknown" is what a plain `go build`/`go install` without those flags
// produces, so `agentforge --version` always prints something rather
// than silently compiling in empty strings.
var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

// versionString is newRootCmd's version template — not cobra's default
// "<name> version <version>\n", so a build identifies the exact commit
// and Go toolchain it came from, not just the semver tag, which is what
// actually matters when a bug report names a version that was tagged
// days before the fix landed.
func versionString() string {
	return fmt.Sprintf("agentforge %s (%s, built %s, %s)\n", version, commit, date, runtime.Version())
}
