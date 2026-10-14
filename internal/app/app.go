// Package app wires configuration into running components and serves them.
//
// New builds, in order: blob storage, the site registry, the document store,
// the pub/sub broker, the people directory, the authenticator, the realtime
// hub, the AI router, the warehouse service, the embedded deploy gateway and
// finally the HTTP server. Close releases them in reverse order. Run serves
// the handler (and an optional metrics listener) until its context is
// cancelled, then shuts down gracefully.
//
// With mcp.enabled the authenticator also is the Claude connector's
// authorization server (auth.MCPOf), and the remote MCP endpoint
// (internal/mcpremote) is built on the same registry, storage, document store
// and gateway as the hub.
//
// With Options.GatewayOnly only storage, a bearer-only authenticator and the
// deploy gateway are built (`backyard gateway`); mcp.* is then ignored.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/thasami/backyard/internal/ai"
	"github.com/thasami/backyard/internal/ai/router"
	"github.com/thasami/backyard/internal/auth"
	"github.com/thasami/backyard/internal/blob"
	blobbackends "github.com/thasami/backyard/internal/blob/backends"
	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/deploy"
	"github.com/thasami/backyard/internal/docstore"
	docbackends "github.com/thasami/backyard/internal/docstore/backends"
	"github.com/thasami/backyard/internal/identity"
	"github.com/thasami/backyard/internal/mcpremote"
	"github.com/thasami/backyard/internal/mcpserver"
	"github.com/thasami/backyard/internal/pubsub"
	pubsubbackends "github.com/thasami/backyard/internal/pubsub/backends"
	"github.com/thasami/backyard/internal/realtime"
	"github.com/thasami/backyard/internal/server"
	"github.com/thasami/backyard/internal/sites"
	"github.com/thasami/backyard/internal/version"
	"github.com/thasami/backyard/internal/warehouse"
	whservice "github.com/thasami/backyard/internal/warehouse/service"
)

// Options tune App construction.
type Options struct {
	Logger *slog.Logger
	// DevSites maps site names to local directories served live (backyard dev).
	DevSites map[string]string
	// GatewayOnly serves only the deploy gateway (backyard gateway): no sites,
	// no site APIs, bearer-only auth, no database.
	GatewayOnly bool
	// Listener, if set, is used by Run instead of listening on
	// cfg.Server.Listen (tests, socket activation).
	Listener net.Listener
	// OnListen, if set, is called by Run with the bound address once the
	// server accepts connections.
	OnListen func(addr net.Addr)

	// authOptions, if set, adjusts the authenticator's options before it is
	// built (tests: the clock, the client metadata document fetcher).
	authOptions func(*auth.Options)
}

// App is a running Backyard instance.
type App struct {
	cfg  *config.Config
	opts Options
	log  *slog.Logger

	blob      blob.Store
	docs      docstore.Store
	broker    pubsub.Broker
	hub       realtime.Hub
	ai        ai.Service
	warehouse warehouse.Service
	server    *server.Server
	handler   http.Handler
	metrics   http.Handler
	mcpURL    string    // the Claude connector's endpoint ("" while it is off)
	mcpAS     *auth.MCP // its authorization server (nil while it is off)

	closeOnce sync.Once
	closeErr  error
}

