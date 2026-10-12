// Package server is Backyard's HTTP layer (SPEC §2–§6): host/path routing,
// static site serving, the site API (identity, db, files, ai, warehouse,
// websocket), the hub (UI, docs, llms.txt, site list, tokens), the embedded
// deploy gateway mount, health checks, rate limiting and metrics.
package server

import (
	"log/slog"
	"net/http"

	"github.com/thasami/backyard/internal/ai"
	"github.com/thasami/backyard/internal/auth"
	"github.com/thasami/backyard/internal/blob"
	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/docstore"
	"github.com/thasami/backyard/internal/identity"
	"github.com/thasami/backyard/internal/realtime"
	"github.com/thasami/backyard/internal/sites"
	"github.com/thasami/backyard/internal/warehouse"
)

// Deps are the components the server needs. AI, Warehouse, Realtime and
// Gateway may be nil (feature disabled → 404/unsupported responses).
type Deps struct {
	Config    *config.Config
	Logger    *slog.Logger
	Blob      blob.Store
	Docs      docstore.Store
	Sites     *sites.Registry
	Auth      auth.Authenticator
	Tokens    *auth.Tokens
	Users     *identity.UserCache
	Realtime  realtime.Hub
	AI        ai.Service
	Warehouse warehouse.Service
	// Gateway is the embedded deploy gateway, mounted at /__backyard/api/deploy
	// on the hub host with that prefix stripped. GET /__backyard/api/info is
	// served by forwarding to the gateway's /info.
	Gateway http.Handler
	// DevSites maps site names to local directories served live (backyard dev).
	DevSites map[string]string
	// Version string reported by /__backyard/api/info.
	Version string
}

// Server is the assembled HTTP application.
type Server struct {
	deps Deps
}

// New validates deps and builds the server.
// STUB: replaced by the server implementation.
func New(d Deps) (*Server, error) { panic("server: not implemented") }

// Handler returns the root http.Handler.
// STUB: replaced by the server implementation.
func (s *Server) Handler() http.Handler { panic("server: not implemented") }

// MetricsHandler returns the Prometheus handler (for telemetry.metrics_listen).
// STUB: replaced by the server implementation.
func (s *Server) MetricsHandler() http.Handler { panic("server: not implemented") }
