// Package provider defines the LLM completion interface shared by all
// backends (Ollama, Anthropic, OpenAI). Requests and responses use the
// internal message representation; each provider translates to/from its
// own wire format.
package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"agentforge/internal/message"
)

// responseHeaderTimeout bounds how long a provider's HTTP client waits
// for the *first* byte of a response — every provider used to construct
// its client as http.DefaultClient, which has no timeout at all, so a
// provider that accepted the connection and then never answered (a
// misbehaving endpoint, a firewall silently dropping the response) hung
// the run forever with no config knob to bound it, the same gap
// limits.timeout exists to close at the run level. Deliberately NOT
// http.Client.Timeout, which bounds the *entire* request including
// reading the body: that would also cap how long a legitimately slow but
// actively-producing streaming response (or a large local model's slow
// non-streaming generation) is allowed to run, which is a real and
// common case here, not an edge case — Ollama alone can take well over a
// minute just to load a large model into memory before the first token.
// A stalled-after-it-starts response is instead bounded by the run-level
// deadline (limits.timeout, internal/runtime), which wraps ctx for the
// call and aborts it via context cancellation like any other deadline.
const responseHeaderTimeout = 2 * time.Minute

// newHTTPClient returns the default *http.Client every provider
// constructs unless a caller overrides the exported Client field
// afterward (e.g. in tests, against an httptest.Server). A fresh
// *http.Transport per client, not http.DefaultTransport, so this
// timeout is scoped to LLM provider calls specifically and can't be
// changed out from under this package by anything else in the process
// that happens to also mutate http.DefaultTransport.
func newHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = responseHeaderTimeout
	return &http.Client{Transport: transport}
}

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
	// NumCtx sets Ollama's context-window size. Only meaningful for the
	// ollama provider — today the only one with a request-time context-length
	// knob at all; every other provider ignores this field, same as
	// ResponseSchema does for a provider without native structured output.
	NumCtx int
	// Think controls Ollama's top-level "think" request field for
	// hybrid-reasoning models. nil leaves the model/Ollama default in
	// place; only meaningful for the ollama provider, same as NumCtx.
	Think *bool
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
