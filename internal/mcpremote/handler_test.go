package mcpremote

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/auth"
	"github.com/thasami/backyard/internal/docstore/memory"
	"github.com/thasami/backyard/internal/identity"
)

const initialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`

func wantResponse(t *testing.T, w *httptest.ResponseRecorder, status int, body string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d; body %s", w.Code, status, w.Body)
	}
	if body != "" && strings.TrimSpace(w.Body.String()) != body {
		t.Fatalf("body = %s\nwant   %s", w.Body, body)
	}
}

func TestDisabled(t *testing.T) {
	const want = `{"error":{"code":"not_found","message":"the Claude connector is not enabled on this server"}}`
	for name, a := range map[string]AuthServer{"nil": nil, "nil *auth.MCP": (*auth.MCP)(nil)} {
		t.Run(name, func(t *testing.T) {
			e := newTenv(t, tenvOption{opts: func(o *Options) { o.Auth = a }})
			wantResponse(t, e.post(initialize), http.StatusNotFound, want)
			wantResponse(t, e.serve(httptest.NewRequest(http.MethodGet, "/__backyard/mcp", nil)), http.StatusNotFound, want)
		})
	}
}

func TestCanonicalHostOnly(t *testing.T) {
	e := newTenv(t)
	r := request(initialize)
	r.Host = "other.backyard.test"
	wantResponse(t, e.serve(r), http.StatusNotFound, `{"error":{"code":"not_found","message":"not found"}}`)
	// The port and letter case of the host do not matter.
	r = request(initialize)
	r.Host = "HUB.backyard.test:8443"
	wantResponse(t, e.serve(r), http.StatusOK, "")
	// RequestOrigin (trusted proxy headers) decides when given.
	e = newTenv(t, tenvOption{opts: func(o *Options) {
		o.RequestOrigin = func(*http.Request) string { return "https://elsewhere.example" }
	}})
	wantResponse(t, e.post(initialize), http.StatusNotFound, "")
}

func TestOrigin(t *testing.T) {
	e := newTenv(t)
	for _, o := range []string{"", hubURL, "HTTPS://HUB.backyard.test:443", "https://claude.ai", "https://claude.com"} {
		if w := e.post(initialize, "Origin", o); w.Code != http.StatusOK {
			t.Errorf("Origin %q: status %d %s", o, w.Code, w.Body)
		}
	}
	const forbidden = `{"error":{"code":"forbidden","message":"requests from this origin are not accepted"}}`
	for _, o := range []string{"https://evil.example", "null", "https://claude.ai.evil.example", "https://hub.backyard.test.evil"} {
		wantResponse(t, e.post(initialize, "Origin", o), http.StatusForbidden, forbidden)
	}
	// The request's own origin is accepted too (here plain http).
	r := request(initialize, "Origin", "http://hub.backyard.test:8080")
	r.TLS, r.Host = nil, "hub.backyard.test:8080"
	wantResponse(t, e.serve(r), http.StatusOK, "")
	wantResponse(t, e.post(initialize, "Origin", "http://hub.backyard.test:8080"), http.StatusForbidden, forbidden)
	// The origin is checked before the credentials.
	wantResponse(t, e.post(initialize, "Origin", "https://evil.example", "Authorization", ""), http.StatusForbidden, forbidden)
}

func TestCredentials(t *testing.T) {
	e := newTenv(t)
	const unauth = `{"error":{"code":"unauthenticated","message":"connect this app to Backyard to sign in"}}`
	missing := `Bearer resource_metadata="` + prmURL + `", scope="mcp"`
	for _, h := range []string{"", "Basic YWxpY2U6", "Bearer", "Bearer   ", "Token " + aliceToken} {
		w := e.post(initialize, "Authorization", h)
		wantResponse(t, w, http.StatusUnauthorized, unauth)
		if got := w.Header().Get("WWW-Authenticate"); got != missing {
			t.Errorf("Authorization %q: WWW-Authenticate = %q", h, got)
		}
	}
	// The scheme is case-insensitive.
	if w := e.post(initialize, "Authorization", "bEaReR "+aliceToken); w.Code != http.StatusOK {
		t.Fatalf("lowercase scheme: %d", w.Code)
	}

	r := request(initialize)
	r.Header.Add("Authorization", "Bearer "+bobToken)
	w := e.serve(r)
	wantResponse(t, w, http.StatusBadRequest, "")
	if got := w.Header().Get("WWW-Authenticate"); got != `Bearer error="invalid_request", resource_metadata="`+prmURL+`"` {
		t.Fatalf("WWW-Authenticate = %q", got)
	}
	if !strings.Contains(w.Body.String(), `"code":"bad_request"`) {
		t.Fatalf("body = %s", w.Body)
	}
	// One value that joins two credentials (as a proxy or the Workers runtime
	// does) counts as two headers; a credential with auth-params does not.
	w = e.post(initialize, "Authorization", "Bearer "+aliceToken+", Bearer "+bobToken)
	wantResponse(t, w, http.StatusBadRequest, "")
	if got := w.Header().Get("WWW-Authenticate"); !strings.HasPrefix(got, `Bearer error="invalid_request"`) {
		t.Fatalf("joined credentials: WWW-Authenticate = %q", got)
	}
	for v, want := range map[string]bool{"Bearer a, Bearer b": true, "Bearer a,Basic b": true, "Bearer a": false,
		`Digest a=1, b=2`: false, "Bearer a, b": false} {
		if got := severalCredentials(v); got != want {
			t.Errorf("severalCredentials(%q) = %v", v, got)
		}
	}

	for _, desc := range []string{"malformed token", "token expired", "token revoked", "token not issued for this server", "access removed by policy"} {
		tok := "byat_" + strings.ReplaceAll(desc, " ", "-")
		e.auth.bad[tok] = desc
		w := e.post(initialize, "Authorization", "Bearer "+tok)
		wantResponse(t, w, http.StatusUnauthorized, unauth)
		want := `Bearer error="invalid_token", error_description="` + desc + `", resource_metadata="` + prmURL + `", scope="mcp"`
		if got := w.Header().Get("WWW-Authenticate"); got != want {
			t.Errorf("%s: WWW-Authenticate = %q\nwant %q", desc, got, want)
		}
	}
	// API tokens, static tokens and JWTs are just malformed MCP tokens.
	w = e.post(initialize, "Authorization", "Bearer eyJhbGciOiJIUzI1NiJ9.e30.x")
	if got := w.Header().Get("WWW-Authenticate"); w.Code != 401 || !strings.Contains(got, `error_description="malformed token"`) {
		t.Fatalf("JWT: %d %q", w.Code, got)
	}

	e.auth.infra = errors.New("docstore down")
	w = e.post(initialize)
	if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "docstore down") || w.Header().Get("WWW-Authenticate") != "" {
		t.Fatalf("infrastructure failure: %d %s", w.Code, w.Body)
	}
}

func TestRateLimitPerUser(t *testing.T) {
	var keys []string
	e := newTenv(t, tenvOption{opts: func(o *Options) {
		o.Allow = func(key string) (bool, time.Duration) {
			keys = append(keys, key)
			return key != "user:"+bob.ID, 1500 * time.Millisecond
		}
	}})
	wantResponse(t, e.post(initialize), http.StatusOK, "")
	w := e.post(initialize, "Authorization", "Bearer "+bobToken)
	wantResponse(t, w, http.StatusTooManyRequests, "")
	if w.Header().Get("Retry-After") != "2" || !strings.Contains(w.Body.String(), `"code":"rate_limited"`) {
		t.Fatalf("429: %v %s", w.Header(), w.Body)
	}
	if strings.Join(keys, ",") != "user:alice@acme.com,user:bob@acme.com" {
		t.Fatalf("keys = %v", keys)
	}
}

func TestMethodAfterAuthentication(t *testing.T) {
	e := newTenv(t)
	const notAllowed = `{"error":{"code":"bad_request","message":"method not allowed"}}`
	for _, m := range []string{http.MethodGet, http.MethodDelete, http.MethodOptions, http.MethodPut, http.MethodPatch, http.MethodHead} {
		r := request("")
		r.Method = m
		w := e.serve(r)
		if m == http.MethodHead {
			wantResponse(t, w, http.StatusMethodNotAllowed, "")
		} else {
			wantResponse(t, w, http.StatusMethodNotAllowed, notAllowed)
		}
		if w.Header().Get("Allow") != "POST" || w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("%s: headers %v", m, w.Header())
		}
	}
	// Without a token the 401 comes first.
	r := request("", "Authorization", "")
	r.Method = http.MethodGet
	wantResponse(t, e.serve(r), http.StatusUnauthorized, "")
}

func TestMediaTypes(t *testing.T) {
	e := newTenv(t)
	for _, ct := range []string{"", "text/plain", "application/x-www-form-urlencoded", "application/json-seq", "bad;;"} {
		w := e.post(initialize, "Content-Type", ct)
		wantResponse(t, w, http.StatusUnsupportedMediaType, "")
		if !strings.Contains(w.Body.String(), `"code":"bad_request"`) {
			t.Errorf("Content-Type %q: %s", ct, w.Body)
		}
	}
	wantResponse(t, e.post(initialize, "Content-Type", "application/json; charset=utf-8"), http.StatusOK, "")
	for _, a := range []string{"", "application/json", "*/*", "application/*", "text/event-stream, application/json;q=0.9"} {
		if w := e.post(initialize, "Accept", a); w.Code != http.StatusOK {
			t.Errorf("Accept %q: %d", a, w.Code)
		}
	}
	for _, a := range []string{"text/event-stream", "text/html", "image/*"} {
		w := e.post(initialize, "Accept", a)
		wantResponse(t, w, http.StatusBadRequest, "")
		if !strings.Contains(w.Body.String(), `"code":"bad_request"`) {
			t.Errorf("Accept %q: %s", a, w.Body)
		}
	}
}

func TestProtocolVersionHeader(t *testing.T) {
	e := newTenv(t)
	for _, v := range []string{"", "2025-11-25", "2025-06-18"} {
		wantResponse(t, e.post(initialize, "MCP-Protocol-Version", v), http.StatusOK, "")
	}
	for _, v := range []string{"2025-03-26", "2026-07-28", "2024-11-05", "latest"} {
		w := e.post(initialize, "MCP-Protocol-Version", v)
		wantResponse(t, w, http.StatusBadRequest,
			`{"jsonrpc":"2.0","id":null,"error":{"code":-32022,"message":"Unsupported protocol version","data":{"supported":["2025-11-25","2025-06-18"],"requested":"`+v+`"}}}`)
		if w.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("Content-Type = %q", w.Header().Get("Content-Type"))
		}
	}
}

func TestBodyLimit(t *testing.T) {
	e := newTenv(t)
	e.h.maxBody = 1024
	const tooLarge = `{"error":{"code":"too_large","message":"MCP requests are limited to 1024 bytes; publish over several calls"}}`
	big := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"x":"` + strings.Repeat("a", 1024) + `"}}`
	// By Content-Length, before reading.
	r := request(big)
	read := false
	r.Body = io.NopCloser(readFunc(func(p []byte) (int, error) { read = true; return 0, io.EOF }))
	wantResponse(t, e.serve(r), http.StatusRequestEntityTooLarge, tooLarge)
	if read {
		t.Fatal("the body was read despite its Content-Length")
	}
	// By stream, without a length.
	r = request(big)
	r.ContentLength = -1
	wantResponse(t, e.serve(r), http.StatusRequestEntityTooLarge, tooLarge)
	// Exactly at the limit.
	ok := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"x":"` + strings.Repeat("a", 1024-len(`{"jsonrpc":"2.0","id":1,"method":"ping","params":{"x":""}}`)) + `"}}`
	if len(ok) != 1024 {
		t.Fatalf("len = %d", len(ok))
	}
	wantResponse(t, e.post(ok), http.StatusOK, `{"jsonrpc":"2.0","id":1,"result":{}}`)
}

type readFunc func([]byte) (int, error)

func (f readFunc) Read(p []byte) (int, error) { return f(p) }

// addUser lets token authenticate u.
func (e *tenv) addUser(token string, u *identity.User) {
	e.auth.mu.Lock()
	defer e.auth.mu.Unlock()
	g := *e.auth.grants[aliceToken]
	g.User = *u
	e.auth.grants[token] = &g
}

// stalledLarge starts a large request of token whose body arrives only when
// the returned pipe is written and closed; done is closed once it was
// answered with the status in *code.
func (e *tenv) stalledLarge(token string, code *int) (*io.PipeWriter, chan struct{}) {
	pr, pw := io.Pipe()
	r := request("", "Authorization", "Bearer "+token)
	r.Body, r.ContentLength = pr, -1
	done := make(chan struct{})
	go func() {
		defer close(done)
		w := httptest.NewRecorder()
		e.h.ServeHTTP(w, r)
		*code = w.Code
	}()
	return pw, done
}

func TestAdmission(t *testing.T) {
	e := newTenv(t)
	e.h.admitWait = 50 * time.Millisecond
	carol := "byat_carol.0000000000000000000000"
	e.addUser(carol, &identity.User{ID: "carol@acme.com", Email: "carol@acme.com", Name: "Carol"})
	var pipes []*io.PipeWriter
	var dones []chan struct{}
	codes := make([]int, admissionSlots)
	for i, tok := range []string{aliceToken, bobToken, guestToken, adminToken} {
		pw, done := e.stalledLarge(tok, &codes[i])
		pipes, dones = append(pipes, pw), append(dones, done)
	}
	// Wait until the four hold their slots.
	for deadline := time.Now().Add(5 * time.Second); len(e.h.slots) < admissionSlots; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("slots not taken")
		}
	}
	r := request(`{"jsonrpc":"2.0","id":1,"method":"ping"}`, "Authorization", "Bearer "+carol)
	r.ContentLength = -1
	w := e.serve(r)
	wantResponse(t, w, http.StatusTooManyRequests, "")
	if w.Header().Get("Retry-After") != "5" || !strings.Contains(w.Body.String(), `"code":"rate_limited"`) {
		t.Fatalf("429: %v %s", w.Header(), w.Body)
	}
	// Small bodies of known length need no slot.
	wantResponse(t, e.post(`{"jsonrpc":"2.0","id":1,"method":"ping"}`), http.StatusOK, "")
	for i, pw := range pipes {
		_, _ = pw.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		_ = pw.Close()
		<-dones[i]
		if codes[i] != http.StatusOK {
			t.Errorf("admitted request %d: %d", i, codes[i])
		}
	}
	if len(e.h.slots) != 0 || len(e.h.users) != 0 {
		t.Fatalf("%d slots, %d users still taken", len(e.h.slots), len(e.h.users))
	}
}

// One user holds at most one admission slot at a time: their other large
// requests wait for it, and other users are admitted meanwhile.
func TestAdmissionPerUser(t *testing.T) {
	e := newTenv(t)
	e.h.admitWait = 100 * time.Millisecond
	var first, second int
	pw, done := e.stalledLarge(aliceToken, &first)
	for deadline := time.Now().Add(5 * time.Second); len(e.h.slots) < 1; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("slot not taken")
		}
	}
	// Alice's second large request waits for her slot, then gives up.
	pw2, done2 := e.stalledLarge(aliceToken, &second)
	t.Cleanup(func() { _ = pw.Close(); _ = pw2.Close() })
	select {
	case <-done2:
	case <-time.After(5 * time.Second):
		t.Fatal("alice's second large request was admitted while her first holds a slot")
	}
	if second != http.StatusTooManyRequests {
		t.Fatalf("alice's second large request: %d, want 429", second)
	}
	// Bob's is admitted.
	r := request(`{"jsonrpc":"2.0","id":1,"method":"ping"}`, "Authorization", "Bearer "+bobToken)
	r.ContentLength = -1
	wantResponse(t, e.serve(r), http.StatusOK, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	// Once alice's first is answered, her next one gets the slot again.
	_, _ = pw.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	_ = pw.Close()
	<-done
	pw3, done3 := e.stalledLarge(aliceToken, &second)
	_, _ = pw3.Write([]byte(`{"jsonrpc":"2.0","id":2,"method":"ping"}`))
	_ = pw3.Close()
	<-done3
	if first != http.StatusOK || second != http.StatusOK {
		t.Fatalf("alice's requests: %d, %d", first, second)
	}
	if len(e.h.slots) != 0 || len(e.h.users) != 0 {
		t.Fatalf("%d slots, %d users still taken", len(e.h.slots), len(e.h.users))
	}
}

// rawPost sends the headers of a POST with Content-Length n and the given
// body chunks (after each wait), then reads the answer.
func rawPost(t *testing.T, addr, token string, n int, chunks []string, wait time.Duration) (*http.Response, time.Duration, error) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, 0, err
	}
	t.Cleanup(func() { conn.Close() })
	start := time.Now()
	_, _ = fmt.Fprintf(conn, "POST /__backyard/mcp HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n", hubHost, token, n)
	go func() {
		for _, c := range chunks {
			time.Sleep(wait)
			if _, err := io.WriteString(conn, c); err != nil {
				return
			}
		}
	}()
	_ = conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return nil, time.Since(start), err
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	res.Body = io.NopCloser(bytes.NewReader(b))
	return res, time.Since(start), nil
}

// A body that falls below the minimum rate is cut off with 400, which frees
// its admission slot long before the 5-minute deadline; a body that keeps
// the pace is read in full.
func TestBodyPace(t *testing.T) {
	e := newTenv(t)
	e.h.bodyGrace, e.h.minBodyRate, e.h.admitWait = 500*time.Millisecond, 1<<20, 10*time.Second
	carol := "byat_carol.0000000000000000000000"
	e.addUser(carol, &identity.User{ID: "carol@acme.com", Email: "carol@acme.com", Name: "Carol"})
	srv := httptest.NewServer(e.h)
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	ping := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"x":"` // padded below
	big := ping + strings.Repeat("a", 1536<<10) + `"}}`
	type answer struct {
		res     *http.Response
		elapsed time.Duration
		err     error
	}
	stalled := make(chan answer, admissionSlots)
	for _, tok := range []string{aliceToken, bobToken, guestToken, adminToken} {
		go func() {
			// 4 KiB, then nothing: far behind 1 MiB/s.
			res, d, err := rawPost(t, addr, tok, len(big), []string{big[:4096]}, 0)
			stalled <- answer{res, d, err}
		}()
	}
	for deadline := time.Now().Add(5 * time.Second); len(e.h.slots) < admissionSlots; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("slots not taken")
		}
	}
	// Carol waits for a slot: the stalled bodies are cut, and hers is read.
	chunks := []string{}
	for i := 0; i < len(big); i += 256 << 10 {
		chunks = append(chunks, big[i:min(i+256<<10, len(big))])
	}
	res, d, err := rawPost(t, addr, carol, len(big), chunks, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("a paced body: no answer after %v: %v", d, err)
	}
	if b, _ := io.ReadAll(res.Body); res.StatusCode != http.StatusOK || !strings.Contains(string(b), `"result":{}`) {
		t.Fatalf("a paced body after %v: %d %s", d, res.StatusCode, b)
	}
	for range admissionSlots {
		a := <-stalled
		if a.err != nil || a.res.StatusCode != http.StatusBadRequest || a.elapsed > 5*time.Second {
			t.Fatalf("a stalled body: %v %v after %v", a.res, a.err, a.elapsed)
		}
	}
}

