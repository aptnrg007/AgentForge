package eval

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"agentforge/internal/agent"
	"agentforge/internal/config"
	"agentforge/internal/mcp"
	"agentforge/internal/message"
	"agentforge/internal/provider"
	"agentforge/internal/provider/replay"
	"agentforge/internal/runtime"
	"agentforge/internal/schema"
	"agentforge/internal/store"
)

// Mode selects where a Case's model responses come from.
type Mode string

const (
	// ModeReplay drives every case against a recorded fixture — no live
	// model, no API key, deterministic. The default, and the only mode
	// CI runs.
	ModeReplay Mode = "replay"
	// ModeLive drives every case against a real model via
	// agent.DefaultProviderFactory (or opts.ProviderFactory).
	ModeLive Mode = "live"
)

// RunOptions configures RunSuite.
type RunOptions struct {
	Mode Mode
	// Root resolves each case's default fixture path
	// (testdata/fixtures/<suite>/<case>.json); "" means the process's cwd.
	Root string
	// Models is only consulted in ModeLive: one full pass of the suite
	// per entry, each overriding the agent config's model. An entry is
	// either a bare model name ("qwen3:14b", keeping the config's
	// provider) or "provider:name" ("anthropic:claude-...", overriding
	// both). Empty means one pass using the agent config's own model.
	Models []string
	// Record, only meaningful with ModeLive, saves a fixture for every
	// case after it runs — Save's target is the same FixturePath a
	// ModeReplay run of the same suite would load.
	Record bool
	// ProviderFactory overrides agent.DefaultProviderFactory in ModeLive.
	// Tests inject a fake here; nil means the real thing.
	ProviderFactory agent.ProviderFactory
	// Logger is used for the MCP registry a live case's agent may need;
	// nil means slog.Default().
	Logger *slog.Logger
}

// CaseResult is one Case's outcome.
type CaseResult struct {
	Case Case
	// RunErr is a hard failure — the run itself couldn't be driven to a
	// stop point (build failed, store error, missing fixture) — distinct
	// from a failed assertion, which lands in Failures instead.
	RunErr   error
	RunID    string
	State    string
	Failures []string
}

// Passed reports whether c had no hard error and no failed assertion.
func (c CaseResult) Passed() bool { return c.RunErr == nil && len(c.Failures) == 0 }

// ModelRun is every Case's result for one model pass over a Suite.
type ModelRun struct {
	// Model is opts.Models' entry that produced this pass, or "" for the
	// agent config's own model (ModeReplay, or ModeLive with no --model).
	Model string
	Cases []CaseResult
}

// Passed reports whether every case in r passed.
func (r ModelRun) Passed() bool {
	for _, c := range r.Cases {
		if !c.Passed() {
			return false
		}
	}
	return true
}

// SuiteResult is RunSuite's full result: one ModelRun per opts.Models
// entry (or a single unnamed one outside ModeLive/--model).
type SuiteResult struct {
	Suite string
	Runs  []ModelRun
}

// Passed reports whether every run passed.
func (r SuiteResult) Passed() bool {
	for _, run := range r.Runs {
		if !run.Passed() {
			return false
		}
	}
	return true
}

// RunSuite drives every Case in suite once per opts.Models entry (or once,
// for ModeReplay / a Models-less ModeLive), collecting a SuiteResult.
// Each case gets its own SQLite store — a fresh temp file, removed when
// RunSuite returns — so cases (and model passes) never share run history.
func RunSuite(ctx context.Context, suite *Suite, opts RunOptions) (*SuiteResult, error) {
	cfg, err := config.Load(suite.AgentPath())
	if err != nil {
		return nil, fmt.Errorf("eval: %s: load agent %s: %w", suite.Path, suite.Agent, err)
	}

	models := opts.Models
	if len(models) == 0 {
		models = []string{""}
	}

	result := &SuiteResult{Suite: suite.Path}
	for _, model := range models {
		modelCfg := *cfg
		if model != "" {
			if provName, name, ok := strings.Cut(model, ":"); ok {
				modelCfg.Model.Provider = provName
				modelCfg.Model.Name = name
			} else {
				modelCfg.Model.Name = model
			}
		}

		run := ModelRun{Model: model}
		for _, c := range suite.Cases {
			run.Cases = append(run.Cases, runCase(ctx, suite, &modelCfg, c, opts))
		}
		result.Runs = append(result.Runs, run)
	}
	return result, nil
}