// New builds every component from cfg. On error everything built so far is
// released.
func New(ctx context.Context, cfg *config.Config, opts Options) (_ *App, err error) {
	if cfg == nil {
		return nil, errors.New("app: nil config")
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	a := &App{cfg: cfg, opts: opts, log: log}
	defer func() {
		if err != nil {
			_ = a.Close()
		}
	}()

	if a.blob, err = blobbackends.Open(ctx, cfg.Storage); err != nil {
		return nil, fmt.Errorf("storage: %w", err)
	}
	registry := sites.NewRegistry(a.blob, cfg)

	if opts.GatewayOnly {
		return a, a.buildGateway(ctx, registry)
	}

	if a.docs, err = docbackends.Open(ctx, cfg.Database); err != nil {
		return nil, fmt.Errorf("database: %w", err)
	}
	if a.broker, err = pubsubbackends.Open(ctx, cfg.PubSub); err != nil {
		return nil, fmt.Errorf("pubsub: %w", err)
	}
	dir, err := identity.NewDirectory(cfg.Identity.Directory)
	if err != nil {
		return nil, fmt.Errorf("identity directory: %w", err)
	}
	// The connector's grants live in the same document store as the audit
	// records (_system), unwrapped like the server's own system writes.
	authOpts := auth.Options{Directory: dir, Logger: log, Docs: a.docs}
	if opts.authOptions != nil {
		opts.authOptions(&authOpts)
	}
	authn, err := auth.New(ctx, cfg, authOpts)
	if err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}
	mcpAS := auth.MCPOf(authn)
	if mcpAS != nil && authn.Mode() == auth.ModeNone && !cfg.Server.Dev {
		log.Warn("mcp: auth.mode none: every connector connection is an anonymous guest")
	}

	// The hub is built before the server, so frame metrics reach the server
	// through a pointer that is filled in once it exists.
	var srvRef atomic.Pointer[server.Server]
	if !cfg.Realtime.Disabled {
		instance, _ := os.Hostname()
		a.hub = realtime.New(a.broker, realtime.Options{
			Config:     cfg.Realtime,
			InstanceID: instance,
			Logger:     log,
			OnFrame: func(dir string) {
				if s := srvRef.Load(); s != nil {
					s.OnWSFrame(dir)
				}
			},
		})
	}

	if a.ai, err = router.New(ctx, cfg.AI, router.Options{Logger: log, UserIDKey: []byte(cfg.Server.Secret)}); err != nil {
		return nil, fmt.Errorf("ai: %w", err)
	}
	if a.warehouse, err = whservice.New(ctx, cfg.Warehouse); err != nil {
		return nil, fmt.Errorf("warehouse: %w", err)
	}

	docs := a.docs
	gw, err := deploy.NewGateway(deploy.GatewayOptions{
		Config:   cfg,
		Store:    a.blob,
		Registry: registry,
		Auth:     authn,
		BaseURL:  cfg.Server.PublicURL + deploy.EmbeddedPath,
		OnDeleteSite: func(ctx context.Context, site string) error {
			return docs.DeleteSite(ctx, site)
		},
		MCPURL:  mcpAS.Resource(), // "" (no mcp_url in /info) while the connector is off
		Logger:  log,
		Version: version.Version,
	})
	if err != nil {
		return nil, fmt.Errorf("deploy gateway: %w", err)
	}

	var mcpEndpoint http.Handler
	if mcpAS != nil {
		mcpEndpoint = a.newMCPEndpoint(mcpAS, registry, gw, &srvRef)
		a.mcpURL = mcpAS.Resource()
		a.mcpAS = mcpAS
	}

	a.server, err = server.New(server.Deps{
		Config:      cfg,
		Logger:      log,
		Blob:        a.blob,
		Docs:        a.docs,
		Sites:       registry,
		Auth:        authn,
		Tokens:      auth.NewTokens(cfg.Server.Secret, cfg.Auth.TokenTTL.D()),
		Users:       identity.NewUserCache(a.docs, dir),
		Realtime:    a.hub,
		AI:          a.ai,
		Warehouse:   a.warehouse,
		Gateway:     gw,
		MCP:         mcpAS,
		MCPEndpoint: mcpEndpoint,
		DevSites:    opts.DevSites,
		Version:     version.Version,
	})
	if err != nil {
		return nil, fmt.Errorf("server: %w", err)
	}
	srvRef.Store(a.server)
	a.handler = a.server.Handler()
	a.metrics = a.server.MetricsHandler()
	return a, nil
}

// newMCPEndpoint builds the remote MCP endpoint (POST /__backyard/mcp). It
// reaches the server, which is built afterwards, through srv: the per-user
// rate limit and, after every publish that reached the gateway, the static
// cache purge and deploys_total{method="mcp"}.
func (a *App) newMCPEndpoint(m *auth.MCP, registry *sites.Registry, gw *deploy.Gateway, srv *atomic.Pointer[server.Server]) http.Handler {
	cfg := a.cfg
	var publisher *deploy.Gateway
	if cfg.Deploy.EmbeddedGateway == nil || *cfg.Deploy.EmbeddedGateway {
		publisher = gw // nil: backyard_publish answers that the hub's gateway is off
	}
	return mcpremote.New(mcpremote.Options{
		Config:  cfg,
		Auth:    m,
		Sites:   registry,
		Blob:    a.blob,
		Docs:    a.docs,
		Gateway: publisher,
		DocsFor: mcpserver.DefaultDocs,
		Allow: func(key string) (bool, time.Duration) {
			return srv.Load().AllowAPI(key) // nil-safe until the server exists
		},
		OnPublish: func(site string, ok bool) {
			s := srv.Load()
			// Always purge: a failed publish may have written files.
			s.PurgeSite(site)
			result := "ok"
			if !ok {
				result = "error"
			}
			s.ObserveDeploy("mcp", result)
		},
		RequestOrigin: server.RequestOrigin,
		ClientIP:      server.ClientIP,
		Version:       version.Version,
		Logger:        a.log,
	})
}

