package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	"agentforge/internal/runtime"
	"agentforge/internal/store"
)

const testStorySchema = `{
	"$schema": "https://json-schema.org/draft/2020-12/schema",
	"type": "object",
	"required": ["title", "beats"],
	"properties": {
		"title": {"type": "string"},
		"beats": {"type": "array", "items": {"type": "string"}, "minItems": 1}
	}
}`

// scriptedProvider replays fixed responses, standing in for a real LLM —
// the same pattern established in internal/api, internal/cli, and
// internal/mcp's own test files.
type scriptedProvider struct {
	responses []*provider.Response
	calls     int
	caps      provider.Capabilities
	requests  []provider.Request
}

func (p *scriptedProvider) Name() string { return "scripted" }

func (p *scriptedProvider) Complete(ctx context.Context, r provider.Request) (*provider.Response, error) {
	p.requests = append(p.requests, r)
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

func (p *scriptedProvider) Capabilities() provider.Capabilities { return p.caps }

func textResp(text string) *provider.Response {
	return &provider.Response{Content: []message.ContentBlock{{Type: message.BlockText, Text: text}}, StopReason: "end_turn"}
}

func newTestStoreAndRegistry(t *testing.T) (*store.Store, *mcp.Registry) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	registry := mcp.NewRegistry(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { registry.Close() })
	return st, registry
}

func fakeFactory(p provider.Provider) ProviderFactory {
	return func(config.ModelConfig) (provider.Provider, error) { return p, nil }
}

func TestBuildFailsOnMissingSchemaFile(t *testing.T) {
	st, registry := newTestStoreAndRegistry(t)
	cfg := &config.Config{
		Name:   "a",
		Model:  config.ModelConfig{Provider: "ollama", Name: "m"},
		Output: config.OutputConfig{Schema: "./does-not-exist.json"},
	}
	_, err := Build(context.Background(), st, registry, cfg, fakeFactory(&scriptedProvider{}))
	if err == nil {
		t.Fatal("expected an error for a missing schema file")
	}
	if !strings.Contains(err.Error(), "does-not-exist.json") {
		t.Fatalf("expected the error to name the schema path, got: %v", err)
	}
}

func TestBuildFailsOnMissingSchemaFileWithoutSourceDirHint(t *testing.T) {
	st, registry := newTestStoreAndRegistry(t)
	cfg := &config.Config{
		Name:   "a",
		Model:  config.ModelConfig{Provider: "ollama", Name: "m"},
		Output: config.OutputConfig{Schema: "./relative.json"},
		// SourceDir deliberately left empty, as it would be for a config
		// reconstructed via config.Parse (e.g. the daemon or a resumed
		// run) rather than config.Load.
	}
	_, err := Build(context.Background(), st, registry, cfg, fakeFactory(&scriptedProvider{}))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "no source directory") {
		t.Fatalf("expected the error to explain the missing source directory, got: %v", err)
	}
}

func TestBuildFailsOnUnparseableSchema(t *testing.T) {
	dir := t.TempDir()
	schemaPath := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(schemaPath, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write schema: %v", err)
	}

	st, registry := newTestStoreAndRegistry(t)
	cfg := &config.Config{
		Name:   "a",
		Model:  config.ModelConfig{Provider: "ollama", Name: "m"},
		Output: config.OutputConfig{Schema: schemaPath},
	}
	_, err := Build(context.Background(), st, registry, cfg, fakeFactory(&scriptedProvider{}))
	if err == nil {
		t.Fatal("expected an error for an unparseable schema")
	}
}

