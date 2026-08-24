package store

import (
	"context"
	"testing"

	"agentforge/internal/message"
)

func TestStatsAggregatesAcrossRunStatesToolsAndTokens(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	if err := st.EnsureAgentExists(ctx, "agent-a"); err != nil {
		t.Fatalf("EnsureAgentExists: %v", err)
	}

	// run-1: completed, 2 turns, one successful and one failed tool call,
	// with token usage on its assistant message.
	if err := st.CreateRun(ctx, "run-1", "agent-a", "ready_for_model"); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := st.UpdateRun(ctx, "run-1", "completed", 2, 0, nil); err != nil {
		t.Fatalf("UpdateRun: %v", err)
	}
	if _, err := st.AppendMessageWithUsage(ctx, "run-1", message.Text(message.RoleAssistant, "ok"), 100, 20, 50); err != nil {
		t.Fatalf("AppendMessageWithUsage: %v", err)
	}
	if err := st.InsertToolCall(ctx, ToolCall{ID: "tc-1", RunID: "run-1", ToolName: "t.a", ArgsJSON: "{}", Approval: "auto"}); err != nil {
		t.Fatalf("InsertToolCall: %v", err)
	}
	if err := st.UpdateToolCallResult(ctx, "tc-1", "ok", false, 5); err != nil {
		t.Fatalf("UpdateToolCallResult: %v", err)
	}
	if err := st.InsertToolCall(ctx, ToolCall{ID: "tc-2", RunID: "run-1", ToolName: "t.b", ArgsJSON: "{}", Approval: "auto"}); err != nil {
		t.Fatalf("InsertToolCall: %v", err)
	}
	if err := st.UpdateToolCallResult(ctx, "tc-2", "boom", true, 3); err != nil {
		t.Fatalf("UpdateToolCallResult: %v", err)
	}

	// run-2: failed, 4 turns, no tool calls.
	if err := st.CreateRun(ctx, "run-2", "agent-a", "ready_for_model"); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := st.UpdateRun(ctx, "run-2", "failed", 4, 0, nil); err != nil {
		t.Fatalf("UpdateRun: %v", err)
	}

	// run-1b: interrupted, 0 turns (the retry budget ran out before any
	// assistant message was ever produced — see runtime.stepModel).
	if err := st.CreateRun(ctx, "run-1b", "agent-a", "ready_for_model"); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := st.UpdateRun(ctx, "run-1b", "interrupted", 0, 0, nil); err != nil {
		t.Fatalf("UpdateRun: %v", err)
	}

	// run-3: a different agent entirely — must not pollute agent-a's stats.
	if err := st.EnsureAgentExists(ctx, "agent-b"); err != nil {
		t.Fatalf("EnsureAgentExists: %v", err)
	}
	if err := st.CreateRun(ctx, "run-3", "agent-b", "ready_for_model"); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := st.UpdateRun(ctx, "run-3", "completed", 1, 0, nil); err != nil {
		t.Fatalf("UpdateRun: %v", err)
	}

	stats, err := st.Stats(ctx, "agent-a")
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.TotalRuns != 3 {
		t.Errorf("TotalRuns = %d, want 3", stats.TotalRuns)
	}
	if stats.CompletedRuns != 1 || stats.FailedRuns != 1 || stats.CancelledRuns != 0 {
		t.Errorf("CompletedRuns=%d FailedRuns=%d CancelledRuns=%d, want 1,1,0", stats.CompletedRuns, stats.FailedRuns, stats.CancelledRuns)
	}
	if stats.InterruptedRuns != 1 {
		t.Errorf("InterruptedRuns = %d, want 1", stats.InterruptedRuns)
	}
	if stats.OtherRuns != 0 {
		t.Errorf("OtherRuns = %d, want 0 (interrupted must count in InterruptedRuns, not OtherRuns)", stats.OtherRuns)
	}
	if stats.AvgTurns != 2 { // (2 + 4 + 0) / 3
		t.Errorf("AvgTurns = %v, want 2", stats.AvgTurns)
	}
	if stats.TotalToolCalls != 2 || stats.FailedToolCalls != 1 {
		t.Errorf("TotalToolCalls=%d FailedToolCalls=%d, want 2,1", stats.TotalToolCalls, stats.FailedToolCalls)
	}
	if got, want := stats.AvgToolCalls, 2.0/3.0; got != want { // 2 calls / 3 runs
		t.Errorf("AvgToolCalls = %v, want %v", got, want)
	}
	if stats.InputTokens != 100 || stats.OutputTokens != 20 {
		t.Errorf("InputTokens=%d OutputTokens=%d, want 100,20", stats.InputTokens, stats.OutputTokens)
	}
	if got, want := stats.SuccessRate(), 1.0/3.0; got != want {
		t.Errorf("SuccessRate() = %v, want %v", got, want)
	}
	if got := stats.ToolFailureRate(); got != 0.5 {
		t.Errorf("ToolFailureRate() = %v, want 0.5", got)
	}

	all, err := st.Stats(ctx, "")
	if err != nil {
		t.Fatalf("Stats(\"\"): %v", err)
	}
	if all.TotalRuns != 4 {
		t.Errorf("Stats(\"\").TotalRuns = %d, want 4 (every agent)", all.TotalRuns)
	}
}

func TestStatsOnEmptyStoreReportsZeroNotDivideByZero(t *testing.T) {
	st := openTestStore(t)
	stats, err := st.Stats(context.Background(), "")
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.TotalRuns != 0 {
		t.Fatalf("TotalRuns = %d, want 0", stats.TotalRuns)
	}
	if got := stats.SuccessRate(); got != 0 {
		t.Fatalf("SuccessRate() on an empty store = %v, want 0", got)
	}
	if got := stats.ToolFailureRate(); got != 0 {
		t.Fatalf("ToolFailureRate() on an empty store = %v, want 0", got)
	}
}
