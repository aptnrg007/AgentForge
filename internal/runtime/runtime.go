// Package runtime implements the agent run state machine described in
// PLAN.md section 5. Every call to Step loads run state from SQLite,
// performs exactly one transition, and writes it back — a run survives
// process restart because nothing lives only in memory.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"agentforge/internal/message"
	"agentforge/internal/provider"
	"agentforge/internal/store"
)

type State string

const (
	StateReadyForModel    State = "ready_for_model"
	StateAwaitingApproval State = "awaiting_approval"
	StateReadyForTools    State = "ready_for_tools"
	StateCompleted        State = "completed"
	StateFailed           State = "failed"
	StateCancelled        State = "cancelled"
)

// maxRepairAttempts is how many consecutive self-correction turns are
// tolerated before a run is failed outright — both for malformed tool
// calls (stepModel's repair path) and, sharing the same run.RepairCount
// column and the same cap by default, for output-schema violations
// (stepModel's validateOutput). The two are still distinguishable in a
// run's message trace: repair appends a RoleTool message, a schema
// violation appends a RoleUser message.
const maxRepairAttempts = 2

// ToolExecutor runs a single tool call and returns its result text.
type ToolExecutor func(ctx context.Context, input json.RawMessage) (string, error)

type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
	// ReadOnlyHint mirrors MCP's tool annotation of the same name: true
	// means the tool doesn't modify its environment. It's advisory (self
	// reported by the server) and only used as a default under
	// ApprovalPolicy.Mode == "annotated".
	ReadOnlyHint bool
	// RequiresApproval forces this tool to a human decision regardless
	// of ApprovalPolicy.Mode (though ApprovalPolicy.Require/AutoApprove
	// still take precedence — see ApprovalPolicy.evaluate). Set by
	// exec-backed tool_definitions (internal/tools), where auto-running
	// under the default mode "never" would hand the model unguarded
	// process execution; the opt-out is an explicit
	// approvals.auto_approve pattern.
	RequiresApproval bool
	Execute          ToolExecutor
	// ExecuteRich, when non-nil, is preferred over Execute and can return
	// a multi-part result (text and/or images) instead of a flat string —
	// set only by MCP-backed tools (internal/mcp/registry.go) alongside
	// their existing Execute, since http:/command:-backed tools
	// (internal/tools) only ever produce text and have no reason to set
	// it. stepTools still needs a plain-text summary either way (for
	// tool_calls.result_json and Event.Result), which
	// summarizeToolResultParts computes from the rich result.
	ExecuteRich func(ctx context.Context, input json.RawMessage) ([]message.ContentBlock, error)
}

// ApprovalPolicy decides, per tool call, whether it can run immediately
// ("auto") or needs a human decision ("pending"). Require and AutoApprove
// are glob patterns against the tool's namespaced name and always take
// precedence over Mode.
type ApprovalPolicy struct {
	Mode        string // never | annotated | always
	Require     []string
	AutoApprove []string
	Timeout     time.Duration // 0 = no timeout
	OnTimeout   string        // deny | allow; empty defaults to deny
}

// evaluate returns "auto" or "pending" for a tool call. t is the tool
// being called (zero value for one the engine doesn't know about, which
// evaluate treats like any other non-annotated, non-approval-required
// tool — stepTools separately reports "no longer registered" for that
// case).
func (p ApprovalPolicy) evaluate(toolName string, t Tool) string {
	for _, pat := range p.Require {
		if globMatch(pat, toolName) {
			return "pending"
		}
	}
	for _, pat := range p.AutoApprove {
		if globMatch(pat, toolName) {
			return "auto"
		}
	}
	if t.RequiresApproval {
		return "pending"
	}
	switch p.Mode {
	case "always":
		return "pending"
	case "annotated":
		if t.ReadOnlyHint {
			return "auto"
		}
		return "pending"
	default: // "never" or unset
		return "auto"
	}
}

func globMatch(pattern, name string) bool {
	ok, err := path.Match(pattern, name)
	return err == nil && ok
}

