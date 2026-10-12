package runtime

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aptnrg007/AgentForge/internal/message"
	"github.com/aptnrg007/AgentForge/internal/provider"
)

// toolUseMsg + toolResultMsg build one assistant/tool_result pair — the
// unit shape splitUnits relies on: in settled history, every assistant
// message with a tool_use is immediately followed by the RoleTool
// message carrying its result.
func toolUseMsg(id string, inputLen int) message.Message {
	// A JSON string literal, not just repeated bytes: ContentBlock.Input
	// is a json.RawMessage, and the message gets round-tripped through
	// store.AppendMessage in the engine-level tests below, which requires
	// it to actually be valid JSON.
	input, _ := json.Marshal(strings.Repeat("a", inputLen))
	return message.Message{Role: message.RoleAssistant, Content: []message.ContentBlock{
		{Type: message.BlockToolUse, ID: id, Name: "t", Input: input},
	}}
}

func toolResultMsg(id string, resultLen int) message.Message {
	return message.Message{Role: message.RoleTool, Content: []message.ContentBlock{
		{Type: message.BlockToolResult, ToolUseID: id, Content: strings.Repeat("x", resultLen)},
	}}
}

func textMsg(role message.Role, text string) message.Message {
	return message.Text(role, text)
}

func TestEstimateTokens(t *testing.T) {
	msgs := []message.Message{textMsg(message.RoleUser, strings.Repeat("a", 40))}
	if got, want := estimateTokens(msgs), 10; got != want {
		t.Errorf("estimateTokens(40 chars) = %d, want %d (40/charsPerToken)", got, want)
	}
	// Rounds up rather than down — undercounting tokens is the unsafe
	// direction for a budget.
	msgs = []message.Message{textMsg(message.RoleUser, strings.Repeat("a", 41))}
	if got, want := estimateTokens(msgs), 11; got != want {
		t.Errorf("estimateTokens(41 chars) = %d, want %d (rounded up)", got, want)
	}
	if got := estimateTokens(nil); got != 0 {
		t.Errorf("estimateTokens(nil) = %d, want 0", got)
	}
}

func TestSplitUnits(t *testing.T) {
	msgs := []message.Message{
		textMsg(message.RoleUser, "hi"), // unit 0: standalone
		toolUseMsg("call_1", 5),         // unit 1: paired with the next message
		toolResultMsg("call_1", 5),
		textMsg(message.RoleAssistant, "done"), // unit 2: standalone
	}
	units := splitUnits(msgs)
	want := []unit{{0, 1}, {1, 3}, {3, 4}}
	if len(units) != len(want) {
		t.Fatalf("splitUnits: got %d units, want %d: %v", len(units), len(want), units)
	}
	for i, u := range units {
		if u != want[i] {
			t.Errorf("splitUnits[%d] = %v, want %v", i, u, want[i])
		}
	}
}

func TestSplitUnits_trailingToolUseWithNoResultYet(t *testing.T) {
	// stepModel only ever calls fitHistory on settled history (ListMessages
	// after stepTools has already run), so a tool_use with no following
	// message shouldn't occur in practice — but splitUnits must not panic
	// or go out of bounds if it ever does; it degrades to a standalone unit.
	msgs := []message.Message{toolUseMsg("call_1", 5)}
	units := splitUnits(msgs)
	if len(units) != 1 || units[0] != (unit{0, 1}) {
		t.Fatalf("splitUnits(trailing tool_use) = %v, want [{0 1}]", units)
	}
}

func TestProtectedUnits(t *testing.T) {
	// Small enough that everything is protected — never touch the first
	// message or the most recent minProtectedUnits turns, and here
	// they overlap.
	if p := protectedUnits(3); !p[0] || !p[1] || !p[2] {
		t.Errorf("protectedUnits(3) = %v, want all true", p)
	}
	// Large enough to have real eligible units in the middle.
	p := protectedUnits(6)
	want := []bool{true, false, false, false, true, true}
	for i := range want {
		if p[i] != want[i] {
			t.Errorf("protectedUnits(6)[%d] = %v, want %v (full: %v)", i, p[i], want[i], p)
		}
	}
}

