package runtime

import "encoding/json"

// EventKind identifies what an Event reports. These are the fine-grained,
// in-turn events the engine can emit; the coarse run-level events
// (awaiting_approval, done) are derived by callers (the HTTP handler) from
// the State that Step returns, since Step already reports those.
type EventKind string

const (
	// EventToken carries a chunk of assistant text as it's generated.
	EventToken EventKind = "token"
	// EventToolCall reports a tool call the model just made, before it
	// runs — whether it will run immediately or is pending approval.
	EventToolCall EventKind = "tool_call"
	// EventToolResult reports a tool call's outcome, including a denied
	// call's synthesized error result.
	EventToolResult EventKind = "tool_result"
	// EventCompaction reports that stepModel shrank or dropped part of
	// the message history to fit limits.context.max_input_tokens before
	// sending it to the model — see internal/runtime/context.go. The
	// full, untouched history stays in SQLite (ground rule 1); only the
	// outbound request shrank.
	EventCompaction EventKind = "compaction"
)

// Event is a single observation of a run's progress, delivered to an
// Engine's OnEvent callback. Only the fields relevant to Kind are set.
type Event struct {
	Kind EventKind

	Text string // EventToken

	CallID   string          // EventToolCall, EventToolResult
	ToolName string          // EventToolCall, EventToolResult
	Args     json.RawMessage // EventToolCall

	Result  string // EventToolResult
	IsError bool   // EventToolResult

	// EventCompaction — see Elision, which these mirror field-for-field.
	TokensBefore int
	TokensAfter  int
	ElidedBlocks int
	DroppedUnits int
}
