package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agentforge/internal/message"
)

// --- translation unit tests (no HTTP) ---

// TestToAnthropicMessagesAlternatesRolesAndMarksDeniedError is a direct
// check of the two things the roadmap flagged as "a 400, not a soft
// failure" if gotten wrong: role alternation, and a denied approval's
// synthesized error reaching the model as a tool_result with is_error
// true (not as a user-authored message). The message sequence mirrors
// exactly what runtime.go's stepModel/stepTools produce.
func TestToAnthropicMessagesAlternatesRolesAndMarksDeniedError(t *testing.T) {
	msgs := []message.Message{
		message.Text(message.RoleUser, "do the thing"),
		{Role: message.RoleAssistant, Content: []message.ContentBlock{
			{Type: message.BlockToolUse, ID: "toolu_1", Name: "danger.tool", Input: json.RawMessage(`{"x":1}`)},
		}},
		{Role: message.RoleTool, Content: []message.ContentBlock{
			{Type: message.BlockToolResult, ToolUseID: "toolu_1", Content: "tool call denied: no reason given", IsError: true},
		}},
		message.Text(message.RoleAssistant, "no problem, trying something else"),
	}

	got := toAnthropicMessages(msgs)
	if len(got) != 4 {
		t.Fatalf("expected 4 messages, got %d: %+v", len(got), got)
	}

	wantRoles := []string{"user", "assistant", "user", "assistant"}
	for i, want := range wantRoles {
		if got[i].Role != want {
			t.Fatalf("message %d role = %q, want %q (roles must alternate or Anthropic 400s)", i, got[i].Role, want)
		}
	}

	toolResultMsg := got[2]
	if len(toolResultMsg.Content) != 1 {
		t.Fatalf("expected the tool message to carry exactly one content block, got %+v", toolResultMsg.Content)
	}
	tr := toolResultMsg.Content[0]
	if tr.Type != "tool_result" || tr.ToolUseID != "toolu_1" {
		t.Fatalf("expected a tool_result referencing toolu_1, got %+v", tr)
	}
	if !tr.IsError {
		t.Fatal("a denied call's synthesized error must reach the model with is_error true")
	}
}

func TestToAnthropicMessagesSkipsEmptyContent(t *testing.T) {
	// A message with no renderable blocks (e.g. an assistant turn that,
	// after filtering, has nothing) must be dropped — Anthropic rejects
	// an empty content array outright.
	msgs := []message.Message{
		message.Text(message.RoleUser, "hi"),
		{Role: message.RoleAssistant, Content: []message.ContentBlock{{Type: message.BlockText, Text: ""}}},
	}
	got := toAnthropicMessages(msgs)
	if len(got) != 1 {
		t.Fatalf("expected the empty-content message to be dropped, got %+v", got)
	}
}

func TestMaxTokensOrDefault(t *testing.T) {
	if got := maxTokensOrDefault(0); got != defaultAnthropicMaxTokens {
		t.Fatalf("expected unset MaxTokens to default to %d, got %d", defaultAnthropicMaxTokens, got)
	}
	if got := maxTokensOrDefault(-1); got != defaultAnthropicMaxTokens {
		t.Fatalf("expected negative MaxTokens to default to %d, got %d", defaultAnthropicMaxTokens, got)
	}
	if got := maxTokensOrDefault(1000); got != 1000 {
		t.Fatalf("expected an explicit MaxTokens to pass through, got %d", got)
	}
}

// --- HTTP-level tests ---

func newTestAnthropic(t *testing.T, handler http.HandlerFunc) *Anthropic {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewAnthropic("test-key", srv.URL)
}

func writeSSEFrame(t *testing.T, w http.ResponseWriter, fl http.Flusher, event string, data any) {
	t.Helper()
	payload, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal SSE payload: %v", err)
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload); err != nil {
		t.Fatalf("write SSE frame: %v", err)
	}
	fl.Flush()
}

