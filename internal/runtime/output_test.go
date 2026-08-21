package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"agentforge/internal/message"
	"agentforge/internal/provider"
)

// storySpecValidate is a hand-written stand-in for the closure
// agent.Build wires around internal/schema — deliberately not importing
// that package here, per the project's convention that internal/runtime
// stays free of the JSON Schema library. It accepts
// {"title": string, "beats": [string, ...non-empty]}.
func storySpecValidate(instance []byte) []string {
	var v struct {
		Title *string  `json:"title"`
		Beats []string `json:"beats"`
	}
	if err := json.Unmarshal(instance, &v); err != nil {
		return []string{fmt.Sprintf("not valid JSON: %v", err)}
	}
	var problems []string
	if v.Title == nil {
		problems = append(problems, "missing required property \"title\"")
	}
	if v.Beats == nil {
		problems = append(problems, "missing required property \"beats\"")
	} else if len(v.Beats) == 0 {
		problems = append(problems, "\"beats\" must have at least one item")
	}
	return problems
}

func TestStructuredOutputSelfCorrectsByTurnThree(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))

	fp := &fakeProvider{responses: []*provider.Response{
		textResponse(`{"title":"x"}`),                // missing "beats"
		textResponse(`{"title":"x","beats":"nope"}`), // wrong type for "beats"
		textResponse(`{"title":"x","beats":["a"]}`),  // valid
	}}
	eng := NewEngine(st, fp, Config{
		AgentName: "test-agent", Model: "test-model", MaxTurns: 10,
		Output: OutputPolicy{Validate: storySpecValidate, OnInvalid: "retry"},
	})

	runID := "run-self-correct"
	if err := eng.NewRun(ctx, runID, "write a story spec"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}

	state := runToTerminal(t, eng, runID)
	if state != StateCompleted {
		run, _ := st.GetRun(ctx, runID)
		t.Fatalf("expected completed, got %s (error=%v)", state, run.Error)
	}
	if fp.calls != 3 {
		t.Fatalf("expected 3 model calls, got %d", fp.calls)
	}

	run, err := st.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.TurnCount != 3 {
		t.Fatalf("expected turn_count 3, got %d", run.TurnCount)
	}
	if run.RepairCount != 0 {
		t.Fatalf("expected repair_count reset to 0 on success, got %d", run.RepairCount)
	}

	msgs, err := st.ListMessages(ctx, runID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	wantRoles := []message.Role{
		message.RoleUser, message.RoleAssistant,
		message.RoleUser, message.RoleAssistant,
		message.RoleUser, message.RoleAssistant,
	}
	if len(msgs) != len(wantRoles) {
		t.Fatalf("expected %d messages, got %d: %+v", len(wantRoles), len(msgs), msgs)
	}
	for i, want := range wantRoles {
		if msgs[i].Role != want {
			t.Fatalf("message %d role = %s, want %s", i, msgs[i].Role, want)
		}
	}

	// The two feedback turns (index 2, 4) are the validation errors, not
	// tool_result blocks.
	if !strings.Contains(msgs[2].Content[0].Text, "schema") && !strings.Contains(msgs[2].Content[0].Text, "beats") {
		t.Fatalf("expected feedback turn to mention the violation, got %q", msgs[2].Content[0].Text)
	}

	finalText := msgs[5].Content[0].Text
	if storySpecValidate([]byte(finalText)) != nil {
		t.Fatalf("expected the final assistant text to be schema-valid, got %q", finalText)
	}
}

func TestStructuredOutputValidFirstTry(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))

	fp := &fakeProvider{responses: []*provider.Response{textResponse(`{"title":"x","beats":["a"]}`)}}
	eng := NewEngine(st, fp, Config{
		AgentName: "test-agent", Model: "test-model", MaxTurns: 10,
		Output: OutputPolicy{Validate: storySpecValidate},
	})

	runID := "run-valid-first-try"
	if err := eng.NewRun(ctx, runID, "go"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	if state := runToTerminal(t, eng, runID); state != StateCompleted {
		t.Fatalf("expected completed, got %s", state)
	}
	if fp.calls != 1 {
		t.Fatalf("expected exactly 1 model call, got %d", fp.calls)
	}
	msgs, err := st.ListMessages(ctx, runID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages (user, assistant), got %d", len(msgs))
	}
}

