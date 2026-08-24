package provider

import (
	"net/http"
	"testing"
)

// TestNewHTTPClientSetsResponseHeaderTimeoutNotClientTimeout locks in the
// deliberate choice documented on newHTTPClient: ResponseHeaderTimeout
// (bounds only time-to-first-byte, so a hung provider can't wedge a run
// forever) rather than Client.Timeout (which would also cap a
// legitimately long streaming response or a slow local model generation).
// Getting this backwards would silently reintroduce the exact "no
// provider has any timeout" gap this exists to close, or break real
// long-running generations — either way, quietly, with no test failure
// unless this asserts on it directly.
func TestNewHTTPClientSetsResponseHeaderTimeoutNotClientTimeout(t *testing.T) {
	c := newHTTPClient()

	if c.Timeout != 0 {
		t.Fatalf("Client.Timeout = %v, want 0 (must not cap a whole streaming response)", c.Timeout)
	}

	transport, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T, want *http.Transport", c.Transport)
	}
	if transport.ResponseHeaderTimeout <= 0 {
		t.Fatalf("ResponseHeaderTimeout = %v, want > 0", transport.ResponseHeaderTimeout)
	}
}

// TestEveryProviderConstructorUsesNewHTTPClient guards against a future
// provider (or a copy-paste of an existing one) reverting to
// http.DefaultClient, which has no timeout of any kind — the original bug.
func TestEveryProviderConstructorUsesNewHTTPClient(t *testing.T) {
	providers := map[string]*http.Client{
		"ollama":    NewOllama("").Client,
		"anthropic": NewAnthropic("k", "").Client,
		"openai":    NewOpenAI("k", "").Client,
		"gemini":    NewGemini("k", "").Client,
	}
	for name, c := range providers {
		if c == http.DefaultClient {
			t.Errorf("%s: constructor uses http.DefaultClient (no timeout at all)", name)
		}
		transport, ok := c.Transport.(*http.Transport)
		if !ok || transport.ResponseHeaderTimeout <= 0 {
			t.Errorf("%s: expected a client built by newHTTPClient (ResponseHeaderTimeout set), got Transport=%T", name, c.Transport)
		}
	}
}
