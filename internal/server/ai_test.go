package server

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/thasami/backyard/internal/ai"
	"github.com/thasami/backyard/internal/config"
)

const aiURL = "https://foo.backyard.test/__backyard/api/ai"

func TestAIChatJSON(t *testing.T) {
	h := newHarness(t)
	img := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nimg"))
	w := h.api("POST", aiURL+"/chat", map[string]any{
		"model":  "smart",
		"system": "Be brief.",
		"messages": []any{
			map[string]any{"role": "system", "content": "Answer in French."},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "What is this?"},
				map[string]any{"type": "image", "data": img, "media_type": "image/png"},
				map[string]any{"type": "image", "url": "https://img.example/a.png"},
				map[string]any{"type": "image", "data": "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString([]byte("jpg"))},
			}},
			map[string]any{"role": "assistant", "content": "Un logo."},
			map[string]any{"role": "user", "content": "Merci"},
		},
		"max_tokens":  100,
		"temperature": 0.2,
		"json":        true,
	})
	expectStatus(t, w, 200)
	resp := decode[map[string]any](t, w)
	if resp["content"] != "hello" || resp["model"] != "smart" || resp["stop_reason"] != "end_turn" {
		t.Fatalf("resp = %v", resp)
	}
	if u := resp["usage"].(map[string]any); u["input_tokens"] != float64(12) || u["output_tokens"] != float64(40) {
		t.Fatalf("usage = %v", u)
	}
	alias, req := h.ai.lastChat()
	if alias != "smart" || req.System != "Be brief.\n\nAnswer in French." || !req.JSON || req.MaxTokens != 100 ||
		req.Temperature == nil || *req.Temperature != 0.2 || req.User != "alice@acme.com" {
		t.Fatalf("req = %+v alias=%q", req, alias)
	}
	if len(req.Messages) != 3 || req.Messages[0].Role != "user" || len(req.Messages[0].Content) != 4 {
		t.Fatalf("messages = %+v", req.Messages)
	}
	parts := req.Messages[0].Content
	if string(parts[1].Data) != "\x89PNG\r\n\x1a\nimg" || parts[1].MediaType != "image/png" {
		t.Fatalf("image part = %+v", parts[1])
	}
	if parts[2].URL != "https://img.example/a.png" || parts[2].Data != nil {
		t.Fatalf("url part = %+v", parts[2])
	}
	if string(parts[3].Data) != "jpg" || parts[3].MediaType != "image/jpeg" {
		t.Fatalf("data url part = %+v", parts[3])
	}
	// Token usage counts against the daily budget.
	if used := h.srv.limiter.TokensUsed("u:alice@acme.com"); used != 52 {
		t.Fatalf("tokens used = %d", used)
	}
}

func TestAIChatValidationAndErrors(t *testing.T) {
	h := newHarness(t, withConfig(func(c *config.Config) {
		c.AI.Models = map[string]config.ModelConfig{"smart": {Provider: "p", Model: "m"}}
	}))
	msgs := []any{map[string]any{"role": "user", "content": "hi"}}
	tests := []struct {
		name   string
		body   any
		err    error
		status int
		code   string
	}{
		{"no messages", map[string]any{"messages": []any{}}, nil, 400, "bad_request"},
		{"bad role", map[string]any{"messages": []any{map[string]any{"role": "tool", "content": "x"}}}, nil, 400, "bad_request"},
		{"only system", map[string]any{"messages": []any{map[string]any{"role": "system", "content": "x"}}}, nil, 400, "bad_request"},
		{"bad part type", map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "audio"}}}}}, nil, 400, "bad_request"},
		{"bad base64", map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "image", "data": "!!!"}}}}}, nil, 400, "bad_request"},
		{"numeric content", map[string]any{"messages": []any{map[string]any{"role": "user", "content": 5}}}, nil, 400, "bad_request"},
		{"negative max tokens", map[string]any{"messages": msgs, "max_tokens": -1}, nil, 400, "bad_request"},
		{"unknown model", map[string]any{"messages": msgs, "model": "nope"}, nil, 404, "not_found"},
		{"raw model disallowed", map[string]any{"messages": msgs, "model": "openai/gpt"}, nil, 404, "not_found"},
		{"provider bad request", map[string]any{"messages": msgs}, fmt.Errorf("%w: image too large", ai.ErrBadRequest), 400, "bad_request"},
		{"unsupported", map[string]any{"messages": msgs}, ai.ErrUnsupported, 501, "unsupported"},
		{"unknown model from router", map[string]any{"messages": msgs}, ai.ErrUnknownModel, 404, "not_found"},
		{"upstream", map[string]any{"messages": msgs}, &ai.UpstreamError{Provider: "anthropic", Status: 529, Message: "overloaded"}, 502, "upstream_error"},
		{"timeout", map[string]any{"messages": msgs}, context.DeadlineExceeded, 504, "upstream_error"},
		{"other", map[string]any{"messages": msgs}, fmt.Errorf("dial tcp: refused"), 502, "upstream_error"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h.ai.chatErr = tc.err
			w := h.api("POST", aiURL+"/chat", tc.body)
			e := expectError(t, w, tc.status, tc.code)
			if tc.name == "upstream" && !strings.Contains(e.Error.Message, "overloaded") {
				t.Fatalf("message = %q", e.Error.Message)
			}
		})
	}
}