// ToolPolicy bounds how long a single tool call may run. The zero value
// (Timeout 0, no Overrides) is unbounded — the pre-existing behavior,
// unchanged unless a config opts in via tool_policy.
type ToolPolicy struct {
	Timeout   time.Duration // 0 = unbounded default
	OnTimeout string        // "error" (default, empty) | "fail"
	Overrides []ToolTimeoutRule
}

// ToolTimeoutRule sets Timeout for any tool whose namespaced name matches
// one of Patterns.
type ToolTimeoutRule struct {
	Patterns []string
	Timeout  time.Duration
}

// timeoutFor returns the timeout for toolName: the first Overrides entry
// whose pattern matches, else the policy default, else 0 (unbounded).
// Ordered rather than a map lookup so two patterns that both match one
// tool resolve deterministically — Go map iteration order is randomized.
func (p ToolPolicy) timeoutFor(toolName string) time.Duration {
	for _, rule := range p.Overrides {
		for _, pat := range rule.Patterns {
			if globMatch(pat, toolName) {
				return rule.Timeout
			}
		}
	}
	return p.Timeout
}

// OutputPolicy turns on schema-validated structured output for a run's
// final answer. Validate is a plain func rather than an interface or a
// concrete schema type so internal/runtime never needs to import a JSON
// Schema library — agent.Build supplies the closure (JSON extraction +
// validation) built around internal/schema. A nil Validate means no
// validation at all, which is also what Engine.ClearOutputPolicy sets,
// e.g. for a chat session that shouldn't force every reply through a
// schema meant for a one-shot run's final answer.
type OutputPolicy struct {
	// Validate returns nil on success, or a non-empty list of problems
	// (e.g. "not valid JSON", or a schema violation) otherwise.
	Validate func(instance []byte) []string
	// Schema is embedded in retry feedback when !Native (with native
	// enforcement the schema is already applied server-side, so
	// re-sending it on a retry would just be wasted tokens).
	Schema json.RawMessage
	// OnInvalid is "retry" (default, empty) or "fail".
	OnInvalid string
	// MaxRetries caps consecutive schema-violation turns. <= 0 means
	// "use the engine default" (see NewEngine) — a policy is only
	// constructed at all when validation is wanted, so 0 here never
	// means "no retries"; on_invalid: fail is how a caller asks for that.
	MaxRetries int
	// Native means the provider was asked to (and can) natively enforce
	// Schema — see Engine.responseSchemaForRequest.
	Native bool
}

type Config struct {
	AgentName   string
	Model       string
	System      string
	MaxTurns    int
	MaxTokens   int
	Temperature float64
	Approvals   ApprovalPolicy
	Output      OutputPolicy
	ToolPolicy  ToolPolicy
}

type Engine struct {
	store    *store.Store
	provider provider.Provider
	tools    map[string]Tool
	cfg      Config
	onEvent  func(Event)
}

func NewEngine(st *store.Store, p provider.Provider, cfg Config) *Engine {
	if cfg.MaxTurns <= 0 {
		cfg.MaxTurns = 10
	}
	if cfg.Output.Validate != nil && cfg.Output.MaxRetries <= 0 {
		// Matches maxRepairAttempts: 2 retries = 3 total turns, which is
		// also the structured-output acceptance bar ("self-corrects by
		// turn 3").
		cfg.Output.MaxRetries = maxRepairAttempts
	}
	return &Engine{store: st, provider: p, tools: map[string]Tool{}, cfg: cfg}
}

func (e *Engine) RegisterTool(t Tool) {
	e.tools[t.Name] = t
}

// ClearOutputPolicy turns off structured-output validation on an
// already-built engine. internal/cli/chat.go calls this right after
// agent.Build: an output: schema is meant to gate a one-shot run's final
// answer, and forcing every reply in an interactive chat session to
// conform to it would make chat unusable. Not safe to call concurrently
// with Step.
func (e *Engine) ClearOutputPolicy() {
	e.cfg.Output = OutputPolicy{}
}