func TestShrinkToolResults(t *testing.T) {
	m := toolResultMsg("call_1", 1000)
	shrunk, n := shrinkToolResults(m)
	if n != 1 {
		t.Fatalf("shrinkToolResults: n = %d, want 1", n)
	}
	got := shrunk.Content[0].Content
	if !isElisionMarker(got) {
		t.Errorf("shrinkToolResults content = %q, not a recognized elision marker", got)
	}
	if shrunk.Content[0].ToolUseID != "call_1" {
		t.Errorf("shrinkToolResults dropped ToolUseID: got %q", shrunk.Content[0].ToolUseID)
	}

	// Already shrunk: idempotent, reports nothing changed.
	reshrunk, n2 := shrinkToolResults(shrunk)
	if n2 != 0 {
		t.Errorf("shrinkToolResults on an already-shrunk message: n = %d, want 0", n2)
	}
	if reshrunk.Content[0].Content != got {
		t.Errorf("shrinkToolResults mutated an already-shrunk marker: got %q, want %q", reshrunk.Content[0].Content, got)
	}
}

// buildToolHeavyHistory returns a user message, n assistant/tool_result
// pairs each with a resultLen-byte tool result, and a final short
// assistant answer — the shape a real multi-step tool-calling run
// persists.
func buildToolHeavyHistory(n, resultLen int) []message.Message {
	msgs := []message.Message{textMsg(message.RoleUser, "please investigate")}
	for i := 0; i < n; i++ {
		id := "call_" + string(rune('a'+i))
		msgs = append(msgs, toolUseMsg(id, 10), toolResultMsg(id, resultLen))
	}
	msgs = append(msgs, textMsg(message.RoleAssistant, "done"))
	return msgs
}

func TestFitHistory_noOpWhenBudgetDisabled(t *testing.T) {
	msgs := buildToolHeavyHistory(5, 4000)
	fitted, el, changed := fitHistory(msgs, ContextPolicy{}) // zero value: no budget
	if changed {
		t.Fatalf("fitHistory with no budget: changed = true, want false")
	}
	if len(fitted) != len(msgs) {
		t.Fatalf("fitHistory with no budget: returned %d messages, want the original %d unchanged", len(fitted), len(msgs))
	}
	for i := range msgs {
		if fitted[i].Role != msgs[i].Role || len(fitted[i].Content) != len(msgs[i].Content) {
			t.Fatalf("fitHistory with no budget altered message %d", i)
		}
	}
	if el.EstimatedTokensBefore != el.EstimatedTokensAfter {
		t.Errorf("no-op Elision should report before == after, got %d != %d", el.EstimatedTokensBefore, el.EstimatedTokensAfter)
	}
}

func TestFitHistory_noOpWhenAlreadyUnderBudget(t *testing.T) {
	msgs := buildToolHeavyHistory(2, 100)
	before := estimateTokens(msgs)
	fitted, _, changed := fitHistory(msgs, ContextPolicy{MaxInputTokens: before + 1000})
	if changed {
		t.Fatalf("fitHistory already under budget: changed = true, want false")
	}
	if len(fitted) != len(msgs) {
		t.Fatalf("fitHistory already under budget: message count changed")
	}
}

func TestFitHistory_shrinkPassAlone(t *testing.T) {
	msgs := buildToolHeavyHistory(5, 4000)
	before := estimateTokens(msgs)
	budget := before / 2

	fitted, el, changed := fitHistory(msgs, ContextPolicy{MaxInputTokens: budget})
	if !changed {
		t.Fatalf("fitHistory: changed = false, want true (before=%d budget=%d)", before, budget)
	}
	if got := estimateTokens(fitted); got > budget {
		t.Errorf("fitHistory: estimate after = %d, exceeds budget %d", got, budget)
	}
	if len(fitted) != len(msgs) {
		t.Errorf("fitHistory shrink pass should never drop a message, got %d messages, want %d", len(fitted), len(msgs))
	}
	if el.DroppedUnits != 0 {
		t.Errorf("fitHistory: DroppedUnits = %d, want 0 — shrinking alone should have been enough", el.DroppedUnits)
	}
	if el.ElidedBlocks == 0 {
		t.Errorf("fitHistory: ElidedBlocks = 0, want > 0")
	}
	assertPairingIntact(t, fitted)
}

