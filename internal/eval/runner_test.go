package eval

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentforge/internal/config"
	"agentforge/internal/message"
	"agentforge/internal/provider"
)

const testAgentYAML = `name: test-agent
model:
  provider: ollama
  name: test-model
instructions: "test agent"
limits:
  max_turns: 5
`

const testFixtureJSON = `{
  "turns": [
    {"response": {"Content": [{"type": "text", "text": "hello there"}], "StopReason": "end_turn"}}
  ]
}`

func newTestSuite(t *testing.T, extraExpect string) (*Suite, string) {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "agent.yaml"), testAgentYAML)
	writeFile(t, filepath.Join(dir, "fixture.json"), testFixtureJSON)
	suiteYAML := "agent: agent.yaml\ncases:\n  - name: greets\n    input: hi\n    fixture_file: fixture.json\n    expect:\n" + extraExpect
	suitePath := filepath.Join(dir, "suite.yaml")
	writeFile(t, suitePath, suiteYAML)

	suite, err := Load(suitePath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return suite, dir
}

func TestRunSuiteReplayPassesMatchingCase(t *testing.T) {
	suite, dir := newTestSuite(t, "      final_state: completed\n      output_contains: [\"hello\"]\n      no_repairs: true\n")

	result, err := RunSuite(context.Background(), suite, RunOptions{Mode: ModeReplay, Root: dir})
	if err != nil {
		t.Fatalf("RunSuite: %v", err)
	}
	if !result.Passed() {
		t.Fatalf("expected the suite to pass, got %+v", result.Runs[0].Cases[0])
	}
}

func TestRunSuiteReplayReportsAssertionFailures(t *testing.T) {
	suite, dir := newTestSuite(t, "      final_state: completed\n      output_contains: [\"goodbye\"]\n")

	result, err := RunSuite(context.Background(), suite, RunOptions{Mode: ModeReplay, Root: dir})
	if err != nil {
		t.Fatalf("RunSuite: %v", err)
	}
	if result.Passed() {
		t.Fatal("expected the suite to fail on a mismatched output_contains assertion")
	}
	c := result.Runs[0].Cases[0]
	if c.RunErr != nil {
		t.Fatalf("expected an assertion failure, not a hard error: %v", c.RunErr)
	}
	if len(c.Failures) != 1 || !strings.Contains(c.Failures[0], "output_contains") {
		t.Fatalf("Failures = %v, want one output_contains failure", c.Failures)
	}
}

