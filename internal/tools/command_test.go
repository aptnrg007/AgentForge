package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentforge/internal/config"
)

func mustBuildOneWithSourceDir(t *testing.T, def config.ToolDefinition, sourceDir string) func(ctx context.Context, input json.RawMessage) (string, error) {
	t.Helper()
	cfg := &config.Config{
		Name:            "test",
		Model:           config.ModelConfig{Provider: "ollama", Name: "test-model"},
		ToolDefinitions: []config.ToolDefinition{def},
		SourceDir:       sourceDir,
	}
	tools, err := Build(cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return tools[0].Execute
}

// TestCommandToolNoShellInvolved is the no-shell proof: a rendered value
// containing shell metacharacters must reach the process as one inert
// argv element, never interpreted by a shell.
func TestCommandToolNoShellInvolved(t *testing.T) {
	exec := mustBuildOneWithSourceDir(t, config.ToolDefinition{
		Name: "echo", Description: "test", InputSchema: objSchema("msg"),
		Command: &config.CommandToolConfig{Argv: []string{"/bin/echo", "{{.msg}}"}},
	}, "")

	out, err := exec(context.Background(), json.RawMessage(`{"msg":"hi; touch /tmp/should-not-exist-agentforge-test; echo pwned"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	want := "hi; touch /tmp/should-not-exist-agentforge-test; echo pwned\n"
	if out != want {
		t.Fatalf("out = %q, want %q (the whole string echoed literally, not shell-interpreted)", out, want)
	}
	if _, err := os.Stat("/tmp/should-not-exist-agentforge-test"); err == nil {
		_ = os.Remove("/tmp/should-not-exist-agentforge-test")
		t.Fatal("the embedded `touch` ran — input reached a shell")
	}
}

func TestCommandToolNonZeroExitSurfacesStderr(t *testing.T) {
	exec := mustBuildOneWithSourceDir(t, config.ToolDefinition{
		Name: "fail", Description: "test", InputSchema: json.RawMessage(`{"type":"object"}`),
		Command: &config.CommandToolConfig{Argv: []string{"/bin/sh", "-c", "echo boom >&2; exit 3"}},
	}, "")

	_, err := exec(context.Background(), json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected an error for a non-zero exit")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error = %v, want it to include captured stderr", err)
	}
}

func TestCommandToolOutputTruncatedAtCap(t *testing.T) {
	exec := mustBuildOneWithSourceDir(t, config.ToolDefinition{
		Name: "big", Description: "test", InputSchema: json.RawMessage(`{"type":"object"}`),
		Command: &config.CommandToolConfig{
			Argv:           []string{"/bin/sh", "-c", "printf 'x%.0s' $(seq 1 100)"},
			MaxOutputBytes: 10,
		},
	}, "")

	out, err := exec(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.HasPrefix(out, strings.Repeat("x", 10)) || !strings.HasSuffix(out, "[truncated]") {
		t.Fatalf("out = %q, want 10 x's then a truncated marker", out)
	}
}

// TestCommandToolCancelKillsProcessGroup proves a context cancellation
// (which is what tool_policy's timeout produces via context.WithTimeout
// in runtime.stepTools) kills not just the direct child but any process
// it has spawned — the reason configureProcessGroup exists at all.
func TestCommandToolCancelKillsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "child-alive")

	// The direct child sleeps briefly then execs into a detached-looking
	// grandchild that would, left alone, write the marker file after the
	// parent's context is long dead.
	script := `(sleep 5 && touch ` + marker + `) & sleep 5`
	exec := mustBuildOneWithSourceDir(t, config.ToolDefinition{
		Name: "spawn", Description: "test", InputSchema: json.RawMessage(`{"type":"object"}`),
		Command: &config.CommandToolConfig{Argv: []string{"/bin/sh", "-c", script}},
	}, "")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, _ = exec(ctx, json.RawMessage(`{}`))
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Execute took %s, expected it to return promptly after the context deadline", elapsed)
	}

	time.Sleep(1 * time.Second) // give the (correctly killed) grandchild's original 5s deadline no chance to fire
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the background grandchild survived cancellation and wrote its marker — process group wasn't killed")
	}
}

func TestCommandToolRelativeWorkdirResolvesAgainstSourceDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker.txt"), []byte("here"), 0o644); err != nil {
		t.Fatal(err)
	}

	exec := mustBuildOneWithSourceDir(t, config.ToolDefinition{
		Name: "list", Description: "test", InputSchema: json.RawMessage(`{"type":"object"}`),
		Command: &config.CommandToolConfig{Argv: []string{"/bin/ls", "marker.txt"}, Workdir: "."},
	}, dir)

	out, err := exec(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.TrimSpace(out) != "marker.txt" {
		t.Fatalf("out = %q, want the workdir resolved against SourceDir", out)
	}
}

// TestCommandToolMinimalEnvironment proves the child does not inherit
// the daemon's full environment — a secret set in the parent process
// must not be visible unless explicitly passed via command.env.
func TestCommandToolMinimalEnvironment(t *testing.T) {
	t.Setenv("AGENTFORGE_TEST_SECRET", "do-not-leak")

	exec := mustBuildOneWithSourceDir(t, config.ToolDefinition{
		Name: "printenv", Description: "test", InputSchema: json.RawMessage(`{"type":"object"}`),
		Command: &config.CommandToolConfig{Argv: []string{"/usr/bin/env"}},
	}, "")

	out, err := exec(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(out, "AGENTFORGE_TEST_SECRET") {
		t.Fatalf("child environment leaked the parent's secret:\n%s", out)
	}
	if !strings.Contains(out, "PATH=") {
		t.Fatalf("expected PATH to still be passed through, got:\n%s", out)
	}
}

func TestCommandToolEnvOverrideIsVisible(t *testing.T) {
	exec := mustBuildOneWithSourceDir(t, config.ToolDefinition{
		Name: "printenv", Description: "test", InputSchema: objSchema("value"),
		Command: &config.CommandToolConfig{
			Argv: []string{"/usr/bin/env"},
			Env:  map[string]string{"FOO": "{{.value}}"},
		},
	}, "")

	out, err := exec(context.Background(), json.RawMessage(`{"value":"bar"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "FOO=bar") {
		t.Fatalf("expected FOO=bar in child environment, got:\n%s", out)
	}
}