type sseEvent map[string]any

func readSSE(t *testing.T, body string) []sseEvent {
	t.Helper()
	var out []sseEvent
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := sc.Text()
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var ev sseEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			t.Fatalf("bad event %q: %v", data, err)
		}
		out = append(out, ev)
	}
	return out
}

func TestAIChatStream(t *testing.T) {
	h := newHarness(t)
	srv := httptest.NewServer(h.h)
	defer srv.Close()

	do := func(body string) *http.Response {
		t.Helper()
		r, _ := http.NewRequest("POST", srv.URL+"/__backyard/api/ai/chat", strings.NewReader(body))
		r.Host = "foo.backyard.test"
		r.Header.Set("X-Backyard", "1")
		r.Header.Set("Content-Type", "application/json")
		resp, err := srv.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp := do(`{"messages":[{"role":"user","content":"hi"}],"stream":true}`)
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" || resp.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("status %d headers %v", resp.StatusCode, resp.Header)
	}
	var sb strings.Builder
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		sb.WriteString(sc.Text() + "\n")
	}
	evs := readSSE(t, sb.String())
	if len(evs) != 4 {
		t.Fatalf("events = %v", evs)
	}
	var text strings.Builder
	for _, ev := range evs[:3] {
		if ev["type"] != "text" {
			t.Fatalf("event = %v", ev)
		}
		text.WriteString(ev["text"].(string))
	}
	if text.String() != "Hello world" {
		t.Fatalf("text = %q", text.String())
	}
	done := evs[3]
	if done["type"] != "done" || done["model"] != "smart" || done["stop_reason"] != "end_turn" {
		t.Fatalf("done = %v", done)
	}
	if u := done["usage"].(map[string]any); u["output_tokens"] != float64(7) {
		t.Fatalf("usage = %v", u)
	}

	// Mid-stream failure → error event.
	h.ai.streamErr = &ai.UpstreamError{Provider: "openai", Message: "boom"}
	resp2 := do(`{"messages":[{"role":"user","content":"hi"}],"stream":true}`)
	defer resp2.Body.Close()
	sb.Reset()
	sc = bufio.NewScanner(resp2.Body)
	for sc.Scan() {
		sb.WriteString(sc.Text() + "\n")
	}
	evs = readSSE(t, sb.String())
	last := evs[len(evs)-1]
	if last["type"] != "error" || last["error"].(map[string]any)["code"] != "upstream_error" {
		t.Fatalf("last event = %v", last)
	}
	// Unknown model fails before the stream starts.
	resp3 := do(`{"messages":[{"role":"user","content":"hi"}],"stream":true,"model":"nope"}`)
	defer resp3.Body.Close()
	if resp3.StatusCode != 404 || !strings.HasPrefix(resp3.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("unknown model stream: %d %s", resp3.StatusCode, resp3.Header.Get("Content-Type"))
	}
}

