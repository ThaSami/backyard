package pubsub

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateTopic(t *testing.T) {
	tests := []struct {
		name  string
		topic string
		ok    bool
	}{
		{"simple", "rt.site.db", true},
		{"room", "rt.my-site.room.lobby:1", true},
		{"unicode", "rt.site.room.café", true},
		{"max length", strings.Repeat("a", MaxTopicBytes), true},
		{"empty", "", false},
		{"too long", strings.Repeat("a", MaxTopicBytes+1), false},
		{"invalid utf8", "rt.\xff", false},
		{"newline", "rt.a\nb", false},
		{"nul", "rt.a\x00b", false},
		{"del", "rt.a\x7fb", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateTopic(tt.topic)
			if tt.ok && err != nil {
				t.Fatalf("ValidateTopic: %v", err)
			}
			if !tt.ok && !errors.Is(err, ErrInvalidTopic) {
				t.Fatalf("ValidateTopic = %v, want ErrInvalidTopic", err)
			}
		})
	}
}

func TestValidatePublish(t *testing.T) {
	if err := ValidatePublish("t", make([]byte, MaxPayloadBytes)); err != nil {
		t.Fatalf("max payload rejected: %v", err)
	}
	if err := ValidatePublish("t", nil); err != nil {
		t.Fatalf("empty payload rejected: %v", err)
	}
	if err := ValidatePublish("t", make([]byte, MaxPayloadBytes+1)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized payload: %v, want ErrTooLarge", err)
	}
	if err := ValidatePublish("", nil); !errors.Is(err, ErrInvalidTopic) {
		t.Fatalf("empty topic: %v, want ErrInvalidTopic", err)
	}
}
