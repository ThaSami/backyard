// Package app wires configuration into running components and serves them.
package app

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/thasami/backyard/internal/config"
)

// Options tune App construction.
type Options struct {
	Logger *slog.Logger
	// DevSites maps site names to local directories served live (backyard dev).
	DevSites map[string]string
	// GatewayOnly serves only the deploy gateway (backyard gateway): no sites,
	// no site APIs, bearer-only auth, no database.
	GatewayOnly bool
}

// App is a running Backyard instance.
type App struct {
	cfg  *config.Config
	opts Options
}

// New builds every component from cfg.
// STUB: replaced during integration.
func New(ctx context.Context, cfg *config.Config, opts Options) (*App, error) {
	panic("app: not implemented")
}

// Handler returns the root handler.
func (a *App) Handler() http.Handler { panic("app: not implemented") }

// Run listens on cfg.Server.Listen (and telemetry.metrics_listen) until ctx is
// cancelled, then shuts down gracefully.
func (a *App) Run(ctx context.Context) error { panic("app: not implemented") }

// Close releases all resources.
func (a *App) Close() error { panic("app: not implemented") }