func TestStructuredOutputExhaustsRetries(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))

	fp := &fakeProvider{responses: []*provider.Response{
		textResponse(`{}`), textResponse(`{}`), textResponse(`{}`),
	}}
	eng := NewEngine(st, fp, Config{
		AgentName: "test-agent", Model: "test-model", MaxTurns: 10,
		Output: OutputPolicy{Validate: storySpecValidate, OnInvalid: "retry", MaxRetries: 2},
	})

	runID := "run-exhausts-retries"
	if err := eng.NewRun(ctx, runID, "go"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	state := runToTerminal(t, eng, runID)
	if state != StateFailed {
		t.Fatalf("expected failed, got %s", state)
	}
	if fp.calls != 3 {
		t.Fatalf("expected 3 model calls (1 + 2 retries), got %d", fp.calls)
	}
	run, err := st.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Error == nil || !strings.Contains(*run.Error, "schema") {
		t.Fatalf("expected a schema-validation error, got %v", run.Error)
	}
}

func TestStructuredOutputOnInvalidFail(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))

	fp := &fakeProvider{responses: []*provider.Response{textResponse(`{}`)}}
	eng := NewEngine(st, fp, Config{
		AgentName: "test-agent", Model: "test-model", MaxTurns: 10,
		Output: OutputPolicy{Validate: storySpecValidate, OnInvalid: "fail"},
	})

	runID := "run-on-invalid-fail"
	if err := eng.NewRun(ctx, runID, "go"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	state := runToTerminal(t, eng, runID)
	if state != StateFailed {
		t.Fatalf("expected failed on the first violation, got %s", state)
	}
	if fp.calls != 1 {
		t.Fatalf("expected exactly 1 model call (no retries with on_invalid: fail), got %d", fp.calls)
	}

	// The invalid assistant message is still persisted even though the
	// run failed, matching how a repair failure leaves its history intact.
	msgs, err := st.ListMessages(ctx, runID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 2 || msgs[1].Role != message.RoleAssistant {
		t.Fatalf("expected the invalid assistant message to remain in the trace, got %+v", msgs)
	}
}

func TestStructuredOutputFeedbackIsUserTurn(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))

	fp := &fakeProvider{responses: []*provider.Response{
		textResponse(`{}`), textResponse(`{"title":"x","beats":["a"]}`),
	}}
	eng := NewEngine(st, fp, Config{
		AgentName: "test-agent", Model: "test-model", MaxTurns: 10,
		Output: OutputPolicy{Validate: storySpecValidate},
	})

	runID := "run-feedback-shape"
	if err := eng.NewRun(ctx, runID, "go"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	if state := runToTerminal(t, eng, runID); state != StateCompleted {
		t.Fatalf("expected completed, got %s", state)
	}

	msgs, err := st.ListMessages(ctx, runID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	feedback := msgs[2]
	if feedback.Role != message.RoleUser {
		t.Fatalf("expected feedback role RoleUser, got %s", feedback.Role)
	}
	if len(feedback.Content) != 1 || feedback.Content[0].Type != message.BlockText {
		t.Fatalf("expected a single BlockText block, got %+v", feedback.Content)
	}
	if feedback.Content[0].ToolUseID != "" {
		t.Fatalf("expected no tool_use_id on a schema-feedback turn, got %q", feedback.Content[0].ToolUseID)
	}
}

func TestStructuredOutputSkippedWhenToolCallInTurn(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))

	// The first response is invalid JSON text, but stepModel only
	// reaches the validation branch when there are zero tool_use blocks
	// — a turn with a tool call is untouched by validation regardless of
	// its text content.
	fp := &fakeProvider{responses: []*provider.Response{
		toolUseResponse("call_1", "echo", `{"message":"hi"}`),
		textResponse(`{"title":"x","beats":["a"]}`),
	}}
	eng := NewEngine(st, fp, Config{
		AgentName: "test-agent", Model: "test-model", MaxTurns: 10,
		Output: OutputPolicy{Validate: storySpecValidate},
	})
	eng.RegisterTool(NewEchoTool())

	runID := "run-tool-call-skips-validation"
	if err := eng.NewRun(ctx, runID, "go"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	if state := runToTerminal(t, eng, runID); state != StateCompleted {
		run, _ := st.GetRun(ctx, runID)
		t.Fatalf("expected completed, got %s (error=%v)", state, run.Error)
	}
	run, err := st.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.RepairCount != 0 {
		t.Fatalf("expected no repair/retry turns since the tool-call turn was never validated, got repair_count=%d", run.RepairCount)
	}
}