// buildGateway assembles the standalone deploy gateway (backyard gateway).
// It answers the protocol at the root ("/info", "/sites/…") and, for
// convenience, under the embedded path as well, plus a health check.
func (a *App) buildGateway(ctx context.Context, registry *sites.Registry) error {
	authn, err := auth.NewBearer(ctx, a.cfg, auth.Options{Logger: a.log})
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	gw, err := deploy.NewGateway(deploy.GatewayOptions{
		Config:   a.cfg,
		Store:    a.blob,
		Registry: registry,
		Auth:     authn,
		BaseURL:  a.cfg.Deploy.GatewayURL, // empty: derived from each request
		Logger:   a.log,
		Version:  version.Version,
	})
	if err != nil {
		return fmt.Errorf("deploy gateway: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /__backyard/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle(deploy.EmbeddedPath+"/", http.StripPrefix(deploy.EmbeddedPath, gw))
	mux.Handle("/", gw)
	a.handler = mux
	return nil
}

// Handler returns the root handler.
func (a *App) Handler() http.Handler { return a.handler }

// MetricsHandler returns the Prometheus handler (nil in gateway-only mode).
func (a *App) MetricsHandler() http.Handler { return a.metrics }

// Run listens on cfg.Server.Listen (and telemetry.metrics_listen) until ctx is
// cancelled, then shuts down gracefully within server.shutdown_timeout.
func (a *App) Run(ctx context.Context) error {
	cfg := a.cfg
	ln := a.opts.Listener
	if ln == nil {
		var err error
		if ln, err = net.Listen("tcp", cfg.Server.Listen); err != nil {
			return fmt.Errorf("listen %s: %w", cfg.Server.Listen, err)
		}
	}
	errLog := slog.NewLogLogger(a.log.Handler(), slog.LevelWarn)
	srv := &http.Server{
		Handler:           a.handler,
		ReadHeaderTimeout: 20 * time.Second,
		ReadTimeout:       cfg.Server.ReadTimeout.D(),
		WriteTimeout:      cfg.Server.WriteTimeout.D(),
		IdleTimeout:       cfg.Server.IdleTimeout.D(),
		MaxHeaderBytes:    1 << 20,
		ErrorLog:          errLog,
	}

	errc := make(chan error, 2)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- fmt.Errorf("serve: %w", err)
		}
	}()

	var msrv *http.Server
	if cfg.Telemetry.MetricsListen != "" && a.metrics != nil {
		mln, err := net.Listen("tcp", cfg.Telemetry.MetricsListen)
		if err != nil {
			_ = srv.Close()
			return fmt.Errorf("listen metrics %s: %w", cfg.Telemetry.MetricsListen, err)
		}
		mux := http.NewServeMux()
		mux.Handle("/metrics", a.metrics)
		msrv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, ErrorLog: errLog}
		go func() {
			if err := msrv.Serve(mln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("serve metrics: %w", err)
			}
		}()
		a.log.Info("metrics listening", "addr", mln.Addr().String())
	}

	mode := "server"
	if a.opts.GatewayOnly {
		mode = "gateway"
	}
	attrs := []any{
		"mode", mode, "addr", ln.Addr().String(), "public_url", cfg.Server.PublicURL,
		"url_mode", cfg.Server.URLMode, "auth", cfg.Auth.Mode, "storage", cfg.Storage.Type,
		"database", cfg.Database.Type, "pubsub", cfg.PubSub.Type, "version", version.Version,
	}
	if a.mcpURL != "" {
		attrs = append(attrs, "mcp_url", a.mcpURL)
	}
	a.log.Info("backyard listening", attrs...)
	if a.opts.OnListen != nil {
		a.opts.OnListen(ln.Addr())
	}

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errc:
	}

	a.log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout.D())
	defer cancel()
	// Websockets are hijacked and not tracked by Shutdown: close the hub
	// first so clients get a going-away frame and presence leaves are
	// gossiped while the broker is still open.
	if a.hub != nil {
		if err := a.hub.Close(); err != nil {
			a.log.Warn("shutdown: realtime hub", "error", err)
		}
	}
	if err := srv.Shutdown(sctx); err != nil {
		a.log.Warn("shutdown: http server", "error", err)
	}
	if msrv != nil {
		_ = msrv.Shutdown(sctx)
	}
	if a.server != nil {
		if err := a.server.Close(sctx); err != nil {
			a.log.Warn("shutdown: background work", "error", err)
		}
	}
	return runErr
}

// Close releases all resources (idempotent). It is safe to call after Run.
func (a *App) Close() error {
	a.closeOnce.Do(func() {
		var errs []error
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if a.server != nil {
			if err := a.server.Close(ctx); err != nil {
				errs = append(errs, fmt.Errorf("server: %w", err))
			}
		}
		if a.mcpAS != nil { // background grant pruning still uses the document store
			if err := a.mcpAS.Close(ctx); err != nil {
				errs = append(errs, fmt.Errorf("mcp: %w", err))
			}
		}
		if a.hub != nil {
			if err := a.hub.Close(); err != nil {
				errs = append(errs, fmt.Errorf("realtime: %w", err))
			}
		}
		if a.warehouse != nil {
			if err := a.warehouse.Close(); err != nil {
				errs = append(errs, fmt.Errorf("warehouse: %w", err))
			}
		}
		if a.broker != nil {
			if err := a.broker.Close(); err != nil {
				errs = append(errs, fmt.Errorf("pubsub: %w", err))
			}
		}
		if a.docs != nil {
			if err := a.docs.Close(); err != nil {
				errs = append(errs, fmt.Errorf("database: %w", err))
			}
		}
		if a.blob != nil {
			if err := a.blob.Close(); err != nil {
				errs = append(errs, fmt.Errorf("storage: %w", err))
			}
		}
		a.closeErr = errors.Join(errs...)
	})
	return a.closeErr
}
