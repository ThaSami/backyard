// Command backyard-lambda runs the Backyard deploy gateway (SPEC §7) as an
// AWS Lambda function behind a function URL (or an API Gateway HTTP API).
//
// Configuration is read like the server's (BACKYARD_CONFIG_YAML holds the
// YAML inline, or BACKYARD_CONFIG names a file bundled with the function).
// Storage is normally S3, so uploads go straight to presigned S3 URLs and
// the 6 MB Lambda payload limit only applies to gateway responses.
// BACKYARD_GATEWAY_URL (or deploy.gateway_url) sets the URL advertised to
// CLIs; when unset it is derived from each request's host.
//
// Build:
//
//	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -tags lambda.norpc -o bootstrap ./cmd/backyard-lambda
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/aws/aws-lambda-go/lambda"

	"github.com/thasami/backyard/internal/auth"
	"github.com/thasami/backyard/internal/blob/backends"
	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/deploy"
	"github.com/thasami/backyard/internal/sites"
	"github.com/thasami/backyard/internal/version"
)

func main() {
	if err := run(); err != nil {
		slog.Error("backyard-lambda: startup failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load("", false)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	logger := newLogger(cfg.Telemetry)
	slog.SetDefault(logger)

	// The storage and identity-provider clients built here live as long as
	// the execution environment, and some SDKs keep the constructor's
	// context for later credential refreshes, so it must never be
	// cancelled (lambda.Start below never returns). Lambda bounds the init
	// phase itself; the auth package's HTTP client carries its own timeout.
	ctx := context.Background()
	store, err := backends.Open(ctx, cfg.Storage)
	if err != nil {
		return fmt.Errorf("open storage: %w", err)
	}
	authn, err := auth.NewBearer(ctx, cfg, auth.Options{Logger: logger})
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	base := strings.TrimSpace(os.Getenv("BACKYARD_GATEWAY_URL"))
	if base == "" {
		base = cfg.Deploy.GatewayURL
	}
	gw, err := deploy.NewGateway(deploy.GatewayOptions{
		Config:   cfg,
		Store:    store,
		Registry: sites.NewRegistry(store, cfg),
		Auth:     authn,
		BaseURL:  base,
		Logger:   logger,
		Version:  version.Version,
	})
	if err != nil {
		return fmt.Errorf("gateway: %w", err)
	}
	logger.Info("backyard-lambda: ready", "version", version.Version, "storage", cfg.Storage.Type, "auth", cfg.Auth.Mode)
	lambda.Start(deploy.LambdaHandler(gw))
	return nil
}

// newLogger builds the slog logger from telemetry settings (JSON by default:
// CloudWatch Logs Insights parses it).
func newLogger(t config.TelemetryConfig) *slog.Logger {
	var level slog.Level
	switch strings.ToLower(t.LogLevel) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	if t.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, opts))
}
