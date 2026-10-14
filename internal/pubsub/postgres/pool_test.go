package postgres

import (
	"context"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestPoolSize(t *testing.T) {
	tests := []struct {
		requested, cpus, want int
	}{
		{0, 1, 2},
		{0, 2, 2},
		{0, 5, 2},
		{0, 6, 3},
		{0, 8, 4},
		{0, 128, 4},
		{1, 128, 1},
		{10, 1, 10},
		{-3, 8, 4},
	}
	for _, tt := range tests {
		if got := poolSize(tt.requested, tt.cpus); got != tt.want {
			t.Errorf("poolSize(%d, %d) = %d, want %d", tt.requested, tt.cpus, got, tt.want)
		}
	}
}

func TestPoolConfig(t *testing.T) {
	const dsn = "postgres://u:p@127.0.0.1:5432/db"
	tests := []struct {
		name        string
		opts        Options
		maxConnsSet bool
		wantMax     int32
		wantApp     string
	}{
		{"derived", Options{DSN: dsn}, false, int32(poolSize(0, runtime.NumCPU())), applicationName},
		{"option", Options{DSN: dsn, MaxPoolConns: 7}, false, 7, applicationName},
		{"dsn wins", Options{DSN: dsn + "?pool_max_conns=9", MaxPoolConns: 7}, true, 9, applicationName},
		{"keyword dsn", Options{DSN: "host=127.0.0.1 user=u dbname=db application_name=mine"}, false,
			int32(poolSize(0, runtime.NumCPU())), "mine"},
		{"min conns clamped", Options{DSN: dsn + "?pool_min_conns=10&pool_min_idle_conns=10", MaxPoolConns: 3}, false, 3, applicationName},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := poolConfig(tt.opts, tt.maxConnsSet)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.MaxConns != tt.wantMax {
				t.Errorf("MaxConns = %d, want %d", cfg.MaxConns, tt.wantMax)
			}
			if cfg.MinConns > cfg.MaxConns || cfg.MinIdleConns > cfg.MaxConns {
				t.Errorf("MinConns %d / MinIdleConns %d exceed MaxConns %d", cfg.MinConns, cfg.MinIdleConns, cfg.MaxConns)
			}
			if got := cfg.ConnConfig.RuntimeParams["application_name"]; got != tt.wantApp {
				t.Errorf("application_name = %q, want %q", got, tt.wantApp)
			}
		})
	}
	if _, err := poolConfig(Options{DSN: "postgres://u:hunter2@127.0.0.1/db?pool_max_conns=x"}, true); err == nil ||
		strings.Contains(err.Error(), "hunter2") {
		t.Errorf("invalid pool_max_conns: err = %v", err)
	}
}

// TestNewPoolBounds checks the pool a Broker opens, and that pgxpool's
// pool_* DSN parameters (common when the DSN is shared with the document
// store) are not sent to the server on the LISTEN connection.
func TestNewPoolBounds(t *testing.T) {
	base := os.Getenv("BACKYARD_TEST_POSTGRES_DSN")
	if base == "" {
		t.Skip("BACKYARD_TEST_POSTGRES_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	_ = conn.Close(ctx)
	if !strings.Contains(base, "://") {
		t.Skip("test needs a URL-form DSN")
	}
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}

	tests := []struct {
		name    string
		dsn     string
		wantMax int32
	}{
		{"derived", base, int32(poolSize(0, runtime.NumCPU()))},
		{"dsn pool params", base + sep + "pool_max_conns=3&pool_max_conn_idle_time=1m", 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A canceled startup context must not stop the broker.
			sctx, scancel := context.WithTimeout(ctx, 15*time.Second)
			b, err := New(sctx, Options{DSN: tt.dsn, Channel: "backyard_pool_test"})
			scancel()
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			defer b.Close()
			if got := b.pool.Config().MaxConns; got != tt.wantMax {
				t.Errorf("MaxConns = %d, want %d", got, tt.wantMax)
			}
			for k := range b.listenCfg.RuntimeParams {
				if strings.HasPrefix(k, "pool_") {
					t.Errorf("listener runtime params include %q", k)
				}
			}
			got := make(chan string, 1)
			unsub, err := b.Subscribe(ctx, "pool", func(p []byte) { got <- string(p) })
			if err != nil {
				t.Fatal(err)
			}
			defer unsub()
			if err := b.Publish(ctx, "pool", []byte("hi")); err != nil {
				t.Fatal(err)
			}
			select {
			case p := <-got:
				if p != "hi" {
					t.Fatalf("got %q", p)
				}
			case <-ctx.Done():
				t.Fatal("message not delivered")
			}
		})
	}
}
