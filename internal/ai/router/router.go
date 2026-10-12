// Package router maps client-facing model aliases to configured providers and
// implements ai.Service.
package router

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/thasami/backyard/internal/ai"
	"github.com/thasami/backyard/internal/config"
)

// Options are optional dependencies.
type Options struct {
	HTTPClient *http.Client // used by HTTP-based providers (tests inject fakes)
	Logger     *slog.Logger
}

// New builds providers from cfg.Providers and validates cfg.Models.
// When AI is disabled it returns a Service whose Enabled() is false and whose
// methods return ai.ErrDisabled.
// STUB: replaced by the ai implementation.
func New(ctx context.Context, cfg config.AIConfig, opts Options) (ai.Service, error) {
	panic("ai/router: not implemented")
}
