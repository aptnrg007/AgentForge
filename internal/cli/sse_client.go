package cli

import (
	"bufio"
	"bytes"
	"io"
	"strings"
)

// sseEvent is one parsed "event: <name>\ndata: <json>\n\n" frame — the
// CLI-side counterpart to internal/api/sse.go's writer, decoding what
// handleStreamAgent (POST /v1/agents/{name}/stream) produces. Data holds
// the raw JSON payload; callers unmarshal it into whichever event-specific
// shape Event names (see internal/api/dto.go's SSE payload types).
type sseEvent struct {
	Event string
	Data  []byte
}

// readSSE reads "event:"/"data:" frames off r, calling fn for each
// complete one, until r is exhausted or fn asks to stop. It's a minimal
// reader matched to what newSSEWriter actually emits — one data: line per
// frame, no retry:/id:/comment lines — not a general SSE client.
func readSSE(r io.Reader, fn func(sseEvent) (stop bool)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var event string
	var data bytes.Buffer
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if event != "" {
				if fn(sseEvent{Event: event, Data: append([]byte(nil), data.Bytes()...)}) {
					return nil
				}
			}
			event = ""
			data.Reset()
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(line, "data: "))
		}
	}
	return sc.Err()
}
