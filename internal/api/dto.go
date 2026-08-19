package api

import (
	"encoding/json"

	"agentforge/internal/message"
	"agentforge/internal/store"
)

type agentSummary struct {
	Name      string `json:"name"`
	YAML      string `json:"yaml"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

func toAgentSummary(a *store.Agent) agentSummary {
	return agentSummary{Name: a.Name, YAML: a.YAML, CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt}
}

type toolInfo struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type runRequest struct {
	Message string `json:"message"`
}

type pendingCall struct {
	CallID string          `json:"call_id"`
	Tool   string          `json:"tool"`
	Args   json.RawMessage `json:"args"`
}

type runResponse struct {
	RunID    string            `json:"run_id"`
	State    string            `json:"state"`
	Error    *string           `json:"error,omitempty"`
	Messages []message.Message `json:"messages,omitempty"`
	Pending  []pendingCall     `json:"pending,omitempty"`
}

type approveRequest struct {
	CallID   string `json:"call_id"`
	Decision string `json:"decision"` // "approved" | "denied"
	Reason   string `json:"reason,omitempty"`
}

// toolCallDTO mirrors store.ToolCall for the API surface. Args is typed as
// json.RawMessage because it's always validated JSON before it reaches the
// store; Result stays a plain string because tool output is arbitrary text
// and forcing it through RawMessage would emit invalid JSON whenever the
// text isn't itself valid JSON.
type toolCallDTO struct {
	ID         string          `json:"id"`
	ToolName   string          `json:"tool_name"`
	Args       json.RawMessage `json:"args"`
	Approval   string          `json:"approval"`
	DecidedBy  *string         `json:"decided_by,omitempty"`
	Reason     *string         `json:"reason,omitempty"`
	Result     *string         `json:"result,omitempty"`
	IsError    bool            `json:"is_error,omitempty"`
	CreatedAt  int64           `json:"created_at"`
	ExecutedAt *int64          `json:"executed_at,omitempty"`
}

func toToolCallDTO(tc store.ToolCall) toolCallDTO {
	return toolCallDTO{
		ID:         tc.ID,
		ToolName:   tc.ToolName,
		Args:       json.RawMessage(tc.ArgsJSON),
		Approval:   tc.Approval,
		DecidedBy:  tc.DecidedBy,
		Reason:     tc.Reason,
		Result:     tc.Result,
		IsError:    tc.IsError,
		CreatedAt:  tc.CreatedAt,
		ExecutedAt: tc.ExecutedAt,
	}
}

// --- SSE event payloads (POST /v1/agents/{name}/stream) ---
//
// These follow toolCallDTO's convention above: Args stays json.RawMessage
// (always validated JSON by the time an event is emitted), Result stays a
// plain string (arbitrary tool output, not necessarily JSON).

type tokenEvent struct {
	Text string `json:"text"`
}

type toolCallEvent struct {
	CallID string          `json:"call_id"`
	Tool   string          `json:"tool"`
	Args   json.RawMessage `json:"args"`
}

type toolResultEvent struct {
	CallID  string `json:"call_id"`
	Tool    string `json:"tool"`
	Result  string `json:"result"`
	IsError bool   `json:"is_error,omitempty"`
}

type awaitingApprovalEvent struct {
	RunID   string        `json:"run_id"`
	Pending []pendingCall `json:"pending"`
}

// doneEvent is the single terminal frame for a stream, for every outcome
// (completed, failed, cancelled) — a client reads State the same way it
// would read it off a POST /run response, rather than needing a second
// handler for a distinct error event.
type doneEvent struct {
	RunID string  `json:"run_id"`
	State string  `json:"state"`
	Error *string `json:"error,omitempty"`
}

// runSummary is the lightweight per-run shape for GET /v1/runs — a run's
// header without its messages/tool calls, which GET /v1/runs/{id} still
// carries in full via runTrace below.
type runSummary struct {
	RunID     string  `json:"run_id"`
	AgentName string  `json:"agent_name"`
	State     string  `json:"state"`
	TurnCount int     `json:"turn_count"`
	Error     *string `json:"error,omitempty"`
	CreatedAt int64   `json:"created_at"`
	UpdatedAt int64   `json:"updated_at"`
}

func toRunSummary(r store.Run) runSummary {
	return runSummary{
		RunID: r.ID, AgentName: r.AgentName, State: r.State, TurnCount: r.TurnCount,
		Error: r.Error, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

type runTrace struct {
	RunID     string            `json:"run_id"`
	AgentName string            `json:"agent_name"`
	State     string            `json:"state"`
	TurnCount int               `json:"turn_count"`
	Error     *string           `json:"error,omitempty"`
	CreatedAt int64             `json:"created_at"`
	UpdatedAt int64             `json:"updated_at"`
	Messages  []message.Message `json:"messages"`
	ToolCalls []toolCallDTO     `json:"tool_calls"`
}
