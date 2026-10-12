package cli

import (
	"strings"
	"testing"
)

func TestVersionStringIncludesAllFields(t *testing.T) {
	origVersion, origCommit, origDate := version, commit, date
	t.Cleanup(func() { version, commit, date = origVersion, origCommit, origDate })

	version, commit, date = "v1.2.3", "abc1234", "2026-01-01T00:00:00Z"
	got := versionString()
	for _, want := range []string{"agentforge", "v1.2.3", "abc1234", "2026-01-01T00:00:00Z"} {
		if !strings.Contains(got, want) {
			t.Errorf("versionString() = %q, missing %q", got, want)
		}
	}
}

func TestNewRootCmdWiresVersionFlag(t *testing.T) {
	version = "v9.9.9"
	t.Cleanup(func() { version = "dev" })

	root := newRootCmd()
	root.SetArgs([]string{"--version"})
	var out strings.Builder
	root.SetOut(&out)
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out.String(), "v9.9.9") {
		t.Errorf("--version output = %q, want it to contain %q", out.String(), "v9.9.9")
	}
}