func TestBuildResolvesSchemaRelativeToSourceDir(t *testing.T) {
	dir := t.TempDir()
	schemaPath := filepath.Join(dir, "story-spec.json")
	if err := os.WriteFile(schemaPath, []byte(testStorySchema), 0o644); err != nil {
		t.Fatalf("write schema: %v", err)
	}

	st, registry := newTestStoreAndRegistry(t)
	cfg := &config.Config{
		Name:      "a",
		Model:     config.ModelConfig{Provider: "ollama", Name: "m"},
		Output:    config.OutputConfig{Schema: "story-spec.json"}, // relative
		SourceDir: dir,
	}
	sp := &scriptedProvider{responses: []*provider.Response{textResp(`{"title":"x","beats":["a"]}`)}}
	eng, err := Build(context.Background(), st, registry, cfg, fakeFactory(sp))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	ctx := context.Background()
	if err := eng.NewRun(ctx, "run-1", "go"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	state, err := eng.Step(ctx, "run-1")
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if state != runtime.StateCompleted {
		t.Fatalf("expected completed (schema resolved and satisfied), got %s", state)
	}
}

// TestBuildEndToEndSelfCorrectsWithRealSchemaValidator wires the real
// internal/schema validator through Build into a working Engine — the
// one place the closure seam (runtime.OutputPolicy.Validate, built in
// compileOutputPolicy) is proven to actually connect end to end, not just
// each side tested in isolation. Also exercises ExtractJSON on a
// markdown-fenced first attempt, which internal/runtime's own tests
// deliberately don't cover (they use a hand-written Validate with no
// extraction step, to stay free of the schema-library import).
func TestBuildEndToEndSelfCorrectsWithRealSchemaValidator(t *testing.T) {
	dir := t.TempDir()
	schemaPath := filepath.Join(dir, "story-spec.json")
	if err := os.WriteFile(schemaPath, []byte(testStorySchema), 0o644); err != nil {
		t.Fatalf("write schema: %v", err)
	}

	st, registry := newTestStoreAndRegistry(t)
	cfg := &config.Config{
		Name:      "a",
		Model:     config.ModelConfig{Provider: "ollama", Name: "m"},
		Output:    config.OutputConfig{Schema: "story-spec.json", MaxRetries: 2},
		SourceDir: dir,
	}
	sp := &scriptedProvider{responses: []*provider.Response{
		textResp("```json\n{\"title\":\"x\"}\n```"), // fenced, missing "beats"
		textResp(`{"title":"x","beats":["a","b"]}`), // valid
	}}
	eng, err := Build(context.Background(), st, registry, cfg, fakeFactory(sp))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	ctx := context.Background()
	if err := eng.NewRun(ctx, "run-1", "go"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	var state runtime.State
	for i := 0; i < 10; i++ {
		state, err = eng.Step(ctx, "run-1")
		if err != nil {
			t.Fatalf("Step: %v", err)
		}
		if state == runtime.StateCompleted || state == runtime.StateFailed {
			break
		}
	}
	if state != runtime.StateCompleted {
		run, _ := st.GetRun(ctx, "run-1")
		t.Fatalf("expected completed, got %s (error=%v)", state, run.Error)
	}
	if sp.calls != 2 {
		t.Fatalf("expected the fenced-but-invalid turn to be extracted and rejected, then corrected: got %d calls", sp.calls)
	}

	msgs, err := st.ListMessages(ctx, "run-1")
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages (user, assistant, user-feedback, assistant), got %d: %+v", len(msgs), msgs)
	}
	if msgs[2].Role != message.RoleUser {
		t.Fatalf("expected the feedback turn to be RoleUser, got %s", msgs[2].Role)
	}
}

// newBlockingTool returns a tool whose Execute blocks until ctx is done,
// standing in for a hung MCP server — the same fixture internal/runtime's
// own tool_policy tests use.
func newBlockingTool(name string) runtime.Tool {
	return runtime.Tool{
		Name:        name,
		Description: "test tool that never returns on its own",
		InputSchema: json.RawMessage(`{}`),
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		},
	}
}

