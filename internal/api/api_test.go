package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"agentforge/internal/agent"
	"agentforge/internal/config"
	"agentforge/internal/mcp"
	"agentforge/internal/message"
	"agentforge/internal/provider"
	"agentforge/internal/runtime"
	"agentforge/internal/store"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func requireNpx(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("npx"); err != nil {
		t.Skip("npx not available; skipping test against the real MCP server")
	}
}

// scriptedProvider replays fixed responses, standing in for a real LLM.
type scriptedProvider struct {
	responses []*provider.Response
	calls     int
}

func (p *scriptedProvider) Name() string { return "scripted" }

func (p *scriptedProvider) Complete(ctx context.Context, r provider.Request) (*provider.Response, error) {
	if p.calls >= len(p.responses) {
		return nil, fmt.Errorf("scripted provider: no more responses")
	}
	resp := p.responses[p.calls]
	p.calls++
	return resp, nil
}

func (p *scriptedProvider) Stream(ctx context.Context, r provider.Request) (provider.Stream, error) {
	resp, err := p.Complete(ctx, r)
	if err != nil {
		return nil, err
	}
	return provider.NewResponseStream(resp), nil
}

func (p *scriptedProvider) Capabilities() provider.Capabilities { return provider.Capabilities{} }

func fakeProviderFactory(responses ...*provider.Response) agent.ProviderFactory {
	sp := &scriptedProvider{responses: responses}
	return func(model config.ModelConfig) (provider.Provider, error) {
		return sp, nil
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
		Content: []message.ContentBlock{{
			Type: message.BlockToolUse, ID: id, Name: name, Input: json.RawMessage(input),
		}},
		StopReason: "tool_use",
	}
}

func newTestServer(t *testing.T, factory agent.ProviderFactory) *httptest.Server {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	registry := mcp.NewRegistry(discardLogger())
	t.Cleanup(func() { registry.Close() })

	srv := &Server{store: st, registry: registry, logger: discardLogger(), providerFactory: factory}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func postAgent(t *testing.T, ts *httptest.Server, yaml string) agentSummary {
	t.Helper()
	resp, err := http.Post(ts.URL+"/v1/agents", "text/yaml", strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("POST /v1/agents: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/agents: status %d: %s", resp.StatusCode, body)
	}
	var out agentSummary
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode agent response: %v (body=%s)", err, body)
	}
	return out
}

const minimalYAML = `
name: minimal
model:
  provider: ollama
  name: test-model
instructions: you are a test assistant
limits:
  max_turns: 10
`

func TestCreateAgentThenRun(t *testing.T) {
	ts := newTestServer(t, fakeProviderFactory(textResponse("hello back")))

	created := postAgent(t, ts, minimalYAML)
	if created.Name != "minimal" {
		t.Fatalf("created.Name = %q, want %q", created.Name, "minimal")
	}

	resp, err := http.Post(ts.URL+"/v1/agents/minimal/run", "application/json", strings.NewReader(`{"message":"hi"}`))
	if err != nil {
		t.Fatalf("POST run: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST run: status %d: %s", resp.StatusCode, body)
	}

	var out runResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode run response: %v (body=%s)", err, body)
	}
	if out.State != "completed" {
		t.Fatalf("state = %q, want completed (body=%s)", out.State, body)
	}
	if out.RunID == "" {
		t.Fatal("expected a non-empty run_id")
	}

	var gotText string
	for _, m := range out.Messages {
		for _, b := range m.Content {
			if b.Type == message.BlockText && m.Role == message.RoleAssistant {
				gotText = b.Text
			}
		}
	}
	if gotText != "hello back" {
		t.Fatalf("assistant text = %q, want %q", gotText, "hello back")
	}
}

