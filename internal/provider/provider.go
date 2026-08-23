// Package provider defines the LLM completion interface shared by all
// backends (Ollama, Anthropic, OpenAI). Requests and responses use the
// internal message representation; each provider translates to/from its
// own wire format.
package provider

import (
	"context"
	"encoding/json"
	"strings"

	"agentforge/internal/message"
)

// ToolDef describes a tool available to the model, in namespaced form
// (e.g. "github.search").
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// toWireToolName/fromWireToolName round-trip our namespaced tool names
// ("github.search") through a provider function-name charset that
// forbids dots — OpenAI's is ^[a-zA-Z0-9_-]{1,64}$, Anthropic's is
// ^[a-zA-Z0-9_-]{1,128}$; both reject "." outright, so both providers
// use this same translation. Getting the outbound half right and the
// inbound half wrong means every tool call comes back "unknown tool" —
// see TestOpenAIToolNameRoundTrips / TestAnthropicToolNameRoundTrips.
// Gemini accepts dotted names natively (TestGeminiToolNamesStayDotted)
// and Ollama has no name restriction at all, so neither uses this.
func toWireToolName(name string) string   { return strings.ReplaceAll(name, ".", "__") }
func fromWireToolName(name string) string { return strings.ReplaceAll(name, "__", ".") }

type Request struct {
	Model       string
	System      string
	Messages    []message.Message
	Tools       []ToolDef
	MaxTokens   int
	Temperature float64
	// ResponseSchema, when non-empty, asks the provider to natively
	// constrain its output to this JSON Schema. Only meaningful for a
	// provider whose Capabilities().StructuredOutput is true; others
	// ignore it, and the caller falls back to validating the response
	// text itself and retrying.
	ResponseSchema json.RawMessage
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
