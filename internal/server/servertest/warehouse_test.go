package servertest

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/pubsub/memory"
	"github.com/thasami/backyard/internal/server"
	whservice "github.com/thasami/backyard/internal/warehouse/service"
)

// TestWarehouseEndToEnd runs warehouse queries through the HTTP API, the
// warehouse service and a real PostgreSQL source.
func TestWarehouseEndToEnd(t *testing.T) {
	dsn := os.Getenv("BACKYARD_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("BACKYARD_TEST_POSTGRES_DSN not set")
	}
	docs := openSQLite(t)
	in := newInstance(t, t.TempDir(), docs, memory.New(), func(cfg *config.Config, d *server.Deps) {
		cfg.Warehouse = config.WarehouseConfig{
			Sources: map[string]config.WarehouseSource{
				"default": {Type: "postgres", DSN: dsn},
				// Nothing listens on port 1: every query fails to connect,
				// with the user and database names in the driver's error.
				"down": {Type: "postgres", DSN: "postgres://leaky_user:leaky_pw@127.0.0.1:1/leaky_db?sslmode=disable&connect_timeout=2"},
			},
			DefaultSource: "default",
			AllowRawSQL:   true,
			MaxRows:       100,
			Timeout:       config.Duration(20 * time.Second),
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		wh, err := whservice.New(ctx, cfg.Warehouse)
		if err != nil {
			t.Fatalf("warehouse: %v", err)
		}
		t.Cleanup(func() { _ = wh.Close() })
		d.Warehouse = wh
	})
	deploySite(t, in, "wh", map[string]string{"index.html": "wh"})
	query := func(body string) (int, map[string]any) {
		t.Helper()
		resp, b := in.do("POST", "/s/wh/__backyard/api/warehouse/query", strings.NewReader(body),
			map[string]string{"X-Backyard": "1", "Content-Type": "application/json", "Origin": in.url})
		var out map[string]any
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.UseNumber()
		if err := dec.Decode(&out); err != nil {
			t.Fatalf("decode %s: %v", b, err)
		}
		return resp.StatusCode, out
	}

	// Integers beyond 2^53 survive the round trip exactly (as parameters
	// and in results).
	status, out := query(`{"sql":"SELECT CAST(? AS bigint) AS big, CAST(? AS bigint)::text AS big_text","params":[9007199254740993,-9007199254740995]}`)
	if status != 200 {
		t.Fatalf("status %d: %v", status, out)
	}
	rows, _ := out["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("rows = %v", out)
	}
	row := rows[0].(map[string]any)
	if row["big"] != json.Number("9007199254740993") || row["big_text"] != "-9007199254740995" {
		t.Fatalf("row = %v", row)
	}

	// A source that cannot be reached: 502 with a generic message naming
	// the request id, never the connection details.
	status, out = query(`{"sql":"SELECT 1","source":"down"}`)
	errObj, _ := out["error"].(map[string]any)
	msg, _ := errObj["message"].(string)
	if status != 502 || errObj["code"] != "upstream_error" || !strings.Contains(msg, "request id") {
		t.Fatalf("down source: %d %v", status, out)
	}
	for _, leak := range []string{"leaky_user", "leaky_pw", "leaky_db", "127.0.0.1"} {
		if strings.Contains(msg, leak) {
			t.Fatalf("message %q leaks %q", msg, leak)
		}
	}

	// Query errors caused by the caller keep their message.
	status, out = query(`{"sql":"SELECT no_such_column FROM (SELECT 1) AS t"}`)
	errObj, _ = out["error"].(map[string]any)
	if status != 400 || !strings.Contains(errObj["message"].(string), "no_such_column") {
		t.Fatalf("bad column: %d %v", status, out)
	}
}
