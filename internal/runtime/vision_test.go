package runtime

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"agentforge/internal/message"
	"agentforge/internal/provider"
)

// TestStepToolsPrefersExecuteRichOverExecute pins the precedence stepTools
// implements: when a Tool sets both, ExecuteRich wins. This shouldn't
// happen for a real MCP tool (registry.go always sets both together, from
// the same call), but it's a real `if` in stepTools and worth locking
// down directly rather than only through the MCP integration.
func TestStepToolsPrefersExecuteRichOverExecute(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))

	fp := &fakeProvider{responses: []*provider.Response{
		toolUseResponse("call_1", "snap", `{}`),
		textResponse("here's what I saw"),
	}}

	eng := NewEngine(st, fp, Config{AgentName: "test-agent", Model: "test-model", MaxTurns: 10})
	eng.RegisterTool(Tool{
		Name:        "snap",
		InputSchema: json.RawMessage(`{}`),
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			return "flat text — should not be used", nil
		},
		ExecuteRich: func(ctx context.Context, input json.RawMessage) ([]message.ContentBlock, error) {
			return []message.ContentBlock{
				{Type: message.BlockText, Text: "a screenshot"},
				{Type: message.BlockImage, ImageData: "aGVsbG8=", ImageMediaType: "image/png"},
			}, nil
		},
	})

	runID := "run-vision"
	if err := eng.NewRun(ctx, runID, "take a screenshot"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	if state := runToTerminal(t, eng, runID); state != StateCompleted {
		t.Fatalf("expected completed, got %s", state)
	}

	// The message.ContentBlock actually persisted (and later replayed to
	// providers) must carry the rich result, not the flat-Execute text.
	msgs, err := st.ListMessages(ctx, runID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var toolResult *message.ContentBlock
	for _, m := range msgs {
		for i, b := range m.Content {
			if b.Type == message.BlockToolResult {
				toolResult = &m.Content[i]
			}
		}
	}
	if toolResult == nil {
		t.Fatal("expected a tool_result block in the persisted messages")
	}
	if len(toolResult.ToolResultParts) != 2 {
		t.Fatalf("expected ExecuteRich's 2 parts to be preferred over Execute's flat text, got %+v", toolResult)
	}

	// Content must still carry the summarized text (not raw base64, not
	// Execute's flat text) — this is what feeds tool_calls.result_json,
	// Event.Result, and every non-Anthropic display/consumer.
	wantSummary := "a screenshot [+1 image(s)]"
	if toolResult.Content != wantSummary {
		t.Fatalf("Content = %q, want the summarized text %q", toolResult.Content, wantSummary)
	}

	calls, err := st.ListToolCalls(ctx, runID)
	if err != nil {
		t.Fatalf("ListToolCalls: %v", err)
	}
	if len(calls) != 1 || calls[0].Result == nil {
		t.Fatalf("expected 1 tool call with a result, got %+v", calls)
	}
	if *calls[0].Result != wantSummary {
		t.Fatalf("tool_calls.result_json = %q, want the summarized text %q (not raw image bytes)", *calls[0].Result, wantSummary)
	}
}

// TestSummarizeToolResultPartsCountsImagesAndJoinsText covers
// summarizeToolResultParts directly: text parts are concatenated in
// order, and any image parts contribute only a trailing count, never
// their bytes.
func TestSummarizeToolResultPartsCountsImagesAndJoinsText(t *testing.T) {
	got := summarizeToolResultParts([]message.ContentBlock{
		{Type: message.BlockText, Text: "before"},
		{Type: message.BlockImage, ImageData: "aGVsbG8=", ImageMediaType: "image/png"},
		{Type: message.BlockImage, ImageData: "d29ybGQ=", ImageMediaType: "image/png"},
	})
	if got != "before [+2 image(s)]" {
		t.Fatalf("got %q, want %q", got, "before [+2 image(s)]")
	}

	if got := summarizeToolResultParts([]message.ContentBlock{{Type: message.BlockText, Text: "just text"}}); got != "just text" {
		t.Fatalf("got %q, want no trailing marker when there are no images", got)
	}
}
