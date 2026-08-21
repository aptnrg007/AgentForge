package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"agentforge/internal/provider"
)

func TestToolPolicyTimeoutFor(t *testing.T) {
	policy := ToolPolicy{
		Timeout: 30 * time.Second,
		Overrides: []ToolTimeoutRule{
			{Patterns: []string{"github.*"}, Timeout: 90 * time.Second},
			{Patterns: []string{"render.video", "build.*"}, Timeout: 10 * time.Minute},
		},
	}

	cases := []struct {
		name string
		tool string
		want time.Duration
	}{
		{"no match falls back to default", "search.web", 30 * time.Second},
		{"first override matches", "github.create_issue", 90 * time.Second},
		{"second override matches by exact name", "render.video", 10 * time.Minute},
		{"second override matches by glob", "build.container", 10 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := policy.timeoutFor(tc.tool); got != tc.want {
				t.Errorf("timeoutFor(%q) = %s, want %s", tc.tool, got, tc.want)
			}
		})
	}

	t.Run("first match wins when two overrides both match", func(t *testing.T) {
		p := ToolPolicy{
			Timeout: 30 * time.Second,
			Overrides: []ToolTimeoutRule{
				{Patterns: []string{"github.*"}, Timeout: 90 * time.Second},
				{Patterns: []string{"github.create_issue"}, Timeout: 5 * time.Minute},
			},
		}
		if got := p.timeoutFor("github.create_issue"); got != 90*time.Second {
			t.Errorf("timeoutFor = %s, want the first matching override (90s)", got)
		}
	})

	t.Run("no default and no match is unbounded", func(t *testing.T) {
		p := ToolPolicy{
			Overrides: []ToolTimeoutRule{{Patterns: []string{"github.*"}, Timeout: 90 * time.Second}},
		}
		if got := p.timeoutFor("search.web"); got != 0 {
			t.Errorf("timeoutFor = %s, want 0 (unbounded)", got)
		}
	})

	t.Run("zero value policy is unbounded for everything", func(t *testing.T) {
		var p ToolPolicy
		if got := p.timeoutFor("anything.at_all"); got != 0 {
			t.Errorf("timeoutFor = %s, want 0 (unbounded)", got)
		}
	})
}

// newBlockingTool returns a tool whose Execute blocks until ctx is done,
// then returns ctx.Err() — the fixture a hung MCP server would look like
// from the engine's perspective.
func newBlockingTool(name string) Tool {
	return Tool{
		Name:        name,
		Description: "test tool that never returns on its own",
		InputSchema: json.RawMessage(`{}`),
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		},
	}
}

// newSleepingTool returns a tool that finishes normally after d, for
// asserting a timeout doesn't fire on a tool that's merely slow, not hung.
func newSleepingTool(name string, d time.Duration) Tool {
	return Tool{
		Name:        name,
		Description: "test tool that finishes after a short delay",
		InputSchema: json.RawMessage(`{}`),
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			select {
			case <-time.After(d):
				return "finished", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		},
	}
}

func TestToolTimeoutDefaultErrorSurvivesRun(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))

	fp := &fakeProvider{responses: []*provider.Response{
		toolUseResponse("call_1", "hang.tool", `{}`),
		textResponse("worked around the timeout"),
	}}

	eng := NewEngine(st, fp, Config{
		AgentName: "test-agent", Model: "test-model", MaxTurns: 10,
		ToolPolicy: ToolPolicy{Timeout: 20 * time.Millisecond},
	})
	eng.RegisterTool(newBlockingTool("hang.tool"))

	runID := "run-tool-timeout-error"
	if err := eng.NewRun(ctx, runID, "use the hanging tool"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}

	if final := runToTerminal(t, eng, runID); final != StateCompleted {
		run, _ := st.GetRun(ctx, runID)
		t.Fatalf("expected the run to survive a tool timeout and complete, got %s (error=%v)", final, run.Error)
	}

	calls, err := st.ListToolCalls(ctx, runID)
	if err != nil {
		t.Fatalf("ListToolCalls: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("expected one tool call, got %d", len(calls))
	}
	if !calls[0].IsError {
		t.Fatalf("expected the timed-out call to be recorded as an error, got %+v", calls[0])
	}
	wantMsg := fmt.Sprintf("tool %q timed out after %s", "hang.tool", 20*time.Millisecond)
	if calls[0].Result == nil || *calls[0].Result != wantMsg {
		t.Fatalf("result = %v, want %q", calls[0].Result, wantMsg)
	}
}