// OnEvent installs a callback that receives fine-grained progress events
// (token, tool_call, tool_result) as a run advances. Installing one causes
// stepModel to use the provider's Stream method instead of Complete; with
// no callback installed, behavior is unchanged from before this existed.
// Not safe to call concurrently with Step.
func (e *Engine) OnEvent(fn func(Event)) {
	e.onEvent = fn
}

func (e *Engine) emit(ev Event) {
	if e.onEvent != nil {
		e.onEvent(ev)
	}
}

func (e *Engine) toolDefs() []provider.ToolDef {
	defs := make([]provider.ToolDef, 0, len(e.tools))
	for _, t := range e.tools {
		defs = append(defs, provider.ToolDef{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
	}
	return defs
}

func (e *Engine) toolNames() []string {
	names := make([]string, 0, len(e.tools))
	for n := range e.tools {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// NewRun creates a run seeded with an initial user message and returns its ID.
func (e *Engine) NewRun(ctx context.Context, runID, userMessage string) error {
	if err := e.store.EnsureAgentExists(ctx, e.cfg.AgentName); err != nil {
		return err
	}
	if err := e.store.CreateRun(ctx, runID, e.cfg.AgentName, string(StateReadyForModel)); err != nil {
		return err
	}
	if _, err := e.store.AppendMessage(ctx, runID, message.Text(message.RoleUser, userMessage)); err != nil {
		return err
	}
	return nil
}

// ContinueRun appends a new user message to a run that has already reached
// a terminal state and puts it back to ready_for_model, so a REPL can carry
// on the same conversation instead of starting a fresh run every turn.
func (e *Engine) ContinueRun(ctx context.Context, runID, userMessage string) error {
	run, err := e.store.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	switch State(run.State) {
	case StateCompleted, StateFailed, StateCancelled:
	default:
		return fmt.Errorf("runtime: run %s is %s, not in a terminal state", runID, run.State)
	}

	if _, err := e.store.AppendMessage(ctx, runID, message.Text(message.RoleUser, userMessage)); err != nil {
		return err
	}
	return e.store.UpdateRun(ctx, runID, string(StateReadyForModel), run.TurnCount, 0, nil)
}

// Step performs exactly one state transition for the given run and persists
// the result. Callers loop until it returns a terminal state or
// StateAwaitingApproval.
func (e *Engine) Step(ctx context.Context, runID string) (State, error) {
	run, err := e.store.GetRun(ctx, runID)
	if err != nil {
		return "", err
	}

	switch State(run.State) {
	case StateReadyForModel:
		return e.stepModel(ctx, run)
	case StateReadyForTools:
		return e.stepTools(ctx, run)
	case StateAwaitingApproval:
		return e.stepAwaitingApproval(ctx, run)
	default:
		// completed, failed, cancelled: nothing to do.
		return State(run.State), nil
	}
}

// stepAwaitingApproval lazily applies the approval timeout, if one is
// configured and has elapsed. There's no background timer (ground rule:
// state is re-evaluated from persisted data, not ticked by a goroutine) —
// whoever next calls Step (or the API's /approve, /resume handlers) is the
// one that discovers the timeout and resolves it.
func (e *Engine) stepAwaitingApproval(ctx context.Context, run *store.Run) (State, error) {
	if e.cfg.Approvals.Timeout <= 0 {
		return StateAwaitingApproval, nil
	}
	if time.Since(time.UnixMilli(run.UpdatedAt)) < e.cfg.Approvals.Timeout {
		return StateAwaitingApproval, nil
	}

	decision := "denied"
	if e.cfg.Approvals.OnTimeout == "allow" {
		decision = "approved"
	}
	pending, err := e.store.ListPendingApprovals(ctx, run.ID)
	if err != nil {
		return "", err
	}
	for _, tc := range pending {
		if err := e.store.DecideToolCall(ctx, tc.ID, decision, "system:timeout", "approval timed out"); err != nil {
			return "", err
		}
	}

	state, err := e.resolveAwaitingApproval(ctx, run.ID)
	if err != nil {
		return "", err
	}
	if state != StateReadyForTools {
		return state, nil
	}
	// The timeout resolved every pending call in one shot; continue this
	// Step call into execution so callers looping on Step don't need to
	// special-case a timeout resolution.
	run, err = e.store.GetRun(ctx, run.ID)
	if err != nil {
		return "", err
	}
	return e.stepTools(ctx, run)
}

// RecordApproval records a human decision ("approved" or "denied") on a
// pending call and, once every pending call for the run has been decided,
// moves the run from awaiting_approval to ready_for_tools.
func (e *Engine) RecordApproval(ctx context.Context, runID, callID, decision, decidedBy, reason string) (State, error) {
	if decision != "approved" && decision != "denied" {
		return "", fmt.Errorf("runtime: invalid decision %q", decision)
	}
	if err := e.store.DecideToolCall(ctx, callID, decision, decidedBy, reason); err != nil {
		return "", err
	}
	return e.resolveAwaitingApproval(ctx, runID)
}

// EditPendingCallArgs overwrites a pending call's arguments in place (the
// chat REPL's "edit" action), without deciding it.
func (e *Engine) EditPendingCallArgs(ctx context.Context, callID string, args json.RawMessage) error {
	if !json.Valid(args) {
		return fmt.Errorf("runtime: edited args are not valid JSON")
	}
	return e.store.UpdateToolCallArgs(ctx, callID, string(args))
}

func (e *Engine) resolveAwaitingApproval(ctx context.Context, runID string) (State, error) {
	run, err := e.store.GetRun(ctx, runID)
	if err != nil {
		return "", err
	}
	if State(run.State) != StateAwaitingApproval {
		return State(run.State), nil
	}

	n, err := e.store.CountPendingApprovals(ctx, runID)
	if err != nil {
		return "", err
	}
	if n > 0 {
		return StateAwaitingApproval, nil
	}

	if err := e.store.UpdateRun(ctx, runID, string(StateReadyForTools), run.TurnCount, 0, nil); err != nil {
		return "", err
	}
	return StateReadyForTools, nil
}

// complete is the engine's sole path to the provider. With no event sink
// installed it's a passthrough to Complete, unchanged from before
// streaming existed. With a sink installed it drives Stream instead,
// emitting an EventToken per text delta, and returns the same assembled
// *Response either way — everything downstream of this call (repair
// validation, approval evaluation, persistence) is streaming-agnostic.
func (e *Engine) complete(ctx context.Context, req provider.Request) (*provider.Response, error) {
	if e.onEvent == nil {
		return e.provider.Complete(ctx, req)
	}

	stream, err := e.provider.Stream(ctx, req)
	if err != nil {
		return nil, err
	}
	defer stream.Close()

	for stream.Next() {
		if delta := stream.Delta(); delta != "" {
			e.emit(Event{Kind: EventToken, Text: delta})
		}
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	return stream.Response()
}

func (e *Engine) stepModel(ctx context.Context, run *store.Run) (State, error) {
	if run.TurnCount >= e.cfg.MaxTurns {
		errStr := fmt.Sprintf("max turns (%d) exceeded", e.cfg.MaxTurns)
		if err := e.store.UpdateRun(ctx, run.ID, string(StateFailed), run.TurnCount, run.RepairCount, &errStr); err != nil {
			return "", err
		}
		return StateFailed, nil
	}

	msgs, err := e.store.ListMessages(ctx, run.ID)
	if err != nil {
		return "", err
	}

	resp, err := e.complete(ctx, provider.Request{
		Model:          e.cfg.Model,
		System:         e.cfg.System,
		Messages:       msgs,
		Tools:          e.toolDefs(),
		MaxTokens:      e.cfg.MaxTokens,
		Temperature:    e.cfg.Temperature,
		ResponseSchema: e.responseSchemaForRequest(),
	})
	turnCount := run.TurnCount + 1
	if err != nil {
		errStr := fmt.Sprintf("provider error: %v", err)
		if uerr := e.store.UpdateRun(ctx, run.ID, string(StateFailed), turnCount, run.RepairCount, &errStr); uerr != nil {
			return "", uerr
		}
		return StateFailed, nil
	}

	if _, err := e.store.AppendMessage(ctx, run.ID, message.Message{Role: message.RoleAssistant, Content: resp.Content}); err != nil {
		return "", err
	}

	var toolUses []message.ContentBlock
	for _, b := range resp.Content {
		if b.Type == message.BlockToolUse {
			toolUses = append(toolUses, b)
		}
	}

	if len(toolUses) == 0 {
		if e.cfg.Output.Validate != nil {
			if state, done, err := e.validateOutput(ctx, run, resp.Content, turnCount); done {
				return state, err
			}
		}
		if err := e.store.UpdateRun(ctx, run.ID, string(StateCompleted), turnCount, 0, nil); err != nil {
			return "", err
		}
		return StateCompleted, nil
	}

	problems := make([]string, len(toolUses))
	anyProblem := false
	for i, tu := range toolUses {
		if p := e.validateToolUse(tu); p != "" {
			problems[i] = p
			anyProblem = true
		}
	}

	if anyProblem {
		results := make([]message.ContentBlock, len(toolUses))
		for i, tu := range toolUses {
			content := problems[i]
			if content == "" {
				content = "not executed: a sibling tool call in this turn was malformed; please retry"
			}
			results[i] = message.ContentBlock{Type: message.BlockToolResult, ToolUseID: tu.ID, Content: content, IsError: true}
		}
		if _, err := e.store.AppendMessage(ctx, run.ID, message.Message{Role: message.RoleTool, Content: results}); err != nil {
			return "", err
		}

		repairCount := run.RepairCount + 1
		if repairCount > maxRepairAttempts {
			errStr := "tool call repair failed after repeated malformed calls"
			if err := e.store.UpdateRun(ctx, run.ID, string(StateFailed), turnCount, repairCount, &errStr); err != nil {
				return "", err
			}
			return StateFailed, nil
		}
		if err := e.store.UpdateRun(ctx, run.ID, string(StateReadyForModel), turnCount, repairCount, nil); err != nil {
			return "", err
		}
		return StateReadyForModel, nil
	}

	// All tool calls are well-formed: evaluate the approval policy for
	// each and persist its decision (or lack of one).
	allAuto := true
	for _, tu := range toolUses {
		approval := e.cfg.Approvals.evaluate(tu.Name, e.tools[tu.Name])
		if approval != "auto" {
			allAuto = false
		}
		e.emit(Event{Kind: EventToolCall, CallID: tu.ID, ToolName: tu.Name, Args: tu.Input})
		if err := e.store.InsertToolCall(ctx, store.ToolCall{
			ID:       tu.ID,
			RunID:    run.ID,
			ToolName: tu.Name,
			ArgsJSON: string(tu.Input),
			Approval: approval,
		}); err != nil {
			return "", err
		}
	}

	nextState := StateReadyForTools
	if !allAuto {
		nextState = StateAwaitingApproval
	}
	if err := e.store.UpdateRun(ctx, run.ID, string(nextState), turnCount, 0, nil); err != nil {
		return "", err
	}
	return nextState, nil
}

func (e *Engine) validateToolUse(tu message.ContentBlock) string {
	if tu.Name == "" {
		return "tool call is missing a name"
	}
	if _, ok := e.tools[tu.Name]; !ok {
		return fmt.Sprintf("unknown tool %q; available tools: %v", tu.Name, e.toolNames())
	}
	if len(tu.Input) == 0 || !json.Valid(tu.Input) {
		return fmt.Sprintf("tool %q call has malformed JSON input", tu.Name)
	}
	return ""
}

// responseSchemaForRequest returns the schema to send for native
// enforcement, or nil. Whether native enforcement is safe to combine with
// tool use varies by provider — Ollama's constrained decoding makes tool
// calls impossible on that turn, OpenAI's response_format composes with
// tools on the same request — so that decision belongs to each Provider,
// not here; the engine just forwards the schema whenever the provider has
// advertised it can enforce it at all.
func (e *Engine) responseSchemaForRequest() json.RawMessage {
	if !e.cfg.Output.Native {
		return nil
	}
	return e.cfg.Output.Schema
}

// validateOutput checks a final (no-tool-call) turn's text against
// e.cfg.Output.Validate. done reports whether it already decided (and
// persisted) the run's next state — StateFailed or StateReadyForModel —
// so stepModel's caller should return immediately; done is false only
// when validation passed, in which case the caller proceeds to its own
// StateCompleted transition exactly as it did before validation existed.
// This mirrors the tool-call repair block that follows it in stepModel as
// closely as possible: same RepairCount reuse, same "append feedback and
// loop back to ready_for_model, capped, else fail" shape.
func (e *Engine) validateOutput(ctx context.Context, run *store.Run, content []message.ContentBlock, turnCount int) (state State, done bool, err error) {
	var text strings.Builder
	for _, b := range content {
		if b.Type == message.BlockText {
			text.WriteString(b.Text)
		}
	}

	problems := e.cfg.Output.Validate([]byte(text.String()))
	if len(problems) == 0 {
		return "", false, nil
	}

	if e.cfg.Output.OnInvalid == "fail" {
		errStr := "output schema validation failed: " + strings.Join(problems, "; ")
		if uerr := e.store.UpdateRun(ctx, run.ID, string(StateFailed), turnCount, run.RepairCount, &errStr); uerr != nil {
			return "", true, uerr
		}
		return StateFailed, true, nil
	}

	retries := run.RepairCount + 1
	if retries > e.cfg.Output.MaxRetries {
		errStr := fmt.Sprintf("output schema validation failed after %d retries: %s", e.cfg.Output.MaxRetries, strings.Join(problems, "; "))
		if uerr := e.store.UpdateRun(ctx, run.ID, string(StateFailed), turnCount, retries, &errStr); uerr != nil {
			return "", true, uerr
		}
		return StateFailed, true, nil
	}

	feedback := message.Text(message.RoleUser, e.outputRetryFeedback(problems))
	if _, aerr := e.store.AppendMessage(ctx, run.ID, feedback); aerr != nil {
		return "", true, aerr
	}
	if uerr := e.store.UpdateRun(ctx, run.ID, string(StateReadyForModel), turnCount, retries, nil); uerr != nil {
		return "", true, uerr
	}
	return StateReadyForModel, true, nil
}

// outputRetryFeedback builds the RoleUser turn appended after a schema
// violation — not RoleTool/BlockToolResult: there is no tool_use_id to
// reference, and a tool_result with no matching tool_use is a hard error
// for at least Anthropic. RoleUser is what every provider accepts after
// an assistant turn. The schema itself is only re-included when
// validation isn't native — with native enforcement it's already applied
// server-side, so repeating it would just spend tokens.
func (e *Engine) outputRetryFeedback(problems []string) string {
	var b strings.Builder
	b.WriteString("Your previous response did not conform to the required JSON schema.\n\nErrors:\n")
	for _, p := range problems {
		b.WriteString("  - ")
		b.WriteString(p)
		b.WriteString("\n")
	}
	b.WriteString("\nReply with only a JSON object conforming to the schema. Do not include prose, explanations, or markdown code fences.")
	if !e.cfg.Output.Native && len(e.cfg.Output.Schema) > 0 {
		b.WriteString("\n\nSchema:\n")
		b.Write(e.cfg.Output.Schema)
	}
	return b.String()
}

func (e *Engine) stepTools(ctx context.Context, run *store.Run) (State, error) {
	unexecuted, err := e.store.ListUnexecutedToolCalls(ctx, run.ID)
	if err != nil {
		return "", err
	}

	var results []message.ContentBlock
	for _, tc := range unexecuted {
		var resultText string
		var isError bool
		var richParts []message.ContentBlock

		switch tc.Approval {
		case "denied":
			reason := "no reason given"
			if tc.Reason != nil && *tc.Reason != "" {
				reason = *tc.Reason
			}
			resultText, isError = fmt.Sprintf("tool call denied: %s", reason), true
		default: // auto, approved
			if tool, ok := e.tools[tc.ToolName]; ok {
				callCtx := ctx
				cancel := func() {}
				if d := e.cfg.ToolPolicy.timeoutFor(tc.ToolName); d > 0 {
					callCtx, cancel = context.WithTimeout(ctx, d)
				}

				var out string
				var err error
				if tool.ExecuteRich != nil {
					richParts, err = tool.ExecuteRich(callCtx, json.RawMessage(tc.ArgsJSON))
				} else {
					out, err = tool.Execute(callCtx, json.RawMessage(tc.ArgsJSON))
				}
				cancel()

				switch {
				case err != nil && errors.Is(err, context.DeadlineExceeded):
					d := e.cfg.ToolPolicy.timeoutFor(tc.ToolName)
					timeoutMsg := fmt.Sprintf("tool %q timed out after %s", tc.ToolName, d)
					if e.cfg.ToolPolicy.OnTimeout == "fail" {
						// A fail-on-timeout run ends here: record what
						// happened on the call itself (so `runs get`
						// shows the timeout), then fail the run
						// directly — no further unexecuted calls in
						// this batch are processed, and no next model
						// turn is needed.
						if uerr := e.store.UpdateToolCallResult(ctx, tc.ID, timeoutMsg, true); uerr != nil {
							return "", uerr
						}
						e.emit(Event{Kind: EventToolResult, CallID: tc.ID, ToolName: tc.ToolName, Result: timeoutMsg, IsError: true})
						if uerr := e.store.UpdateRun(ctx, run.ID, string(StateFailed), run.TurnCount, run.RepairCount, &timeoutMsg); uerr != nil {
							return "", uerr
						}
						return StateFailed, nil
					}
					resultText, isError = timeoutMsg, true
				case err != nil:
					resultText, isError = err.Error(), true
				case tool.ExecuteRich != nil:
					resultText = summarizeToolResultParts(richParts)
				default:
					resultText = out
				}
			} else {
				resultText, isError = fmt.Sprintf("tool %q is no longer registered", tc.ToolName), true
			}
		}

		if err := e.store.UpdateToolCallResult(ctx, tc.ID, resultText, isError); err != nil {
			return "", err
		}
		e.emit(Event{Kind: EventToolResult, CallID: tc.ID, ToolName: tc.ToolName, Result: resultText, IsError: isError})
		block := message.ContentBlock{
			Type:      message.BlockToolResult,
			ToolUseID: tc.ID,
			Content:   resultText,
			IsError:   isError,
		}
		if !isError && len(richParts) > 0 {
			block.ToolResultParts = richParts
		}
		results = append(results, block)
	}

	if len(results) > 0 {
		if _, err := e.store.AppendMessage(ctx, run.ID, message.Message{Role: message.RoleTool, Content: results}); err != nil {
			return "", err
		}
	}

	if err := e.store.UpdateRun(ctx, run.ID, string(StateReadyForModel), run.TurnCount, 0, nil); err != nil {
		return "", err
	}
	return StateReadyForModel, nil
}

// summarizeToolResultParts renders an ExecuteRich result down to the flat
// text every non-Anthropic consumer of a tool_result still expects
// (tool_calls.result_json, Event.Result, and — via ContentBlock.Content,
// which this feeds — the CLI transcript/TUI and every provider besides
// Anthropic). Only Anthropic's translator reaches past this into
// ToolResultParts for the images themselves.
func summarizeToolResultParts(parts []message.ContentBlock) string {
	var b strings.Builder
	imageCount := 0
	for _, p := range parts {
		if p.Type == message.BlockText {
			b.WriteString(p.Text)
		} else if p.Type == message.BlockImage {
			imageCount++
		}
	}
	if imageCount > 0 {
		fmt.Fprintf(&b, " [+%d image(s)]", imageCount)
	}
	return b.String()
}
