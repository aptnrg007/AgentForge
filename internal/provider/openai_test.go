package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentforge/internal/message"
)

// newTestOpenAI points an OpenAI provider at an httptest.Server running
// handler, mirroring newTestAnthropic/newTestOllama.
func newTestOpenAI(t *testing.T, handler http.HandlerFunc) *OpenAI {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewOpenAI("test-key", srv.URL)
}

// --- translation unit tests (no HTTP) ---

// TestOpenAIToolNameRoundTrips is the single most important test in this
// file: OpenAI rejects function names containing ".", so every namespaced
// tool name ("github.search") must survive being mapped to the wire
// charset and back, or every tool call comes back "unknown tool".
func TestOpenAIToolNameRoundTrips(t *testing.T) {
	tools := toOpenAITools([]ToolDef{{Name: "github.search", Description: "d", InputSchema: json.RawMessage(`{}`)}})
	if len(tools) != 1 || tools[0].Function.Name != "github__search" {
		t.Fatalf("expected outbound name github__search, got %+v", tools)
	}

	blocks := fromOpenAIMessage(openAIMessage{ToolCalls: []openAIToolCall{
		{ID: "call_1", Type: "function", Function: openAIFunctionCall{Name: "github__search", Arguments: `{"q":"x"}`}},
	}})
	if len(blocks) != 1 || blocks[0].Name != "github.search" {
		t.Fatalf("expected the namespaced name to come back, got %+v", blocks)
	}
}

func TestToOpenAIMessagesArgumentsIsAJSONString(t *testing.T) {
	msgs := []message.Message{
		{Role: message.RoleAssistant, Content: []message.ContentBlock{
			{Type: message.BlockToolUse, ID: "call_1", Name: "sum", Input: json.RawMessage(`{"a":1,"b":2}`)},
		}},
	}
	got := toOpenAIMessages("", msgs)
	if len(got) != 1 || len(got[0].ToolCalls) != 1 {
		t.Fatalf("expected 1 message with 1 tool call, got %+v", got)
	}
	if arg := got[0].ToolCalls[0].Function.Arguments; arg != `{"a":1,"b":2}` {
		t.Fatalf("expected arguments as a raw JSON string, got %q", arg)
	}
}

// TestToOpenAIMessagesPreservesToolCallID pins the detail that most
// diverges from Ollama's translation: OpenAI matches a tool result back
// to its call by ID, so the real ID must survive, not a synthesized one.
func TestToOpenAIMessagesPreservesToolCallID(t *testing.T) {
	msgs := []message.Message{
		{Role: message.RoleTool, Content: []message.ContentBlock{
			{Type: message.BlockToolResult, ToolUseID: "call_1", Content: "42"},
		}},
	}
	got := toOpenAIMessages("", msgs)
	if len(got) != 1 || got[0].Role != "tool" || got[0].ToolCallID != "call_1" || got[0].Content != "42" {
		t.Fatalf("expected a tool message referencing call_1, got %+v", got)
	}
}

func TestToOpenAIMessagesDeniedToolResultBecomesErrorPrefix(t *testing.T) {
	msgs := []message.Message{
		{Role: message.RoleTool, Content: []message.ContentBlock{
			{Type: message.BlockToolResult, ToolUseID: "call_1", Content: "tool call denied: no reason given", IsError: true},
		}},
	}
	got := toOpenAIMessages("", msgs)
	if len(got) != 1 || got[0].Content != "ERROR: tool call denied: no reason given" {
		t.Fatalf("expected an ERROR:-prefixed content, got %+v", got)
	}
}

func TestToOpenAIMessagesPrependsSystemMessage(t *testing.T) {
	got := toOpenAIMessages("be helpful", []message.Message{message.Text(message.RoleUser, "hi")})
	if len(got) != 2 || got[0].Role != "system" || got[0].Content != "be helpful" {
		t.Fatalf("expected a leading system message, got %+v", got)
	}
}

