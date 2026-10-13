package anthropic

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/thasami/backyard/internal/ai"
	"github.com/thasami/backyard/internal/config"
)

var pngBytes = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89\x00\x00\x00\rIDATx\x9cc\xf8\x0f\x00\x00\x01\x01\x00\x05\x18\xd8N\x00\x00\x00\x00IEND\xaeB`\x82")

type captured struct {
	Path   string
	Query  string
	Header http.Header
	Body   map[string]any
}

type fake struct {
	mu   sync.Mutex
	reqs []captured
	srv  *httptest.Server
}

func newFake(t *testing.T, h func(w http.ResponseWriter, r *http.Request, body map[string]any)) *fake {
	t.Helper()
	f := &fake{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		f.mu.Lock()
		f.reqs = append(f.reqs, captured{r.URL.Path, r.URL.RawQuery, r.Header.Clone(), body})
		f.mu.Unlock()
		h(w, r, body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) last(t *testing.T) captured {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		t.Fatal("no request captured")
	}
	return f.reqs[len(f.reqs)-1]
}

func messageJSON(model, text, stop string) map[string]any {
	return map[string]any{
		"id": "msg_01", "type": "message", "role": "assistant", "model": model,
		"content":     []any{map[string]any{"type": "text", "text": text}},
		"stop_reason": stop, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": 10, "output_tokens": 5, "cache_creation_input_tokens": 1, "cache_read_input_tokens": 2},
	}
}

func replyJSON(v any) func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
	return func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
}

func zero() *int { z := 0; return &z }

func newFirstParty(t *testing.T, f *fake, pc config.ProviderConfig) *Provider {
	t.Helper()
	pc.APIKey = "sk-ant-test"
	pc.BaseURL = f.srv.URL
	p, err := New(context.Background(), "claude", pc, Options{MaxRetries: zero()})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func userText(s string) []ai.Message {
	return []ai.Message{{Role: "user", Content: []ai.Part{{Type: "text", Text: s}}}}
}

func TestChatCurrentModel(t *testing.T) {
	f := newFake(t, replyJSON(messageJSON("claude-opus-5-5", "Hello there", "end_turn")))
	p := newFirstParty(t, f, config.ProviderConfig{Effort: "high", Headers: map[string]string{"X-Team": "growth"}})
	resp, err := p.Chat(context.Background(), ai.ChatRequest{
		Model:       "claude-opus-5-5",
		System:      "You are helpful.",
		Messages:    userText("Hi"),
		MaxTokens:   1000,
		Temperature: ptr(0.3),
		User:        "u_abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "Hello there" || resp.StopReason != ai.StopEndTurn || resp.Model != "claude-opus-5-5" {
		t.Fatalf("resp = %+v", resp)
	}
	if resp.Usage != (ai.Usage{InputTokens: 13, OutputTokens: 5}) {
		t.Fatalf("usage = %+v", resp.Usage)
	}
	req := f.last(t)
	if req.Path != "/v1/messages" {
		t.Errorf("path = %s", req.Path)
	}
	if req.Header.Get("X-Api-Key") != "sk-ant-test" || req.Header.Get("Anthropic-Version") == "" || req.Header.Get("X-Team") != "growth" {
		t.Errorf("headers = %v", req.Header)
	}
	if !strings.Contains(req.Header.Get("Anthropic-Beta"), "server-side-fallback-2026-07-01") {
		t.Errorf("fallback beta header missing: %q", req.Header.Get("Anthropic-Beta"))
	}
	b := req.Body
	if b["fallbacks"] != "default" {
		t.Errorf("fallbacks = %v", b["fallbacks"])
	}
	if _, ok := b["temperature"]; ok {
		t.Errorf("temperature must not be sent to claude-opus-5-5")
	}
	if _, ok := b["thinking"]; ok {
		t.Errorf("thinking must be left unset")
	}
	if oc, _ := b["output_config"].(map[string]any); oc["effort"] != "high" {
		t.Errorf("output_config = %v", b["output_config"])
	}
	if b["max_tokens"] != float64(1000) || b["model"] != "claude-opus-5-5" {
		t.Errorf("body = %v", b)
	}
	if md, _ := b["metadata"].(map[string]any); md["user_id"] != "u_abc" {
		t.Errorf("metadata = %v", b["metadata"])
	}
	sys := b["system"].([]any)[0].(map[string]any)
	if sys["text"] != "You are helpful." {
		t.Errorf("system = %v", sys)
	}
}

func TestChatLegacyModelSampling(t *testing.T) {
	f := newFake(t, replyJSON(messageJSON("claude-sonnet-4-5", "ok", "max_tokens")))
	p := newFirstParty(t, f, config.ProviderConfig{Effort: "max"})
	resp, err := p.Chat(context.Background(), ai.ChatRequest{
		Model: "claude-sonnet-4-5", Messages: userText("x"), MaxTokens: 10, Temperature: ptr(1.7),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != ai.StopMaxTokens {
		t.Errorf("stop = %s", resp.StopReason)
	}
	req := f.last(t)
	if req.Body["temperature"] != float64(1) {
		t.Errorf("temperature should be clamped to 1: %v", req.Body["temperature"])
	}
	if _, ok := req.Body["fallbacks"]; ok {
		t.Errorf("no fallbacks for sonnet 4.5")
	}
	if _, ok := req.Body["output_config"]; ok {
		t.Errorf("sonnet 4.5 does not take effort: %v", req.Body["output_config"])
	}
	if strings.Contains(req.Header.Get("Anthropic-Beta"), "fallback") {
		t.Errorf("unexpected beta header %q", req.Header.Get("Anthropic-Beta"))
	}
}

func TestRequestEffortAndFallbackOptOut(t *testing.T) {
	f := newFake(t, replyJSON(messageJSON("claude-sonnet-5-5", "ok", "end_turn")))
	p := newFirstParty(t, f, config.ProviderConfig{RefusalFallback: ptr(false), Effort: "low"})
	if _, err := p.Chat(context.Background(), ai.ChatRequest{Model: "claude-sonnet-5-5", Messages: userText("x"), Effort: "xhigh"}); err != nil {
		t.Fatal(err)
	}
	b := f.last(t).Body
	if _, ok := b["fallbacks"]; ok {
		t.Errorf("refusal_fallback=false must disable fallbacks")
	}
	if oc, _ := b["output_config"].(map[string]any); oc["effort"] != "xhigh" {
		t.Errorf("request effort should win over provider default: %v", b["output_config"])
	}
	if b["max_tokens"] != float64(defaultMaxTokens) {
		t.Errorf("default max_tokens = %v", b["max_tokens"])
	}
}

func TestImagesAndJSONMode(t *testing.T) {
	f := newFake(t, replyJSON(messageJSON("claude-opus-5-5", "```json\n{\"animal\":\"cat\"}\n```", "end_turn")))
	p := newFirstParty(t, f, config.ProviderConfig{})
	resp, err := p.Chat(context.Background(), ai.ChatRequest{
		Model: "claude-opus-5-5", JSON: true,
		Messages: []ai.Message{
			{Role: "user", Content: []ai.Part{
				{Type: "image", Data: pngBytes, MediaType: "image/png"},
				{Type: "image", URL: "https://example.com/cat.jpg"},
				{Type: "image", URL: "data:image/gif;base64,R0lGODlh"},
				{Type: "image", Data: pngBytes}, // media type sniffed
				{Type: "text", Text: "What animal?"},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != `{"animal":"cat"}` {
		t.Errorf("content = %q", resp.Content)
	}
	b := f.last(t).Body
	sys := b["system"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(sys, "JSON") {
		t.Errorf("JSON instruction missing: %q", sys)
	}
	content := b["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(content) != 5 {
		t.Fatalf("content = %v", content)
	}
	src0 := content[0].(map[string]any)["source"].(map[string]any)
	if src0["type"] != "base64" || src0["media_type"] != "image/png" || src0["data"] != base64.StdEncoding.EncodeToString(pngBytes) {
		t.Errorf("base64 source = %v", src0)
	}
	src1 := content[1].(map[string]any)["source"].(map[string]any)
	if src1["type"] != "url" || src1["url"] != "https://example.com/cat.jpg" {
		t.Errorf("url source = %v", src1)
	}
	src2 := content[2].(map[string]any)["source"].(map[string]any)
	if src2["type"] != "base64" || src2["media_type"] != "image/gif" {
		t.Errorf("data url source = %v", src2)
	}
	if src3 := content[3].(map[string]any)["source"].(map[string]any); src3["media_type"] != "image/png" {
		t.Errorf("sniffed source = %v", src3)
	}
}

func TestBadRequests(t *testing.T) {
	f := newFake(t, replyJSON(messageJSON("m", "x", "end_turn")))
	p := newFirstParty(t, f, config.ProviderConfig{})
	ctx := context.Background()
	tests := []ai.ChatRequest{
		{Messages: userText("x")}, // no model
		{Model: "claude-opus-5-5"},
		{Model: "claude-opus-5-5", Messages: []ai.Message{{Role: "system", Content: []ai.Part{{Type: "text", Text: "x"}}}}},
		{Model: "claude-opus-5-5", Messages: []ai.Message{{Role: "user", Content: []ai.Part{{Type: "text", Text: ""}}}}},
		{Model: "claude-opus-5-5", Messages: []ai.Message{{Role: "user", Content: []ai.Part{{Type: "audio"}}}}},
		{Model: "claude-opus-5-5", Messages: []ai.Message{{Role: "user", Content: []ai.Part{{Type: "image"}}}}},
		{Model: "claude-opus-5-5", Messages: []ai.Message{{Role: "user", Content: []ai.Part{{Type: "image", Data: []byte("%PDF-1.4"), MediaType: "application/pdf"}}}}},
		{Model: "claude-opus-5-5", Messages: []ai.Message{{Role: "user", Content: []ai.Part{{Type: "image", URL: "data:image/png;base64,***"}}}}},
	}
	for i, req := range tests {
		if _, err := p.Chat(ctx, req); !errors.Is(err, ai.ErrBadRequest) {
			t.Errorf("case %d: want ErrBadRequest, got %v", i, err)
		}
	}
	if _, err := p.Image(ctx, ai.ImageRequest{}); !errors.Is(err, ai.ErrUnsupported) {
		t.Errorf("Image: %v", err)
	}
	if _, err := p.Embed(ctx, ai.EmbedRequest{}); !errors.Is(err, ai.ErrUnsupported) {
		t.Errorf("Embed: %v", err)
	}
}

func TestNewValidation(t *testing.T) {
	ctx := context.Background()
	if _, err := New(ctx, "x", config.ProviderConfig{Platform: "azure"}, Options{}); err == nil {
		t.Error("unknown platform should fail")
	}
	if _, err := New(ctx, "x", config.ProviderConfig{Effort: "extreme"}, Options{}); err == nil {
		t.Error("invalid effort should fail")
	}
	p, err := New(ctx, "x", config.ProviderConfig{Platform: "vertex"}, Options{})
	if err != nil || p.fallbacks {
		t.Errorf("vertex: err=%v fallbacks=%v (fallbacks are first-party only)", err, p != nil && p.fallbacks)
	}
}

func TestUpstreamErrors(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"Number of request tokens has exceeded your per-minute rate limit"},"request_id":"req_1"}`))
	})
	p := newFirstParty(t, f, config.ProviderConfig{})
	_, err := p.Chat(context.Background(), ai.ChatRequest{Model: "claude-opus-5-5", Messages: userText("x")})
	var ue *ai.UpstreamError
	if !errors.As(err, &ue) || ue.Status != 429 || ue.Provider != "claude" || !strings.Contains(ue.Message, "rate limit") {
		t.Fatalf("got %#v", err)
	}
	if strings.Contains(ue.Error(), "sk-ant-test") {
		t.Fatal("error leaks the API key")
	}
	if ue.RetryAfter() != 0 {
		t.Fatalf("RetryAfter = %v without retry headers", ue.RetryAfter())
	}
}

// TestUpstreamErrorRetryAfter: the provider's Retry-After (or, on a 429,
// its rate-limit reset headers) reaches the *ai.UpstreamError, so the HTTP
// layer can answer 429 with a Retry-After of its own.
func TestUpstreamErrorRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		header map[string]string
		want   time.Duration
	}{
		{"retry-after", 429, map[string]string{"Retry-After": "17"}, 17 * time.Second},
		{"retry-after-ms", 429, map[string]string{"Retry-After-Ms": "2500", "Retry-After": "3"}, 2500 * time.Millisecond},
		{"overloaded retry-after", 529, map[string]string{"Retry-After": "4"}, 4 * time.Second},
		{"reset headers", 429, map[string]string{
			"Anthropic-Ratelimit-Requests-Remaining": "0",
			"Anthropic-Ratelimit-Requests-Reset":     time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		}, time.Hour},
		{"reset headers ignored for 400", 400, map[string]string{
			"Anthropic-Ratelimit-Requests-Remaining": "0",
			"Anthropic-Ratelimit-Requests-Reset":     time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
				for k, v := range tc.header {
					w.Header().Set(k, v)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
			})
			p := newFirstParty(t, f, config.ProviderConfig{})
			_, err := p.Chat(context.Background(), ai.ChatRequest{Model: "claude-opus-5-5", Messages: userText("x")})
			var ue *ai.UpstreamError
			if !errors.As(err, &ue) || ue.Status != tc.status {
				t.Fatalf("got %#v", err)
			}
			// RFC 3339 has second precision: allow for the clock moving on.
			if got := ue.RetryAfter(); got > tc.want || got < tc.want-2*time.Second {
				t.Fatalf("RetryAfter = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestContextCancel(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		<-r.Context().Done()
	})
	p := newFirstParty(t, f, config.ProviderConfig{})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := p.Chat(ctx, ai.ChatRequest{Model: "claude-opus-5-5", Messages: userText("x")})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
}

// sse writes Anthropic-style named events.
func sse(w http.ResponseWriter, events ...[2]string) {
	w.Header().Set("Content-Type", "text/event-stream")
	fl := w.(http.Flusher)
	for _, e := range events {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e[0], e[1])
		fl.Flush()
	}
}

func streamEvents(model string, texts []string, stop string) [][2]string {
	ev := [][2]string{
		{"message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"` + model + `","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":21,"output_tokens":1}}}`},
		{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
		{"ping", `{"type":"ping"}`},
	}
	for _, t := range texts {
		b, _ := json.Marshal(t)
		ev = append(ev, [2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":` + string(b) + `}}`})
	}
	ev = append(ev,
		[2]string{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		[2]string{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"` + stop + `","stop_sequence":null},"usage":{"output_tokens":9}}`},
		[2]string{"message_stop", `{"type":"message_stop"}`},
	)
	return ev
}

func TestChatStream(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		if body["stream"] != true {
			http.Error(w, "expected stream", 400)
			return
		}
		sse(w, streamEvents("claude-opus-5-5", []string{"Hel", "lo", "!"}, "end_turn")...)
	})
	p := newFirstParty(t, f, config.ProviderConfig{})
	var deltas []string
	resp, err := p.ChatStream(context.Background(), ai.ChatRequest{Model: "claude-opus-5-5", Messages: userText("hi"), MaxTokens: 64000},
		func(s string) error { deltas = append(deltas, s); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(deltas, "|") != "Hel|lo|!" || resp.Content != "Hello!" || resp.StopReason != ai.StopEndTurn {
		t.Fatalf("deltas=%v resp=%+v", deltas, resp)
	}
	if resp.Usage != (ai.Usage{InputTokens: 21, OutputTokens: 9}) {
		t.Fatalf("usage = %+v", resp.Usage)
	}
}

func TestChatStreamFallback(t *testing.T) {
	// Mid-stream refusal by the requested model, continued by the fallback
	// model on the same stream (server-side fallbacks).
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		sse(w,
			[2]string{"message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":30,"output_tokens":1}}}`},
			[2]string{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
			[2]string{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Part "}}`},
			[2]string{"content_block_stop", `{"type":"content_block_stop","index":0}`},
			[2]string{"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"fallback","from":{"model":"claude-opus-5-5"},"to":{"model":"claude-opus-4-8"}}}`},
			[2]string{"content_block_stop", `{"type":"content_block_stop","index":1}`},
			[2]string{"content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}`},
			[2]string{"content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"two."}}`},
			[2]string{"content_block_stop", `{"type":"content_block_stop","index":2}`},
			[2]string{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":4,"iterations":[{"type":"message","model":"claude-opus-5-5","input_tokens":30,"output_tokens":2},{"type":"fallback_message","model":"claude-opus-4-8","input_tokens":32,"output_tokens":4}]}}`},
			[2]string{"message_stop", `{"type":"message_stop"}`},
		)
	})
	p := newFirstParty(t, f, config.ProviderConfig{})
	var deltas []string
	resp, err := p.ChatStream(context.Background(), ai.ChatRequest{Model: "claude-opus-5-5", Messages: userText("hi")},
		func(s string) error { deltas = append(deltas, s); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "Part two." || strings.Join(deltas, "") != "Part two." {
		t.Fatalf("resp=%+v deltas=%v", resp, deltas)
	}
	if resp.Usage != (ai.Usage{InputTokens: 62, OutputTokens: 6}) {
		t.Fatalf("usage should total all attempts: %+v", resp.Usage)
	}
	if b := f.last(t).Body; b["fallbacks"] != "default" {
		t.Errorf("fallbacks = %v", b["fallbacks"])
	}
}

func TestChatStreamRefusalAndErrors(t *testing.T) {
	ctx := context.Background()
	t.Run("refusal", func(t *testing.T) {
		f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
			sse(w, streamEvents("claude-fable-5-1", nil, "refusal")...)
		})
		p := newFirstParty(t, f, config.ProviderConfig{})
		resp, err := p.ChatStream(ctx, ai.ChatRequest{Model: "claude-fable-5-1", Messages: userText("x")}, nil)
		if err != nil || resp.StopReason != ai.StopRefusal || resp.Content != "" {
			t.Fatalf("resp=%+v err=%v", resp, err)
		}
	})
	t.Run("error event", func(t *testing.T) {
		f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
			ev := streamEvents("claude-opus-5-5", []string{"a"}, "end_turn")[:4]
			ev = append(ev, [2]string{"error", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`})
			sse(w, ev...)
		})
		p := newFirstParty(t, f, config.ProviderConfig{})
		_, err := p.ChatStream(ctx, ai.ChatRequest{Model: "claude-opus-5-5", Messages: userText("x")}, func(string) error { return nil })
		var ue *ai.UpstreamError
		if !errors.As(err, &ue) || ue.Status != 529 || ue.Message != "Overloaded" {
			t.Fatalf("got %#v", err)
		}
	})
	t.Run("rate limit event ignores reset headers of the 200 stream", func(t *testing.T) {
		f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
			// Reset headers describe every response; on a successful
			// stream they say nothing about when to retry.
			w.Header().Set("Anthropic-Ratelimit-Requests-Remaining", "0")
			w.Header().Set("Anthropic-Ratelimit-Requests-Reset", time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
			ev := streamEvents("claude-opus-5-5", []string{"a"}, "end_turn")[:4]
			ev = append(ev, [2]string{"error", `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`})
			sse(w, ev...)
		})
		p := newFirstParty(t, f, config.ProviderConfig{})
		_, err := p.ChatStream(ctx, ai.ChatRequest{Model: "claude-opus-5-5", Messages: userText("x")}, func(string) error { return nil })
		var ue *ai.UpstreamError
		if !errors.As(err, &ue) || ue.Status != 429 || ue.RetryAfter() != 0 {
			t.Fatalf("got %#v", err)
		}
	})
	t.Run("truncated", func(t *testing.T) {
		f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
			sse(w, streamEvents("claude-opus-5-5", []string{"a"}, "end_turn")[:4]...)
		})
		p := newFirstParty(t, f, config.ProviderConfig{})
		_, err := p.ChatStream(ctx, ai.ChatRequest{Model: "claude-opus-5-5", Messages: userText("x")}, func(string) error { return nil })
		var ue *ai.UpstreamError
		if !errors.As(err, &ue) || !strings.Contains(ue.Message, "unexpectedly") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("callback abort", func(t *testing.T) {
		f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
			sse(w, streamEvents("claude-opus-5-5", []string{"a", "b"}, "end_turn")...)
		})
		p := newFirstParty(t, f, config.ProviderConfig{})
		stop := errors.New("stop")
		_, err := p.ChatStream(ctx, ai.ChatRequest{Model: "claude-opus-5-5", Messages: userText("x")}, func(string) error { return stop })
		if !errors.Is(err, stop) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("http error", func(t *testing.T) {
		f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"temperature: not supported"}}`))
		})
		p := newFirstParty(t, f, config.ProviderConfig{})
		_, err := p.ChatStream(ctx, ai.ChatRequest{Model: "claude-opus-5-5", Messages: userText("x")}, func(string) error { return nil })
		var ue *ai.UpstreamError
		if !errors.As(err, &ue) || ue.Status != 400 || ue.Message != "temperature: not supported" {
			t.Fatalf("got %#v", err)
		}
	})
}

func TestStopReasonMapping(t *testing.T) {
	tests := map[string]string{
		"end_turn": ai.StopEndTurn, "max_tokens": ai.StopMaxTokens, "stop_sequence": ai.StopStopSequence,
		"refusal": ai.StopRefusal, "tool_use": ai.StopOther, "pause_turn": ai.StopOther,
		"model_context_window_exceeded": ai.StopMaxTokens, "": ai.StopOther,
	}
	for in, want := range tests {
		f := newFake(t, replyJSON(messageJSON("claude-opus-5-5", "x", in)))
		p := newFirstParty(t, f, config.ProviderConfig{})
		resp, err := p.Chat(context.Background(), ai.ChatRequest{Model: "claude-opus-5-5", Messages: userText("x")})
		if err != nil {
			t.Fatal(err)
		}
		if resp.StopReason != want {
			t.Errorf("%q -> %q, want %q", in, resp.StopReason, want)
		}
	}
}

func TestVertex(t *testing.T) {
	f := newFake(t, replyJSON(messageJSON("claude-opus-5-5", "from vertex", "end_turn")))
	creds := &google.Credentials{
		ProjectID:   "adc-project",
		TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "ya29.test", Expiry: time.Now().Add(time.Hour)}),
	}
	var fetched string
	p, err := New(context.Background(), "vtx", config.ProviderConfig{
		Type: "anthropic", Platform: "vertex", ProjectID: "my-proj", BaseURL: f.srv.URL,
	}, Options{
		GoogleCredentials: creds, MaxRetries: zero(), HTTPClient: f.srv.Client(),
		FetchImage: func(ctx context.Context, u string) ([]byte, string, error) {
			fetched = u
			return pngBytes, "image/png", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := p.Chat(context.Background(), ai.ChatRequest{Model: "claude-opus-5-5", Messages: []ai.Message{{Role: "user", Content: []ai.Part{
		{Type: "text", Text: "hi"}, {Type: "image", URL: "https://example.com/x.png"},
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "from vertex" {
		t.Fatalf("resp = %+v", resp)
	}
	req := f.last(t)
	if req.Path != "/v1/projects/my-proj/locations/global/publishers/anthropic/models/claude-opus-5-5:rawPredict" {
		t.Errorf("path = %s", req.Path)
	}
	if req.Header.Get("Authorization") != "Bearer ya29.test" {
		t.Errorf("authorization = %q", req.Header.Get("Authorization"))
	}
	if req.Body["anthropic_version"] != "vertex-2023-10-16" {
		t.Errorf("anthropic_version = %v", req.Body["anthropic_version"])
	}
	if _, ok := req.Body["model"]; ok {
		t.Errorf("vertex body must not carry model")
	}
	if _, ok := req.Body["fallbacks"]; ok {
		t.Errorf("no server-side fallbacks on vertex")
	}
	if fetched != "https://example.com/x.png" {
		t.Errorf("URL image should be fetched and inlined on vertex, fetched=%q", fetched)
	}
	src := req.Body["messages"].([]any)[0].(map[string]any)["content"].([]any)[1].(map[string]any)["source"].(map[string]any)
	if src["type"] != "base64" {
		t.Errorf("source = %v", src)
	}

	// Streaming uses streamRawPredict.
	f2 := newFake(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		sse(w, streamEvents("claude-opus-5-5", []string{"v"}, "end_turn")...)
	})
	p2, _ := New(context.Background(), "vtx", config.ProviderConfig{Platform: "vertex", Location: "us-east5", BaseURL: f2.srv.URL},
		Options{GoogleCredentials: creds, MaxRetries: zero()})
	if _, err := p2.ChatStream(context.Background(), ai.ChatRequest{Model: "claude-sonnet-5-5", Messages: userText("x")}, nil); err != nil {
		t.Fatal(err)
	}
	if got := f2.last(t).Path; got != "/v1/projects/adc-project/locations/us-east5/publishers/anthropic/models/claude-sonnet-5-5:streamRawPredict" {
		t.Errorf("stream path = %s", got)
	}
}

func TestVertexMissingProject(t *testing.T) {
	creds := &google.Credentials{TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "x"})}
	p, err := New(context.Background(), "vtx", config.ProviderConfig{Platform: "vertex"}, Options{GoogleCredentials: creds})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Chat(context.Background(), ai.ChatRequest{Model: "claude-opus-5-5", Messages: userText("x")})
	var ue *ai.UpstreamError
	if !errors.As(err, &ue) || !strings.Contains(ue.Message, "project_id") {
		t.Fatalf("got %v", err)
	}
}

func TestBedrockMantle(t *testing.T) {
	f := newFake(t, replyJSON(messageJSON("anthropic.claude-opus-5-5", "from bedrock", "end_turn")))
	p, err := New(context.Background(), "br", config.ProviderConfig{
		Platform: "bedrock", Region: "us-west-2", APIKey: "bedrock-api-key", BaseURL: f.srv.URL + "/anthropic",
	}, Options{MaxRetries: zero(),
		FetchImage: func(context.Context, string) ([]byte, string, error) { return pngBytes, "image/png", nil }})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := p.Chat(context.Background(), ai.ChatRequest{Model: "anthropic.claude-opus-5-5", Messages: userText("hi"), Temperature: ptr(0.5)})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "from bedrock" {
		t.Fatalf("resp = %+v", resp)
	}
	req := f.last(t)
	if req.Path != "/anthropic/v1/messages" {
		t.Errorf("path = %s", req.Path)
	}
	auth := req.Header.Get("Authorization") + req.Header.Get("X-Api-Key")
	if !strings.Contains(auth, "bedrock-api-key") {
		t.Errorf("api key not sent: %v", req.Header)
	}
	if req.Body["model"] != "anthropic.claude-opus-5-5" {
		t.Errorf("model = %v", req.Body["model"])
	}
	if _, ok := req.Body["fallbacks"]; ok {
		t.Errorf("no server-side fallbacks on bedrock")
	}
	if _, ok := req.Body["temperature"]; ok {
		t.Errorf("no temperature for opus 5.5 on bedrock either")
	}
}

func ptr[T any](v T) *T { return &v }

func TestCloudflareAIGatewayBaseURL(t *testing.T) {
	f := newFake(t, replyJSON(messageJSON("claude-sonnet-5-5", "via gateway", "end_turn")))
	p, err := New(context.Background(), "claude-gw", config.ProviderConfig{
		APIKey:  "sk-ant-test",
		BaseURL: f.srv.URL + "/v1/acct/gw/anthropic",
		Headers: map[string]string{"cf-aig-authorization": "Bearer gw-token"},
	}, Options{MaxRetries: zero()})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := p.Chat(context.Background(), ai.ChatRequest{Model: "claude-sonnet-5-5", Messages: userText("hi")})
	if err != nil || resp.Content != "via gateway" {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	req := f.last(t)
	if req.Path != "/v1/acct/gw/anthropic/v1/messages" || req.Header.Get("Cf-Aig-Authorization") != "Bearer gw-token" {
		t.Errorf("req = %s %v", req.Path, req.Header)
	}
}

func TestNewWithoutAPIKeyUsesEnvironment(t *testing.T) {
	f := newFake(t, replyJSON(messageJSON("claude-opus-5-5", "env key", "end_turn")))
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-from-env")
	t.Setenv("ANTHROPIC_BASE_URL", f.srv.URL)
	p, err := New(context.Background(), "claude", config.ProviderConfig{}, Options{MaxRetries: zero()})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := p.Chat(context.Background(), ai.ChatRequest{Model: "claude-opus-5-5", Messages: userText("x")})
	if err != nil || resp.Content != "env key" {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	if got := f.last(t).Header.Get("X-Api-Key"); got != "sk-ant-from-env" {
		t.Errorf("x-api-key = %q", got)
	}
}

func TestOpus45EffortBeta(t *testing.T) {
	f := newFake(t, replyJSON(messageJSON("claude-opus-4-5", "ok", "end_turn")))
	p := newFirstParty(t, f, config.ProviderConfig{})
	if _, err := p.Chat(context.Background(), ai.ChatRequest{Model: "claude-opus-4-5", Messages: userText("x"), Effort: "max"}); err != nil {
		t.Fatal(err)
	}
	req := f.last(t)
	if oc, _ := req.Body["output_config"].(map[string]any); oc["effort"] != "high" {
		t.Errorf("Opus 4.5 effort should be clamped to high: %v", req.Body["output_config"])
	}
	if !strings.Contains(req.Header.Get("Anthropic-Beta"), "effort-2025-11-24") {
		t.Errorf("Opus 4.5 effort needs the effort beta, got %q", req.Header.Get("Anthropic-Beta"))
	}

	// No effort → no beta; newer models never get it.
	for _, tc := range []ai.ChatRequest{
		{Model: "claude-opus-4-5", Messages: userText("x")},
		{Model: "claude-opus-4-6", Messages: userText("x"), Effort: "high"},
	} {
		if _, err := p.Chat(context.Background(), tc); err != nil {
			t.Fatal(err)
		}
		if b := f.last(t).Header.Get("Anthropic-Beta"); strings.Contains(b, "effort-") {
			t.Errorf("%s (effort %q): unexpected effort beta %q", tc.Model, tc.Effort, b)
		}
	}
}

func TestVertexRejectsUnsafeModelIDs(t *testing.T) {
	f := newFake(t, replyJSON(messageJSON("claude-opus-5-5", "x", "end_turn")))
	creds := &google.Credentials{ProjectID: "p", TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "t", Expiry: time.Now().Add(time.Hour)})}
	p, err := New(context.Background(), "vtx", config.ProviderConfig{Platform: "vertex", BaseURL: f.srv.URL},
		Options{GoogleCredentials: creds, MaxRetries: zero()})
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range []string{
		"../../../projects/other/locations/us/publishers/google/models/gemini",
		"claude/../x", "claude?x=1", "claude#frag", "claude opus", "..",
	} {
		_, err := p.Chat(context.Background(), ai.ChatRequest{Model: model, Messages: userText("x")})
		if !errors.Is(err, ai.ErrBadRequest) {
			t.Errorf("model %q: err = %v, want ErrBadRequest", model, err)
		}
	}
	f.mu.Lock()
	n := len(f.reqs)
	f.mu.Unlock()
	if n != 0 {
		t.Fatalf("unsafe model ids reached the upstream (%d requests)", n)
	}
	if _, err := p.Chat(context.Background(), ai.ChatRequest{Model: "claude-opus-4-5@20251101", Messages: userText("x")}); err != nil {
		t.Fatalf("versioned Vertex id rejected: %v", err)
	}
}

func TestVertexLocationValidation(t *testing.T) {
	for _, loc := range []string{"evil.example.com/x", "us-east5.", "US", "a b", "-us"} {
		if _, err := New(context.Background(), "vtx", config.ProviderConfig{Platform: "vertex", Location: loc}, Options{}); err == nil {
			t.Errorf("location %q accepted", loc)
		}
	}
	for _, loc := range []string{"global", "us-east5", "europe-west1", "us", "eu"} {
		if _, err := New(context.Background(), "vtx", config.ProviderConfig{Platform: "vertex", Location: loc}, Options{}); err != nil {
			t.Errorf("location %q rejected: %v", loc, err)
		}
	}
}

func TestTransportErrorHidesURL(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL + "/secret-path"
	srv.Close() // connection refused
	p, err := New(context.Background(), "claude", config.ProviderConfig{APIKey: "k", BaseURL: base}, Options{MaxRetries: zero()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Chat(context.Background(), ai.ChatRequest{Model: "claude-opus-5-5", Messages: userText("x")})
	var ue *ai.UpstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(ue.Message, "secret-path") || strings.Contains(ue.Message, "127.0.0.1:") && strings.Contains(ue.Message, "http://") {
		t.Errorf("upstream error leaks the endpoint URL: %q", ue.Message)
	}
}

func TestWhitespaceTextBlocks(t *testing.T) {
	f := newFake(t, replyJSON(messageJSON("claude-3-7-sonnet-20250219", "ok", "end_turn")))
	p := newFirstParty(t, f, config.ProviderConfig{})
	_, err := p.Chat(context.Background(), ai.ChatRequest{Model: "claude-3-7-sonnet-20250219", Messages: []ai.Message{
		{Role: "user", Content: []ai.Part{{Type: "text", Text: "Hi"}, {Type: "text", Text: " \n"}}},
		{Role: "assistant", Content: []ai.Part{{Type: "text", Text: "Sure, here it is: \n"}, {Type: "text", Text: "  "}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	msgs := f.last(t).Body["messages"].([]any)
	if n := len(msgs[0].(map[string]any)["content"].([]any)); n != 1 {
		t.Errorf("whitespace-only block should be skipped, user blocks = %d", n)
	}
	if got := msgs[1].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]; got != "Sure, here it is:" {
		t.Errorf("prefill should be right-trimmed, got %q", got)
	}
	// A message with nothing but whitespace is a bad request.
	_, err = p.Chat(context.Background(), ai.ChatRequest{Model: "claude-opus-5-5", Messages: userText("  ")})
	if !errors.Is(err, ai.ErrBadRequest) {
		t.Fatalf("err = %v", err)
	}
}
