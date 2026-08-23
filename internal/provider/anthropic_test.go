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

// TestAnthropicToolNameRoundTrips is the Anthropic mirror of
// TestOpenAIToolNameRoundTrips: Anthropic rejects function names
// containing "." (^[a-zA-Z0-9_-]{1,128}$), so every namespaced tool name
// ("github.search") must survive being mapped to the wire charset and
// back on all four sites that touch a tool name, or the very first
// request 400s before any tool call happens.
func TestAnthropicToolNameRoundTrips(t *testing.T) {
	// Outbound: declaring a tool.
	tools := toAnthropicTools([]ToolDef{{Name: "github.search", Description: "d", InputSchema: json.RawMessage(`{}`)}})
	if len(tools) != 1 || tools[0].Name != "github__search" {
		t.Fatalf("expected outbound tool name github__search, got %+v", tools)
	}

	// Outbound: replaying a tool_use block in history.
	msgs := toAnthropicMessages([]message.Message{
		{Role: message.RoleAssistant, Content: []message.ContentBlock{
			{Type: message.BlockToolUse, ID: "toolu_1", Name: "github.search", Input: json.RawMessage(`{}`)},
		}},
	})
	if len(msgs) != 1 || len(msgs[0].Content) != 1 || msgs[0].Content[0].Name != "github__search" {
		t.Fatalf("expected the replayed tool_use name to be mangled, got %+v", msgs)
	}

	// Inbound: Complete's non-streaming path.
	blocks := fromAnthropicContent([]anthropicContent{
		{Type: "tool_use", ID: "toolu_1", Name: "github__search", Input: json.RawMessage(`{}`)},
	})
	if len(blocks) != 1 || blocks[0].Name != "github.search" {
		t.Fatalf("expected the namespaced name to come back, got %+v", blocks)
	}

	// Inbound: the streaming path's Response() assembly.
	s := &anthropicStream{blocks: []*anthropicContentAccum{
		{blockType: "tool_use", id: "toolu_1", name: "github__search"},
	}}
	resp, err := s.Response()
	if err != nil {
		t.Fatalf("Response: %v", err)
	}
	if len(resp.Content) != 1 || resp.Content[0].Name != "github.search" {
		t.Fatalf("expected the streamed tool_use name to come back namespaced, got %+v", resp.Content)
	}
}

// TestToAnthropicMessagesToolResultPartsBecomesRealArray is the
// regression test for the wire shape verified live against the real
// Messages API: a tool_result whose ToolResultParts carries a text part
// and an image part must marshal Content into a real 2-element JSON
// array, not a flat string, with the image nested as its own sub-block.
func TestToAnthropicMessagesToolResultPartsBecomesRealArray(t *testing.T) {
	msgs := []message.Message{
		{Role: message.RoleTool, Content: []message.ContentBlock{
			{
				Type: message.BlockToolResult, ToolUseID: "toolu_1", Content: "here is a screenshot [+1 image(s)]",
				ToolResultParts: []message.ContentBlock{
					{Type: message.BlockText, Text: "here is a screenshot"},
					{Type: message.BlockImage, ImageData: "aGVsbG8=", ImageMediaType: "image/png"},
				},
			},
		}},
	}

	got := toAnthropicMessages(msgs)
	if len(got) != 1 || len(got[0].Content) != 1 {
		t.Fatalf("expected 1 message with 1 content block, got %+v", got)
	}

	var parts []anthropicContent
	if err := json.Unmarshal(got[0].Content[0].Content, &parts); err != nil {
		t.Fatalf("expected Content to unmarshal as a JSON array, got %s: %v", got[0].Content[0].Content, err)
	}
	if len(parts) != 2 {
		t.Fatalf("expected 2 sub-blocks, got %+v", parts)
	}
	if parts[0].Type != "text" || parts[0].Text != "here is a screenshot" {
		t.Fatalf("expected a leading text sub-block, got %+v", parts[0])
	}
	if parts[1].Type != "image" || parts[1].Source == nil {
		t.Fatalf("expected an image sub-block with a Source, got %+v", parts[1])
	}
	if parts[1].Source.Type != "base64" || parts[1].Source.MediaType != "image/png" || parts[1].Source.Data != "aGVsbG8=" {
		t.Fatalf("expected the image source to carry base64/media type verbatim, got %+v", parts[1].Source)
	}
}

