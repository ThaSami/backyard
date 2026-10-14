package ai

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestUpstreamErrorRetryAfter(t *testing.T) {
	// The HTTP layer finds the retry time through an interface anywhere in
	// the error chain.
	var ra interface{ RetryAfter() time.Duration }
	err := fmt.Errorf("router: %w", &UpstreamError{Provider: "p", Status: 429, Message: "slow down", RetryDelay: 3 * time.Second})
	if !errors.As(err, &ra) || ra.RetryAfter() != 3*time.Second {
		t.Fatalf("RetryAfter via errors.As: %v", ra)
	}
	if d := (&UpstreamError{RetryDelay: -time.Second}).RetryAfter(); d != 0 {
		t.Fatalf("negative delay: got %v", d)
	}
	if d := (&UpstreamError{}).RetryAfter(); d != 0 {
		t.Fatalf("unset delay: got %v", d)
	}
	var nilErr *UpstreamError
	if d := nilErr.RetryAfter(); d != 0 {
		t.Fatalf("nil receiver: got %v", d)
	}
}