// A body of at most 1 MiB is not paced: sent slower than the pace of large
// bodies, it is read in full within the server's read timeout.
func TestSmallBodyKeepsServerTimeout(t *testing.T) {
	e := newTenv(t)
	e.h.bodyGrace, e.h.minBodyRate = 200*time.Millisecond, 1<<20
	srv := httptest.NewUnstartedServer(e.h)
	srv.Config.ReadTimeout = 5 * time.Second
	srv.Start()
	t.Cleanup(srv.Close)
	ping := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"x":"`
	small := ping + strings.Repeat("a", 512<<10-len(ping)-4) + `"}}`
	chunks := []string{}
	for i := 0; i < len(small); i += 16 << 10 {
		chunks = append(chunks, small[i:min(i+16<<10, len(small))])
	}
	// 32 chunks, one every 60 ms: about 270 KiB/s, behind the pace.
	res, d, err := rawPost(t, srv.Listener.Addr().String(), aliceToken, len(small), chunks, 60*time.Millisecond)
	if err != nil {
		t.Fatalf("no answer after %v: %v", d, err)
	}
	if b, _ := io.ReadAll(res.Body); res.StatusCode != http.StatusOK || !strings.Contains(string(b), `"result":{}`) {
		t.Fatalf("a small slow body after %v: %d %s", d, res.StatusCode, b)
	}
}