func TestMaxTurnsBeatsMaxRetries(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))

	fp := &fakeProvider{responses: []*provider.Response{textResponse(`{}`), textResponse(`{}`)}}
	eng := NewEngine(st, fp, Config{
		AgentName: "test-agent", Model: "test-model", MaxTurns: 2, // lower than MaxRetries would allow
		Output: OutputPolicy{Validate: storySpecValidate, OnInvalid: "retry", MaxRetries: 5},
	})

	runID := "run-max-turns-wins"
	if err := eng.NewRun(ctx, runID, "go"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	state := runToTerminal(t, eng, runID)
	if state != StateFailed {
		t.Fatalf("expected failed, got %s", state)
	}
	run, err := st.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Error == nil || !strings.Contains(*run.Error, "max turns") {
		t.Fatalf("expected a max-turns error (not a schema error), got %v", run.Error)
	}
}

func TestResponseSchemaSentWheneverNative(t *testing.T) {
	ctx := context.Background()
	schema := json.RawMessage(`{"type":"object"}`)

	t.Run("native, no tools: schema sent", func(t *testing.T) {
		st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))
		fp := &fakeProvider{responses: []*provider.Response{textResponse(`{"title":"x","beats":["a"]}`)}}
		eng := NewEngine(st, fp, Config{
			AgentName: "a", Model: "m", MaxTurns: 10,
			Output: OutputPolicy{Validate: storySpecValidate, Schema: schema, Native: true},
		})
		if err := eng.NewRun(ctx, "run-native", "go"); err != nil {
			t.Fatalf("NewRun: %v", err)
		}
		runToTerminal(t, eng, "run-native")
		if len(fp.requests) != 1 || string(fp.requests[0].ResponseSchema) != string(schema) {
			t.Fatalf("expected the schema to be sent natively, got requests=%+v", fp.requests)
		}
	})

	// Whether native enforcement is safe to combine with tool use is a
	// per-provider decision (Ollama's constrained decoding can't do both
	// on one request; OpenAI's response_format can) — see
	// internal/provider/ollama_format_test.go for the withholding case.
	// The engine itself no longer knows about that limitation: it
	// forwards the schema whenever Native is set, tools or not.
	t.Run("native, tools registered: schema still forwarded by the engine", func(t *testing.T) {
		st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))
		fp := &fakeProvider{responses: []*provider.Response{textResponse(`{"title":"x","beats":["a"]}`)}}
		eng := NewEngine(st, fp, Config{
			AgentName: "a", Model: "m", MaxTurns: 10,
			Output: OutputPolicy{Validate: storySpecValidate, Schema: schema, Native: true},
		})
		eng.RegisterTool(NewEchoTool())
		if err := eng.NewRun(ctx, "run-native-tools", "go"); err != nil {
			t.Fatalf("NewRun: %v", err)
		}
		runToTerminal(t, eng, "run-native-tools")
		if len(fp.requests) != 1 || string(fp.requests[0].ResponseSchema) != string(schema) {
			t.Fatalf("expected the engine to still forward the schema with tools registered, got requests=%+v", fp.requests)
		}
	})

	t.Run("not native: schema never sent", func(t *testing.T) {
		st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))
		fp := &fakeProvider{responses: []*provider.Response{textResponse(`{"title":"x","beats":["a"]}`)}}
		eng := NewEngine(st, fp, Config{
			AgentName: "a", Model: "m", MaxTurns: 10,
			Output: OutputPolicy{Validate: storySpecValidate, Schema: schema, Native: false},
		})
		if err := eng.NewRun(ctx, "run-fallback", "go"); err != nil {
			t.Fatalf("NewRun: %v", err)
		}
		runToTerminal(t, eng, "run-fallback")
		if len(fp.requests) != 1 || fp.requests[0].ResponseSchema != nil {
			t.Fatalf("expected no schema sent in fallback mode, got requests=%+v", fp.requests)
		}
	})
}

func TestClearOutputPolicyDisablesValidation(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))

	fp := &fakeProvider{responses: []*provider.Response{textResponse(`not json at all`)}}
	eng := NewEngine(st, fp, Config{
		AgentName: "test-agent", Model: "test-model", MaxTurns: 10,
		Output: OutputPolicy{Validate: storySpecValidate},
	})
	eng.ClearOutputPolicy()

	runID := "run-cleared-policy"
	if err := eng.NewRun(ctx, runID, "go"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	state := runToTerminal(t, eng, runID)
	if state != StateCompleted {
		run, _ := st.GetRun(ctx, runID)
		t.Fatalf("expected completed (validation disabled), got %s (error=%v)", state, run.Error)
	}
	if fp.calls != 1 {
		t.Fatalf("expected exactly 1 model call with no retries, got %d", fp.calls)
	}
}
