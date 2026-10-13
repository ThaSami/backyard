package pubsub

import (
	"errors"
	"fmt"
	"unicode/utf8"
)

// Limits shared by every Broker implementation so that behaviour is identical
// across backends (a payload accepted by the memory broker in development is
// accepted by Redis or Postgres in production).
const (
	// MaxPayloadBytes is the largest payload Publish accepts.
	MaxPayloadBytes = 1 << 20
	// MaxTopicBytes is the longest topic name Publish and Subscribe accept.
	MaxTopicBytes = 1024
)

var (
	// ErrTooLarge is returned by Publish for payloads over MaxPayloadBytes.
	ErrTooLarge = errors.New("pubsub: payload too large")
	// ErrInvalidTopic is returned for empty, oversized or non-UTF-8 topics.
	ErrInvalidTopic = errors.New("pubsub: invalid topic")
	// ErrNilHandler is returned by Subscribe when the handler is nil.
	ErrNilHandler = errors.New("pubsub: nil handler")
)

// ValidateTopic reports whether topic is acceptable to every backend: non-empty,
// at most MaxTopicBytes, valid UTF-8 and free of control characters.
func ValidateTopic(topic string) error {
	if topic == "" {
		return fmt.Errorf("%w: empty", ErrInvalidTopic)
	}
	if len(topic) > MaxTopicBytes {
		return fmt.Errorf("%w: longer than %d bytes", ErrInvalidTopic, MaxTopicBytes)
	}
	if !utf8.ValidString(topic) {
		return fmt.Errorf("%w: not valid UTF-8", ErrInvalidTopic)
	}
	for _, r := range topic {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: contains control characters", ErrInvalidTopic)
		}
	}
	return nil
}

// ValidatePublish checks topic and payload size for Publish.
func ValidatePublish(topic string, payload []byte) error {
	if err := ValidateTopic(topic); err != nil {
		return err
	}
	if len(payload) > MaxPayloadBytes {
		return fmt.Errorf("%w: %d bytes (max %d)", ErrTooLarge, len(payload), MaxPayloadBytes)
	}
	return nil
}