// deadlineWriter records the deadlines set through http.ResponseController.
type deadlineWriter struct {
	*httptest.ResponseRecorder
	bodyRead          *bool
	readSet, writeSet bool
	readBefore        bool      // the first read deadline was set before the body was read
	firstRead         time.Time // the first read deadline
	read, write       time.Time // the last ones
}

func (d *deadlineWriter) SetReadDeadline(t time.Time) error {
	if !d.readSet {
		d.firstRead, d.readBefore = t, !*d.bodyRead
	}
	d.readSet, d.read = true, t
	return nil
}

func (d *deadlineWriter) SetWriteDeadline(t time.Time) error {
	d.writeSet, d.write = true, t
	return nil
}

func TestDeadlinesBeforeBody(t *testing.T) {
	e := newTenv(t)
	read := false
	r := request(initialize)
	body := strings.NewReader(initialize)
	r.Body = io.NopCloser(readFunc(func(p []byte) (int, error) { read = true; return body.Read(p) }))
	w := &deadlineWriter{ResponseRecorder: httptest.NewRecorder(), bodyRead: &read}
	e.h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !read {
		t.Fatalf("status %d, body read %v", w.Code, read)
	}
	// A small body is read under the server's read timeout (no read
	// deadline before it); the answer, and the connection once the body is
	// read, get 5 minutes.
	if !w.readSet || !w.writeSet || w.readBefore {
		t.Fatalf("deadlines: read %v (before body %v), write %v", w.readSet, w.readBefore, w.writeSet)
	}
	for _, dl := range []time.Time{w.read, w.write} {
		if d := time.Until(dl); d < 4*time.Minute || d > 5*time.Minute+time.Second {
			t.Fatalf("deadline in %v", d)
		}
	}
	// A body of unknown length is admitted and paced: due within the grace
	// for its first bytes.
	read = false
	r = request(initialize)
	body = strings.NewReader(initialize)
	r.ContentLength = -1
	r.Body = io.NopCloser(readFunc(func(p []byte) (int, error) { read = true; return body.Read(p) }))
	w = &deadlineWriter{ResponseRecorder: httptest.NewRecorder(), bodyRead: &read}
	e.h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !w.readBefore {
		t.Fatalf("unknown length: status %d, read deadline before the body %v", w.Code, w.readBefore)
	}
	if d := time.Until(w.firstRead); d <= 0 || d > bodyGrace+time.Second {
		t.Fatalf("first read deadline in %v", d)
	}
	if d := time.Until(w.read); d < 4*time.Minute || d > 5*time.Minute+time.Second {
		t.Fatalf("read deadline once the body is read in %v", d)
	}
	// Requests refused before the body is read (Origin, credentials) keep
	// the server's timeouts.
	for _, hdr := range [][]string{{"Origin", "https://evil.example"}, {"Authorization", ""}, {"Authorization", "Bearer byat_bad"}} {
		read = false
		r := request(initialize, hdr...)
		w := &deadlineWriter{ResponseRecorder: httptest.NewRecorder(), bodyRead: &read}
		e.h.ServeHTTP(w, r)
		if w.Code < 400 || w.readSet || w.writeSet {
			t.Errorf("%v: status %d, deadlines set: read %v, write %v", hdr, w.Code, w.readSet, w.writeSet)
		}
	}
}

