// Package provider defines the LLM completion interface shared by all
// backends (Ollama, Anthropic, OpenAI). Requests and responses use the
// internal message representation; each provider translates to/from its
// own wire format.
package provider

import (
	"context"
	"encoding/json"

	"agentforge/internal/message"
)

// ToolDef describes a tool available to the model, in namespaced form
// (e.g. "github.search").
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type Request struct {
	Model       string
	System      string
	Messages    []message.Message
	Tools       []ToolDef
	MaxTokens   int
	Temperature float64
}

type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type Response struct {
	Content    []message.ContentBlock
	StopReason string // "end_turn" | "tool_use" | "max_tokens"
	Usage      Usage
}

// Capabilities describes what a provider supports, so callers can adapt
// instead of assuming every provider behaves like Ollama (e.g. whether
// image tool results can be passed through instead of stringified).
type Capabilities struct {
	Vision           bool
	ToolUse          bool
	StructuredOutput bool
}

type Provider interface {
	Name() string
	Complete(ctx context.Context, r Request) (*Response, error)
	// Stream behaves like Complete but returns the response incrementally.
	// A provider that can't stream natively can satisfy this with
	// NewResponseStream(resp) after computing the full response.
	Stream(ctx context.Context, r Request) (Stream, error)
	Capabilities() Capabilities
}
