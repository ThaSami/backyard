// Package sse is a small, allocation-conscious reader for the
// text/event-stream format (https://html.spec.whatwg.org/multipage/server-sent-events.html)
// as emitted by OpenAI-compatible and Gemini streaming endpoints.
package sse

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
)

// MaxLineBytes bounds a single SSE line (a JSON chunk). Providers send small
// chunks; the cap only protects against a misbehaving upstream.
const MaxLineBytes = 8 << 20

// Event is one dispatched server-sent event.
type Event struct {
	Name string // value of the "event:" field ("" when absent)
	Data string // "data:" lines joined with "\n"
}

// Reader reads events from an event stream.
type Reader struct {
	sc *bufio.Scanner
}

// NewReader returns a Reader for r.
func NewReader(r io.Reader) *Reader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), MaxLineBytes)
	return &Reader{sc: sc}
}

// Next returns the next event. It returns io.EOF when the stream ends. An
// event that is still being accumulated when the stream ends is dispatched
// (servers frequently omit the final blank line).
func (r *Reader) Next() (Event, error) {
	var (
		ev      Event
		data    bytes.Buffer
		hasData bool
	)
	for r.sc.Scan() {
		line := r.sc.Bytes()
		line = bytes.TrimSuffix(line, []byte("\r"))
		if len(line) == 0 {
			if hasData {
				ev.Data = data.String()
				return ev, nil
			}
			ev = Event{}
			continue
		}
		if line[0] == ':' { // comment / keep-alive
			continue
		}
		field, value := line, []byte(nil)
		if i := bytes.IndexByte(line, ':'); i >= 0 {
			field, value = line[:i], line[i+1:]
			value = bytes.TrimPrefix(value, []byte(" "))
		}
		switch string(field) {
		case "data":
			if hasData {
				data.WriteByte('\n')
			}
			data.Write(value)
			hasData = true
		case "event":
			ev.Name = string(value)
		}
	}
	if err := r.sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return Event{}, fmt.Errorf("sse: line exceeds %d bytes: %w", MaxLineBytes, err)
		}
		return Event{}, fmt.Errorf("sse: read: %w", err)
	}
	if hasData {
		ev.Data = data.String()
		return ev, nil
	}
	return Event{}, io.EOF
}