func TestFitHistory_dropPassWhenShrinkingCannotHelp(t *testing.T) {
	// Big plain-text assistant replies, no tool calls at all: shrinkToolResults
	// only ever touches RoleTool messages, so the shrink pass has nothing to
	// do here — only the drop pass (whole units) can bring this under budget.
	msgs := []message.Message{
		textMsg(message.RoleUser, "hi"),
		textMsg(message.RoleAssistant, strings.Repeat("y", 3000)), // unit 1: eligible
		textMsg(message.RoleAssistant, strings.Repeat("y", 3000)), // unit 2: eligible
		textMsg(message.RoleAssistant, strings.Repeat("y", 3000)), // unit 3: eligible
		textMsg(message.RoleAssistant, strings.Repeat("y", 3000)), // unit 4: protected (most recent 2)
		textMsg(message.RoleAssistant, "done"),                    // unit 5: protected (most recent 2)
	}
	floor := estimateTokens([]message.Message{msgs[0], msgs[4], msgs[5]})
	budget := floor + 50

	fitted, el, changed := fitHistory(msgs, ContextPolicy{MaxInputTokens: budget})
	if !changed {
		t.Fatalf("fitHistory: changed = false, want true")
	}
	if el.ElidedBlocks != 0 {
		t.Errorf("fitHistory: ElidedBlocks = %d, want 0 — no tool_result blocks exist to shrink", el.ElidedBlocks)
	}
	if el.DroppedUnits == 0 {
		t.Errorf("fitHistory: DroppedUnits = 0, want > 0")
	}
	if got := estimateTokens(fitted); got > budget {
		t.Errorf("fitHistory: estimate after = %d, exceeds budget %d", got, budget)
	}
	want := []message.Message{msgs[0], msgs[4], msgs[5]}
	if len(fitted) != len(want) {
		t.Fatalf("fitHistory: got %d messages, want exactly the 3 unsheddable ones", len(fitted))
	}
	for i := range want {
		if fitted[i].Role != want[i].Role {
			t.Errorf("fitHistory result[%d].Role = %q, want %q", i, fitted[i].Role, want[i].Role)
		}
	}
}

func TestFitHistory_idempotent(t *testing.T) {
	msgs := buildToolHeavyHistory(6, 3000)
	pol := ContextPolicy{MaxInputTokens: estimateTokens(msgs) / 3}

	once, _, _ := fitHistory(msgs, pol)
	twice, el2, changed2 := fitHistory(once, pol)

	if changed2 {
		t.Errorf("fitHistory applied to its own output reported changed=true — not idempotent (el=%+v)", el2)
	}
	if len(twice) != len(once) {
		t.Fatalf("fitHistory applied twice: message count changed from %d to %d", len(once), len(twice))
	}
	for i := range once {
		if len(twice[i].Content) != len(once[i].Content) {
			t.Errorf("fitHistory applied twice: message %d content changed", i)
		}
	}
}

func TestFitHistory_neverOrphansATooUsePair(t *testing.T) {
	// A budget tight enough to force dropping several of the oldest
	// tool-call pairs — the invariant under test is that it's never only
	// half of one.
	msgs := buildToolHeavyHistory(8, 500)
	pol := ContextPolicy{MaxInputTokens: 50}

	fitted, _, changed := fitHistory(msgs, pol)
	if !changed {
		t.Fatalf("fitHistory: changed = false, want true")
	}
	assertPairingIntact(t, fitted)
}

// assertPairingIntact fails t if fitted contains a tool_use block with no
// matching tool_result (or vice versa) anywhere in the slice — the wire-
// format invariant every provider's translator depends on (see
// toGeminiContents' functionResponse.name lookup, which 400s on a miss).
func assertPairingIntact(t *testing.T, fitted []message.Message) {
	t.Helper()
	toolUseIDs := map[string]bool{}
	toolResultIDs := map[string]bool{}
	for _, m := range fitted {
		for _, b := range m.Content {
			switch b.Type {
			case message.BlockToolUse:
				toolUseIDs[b.ID] = true
			case message.BlockToolResult:
				toolResultIDs[b.ToolUseID] = true
			}
		}
	}
	for id := range toolUseIDs {
		if !toolResultIDs[id] {
			t.Errorf("fitHistory orphaned tool_use %q with no matching tool_result", id)
		}
	}
	for id := range toolResultIDs {
		if !toolUseIDs[id] {
			t.Errorf("fitHistory orphaned tool_result for %q with no matching tool_use", id)
		}
	}
}

