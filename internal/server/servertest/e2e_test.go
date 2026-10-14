package servertest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"

	"github.com/thasami/backyard/internal/auth"
	"github.com/thasami/backyard/internal/blob/fs"
	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/deploy"
	"github.com/thasami/backyard/internal/docstore"
	"github.com/thasami/backyard/internal/docstore/postgres"
	"github.com/thasami/backyard/internal/docstore/sqlite"
	"github.com/thasami/backyard/internal/identity"
	"github.com/thasami/backyard/internal/pubsub"
	"github.com/thasami/backyard/internal/pubsub/memory"
	"github.com/thasami/backyard/internal/pubsub/redis"
	"github.com/thasami/backyard/internal/realtime"
	"github.com/thasami/backyard/internal/server"
	"github.com/thasami/backyard/internal/sites"
	"github.com/thasami/backyard/web"
)

func suffix() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// instance is one running server.
type instance struct {
	t      *testing.T
	url    string
	server *server.Server
	cfg    *config.Config
	client *http.Client
}

// newInstance starts a path-mode server (auth mode none, anonymous deploys)
// on blobDir and docs, fanning realtime out through broker. configure, if
// given, adjusts the (defaulted) configuration and the dependencies before
// the server is built.
func newInstance(t *testing.T, blobDir string, docs docstore.Store, broker pubsub.Broker, configure ...func(*config.Config, *server.Deps)) *instance {
	t.Helper()
	ctx := context.Background()
	var handler atomic.Pointer[http.Handler]
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		(*handler.Load()).ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)

	cfg := &config.Config{}
	cfg.Server.BaseDomain = "127.0.0.1"
	cfg.Server.PublicURL = ts.URL
	cfg.Server.URLMode = "path"
	cfg.Server.Secret = "servertest-secret-0123456789abcdef0123"
	cfg.Auth.Mode = "none"
	cfg.Deploy.AllowAnonymous = true
	cfg.Storage.Type = "fs"
	cfg.Storage.FS.Root = blobDir
	cfg.Database.Type = "memory"
	cfg.Telemetry.Metrics = true
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}

	store, err := fs.New(blobDir)
	if err != nil {
		t.Fatalf("fs: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	reg := sites.NewRegistry(store, cfg)
	authn, err := auth.New(ctx, cfg, auth.Options{})
	if err != nil {
		t.Fatalf("auth: %v", err)
	}
	var srvRef atomic.Pointer[server.Server]
	hub := realtime.New(broker, realtime.Options{Config: cfg.Realtime, OnFrame: func(dir string) {
		if s := srvRef.Load(); s != nil {
			s.OnWSFrame(dir)
		}
	}})
	t.Cleanup(func() { _ = hub.Close() })
	gw, err := deploy.NewGateway(deploy.GatewayOptions{
		Config: cfg, Store: store, Registry: reg, Auth: authn, BaseURL: ts.URL + deploy.EmbeddedPath,
		OnDeleteSite: func(ctx context.Context, site string) error { return docs.DeleteSite(ctx, site) },
	})
	if err != nil {
		t.Fatalf("gateway: %v", err)
	}
	deps := server.Deps{
		Config: cfg, Blob: store, Docs: docs, Sites: reg, Auth: authn,
		Tokens:   auth.NewTokens(cfg.Server.Secret, cfg.Auth.TokenTTL.D()),
		Users:    identity.NewUserCache(docs, nil),
		Realtime: hub, Gateway: gw, Version: "e2e",
	}
	for _, f := range configure {
		f(cfg, &deps)
	}
	s, err := server.New(deps)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	srvRef.Store(s)
	h := s.Handler()
	handler.Store(&h)
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Close(cctx)
	})
	jar, _ := cookiejar.New(nil)
	return &instance{t: t, url: ts.URL, server: s, cfg: cfg, client: &http.Client{Jar: jar, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (in *instance) do(method, path string, body io.Reader, hdr map[string]string) (*http.Response, []byte) {
	in.t.Helper()
	req, err := http.NewRequest(method, in.url+path, body)
	if err != nil {
		in.t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := in.client.Do(req)
	if err != nil {
		in.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		in.t.Fatal(err)
	}
	return resp, b
}

func (in *instance) api(method, path string, v any) (*http.Response, []byte) {
	in.t.Helper()
	var body io.Reader
	if v != nil {
		b, _ := json.Marshal(v)
		body = bytes.NewReader(b)
	}
	return in.do(method, path, body, map[string]string{"X-Backyard": "1", "Content-Type": "application/json", "Origin": in.url})
}

func expect(t *testing.T, resp *http.Response, body []byte, status int) {
	t.Helper()
	if resp.StatusCode != status {
		t.Fatalf("%s %s: status %d, want %d: %s", resp.Request.Method, resp.Request.URL, resp.StatusCode, status, body)
	}
}

// deploySite deploys files through the embedded gateway with the Go client.
func deploySite(t *testing.T, in *instance, site string, files map[string]string) {
	t.Helper()
	dir := t.TempDir()
	for p, c := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c := deploy.NewClient(in.url+deploy.EmbeddedPath, "")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := c.Deploy(ctx, site, dir, deploy.DeployOptions{}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
}

// openDocs returns the document stores to test against.
func openDocs(t *testing.T) map[string]docstore.Store {
	t.Helper()
	out := map[string]docstore.Store{"sqlite": openSQLite(t)}
	if pg := openPostgres(t); pg != nil {
		out["postgres"] = pg
	}
	return out
}

// openSQLite opens a document store in a fresh SQLite database.
func openSQLite(t *testing.T) docstore.Store {
	t.Helper()
	sq, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "backyard.db"))
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	t.Cleanup(func() { _ = sq.Close() })
	return sq
}

// openPostgres opens a store in a fresh schema, or returns nil (logged) when
// BACKYARD_TEST_POSTGRES_DSN is unset or unreachable.
func openPostgres(t *testing.T) docstore.Store {
	t.Helper()
	dsn := os.Getenv("BACKYARD_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Log("BACKYARD_TEST_POSTGRES_DSN not set; skipping PostgreSQL")
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	schema := "servertest_" + suffix()
	pg, err := postgres.Open(ctx, config.PostgresConfig{DSN: dsn, Schema: schema, MaxConns: 8})
	if err != nil {
		t.Logf("PostgreSQL unavailable (%v); skipping", err)
		return nil
	}
	t.Cleanup(func() {
		_ = pg.Close()
		cctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		conn, err := pgx.Connect(cctx, dsn)
		if err != nil {
			return
		}
		defer conn.Close(cctx)
		_, _ = conn.Exec(cctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	})
	return pg
}

func TestEndToEnd(t *testing.T) {
	for name, docs := range openDocs(t) {
		t.Run(name, func(t *testing.T) {
			broker := memory.New()
			t.Cleanup(func() { _ = broker.Close() })
			in := newInstance(t, t.TempDir(), docs, broker)
			site := "demo-" + suffix()
			deploySite(t, in, site, map[string]string{
				"index.html":      "<!doctype html><title>Demo site</title><script src=\"/__backyard/backyard.js\"></script>",
				"about.html":      "about us",
				"docs/index.html": "docs",
				"404.html":        "custom not found",
			})
			runSiteChecks(t, in, site)
		})
	}
}

func runSiteChecks(t *testing.T, in *instance, site string) {
	base := "/s/" + site

	// Static serving.
	resp, body := in.do("GET", base, nil, nil)
	expect(t, resp, body, http.StatusMovedPermanently)
	if loc := resp.Header.Get("Location"); loc != base+"/" {
		t.Fatalf("Location = %q", loc)
	}
	resp, body = in.do("GET", base+"/", nil, nil)
	expect(t, resp, body, 200)
	if !strings.Contains(string(body), "Demo site") || resp.Header.Get("ETag") == "" {
		t.Fatalf("index = %q %v", body, resp.Header)
	}
	resp, body = in.do("GET", base+"/", nil, map[string]string{"If-None-Match": resp.Header.Get("ETag")})
	expect(t, resp, body, http.StatusNotModified)
	resp, body = in.do("GET", base+"/about", nil, nil)
	expect(t, resp, body, 200)
	if string(body) != "about us" {
		t.Fatalf("about = %q", body)
	}
	resp, body = in.do("GET", base+"/nope", nil, nil)
	expect(t, resp, body, 404)
	if string(body) != "custom not found" {
		t.Fatalf("404 body = %q", body)
	}
	resp, body = in.do("GET", "/s/missing-"+suffix()+"/", nil, nil)
	expect(t, resp, body, 404)
	if !strings.Contains(string(body), "Site not found") {
		t.Fatalf("missing site body = %q", body)
	}

	// SDK preamble.
	resp, body = in.do("GET", base+"/__backyard/backyard.js", nil, nil)
	switch {
	case resp.StatusCode == http.StatusInternalServerError && sdkStub():
		t.Log("web SDK is not built into this checkout; skipping the SDK check")
	case resp.StatusCode != 200:
		t.Fatalf("sdk status %d: %s", resp.StatusCode, body)
	case !strings.HasPrefix(string(body), `globalThis.__BACKYARD__ = {"v":1,"mode":"path","site":"`+site+`"`):
		t.Fatalf("sdk preamble = %q", truncate(string(body), 200))
	}

	// Hub API: site list includes the deploy with its title.
	resp, body = in.api("GET", "/__backyard/api/sites", nil)
	expect(t, resp, body, 200)
	if !strings.Contains(string(body), `"name":"`+site+`"`) || !strings.Contains(string(body), "Demo site") {
		t.Fatalf("sites = %s", body)
	}
	resp, body = in.api("GET", base+"/__backyard/api/site", nil)
	expect(t, resp, body, 200)

	// Identity: an anonymous guest keyed by cookie.
	resp, body = in.api("GET", base+"/__backyard/api/me", nil)
	expect(t, resp, body, 200)
	var me identity.User
	_ = json.Unmarshal(body, &me)
	if !me.Anonymous || !strings.HasPrefix(me.ID, "anon:") {
		t.Fatalf("me = %+v", me)
	}

	// Websocket subscription receives database changes.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn := dialWS(ctx, t, in, base)
	defer conn.CloseNow()
	sub(ctx, t, conn, "posts")

	resp, body = in.api("POST", base+"/__backyard/api/db/posts", map[string]any{"title": "Hello", "votes": 2})
	expect(t, resp, body, http.StatusCreated)
	var created map[string]any
	_ = json.Unmarshal(body, &created)
	id, _ := created["id"].(string)
	ev := readFrame(ctx, t, conn, "db")
	if ev["op"] != "create" || ev["doc_id"] != id || ev["collection"] != "posts" {
		t.Fatalf("db event = %v", ev)
	}
	if doc, _ := ev["doc"].(map[string]any); doc["title"] != "Hello" {
		t.Fatalf("event doc = %v", ev["doc"])
	}

	resp, body = in.api("PATCH", base+"/__backyard/api/db/posts/"+id, map[string]any{"votes": nil, "done": true})
	expect(t, resp, body, 200)
	if ev := readFrame(ctx, t, conn, "db"); ev["op"] != "update" {
		t.Fatalf("update event = %v", ev)
	}
	resp, body = in.api("POST", base+"/__backyard/api/db/posts/query", map[string]any{
		"where": []any{map[string]any{"field": "done", "op": "==", "value": true}},
	})
	expect(t, resp, body, 200)
	if !strings.Contains(string(body), id) || !strings.Contains(string(body), `"has_more":false`) {
		t.Fatalf("query = %s", body)
	}
	resp, body = in.api("GET", base+"/__backyard/api/db", nil)
	expect(t, resp, body, 200)
	if strings.TrimSpace(string(body)) != `{"collections":["posts"]}` {
		t.Fatalf("collections = %s", body)
	}
	resp, body = in.api("DELETE", base+"/__backyard/api/db/posts/"+id, nil)
	expect(t, resp, body, http.StatusNoContent)
	if ev := readFrame(ctx, t, conn, "db"); ev["op"] != "delete" || ev["doc"] != nil {
		t.Fatalf("delete event = %v", ev)
	}

	// Files: multipart upload to the blob store, then ranged download.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "notes.txt")
	_, _ = io.WriteString(fw, "0123456789")
	_ = mw.Close()
	resp, body = in.do("POST", base+"/__backyard/api/files", &buf, map[string]string{"X-Backyard": "1", "Content-Type": mw.FormDataContentType()})
	expect(t, resp, body, http.StatusCreated)
	var fi struct {
		ID, URL string
		Size    int64
	}
	_ = json.Unmarshal(body, &fi)
	if fi.Size != 10 || !strings.HasPrefix(fi.URL, base+"/__backyard/files/"+fi.ID+"/") {
		t.Fatalf("file = %s", body)
	}
	resp, body = in.do("GET", fi.URL, nil, map[string]string{"Range": "bytes=3-5"})
	expect(t, resp, body, http.StatusPartialContent)
	if string(body) != "345" {
		t.Fatalf("range body = %q", body)
	}
	resp, body = in.api("GET", base+"/__backyard/api/files", nil)
	expect(t, resp, body, 200)
	if !strings.Contains(string(body), fi.ID) {
		t.Fatalf("files = %s", body)
	}
	resp, body = in.api("DELETE", base+"/__backyard/api/files/"+fi.ID, nil)
	expect(t, resp, body, http.StatusNoContent)
	resp, body = in.do("GET", fi.URL, nil, nil)
	expect(t, resp, body, 404)

	// CSRF and unknown features.
	resp, body = in.do("POST", base+"/__backyard/api/db/posts", strings.NewReader(`{}`), nil)
	expect(t, resp, body, http.StatusForbidden)
	resp, body = in.api("GET", base+"/__backyard/api/ai/models", nil)
	expect(t, resp, body, http.StatusNotImplemented)

	// Health and metrics.
	resp, body = in.do("GET", "/__backyard/readyz", nil, nil)
	expect(t, resp, body, 200)
	resp, body = in.do("GET", "/__backyard/metrics", nil, nil)
	expect(t, resp, body, 200)
	for _, want := range []string{`backyard_deploys_total{method="api",result="ok"} 1`, `backyard_ws_messages_total{dir="in"}`, "backyard_db_ops_total"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("metrics lack %q", want)
		}
	}

	// Redeploy is visible immediately (the embedded gateway purges caches).
	deploySite(t, in, site, map[string]string{"index.html": "v2"})
	resp, body = in.do("GET", base+"/", nil, nil)
	expect(t, resp, body, 200)
	if string(body) != "v2" {
		t.Fatalf("after redeploy = %q", body)
	}
	resp, body = in.do("GET", base+"/about", nil, nil)
	expect(t, resp, body, 404)
}

func dialWS(ctx context.Context, t *testing.T, in *instance, base string) *websocket.Conn {
	t.Helper()
	u := "ws" + strings.TrimPrefix(in.url, "http") + base + "/__backyard/api/ws"
	conn, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{
		// The websocket library rejects clients with a Timeout; share the jar.
		HTTPClient: &http.Client{Jar: in.client.Jar},
		HTTPHeader: http.Header{"Origin": []string{in.url}},
	})
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("dial %s: %v (status %d)", u, err, status)
	}
	if hello := readFrame(ctx, t, conn, "hello"); hello["cid"] == nil {
		t.Fatalf("hello = %v", hello)
	}
	return conn
}

func sub(ctx context.Context, t *testing.T, conn *websocket.Conn, collection string) {
	t.Helper()
	msg, _ := json.Marshal(map[string]any{"t": "sub", "id": "s-" + collection, "collection": collection})
	if err := conn.Write(ctx, websocket.MessageText, msg); err != nil {
		t.Fatalf("sub: %v", err)
	}
	if ack := readFrame(ctx, t, conn, "ack"); ack["id"] != "s-"+collection {
		t.Fatalf("ack = %v", ack)
	}
}

// readFrame reads frames until one of type typ arrives.
func readFrame(ctx context.Context, t *testing.T, conn *websocket.Conn, typ string) map[string]any {
	t.Helper()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("waiting for %q frame: %v", typ, err)
		}
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("frame %q: %v", data, err)
		}
		if m["t"] == typ {
			return m
		}
		if m["t"] == "error" {
			t.Fatalf("error frame while waiting for %q: %v", typ, m)
		}
	}
}

