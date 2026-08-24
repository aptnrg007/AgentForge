package agentforge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentforge/internal/config"
	"agentforge/internal/mcp"
	"agentforge/internal/message"
	"agentforge/internal/provider"
	"agentforge/internal/provider/replay"
	"agentforge/internal/store"
)

const testAgentYAML = `name: test-agent
model:
  provider: ollama
  name: test-model
instructions: "test agent"
limits:
  max_turns: 5
`

// newTestAgent builds an *Agent the way Load would, but against a
// replay.Provider instead of a real one — Load itself always uses
// agent.DefaultProviderFactory, which would try to reach a real Ollama,
// so these tests construct the struct directly (same package, so its
// unexported fields are reachable) rather than going through Load.
func newTestAgent(t *testing.T, cfgYAML string, responses []*provider.Response) *Agent {
	t.Helper()
	cfg, err := config.Parse([]byte(cfgYAML))
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.UpsertAgent(context.Background(), cfg.Name, cfgYAML); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}

	rp := replay.New(responses, provider.Capabilities{})
	return &Agent{
		cfg:      cfg,
		st:       st,
		registry: mcp.NewRegistry(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 1}))),
		pf:       func(config.ModelConfig) (provider.Provider, error) { return rp, nil },
	}
}

func textResponse(text string) *provider.Response {
	return &provider.Response{
		Content:    []message.ContentBlock{{Type: message.BlockText, Text: text}},
		StopReason: "end_turn",
	}
}

func toolUseResponse(id, name, input string) *provider.Response {
	return &provider.Response{
		Content:    []message.ContentBlock{{Type: message.BlockToolUse, ID: id, Name: name, Input: json.RawMessage(input)}},
		StopReason: "tool_use",
	}
}

func TestRunCompletesAndReportsOutput(t *testing.T) {
	a := newTestAgent(t, testAgentYAML, []*provider.Response{textResponse("hello from the model")})
	defer a.registry.Close()

	run, err := a.Run(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if run.State != "completed" {
		t.Fatalf("State = %q, want completed", run.State)
	}
	if run.Output != "hello from the model" {
		t.Fatalf("Output = %q, want %q", run.Output, "hello from the model")
	}
}

// dangerToolAgentYAML builds a config with a real, registered
// "danger.tool" (an HTTP tool_definitions entry pointed at ts) gated by
// approvals.require — a scripted tool_use call to it needs to resolve to
// a genuinely registered tool, or validateToolUse rejects it as unknown
// before approval is ever evaluated, which would make these tests pass
// for the wrong reason (repair, not approval).
func dangerToolAgentYAML(ts *httptest.Server) string {
	return testAgentYAML + fmt.Sprintf(`approvals:
  require: ["danger.tool"]
tool_definitions:
  - name: danger.tool
    description: a dangerous tool
    input_schema:
      type: object
    http:
      url: %s
`, ts.URL)
}

func TestRunStopsAtAwaitingApprovalThenApproveResumeCompletes(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`)) //nolint:errcheck
	}))
	defer ts.Close()

	a := newTestAgent(t, dangerToolAgentYAML(ts), []*provider.Response{
		toolUseResponse("call_1", "danger.tool", `{}`),
		textResponse("done after approval"),
	})
	defer a.registry.Close()

	ctx := context.Background()
	run, err := a.Run(ctx, "please do the dangerous thing")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if run.State != "awaiting_approval" {
		t.Fatalf("State = %q, want awaiting_approval", run.State)
	}
	if len(run.Pending) != 1 || run.Pending[0].Tool != "danger.tool" {
		t.Fatalf("Pending = %+v, want one call to danger.tool", run.Pending)
	}

	if err := a.Approve(ctx, run.ID, run.Pending[0].CallID, "looks fine"); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	final, err := a.Resume(ctx, run.ID)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if final.State != "completed" {
		t.Fatalf("final State = %q, want completed (error=%v)", final.State, final.Error)
	}
	if final.Output != "done after approval" {
		t.Fatalf("final Output = %q, want %q", final.Output, "done after approval")
	}
}

func TestDenyFeedsAnErrorBackAndRunCompletes(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`)) //nolint:errcheck
	}))
	defer ts.Close()

	a := newTestAgent(t, dangerToolAgentYAML(ts), []*provider.Response{
		toolUseResponse("call_1", "danger.tool", `{}`),
		textResponse("okay, skipping that"),
	})
	defer a.registry.Close()

	ctx := context.Background()
	run, err := a.Run(ctx, "please do the dangerous thing")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := a.Deny(ctx, run.ID, run.Pending[0].CallID, "not today"); err != nil {
		t.Fatalf("Deny: %v", err)
	}
	final, err := a.Resume(ctx, run.ID)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if final.State != "completed" {
		t.Fatalf("final State = %q, want completed", final.State)
	}
}

func TestCancelStopsANonTerminalRun(t *testing.T) {
	a := newTestAgent(t, testAgentYAML, nil)
	defer a.registry.Close()
	ctx := context.Background()

	if err := a.st.CreateRun(ctx, "run-1", "test-agent", "ready_for_model"); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	state, err := a.Cancel(ctx, "run-1")
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if state != "cancelled" {
		t.Fatalf("state = %q, want cancelled", state)
	}

	if _, err := a.Cancel(ctx, "run-1"); err == nil {
		t.Fatal("expected cancelling an already-cancelled run to error")
	}
}

func TestWithEventsReceivesTokenEvents(t *testing.T) {
	a := newTestAgent(t, testAgentYAML, []*provider.Response{textResponse("streamed answer")})
	defer a.registry.Close()

	var tokens []string
	_, err := a.Run(context.Background(), "hi", WithEvents(func(ev Event) {
		if ev.Kind == EventToken {
			tokens = append(tokens, ev.Text)
		}
	}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := strings.Join(tokens, ""); got != "streamed answer" {
		t.Fatalf("streamed tokens = %q, want %q", got, "streamed answer")
	}
}

func TestLoadOpensStoreAndRegistersAgent(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent.yaml")
	if err := os.WriteFile(cfgPath, []byte(testAgentYAML), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	dbPath := filepath.Join(dir, "store.db")

	ag, err := Load(cfgPath, WithDB(dbPath))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer ag.Close()

	stored, err := ag.st.GetAgent(context.Background(), "test-agent")
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if stored.YAML != testAgentYAML {
		t.Fatalf("stored YAML = %q, want %q", stored.YAML, testAgentYAML)
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("expected Load to create the db at %s: %v", dbPath, err)
	}
}