func TestRunUnknownAgentReturns404(t *testing.T) {
	ts := newTestServer(t, fakeProviderFactory(textResponse("unused")))

	resp, err := http.Post(ts.URL+"/v1/agents/nope/run", "application/json", strings.NewReader(`{"message":"hi"}`))
	if err != nil {
		t.Fatalf("POST run: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

const everythingDemoYAML = `
name: everything-demo
model:
  provider: ollama
  name: test-model
mcp:
  - name: everything
    transport: stdio
    command: ["npx", "-y", "@modelcontextprotocol/server-everything"]
tools:
  - "everything.echo"
  - "everything.get-sum"
`

func TestAgentToolsEndpointResolvesFilteredNamespacedList(t *testing.T) {
	requireNpx(t)
	ts := newTestServer(t, fakeProviderFactory())

	postAgent(t, ts, everythingDemoYAML)

	resp, err := http.Get(ts.URL + "/v1/agents/everything-demo/tools")
	if err != nil {
		t.Fatalf("GET tools: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET tools: status %d: %s", resp.StatusCode, body)
	}

	var tools []toolInfo
	if err := json.Unmarshal(body, &tools); err != nil {
		t.Fatalf("decode tools: %v (body=%s)", err, body)
	}
	if len(tools) != 2 {
		t.Fatalf("expected exactly 2 filtered tools, got %d: %s", len(tools), body)
	}
	names := map[string]bool{}
	for _, tool := range tools {
		names[tool.Name] = true
		if tool.Description == "" {
			t.Errorf("tool %s missing description", tool.Name)
		}
	}
	if !names["everything.echo"] || !names["everything.get-sum"] {
		t.Fatalf("expected everything.echo and everything.get-sum, got %v", names)
	}
}

func TestGetRunTraceIncludesMessagesAndToolCalls(t *testing.T) {
	requireNpx(t)
	ts := newTestServer(t, fakeProviderFactory(
		toolUseResponse("call_1", "everything.echo", `{"message":"via api"}`),
		textResponse("done"),
	))

	postAgent(t, ts, everythingDemoYAML)

	resp, err := http.Post(ts.URL+"/v1/agents/everything-demo/run", "application/json", strings.NewReader(`{"message":"please echo"}`))
	if err != nil {
		t.Fatalf("POST run: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST run: status %d: %s", resp.StatusCode, body)
	}
	var runOut runResponse
	if err := json.Unmarshal(body, &runOut); err != nil {
		t.Fatalf("decode run response: %v", err)
	}
	if runOut.State != "completed" {
		t.Fatalf("state = %q, want completed (body=%s)", runOut.State, body)
	}

	traceResp, err := http.Get(ts.URL + "/v1/runs/" + runOut.RunID)
	if err != nil {
		t.Fatalf("GET run trace: %v", err)
	}
	defer traceResp.Body.Close()
	traceBody, _ := io.ReadAll(traceResp.Body)
	if traceResp.StatusCode != http.StatusOK {
		t.Fatalf("GET run trace: status %d: %s", traceResp.StatusCode, traceBody)
	}

	var trace runTrace
	if err := json.Unmarshal(traceBody, &trace); err != nil {
		t.Fatalf("decode trace: %v (body=%s)", err, traceBody)
	}
	if trace.State != "completed" {
		t.Fatalf("trace.State = %q, want completed", trace.State)
	}
	if len(trace.Messages) == 0 {
		t.Fatal("expected a non-empty message trace")
	}
	if len(trace.ToolCalls) != 1 {
		t.Fatalf("expected exactly 1 tool call in the trace, got %d: %+v", len(trace.ToolCalls), trace.ToolCalls)
	}
	tc := trace.ToolCalls[0]
	if tc.ToolName != "everything.echo" {
		t.Errorf("tool_name = %q, want everything.echo", tc.ToolName)
	}
	if tc.Approval != "auto" {
		t.Errorf("approval = %q, want auto", tc.Approval)
	}
	if tc.Result == nil || *tc.Result != "Echo: via api" {
		t.Errorf("result = %v, want \"Echo: via api\"", tc.Result)
	}
}

func TestGetRunUnknownReturns404(t *testing.T) {
	ts := newTestServer(t, fakeProviderFactory())
	resp, err := http.Get(ts.URL + "/v1/runs/does-not-exist")
	if err != nil {
		t.Fatalf("GET run: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestDaemonRestartAgentsIntact(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "restart.db")

	st1, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	registry1 := mcp.NewRegistry(discardLogger())
	srv1 := &Server{store: st1, registry: registry1, logger: discardLogger(), providerFactory: fakeProviderFactory()}
	ts1 := httptest.NewServer(srv1.Handler())

	postAgent(t, ts1, minimalYAML)

	// Simulate the daemon restarting: tear everything down...
	ts1.Close()
	registry1.Close()
	if err := st1.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	// ...and bring up a fresh daemon pointed at the same DB file.
	st2, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer st2.Close()
	registry2 := mcp.NewRegistry(discardLogger())
	defer registry2.Close()
	srv2 := &Server{store: st2, registry: registry2, logger: discardLogger(), providerFactory: fakeProviderFactory()}
	ts2 := httptest.NewServer(srv2.Handler())
	defer ts2.Close()

	resp, err := http.Get(ts2.URL + "/v1/agents/minimal")
	if err != nil {
		t.Fatalf("GET agent after restart: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET agent after restart: status %d: %s", resp.StatusCode, body)
	}
	var ag agentSummary
	if err := json.Unmarshal(body, &ag); err != nil {
		t.Fatalf("decode agent: %v", err)
	}
	if ag.Name != "minimal" || !strings.Contains(ag.YAML, "name: minimal") {
		t.Fatalf("agent did not survive restart intact: %+v", ag)
	}
}

const approvalsDemoYAML = `
name: approvals-demo
model:
  provider: ollama
  name: test-model
mcp:
  - name: everything
    transport: stdio
    command: ["npx", "-y", "@modelcontextprotocol/server-everything"]
tools:
  - "everything.echo"
approvals:
  require:
    - "everything.echo"
`

func TestRunPausesForApprovalThenApproveAndResume(t *testing.T) {
	requireNpx(t)
	ts := newTestServer(t, fakeProviderFactory(
		toolUseResponse("call_1", "everything.echo", `{"message":"gated"}`),
		textResponse("done"),
	))

	postAgent(t, ts, approvalsDemoYAML)

	resp, err := http.Post(ts.URL+"/v1/agents/approvals-demo/run", "application/json", strings.NewReader(`{"message":"please echo"}`))
	if err != nil {
		t.Fatalf("POST run: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST run: status %d, want 202: %s", resp.StatusCode, body)
	}

	var runOut runResponse
	if err := json.Unmarshal(body, &runOut); err != nil {
		t.Fatalf("decode run response: %v (body=%s)", err, body)
	}
	if runOut.State != "awaiting_approval" {
		t.Fatalf("state = %q, want awaiting_approval", runOut.State)
	}
	if len(runOut.Pending) != 1 || runOut.Pending[0].Tool != "everything.echo" {
		t.Fatalf("expected exactly one pending call for everything.echo, got %+v", runOut.Pending)
	}
	callID := runOut.Pending[0].CallID
	if callID == "" {
		t.Fatal("expected a non-empty call_id")
	}

	// Approve: does not by itself continue the run.
	approveBody, _ := json.Marshal(map[string]string{"call_id": callID, "decision": "approved"})
	aResp, err := http.Post(ts.URL+"/v1/runs/"+runOut.RunID+"/approve", "application/json", bytes.NewReader(approveBody))
	if err != nil {
		t.Fatalf("POST approve: %v", err)
	}
	aBody, _ := io.ReadAll(aResp.Body)
	aResp.Body.Close()
	if aResp.StatusCode != http.StatusOK {
		t.Fatalf("POST approve: status %d: %s", aResp.StatusCode, aBody)
	}

	// Resume: drives the engine forward, executing the approved call.
	rResp, err := http.Post(ts.URL+"/v1/runs/"+runOut.RunID+"/resume", "application/json", nil)
	if err != nil {
		t.Fatalf("POST resume: %v", err)
	}
	rBody, _ := io.ReadAll(rResp.Body)
	rResp.Body.Close()
	if rResp.StatusCode != http.StatusOK {
		t.Fatalf("POST resume: status %d: %s", rResp.StatusCode, rBody)
	}

	var resumeOut runResponse
	if err := json.Unmarshal(rBody, &resumeOut); err != nil {
		t.Fatalf("decode resume response: %v (body=%s)", err, rBody)
	}
	if resumeOut.State != "completed" {
		t.Fatalf("state after resume = %q, want completed (body=%s)", resumeOut.State, rBody)
	}

	var gotResult string
	for _, m := range resumeOut.Messages {
		for _, b := range m.Content {
			if b.Type == message.BlockToolResult {
				gotResult = b.Content
			}
		}
	}
	if gotResult != "Echo: gated" {
		t.Fatalf("expected the approved call's real MCP result, got %q", gotResult)
	}
}

func TestApproveUnknownCallReturnsConflict(t *testing.T) {
	requireNpx(t)
	ts := newTestServer(t, fakeProviderFactory(
		toolUseResponse("call_1", "everything.echo", `{"message":"gated"}`),
	))
	postAgent(t, ts, approvalsDemoYAML)

	resp, err := http.Post(ts.URL+"/v1/agents/approvals-demo/run", "application/json", strings.NewReader(`{"message":"please echo"}`))
	if err != nil {
		t.Fatalf("POST run: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var runOut runResponse
	if err := json.Unmarshal(body, &runOut); err != nil {
		t.Fatalf("decode: %v", err)
	}

	approveBody, _ := json.Marshal(map[string]string{"call_id": "not-a-real-call-id", "decision": "approved"})
	aResp, err := http.Post(ts.URL+"/v1/runs/"+runOut.RunID+"/approve", "application/json", bytes.NewReader(approveBody))
	if err != nil {
		t.Fatalf("POST approve: %v", err)
	}
	defer aResp.Body.Close()
	if aResp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", aResp.StatusCode)
	}
}

// TestBuildEngineForRunRejectsPlaceholderAgent mirrors
// internal/cli's TestBuildEngineFromStoreRejectsPlaceholderAgent: a run
// whose agent row is Engine.NewRun's empty-YAML placeholder
// (EnsureAgentExists), not a real UpsertAgent — every current API path
// upserts real config via POST /v1/agents before a run can start, so this
// can't happen through the HTTP surface today, but buildEngineForRun is
// the same reconstruction path a future direct-Engine caller would hit.
// Exercises buildEngineForRun directly rather than through the HTTP
// handler, same as the store/registry setup newTestServer normally hides.
func TestBuildEngineForRunRejectsPlaceholderAgent(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	registry := mcp.NewRegistry(discardLogger())
	t.Cleanup(func() { registry.Close() })
	srv := &Server{store: st, registry: registry, logger: discardLogger(), providerFactory: fakeProviderFactory()}

	if err := st.EnsureAgentExists(ctx, "placeholder-agent"); err != nil {
		t.Fatalf("EnsureAgentExists: %v", err)
	}
	if err := st.CreateRun(ctx, "run-placeholder", "placeholder-agent", string(runtime.StateReadyForModel)); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := st.AppendMessage(ctx, "run-placeholder", message.Text(message.RoleUser, "go")); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	_, _, err = srv.buildEngineForRun(ctx, "run-placeholder")
	if err == nil {
		t.Fatal("expected buildEngineForRun to reject a placeholder (empty-YAML) agent")
	}
	if !strings.Contains(err.Error(), "has no stored config") {
		t.Fatalf("expected a clear \"has no stored config\" error, got: %v", err)
	}
}

// TestEngineRunCancelledContextPersistsCancelled exercises
// runtime.Engine.Run (which handleRunAgent/handleResume call directly) as
// the HTTP daemon actually uses it — internal/cli's
// TestDriveLocalRunCancelledContextStopsCleanly covers the same scenario
// for the CLI drive loop. A client disconnect cancels r.Context() the
// same way Ctrl-C cancels the CLI's ctx, and should leave the run at
// StateCancelled instead of stuck non-terminal.
func TestEngineRunCancelledContextPersistsCancelled(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	registry := mcp.NewRegistry(discardLogger())
	t.Cleanup(func() { registry.Close() })
	srv := &Server{store: st, registry: registry, logger: discardLogger(), providerFactory: fakeProviderFactory(textResponse("should never be reached"))}

	if err := st.UpsertAgent(ctx, "minimal", minimalYAML); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}
	if err := st.CreateRun(ctx, "run-cancel", "minimal", string(runtime.StateReadyForModel)); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := st.AppendMessage(ctx, "run-cancel", message.Text(message.RoleUser, "go")); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	eng, run, err := srv.buildEngineForRun(ctx, "run-cancel")
	if err != nil {
		t.Fatalf("buildEngineForRun: %v", err)
	}

	cancelledCtx, cancel := context.WithCancel(ctx)
	cancel()

	if _, err := eng.Run(cancelledCtx, run.ID); err == nil {
		t.Fatal("expected eng.Run to return an error for a cancelled ctx")
	}

	got, err := st.GetRun(ctx, "run-cancel")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.State != string(runtime.StateCancelled) {
		t.Fatalf("persisted state = %s, want cancelled", got.State)
	}
}

// TestCancelStopsAwaitingApprovalRun exercises POST /v1/runs/{id}/cancel
// through real HTTP, on a run genuinely stuck at a stop point (paused for
// approval) rather than one already terminal by the time the request
// lands — handleRunAgent drives synchronously to the next stop point, so
// a run with no pending approval would already be completed before there
// was anything to cancel.
func TestCancelStopsAwaitingApprovalRun(t *testing.T) {
	requireNpx(t)
	ts := newTestServer(t, fakeProviderFactory(
		toolUseResponse("call_1", "everything.echo", `{"message":"gated"}`),
	))
	postAgent(t, ts, approvalsDemoYAML)

	resp, err := http.Post(ts.URL+"/v1/agents/approvals-demo/run", "application/json", strings.NewReader(`{"message":"please echo"}`))
	if err != nil {
		t.Fatalf("POST run: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var runOut runResponse
	if err := json.Unmarshal(body, &runOut); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	if runOut.State != "awaiting_approval" {
		t.Fatalf("precondition: state = %q, want awaiting_approval", runOut.State)
	}

	cResp, err := http.Post(ts.URL+"/v1/runs/"+runOut.RunID+"/cancel", "application/json", nil)
	if err != nil {
		t.Fatalf("POST cancel: %v", err)
	}
	defer cResp.Body.Close()
	cBody, _ := io.ReadAll(cResp.Body)
	if cResp.StatusCode != http.StatusOK {
		t.Fatalf("POST cancel: status %d, want 200: %s", cResp.StatusCode, cBody)
	}
	var cancelOut map[string]string
	if err := json.Unmarshal(cBody, &cancelOut); err != nil {
		t.Fatalf("decode cancel response: %v (body=%s)", err, cBody)
	}
	if cancelOut["state"] != "cancelled" {
		t.Fatalf("state = %q, want cancelled", cancelOut["state"])
	}

	// GET /runs/{id} confirms it actually persisted, not just what the
	// handler echoed back.
	gResp, err := http.Get(ts.URL + "/v1/runs/" + runOut.RunID)
	if err != nil {
		t.Fatalf("GET run: %v", err)
	}
	defer gResp.Body.Close()
	gBody, _ := io.ReadAll(gResp.Body)
	var getOut runResponse
	if err := json.Unmarshal(gBody, &getOut); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, gBody)
	}
	if getOut.State != "cancelled" {
		t.Fatalf("persisted state = %q, want cancelled", getOut.State)
	}
}

// TestCancelAlreadyCompletedReturnsConflict proves cancelling a run that
// already finished is rejected (409, matching the status
// TestApproveUnknownCallReturnsConflict gets for the analogous "nothing
// left to decide" case) rather than silently succeeding and hiding the
// run's real outcome.
func TestCancelAlreadyCompletedReturnsConflict(t *testing.T) {
	ts := newTestServer(t, fakeProviderFactory(textResponse("hello back")))
	postAgent(t, ts, minimalYAML)

	resp, err := http.Post(ts.URL+"/v1/agents/minimal/run", "application/json", strings.NewReader(`{"message":"hi"}`))
	if err != nil {
		t.Fatalf("POST run: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var runOut runResponse
	if err := json.Unmarshal(body, &runOut); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	if runOut.State != "completed" {
		t.Fatalf("precondition: state = %q, want completed", runOut.State)
	}

	cResp, err := http.Post(ts.URL+"/v1/runs/"+runOut.RunID+"/cancel", "application/json", nil)
	if err != nil {
		t.Fatalf("POST cancel: %v", err)
	}
	defer cResp.Body.Close()
	if cResp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", cResp.StatusCode)
	}
}

// TestCancelUnknownRunReturns404 checks the not-found path specifically
// because handleCancel builds its Engine directly (runtime.NewEngine)
// instead of going through buildEngineForRun — it's worth confirming that
// shortcut didn't also skip buildEngineForRun's usual 404 mapping.
func TestCancelUnknownRunReturns404(t *testing.T) {
	ts := newTestServer(t, fakeProviderFactory())
	resp, err := http.Post(ts.URL+"/v1/runs/does-not-exist/cancel", "application/json", nil)
	if err != nil {
		t.Fatalf("POST cancel: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestHealthz(t *testing.T) {
	ts := newTestServer(t, fakeProviderFactory())
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}
