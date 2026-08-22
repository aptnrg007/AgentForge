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

// TestToGeminiContentsSignatureRoundTrips is the single most important
// test in this file: Gemini's thinking models reject a replayed
// functionCall part that lost its thoughtSignature with
// "400 ... missing a thought_signature in functionCall parts" — this is
// the whole bug the native provider exists to fix.
func TestToGeminiContentsSignatureRoundTrips(t *testing.T) {
	msgs := []message.Message{
		{Role: message.RoleAssistant, Content: []message.ContentBlock{
			{Type: message.BlockToolUse, ID: "call_1", Name: "list_algorithms", Input: json.RawMessage(`{}`), Signature: "sig-abc"},
		}},
	}
	got := toGeminiContents(msgs)
	if len(got) != 1 || len(got[0].Parts) != 1 {
		t.Fatalf("expected 1 content with 1 part, got %+v", got)
	}
	part := got[0].Parts[0]
	if part.FunctionCall == nil || part.FunctionCall.Name != "list_algorithms" {
		t.Fatalf("expected a functionCall part for list_algorithms, got %+v", part)
	}
	if part.ThoughtSignature != "sig-abc" {
		t.Fatalf("expected thoughtSignature %q echoed back, got %q", "sig-abc", part.ThoughtSignature)
	}
}

// TestToGeminiContentsNoSignatureIsInvented covers the parallel-call
// case observed directly against Gemini: only the first of several
// parallel function calls carries a signature, so a block with an empty
// Signature must never gain a synthesized one.
func TestToGeminiContentsNoSignatureIsInvented(t *testing.T) {
	msgs := []message.Message{
		{Role: message.RoleAssistant, Content: []message.ContentBlock{
			{Type: message.BlockToolUse, ID: "call_2", Name: "second_call", Input: json.RawMessage(`{}`)},
		}},
	}
	data, err := json.Marshal(toGeminiContents(msgs))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "thoughtSignature") {
		t.Fatalf("expected no thoughtSignature key for a block with no Signature, got %s", data)
	}
}

// TestToGeminiContentsResolvesFunctionResponseName covers a
// Gemini-specific hard 400 ("function_response.name: Name cannot be
// empty"): message.ContentBlock's tool_result only carries ToolUseID, so
// the name must be recovered by matching it back to the tool_use that
// produced it.
func TestToGeminiContentsResolvesFunctionResponseName(t *testing.T) {
	msgs := []message.Message{
		{Role: message.RoleAssistant, Content: []message.ContentBlock{
			{Type: message.BlockToolUse, ID: "call_1", Name: "list_algorithms", Input: json.RawMessage(`{}`)},
		}},
		{Role: message.RoleTool, Content: []message.ContentBlock{
			{Type: message.BlockToolResult, ToolUseID: "call_1", Content: `["bfs","dfs"]`},
		}},
	}
	got := toGeminiContents(msgs)
	if len(got) != 2 {
		t.Fatalf("expected 2 contents, got %+v", got)
	}
	part := got[1].Parts[0]
	if part.FunctionResponse == nil || part.FunctionResponse.Name != "list_algorithms" {
		t.Fatalf("expected functionResponse.name resolved to list_algorithms, got %+v", part.FunctionResponse)
	}
}

// TestToGeminiContentsDeniedToolResultBecomesErrorPrefix mirrors the
// Anthropic/OpenAI coverage of the same synthesized-denial path
// (runtime.go's stepTools). Gemini's functionResponse has no separate
// is_error field, so the convention is the shared ERROR: content prefix.
func TestToGeminiContentsDeniedToolResultBecomesErrorPrefix(t *testing.T) {
	msgs := []message.Message{
		{Role: message.RoleTool, Content: []message.ContentBlock{
			{Type: message.BlockToolResult, ToolUseID: "call_1", Content: "tool call denied: no reason given", IsError: true},
		}},
	}
	got := toGeminiContents(msgs)
	if len(got) != 1 || len(got[0].Parts) != 1 {
		t.Fatalf("expected 1 content with 1 part, got %+v", got)
	}
	var resp map[string]string
	if err := json.Unmarshal(got[0].Parts[0].FunctionResponse.Response, &resp); err != nil {
		t.Fatalf("unmarshal functionResponse.response: %v", err)
	}
	if resp["result"] != "ERROR: tool call denied: no reason given" {
		t.Fatalf("expected an ERROR:-prefixed result, got %+v", resp)
	}
}