func runCase(ctx context.Context, suite *Suite, cfg *config.Config, c Case, opts RunOptions) CaseResult {
	res := CaseResult{Case: c}

	dbFile, err := os.CreateTemp("", "agentforge-eval-*.db")
	if err != nil {
		res.RunErr = fmt.Errorf("create temp store: %w", err)
		return res
	}
	dbPath := dbFile.Name()
	_ = dbFile.Close()
	defer func() { _ = os.Remove(dbPath) }()

	st, err := store.Open(dbPath)
	if err != nil {
		res.RunErr = fmt.Errorf("open store: %w", err)
		return res
	}
	defer st.Close()

	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	registry := mcp.NewRegistry(logger)
	defer registry.Close()

	pf, getRecorder, err := providerFactory(suite, cfg, c, opts)
	if err != nil {
		res.RunErr = err
		return res
	}

	eng, err := agent.Build(ctx, st, registry, cfg, pf)
	if err != nil {
		res.RunErr = fmt.Errorf("build agent: %w", err)
		return res
	}
	// agent.Build already called pf (it needs the provider to build the
	// engine), so getRecorder — which just reads back what pf assigned —
	// is safe to call now.
	recorder := getRecorder()

	runID := "eval_" + sanitizeFilename(c.Name)
	if err := eng.NewRun(ctx, runID, c.Input); err != nil {
		res.RunErr = fmt.Errorf("start run: %w", err)
		return res
	}
	res.RunID = runID

	// A generous, fixed step ceiling independent of the agent's own
	// max_turns: each turn can take more than one Step call (model,
	// then tools), so bounding on Step calls (not turns) is what
	// actually prevents this loop running forever if a case's Expect is
	// simply wrong about how the run ends.
	const maxSteps = 100
	var state runtime.State
	for i := 0; i < maxSteps; i++ {
		state, err = eng.Step(ctx, runID)
		if err != nil {
			res.RunErr = fmt.Errorf("step: %w", err)
			return res
		}
		if state == runtime.StateCompleted || state == runtime.StateFailed ||
			state == runtime.StateCancelled || state == runtime.StateAwaitingApproval {
			break
		}
	}
	res.State = string(state)

	if opts.Record && recorder != nil {
		if err := recorder.Save(suite.FixturePath(c, opts.Root)); err != nil {
			res.RunErr = fmt.Errorf("save fixture: %w", err)
			return res
		}
	}

	run, err := st.GetRun(ctx, runID)
	if err != nil {
		res.RunErr = fmt.Errorf("get run: %w", err)
		return res
	}
	msgs, err := st.ListMessages(ctx, runID)
	if err != nil {
		res.RunErr = fmt.Errorf("list messages: %w", err)
		return res
	}
	calls, err := st.ListToolCalls(ctx, runID)
	if err != nil {
		res.RunErr = fmt.Errorf("list tool calls: %w", err)
		return res
	}

	res.Failures = checkExpect(suite, c.Expect, run, msgs, calls)
	return res
}

