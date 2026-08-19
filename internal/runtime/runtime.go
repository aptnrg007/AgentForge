// Package runtime implements the agent run state machine described in
// PLAN.md section 5. Every call to Step loads run state from SQLite,
// performs exactly one transition, and writes it back — a run survives
// process restart because nothing lives only in memory.
package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
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

// maxRepairAttempts is how many consecutive malformed-tool-call turns are
// tolerated before a run is failed outright.
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
	Execute      ToolExecutor
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

// evaluate returns "auto" or "pending" for a tool call.
func (p ApprovalPolicy) evaluate(toolName string, readOnly bool) string {
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
	switch p.Mode {
	case "always":
		return "pending"
	case "annotated":
		if readOnly {
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

type Config struct {
	AgentName   string
	Model       string
	System      string
	MaxTurns    int
	MaxTokens   int
	Temperature float64
	Approvals   ApprovalPolicy
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
	return &Engine{store: st, provider: p, tools: map[string]Tool{}, cfg: cfg}
}

func (e *Engine) RegisterTool(t Tool) {
	e.tools[t.Name] = t
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
		Model:       e.cfg.Model,
		System:      e.cfg.System,
		Messages:    msgs,
		Tools:       e.toolDefs(),
		MaxTokens:   e.cfg.MaxTokens,
		Temperature: e.cfg.Temperature,
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
		approval := e.cfg.Approvals.evaluate(tu.Name, e.tools[tu.Name].ReadOnlyHint)
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

func (e *Engine) stepTools(ctx context.Context, run *store.Run) (State, error) {
	unexecuted, err := e.store.ListUnexecutedToolCalls(ctx, run.ID)
	if err != nil {
		return "", err
	}

	var results []message.ContentBlock
	for _, tc := range unexecuted {
		var resultText string
		var isError bool

		switch tc.Approval {
		case "denied":
			reason := "no reason given"
			if tc.Reason != nil && *tc.Reason != "" {
				reason = *tc.Reason
			}
			resultText, isError = fmt.Sprintf("tool call denied: %s", reason), true
		default: // auto, approved
			if tool, ok := e.tools[tc.ToolName]; ok {
				out, err := tool.Execute(ctx, json.RawMessage(tc.ArgsJSON))
				if err != nil {
					resultText, isError = err.Error(), true
				} else {
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
		results = append(results, message.ContentBlock{
			Type:      message.BlockToolResult,
			ToolUseID: tc.ID,
			Content:   resultText,
			IsError:   isError,
		})
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
