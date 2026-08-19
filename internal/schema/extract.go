package schema

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ExtractJSON pulls a JSON document out of freeform model output. Models
// reliably wrap structured output in prose or a markdown code fence even
// when told not to, so this tries progressively looser strategies before
// giving up: the whole trimmed text; the whole text with a ``` fence
// stripped; then the first complete JSON value found anywhere in the
// text, ignoring any trailing prose.
func ExtractJSON(text string) ([]byte, error) {
	trimmed := strings.TrimSpace(text)

	if raw, ok := decodeWhole(trimmed); ok {
		return raw, nil
	}
	if fenced, ok := stripCodeFence(trimmed); ok {
		fenced = strings.TrimSpace(fenced)
		if raw, ok := decodeWhole(fenced); ok {
			return raw, nil
		}
		trimmed = fenced
	}
	if raw, ok := decodeFirstValue(trimmed); ok {
		return raw, nil
	}

	return nil, fmt.Errorf("response is not valid JSON")
}

func decodeWhole(s string) ([]byte, bool) {
	if !json.Valid([]byte(s)) {
		return nil, false
	}
	return []byte(s), true
}

// decodeFirstValue finds the first '{' or '[' in s and decodes exactly
// one JSON value starting there, via json.Decoder rather than manual
// brace-counting — that way a '{' or '[' character inside a string
// literal can't throw off the match.
func decodeFirstValue(s string) ([]byte, bool) {
	start := strings.IndexAny(s, "{[")
	if start < 0 {
		return nil, false
	}
	dec := json.NewDecoder(strings.NewReader(s[start:]))
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return nil, false
	}
	return []byte(raw), true
}

// stripCodeFence removes a leading ```[lang] line and a trailing ```
// fence, if present.
func stripCodeFence(s string) (string, bool) {
	if !strings.HasPrefix(s, "```") {
		return s, false
	}
	rest := s[3:]
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		rest = rest[nl+1:] // drop the opening fence's language tag, e.g. "json"
	}
	rest = strings.TrimRight(rest, "\n\t ")
	rest = strings.TrimSuffix(rest, "```")
	return rest, true
}

// Instruction returns the system-prompt appendix telling the model to
// emit only JSON conforming to raw. Without this, the model has no idea
// a schema is being enforced and a first-turn violation is close to
// guaranteed rather than merely possible.
func Instruction(raw json.RawMessage) string {
	return fmt.Sprintf(`

Your final response must be a single JSON document conforming exactly to
the following JSON Schema. Do not include any prose, explanation, or
markdown code fences in your final response — output only the JSON.

Schema:
%s`, raw)
}