func TestNormalizeOpenAIStopReason(t *testing.T) {
	cases := map[string]string{"stop": "end_turn", "tool_calls": "tool_use", "length": "max_tokens", "content_filter": "content_filter"}
	for in, want := range cases {
		if got := normalizeOpenAIStopReason(in); got != want {
			t.Errorf("normalizeOpenAIStopReason(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- HTTP-backed tests ---

func TestOpenAICompleteTranslatesTextAndToolUse(t *testing.T) {
	o := newTestOpenAI(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("expected Authorization: Bearer test-key, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openAIChatResponse{
			Choices: []openAIChoice{{
				Message: openAIMessage{
					Content: "let me check",
					ToolCalls: []openAIToolCall{
						{ID: "call_1", Type: "function", Function: openAIFunctionCall{Name: "sum", Arguments: `{"a":1,"b":2}`}},
					},
				},
				FinishReason: "tool_calls",
			}},
			Usage: openAIUsage{PromptTokens: 10, CompletionTokens: 20},
		})
	})

	resp, err := o.Complete(context.Background(), Request{Model: "gpt-5"})
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
	if tu.Type != message.BlockToolUse || tu.Name != "sum" || tu.ID != "call_1" {
		t.Fatalf("expected a sum tool_use block with id call_1, got %+v", tu)
	}
	if resp.StopReason != "tool_use" {
		t.Fatalf("expected tool_use, got %q", resp.StopReason)
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 20 {
		t.Fatalf("expected usage {10,20}, got %+v", resp.Usage)
	}
}

func TestOpenAIUnparseableArgumentsDegradeToEmptyObject(t *testing.T) {
	o := newTestOpenAI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openAIChatResponse{
			Choices: []openAIChoice{{Message: openAIMessage{
				ToolCalls: []openAIToolCall{{ID: "call_1", Function: openAIFunctionCall{Name: "sum", Arguments: "not json"}}},
			}}},
		})
	})
	resp, err := o.Complete(context.Background(), Request{Model: "m"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(resp.Content) != 1 || string(resp.Content[0].Input) != "{}" {
		t.Fatalf("expected malformed arguments to degrade to {}, got %+v", resp.Content)
	}
}

func TestOpenAICompleteSurfacesAPIErrorMessage(t *testing.T) {
	o := newTestOpenAI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(openAIErrorBody{
			Error: struct {
				Message string `json:"message"`
			}{Message: "invalid api key"},
		})
	})
	_, err := o.Complete(context.Background(), Request{Model: "m"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := err.Error(); !strings.Contains(got, "invalid api key") {
		t.Fatalf("expected the API's error message to surface, got %q", got)
	}
}

// TestOpenAISendsResponseFormatAlongsideTools pins the capability this
// whole item is for: unlike Ollama (which withholds its native format
// whenever tools are registered — see ollama_format_test.go), OpenAI can
// combine schema enforcement and tool use on the same request.
func TestOpenAISendsResponseFormatAlongsideTools(t *testing.T) {
	var captured map[string]any
	o := newTestOpenAI(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openAIChatResponse{Choices: []openAIChoice{{FinishReason: "stop"}}})
	})

	req := Request{
		Model:          "m",
		Tools:          []ToolDef{{Name: "sum", InputSchema: json.RawMessage(`{}`)}},
		ResponseSchema: json.RawMessage(`{"type":"object"}`),
	}
	if _, err := o.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, ok := captured["tools"]; !ok {
		t.Fatal("expected tools on the request")
	}
	rf, ok := captured["response_format"].(map[string]any)
	if !ok {
		t.Fatalf("expected a response_format object alongside tools, got %v", captured["response_format"])
	}
	if rf["type"] != "json_schema" {
		t.Fatalf("expected response_format.type = json_schema, got %v", rf["type"])
	}
	schema, ok := rf["json_schema"].(map[string]any)
	if !ok || schema["strict"] != false {
		t.Fatalf("expected json_schema.strict = false, got %v", rf["json_schema"])
	}
}

func TestOpenAIOmitsResponseFormatWhenNoResponseSchema(t *testing.T) {
	var captured map[string]any
	o := newTestOpenAI(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openAIChatResponse{Choices: []openAIChoice{{FinishReason: "stop"}}})
	})
	if _, err := o.Complete(context.Background(), Request{Model: "m"}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, ok := captured["response_format"]; ok {
		t.Fatalf("expected no response_format field when ResponseSchema is unset, got %v", captured["response_format"])
	}
}

func TestOpenAIAdvertisesStructuredOutputCapability(t *testing.T) {
	o := NewOpenAI("k", "")
	caps := o.Capabilities()
	if !caps.StructuredOutput || !caps.ToolUse || !caps.Vision {
		t.Fatalf("expected Vision, ToolUse, and StructuredOutput all true, got %+v", caps)
	}
}

// --- streaming ---

// writeOpenAISSELine writes one "data: <json>\n\n" line and flushes it.
func writeOpenAISSELine(t *testing.T, w http.ResponseWriter, fl http.Flusher, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal chunk: %v", err)
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		t.Fatalf("write chunk: %v", err)
	}
	fl.Flush()
}

func writeOpenAIDone(t *testing.T, w http.ResponseWriter, fl http.Flusher) {
	t.Helper()
	if _, err := fmt.Fprint(w, "data: [DONE]\n\n"); err != nil {
		t.Fatalf("write [DONE]: %v", err)
	}
	fl.Flush()
}

