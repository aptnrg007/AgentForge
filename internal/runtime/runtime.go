// Package runtime implements the agent run state machine described in
// PLAN.md section 5. Every call to Step loads run state from SQLite,
// performs exactly one transition, and writes it back — a run survives
// process restart because nothing lives only in memory.
package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

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
	Execute     ToolExecutor
}

type Config struct {
	AgentName   string
	Model       string
	System      string
	MaxTurns    int
	MaxTokens   int
	Temperature float64
}

type Engine struct {
	store    *store.Store
	provider provider.Provider
	tools    map[string]Tool
	cfg      Config
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
	default:
		// awaiting_approval, completed, failed, cancelled: nothing to do.
		return State(run.State), nil
	}
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

	resp, err := e.provider.Complete(ctx, provider.Request{
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

	// All tool calls are well-formed. Phase 1 has no approval policy yet
	// (that's Phase 6): auto-approve everything.
	for _, tu := range toolUses {
		if err := e.store.InsertToolCall(ctx, store.ToolCall{
			ID:       tu.ID,
			RunID:    run.ID,
			ToolName: tu.Name,
			ArgsJSON: string(tu.Input),
			Approval: "auto",
		}); err != nil {
			return "", err
		}
	}

	if err := e.store.UpdateRun(ctx, run.ID, string(StateReadyForTools), turnCount, 0, nil); err != nil {
		return "", err
	}
	return StateReadyForTools, nil
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
	pending, err := e.store.ListPendingToolCalls(ctx, run.ID)
	if err != nil {
		return "", err
	}

	var results []message.ContentBlock
	for _, tc := range pending {
		var resultText string
		var isError bool

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

		if err := e.store.UpdateToolCallResult(ctx, tc.ID, resultText, isError); err != nil {
			return "", err
		}
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
