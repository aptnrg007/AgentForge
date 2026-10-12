package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aptnrg007/AgentForge/internal/config"
	"github.com/aptnrg007/AgentForge/internal/message"
	"github.com/aptnrg007/AgentForge/internal/provider"
	"github.com/aptnrg007/AgentForge/internal/runtime"
)

func TestContextPolicyNilIsZeroValue(t *testing.T) {
	pol := contextPolicy(nil)
	if pol != (runtime.ContextPolicy{}) {
		t.Errorf("contextPolicy(nil) = %+v, want the zero value", pol)
	}
}

func TestContextPolicyRespectsExplicitValues(t *testing.T) {
	pol := contextPolicy(&config.ContextConfig{MaxInputTokens: 50000, OnOverflow: "fail"})
	want := runtime.ContextPolicy{MaxInputTokens: 50000, OnOverflow: "fail"}
	if pol != want {
		t.Errorf("contextPolicy(...) = %+v, want %+v", pol, want)
	}
}

// TestBuildWiresLimitsContext proves the config -> agent.Build ->
// runtime.Engine wiring for limits.context actually connects, the same
// way TestBuildWiresToolPolicyTimeout proves it for tool_policy.timeout —
// a real effect on a run, not just a struct field copied around
// untested.
func TestBuildWiresLimitsContext(t *testing.T) {
	st, registry := newTestStoreAndRegistry(t)
	cfg := &config.Config{
		Name:   "a",
		Model:  config.ModelConfig{Provider: "ollama", Name: "m"},
		Limits: config.LimitsConfig{Context: &config.ContextConfig{MaxInputTokens: 50, OnOverflow: "truncate"}},
	}
	sp := &scriptedProvider{responses: []*provider.Response{textResp("final answer")}}
	eng, err := Build(context.Background(), st, registry, cfg, fakeFactory(sp))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	ctx := context.Background()
	runID := "run-context"
	if err := eng.NewRun(ctx, runID, "please investigate"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	// Seed a long tool-heavy history directly, as if several turns had
	// already happened — same approach as
	// internal/runtime's TestEngine_truncateCompletesAndPreservesFullTrace,
	// here proving the same thing through the full config -> Build path
	// instead of constructing runtime.Config by hand.
	for i := 0; i < 6; i++ {
		id := "call_" + string(rune('a'+i))
		input, _ := json.Marshal("x")
		toolUse := message.Message{Role: message.RoleAssistant, Content: []message.ContentBlock{
			{Type: message.BlockToolUse, ID: id, Name: "t", Input: input},
		}}
		toolResult := message.Message{Role: message.RoleTool, Content: []message.ContentBlock{
			{Type: message.BlockToolResult, ToolUseID: id, Content: strings.Repeat("x", 2000)},
		}}
		if _, err := st.AppendMessage(ctx, runID, toolUse); err != nil {
			t.Fatalf("seed tool_use: %v", err)
		}
		if _, err := st.AppendMessage(ctx, runID, toolResult); err != nil {
			t.Fatalf("seed tool_result: %v", err)
		}
	}
	fullHistory, err := st.ListMessages(ctx, runID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}

	state, err := eng.Step(ctx, runID)
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if state != runtime.StateCompleted {
		run, _ := st.GetRun(ctx, runID)
		t.Fatalf("Step: state = %s, want %s (error=%v)", state, runtime.StateCompleted, run.Error)
	}
	if len(sp.requests) != 1 {
		t.Fatalf("expected exactly one provider call, got %d", len(sp.requests))
	}
	if got := len(sp.requests[0].Messages); got >= len(fullHistory) {
		t.Errorf("provider received %d messages, expected fewer than the full %d-message history — limits.context.max_input_tokens: 50 should have forced compaction", got, len(fullHistory))
	}
}

// TestBuildWithNoLimitsContextIsExactNoOp proves the absent-config case
// for the same wiring: no limits.context block at all must leave the
// provider request byte-for-byte identical to the stored history, the
// pre-existing behavior unchanged.
func TestBuildWithNoLimitsContextIsExactNoOp(t *testing.T) {
	st, registry := newTestStoreAndRegistry(t)
	cfg := &config.Config{
		Name:  "a",
		Model: config.ModelConfig{Provider: "ollama", Name: "m"},
	}
	sp := &scriptedProvider{responses: []*provider.Response{textResp("ok")}}
	eng, err := Build(context.Background(), st, registry, cfg, fakeFactory(sp))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	ctx := context.Background()
	runID := "run-no-context"
	if err := eng.NewRun(ctx, runID, "a request with some real length to it"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	// Captured before Step, not after: Step appends the assistant's reply
	// too, which would make an exact-length comparison against the
	// post-Step history off by one regardless of whether compaction ran.
	stored, err := st.ListMessages(ctx, runID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if _, err := eng.Step(ctx, runID); err != nil {
		t.Fatalf("Step: %v", err)
	}

	sent := sp.requests[0].Messages
	if len(sent) != len(stored) {
		t.Fatalf("sent %d messages, stored had %d — expected an exact match", len(sent), len(stored))
	}
	if sent[0].Content[0].Text != stored[0].Content[0].Text {
		t.Errorf("provider received a different message than what's stored: sent %q, stored %q",
			sent[0].Content[0].Text, stored[0].Content[0].Text)
	}
}
