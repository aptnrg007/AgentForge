package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"agentforge/internal/provider"
)

// These drive the real Engine against a real provider.Gemini pointed at
// an httptest.Server emulating generateContent — not a fake
// provider.Provider — so the roundtrip actually exercises
// toGeminiContents a second time on the resumed turn. That is precisely
// where the bug this provider exists to fix would resurface: a
// thoughtSignature dropped anywhere in the
// fromGeminiParts -> AppendMessage -> SQLite -> ListMessages ->
// toGeminiContents path reproduces Gemini's real
// "missing a thought_signature in functionCall parts" 400. Mirrors
// anthropic_integration_test.go.

// geminiScript replays scripted generateContent responses keyed by call
// count and records the raw request bodies it received, so a test can
// assert on the exact JSON sent on a resumed turn.
type geminiScript struct {
	t         *testing.T
	responses []map[string]any
	requests  []map[string]any
}

func newGeminiScript(t *testing.T, responses ...map[string]any) *geminiScript {
	return &geminiScript{t: t, responses: responses}
}

func (s *geminiScript) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			s.t.Fatalf("decode request body: %v", err)
		}
		s.requests = append(s.requests, body)

		if len(s.responses) == 0 {
			s.t.Fatalf("geminiScript: no more scripted responses (call %d)", len(s.requests))
		}
		resp := s.responses[0]
		s.responses = s.responses[1:]

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			s.t.Fatalf("encode scripted response: %v", err)
		}
	}
}

func geminiToolUseResponse(id, name string, input map[string]any, signature string) map[string]any {
	part := map[string]any{
		"functionCall": map[string]any{"id": id, "name": name, "args": input},
	}
	if signature != "" {
		part["thoughtSignature"] = signature
	}
	return map[string]any{
		"candidates": []map[string]any{
			{"content": map[string]any{"role": "model", "parts": []map[string]any{part}}, "finishReason": "STOP"},
		},
		"usageMetadata": map[string]int{"promptTokenCount": 10, "candidatesTokenCount": 5},
	}
}

func geminiTextResponse(text string) map[string]any {
	return map[string]any{
		"candidates": []map[string]any{
			{"content": map[string]any{"role": "model", "parts": []map[string]any{{"text": text}}}, "finishReason": "STOP"},
		},
		"usageMetadata": map[string]int{"promptTokenCount": 10, "candidatesTokenCount": 5},
	}
}

// TestGeminiEndToEndSignatureSurvivesPersistenceAndReplay is the
// acceptance test for this provider: it proves a thoughtSignature
// attached to turn one's functionCall part is still present, byte for
// byte, on the exact same part when the resumed turn replays it — the
// one thing no translation-level unit test can verify, since it never
// exercises the store.
func TestGeminiEndToEndSignatureSurvivesPersistenceAndReplay(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))

	script := newGeminiScript(t,
		geminiToolUseResponse("call_1", "echo", map[string]any{"message": "hi"}, "sig-abc"),
		geminiTextResponse("done: hi"),
	)
	srv := httptest.NewServer(script.handler())
	defer srv.Close()

	gp := provider.NewGemini("test-key", srv.URL)
	eng := NewEngine(st, gp, Config{AgentName: "test-agent", Model: "gemini-3.7-flash", MaxTurns: 10})
	eng.RegisterTool(NewEchoTool())

	runID := "run-gemini-signature"
	if err := eng.NewRun(ctx, runID, "please echo hi"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}

	finalState := runToTerminal(t, eng, runID)
	if finalState != StateCompleted {
		run, _ := st.GetRun(ctx, runID)
		t.Fatalf("expected completed, got %s (error=%v)", finalState, run.Error)
	}

	if len(script.requests) != 2 {
		t.Fatalf("expected 2 requests sent to Gemini, got %d", len(script.requests))
	}

	contents, ok := script.requests[1]["contents"].([]any)
	if !ok || len(contents) < 2 {
		t.Fatalf("expected at least 2 contents on the resumed request, got %+v", script.requests[1]["contents"])
	}
	assistantContent := contents[1].(map[string]any)
	if assistantContent["role"] != "model" {
		t.Fatalf("expected the replayed tool_use content to have role model, got %+v", assistantContent)
	}
	parts, ok := assistantContent["parts"].([]any)
	if !ok || len(parts) == 0 {
		t.Fatalf("expected at least one part on the replayed content, got %+v", assistantContent)
	}
	part := parts[0].(map[string]any)
	if part["thoughtSignature"] != "sig-abc" {
		t.Fatalf("expected thoughtSignature %q to survive persistence and replay, got %+v", "sig-abc", part)
	}
	fc, ok := part["functionCall"].(map[string]any)
	if !ok || fc["name"] != "echo" {
		t.Fatalf("expected the replayed functionCall to be echo, got %+v", part)
	}
}