// An unauthenticated client that sends headers and then trickles (or
// withholds) its body is answered and let go within server.read_timeout,
// not the 5-minute deadline of an authenticated publish.
func TestUnauthenticatedSlowBody(t *testing.T) {
	e := newTenv(t)
	srv := httptest.NewUnstartedServer(e.h)
	srv.Config.ReadTimeout = 300 * time.Millisecond
	srv.Start()
	t.Cleanup(srv.Close)
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = fmt.Fprintf(conn, "POST /__backyard/mcp HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: 1000\r\n\r\n", hubHost)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	start := time.Now()
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no answer within 5 s (%v): the unread body is drained under the 5-minute deadline", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized || time.Since(start) > 2*time.Second {
		t.Fatalf("status %d after %v", res.StatusCode, time.Since(start))
	}
}

func TestJSONRPCErrors(t *testing.T) {
	e := newTenv(t)
	const (
		parseError = `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error"}}`
		invalid    = `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Invalid Request"}}`
	)
	cases := []struct{ body, want string }{
		{`{"jsonrpc":"2.0","id":1,"method":`, parseError},
		{``, parseError},
		{`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Invalid Request: JSON-RPC batches are not supported"}}`},
		{`"ping"`, invalid},
		{`{"jsonrpc":"1.0","id":1,"method":"ping"}`, invalid},
		{`{"id":1,"method":"ping"}`, invalid},
		{`{"jsonrpc":"2.0","id":1,"method":7}`, invalid},
		{`{"jsonrpc":"2.0","id":null,"method":"ping"}`, invalid},
		{`{"jsonrpc":"2.0","id":1.5,"method":"ping"}`, invalid},
		{`{"jsonrpc":"2.0","id":1e3,"method":"ping"}`, invalid},
		{`{"jsonrpc":"2.0","id":{"a":1},"method":"ping"}`, invalid},
		{`{"jsonrpc":"2.0","id":1}`, invalid},
		{`{"jsonrpc":"2.0","id":true,"method":"ping"}`, invalid},
		{`{"jsonrpc":"2.0","id":"x","method":"ping","params":[1}`, parseError},
		// Not UTF-8 (a truncated sequence, a stray byte) is no JSON text.
		{"{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"ping\",\"params\":{\"path\":\"a\xf0\x9f\x98.html\"}}", parseError},
		{"{\"jsonrpc\":\"2.0\",\"id\":\"\xff\",\"method\":\"ping\"}", parseError},
		{"\n {\"jsonrpc\":\"2.0\",\"id\":\"a\",\"method\":\"ping\"} ", `{"jsonrpc":"2.0","id":"a","result":{}}`},
	}
	for _, c := range cases {
		status := http.StatusBadRequest
		if strings.Contains(c.want, `"result"`) {
			status = http.StatusOK
		}
		wantResponse(t, e.post(c.body), status, c.want)
	}
	// Notifications and client responses: 202 without a body.
	for _, body := range []string{
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`,
		`{"jsonrpc":"2.0","id":3,"result":{}}`,
		`{"jsonrpc":"2.0","id":3,"error":{"code":1,"message":"x"}}`,
	} {
		w := e.post(body)
		if w.Code != http.StatusAccepted || w.Body.Len() != 0 {
			t.Errorf("%s: %d %q", body, w.Code, w.Body)
		}
	}
}

func TestInitialize(t *testing.T) {
	e := newTenv(t)
	for requested, want := range map[string]string{"2025-06-18": "2025-06-18", "2025-11-25": "2025-11-25", "2024-11-05": "2025-11-25", "x": "2025-11-25"} {
		res, rerr := e.rpc("initialize", map[string]any{"protocolVersion": requested, "capabilities": map[string]any{}})
		if rerr != nil {
			t.Fatal(rerr)
		}
		var got struct {
			ProtocolVersion string          `json:"protocolVersion"`
			Capabilities    json.RawMessage `json:"capabilities"`
			ServerInfo      map[string]any  `json:"serverInfo"`
			Instructions    string          `json:"instructions"`
		}
		if err := json.Unmarshal(res, &got); err != nil {
			t.Fatal(err)
		}
		if got.ProtocolVersion != want || string(got.Capabilities) != `{"tools":{}}` || got.Instructions != e.h.cat.instructions ||
			got.ServerInfo["name"] != "backyard" || got.ServerInfo["title"] != "Backyard" || got.ServerInfo["version"] != "1.2.3" {
			t.Fatalf("initialize(%s) = %s", requested, res)
		}
		if !strings.HasPrefix(string(res), `{"protocolVersion":`) || !strings.Contains(string(res), `<script src=\"/__backyard/backyard.js\"></script>`) {
			t.Fatalf("result is not compact or HTML-escaped: %s", res)
		}
	}
	for _, params := range []any{
		nil, map[string]any{}, map[string]any{"protocolVersion": 20251125}, []any{1},
		map[string]any{"protocolVersion": nil},
		// The member name is exact (encoding/json would match these to a struct field).
		map[string]any{"ProtocolVersion": "2025-06-18"}, map[string]any{"PROTOCOLVERSION": "2025-06-18"},
	} {
		if _, rerr := e.rpc("initialize", params); rerr == nil || rerr.Code != codeInvalidParams || rerr.Message != "Invalid params" {
			t.Errorf("initialize(%v) = %+v", params, rerr)
		}
	}
	// No prior initialize is needed, and a session id is ignored.
	if _, rerr := e.rpc("ping", nil, "Mcp-Session-Id", "abc", "Last-Event-ID", "5"); rerr != nil {
		t.Fatal(rerr)
	}
}