// TestGeminiStopReasonFromFunctionCallNotFinishReason guards the easiest
// thing in this whole provider to get wrong: finishReason is "STOP" even
// when the candidate contains function calls.
func TestGeminiStopReasonFromFunctionCallNotFinishReason(t *testing.T) {
	parts := []geminiPart{{FunctionCall: &geminiFunctionCall{Name: "f"}}}
	if got := geminiStopReason("STOP", parts); got != "tool_use" {
		t.Fatalf("expected tool_use even though finishReason is STOP, got %q", got)
	}
}

// TestGeminiToolNamesStayDotted is the mirror of
// TestOpenAIToolNameRoundTrips: unlike OpenAI, Gemini accepts dotted
// namespaced tool names natively, so no toWireToolName-style translation
// should exist here at all.
func TestGeminiToolNamesStayDotted(t *testing.T) {
	tools := toGeminiTools([]ToolDef{{Name: "algoreel.validate_spec", Description: "d", InputSchema: json.RawMessage(`{}`)}})
	if len(tools) != 1 || len(tools[0].FunctionDeclarations) != 1 || tools[0].FunctionDeclarations[0].Name != "algoreel.validate_spec" {
		t.Fatalf("expected dotted name to pass through unchanged, got %+v", tools)
	}

	blocks := fromGeminiParts([]geminiPart{{FunctionCall: &geminiFunctionCall{Name: "algoreel.validate_spec", Args: json.RawMessage(`{}`)}}})
	if len(blocks) != 1 || blocks[0].Name != "algoreel.validate_spec" {
		t.Fatalf("expected the dotted name to come back unchanged, got %+v", blocks)
	}
}

// --- HTTP-level tests ---

func newTestGemini(t *testing.T, handler http.HandlerFunc) *Gemini {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewGemini("test-key", srv.URL)
}

// writeGeminiSSELine marshals v as a bare "data: <json>" SSE line —
// Gemini's streaming frames have no event: field, unlike Anthropic's.
func writeGeminiSSELine(t *testing.T, w http.ResponseWriter, fl http.Flusher, v geminiResponse) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal SSE payload: %v", err)
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		t.Fatalf("write SSE line: %v", err)
	}
	fl.Flush()
}

