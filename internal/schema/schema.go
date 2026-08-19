// Package schema wraps github.com/google/jsonschema-go for AgentForge's
// structured-output feature (agent config's output: block). It is
// deliberately the only package that imports the JSON Schema library —
// internal/runtime takes a plain func([]byte) []string instead, so
// swapping libraries or dropping the dependency never touches the engine.
package schema

import (
	"encoding/json"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
)

// Validator holds a compiled, resolved JSON Schema ready to validate
// instances against.
type Validator struct {
	resolved *jsonschema.Resolved
	raw      json.RawMessage
}

// Compile parses and resolves a JSON Schema document (draft-07 or draft
// 2020-12 — anything else is rejected, by the library, at Validate time
// with a clear "supported versions" message).
func Compile(raw []byte) (*Validator, error) {
	var s jsonschema.Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("schema: parse: %w", err)
	}
	resolved, err := s.Resolve(nil)
	if err != nil {
		return nil, fmt.Errorf("schema: resolve: %w", err)
	}
	return &Validator{resolved: resolved, raw: json.RawMessage(raw)}, nil
}

// Validate checks instance (a JSON document) against the schema. It
// returns nil on success. The underlying library reports only the first
// violation it finds, hence a single-element slice rather than one error
// — the slice return keeps the door open to a richer validator later
// without changing this signature or any caller.
func (v *Validator) Validate(instance []byte) []string {
	var value any
	if err := json.Unmarshal(instance, &value); err != nil {
		return []string{fmt.Sprintf("not valid JSON: %v", err)}
	}
	if err := v.resolved.Validate(value); err != nil {
		return []string{err.Error()}
	}
	return nil
}

// Raw returns the schema document as originally compiled, for embedding
// in a system prompt or retry feedback.
func (v *Validator) Raw() json.RawMessage {
	return v.raw
}
