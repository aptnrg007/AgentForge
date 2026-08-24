package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// decodeResponse reads resp's body (closing it either way), maps a
// non-200 status to a provider-scoped *Error via errMsg (and, for a
// provider with no Retry-After header, retryAfterFromBody — nil for every
// provider but Gemini), and otherwise unmarshals the body into out. This
// is the tail every provider's non-streaming Complete repeats after
// getting a live *http.Response back from its own doRequest — status
// check, read, unmarshal — factored out here since all four were
// otherwise identical apart from the provider name and how each extracts
// a human-readable message from an error body. Streaming (Stream)
// doesn't use this: it needs the response body kept open for progressive
// reading, not consumed and closed here.
func decodeResponse(providerName string, resp *http.Response, errMsg func([]byte) string, retryAfterFromBody func([]byte) time.Duration, out any) error {
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%s: read response: %w", providerName, err)
	}
	if resp.StatusCode != http.StatusOK {
		return newStatusError(providerName, resp, body, errMsg(body), retryAfterFromBody)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s: decode response: %w", providerName, err)
	}
	return nil
}
