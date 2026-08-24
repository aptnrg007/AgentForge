package agentforge

import "agentforge/internal/runtime"

// Event is a fine-grained progress notification from a running turn — a
// token of assistant text, a tool call being made, or a tool call's
// result — delivered synchronously from within Run/Resume via WithEvents,
// the same mechanism `agentforge chat` and the HTTP API's streaming
// endpoint use internally.
type Event = runtime.Event

// EventKind identifies what an Event reports.
type EventKind = runtime.EventKind

const (
	EventToken      = runtime.EventToken
	EventToolCall   = runtime.EventToolCall
	EventToolResult = runtime.EventToolResult
)

type runOptions struct {
	onEvent func(Event)
}

// RunOption configures Run and Resume.
type RunOption func(*runOptions)

// WithEvents subscribes fn to every Event a Run/Resume call produces
// while it's stepping — fn is called synchronously from within that
// call, in order, before Run/Resume returns.
func WithEvents(fn func(Event)) RunOption {
	return func(o *runOptions) { o.onEvent = fn }
}
