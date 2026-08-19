package provider

import "agentforge/internal/message"

// Stream is a pull-based iterator over an in-progress model response. The
// caller drives it:
//
//	for stream.Next() {
//	    fmt.Print(stream.Delta())
//	}
//	resp, err := stream.Response()
//
// Next returns false once the stream has ended, either because the
// response is complete or because an error occurred — Err distinguishes
// the two.
type Stream interface {
	// Next advances to the next chunk and reports whether one was
	// produced. It returns false at the end of the stream or on error.
	Next() bool
	// Delta is the text produced by the chunk Next just advanced to. It
	// is empty for chunks that don't carry text (e.g. one that only
	// carries part of a tool call).
	Delta() string
	// Response returns the fully assembled response. Only meaningful
	// once Next has returned false with a nil Err.
	Response() (*Response, error)
	// Err reports the error that ended the stream, if any.
	Err() error
	// Close releases any resources held by the stream (e.g. an HTTP
	// response body). Safe to call multiple times and after the stream
	// has already ended.
	Close() error
}

// staticStream adapts an already-complete Response to the Stream
// interface, for providers (and test doubles) that don't implement true
// incremental streaming.
type staticStream struct {
	resp   *Response
	text   string
	served bool
}

// NewResponseStream returns a Stream that yields resp's assembled text as
// a single delta and then completes with resp as the final Response. This
// is the adapter used by test doubles, and by any provider that only
// implements Complete.
func NewResponseStream(resp *Response) Stream {
	var text string
	for _, b := range resp.Content {
		if b.Type == message.BlockText {
			text += b.Text
		}
	}
	return &staticStream{resp: resp, text: text}
}

func (s *staticStream) Next() bool {
	if s.served {
		return false
	}
	s.served = true
	return true
}

func (s *staticStream) Delta() string {
	if !s.served {
		return ""
	}
	return s.text
}

func (s *staticStream) Response() (*Response, error) { return s.resp, nil }
func (s *staticStream) Err() error                   { return nil }
func (s *staticStream) Close() error                 { return nil }
