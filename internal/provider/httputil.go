package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// decodeResponse reads resp's body (closing it either way), maps a
// non-200 status to a provider-scoped error via errMsg, and otherwise
// unmarshals the body into out. This is the tail every provider's
// non-streaming Complete repeats after getting a live *http.Response
// back from its own doRequest — status check, read, unmarshal — factored
// out here since all four were otherwise identical apart from the
// provider name and how each extracts a human-readable message from an
// error body. Streaming (Stream) doesn't use this: it needs the response
// body kept open for progressive reading, not consumed and closed here.
func decodeResponse(providerName string, resp *http.Response, errMsg func([]byte) string, out any) error {
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%s: read response: %w", providerName, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: status %d: %s", providerName, resp.StatusCode, errMsg(body))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s: decode response: %w", providerName, err)
	}
	return nil
}