func TestPingListUnknown(t *testing.T) {
	e := newTenv(t)
	if res, rerr := e.rpc("ping", nil); rerr != nil || string(res) != "{}" {
		t.Fatalf("ping = %s %v", res, rerr)
	}
	res, rerr := e.rpc("tools/list", map[string]any{"cursor": "x"})
	if rerr != nil {
		t.Fatal(rerr)
	}
	var doc struct {
		Tools json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(toolsJSON, &doc); err != nil {
		t.Fatal(err)
	}
	var want bytes.Buffer
	if err := json.Compact(&want, doc.Tools); err != nil {
		t.Fatal(err)
	}
	if string(res) != `{"tools":`+want.String()+`}` {
		t.Fatalf("tools/list is not the catalog verbatim:\n%s", res)
	}
	for _, m := range []string{"resources/list", "prompts/list", "logging/setLevel", "completion/complete", "server/discover", "tools/List"} {
		if _, rerr := e.rpc(m, nil); rerr == nil || rerr.Code != codeMethodNotFound || rerr.Message != "Method not found" {
			t.Errorf("%s = %+v", m, rerr)
		}
	}
}

func TestToolsCallParams(t *testing.T) {
	e := newTenv(t)
	for _, params := range []any{nil, []any{}, map[string]any{}, map[string]any{"name": 5}, map[string]any{"name": "backyard_whoami", "arguments": []any{}},
		map[string]any{"name": "backyard_whoami", "arguments": "x"}, map[string]any{"name": "backyard_whoami", "arguments": nil}} {
		if _, rerr := e.rpc("tools/call", params); rerr == nil || rerr.Code != codeInvalidParams || rerr.Message != "Invalid params" {
			t.Errorf("tools/call %v = %+v", params, rerr)
		}
	}
	if _, rerr := e.rpc("tools/call", map[string]any{"name": "nope"}); rerr == nil || rerr.Code != codeInvalidParams || rerr.Message != "Unknown tool: nope" {
		t.Fatalf("unknown tool = %+v", rerr)
	}
	// Absent arguments are {}.
	e.toolOK(aliceToken, "backyard_whoami", nil, nil)
	// Schema violations are tool errors.
	e.toolErr(aliceToken, "backyard_whoami", map[string]any{"x": 1}, "Invalid arguments: ")
	e.toolErr(aliceToken, "backyard_docs", map[string]any{"topic": "secrets"}, "Invalid arguments: ")
	e.toolErr(aliceToken, "backyard_site_files", map[string]any{"site": "Bad"}, "Invalid arguments: ")
	e.toolErr(aliceToken, "backyard_publish", map[string]any{"site": "a"}, "Invalid arguments: ")
	e.toolErr(aliceToken, "backyard_publish", map[string]any{"site": "a", "files": []any{map[string]any{"path": "a", "content": "x", "encoding": "hex"}}}, "Invalid arguments: ")
	long := e.toolErr(aliceToken, "backyard_site_files", map[string]any{"site": strings.Repeat("A", 100_000)}, "Invalid arguments: ")
	if len(long) > maxDetail+100 {
		t.Fatalf("the detail is not bounded: %d bytes", len(long))
	}
}

func TestPanicIsInternalError(t *testing.T) {
	e := newTenv(t)
	e.h.tools["backyard_whoami"] = func(context.Context, *call, json.RawMessage) toolResult { panic("boom") }
	_, rerr := e.rpc("tools/call", map[string]any{"name": "backyard_whoami"})
	if rerr == nil || rerr.Code != codeInternalError || !strings.HasPrefix(rerr.Message, "Internal error (request ") {
		t.Fatalf("panic = %+v", rerr)
	}
}

func TestRequestIDFromMiddleware(t *testing.T) {
	e := newTenv(t)
	e.h.tools["backyard_whoami"] = func(context.Context, *call, json.RawMessage) toolResult { panic("boom") }
	w := httptest.NewRecorder()
	w.Header().Set("X-Request-Id", "req-42")
	e.h.ServeHTTP(w, request(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"backyard_whoami"}}`))
	if !strings.Contains(w.Body.String(), "Internal error (request req-42)") {
		t.Fatalf("body = %s", w.Body)
	}
}

// TestRealAuthServer runs the gate against the authorization server of
// internal/auth (the challenges and origins come from it).
func TestRealAuthServer(t *testing.T) {
	cfg := newTestConfig(t)
	cfg.Auth.Mode = auth.ModeNone
	cfg.Server.PublicURL = "http://127.0.0.1:8080"
	cfg.MCP.Enabled = true
	cfg.ApplyDefaults()
	a, err := auth.New(context.Background(), cfg, auth.Options{Docs: memory.New()})
	if err != nil {
		t.Fatal(err)
	}
	m := auth.MCPOf(a)
	if m == nil {
		t.Fatal("MCPOf = nil with mcp.enabled")
	}
	h := New(Options{Config: cfg, Auth: m})
	serve := func(hdr ...string) *httptest.ResponseRecorder {
		r := request(initialize, hdr...)
		r.TLS, r.Host = nil, "127.0.0.1:8080"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	prm := "http://127.0.0.1:8080/.well-known/oauth-protected-resource/__backyard/mcp"
	w := serve("Authorization", "")
	if w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") != `Bearer resource_metadata="`+prm+`", scope="mcp"` {
		t.Fatalf("missing token: %d %q", w.Code, w.Header().Get("WWW-Authenticate"))
	}
	w = serve("Authorization", "Bearer byat_garbage")
	if want := `Bearer error="invalid_token", error_description="malformed token", resource_metadata="` + prm + `", scope="mcp"`; w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") != want {
		t.Fatalf("garbage token: %d %q", w.Code, w.Header().Get("WWW-Authenticate"))
	}
	for origin, status := range map[string]int{"https://claude.ai": 401, "https://claude.com": 401, "http://127.0.0.1:8080": 401, "https://evil.example": 403} {
		if w := serve("Origin", origin, "Authorization", ""); w.Code != status {
			t.Errorf("Origin %s: %d", origin, w.Code)
		}
	}
}
