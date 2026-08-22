// Package tools turns config.ToolDefinition entries — a name, description
// and input_schema backed by either an HTTP request or an exec'd command
// — into runtime.Tool values, the same shape internal/mcp.Registry
// produces from an MCP server. This is the narrow, in-process alternative
// to writing an MCP server: no subprocess, no extra dependency, one
// request or one command.
package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"text/template"
)

// renderAll parses and executes tmpl against data, in the same
// missingkey=error dialect config.validateTemplate parses (but does not
// execute) at load time — a template the model's input doesn't fully
// satisfy is a tool-call error, not a silently empty substitution.
func render(tmpl string, data map[string]any) (string, error) {
	if tmpl == "" {
		return "", nil
	}
	t, err := template.New("tool").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		// Unreachable in practice: config.validateTemplate already
		// parsed this exact string at load time.
		return "", fmt.Errorf("template: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("template: %w", err)
	}
	return buf.String(), nil
}

// renderMap renders every value of m against data, returning a fresh map
// (m itself, sourced from config, is never mutated).
func renderMap(m map[string]string, data map[string]any) (map[string]string, error) {
	if len(m) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		rv, err := render(v, data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		out[k] = rv
	}
	return out, nil
}

// pathEscaped returns a copy of data with every string value run through
// url.PathEscape, for rendering the URL template — so
// {{.city}} == "New York" lands as "New%20York" in the path instead of
// breaking it into two segments. query: values are rendered against the
// raw (unescaped) data instead and go through url.Values.Encode(), which
// is why query: exists as its own key rather than being written directly
// into url:.
func pathEscaped(data map[string]any) map[string]any {
	out := make(map[string]any, len(data))
	for k, v := range data {
		if s, ok := v.(string); ok {
			out[k] = url.PathEscape(s)
			continue
		}
		out[k] = v
	}
	return out
}

// decodeInput unmarshals a tool call's raw JSON input into a
// map[string]any for use as template data. A non-object input (or
// invalid JSON) is a tool-call error, not a panic in the template
// engine.
func decodeInput(input json.RawMessage) (map[string]any, error) {
	if len(input) == 0 {
		return map[string]any{}, nil
	}
	var data map[string]any
	if err := json.Unmarshal(input, &data); err != nil {
		return nil, fmt.Errorf("input: not a JSON object: %w", err)
	}
	return data, nil
}
