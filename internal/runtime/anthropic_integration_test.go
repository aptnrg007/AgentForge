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

// These drive the real Engine against a real provider.Anthropic pointed
// at an httptest.Server emulating the Messages API — not a fake
// provider.Provider — so the roundtrip actually exercises
// toAnthropicMessages a second time on the resumed turn, which is
// precisely where an ordering or is_error mistake would surface (the
// roadmap's own "a 400, not a soft failure" warning). No live Anthropic
// account is available in this sandbox, so this is the strongest
// available verification of the acceptance criterion "approval, denial,
// and resume all work" against a real Anthropic-shaped wire protocol.

// anthropicScript replays scripted Messages-API responses keyed by call
// count and records the raw request bodies it received, so a test can
// assert on the exact JSON the engine sent back on a resumed turn.
type anthropicScript struct {
	t         *testing.T
	responses []map[string]any
	requests  []map[string]any
}

func newAnthropicScript(t *testing.T, responses ...map[string]any) *anthropicScript {
	return &anthropicScript{t: t, responses: responses}
}

func (s *anthropicScript) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			s.t.Fatalf("decode request body: %v", err)
		}
		s.requests = append(s.requests, body)

		if len(s.responses) == 0 {
			s.t.Fatalf("anthropicScript: no more scripted responses (call %d)", len(s.requests))
		}
		resp := s.responses[0]
		s.responses = s.responses[1:]

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			s.t.Fatalf("encode scripted response: %v", err)
		}
	}
}

func anthropicToolUseResponse(id, name string, input map[string]any) map[string]any {
	return map[string]any{
		"content": []map[string]any{
			{"type": "tool_use", "id": id, "name": name, "input": input},
		},
		"stop_reason": "tool_use",
		"usage":       map[string]int{"input_tokens": 10, "output_tokens": 5},
	}
}

func anthropicTextResponse(text string) map[string]any {
	return map[string]any{
		"content":     []map[string]any{{"type": "text", "text": text}},
		"stop_reason": "end_turn",
		"usage":       map[string]int{"input_tokens": 10, "output_tokens": 5},
	}
}

func TestAnthropicEndToEndRequireApprovalThenDeny(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))

	script := newAnthropicScript(t,
		// Real Anthropic can only ever send back "danger__tool" — it
		// declared that mangled name in tools[0].name on the outbound
		// request, since "danger.tool" would 400 the request outright
		// (tools.0.custom.name must match ^[a-zA-Z0-9_-]{1,128}$). This
		// fixture would have been a lie before the toWireToolName fix.
		anthropicToolUseResponse("toolu_1", "danger__tool", map[string]any{"x": float64(1)}),
		anthropicTextResponse("no problem, trying something else"),
	)
	srv := httptest.NewServer(script.handler())
	defer srv.Close()

	ap := provider.NewAnthropic("test-key", srv.URL)
	eng := NewEngine(st, ap, Config{
		AgentName: "test-agent", Model: "claude-sonnet-4-6", MaxTurns: 10,
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

	runID := "run-anthropic-deny"
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

	if _, err := eng.RecordApproval(ctx, runID, "toolu_1", "denied", "user", "no thanks"); err != nil {
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

	// The exact field the 400 in the reported bug complains about: the
	// dotted tool name must have been mangled before it ever reached
	// Anthropic, on the very first request.
	firstTools, ok := script.requests[0]["tools"].([]any)
	if !ok || len(firstTools) != 1 {
		t.Fatalf("expected 1 declared tool on the first request, got %+v", script.requests[0]["tools"])
	}
	if name := firstTools[0].(map[string]any)["name"]; name != "danger__tool" {
		t.Fatalf("expected the declared tool name to be mangled to danger__tool, got %v", name)
	}

	// The crux of the "watch out for" warnings: on the resumed turn, the
	// denial's synthesized error must reach Anthropic as a tool_result
	// with is_error true, in a message immediately following the
	// assistant's tool_use — not as a free-floating user message.
	if len(script.requests) != 2 {
		t.Fatalf("expected 2 requests sent to Anthropic, got %d", len(script.requests))
	}
	msgs, ok := script.requests[1]["messages"].([]any)
	if !ok || len(msgs) < 3 {
		t.Fatalf("expected at least 3 messages on the resumed request, got %+v", script.requests[1]["messages"])
	}
	toolResultMsg := msgs[2].(map[string]any)
	if toolResultMsg["role"] != "user" {
		t.Fatalf("expected the tool_result message to have role user, got %+v", toolResultMsg)
	}
	content := toolResultMsg["content"].([]any)[0].(map[string]any)
	if content["type"] != "tool_result" || content["tool_use_id"] != "toolu_1" {
		t.Fatalf("expected a tool_result referencing toolu_1, got %+v", content)
	}
	if content["is_error"] != true {
		t.Fatalf("expected is_error true on the denied call's tool_result, got %+v", content)
	}
}

func TestAnthropicEndToEndApprovalThenResume(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))

	script := newAnthropicScript(t,
		anthropicToolUseResponse("toolu_1", "echo", map[string]any{"message": "hi"}),
		anthropicTextResponse("done: hi"),
	)
	srv := httptest.NewServer(script.handler())
	defer srv.Close()

	ap := provider.NewAnthropic("test-key", srv.URL)
	eng := NewEngine(st, ap, Config{
		AgentName: "test-agent", Model: "claude-sonnet-4-6", MaxTurns: 10,
		Approvals: ApprovalPolicy{Require: []string{"echo"}},
	})
	eng.RegisterTool(NewEchoTool())

	runID := "run-anthropic-approve"
	if err := eng.NewRun(ctx, runID, "please echo hi"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	if state, err := eng.Step(ctx, runID); err != nil || state != StateAwaitingApproval {
		t.Fatalf("expected awaiting_approval, got %v (err=%v)", state, err)
	}
	if _, err := eng.RecordApproval(ctx, runID, "toolu_1", "approved", "user", ""); err != nil {
		t.Fatalf("RecordApproval: %v", err)
	}

	finalState := runToTerminal(t, eng, runID)
	if finalState != StateCompleted {
		run, _ := st.GetRun(ctx, runID)
		t.Fatalf("expected completed, got %s (error=%v)", finalState, run.Error)
	}

	msgs, err := st.ListMessages(ctx, runID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if msgs[2].Content[0].Content != "hi" {
		t.Fatalf("expected the echoed tool result 'hi', got %+v", msgs[2])
	}
}