// TestGeminiEndToEndRequireApprovalThenDeny mirrors
// TestAnthropicEndToEndRequireApprovalThenDeny: a denied approval's
// synthesized error must reach Gemini as a functionResponse with the
// ERROR: prefix (Gemini has no is_error field), in a content immediately
// following the model's functionCall — not as a free-floating user turn
// — and the tool itself must never execute.
func TestGeminiEndToEndRequireApprovalThenDeny(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))

	script := newGeminiScript(t,
		geminiToolUseResponse("call_1", "danger.tool", map[string]any{"x": float64(1)}, ""),
		geminiTextResponse("no problem, trying something else"),
	)
	srv := httptest.NewServer(script.handler())
	defer srv.Close()

	gp := provider.NewGemini("test-key", srv.URL)
	eng := NewEngine(st, gp, Config{
		AgentName: "test-agent", Model: "gemini-3.7-flash", MaxTurns: 10,
		Approvals: ApprovalPolicy{Require: []string{"danger.tool"}},
	})
	var executed bool
	eng.RegisterTool(Tool{
		Name: "danger.tool", InputSchema: json.RawMessage(`{}`),
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			executed = true
			return "should not run", nil
		},
	})

	runID := "run-gemini-deny"
	if err := eng.NewRun(ctx, runID, "do the dangerous thing"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}

	state, err := eng.Step(ctx, runID)
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if state != StateAwaitingApproval {
		t.Fatalf("expected awaiting_approval, got %s", state)
	}

	if _, err := eng.RecordApproval(ctx, runID, "call_1", "denied", "user", "no thanks"); err != nil {
		t.Fatalf("RecordApproval: %v", err)
	}

	finalState := runToTerminal(t, eng, runID)
	if finalState != StateCompleted {
		run, _ := st.GetRun(ctx, runID)
		t.Fatalf("expected completed, got %s (error=%v)", finalState, run.Error)
	}
	if executed {
		t.Fatal("a denied tool must never execute")
	}

	if len(script.requests) != 2 {
		t.Fatalf("expected 2 requests sent to Gemini, got %d", len(script.requests))
	}
	contents, ok := script.requests[1]["contents"].([]any)
	if !ok || len(contents) < 3 {
		t.Fatalf("expected at least 3 contents on the resumed request, got %+v", script.requests[1]["contents"])
	}
	toolResultContent := contents[2].(map[string]any)
	if toolResultContent["role"] != "user" {
		t.Fatalf("expected the functionResponse content to have role user, got %+v", toolResultContent)
	}
	part := toolResultContent["parts"].([]any)[0].(map[string]any)
	fr, ok := part["functionResponse"].(map[string]any)
	if !ok || fr["name"] != "danger.tool" {
		t.Fatalf("expected a functionResponse referencing danger.tool, got %+v", part)
	}
	respBody, ok := fr["response"].(map[string]any)
	if !ok {
		t.Fatalf("expected functionResponse.response to be an object, got %+v", fr["response"])
	}
	result, _ := respBody["result"].(string)
	if result == "" || result[:6] != "ERROR:" {
		t.Fatalf("expected a denied call's result to carry the ERROR: prefix, got %q", result)
	}
}
