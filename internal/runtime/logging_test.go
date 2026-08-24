package runtime

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"agentforge/internal/provider"
)

// TestStepLogsStateTransitionsWithRunID confirms Config.Logger is
// actually wired through Step (previously internal/runtime had not one
// log line at all — docs/DESIGN.md section 13, "make the run loop
// audible") and that every line carries a run_id, so a multi-run
// process's log can be filtered down to one run.
func TestStepLogsStateTransitionsWithRunID(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))
	fp := &fakeProvider{responses: []*provider.Response{textResponse("done")}}
	eng := NewEngine(st, fp, Config{AgentName: "a", Model: "m", MaxTurns: 10, Logger: logger})

	ctx := context.Background()
	if err := eng.NewRun(ctx, "r1", "go"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	if _, err := eng.Step(ctx, "r1"); err != nil {
		t.Fatalf("Step: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "runtime: run started") {
		t.Fatalf("log output = %q, want a \"runtime: run started\" line", out)
	}
	if !strings.Contains(out, "runtime: state transition") {
		t.Fatalf("log output = %q, want a \"runtime: state transition\" line", out)
	}
	if !strings.Contains(out, "run_id=r1") {
		t.Fatalf("log output = %q, want every line to carry run_id=r1", out)
	}
	if !strings.Contains(out, "to=completed") {
		t.Fatalf("log output = %q, want the transition to record to=completed", out)
	}
}

// TestNewEngineDefaultsToSlogDefaultWhenNoLoggerConfigured confirms a nil
// Config.Logger doesn't leave Engine unable to log at all (a nil
// *slog.Logger method call panics) — the same "nil means slog.Default()"
// convention mcp.NewRegistry and api.NewServer already use.
func TestNewEngineDefaultsToSlogDefaultWhenNoLoggerConfigured(t *testing.T) {
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))
	fp := &fakeProvider{responses: []*provider.Response{textResponse("done")}}
	eng := NewEngine(st, fp, Config{AgentName: "a", Model: "m", MaxTurns: 10})

	ctx := context.Background()
	if err := eng.NewRun(ctx, "r1", "go"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	if _, err := eng.Step(ctx, "r1"); err != nil {
		t.Fatalf("Step: %v", err)
	}
}