func TestOpenAIStreamAssemblesTextAcrossChunks(t *testing.T) {
	o := newTestOpenAI(t, func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		writeOpenAISSELine(t, w, fl, map[string]any{"choices": []map[string]any{{"delta": map[string]any{"content": "hel"}}}})
		writeOpenAISSELine(t, w, fl, map[string]any{"choices": []map[string]any{{"delta": map[string]any{"content": "lo"}}}})
		writeOpenAISSELine(t, w, fl, map[string]any{"choices": []map[string]any{{"delta": map[string]any{}, "finish_reason": "stop"}}})
		writeOpenAIDone(t, w, fl)
	})

	stream, err := o.Stream(context.Background(), Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	deltas := drainStream(t, stream)
	if strings.Join(deltas, "") != "hello" {
		t.Fatalf("expected deltas to join into %q, got %q", "hello", strings.Join(deltas, ""))
	}
	resp, err := stream.Response()
	if err != nil {
		t.Fatalf("Response: %v", err)
	}
	if len(resp.Content) != 1 || resp.Content[0].Text != "hello" {
		t.Fatalf("expected a single text block \"hello\", got %+v", resp.Content)
	}
	if resp.StopReason != "end_turn" {
		t.Fatalf("expected end_turn, got %q", resp.StopReason)
	}
}

// TestOpenAIStreamReassemblesToolCallAcrossIndexedFragments is the
// streaming counterpart of TestOpenAIToolNameRoundTrips: argument
// fragments arrive keyed by index and must accumulate into one call, not
// be dropped or interleaved with a second call sharing the response.
func TestOpenAIStreamReassemblesToolCallAcrossIndexedFragments(t *testing.T) {
	o := newTestOpenAI(t, func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		writeOpenAISSELine(t, w, fl, map[string]any{"choices": []map[string]any{{"delta": map[string]any{
			"tool_calls": []map[string]any{{"index": 0, "id": "call_1", "function": map[string]any{"name": "github__search", "arguments": `{"q":`}}},
		}}}})
		writeOpenAISSELine(t, w, fl, map[string]any{"choices": []map[string]any{{"delta": map[string]any{
			"tool_calls": []map[string]any{{"index": 0, "function": map[string]any{"arguments": `"x"}`}}},
		}}}})
		writeOpenAISSELine(t, w, fl, map[string]any{"choices": []map[string]any{{"delta": map[string]any{}, "finish_reason": "tool_calls"}}})
		writeOpenAISSELine(t, w, fl, map[string]any{"choices": []map[string]any{}, "usage": map[string]int{"prompt_tokens": 5, "completion_tokens": 7}})
		writeOpenAIDone(t, w, fl)
	})

	stream, err := o.Stream(context.Background(), Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	drainStream(t, stream)

	resp, err := stream.Response()
	if err != nil {
		t.Fatalf("Response: %v", err)
	}
	if len(resp.Content) != 1 {
		t.Fatalf("expected exactly one reassembled tool_use block, got %+v", resp.Content)
	}
	tu := resp.Content[0]
	if tu.Type != message.BlockToolUse || tu.ID != "call_1" || tu.Name != "github.search" || string(tu.Input) != `{"q":"x"}` {
		t.Fatalf("expected a fully reassembled github.search call, got %+v", tu)
	}
	if resp.StopReason != "tool_use" {
		t.Fatalf("expected tool_use, got %q", resp.StopReason)
	}
	if resp.Usage.InputTokens != 5 || resp.Usage.OutputTokens != 7 {
		t.Fatalf("expected usage {5,7} from the trailing usage chunk, got %+v", resp.Usage)
	}
}

func TestOpenAIStreamEndsWithoutDoneSurfacesErr(t *testing.T) {
	o := newTestOpenAI(t, func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		writeOpenAISSELine(t, w, fl, map[string]any{"choices": []map[string]any{{"delta": map[string]any{"content": "hi"}}}})
		// No [DONE] — the connection just closes.
	})

	stream, err := o.Stream(context.Background(), Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	drainStream(t, stream)

	if stream.Err() == nil {
		t.Fatal("expected an error when the stream ends without [DONE]")
	}
}

func TestOpenAIStreamNonOKStatusSurfacesAPIErrorMessage(t *testing.T) {
	o := newTestOpenAI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(openAIErrorBody{
			Error: struct {
				Message string `json:"message"`
			}{Message: "invalid api key"},
		})
	})
	_, err := o.Stream(context.Background(), Request{Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "invalid api key") {
		t.Fatalf("expected the API's error message to surface, got %v", err)
	}
}

func TestOpenAIBaseURLIncludesVersionSegment(t *testing.T) {
	var gotPath string
	o := newTestOpenAI(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openAIChatResponse{Choices: []openAIChoice{{FinishReason: "stop"}}})
	})
	if _, err := o.Complete(context.Background(), Request{Model: "m"}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if gotPath != "/chat/completions" {
		t.Fatalf("expected /chat/completions to be appended directly to base_url, got %q", gotPath)
	}
}