// providerFactory returns the agent.ProviderFactory runCase should build
// the engine with, and — only meaningful in ModeLive with opts.Record —
// a getRecorder func that returns the *replay.Recorder the factory built,
// once the factory has actually run. agent.Build calls the factory
// synchronously before returning, so it's always safe for runCase to call
// getRecorder right after agent.Build returns.
func providerFactory(suite *Suite, cfg *config.Config, c Case, opts RunOptions) (agent.ProviderFactory, func() *replay.Recorder, error) {
	noRecorder := func() *replay.Recorder { return nil }

	switch opts.Mode {
	case ModeLive:
		base := opts.ProviderFactory
		if base == nil {
			base = agent.DefaultProviderFactory
		}
		if !opts.Record {
			return base, noRecorder, nil
		}
		var rec *replay.Recorder
		pf := func(model config.ModelConfig) (provider.Provider, error) {
			p, err := base(model)
			if err != nil {
				return nil, err
			}
			rec = replay.NewRecorder(p)
			return rec, nil
		}
		return pf, func() *replay.Recorder { return rec }, nil
	default: // ModeReplay
		path := suite.FixturePath(c, opts.Root)
		fixtureProvider, err := replay.Load(path, providerCapabilities(cfg))
		if err != nil {
			return nil, nil, fmt.Errorf("case %q: %w", c.Name, err)
		}
		return func(config.ModelConfig) (provider.Provider, error) {
			return fixtureProvider, nil
		}, noRecorder, nil
	}
}

// providerCapabilities builds a real (unconnected) provider just to read
// its Capabilities() — replay.Provider needs the same
// Capabilities().StructuredOutput value the live provider would have
// reported, since agent.Build's compileOutputPolicy reads it to decide
// whether output.schema is enforced natively or via retry-on-invalid.
func providerCapabilities(cfg *config.Config) provider.Capabilities {
	p, err := agent.DefaultProviderFactory(cfg.Model)
	if err != nil {
		return provider.Capabilities{}
	}
	return p.Capabilities()
}

func checkExpect(suite *Suite, expect Expect, run *store.Run, msgs []message.Message, calls []store.ToolCall) []string {
	var failures []string

	if expect.FinalState != "" && run.State != expect.FinalState {
		failures = append(failures, fmt.Sprintf("final_state: got %q, want %q", run.State, expect.FinalState))
	}

	called := make(map[string]bool, len(calls))
	for _, tc := range calls {
		called[tc.ToolName] = true
	}
	for _, want := range expect.ToolCalled {
		if !called[want] {
			failures = append(failures, fmt.Sprintf("tool_called: %q was never called", want))
		}
	}
	for _, unwanted := range expect.ToolNotCalled {
		if called[unwanted] {
			failures = append(failures, fmt.Sprintf("tool_not_called: %q was called", unwanted))
		}
	}

	if expect.MaxTurns > 0 && run.TurnCount > expect.MaxTurns {
		failures = append(failures, fmt.Sprintf("max_turns: run took %d turn(s), want <= %d", run.TurnCount, expect.MaxTurns))
	}

	if expect.NoRepairs && run.RepairCount != 0 {
		failures = append(failures, fmt.Sprintf("no_repairs: run needed %d repair(s)", run.RepairCount))
	}

	if len(expect.OutputContains) > 0 || expect.OutputMatchesSchema != "" {
		output := finalAssistantText(msgs)
		for _, want := range expect.OutputContains {
			if !strings.Contains(output, want) {
				failures = append(failures, fmt.Sprintf("output_contains: output does not contain %q", want))
			}
		}
		if expect.OutputMatchesSchema != "" {
			if err := checkSchema(suite, expect.OutputMatchesSchema, output); err != nil {
				failures = append(failures, fmt.Sprintf("output_matches_schema: %s", err))
			}
		}
	}

	return failures
}

func checkSchema(suite *Suite, schemaPath, output string) error {
	path := schemaPath
	if !filepath.IsAbs(path) {
		path = filepath.Join(suite.SourceDir, path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read schema %s: %w", schemaPath, err)
	}
	validator, err := schema.Compile(raw)
	if err != nil {
		return fmt.Errorf("compile schema %s: %w", schemaPath, err)
	}
	extracted, err := schema.ExtractJSON(output)
	if err != nil {
		return fmt.Errorf("extract JSON from output: %w", err)
	}
	if errs := validator.Validate(extracted); len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// finalAssistantText concatenates every text block of the last assistant
// message in msgs — the run's final answer.
func finalAssistantText(msgs []message.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != message.RoleAssistant {
			continue
		}
		var b strings.Builder
		for _, block := range msgs[i].Content {
			if block.Type == message.BlockText {
				b.WriteString(block.Text)
			}
		}
		return b.String()
	}
	return ""
}