func TestRunSuiteReplayMissingFixtureIsHardError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "agent.yaml"), testAgentYAML)
	writeFile(t, filepath.Join(dir, "suite.yaml"), `agent: agent.yaml
cases:
  - name: greets
    input: hi
    expect:
      final_state: completed
`)
	suite, err := Load(filepath.Join(dir, "suite.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	result, err := RunSuite(context.Background(), suite, RunOptions{Mode: ModeReplay, Root: dir})
	if err != nil {
		t.Fatalf("RunSuite: %v", err)
	}
	c := result.Runs[0].Cases[0]
	if c.RunErr == nil {
		t.Fatal("expected a hard error for a missing fixture, got none")
	}
	if c.Passed() {
		t.Fatal("a missing fixture must not report as passed")
	}
}

func TestRunSuiteToolCalledAssertions(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "agent.yaml"), testAgentYAML)
	writeFile(t, filepath.Join(dir, "fixture.json"), testFixtureJSON)
	writeFile(t, filepath.Join(dir, "suite.yaml"), `agent: agent.yaml
cases:
  - name: greets
    input: hi
    fixture_file: fixture.json
    expect:
      tool_called: ["never.called"]
      tool_not_called: ["never.called"]
`)
	suite, err := Load(filepath.Join(dir, "suite.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	result, err := RunSuite(context.Background(), suite, RunOptions{Mode: ModeReplay, Root: dir})
	if err != nil {
		t.Fatalf("RunSuite: %v", err)
	}
	c := result.Runs[0].Cases[0]
	// tool_called should fail (never called), tool_not_called should pass
	// (also never called) — exactly one failure, not two.
	if len(c.Failures) != 1 || !strings.Contains(c.Failures[0], "tool_called") {
		t.Fatalf("Failures = %v, want exactly one tool_called failure", c.Failures)
	}
}

// scriptedProvider is a minimal provider.Provider double for ModeLive
// tests — RunSuite drives it through agent.DefaultProviderFactory's
// replacement (opts.ProviderFactory), never a real network call.
type scriptedProvider struct {
	responses []*provider.Response
	calls     int
}

func (p *scriptedProvider) Name() string { return "scripted" }

func (p *scriptedProvider) Complete(ctx context.Context, r provider.Request) (*provider.Response, error) {
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

func TestRunSuiteLiveRecordThenReplayRoundTrips(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "agent.yaml"), testAgentYAML)
	writeFile(t, filepath.Join(dir, "suite.yaml"), `agent: agent.yaml
cases:
  - name: greets
    input: hi
    expect:
      final_state: completed
      output_contains: ["recorded"]
`)
	suite, err := Load(filepath.Join(dir, "suite.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	live := &scriptedProvider{responses: []*provider.Response{
		{Content: []message.ContentBlock{{Type: message.BlockText, Text: "this was recorded live"}}, StopReason: "end_turn"},
	}}
	liveFactory := func(config.ModelConfig) (provider.Provider, error) { return live, nil }

	recordResult, err := RunSuite(context.Background(), suite, RunOptions{
		Mode: ModeLive, Root: dir, Record: true, ProviderFactory: liveFactory,
	})
	if err != nil {
		t.Fatalf("RunSuite (live+record): %v", err)
	}
	if !recordResult.Passed() {
		t.Fatalf("expected the live recording pass to pass, got %+v", recordResult.Runs[0].Cases[0])
	}

	fixturePath := suite.FixturePath(suite.Cases[0], dir)
	if _, err := os.Stat(fixturePath); err != nil {
		t.Fatalf("expected a saved fixture at %s: %v", fixturePath, err)
	}

	replayResult, err := RunSuite(context.Background(), suite, RunOptions{Mode: ModeReplay, Root: dir})
	if err != nil {
		t.Fatalf("RunSuite (replay): %v", err)
	}
	if !replayResult.Passed() {
		t.Fatalf("expected the recorded fixture to replay and pass, got %+v", replayResult.Runs[0].Cases[0])
	}
}

func TestRunSuiteLiveModelOverrideRunsOncePerModel(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "agent.yaml"), testAgentYAML)
	writeFile(t, filepath.Join(dir, "suite.yaml"), `agent: agent.yaml
cases:
  - name: greets
    input: hi
    expect:
      final_state: completed
`)
	suite, err := Load(filepath.Join(dir, "suite.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	factory := func(model config.ModelConfig) (provider.Provider, error) {
		return &scriptedProvider{responses: []*provider.Response{
			{Content: []message.ContentBlock{{Type: message.BlockText, Text: "ok from " + model.Name}}, StopReason: "end_turn"},
		}}, nil
	}

	result, err := RunSuite(context.Background(), suite, RunOptions{
		Mode: ModeLive, Root: dir, ProviderFactory: factory,
		Models: []string{"model-a", "model-b"},
	})
	if err != nil {
		t.Fatalf("RunSuite: %v", err)
	}
	if len(result.Runs) != 2 {
		t.Fatalf("got %d model runs, want 2", len(result.Runs))
	}
	if result.Runs[0].Model != "model-a" || result.Runs[1].Model != "model-b" {
		t.Fatalf("model runs = %q, %q, want model-a, model-b", result.Runs[0].Model, result.Runs[1].Model)
	}
	if !result.Passed() {
		t.Fatalf("expected both model runs to pass, got %+v", result.Runs)
	}
}