func TestAnthropicCompleteTranslatesTextAndToolUse(t *testing.T) {
	a := newTestAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-api-key"); got != "test-key" {
			t.Errorf("expected x-api-key header, got %q", got)
		}
		if got := r.Header.Get("anthropic-version"); got != anthropicAPIVersion {
			t.Errorf("expected anthropic-version %q, got %q", anthropicAPIVersion, got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(anthropicResponse{
			Content: []anthropicContent{
				{Type: "text", Text: "let me check"},
				{Type: "tool_use", ID: "toolu_1", Name: "sum", Input: json.RawMessage(`{"a":1,"b":2}`)},
			},
			StopReason: "tool_use",
			Usage:      anthropicUsage{InputTokens: 10, OutputTokens: 20},
		})
	})

	resp, err := a.Complete(context.Background(), Request{Model: "claude-sonnet-4-6"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(resp.Content) != 2 {
		t.Fatalf("expected 2 content blocks, got %+v", resp.Content)
	}
	if resp.Content[0].Type != message.BlockText || resp.Content[0].Text != "let me check" {
		t.Fatalf("expected leading text block, got %+v", resp.Content[0])
	}
	tu := resp.Content[1]
	if tu.Type != message.BlockToolUse || tu.Name != "sum" || tu.ID != "toolu_1" {
		t.Fatalf("expected a sum tool_use block, got %+v", tu)
	}
	if resp.StopReason != "tool_use" {
		t.Fatalf("expected tool_use, got %q", resp.StopReason)
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 20 {
		t.Fatalf("expected usage {10,20}, got %+v", resp.Usage)
	}
}

func TestAnthropicRequestUsesTopLevelSystemField(t *testing.T) {
	var captured anthropicRequest
	a := newTestAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(anthropicResponse{StopReason: "end_turn"})
	})

	_, err := a.Complete(context.Background(), Request{
		Model:    "claude-sonnet-4-6",
		System:   "you are a test assistant",
		Messages: []message.Message{message.Text(message.RoleUser, "hi")},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if captured.System != "you are a test assistant" {
		t.Fatalf("expected System to carry the system prompt, got %q", captured.System)
	}
	for _, m := range captured.Messages {
		if m.Role == "system" {
			t.Fatalf("system prompt must not appear as a message, got %+v", captured.Messages)
		}
	}
}

func TestAnthropicMaxTokensDefaultsWhenUnset(t *testing.T) {
	var captured anthropicRequest
	a := newTestAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(anthropicResponse{StopReason: "end_turn"})
	})

	if _, err := a.Complete(context.Background(), Request{Model: "m"}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if captured.MaxTokens != defaultAnthropicMaxTokens {
		t.Fatalf("expected default max_tokens %d, got %d", defaultAnthropicMaxTokens, captured.MaxTokens)
	}

	if _, err := a.Complete(context.Background(), Request{Model: "m", MaxTokens: 1234}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if captured.MaxTokens != 1234 {
		t.Fatalf("expected explicit max_tokens 1234 to pass through, got %d", captured.MaxTokens)
	}
}

func TestAnthropicCompleteSurfacesAPIErrorMessage(t *testing.T) {
	a := newTestAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(anthropicErrorBody{
			Error: struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			}{Type: "invalid_request_error", Message: "messages: roles must alternate"},
		})
	})

	_, err := a.Complete(context.Background(), Request{Model: "m"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := err.Error(); !strings.Contains(got, "roles must alternate") {
		t.Fatalf("expected the API's error message to surface, got %q", got)
	}
}

func TestAnthropicStreamAssemblesTextAndToolUse(t *testing.T) {
	a := newTestAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSEFrame(t, w, fl, "message_start", map[string]any{
			"type": "message_start", "message": map[string]any{"usage": map[string]int{"input_tokens": 10}},
		})
		writeSSEFrame(t, w, fl, "content_block_start", map[string]any{
			"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""},
		})
		writeSSEFrame(t, w, fl, "content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "Hel"},
		})
		writeSSEFrame(t, w, fl, "content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "lo"},
		})
		writeSSEFrame(t, w, fl, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		writeSSEFrame(t, w, fl, "content_block_start", map[string]any{
			"type": "content_block_start", "index": 1,
			"content_block": map[string]any{"type": "tool_use", "id": "toolu_1", "name": "sum"},
		})
		writeSSEFrame(t, w, fl, "content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 1, "delta": map[string]any{"type": "input_json_delta", "partial_json": `{"a":`},
		})
		writeSSEFrame(t, w, fl, "content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 1, "delta": map[string]any{"type": "input_json_delta", "partial_json": `1,"b":2}`},
		})
		writeSSEFrame(t, w, fl, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 1})
		writeSSEFrame(t, w, fl, "message_delta", map[string]any{
			"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"}, "usage": map[string]int{"output_tokens": 20},
		})
		writeSSEFrame(t, w, fl, "message_stop", map[string]any{"type": "message_stop"})
	})

	stream, err := a.Stream(context.Background(), Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	var deltas []string
	for stream.Next() {
		if d := stream.Delta(); d != "" {
			deltas = append(deltas, d)
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("unexpected stream error: %v", err)
	}
	if fmt.Sprint(deltas) != "[Hel lo]" {
		t.Fatalf("expected text deltas [Hel lo], got %v", deltas)
	}

	resp, err := stream.Response()
	if err != nil {
		t.Fatalf("Response: %v", err)
	}
	if len(resp.Content) != 2 {
		t.Fatalf("expected 2 content blocks, got %+v", resp.Content)
	}
	if resp.Content[0].Type != message.BlockText || resp.Content[0].Text != "Hello" {
		t.Fatalf("expected assembled text %q, got %+v", "Hello", resp.Content[0])
	}
	tu := resp.Content[1]
	if tu.Type != message.BlockToolUse || tu.Name != "sum" || tu.ID != "toolu_1" {
		t.Fatalf("expected a sum tool_use block, got %+v", tu)
	}
	var args map[string]any
	if err := json.Unmarshal(tu.Input, &args); err != nil {
		t.Fatalf("unmarshal assembled tool input: %v", err)
	}
	if args["a"] != float64(1) || args["b"] != float64(2) {
		t.Fatalf("expected args a=1 b=2, got %v", args)
	}
	if resp.StopReason != "tool_use" {
		t.Fatalf("expected tool_use, got %q", resp.StopReason)
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 20 {
		t.Fatalf("expected usage {10,20}, got %+v", resp.Usage)
	}
}

// TestAnthropicStreamTokensArriveIncrementally proves Next() surfaces a
// delta before the response is complete, mirroring the equivalent Ollama
// test: the handler blocks after the first delta until the test signals,
// so a buffer-then-parse implementation would deadlock here instead of
// passing.
func TestAnthropicStreamTokensArriveIncrementally(t *testing.T) {
	release := make(chan struct{})
	a := newTestAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		writeSSEFrame(t, w, fl, "content_block_start", map[string]any{
			"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text"},
		})
		writeSSEFrame(t, w, fl, "content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "first "},
		})
		<-release
		writeSSEFrame(t, w, fl, "content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "second"},
		})
		writeSSEFrame(t, w, fl, "message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}})
		writeSSEFrame(t, w, fl, "message_stop", map[string]any{"type": "message_stop"})
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := a.Stream(ctx, Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	if !stream.Next() {
		t.Fatalf("expected content_block_start, err=%v", stream.Err())
	}
	if !stream.Next() {
		t.Fatalf("expected a first delta, err=%v", stream.Err())
	}
	if stream.Delta() != "first " {
		t.Fatalf("expected delta %q, got %q", "first ", stream.Delta())
	}

	close(release)

	if !stream.Next() {
		t.Fatalf("expected a second delta, err=%v", stream.Err())
	}
	if stream.Delta() != "second" {
		t.Fatalf("expected delta %q, got %q", "second", stream.Delta())
	}
}

func TestAnthropicStreamErrorEventSurfacesErr(t *testing.T) {
	a := newTestAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		writeSSEFrame(t, w, fl, "error", map[string]any{
			"type": "error", "error": map[string]any{"type": "overloaded_error", "message": "overloaded"},
		})
	})

	stream, err := a.Stream(context.Background(), Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	for stream.Next() {
	}
	if stream.Err() == nil {
		t.Fatal("expected an error after an error SSE event")
	}
	if _, err := stream.Response(); err == nil {
		t.Fatal("expected Response to surface the same error")
	}
}
