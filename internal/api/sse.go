package api

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// sseWriter frames values as Server-Sent Events. Constructing one commits
// the response (200, text/event-stream) — callers must not write to the
// underlying http.ResponseWriter themselves afterward.
type sseWriter struct {
	w   http.ResponseWriter
	fl  http.Flusher
	err error
}

// newSSEWriter commits an SSE response on w. It fails only if w doesn't
// support flushing, which never happens with net/http's server but is
// part of the http.ResponseWriter contract to check.
func newSSEWriter(w http.ResponseWriter) (*sseWriter, error) {
	fl, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("api: response writer does not support flushing")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	fl.Flush()
	return &sseWriter{w: w, fl: fl}, nil
}

// send writes one SSE frame: "event: <event>\ndata: <json>\n\n". It uses
// json.Marshal rather than json.Encoder.Encode — Encode appends a
// trailing newline that would corrupt the data: line's framing (a blank
// line ends the frame). Once send has recorded an error, further calls
// are no-ops.
func (s *sseWriter) send(event string, payload any) {
	if s.err != nil {
		return
	}
	data, err := json.Marshal(payload)
	if err != nil {
		s.err = err
		return
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, data); err != nil {
		s.err = err
		return
	}
	s.fl.Flush()
}

// Err reports the first error send encountered, if any (e.g. the client
// disconnected mid-stream and the write failed).
func (s *sseWriter) Err() error { return s.err }