func TestToolTimeoutOnTimeoutFailEndsRun(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))

	fp := &fakeProvider{responses: []*provider.Response{
		toolUseResponse("call_1", "hang.tool", `{}`),
	}}

	eng := NewEngine(st, fp, Config{
		AgentName: "test-agent", Model: "test-model", MaxTurns: 10,
		ToolPolicy: ToolPolicy{Timeout: 20 * time.Millisecond, OnTimeout: "fail"},
	})
	eng.RegisterTool(newBlockingTool("hang.tool"))

	runID := "run-tool-timeout-fail"
	if err := eng.NewRun(ctx, runID, "use the hanging tool"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}

	if final := runToTerminal(t, eng, runID); final != StateFailed {
		t.Fatalf("expected on_timeout: fail to end the run as failed, got %s", final)
	}

	run, err := st.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Error == nil {
		t.Fatalf("expected run.Error to describe the timeout")
	}
	wantMsg := fmt.Sprintf("tool %q timed out after %s", "hang.tool", 20*time.Millisecond)
	if *run.Error != wantMsg {
		t.Fatalf("run.Error = %q, want %q", *run.Error, wantMsg)
	}

	calls, err := st.ListToolCalls(ctx, runID)
	if err != nil {
		t.Fatalf("ListToolCalls: %v", err)
	}
	if len(calls) != 1 || !calls[0].IsError || calls[0].Result == nil || *calls[0].Result != wantMsg {
		t.Fatalf("expected the tool call row to also show the timeout, got %+v", calls)
	}
}

func TestToolTimeoutFastToolUnaffected(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))

	fp := &fakeProvider{responses: []*provider.Response{
		toolUseResponse("call_1", "echo", `{"message":"hi"}`),
		textResponse("done"),
	}}

	eng := NewEngine(st, fp, Config{
		AgentName: "test-agent", Model: "test-model", MaxTurns: 10,
		ToolPolicy: ToolPolicy{Timeout: 5 * time.Second},
	})
	eng.RegisterTool(NewEchoTool())

	runID := "run-tool-timeout-fast"
	if err := eng.NewRun(ctx, runID, "echo hi"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}

	if final := runToTerminal(t, eng, runID); final != StateCompleted {
		t.Fatalf("expected a fast tool under a generous timeout to complete normally, got %s", final)
	}

	calls, err := st.ListToolCalls(ctx, runID)
	if err != nil {
		t.Fatalf("ListToolCalls: %v", err)
	}
	if len(calls) != 1 || calls[0].IsError {
		t.Fatalf("expected the fast call to succeed, got %+v", calls)
	}
}

func TestToolTimeoutNoMatchingPatternRunsUnbounded(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))

	fp := &fakeProvider{responses: []*provider.Response{
		toolUseResponse("call_1", "slow.tool", `{}`),
		textResponse("done"),
	}}

	eng := NewEngine(st, fp, Config{
		AgentName: "test-agent", Model: "test-model", MaxTurns: 10,
		// A default timeout that would fire fast, but slow.tool doesn't
		// match the override pattern, so it stays unbounded.
		ToolPolicy: ToolPolicy{
			Overrides: []ToolTimeoutRule{{Patterns: []string{"github.*"}, Timeout: 5 * time.Millisecond}},
		},
	})
	eng.RegisterTool(newSleepingTool("slow.tool", 40*time.Millisecond))

	runID := "run-tool-timeout-nomatch"
	if err := eng.NewRun(ctx, runID, "use the slow tool"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}

	if final := runToTerminal(t, eng, runID); final != StateCompleted {
		t.Fatalf("expected the unmatched, undefaulted tool to run unbounded and complete, got %s", final)
	}

	calls, err := st.ListToolCalls(ctx, runID)
	if err != nil {
		t.Fatalf("ListToolCalls: %v", err)
	}
	if len(calls) != 1 || calls[0].IsError {
		t.Fatalf("expected the unbounded call to succeed, got %+v", calls)
	}
}

func TestToolTimeoutSlowButNotExpiredCompletesNormally(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))

	fp := &fakeProvider{responses: []*provider.Response{
		toolUseResponse("call_1", "slow.tool", `{}`),
		textResponse("done"),
	}}

	eng := NewEngine(st, fp, Config{
		AgentName: "test-agent", Model: "test-model", MaxTurns: 10,
		ToolPolicy: ToolPolicy{Timeout: 200 * time.Millisecond},
	})
	eng.RegisterTool(newSleepingTool("slow.tool", 20*time.Millisecond))

	runID := "run-tool-timeout-notexpired"
	if err := eng.NewRun(ctx, runID, "use the slow tool"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}

	if final := runToTerminal(t, eng, runID); final != StateCompleted {
		t.Fatalf("expected a tool finishing before its deadline to complete normally, got %s", final)
	}

	calls, err := st.ListToolCalls(ctx, runID)
	if err != nil {
		t.Fatalf("ListToolCalls: %v", err)
	}
	if len(calls) != 1 || calls[0].IsError || calls[0].Result == nil || *calls[0].Result != "finished" {
		t.Fatalf("expected the call to finish with result %q, got %+v", "finished", calls)
	}
}
