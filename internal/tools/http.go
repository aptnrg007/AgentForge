package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"agentforge/internal/config"
	"agentforge/internal/runtime"
	"agentforge/internal/schema"
)

// httpExecutor builds the runtime.ToolExecutor for an http:-backed
// ToolDefinition. name is only used in error messages.
func httpExecutor(name string, cfg config.HTTPToolConfig, validator *schema.Validator) runtime.ToolExecutor {
	method := cfg.Method
	if method == "" {
		method = http.MethodGet
	}
	maxBytes := cfg.MaxResponseBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}

	return func(ctx context.Context, input json.RawMessage) (string, error) {
		if err := validateInput(input, validator); err != nil {
			return "", fmt.Errorf("%s: %w", name, err)
		}
		data, err := decodeInput(input)
		if err != nil {
			return "", fmt.Errorf("%s: %w", name, err)
		}

		// The URL template renders against path-escaped data (so a
		// value like "New York" becomes a valid path segment);
		// query: values render against the raw data and are encoded
		// by url.Values.Encode() instead. Two contexts, two
		// escapings — see the comment on pathEscaped.
		rawURL, err := render(cfg.URL, pathEscaped(data))
		if err != nil {
			return "", fmt.Errorf("%s: url: %w", name, err)
		}
		u, err := url.Parse(rawURL)
		if err != nil {
			return "", fmt.Errorf("%s: url: %q: %w", name, rawURL, err)
		}
		if len(cfg.Query) > 0 {
			q := u.Query()
			rendered, err := renderMap(cfg.Query, data)
			if err != nil {
				return "", fmt.Errorf("%s: query: %w", name, err)
			}
			for k, v := range rendered {
				q.Set(k, v)
			}
			u.RawQuery = q.Encode()
		}

		body, err := render(cfg.Body, data)
		if err != nil {
			return "", fmt.Errorf("%s: body: %w", name, err)
		}

		var bodyReader io.Reader
		if body != "" {
			bodyReader = strings.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, u.String(), bodyReader)
		if err != nil {
			return "", fmt.Errorf("%s: %w", name, err)
		}

		headers, err := renderMap(cfg.Headers, data)
		if err != nil {
			return "", fmt.Errorf("%s: headers: %w", name, err)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}

		// No client-level timeout: ctx already carries the run's
		// tool_policy deadline (internal/runtime.stepTools wraps
		// every call in context.WithTimeout before invoking
		// Execute), and run cancellation propagates the same way.
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", fmt.Errorf("%s: %w", name, err)
		}
		defer resp.Body.Close()

		respBody, truncated, err := readCapped(resp.Body, maxBytes)
		if err != nil {
			return "", fmt.Errorf("%s: reading response: %w", name, err)
		}
		text := string(respBody)
		if truncated {
			text += "\n[truncated]"
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return "", fmt.Errorf("%s: %s %s: %s", name, resp.Request.Method, resp.Request.URL, httpErrorBody(resp.StatusCode, text))
		}
		return text, nil
	}
}

func httpErrorBody(status int, body string) string {
	return fmt.Sprintf("status %d: %s", status, body)
}

// readCapped reads up to max+1 bytes from r, reporting whether the
// stream had more than max bytes available. It never buffers more than
// max+1 bytes regardless of how large the underlying response is.
func readCapped(r io.Reader, max int) (data []byte, truncated bool, err error) {
	limited := io.LimitReader(r, int64(max)+1)
	data, err = io.ReadAll(limited)
	if err != nil {
		return nil, false, err
	}
	if len(data) > max {
		return data[:max], true, nil
	}
	return data, false, nil
}
