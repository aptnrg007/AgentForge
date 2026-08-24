package cli

import (
	"os"
	"path/filepath"
)

// defaultDBPath returns the default location for the local SQLite run
// store: ~/.agentforge/agentforge.db. Every command previously defaulted
// --db to the bare relative name "agentforge.db", which meant running
// agentforge from two different working directories silently pointed at
// two different databases. Falls back to the old relative name if the
// home directory can't be determined.
func defaultDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "agentforge.db"
	}
	return filepath.Join(home, ".agentforge", "agentforge.db")
}

// ensureDBDir creates the parent directory of a --db path if needed, so
// store.Open (which does not create directories) can create the file
// itself on first use.
func ensureDBDir(dbPath string) error {
	dir := filepath.Dir(dbPath)
	if dir == "" || dir == "." {
		return nil
	}
	return os.MkdirAll(dir, 0o755)
}

// defaultAuthToken returns AGENTFORGE_AUTH_TOKEN, the fallback for every
// --server command's --auth-token flag. `serve --auth-token` has no
// equivalent client-side flag today, so a daemon started with one is
// unreachable from run/runs/agents --server: every request 401s with no
// way to authenticate. The env var lets a shell export it once instead of
// passing --auth-token on every invocation (and keeps it out of shell
// history / process listings, unlike a literal flag value).
func defaultAuthToken() string {
	return os.Getenv("AGENTFORGE_AUTH_TOKEN")
}