func TestAIChatStreamHeartbeat(t *testing.T) {
	old := sseHeartbeat
	sseHeartbeat = 10 * time.Millisecond
	t.Cleanup(func() { sseHeartbeat = old })
	h := newHarness(t)
	h.ai.delay = 80 * time.Millisecond
	srv := httptest.NewServer(h.h)
	defer srv.Close()
	r, _ := http.NewRequest("POST", srv.URL+"/__backyard/api/ai/chat", strings.NewReader(`{"messages":[{"role":"user","content":"hi"}],"stream":true}`))
	r.Host = "foo.backyard.test"
	r.Header.Set("X-Backyard", "1")
	resp, err := srv.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	body := string(b)
	if !strings.Contains(body, ": ping\n\n") {
		t.Fatalf("no heartbeat in %q", body)
	}
	evs := readSSE(t, body)
	if len(evs) != 4 || evs[3]["type"] != "done" {
		t.Fatalf("events = %v", evs)
	}
}

func TestAIImageStoredAsFiles(t *testing.T) {
	h := newHarness(t)
	w := h.api("POST", aiURL+"/image", map[string]any{"prompt": "a cat", "n": 2, "size": "1024x1024"})
	expectStatus(t, w, 200)
	var resp struct {
		Model  string `json:"model"`
		Images []struct {
			ID          string `json:"id"`
			URL         string `json:"url"`
			ContentType string `json:"content_type"`
		} `json:"images"`
	}
	mustJSON(t, w.Body.Bytes(), &resp)
	if resp.Model != "image" || len(resp.Images) != 2 {
		t.Fatalf("resp = %+v", resp)
	}
	for i, img := range resp.Images {
		if img.ContentType != "image/png" || !strings.HasPrefix(img.URL, "/__backyard/files/"+img.ID+"/image-") || !strings.HasSuffix(img.URL, ".png") {
			t.Fatalf("image %d = %+v", i, img)
		}
		g := h.get("https://foo.backyard.test" + img.URL)
		expectStatus(t, g, 200)
		if g.Header().Get("Content-Type") != "image/png" || !strings.HasPrefix(g.Body.String(), "\x89PNG") {
			t.Fatalf("served image: %v %q", g.Header(), g.Body.String())
		}
	}
	// They are regular site files.
	w = h.api("GET", filesURL, nil)
	if !strings.Contains(w.Body.String(), resp.Images[0].ID) {
		t.Fatalf("files = %s", w.Body.String())
	}
	w = h.api("POST", aiURL+"/image", map[string]any{"prompt": "  "})
	expectError(t, w, 400, "bad_request")
	h.ai.imageErr = ai.ErrUnsupported
	w = h.api("POST", aiURL+"/image", map[string]any{"prompt": "x", "model": "smart"})
	expectError(t, w, 501, "unsupported")
}

func TestAIEmbedAndModels(t *testing.T) {
	h := newHarness(t)
	w := h.api("POST", aiURL+"/embed", map[string]any{"input": "hello"})
	expectStatus(t, w, 200)
	var resp struct {
		Model      string      `json:"model"`
		Embeddings [][]float64 `json:"embeddings"`
	}
	mustJSON(t, w.Body.Bytes(), &resp)
	if resp.Model != "embed" || len(resp.Embeddings) != 1 {
		t.Fatalf("resp = %+v", resp)
	}
	w = h.api("POST", aiURL+"/embed", map[string]any{"input": []string{"a", "b", "c"}, "model": "embed"})
	mustJSON(t, w.Body.Bytes(), &resp)
	if len(resp.Embeddings) != 3 || resp.Embeddings[2][0] != 2 {
		t.Fatalf("resp = %+v", resp)
	}
	for _, bad := range []any{map[string]any{}, map[string]any{"input": 5}, map[string]any{"input": []any{}}, map[string]any{"input": []any{1}}} {
		w = h.api("POST", aiURL+"/embed", bad)
		expectError(t, w, 400, "bad_request")
	}

	w = h.api("GET", aiURL+"/models", nil)
	expectStatus(t, w, 200)
	m := decode[map[string]any](t, w)
	if m["default"] != "smart" || len(m["models"].([]any)) != 3 {
		t.Fatalf("models = %v", m)
	}
}

