package memory_test

import (
	"context"
	"errors"
	"testing"

	"github.com/thasami/backyard/internal/docstore"
	"github.com/thasami/backyard/internal/docstore/conformance"
	"github.com/thasami/backyard/internal/docstore/memory"
)

func TestConformance(t *testing.T) {
	conformance.Run(t, func(t *testing.T) docstore.Store {
		s := memory.New()
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}

func TestClosed(t *testing.T) {
	s := memory.New()
	ctx := context.Background()
	if _, err := s.Create(ctx, "site", "c", "a", map[string]any{"x": 1}, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "site", "c", "a"); !errors.Is(err, memory.ErrClosed) {
		t.Errorf("Get after Close: %v", err)
	}
	if _, err := s.Create(ctx, "site", "c", "b", nil, ""); !errors.Is(err, memory.ErrClosed) {
		t.Errorf("Create after Close: %v", err)
	}
	if err := s.Ping(ctx); !errors.Is(err, memory.ErrClosed) {
		t.Errorf("Ping after Close: %v", err)
	}
}