// sdkStub reports whether the embedded web package is still a stub.
func sdkStub() (stub bool) {
	defer func() {
		if recover() != nil {
			stub = true
		}
	}()
	_ = web.SDKClassic()
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// TestMultiInstanceRealtime runs two servers sharing PostgreSQL and the blob
// store, fanning realtime events out through Redis: a write on one instance
// reaches a websocket subscriber on the other.
func TestMultiInstanceRealtime(t *testing.T) {
	redisURL := os.Getenv("BACKYARD_TEST_REDIS_URL")
	if redisURL == "" {
		t.Skip("BACKYARD_TEST_REDIS_URL not set")
	}
	docs := openPostgres(t)
	if docs == nil {
		t.Skip("PostgreSQL unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	prefix := "servertest-" + suffix() + ":"
	newBroker := func() pubsub.Broker {
		b, err := redis.New(ctx, redis.Options{URL: redisURL, Prefix: prefix})
		if err != nil {
			t.Skipf("Redis unavailable: %v", err)
		}
		t.Cleanup(func() { _ = b.Close() })
		return b
	}
	blobDir := t.TempDir()
	a := newInstance(t, blobDir, docs, newBroker())
	b := newInstance(t, blobDir, docs, newBroker())
	site := "multi-" + suffix()
	deploySite(t, a, site, map[string]string{"index.html": "shared"})

	// Both instances serve the same storage.
	resp, body := b.do("GET", "/s/"+site+"/", nil, nil)
	expect(t, resp, body, 200)
	if string(body) != "shared" {
		t.Fatalf("instance b body = %q", body)
	}

	conn := dialWS(ctx, t, a, "/s/"+site)
	defer conn.CloseNow()
	sub(ctx, t, conn, "chat")
	resp, body = b.api("POST", "/s/"+site+"/__backyard/api/db/chat", map[string]any{"text": "from b"})
	expect(t, resp, body, http.StatusCreated)
	ev := readFrame(ctx, t, conn, "db")
	if doc, _ := ev["doc"].(map[string]any); ev["op"] != "create" || doc["text"] != "from b" {
		t.Fatalf("cross-instance event = %v", ev)
	}
	// Data written through b is visible through a.
	resp, body = a.api("GET", "/s/"+site+"/__backyard/api/db/chat", nil)
	expect(t, resp, body, 200)
	if !strings.Contains(string(body), "from b") {
		t.Fatalf("list on a = %s", body)
	}
}