// TestBuildWiresToolPolicyTimeout proves the three-file wiring (config ->
// agent.Build's toolPolicy() -> runtime.Engine) actually connects: a
// tool_policy.timeout in the parsed config results in a real deadline
// applied to tool execution, not just a struct copied around untested.
func TestBuildWiresToolPolicyTimeout(t *testing.T) {
	st, registry := newTestStoreAndRegistry(t)
	cfg := &config.Config{
		Name:       "a",
		Model:      config.ModelConfig{Provider: "ollama", Name: "m"},
		ToolPolicy: config.ToolPolicyConfig{Timeout: "20ms"},
	}
	sp := &scriptedProvider{responses: []*provider.Response{
		{Content: []message.ContentBlock{{Type: message.BlockToolUse, ID: "call_1", Name: "hang.tool", Input: json.RawMessage(`{}`)}}, StopReason: "tool_use"},
		textResp("worked around it"),
	}}
	eng, err := Build(context.Background(), st, registry, cfg, fakeFactory(sp))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	eng.RegisterTool(newBlockingTool("hang.tool"))

	ctx := context.Background()
	if err := eng.NewRun(ctx, "run-1", "go"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	var state runtime.State
	for i := 0; i < 10; i++ {
		state, err = eng.Step(ctx, "run-1")
		if err != nil {
			t.Fatalf("Step: %v", err)
		}
		if state == runtime.StateCompleted || state == runtime.StateFailed {
			break
		}
	}
	if state != runtime.StateCompleted {
		run, _ := st.GetRun(ctx, "run-1")
		t.Fatalf("expected the run to survive the wired timeout and complete, got %s (error=%v)", state, run.Error)
	}

	calls, err := st.ListToolCalls(ctx, "run-1")
	if err != nil {
		t.Fatalf("ListToolCalls: %v", err)
	}
	if len(calls) != 1 || !calls[0].IsError || calls[0].Result == nil || !strings.Contains(*calls[0].Result, "timed out after 20ms") {
		t.Fatalf("expected a timeout result naming 20ms, got %+v", calls)
	}
}

// TestBuildWiresLimitsTimeout proves the config -> agent.Build ->
// runtime.Engine wiring for limits.timeout actually connects, the same
// way TestBuildWiresToolPolicyTimeout does for tool_policy.timeout — a
// real run-level deadline, not just a struct field copied around
// untested. Reuses newBlockingTool: with no tool_policy configured at
// all, only the run-level deadline is what can possibly stop it.
func TestBuildWiresLimitsTimeout(t *testing.T) {
	st, registry := newTestStoreAndRegistry(t)
	cfg := &config.Config{
		Name:   "a",
		Model:  config.ModelConfig{Provider: "ollama", Name: "m"},
		Limits: config.LimitsConfig{Timeout: "20ms"},
	}
	sp := &scriptedProvider{responses: []*provider.Response{
		{Content: []message.ContentBlock{{Type: message.BlockToolUse, ID: "call_1", Name: "hang.tool", Input: json.RawMessage(`{}`)}}, StopReason: "tool_use"},
	}}
	eng, err := Build(context.Background(), st, registry, cfg, fakeFactory(sp))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	eng.RegisterTool(newBlockingTool("hang.tool"))

	ctx := context.Background()
	if err := eng.NewRun(ctx, "run-1", "go"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	var state runtime.State
	for i := 0; i < 10; i++ {
		state, err = eng.Step(ctx, "run-1")
		if err != nil {
			t.Fatalf("Step: %v", err)
		}
		if state == runtime.StateCompleted || state == runtime.StateFailed || state == runtime.StateCancelled {
			break
		}
	}
	if state != runtime.StateFailed {
		run, _ := st.GetRun(ctx, "run-1")
		t.Fatalf("expected the wired run timeout to fail the run, got %s (error=%v)", state, run.Error)
	}

	run, err := st.GetRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Error == nil || !strings.Contains(*run.Error, "exceeded its time limit") {
		t.Fatalf("run.Error = %v, want it to mention the time limit", run.Error)
	}
}

// TestBuildWiresToolPolicyOverrideOrdering proves Build preserves the
// config's override order end to end: two overrides that both match the
// same tool must resolve to the first one, exactly as toolPolicy() and
// ToolPolicy.timeoutFor() intend.
func TestBuildWiresToolPolicyOverrideOrdering(t *testing.T) {
	st, registry := newTestStoreAndRegistry(t)
	cfg := &config.Config{
		Name:  "a",
		Model: config.ModelConfig{Provider: "ollama", Name: "m"},
		ToolPolicy: config.ToolPolicyConfig{
			Timeout: "5s",
			Overrides: []config.ToolTimeoutOverride{
				{Tools: []string{"hang.*"}, Timeout: "20ms"},
				{Tools: []string{"hang.tool"}, Timeout: "5m"},
			},
		},
	}
	sp := &scriptedProvider{responses: []*provider.Response{
		{Content: []message.ContentBlock{{Type: message.BlockToolUse, ID: "call_1", Name: "hang.tool", Input: json.RawMessage(`{}`)}}, StopReason: "tool_use"},
		textResp("worked around it"),
	}}
	eng, err := Build(context.Background(), st, registry, cfg, fakeFactory(sp))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	eng.RegisterTool(newBlockingTool("hang.tool"))

	ctx := context.Background()
	if err := eng.NewRun(ctx, "run-1", "go"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	var state runtime.State
	for i := 0; i < 10; i++ {
		state, err = eng.Step(ctx, "run-1")
		if err != nil {
			t.Fatalf("Step: %v", err)
		}
		if state == runtime.StateCompleted || state == runtime.StateFailed {
			break
		}
	}
	if state != runtime.StateCompleted {
		run, _ := st.GetRun(ctx, "run-1")
		t.Fatalf("expected completed, got %s (error=%v)", state, run.Error)
	}

	calls, err := st.ListToolCalls(ctx, "run-1")
	if err != nil {
		t.Fatalf("ListToolCalls: %v", err)
	}
	// The first-listed override (20ms) must win over the second (5m),
	// which also matches — proving Build carried override order through.
	if len(calls) != 1 || !calls[0].IsError || calls[0].Result == nil || !strings.Contains(*calls[0].Result, "timed out after 20ms") {
		t.Fatalf("expected the first matching override (20ms) to win, got %+v", calls)
	}
}

// TestBuildRegistersToolDefinitionsWithoutMCP guards against the early
// return ResolveTools used to have for len(cfg.MCP) == 0 — a config that
// defines tools directly (internal/tools) but has no mcp: servers at all
// is the common case tool_definitions exists for, and must still build
// and register a working tool.
func TestBuildRegistersToolDefinitionsWithoutMCP(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	st, registry := newTestStoreAndRegistry(t)
	cfg := &config.Config{
		Name:  "a",
		Model: config.ModelConfig{Provider: "ollama", Name: "m"},
		ToolDefinitions: []config.ToolDefinition{{
			Name:        "ping.check",
			Description: "test",
			InputSchema: json.RawMessage(`{"type":"object","required":["id"],"properties":{"id":{"type":"string"}}}`),
			HTTP:        &config.HTTPToolConfig{URL: srv.URL, Query: map[string]string{"id": "{{.id}}"}},
		}},
	}
	sp := &scriptedProvider{responses: []*provider.Response{
		{Content: []message.ContentBlock{{Type: message.BlockToolUse, ID: "call_1", Name: "ping.check", Input: json.RawMessage(`{"id":"abc"}`)}}, StopReason: "tool_use"},
		textResp("done"),
	}}
	eng, err := Build(context.Background(), st, registry, cfg, fakeFactory(sp))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	ctx := context.Background()
	if err := eng.NewRun(ctx, "run-1", "go"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	var state runtime.State
	for i := 0; i < 10; i++ {
		state, err = eng.Step(ctx, "run-1")
		if err != nil {
			t.Fatalf("Step: %v", err)
		}
		if state == runtime.StateCompleted || state == runtime.StateFailed {
			break
		}
	}
	if state != runtime.StateCompleted {
		run, _ := st.GetRun(ctx, "run-1")
		t.Fatalf("expected completed, got %s (error=%v)", state, run.Error)
	}
	if gotQuery != "id=abc" {
		t.Fatalf("query = %q, want the tool_definitions HTTP tool to have actually been called", gotQuery)
	}
}

// TestResolveToolsFiltersDefinedToolsByToolsPattern proves the tools:
// glob filter applies uniformly to config.ToolDefinitions-sourced tools,
// not just MCP-discovered ones.
func TestResolveToolsFiltersDefinedToolsByToolsPattern(t *testing.T) {
	_, registry := newTestStoreAndRegistry(t)
	schema := json.RawMessage(`{"type":"object"}`)
	cfg := &config.Config{
		Name:  "a",
		Model: config.ModelConfig{Provider: "ollama", Name: "m"},
		ToolDefinitions: []config.ToolDefinition{
			{Name: "keep.me", Description: "test", InputSchema: schema, HTTP: &config.HTTPToolConfig{URL: "https://example.com"}},
			{Name: "drop.me", Description: "test", InputSchema: schema, HTTP: &config.HTTPToolConfig{URL: "https://example.com"}},
		},
		Tools: []string{"keep.*"},
	}
	tools, err := ResolveTools(context.Background(), registry, cfg)
	if err != nil {
		t.Fatalf("ResolveTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "keep.me" {
		t.Fatalf("expected only keep.me to survive the tools: filter, got %+v", tools)
	}
}

func TestBuildSetsNativeFromProviderCapabilities(t *testing.T) {
	dir := t.TempDir()
	schemaPath := filepath.Join(dir, "story-spec.json")
	if err := os.WriteFile(schemaPath, []byte(testStorySchema), 0o644); err != nil {
		t.Fatalf("write schema: %v", err)
	}

	st, registry := newTestStoreAndRegistry(t)
	cfg := &config.Config{
		Name:      "a",
		Model:     config.ModelConfig{Provider: "ollama", Name: "m"},
		Output:    config.OutputConfig{Schema: "story-spec.json"},
		SourceDir: dir,
	}
	sp := &scriptedProvider{
		caps:      provider.Capabilities{StructuredOutput: true},
		responses: []*provider.Response{textResp(`{"title":"x","beats":["a"]}`)},
	}
	eng, err := Build(context.Background(), st, registry, cfg, fakeFactory(sp))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	ctx := context.Background()
	if err := eng.NewRun(ctx, "run-1", "go"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	if _, err := eng.Step(ctx, "run-1"); err != nil {
		t.Fatalf("Step: %v", err)
	}

	// This is the real assertion: Build read prov.Capabilities().
	// StructuredOutput, set OutputPolicy.Native accordingly, and the
	// engine's responseSchemaForRequest actually sent the compiled
	// schema on the wire — proving the three-file wiring (config ->
	// agent.Build -> runtime) connects, not just that the run completed.
	if len(sp.requests) != 1 || sp.requests[0].ResponseSchema == nil {
		t.Fatalf("expected the schema to be sent natively given StructuredOutput capability, got requests=%+v", sp.requests)
	}

	run, err := st.GetRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.State != string(runtime.StateCompleted) {
		t.Fatalf("expected completed, got %s (error=%v)", run.State, run.Error)
	}
}
