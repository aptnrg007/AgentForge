package provider

import (
	"bufio"
	"io"
	"strings"
)

// sseDataReader parses Server-Sent Events frames of the shape
// "data: <payload>\n\n" — the OpenAI and Gemini streaming wire format
// (bare data lines, no event: field) — shared here since the two
// providers' readers were otherwise byte-for-byte identical.
// Anthropic's stream also sends an event: line per frame and needs the
// whole frame (event + data) accumulated before returning it, so it
// keeps its own anthropicSSEReader in anthropic.go instead of this one.
type sseDataReader struct {
	scanner *bufio.Scanner
}

// newSSEDataReader wraps body in a bufio.Scanner sized for an SSE data
// line larger than the default 64KB (a large tool-call-argument or
// long-answer chunk can exceed that).
func newSSEDataReader(body io.Reader) *sseDataReader {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	return &sseDataReader{scanner: scanner}
}

// next returns the next frame's data, or false at EOF or a scan error
// (distinguish the two via err()).
func (r *sseDataReader) next() ([]byte, bool) {
	for r.scanner.Scan() {
		line := r.scanner.Text()
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			return []byte(data), true
		}
	}
	return nil, false
}

func (r *sseDataReader) err() error { return r.scanner.Err() }
