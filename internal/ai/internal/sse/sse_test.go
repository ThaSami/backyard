package sse

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReader(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []Event
	}{
		{"empty", "", nil},
		{"single", "data: {\"a\":1}\n\n", []Event{{Data: `{"a":1}`}}},
		{"no space", "data:x\n\n", []Event{{Data: "x"}}},
		{"crlf", "data: x\r\n\r\ndata: y\r\n\r\n", []Event{{Data: "x"}, {Data: "y"}}},
		{"multi-line", "data: a\ndata: b\n\n", []Event{{Data: "a\nb"}}},
		{"event name", "event: message_start\ndata: {}\n\n", []Event{{Name: "message_start", Data: "{}"}}},
		{"comments and keepalive", ": ping\n\ndata: x\n\n", []Event{{Data: "x"}}},
		{"no trailing blank line", "data: x\n\ndata: [DONE]", []Event{{Data: "x"}, {Data: "[DONE]"}}},
		{"ignores unknown fields", "id: 1\nretry: 5\ndata: x\n\n", []Event{{Data: "x"}}},
		{"event without data is dropped", "event: ping\n\ndata: x\n\n", []Event{{Data: "x"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewReader(strings.NewReader(tt.in))
			var got []Event
			for {
				ev, err := r.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				got = append(got, ev)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d events %+v, want %d %+v", len(got), got, len(tt.want), tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("event %d = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestReaderLineTooLong(t *testing.T) {
	in := "data: " + strings.Repeat("x", MaxLineBytes+10) + "\n\n"
	_, err := NewReader(strings.NewReader(in)).Next()
	if err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("want error for long line, got %v", err)
	}
}
