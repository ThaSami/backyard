package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/sites"
)

// deadlineWriter is a ResponseWriter that records the connection deadlines
// handlers set through http.ResponseController.
type deadlineWriter struct {
	*httptest.ResponseRecorder
	mu    sync.Mutex
	read  []time.Time
	write []time.Time
}

func newDeadlineWriter() *deadlineWriter {
	return &deadlineWriter{ResponseRecorder: httptest.NewRecorder()}
}

func (d *deadlineWriter) SetReadDeadline(t time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.read = append(d.read, t)
	return nil
}

func (d *deadlineWriter) SetWriteDeadline(t time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.write = append(d.write, t)
	return nil
}

func (d *deadlineWriter) last() (read, write time.Time, nRead, nWrite int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if n := len(d.read); n > 0 {
		read = d.read[n-1]
	}
	if n := len(d.write); n > 0 {
		write = d.write[n-1]
	}
	return read, write, len(d.read), len(d.write)
}

func TestConnectionDeadlines(t *testing.T) {
	h := newHarness(t)
	serve := func(method, url, body string, hdr map[string]string) *deadlineWriter {
		t.Helper()
		r := httptest.NewRequest(method, url, strings.NewReader(body))
		r.Header.Set("X-Backyard", "1")
		r.Header.Set("Content-Type", "application/json")
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		w := newDeadlineWriter()
		h.h.ServeHTTP(w, r)
		return w
	}

	t.Run("sse stream is bounded", func(t *testing.T) {
		start := time.Now()
		w := serve("POST", aiURL+"/chat", `{"messages":[{"role":"user","content":"hi"}],"stream":true}`, nil)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"type":"done"`) {
			t.Fatalf("stream: %d %s", w.Code, w.Body.String())
		}
		_, wd, _, n := w.last()
		if n == 0 || wd.IsZero() {
			t.Fatalf("write deadline cleared or never set (%d calls, last %v): a stalled client would pin the stream", n, wd)
		}
		if min := start.Add(10 * time.Minute); wd.Before(min) {
			t.Fatalf("write deadline %v is shorter than the streaming budget", wd.Sub(start))
		}
		if wd.After(time.Now().Add(time.Hour)) {
			t.Fatalf("write deadline %v is unreasonably long", wd.Sub(start))
		}
	})

	t.Run("websocket clears both", func(t *testing.T) {
		w := serve("GET", "https://foo.backyard.test/__backyard/api/ws", "", nil)
		rd, wd, nr, nw := w.last()
		if nr == 0 || nw == 0 || !rd.IsZero() || !wd.IsZero() {
			t.Fatalf("ws deadlines read=%v (%d) write=%v (%d)", rd, nr, wd, nw)
		}
	})

	t.Run("upload extends read", func(t *testing.T) {
		body, ct := multipartBody(t, part{field: "file", filename: "a.txt", content: "abc"})
		r := httptest.NewRequest("POST", filesURL, body)
		r.Header.Set("X-Backyard", "1")
		r.Header.Set("Content-Type", ct)
		w := newDeadlineWriter()
		h.h.ServeHTTP(w, r)
		if w.Code != http.StatusCreated {
			t.Fatalf("upload: %d %s", w.Code, w.Body.String())
		}
		rd, _, _, _ := w.last()
		if time.Until(rd) < 20*time.Minute {
			t.Fatalf("upload read deadline %v", time.Until(rd))
		}
	})
}

func TestAIChatModelPrecheck(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct {
		name, body, code string
		status           int
	}{
		{"image model streams", `{"model":"image","stream":true,"messages":[{"role":"user","content":"hi"}]}`, "bad_request", 400},
		{"embedding model", `{"model":"embed","messages":[{"role":"user","content":"hi"}]}`, "bad_request", 400},
		{"unknown streams", `{"model":"nope","stream":true,"messages":[{"role":"user","content":"hi"}]}`, "not_found", 404},
		{"raw model without allow_raw_models", `{"model":"openai/gpt-x","stream":true,"messages":[{"role":"user","content":"hi"}]}`, "not_found", 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := h.api("POST", aiURL+"/chat", tc.body)
			expectError(t, w, tc.status, tc.code)
		})
	}
	before := len(h.ai.chatReqs)
	if before != 0 {
		t.Fatalf("the provider was called for invalid models: %d", before)
	}

	// No default chat model: a request without "model" is a clean 404.
	h.ai.noDefault = true
	w := h.api("POST", aiURL+"/chat", `{"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	expectError(t, w, 404, "not_found")

	// Raw models pass the precheck when allowed (the router validates them).
	r := newHarness(t, withConfig(func(c *config.Config) { c.AI.AllowRawModels = true }))
	w = r.api("POST", aiURL+"/chat", `{"model":"openai/gpt-x","messages":[{"role":"user","content":"hi"}]}`)
	expectStatus(t, w, 200)
	if alias, _ := r.ai.lastChat(); alias != "openai/gpt-x" {
		t.Fatalf("alias = %q", alias)
	}
}

func TestDevModeHostAliases(t *testing.T) {
	h := newHarness(t, withConfig(func(c *config.Config) { c.Server.Dev = true }))
	for _, u := range []string{"http://127.0.0.1:8080/", "http://[::1]:8080/", "http://192.168.1.20:8080/", "http://localhost:8080/", "http://x.y.localhost:8080/"} {
		w := h.get(u)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "hub index") {
			t.Fatalf("%s: %d %q", u, w.Code, w.Body.String())
		}
	}
	// Any other name is unknown, even in dev mode (DNS rebinding).
	for _, u := range []string{"http://evil.example:8080/", "http://rebind.attacker.test/__backyard/api/me"} {
		w := h.get(u)
		expectStatus(t, w, 404)
	}
	// CSRF cannot be satisfied by a rebinding origin either.
	w := h.api("POST", "http://evil.example:8080/__backyard/api/tokens", map[string]any{}, "Origin", "http://evil.example:8080")
	expectStatus(t, w, 404)
	// Health checks still answer on any host.
	expectStatus(t, h.get("http://evil.example:8080/__backyard/healthz"), 200)

	// Outside dev mode even IP literals are unknown.
	p := newHarness(t)
	expectStatus(t, p.get("http://127.0.0.1:8080/"), 404)
}

func TestDevSitesHideDotfiles(t *testing.T) {
	dir := t.TempDir()
	for p, c := range map[string]string{
		"index.html":                "home",
		".env":                      "SECRET=1",
		".git/config":               "[core]",
		"sub/.hidden.html":          "hidden",
		".well-known/security.txt":  "contact: x",
		"assets/.cache/data.json":   "{}",
		"assets/app.js":             "js",
		"assets/.well-known/x.json": "ok",
	} {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h := newHarness(t, func(h *harness, d *Deps) { d.DevSites = map[string]string{"local": dir} })
	for _, p := range []string{"/.env", "/.git/config", "/sub/.hidden.html", "/sub/.hidden", "/assets/.cache/data.json"} {
		w := h.get("https://local.backyard.test" + p)
		if w.Code != 404 || strings.Contains(w.Body.String(), "SECRET") || strings.Contains(w.Body.String(), "[core]") {
			t.Fatalf("%s: %d %q", p, w.Code, w.Body.String())
		}
	}
	for p, want := range map[string]string{
		"/":                          "home",
		"/.well-known/security.txt":  "contact: x",
		"/assets/app.js":             "js",
		"/assets/.well-known/x.json": "ok",
	} {
		w := h.get("https://local.backyard.test" + p)
		if w.Code != 200 || w.Body.String() != want {
			t.Fatalf("%s: %d %q", p, w.Code, w.Body.String())
		}
	}
}

func TestHiddenPath(t *testing.T) {
	for p, want := range map[string]bool{
		"index.html": false, "a/b.c": false, ".well-known/x": false, "a/.well-known/x": false,
		".env": true, "a/.git/config": true, "a/..b": true, ".": true, "a/.": true, "x.y/z": false,
	} {
		if got := hiddenPath(p); got != want {
			t.Errorf("hiddenPath(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestObjectCacheFillRacesPurge(t *testing.T) {
	c := newObjectCache(1<<20, time.Minute)
	key := sites.SiteKey("foo", "index.html")
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.do(key, func() (*cacheEntry, error) {
			close(started)
			<-release
			return &cacheEntry{key: key, data: []byte("old")}, nil
		})
	}()
	<-started
	c.purgePrefix(sites.SiteKey("foo", "")) // a deploy lands while the old bytes are read
	close(release)
	<-done
	if _, ok := c.get(key); ok {
		t.Fatal("a fill that started before the purge was cached (stale content after deploy)")
	}
	// Later fills are cached as usual.
	if _, err := c.do(key, func() (*cacheEntry, error) { return &cacheEntry{key: key, data: []byte("new")}, nil }); err != nil {
		t.Fatal(err)
	}
	if e, ok := c.get(key); !ok || string(e.data) != "new" {
		t.Fatalf("fresh fill not cached: %v %v", e, ok)
	}

	// remove also invalidates in-flight fills.
	gen := c.generation()
	c.remove("other")
	c.putIfGen(&cacheEntry{key: "k2", data: []byte("x")}, gen)
	if _, ok := c.get("k2"); ok {
		t.Fatal("putIfGen stored an entry after an invalidation")
	}

	ec := newExistsCache(time.Minute)
	g := ec.generation()
	ec.forget("foo")
	ec.put("foo", false, g)
	if _, ok := ec.get("foo"); ok {
		t.Fatal("a stale 'site does not exist' survived forget")
	}
	ec.put("foo", true, ec.generation())
	if v, ok := ec.get("foo"); !ok || !v {
		t.Fatalf("exists cache: %v %v", v, ok)
	}
}

func TestPurgeSiteMakesFirstDeployVisible(t *testing.T) {
	h := newHarness(t)
	// Visiting a site before it exists caches "site not found".
	w := h.get("https://late.backyard.test/")
	expectStatus(t, w, 404)
	h.deploy("late", map[string]string{"index.html": "hello"})
	h.srv.PurgeSite("late")
	w = h.get("https://late.backyard.test/")
	if w.Code != 200 || w.Body.String() != "hello" {
		t.Fatalf("after deploy: %d %q", w.Code, w.Body.String())
	}
}

func TestBackgroundTracker(t *testing.T) {
	b := newBackground()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.wait(ctx); err != nil {
		t.Fatalf("idle wait: %v", err)
	}
	release := make(chan struct{})
	for i := 0; i < 3; i++ {
		if !b.run(3, func() { <-release }) {
			t.Fatalf("run %d refused below the limit", i)
		}
	}
	if b.run(3, func() {}) {
		t.Fatal("run accepted above the limit")
	}
	if !b.run(0, func() { <-release }) {
		t.Fatal("unlimited run refused")
	}
	short, cancelShort := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelShort()
	if err := b.wait(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait with running tasks = %v", err)
	}
	close(release)
	if err := b.wait(ctx); err != nil || b.running() != 0 {
		t.Fatalf("wait = %v, running %d", err, b.running())
	}

	// Starting work while someone waits is safe (sync.WaitGroup would
	// panic or race here).
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					b.run(0, func() {})
				}
			}
		}()
	}
	for i := 0; i < 20; i++ {
		// Under constant load wait may time out; it must not panic or race.
		wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
		_ = b.wait(wctx)
		wcancel()
	}
	close(stop)
	wg.Wait()
	final, cancelFinal := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelFinal()
	if err := b.wait(final); err != nil {
		t.Fatalf("final wait: %v", err)
	}
}

func TestTouchesAreBounded(t *testing.T) {
	h := newHarness(t)
	release := make(chan struct{})
	for i := 0; i < maxBackgroundTouches; i++ {
		h.srv.bg.run(0, func() { <-release })
	}
	h.api("GET", "https://foo.backyard.test/__backyard/api/me", nil)
	if n := h.srv.bg.running(); n != maxBackgroundTouches {
		t.Fatalf("a touch started beyond the bound: %d running", n)
	}
	close(release)
	waitBackground(t, h)
	// The skipped touch is retried by the next request.
	h.api("GET", "https://foo.backyard.test/__backyard/api/me", nil)
	waitBackground(t, h)
	if n := h.docs.collectionLen("_system", "_users"); n != 1 {
		t.Fatalf("user not recorded after the backlog cleared: %d", n)
	}
}

func TestFileMetadataScopedToSite(t *testing.T) {
	h := newHarness(t)
	h.deploy("foo", map[string]string{"index.html": "static bytes"})
	h.blob.set("uploads/bar/01HX/secret.txt", []byte("bar secret"), time.Now())
	ctx := context.Background()
	for id, key := range map[string]string{
		"evil1": sites.SiteKey("foo", "index.html"),
		"evil2": "uploads/bar/01HX/secret.txt",
		"evil3": "_meta/sites/foo.json",
	} {
		if _, err := h.docs.Create(ctx, "foo", filesCollection, id, map[string]any{"name": "x.txt", "key": key, "size": 3, "content_type": "text/plain"}, "x"); err != nil {
			t.Fatal(err)
		}
		w := h.api("GET", filesURL+"/"+id, nil)
		expectError(t, w, 404, "not_found")
		w = h.get("https://foo.backyard.test/__backyard/files/" + id + "/x.txt")
		if w.Code != 404 || strings.Contains(w.Body.String(), "bytes") || strings.Contains(w.Body.String(), "secret") {
			t.Fatalf("%s (%s) served: %d %q", id, key, w.Code, w.Body.String())
		}
	}
	w := h.api("GET", filesURL, nil)
	expectStatus(t, w, 200)
	if strings.Contains(w.Body.String(), "evil") {
		t.Fatalf("list exposes foreign keys: %s", w.Body.String())
	}
}

func TestFileDeleteWhenBlobDeleteFails(t *testing.T) {
	h := newHarness(t)
	res := h.upload(filesURL, part{field: "file", filename: "a.txt", content: "abc"})
	if res.code != http.StatusCreated {
		t.Fatalf("upload: %d %s", res.code, res.body)
	}
	var fi fileInfo
	if err := jsonUnmarshal(res.body, &fi); err != nil {
		t.Fatal(err)
	}
	h.blob.delErr = errors.New("storage unavailable")
	w := h.api("DELETE", filesURL+"/"+fi.ID, nil)
	expectStatus(t, w, http.StatusNoContent)
	h.blob.delErr = nil
	// Gone for the API and for downloads even though the bytes remain.
	expectError(t, h.api("GET", filesURL+"/"+fi.ID, nil), 404, "not_found")
	expectStatus(t, h.get("https://foo.backyard.test"+fi.URL), 404)
	if !strings.Contains(h.logText(), "deleting upload bytes failed") {
		t.Fatal("blob delete failure not logged")
	}
}

// flushErrWriter is a ResponseWriter whose flush fails (client gone).
type flushErrWriter struct {
	*httptest.ResponseRecorder
}

func (f flushErrWriter) FlushError() error { return errors.New("broken pipe") }

func TestStatusRecorderFlushError(t *testing.T) {
	rec := &statusRecorder{ResponseWriter: flushErrWriter{httptest.NewRecorder()}}
	if err := http.NewResponseController(rec).Flush(); err == nil || !strings.Contains(err.Error(), "broken pipe") {
		t.Fatalf("Flush error = %v", err)
	}
	if !rec.wroteHeader || rec.status != 200 {
		t.Fatalf("flush did not commit the header: %v %d", rec.wroteHeader, rec.status)
	}
	ok := &statusRecorder{ResponseWriter: httptest.NewRecorder()}
	if err := http.NewResponseController(ok).Flush(); err != nil {
		t.Fatalf("Flush = %v", err)
	}
}