func TestGeminiUsesAPIKeyHeaderNotBearer(t *testing.T) {
	g := newTestGemini(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-goog-api-key"); got != "test-key" {
			t.Errorf("expected x-goog-api-key %q, got %q", "test-key", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("expected no Authorization header, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(geminiResponse{Candidates: []geminiCandidate{{FinishReason: "STOP"}}})
	})
	if _, err := g.Complete(context.Background(), Request{Model: "gemini-3.7-flash"}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
}

func TestGeminiUsageIncludesThinkingTokens(t *testing.T) {
	g := newTestGemini(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(geminiResponse{
			Candidates: []geminiCandidate{{
				Content:      geminiContent{Role: "model", Parts: []geminiPart{{Text: "hi"}}},
				FinishReason: "STOP",
			}},
			UsageMetadata: geminiUsageMetadata{PromptTokenCount: 10, CandidatesTokenCount: 13, ThoughtsTokenCount: 138},
		})
	})
	resp, err := g.Complete(context.Background(), Request{Model: "m"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 13+138 {
		t.Fatalf("expected usage {10,151} (candidates+thoughts), got %+v", resp.Usage)
	}
}

// TestGeminiIgnoresResponseSchema pins the "fallback-only for v1" scope
// decision: Gemini's responseSchema is an OpenAPI subset that rejects
// $schema/additionalProperties, so Request.ResponseSchema must never
// surface as generationConfig.responseSchema on the wire — the engine's
// validate-and-retry path is the only structured-output mechanism Gemini
// gets. This is a regression guard so native support can't be
// half-implemented by accident later.
func TestGeminiIgnoresResponseSchema(t *testing.T) {
	var captured map[string]any
	g := newTestGemini(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(geminiResponse{Candidates: []geminiCandidate{{FinishReason: "STOP"}}})
	})
	_, err := g.Complete(context.Background(), Request{Model: "m", ResponseSchema: json.RawMessage(`{"type":"object"}`)})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if gc, ok := captured["generationConfig"].(map[string]any); ok {
		if _, ok := gc["responseSchema"]; ok {
			t.Fatalf("expected no responseSchema field, got %v", gc["responseSchema"])
		}
	}
	if g.Capabilities().StructuredOutput {
		t.Fatal("expected Gemini to not advertise native StructuredOutput support")
	}
}

func TestGeminiSystemInstructionIsTopLevelNotAContent(t *testing.T) {
	var captured geminiRequest
	g := newTestGemini(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(geminiResponse{Candidates: []geminiCandidate{{FinishReason: "STOP"}}})
	})
	_, err := g.Complete(context.Background(), Request{
		Model:    "m",
		System:   "you are a test assistant",
		Messages: []message.Message{message.Text(message.RoleUser, "hi")},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if captured.SystemInstruction == nil || len(captured.SystemInstruction.Parts) != 1 || captured.SystemInstruction.Parts[0].Text != "you are a test assistant" {
		t.Fatalf("expected systemInstruction to carry the system prompt, got %+v", captured.SystemInstruction)
	}
	for _, c := range captured.Contents {
		if c.Role != "user" && c.Role != "model" {
			t.Fatalf("expected only user/model roles in contents, got %+v", c)
		}
	}
}

// TestGeminiStreamAssemblesTextAndToolUse mirrors
// TestAnthropicStreamAssemblesTextAndToolUse: text arrives across
// several chunks, a signature-bearing functionCall arrives whole (no
// jsonBuf needed, unlike Anthropic's input_json_delta fragments), and the
// stream ends at EOF with no terminal frame.
func TestGeminiStreamAssemblesTextAndToolUse(t *testing.T) {
	g := newTestGemini(t, func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		writeGeminiSSELine(t, w, fl, geminiResponse{
			Candidates: []geminiCandidate{{Content: geminiContent{Role: "model", Parts: []geminiPart{{Text: "Hel"}}}}},
		})
		writeGeminiSSELine(t, w, fl, geminiResponse{
			Candidates: []geminiCandidate{{Content: geminiContent{Role: "model", Parts: []geminiPart{{Text: "lo"}}}}},
		})
		writeGeminiSSELine(t, w, fl, geminiResponse{
			Candidates: []geminiCandidate{{
				Content: geminiContent{Role: "model", Parts: []geminiPart{{
					FunctionCall:     &geminiFunctionCall{Name: "sum", Args: json.RawMessage(`{"a":1,"b":2}`), ID: "call_1"},
					ThoughtSignature: "sig-xyz",
				}}},
				FinishReason: "STOP",
			}},
			UsageMetadata: geminiUsageMetadata{PromptTokenCount: 10, CandidatesTokenCount: 15, ThoughtsTokenCount: 5},
		})
	})

	stream, err := g.Stream(context.Background(), Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	deltas := drainStream(t, stream)
	if err := stream.Err(); err != nil {
		t.Fatalf("unexpected stream error: %v", err)
	}
	if strings.Join(deltas, "") != "Hello" {
		t.Fatalf("expected deltas to join into %q, got %q", "Hello", strings.Join(deltas, ""))
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
	if tu.Type != message.BlockToolUse || tu.Name != "sum" || tu.ID != "call_1" || tu.Signature != "sig-xyz" {
		t.Fatalf("expected a signed sum tool_use block, got %+v", tu)
	}
	if resp.StopReason != "tool_use" {
		t.Fatalf("expected tool_use (finishReason STOP notwithstanding), got %q", resp.StopReason)
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 20 {
		t.Fatalf("expected usage {10,20} (candidates+thoughts), got %+v", resp.Usage)
	}
}

// TestGeminiStreamTokensArriveIncrementally mirrors
// TestAnthropicStreamTokensArriveIncrementally: the handler blocks after
// the first delta until the test signals, so a buffer-then-parse
// implementation would deadlock here instead of passing.
func TestGeminiStreamTokensArriveIncrementally(t *testing.T) {
	release := make(chan struct{})
	g := newTestGemini(t, func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		writeGeminiSSELine(t, w, fl, geminiResponse{
			Candidates: []geminiCandidate{{Content: geminiContent{Role: "model", Parts: []geminiPart{{Text: "first "}}}}},
		})
		<-release
		writeGeminiSSELine(t, w, fl, geminiResponse{
			Candidates: []geminiCandidate{{Content: geminiContent{Role: "model", Parts: []geminiPart{{Text: "second"}}}, FinishReason: "STOP"}},
		})
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := g.Stream(ctx, Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

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

func TestGeminiStreamDecodeErrorSurfacesOnErrAndResponse(t *testing.T) {
	g := newTestGemini(t, func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		if _, err := fmt.Fprint(w, "data: not-json\n\n"); err != nil {
			t.Fatalf("write SSE line: %v", err)
		}
		fl.Flush()
	})

	stream, err := g.Stream(context.Background(), Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	for stream.Next() {
	}
	if stream.Err() == nil {
		t.Fatal("expected an error after an undecodable stream chunk")
	}
	if _, err := stream.Response(); err == nil {
		t.Fatal("expected Response to surface the same error")
	}
}