// TestToAnthropicMessagesToolResultBackwardCompat pins that a plain
// (no ToolResultParts) tool_result still marshals Content as a JSON
// string, exactly as it did before ToolResultParts existed — every
// pre-vision Anthropic conversation must keep working unchanged.
func TestToAnthropicMessagesToolResultBackwardCompat(t *testing.T) {
	msgs := []message.Message{
		{Role: message.RoleTool, Content: []message.ContentBlock{
			{Type: message.BlockToolResult, ToolUseID: "toolu_1", Content: "42"},
		}},
	}
	got := toAnthropicMessages(msgs)
	if len(got) != 1 || len(got[0].Content) != 1 {
		t.Fatalf("expected 1 message with 1 content block, got %+v", got)
	}
	var s string
	if err := json.Unmarshal(got[0].Content[0].Content, &s); err != nil {
		t.Fatalf("expected Content to unmarshal as a plain JSON string, got %s: %v", got[0].Content[0].Content, err)
	}
	if s != "42" {
		t.Fatalf("expected Content %q, got %q", "42", s)
	}
}

// TestToAnthropicMessagesOnlyNewestToolResultKeepsImages is the token-cost
// guard: once a tool-result message is no longer the newest thing in
// history, its images collapse to the text summary instead of being
// resent on every later request.
func TestToAnthropicMessagesOnlyNewestToolResultKeepsImages(t *testing.T) {
	richBlock := func(summary string) message.ContentBlock {
		return message.ContentBlock{
			Type: message.BlockToolResult, ToolUseID: "toolu_1", Content: summary,
			ToolResultParts: []message.ContentBlock{
				{Type: message.BlockImage, ImageData: "aGVsbG8=", ImageMediaType: "image/png"},
			},
		}
	}
	msgs := []message.Message{
		{Role: message.RoleTool, Content: []message.ContentBlock{richBlock("[+1 image(s)] (old)")}},
		message.Text(message.RoleAssistant, "got it"),
		{Role: message.RoleTool, Content: []message.ContentBlock{richBlock("[+1 image(s)] (new)")}},
	}

	got := toAnthropicMessages(msgs)
	if len(got) != 3 {
		t.Fatalf("expected 3 messages, got %d: %+v", len(got), got)
	}

	// The older tool_result (index 0, no longer last) must have collapsed
	// to the plain text summary.
	var oldContent string
	if err := json.Unmarshal(got[0].Content[0].Content, &oldContent); err != nil {
		t.Fatalf("expected the older tool_result to be a plain string, got %s: %v", got[0].Content[0].Content, err)
	}
	if oldContent != "[+1 image(s)] (old)" {
		t.Fatalf("expected the older tool_result's text summary, got %q", oldContent)
	}

	// The newest tool_result (index 2, last message) must still be a real
	// array with the image intact.
	var newParts []anthropicContent
	if err := json.Unmarshal(got[2].Content[0].Content, &newParts); err != nil {
		t.Fatalf("expected the newest tool_result to be a JSON array, got %s: %v", got[2].Content[0].Content, err)
	}
	if len(newParts) != 1 || newParts[0].Type != "image" {
		t.Fatalf("expected the newest tool_result to keep its image, got %+v", newParts)
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

// TestAnthropicIgnoresResponseSchema pins the "fallback-only for v1"
// scope decision: Anthropic has no native structured-output field, so
// Request.ResponseSchema must never surface as format/response_format/
// tool_choice on the wire — the engine's validate-and-retry path is the
// only structured-output mechanism Anthropic gets, same as any provider
// with Capabilities().StructuredOutput false. This is a regression guard
// so native support can't be half-implemented by accident later.
func TestAnthropicIgnoresResponseSchema(t *testing.T) {
	var captured map[string]any
	a := newTestAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(anthropicResponse{StopReason: "end_turn"})
	})

	_, err := a.Complete(context.Background(), Request{Model: "m", ResponseSchema: json.RawMessage(`{"type":"object"}`)})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	for _, key := range []string{"format", "response_format", "tool_choice"} {
		if _, ok := captured[key]; ok {
			t.Fatalf("expected no %q field on an Anthropic request, got %v", key, captured[key])
		}
	}
	if a.Capabilities().StructuredOutput {
		t.Fatal("expected Anthropic to not advertise native StructuredOutput support")
	}
}