func TestAIDisabled(t *testing.T) {
	nilAI := newHarness(t, func(h *harness, d *Deps) { d.AI = nil })
	off := newHarness(t)
	off.ai.enabled = false
	for _, h := range []*harness{nilAI, off} {
		for _, tc := range []struct{ method, path string }{
			{"POST", "/chat"}, {"POST", "/image"}, {"POST", "/embed"}, {"GET", "/models"},
		} {
			w := h.api(tc.method, aiURL+tc.path, map[string]any{"messages": []any{}})
			expectError(t, w, http.StatusNotImplemented, "unsupported")
		}
	}
}

func TestAIRateLimits(t *testing.T) {
	h := newHarness(t, withConfig(func(c *config.Config) {
		c.RateLimit.AIPerMinute = 100
		c.RateLimit.AITokensPerDay = 60
	}))
	body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	w := h.api("POST", aiURL+"/chat", body) // 52 tokens
	expectStatus(t, w, 200)
	w = h.api("POST", aiURL+"/chat", body) // 104 ≥ 60 after this one
	expectStatus(t, w, 200)
	w = h.api("POST", aiURL+"/chat", body)
	expectError(t, w, http.StatusTooManyRequests, "rate_limited")
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("no Retry-After")
	}

	g := newHarness(t, withConfig(func(c *config.Config) { c.RateLimit.AIPerMinute = 1 }))
	expectStatus(t, g.api("GET", aiURL+"/models", nil), 200) // models is not an AI call
	expectStatus(t, g.api("POST", aiURL+"/chat", body), 200)
	expectError(t, g.api("POST", aiURL+"/chat", body), http.StatusTooManyRequests, "rate_limited")
}

// retryAfterErr is an upstream error that knows the provider's Retry-After.
type retryAfterErr struct {
	*ai.UpstreamError
	after time.Duration
}

func (e retryAfterErr) RetryAfter() time.Duration { return e.after }
func (e retryAfterErr) Unwrap() error             { return e.UpstreamError }