// --- engine-level: the no-op guarantee and the truncate/fail paths as
// stepModel actually drives them, in the fixture-replay idiom (fakeProvider).

func TestEngine_contextBudgetAbsentIsExactNoOp(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))

	fp := &fakeProvider{responses: []*provider.Response{textResponse("ok")}}
	// Config{} leaves Context at its zero value — no limits.context block,
	// the common case this must be indistinguishable from.
	eng := NewEngine(st, fp, Config{AgentName: "a", Model: "m", MaxTurns: 10})

	runID := "run-noop"
	if err := eng.NewRun(ctx, runID, "hello, this has some length to it"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	if _, err := eng.Step(ctx, runID); err != nil {
		t.Fatalf("Step: %v", err)
	}

	stored, err := st.ListMessages(ctx, runID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	sent := fp.requests[0].Messages
	if len(sent) != 1 || len(stored) < 1 {
		t.Fatalf("unexpected message counts: sent=%d stored=%d", len(sent), len(stored))
	}
	if sent[0].Content[0].Text != stored[0].Content[0].Text {
		t.Errorf("provider received a different message than what's stored: sent %q, stored %q",
			sent[0].Content[0].Text, stored[0].Content[0].Text)
	}
}

func TestEngine_truncateCompletesAndPreservesFullTrace(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))

	fp := &fakeProvider{responses: []*provider.Response{textResponse("final answer")}}
	eng := NewEngine(st, fp, Config{
		AgentName: "a", Model: "m", MaxTurns: 10,
		Context: ContextPolicy{MaxInputTokens: 50, OnOverflow: "truncate"},
	})

	runID := "run-truncate"
	if err := eng.NewRun(ctx, runID, "please investigate"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	// Seed a long tool-heavy history directly into the store, as if several
	// turns had already happened — stepModel always reloads the full
	// history fresh from SQLite (ground rule 1), so this is equivalent to
	// having actually driven those turns, without scripting five more
	// provider responses.
	for i := 0; i < 6; i++ {
		id := "call_" + string(rune('a'+i))
		if _, err := st.AppendMessage(ctx, runID, toolUseMsg(id, 10)); err != nil {
			t.Fatalf("seed tool_use: %v", err)
		}
		if _, err := st.AppendMessage(ctx, runID, toolResultMsg(id, 2000)); err != nil {
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
	if state != StateCompleted {
		t.Fatalf("Step: state = %s, want %s", state, StateCompleted)
	}

	sent := fp.requests[0].Messages
	if len(sent) >= len(fullHistory) {
		t.Errorf("provider received %d messages, expected fewer than the full %d-message history", len(sent), len(fullHistory))
	}

	afterStore, err := st.ListMessages(ctx, runID)
	if err != nil {
		t.Fatalf("ListMessages after Step: %v", err)
	}
	// +1 for the assistant message Step just appended.
	if len(afterStore) != len(fullHistory)+1 {
		t.Errorf("SQLite history was touched by compaction: got %d messages, want %d (full history + 1 new assistant message)",
			len(afterStore), len(fullHistory)+1)
	}
	for i := range fullHistory {
		if len(afterStore[i].Content) != len(fullHistory[i].Content) {
			t.Errorf("stored message %d was altered by compaction", i)
		}
	}
}

func TestEngine_onOverflowFailEndsRunWithoutCallingProvider(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))

	fp := &fakeProvider{} // no scripted responses: a Complete call is a test failure
	eng := NewEngine(st, fp, Config{
		AgentName: "a", Model: "m", MaxTurns: 10,
		Context: ContextPolicy{MaxInputTokens: 10, OnOverflow: "fail"},
	})

	runID := "run-fail"
	if err := eng.NewRun(ctx, runID, strings.Repeat("a", 1000)); err != nil {
		t.Fatalf("NewRun: %v", err)
	}

	state, err := eng.Step(ctx, runID)
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if state != StateFailed {
		t.Fatalf("Step: state = %s, want %s", state, StateFailed)
	}
	if len(fp.requests) != 0 {
		t.Errorf("on_overflow: fail should never call the provider; got %d calls", len(fp.requests))
	}

	run, err := st.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Error == nil || !strings.Contains(*run.Error, "context budget") || !strings.Contains(*run.Error, "limit 10") {
		t.Errorf("run.Error = %v, want it to name the budget (limit 10) and the estimate", run.Error)
	}
}