func TestAIUpstreamStatusMapping(t *testing.T) {
	h := newHarness(t, withConfig(func(c *config.Config) { c.RateLimit.AIPerMinute = 1000 }))
	msgs := []any{map[string]any{"role": "user", "content": "hi"}}
	up := func(status int, msg string) *ai.UpstreamError {
		return &ai.UpstreamError{Provider: "anthropic", Status: status, Message: msg}
	}
	tests := []struct {
		name       string
		err        error
		status     int
		code       string
		retryAfter string // expected Retry-After header ("" = none)
		contains   []string
		excludes   []string
	}{
		{"router timeout", up(504, "no response within 5m0s"), 504, "upstream_error", "", []string{"did not respond in time"}, nil},
		{"provider request timeout", up(408, "timeout"), 504, "upstream_error", "", nil, nil},
		{"throttled with retry-after", retryAfterErr{up(429, "slow down"), 20 * time.Second}, 429, "rate_limited", "20", nil, nil},
		{"throttled wrapped", fmt.Errorf("router: %w", retryAfterErr{up(429, "slow down"), 1500 * time.Millisecond}), 429, "rate_limited", "2", nil, nil},
		{"throttled zero retry-after with hint", retryAfterErr{up(429, "try again in 7s"), 0}, 429, "rate_limited", "7", nil, nil},
		{"throttled hint milliseconds", up(429, "Please try again in 350ms."), 429, "rate_limited", "1", nil, nil},
		{"throttled hint seconds", up(429, "Rate limit reached for gpt-4o in organization org-AbCdEf123456 on tokens per min. Please try again in 1.5s."),
			429, "rate_limited", "2", nil, []string{"org-AbCdEf123456"}},
		{"throttled azure hint", up(429, "Requests have exceeded token rate limit. Please retry after 6 seconds."), 429, "rate_limited", "6", nil, nil},
		{"throttled hint minutes", up(429, "Quota exceeded; try again in 2 minutes"), 429, "rate_limited", "120", nil, nil},
		{"throttled unknown", up(429, "Too many requests, please wait before trying again."), 503, "upstream_error", "", []string{"rate limiting"}, nil},
		{"throttled absurd hint", up(429, "retry after 999999 seconds"), 429, "rate_limited", "3600", nil, nil},
		{"prefill rejected", up(400, "This model does not support assistant message prefill. The conversation must end with a user message."),
			400, "bad_request", "", []string{"rejected the request", "does not support assistant message prefill"}, nil},
		{"unprocessable", up(422, "invalid image"), 400, "bad_request", "", []string{"invalid image"}, nil},
		{"too large", up(413, "request too large"), 413, "too_large", "", []string{"too large"}, nil},
		{"bad request leaks", up(400, "see https://internal.example:8443/v1/deploy/secret-deployment?api-version=1 for key sk-proj-abcdefghijklmnop\x00\nnext"),
			400, "bad_request", "", []string{"[url]", "[redacted]", "next"}, []string{"internal.example", "secret-deployment", "abcdefghijklmnop", "\x00", "\n"}},
		{"empty 400 message", up(400, ""), 400, "bad_request", "", []string{"bad request"}, nil},
		{"credentials", up(401, "invalid x-api-key"), 502, "upstream_error", "", []string{"invalid x-api-key"}, nil},
		{"model missing upstream", up(404, "model not found"), 502, "upstream_error", "", nil, nil},
		{"overloaded", up(529, "Overloaded"), 502, "upstream_error", "", []string{"Overloaded"}, nil},
		{"no status", up(0, "stream ended unexpectedly"), 502, "upstream_error", "", nil, nil},
		{"long message", up(500, strings.Repeat("é", 400)), 502, "upstream_error", "", []string{"…"}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h.ai.chatErr = tc.err
			w := h.api("POST", aiURL+"/chat", map[string]any{"messages": msgs})
			e := expectError(t, w, tc.status, tc.code)
			if got := w.Header().Get("Retry-After"); got != tc.retryAfter {
				t.Fatalf("Retry-After = %q, want %q", got, tc.retryAfter)
			}
			for _, s := range tc.contains {
				if !strings.Contains(e.Error.Message, s) {
					t.Fatalf("message %q does not contain %q", e.Error.Message, s)
				}
			}
			for _, s := range tc.excludes {
				if strings.Contains(e.Error.Message, s) {
					t.Fatalf("message %q contains %q", e.Error.Message, s)
				}
			}
			if len(e.Error.Message) > 400 || !utf8.ValidString(e.Error.Message) {
				t.Fatalf("message not bounded/valid: %d bytes", len(e.Error.Message))
			}
		})
	}

	// The same mapping applies to images and embeddings.
	h.ai.chatErr = nil
	h.ai.imageErr = up(400, "prompt rejected by safety system")
	expectError(t, h.api("POST", aiURL+"/image", map[string]any{"prompt": "x"}), 400, "bad_request")
	h.ai.embedErr = up(504, "no response within 1m0s")
	expectError(t, h.api("POST", aiURL+"/embed", map[string]any{"input": "x"}), 504, "upstream_error")
}

func TestAIUpstreamStatusInStream(t *testing.T) {
	h := newHarness(t)
	srv := httptest.NewServer(h.h)
	defer srv.Close()
	for _, tc := range []struct {
		err  error
		code string
		msg  string
	}{
		{&ai.UpstreamError{Provider: "anthropic", Status: 400, Message: "prefill not supported"}, "bad_request", "prefill not supported"},
		{&ai.UpstreamError{Provider: "anthropic", Status: 429, Message: "slow down"}, "upstream_error", "rate limiting"},
		{&ai.UpstreamError{Provider: "anthropic", Status: 504, Message: "no response within 10m0s"}, "upstream_error", "did not respond in time"},
	} {
		h.ai.streamErr = tc.err
		r, err := http.NewRequest("POST", srv.URL+"/__backyard/api/ai/chat",
			strings.NewReader(`{"messages":[{"role":"user","content":"hi"}],"stream":true}`))
		if err != nil {
			t.Fatal(err)
		}
		r.Host = "foo.backyard.test"
		r.Header.Set("X-Backyard", "1")
		r.Header.Set("Content-Type", "application/json")
		resp, err := srv.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		evs := readSSE(t, string(body))
		if len(evs) == 0 {
			t.Fatalf("no events: %s", body)
		}
		last := evs[len(evs)-1]
		e, _ := last["error"].(map[string]any)
		if last["type"] != "error" || e["code"] != tc.code || !strings.Contains(fmt.Sprint(e["message"]), tc.msg) {
			t.Fatalf("last event = %v, want code %s with %q", last, tc.code, tc.msg)
		}
	}
}
