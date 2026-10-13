package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"

	"github.com/thasami/backyard/internal/ai"
	"github.com/thasami/backyard/internal/config"
)

var pngBytes = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89\x00\x00\x00\rIDATx\x9cc\xf8\x0f\x00\x00\x01\x01\x00\x05\x18\xd8N\x00\x00\x00\x00IEND\xaeB`\x82")

// recorder is a fake upstream that records requests and replies via handler.
type recorder struct {
	mu       sync.Mutex
	requests []recorded
	handler  func(w http.ResponseWriter, r *http.Request, body map[string]any)
}

type recorded struct {
	Method string
	Path   string
	Query  url.Values
	Header http.Header
	Body   map[string]any
}

func (rc *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(b, &body)
	rc.mu.Lock()
	rc.requests = append(rc.requests, recorded{r.Method, r.URL.Path, r.URL.Query(), r.Header.Clone(), body})
	rc.mu.Unlock()
	rc.handler(w, r, body)
}

func (rc *recorder) last(t *testing.T) recorded {
	t.Helper()
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if len(rc.requests) == 0 {
		t.Fatal("no requests recorded")
	}
	return rc.requests[len(rc.requests)-1]
}

func (rc *recorder) count() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return len(rc.requests)
}

func newServer(t *testing.T, h func(w http.ResponseWriter, r *http.Request, body map[string]any)) (*recorder, *httptest.Server) {
	t.Helper()
	rc := &recorder{handler: h}
	srv := httptest.NewServer(rc)
	t.Cleanup(srv.Close)
	return rc, srv
}

// rewrite sends every request to target, keeping path and query (lets tests
// use the real default base URLs such as https://api.openai.com/v1).
type rewrite struct{ target *url.URL }

func (rw rewrite) RoundTrip(r *http.Request) (*http.Response, error) {
	r2 := r.Clone(r.Context())
	r2.URL.Scheme = rw.target.Scheme
	r2.URL.Host = rw.target.Host
	r2.Host = rw.target.Host
	return http.DefaultTransport.RoundTrip(r2)
}

func rewriteClient(srv *httptest.Server) *http.Client {
	u, _ := url.Parse(srv.URL)
	return &http.Client{Transport: rewrite{u}}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func chatOK(content string, finish string) map[string]any {
	return map[string]any{
		"id": "chatcmpl-1", "object": "chat.completion", "model": "gpt-test-2026",
		"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": finish}},
		"usage":   map[string]any{"prompt_tokens": 11, "completion_tokens": 7, "total_tokens": 18},
	}
}

func mustNew(t *testing.T, pc config.ProviderConfig, opts Options) *Provider {
	t.Helper()
	p, err := New("p1", pc, opts)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func ptr[T any](v T) *T { return &v }

func TestNewValidation(t *testing.T) {
	tests := []struct {
		name string
		pc   config.ProviderConfig
		ok   bool
	}{
		{"openai default", config.ProviderConfig{Type: "openai", APIKey: "k"}, true},
		{"empty type", config.ProviderConfig{APIKey: "k"}, true},
		{"ollama default", config.ProviderConfig{Type: "ollama"}, true},
		{"azure key", config.ProviderConfig{Type: "azure-openai", Endpoint: "https://r.openai.azure.com", APIKey: "k"}, true},
		{"azure aad", config.ProviderConfig{Type: "azure-openai", Endpoint: "https://r.openai.azure.com", UseAzureAD: true}, true},
		{"azure no endpoint", config.ProviderConfig{Type: "azure-openai", APIKey: "k"}, false},
		{"azure no auth", config.ProviderConfig{Type: "azure-openai", Endpoint: "https://r.openai.azure.com"}, false},
		{"bad url", config.ProviderConfig{Type: "openai", BaseURL: "ftp://x"}, false},
		{"bad type", config.ProviderConfig{Type: "gemini"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New("x", tt.pc, Options{})
			if (err == nil) != tt.ok {
				t.Fatalf("err=%v, want ok=%v", err, tt.ok)
			}
		})
	}
}

func TestChatOpenAIDefaults(t *testing.T) {
	rc, srv := newServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		writeJSON(w, 200, chatOK("Hello!", "stop"))
	})
	p := mustNew(t, config.ProviderConfig{
		Type: "openai", APIKey: "sk-test", Organization: "org-1", OpenAIProject: "proj-1",
		Headers: map[string]string{"X-Extra": "yes"},
	}, Options{HTTPClient: rewriteClient(srv)})

	resp, err := p.Chat(context.Background(), ai.ChatRequest{
		Model:       "gpt-5",
		System:      "Be terse.",
		Messages:    []ai.Message{{Role: "user", Content: []ai.Part{{Type: "text", Text: "Hi"}}}},
		MaxTokens:   100,
		Temperature: ptr(0.2),
		User:        "u_123",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "Hello!" || resp.StopReason != ai.StopEndTurn || resp.Model != "gpt-test-2026" {
		t.Fatalf("resp = %+v", resp)
	}
	if resp.Usage != (ai.Usage{InputTokens: 11, OutputTokens: 7}) {
		t.Fatalf("usage = %+v", resp.Usage)
	}
	req := rc.last(t)
	if req.Path != "/v1/chat/completions" {
		t.Errorf("path = %s", req.Path)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer sk-test" {
		t.Errorf("auth = %q", got)
	}
	if req.Header.Get("OpenAI-Organization") != "org-1" || req.Header.Get("OpenAI-Project") != "proj-1" || req.Header.Get("X-Extra") != "yes" {
		t.Errorf("headers = %v", req.Header)
	}
	b := req.Body
	if b["model"] != "gpt-5" || b["max_completion_tokens"] != float64(100) || b["max_tokens"] != nil {
		t.Errorf("api.openai.com must use max_completion_tokens: %v", b)
	}
	if b["temperature"] != 0.2 || b["user"] != "u_123" || b["stream"] != nil {
		t.Errorf("body = %v", b)
	}
	msgs := b["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["role"] != "system" || msgs[0].(map[string]any)["content"] != "Be terse." {
		t.Errorf("messages = %v", msgs)
	}
	if msgs[1].(map[string]any)["content"] != "Hi" {
		t.Errorf("text-only content should be a plain string: %v", msgs[1])
	}
}

func TestChatCompatibleUsesMaxTokens(t *testing.T) {
	rc, srv := newServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		writeJSON(w, 200, chatOK("ok", "length"))
	})
	p := mustNew(t, config.ProviderConfig{Type: "openai", BaseURL: srv.URL + "/v1/"}, Options{})
	resp, err := p.Chat(context.Background(), ai.ChatRequest{Model: "llama", MaxTokens: 50,
		Messages: []ai.Message{{Role: "user", Content: []ai.Part{{Type: "text", Text: "x"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != ai.StopMaxTokens {
		t.Errorf("stop = %s", resp.StopReason)
	}
	req := rc.last(t)
	if req.Path != "/v1/chat/completions" || req.Body["max_tokens"] != float64(50) || req.Body["max_completion_tokens"] != nil {
		t.Errorf("req = %s %v", req.Path, req.Body)
	}
	if req.Header.Get("Authorization") != "" {
		t.Errorf("no api key configured, got auth header")
	}
}

func TestChatImagesAndJSON(t *testing.T) {
	rc, srv := newServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		writeJSON(w, 200, chatOK("```json\n{\"ok\": true}\n```", "stop"))
	})
	// api.openai.com downloads image URLs itself: they are passed through.
	p := mustNew(t, config.ProviderConfig{Type: "openai", APIKey: "sk"}, Options{
		HTTPClient: rewriteClient(srv),
		FetchImage: func(context.Context, string) ([]byte, string, error) {
			t.Error("api.openai.com URL images must not be downloaded by Backyard")
			return nil, "", errors.New("unexpected fetch")
		},
	})
	resp, err := p.Chat(context.Background(), ai.ChatRequest{
		Model: "gpt-4o", JSON: true,
		Messages: []ai.Message{{Role: "user", Content: []ai.Part{
			{Type: "text", Text: "Describe"},
			{Type: "image", Data: pngBytes, MediaType: "image/png"},
			{Type: "image", URL: "https://example.com/cat.jpg"},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != `{"ok": true}` {
		t.Errorf("content = %q", resp.Content)
	}
	b := rc.last(t).Body
	if rf, _ := b["response_format"].(map[string]any); rf["type"] != "json_object" {
		t.Errorf("response_format = %v", b["response_format"])
	}
	msgs := b["messages"].([]any)
	sys := msgs[0].(map[string]any)
	if sys["role"] != "system" || !strings.Contains(sys["content"].(string), "JSON") {
		t.Errorf("JSON instruction missing: %v", sys)
	}
	parts := msgs[1].(map[string]any)["content"].([]any)
	if len(parts) != 3 {
		t.Fatalf("parts = %v", parts)
	}
	img := parts[1].(map[string]any)["image_url"].(map[string]any)["url"].(string)
	if img != "data:image/png;base64,"+base64.StdEncoding.EncodeToString(pngBytes) {
		t.Errorf("data url = %s", img)
	}
	if u := parts[2].(map[string]any)["image_url"].(map[string]any)["url"]; u != "https://example.com/cat.jpg" {
		t.Errorf("remote url = %v", u)
	}
}

func TestCompatibleServersGetInlineImages(t *testing.T) {
	// Self-hosted / third-party OpenAI-compatible servers (vLLM, LiteLLM,
	// Workers AI, …) must not be handed user URLs to fetch from inside their
	// network: Backyard downloads (SSRF-guarded) and inlines them.
	rc, srv := newServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		writeJSON(w, 200, chatOK("a cat", "stop"))
	})
	for _, pc := range []config.ProviderConfig{
		{Type: "openai", BaseURL: srv.URL + "/v1"},
		{Type: "openai", BaseURL: "https://api.cloudflare.com/client/v4/accounts/acct/ai/v1", APIKey: "cf"},
		{Type: "openai", BaseURL: "https://gateway.ai.cloudflare.com/v1/acct/gw/workers-ai/v1", APIKey: "cf"},
	} {
		var fetched []string
		p := mustNew(t, pc, Options{
			HTTPClient: rewriteClient(srv),
			FetchImage: func(_ context.Context, u string) ([]byte, string, error) {
				fetched = append(fetched, u)
				return pngBytes, "image/png", nil
			},
		})
		_, err := p.Chat(context.Background(), ai.ChatRequest{Model: "m", Messages: []ai.Message{{Role: "user", Content: []ai.Part{
			{Type: "image", URL: "http://169.254.169.254/latest/meta-data/x.png"},
			{Type: "text", Text: "what is it?"},
		}}}})
		if err != nil {
			t.Fatalf("%s: %v", pc.BaseURL, err)
		}
		if len(fetched) != 1 || fetched[0] != "http://169.254.169.254/latest/meta-data/x.png" {
			t.Errorf("%s: fetched = %v", pc.BaseURL, fetched)
		}
		parts := rc.last(t).Body["messages"].([]any)[0].(map[string]any)["content"].([]any)
		if u := parts[0].(map[string]any)["image_url"].(map[string]any)["url"].(string); !strings.HasPrefix(u, "data:image/png;base64,") {
			t.Errorf("%s: image should be inlined, got %.60s", pc.BaseURL, u)
		}
	}

	// A fetch failure (e.g. the SSRF guard) is a bad request.
	p := mustNew(t, config.ProviderConfig{Type: "openai", BaseURL: srv.URL + "/v1"}, Options{
		FetchImage: func(context.Context, string) ([]byte, string, error) {
			return nil, "", errors.New("imagefetch: address not allowed")
		},
	})
	_, err := p.Chat(context.Background(), ai.ChatRequest{Model: "m", Messages: []ai.Message{{Role: "user", Content: []ai.Part{
		{Type: "image", URL: "http://10.0.0.1/x.png"},
	}}}})
	if !errors.Is(err, ai.ErrBadRequest) {
		t.Fatalf("err = %v, want ErrBadRequest", err)
	}
}

func TestQuirksAreBounded(t *testing.T) {
	p := mustNew(t, config.ProviderConfig{Type: "openai", BaseURL: "http://127.0.0.1:1/v1"}, Options{})
	for i := range maxQuirkModels + 50 {
		p.rememberQuirks(fmt.Sprintf("model-%d", i), quirks{noTemperature: true})
	}
	n := 0
	p.quirks.Range(func(any, any) bool { n++; return true })
	if n != maxQuirkModels || p.quirkCount.Load() != maxQuirkModels {
		t.Fatalf("stored %d quirks (counter %d), want %d", n, p.quirkCount.Load(), maxQuirkModels)
	}
	// Known models can still be updated.
	p.rememberQuirks("model-0", quirks{noStreamOptions: true})
	if q := p.quirksFor("model-0"); !q.noStreamOptions {
		t.Fatalf("update of a known model was dropped: %+v", q)
	}
}

func TestOllamaInlinesURLImages(t *testing.T) {
	rc, srv := newServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		writeJSON(w, 200, chatOK("a cat", "stop"))
	})
	var fetched string
	p := mustNew(t, config.ProviderConfig{Type: "ollama", BaseURL: srv.URL + "/v1"}, Options{
		FetchImage: func(ctx context.Context, u string) ([]byte, string, error) {
			fetched = u
			return pngBytes, "image/png", nil
		},
	})
	_, err := p.Chat(context.Background(), ai.ChatRequest{Model: "llava", User: "u_1",
		Messages: []ai.Message{{Role: "user", Content: []ai.Part{{Type: "image", URL: "https://example.com/c.png"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if fetched != "https://example.com/c.png" {
		t.Errorf("fetched = %q", fetched)
	}
	b := rc.last(t).Body
	parts := b["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if u := parts[0].(map[string]any)["image_url"].(map[string]any)["url"].(string); !strings.HasPrefix(u, "data:image/png;base64,") {
		t.Errorf("url = %s", u)
	}
	if b["user"] != nil {
		t.Errorf("ollama must not get user field")
	}

	// Fetch failures are bad requests.
	p.fetch = func(context.Context, string) ([]byte, string, error) { return nil, "", errors.New("blocked") }
	_, err = p.Chat(context.Background(), ai.ChatRequest{Model: "llava",
		Messages: []ai.Message{{Role: "user", Content: []ai.Part{{Type: "image", URL: "http://10.0.0.1/x.png"}}}}})
	if !errors.Is(err, ai.ErrBadRequest) {
		t.Errorf("want ErrBadRequest, got %v", err)
	}
}

func sseHandler(chunks ...string) func(w http.ResponseWriter, r *http.Request, body map[string]any) {
	return func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			fl.Flush()
		}
	}
}

func TestChatStream(t *testing.T) {
	rc, srv := newServer(t, sseHandler(
		`{"id":"1","model":"gpt-x","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
		`{"id":"1","model":"gpt-x","choices":[{"index":0,"delta":{"content":"Hel"},"finish_reason":null}]}`,
		`{"id":"1","model":"gpt-x","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}`,
		`{"id":"1","model":"gpt-x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"id":"1","model":"gpt-x","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`,
		`[DONE]`,
	))
	p := mustNew(t, config.ProviderConfig{Type: "openai", BaseURL: srv.URL}, Options{})
	var deltas []string
	resp, err := p.ChatStream(context.Background(), ai.ChatRequest{Model: "gpt-x", MaxTokens: 10,
		Messages: []ai.Message{{Role: "user", Content: []ai.Part{{Type: "text", Text: "hi"}}}}},
		func(s string) error { deltas = append(deltas, s); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(deltas, "|") != "Hel|lo" || resp.Content != "Hello" || resp.StopReason != ai.StopEndTurn {
		t.Fatalf("deltas=%v resp=%+v", deltas, resp)
	}
	if resp.Usage != (ai.Usage{InputTokens: 5, OutputTokens: 2}) || resp.Model != "gpt-x" {
		t.Fatalf("resp = %+v", resp)
	}
	b := rc.last(t)
	if b.Body["stream"] != true || b.Body["stream_options"].(map[string]any)["include_usage"] != true {
		t.Errorf("body = %v", b.Body)
	}
	if b.Header.Get("Accept") != "text/event-stream" {
		t.Errorf("accept = %q", b.Header.Get("Accept"))
	}
}

func TestChatStreamErrors(t *testing.T) {
	ctx := context.Background()
	req := ai.ChatRequest{Model: "m", Messages: []ai.Message{{Role: "user", Content: []ai.Part{{Type: "text", Text: "hi"}}}}}

	t.Run("error chunk", func(t *testing.T) {
		_, srv := newServer(t, sseHandler(
			`{"choices":[{"delta":{"content":"a"}}]}`,
			`{"error":{"message":"overloaded","type":"server_error"}}`,
		))
		p := mustNew(t, config.ProviderConfig{BaseURL: srv.URL}, Options{})
		_, err := p.ChatStream(ctx, req, func(string) error { return nil })
		var ue *ai.UpstreamError
		if !errors.As(err, &ue) || ue.Message != "overloaded" {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("truncated", func(t *testing.T) {
		_, srv := newServer(t, sseHandler(`{"choices":[{"delta":{"content":"a"}}]}`))
		p := mustNew(t, config.ProviderConfig{BaseURL: srv.URL}, Options{})
		_, err := p.ChatStream(ctx, req, func(string) error { return nil })
		var ue *ai.UpstreamError
		if !errors.As(err, &ue) || !strings.Contains(ue.Message, "unexpectedly") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("finish without DONE is fine", func(t *testing.T) {
		_, srv := newServer(t, sseHandler(`{"choices":[{"delta":{"content":"a"},"finish_reason":"content_filter"}]}`))
		p := mustNew(t, config.ProviderConfig{BaseURL: srv.URL}, Options{})
		resp, err := p.ChatStream(ctx, req, func(string) error { return nil })
		if err != nil || resp.StopReason != ai.StopRefusal || resp.Content != "a" {
			t.Fatalf("resp=%+v err=%v", resp, err)
		}
	})
	t.Run("callback abort", func(t *testing.T) {
		_, srv := newServer(t, sseHandler(`{"choices":[{"delta":{"content":"a"}}]}`, `[DONE]`))
		p := mustNew(t, config.ProviderConfig{BaseURL: srv.URL}, Options{})
		stop := errors.New("client gone")
		_, err := p.ChatStream(ctx, req, func(string) error { return stop })
		if !errors.Is(err, stop) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("http error", func(t *testing.T) {
		_, srv := newServer(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
			writeJSON(w, 401, map[string]any{"error": map[string]any{"message": "Incorrect API key provided", "type": "invalid_request_error"}})
		})
		p := mustNew(t, config.ProviderConfig{BaseURL: srv.URL, APIKey: "bad"}, Options{})
		_, err := p.ChatStream(ctx, req, func(string) error { return nil })
		var ue *ai.UpstreamError
		if !errors.As(err, &ue) || ue.Status != 401 || ue.Provider != "p1" || !strings.Contains(ue.Message, "Incorrect API key") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("context canceled", func(t *testing.T) {
		_, srv := newServer(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		})
		p := mustNew(t, config.ProviderConfig{BaseURL: srv.URL}, Options{})
		cctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		_, err := p.ChatStream(cctx, req, func(string) error { return nil })
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestRefusal(t *testing.T) {
	_, srv := newServer(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		writeJSON(w, 200, map[string]any{
			"model":   "gpt-x",
			"choices": []any{map[string]any{"message": map[string]any{"content": nil, "refusal": "I can't help with that."}, "finish_reason": "stop"}},
		})
	})
	p := mustNew(t, config.ProviderConfig{BaseURL: srv.URL}, Options{})
	resp, err := p.Chat(context.Background(), ai.ChatRequest{Model: "gpt-x",
		Messages: []ai.Message{{Role: "user", Content: []ai.Part{{Type: "text", Text: "x"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != ai.StopRefusal || resp.Content != "I can't help with that." {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestAdaptiveParameters(t *testing.T) {
	// An upstream that rejects max_tokens (like OpenAI reasoning models
	// behind a proxy) and any non-default temperature.
	rc, srv := newServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		if _, ok := body["max_tokens"]; ok {
			writeJSON(w, 400, map[string]any{"error": map[string]any{
				"message": "Unsupported parameter: 'max_tokens' is not supported with this model. Use 'max_completion_tokens' instead.",
				"type":    "invalid_request_error", "param": "max_tokens", "code": "unsupported_parameter"}})
			return
		}
		if _, ok := body["temperature"]; ok {
			writeJSON(w, 400, map[string]any{"error": map[string]any{
				"message": "Unsupported value: 'temperature' does not support 0.5 with this model. Only the default (1) value is supported.",
				"type":    "invalid_request_error", "param": "temperature", "code": "unsupported_value"}})
			return
		}
		writeJSON(w, 200, chatOK("fine", "stop"))
	})
	p := mustNew(t, config.ProviderConfig{BaseURL: srv.URL}, Options{})
	req := ai.ChatRequest{Model: "o-model", MaxTokens: 20, Temperature: ptr(0.5),
		Messages: []ai.Message{{Role: "user", Content: []ai.Part{{Type: "text", Text: "x"}}}}}
	resp, err := p.Chat(context.Background(), req)
	if err != nil || resp.Content != "fine" {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	if n := rc.count(); n != 3 {
		t.Fatalf("want 3 attempts, got %d", n)
	}
	last := rc.last(t).Body
	if last["max_completion_tokens"] != float64(20) || last["temperature"] != nil {
		t.Errorf("final body = %v", last)
	}
	// The adjustments are remembered for the model.
	if _, err := p.Chat(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if n := rc.count(); n != 4 {
		t.Fatalf("second call should need one attempt, total %d", n)
	}
	// Other models are unaffected.
	req.Model = "other"
	_, _ = p.Chat(context.Background(), req)
	if n := rc.count(); n != 7 {
		t.Fatalf("other model should adapt independently, total %d", n)
	}
}

func TestAdjustDoesNotLoopOnValueErrors(t *testing.T) {
	rc, srv := newServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		writeJSON(w, 400, map[string]any{"error": map[string]any{
			"message": "max_tokens is too large: 500000. This model supports at most 16384 completion tokens"}})
	})
	p := mustNew(t, config.ProviderConfig{BaseURL: srv.URL}, Options{})
	_, err := p.Chat(context.Background(), ai.ChatRequest{Model: "m", MaxTokens: 500000,
		Messages: []ai.Message{{Role: "user", Content: []ai.Part{{Type: "text", Text: "x"}}}}})
	var ue *ai.UpstreamError
	if !errors.As(err, &ue) || ue.Status != 400 {
		t.Fatalf("got %v", err)
	}
	if rc.count() != 1 {
		t.Fatalf("value errors must not be retried, got %d attempts", rc.count())
	}
}

func TestAdjustTable(t *testing.T) {
	base := quirks{maxField: fieldMaxTokens}
	tests := []struct {
		name  string
		q     quirks
		msg   string
		want  quirks
		param string
	}{
		{"max_tokens unsupported", base, "Unsupported parameter: 'max_tokens'", quirks{maxField: fieldMaxCompletionTokens}, "max_tokens"},
		{"max_completion_tokens unrecognized", quirks{maxField: fieldMaxCompletionTokens},
			"Unrecognized request argument supplied: max_completion_tokens", quirks{maxField: fieldMaxTokens}, "max_completion_tokens"},
		{"stream_options", base, "Extra inputs are not permitted: stream_options", quirks{maxField: fieldMaxTokens, noStreamOptions: true}, "stream_options"},
		{"response_format", base, "response_format is not supported by this backend", quirks{maxField: fieldMaxTokens, noResponseFormat: true}, "response_format"},
		{"temperature", base, "Unsupported value: 'temperature'", quirks{maxField: fieldMaxTokens, noTemperature: true}, "temperature"},
		{"unrelated", base, "Invalid API key", base, ""},
		{"value error", base, "max_tokens must be less than 4096", base, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, param, ok := adjust(tt.q, tt.msg, map[string]bool{})
			if ok != (tt.param != "") || got != tt.want || param != tt.param {
				t.Fatalf("got %+v %q %v", got, param, ok)
			}
		})
	}
}

func TestMapFinishReason(t *testing.T) {
	tests := map[string]string{
		"stop": ai.StopEndTurn, "length": ai.StopMaxTokens, "content_filter": ai.StopRefusal,
		"tool_calls": ai.StopOther, "function_call": ai.StopOther, "": ai.StopOther,
	}
	for in, want := range tests {
		if got := mapFinishReason(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestContentText(t *testing.T) {
	tests := map[string]string{
		`"hello"`: "hello",
		`null`:    "",
		``:        "",
		`[{"type":"text","text":"a"},{"type":"image_url"},{"type":"text","text":"b"}]`: "ab",
		`42`: "",
	}
	for in, want := range tests {
		if got := contentText(json.RawMessage(in)); got != want {
			t.Errorf("contentText(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestImages(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString(pngBytes)
	var calls atomic.Int32
	mux := http.NewServeMux()
	rc := &recorder{handler: func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		calls.Add(1)
		n := int(body["n"].(float64))
		data := make([]any, n)
		for i := range data {
			data[i] = map[string]any{"b64_json": b64}
		}
		writeJSON(w, 200, map[string]any{"created": 1, "data": data})
	}}
	mux.Handle("/images/generations", rc)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := mustNew(t, config.ProviderConfig{BaseURL: srv.URL, APIKey: "k"}, Options{})
	ctx := context.Background()

	resp, err := p.Image(ctx, ai.ImageRequest{Model: "gpt-image-1", Prompt: "a cat", Size: "1024x1024", N: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Images) != 2 || resp.Images[0].MediaType != "image/png" || string(resp.Images[0].Data) != string(pngBytes) {
		t.Fatalf("resp = %+v", resp)
	}
	b := rc.last(t).Body
	if b["model"] != "gpt-image-1" || b["size"] != "1024x1024" || b["n"] != float64(2) || b["response_format"] != nil {
		t.Errorf("gpt-image body = %v", b)
	}

	calls.Store(0)
	resp, err = p.Image(ctx, ai.ImageRequest{Model: "dall-e-3", Prompt: "a dog", N: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Images) != 2 || calls.Load() != 2 {
		t.Fatalf("dall-e-3 must be called once per image: images=%d calls=%d", len(resp.Images), calls.Load())
	}
	b = rc.last(t).Body
	if b["response_format"] != "b64_json" || b["n"] != float64(1) {
		t.Errorf("dall-e body = %v", b)
	}

	if _, err := p.Image(ctx, ai.ImageRequest{Model: "gpt-image-1"}); !errors.Is(err, ai.ErrBadRequest) {
		t.Errorf("empty prompt: %v", err)
	}
}

func TestImageURLResult(t *testing.T) {
	mux := http.NewServeMux()
	var srvURL string
	mux.HandleFunc("/images/generations", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"data": []any{map[string]any{"url": srvURL + "/files/img.png"}}})
	})
	mux.HandleFunc("/files/img.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngBytes)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	srvURL = srv.URL
	p := mustNew(t, config.ProviderConfig{BaseURL: srv.URL}, Options{})
	resp, err := p.Image(context.Background(), ai.ImageRequest{Model: "sdxl", Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Images) != 1 || resp.Images[0].MediaType != "image/png" {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestEmbed(t *testing.T) {
	rc, srv := newServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		writeJSON(w, 200, map[string]any{
			"object": "list",
			"data": []any{
				map[string]any{"object": "embedding", "index": 1, "embedding": []float64{0.4, 0.5}},
				map[string]any{"object": "embedding", "index": 0, "embedding": []float64{0.1, 0.2}},
			},
			"model": "text-embedding-3-small",
			"usage": map[string]any{"prompt_tokens": 8, "total_tokens": 8},
		})
	})
	p := mustNew(t, config.ProviderConfig{BaseURL: srv.URL}, Options{})
	resp, err := p.Embed(context.Background(), ai.EmbedRequest{Model: "text-embedding-3-small", Input: []string{"a", "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Embeddings) != 2 || resp.Embeddings[0][0] != 0.1 || resp.Embeddings[1][0] != 0.4 || resp.Usage.InputTokens != 8 {
		t.Fatalf("resp = %+v", resp)
	}
	req := rc.last(t)
	if req.Path != "/embeddings" || req.Body["encoding_format"] != "float" || len(req.Body["input"].([]any)) != 2 {
		t.Errorf("req = %+v", req)
	}

	// Count mismatch is an upstream error.
	_, err = p.Embed(context.Background(), ai.EmbedRequest{Model: "m", Input: []string{"a", "b", "c"}})
	var ue *ai.UpstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("got %v", err)
	}
}

type fakeCred struct{ calls atomic.Int32 }

func (f *fakeCred) GetToken(ctx context.Context, opts policy.TokenRequestOptions) (azcore.AccessToken, error) {
	f.calls.Add(1)
	if len(opts.Scopes) != 1 || opts.Scopes[0] != azureScope {
		return azcore.AccessToken{}, fmt.Errorf("bad scopes %v", opts.Scopes)
	}
	return azcore.AccessToken{Token: "entra-token", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

func TestAzure(t *testing.T) {
	rc, srv := newServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			writeJSON(w, 200, chatOK("azure hi", "stop"))
		case strings.HasSuffix(r.URL.Path, "/embeddings"):
			writeJSON(w, 200, map[string]any{"data": []any{map[string]any{"index": 0, "embedding": []float64{1}}}})
		case strings.HasSuffix(r.URL.Path, "/images/generations"):
			writeJSON(w, 200, map[string]any{"data": []any{map[string]any{"b64_json": base64.StdEncoding.EncodeToString(pngBytes)}}})
		}
	})
	ctx := context.Background()
	msgs := []ai.Message{{Role: "user", Content: []ai.Part{{Type: "text", Text: "hi"}}}}

	t.Run("api key deployments", func(t *testing.T) {
		p := mustNew(t, config.ProviderConfig{Type: "azure-openai", Endpoint: srv.URL + "/", APIKey: "az-key"}, Options{})
		if _, err := p.Chat(ctx, ai.ChatRequest{Model: "my-gpt4o", MaxTokens: 10, Messages: msgs}); err != nil {
			t.Fatal(err)
		}
		req := rc.last(t)
		if req.Path != "/openai/deployments/my-gpt4o/chat/completions" || req.Query.Get("api-version") != "2024-10-21" {
			t.Errorf("url = %s?%s", req.Path, req.Query.Encode())
		}
		if req.Header.Get("api-key") != "az-key" || req.Header.Get("Authorization") != "" {
			t.Errorf("headers = %v", req.Header)
		}
		if _, ok := req.Body["model"]; ok {
			t.Errorf("deployment requests carry no model: %v", req.Body)
		}
		if req.Body["max_completion_tokens"] != float64(10) {
			t.Errorf("body = %v", req.Body)
		}
		if _, err := p.Embed(ctx, ai.EmbedRequest{Model: "emb", Input: []string{"x"}}); err != nil {
			t.Fatal(err)
		}
		if req := rc.last(t); req.Path != "/openai/deployments/emb/embeddings" {
			t.Errorf("embed path = %s", req.Path)
		}
		if _, err := p.Image(ctx, ai.ImageRequest{Model: "img", Prompt: "p"}); err != nil {
			t.Fatal(err)
		}
		if req := rc.last(t); req.Path != "/openai/deployments/img/images/generations" {
			t.Errorf("image path = %s", req.Path)
		}
	})

	t.Run("entra id", func(t *testing.T) {
		cred := &fakeCred{}
		p := mustNew(t, config.ProviderConfig{Type: "azure-openai", Endpoint: srv.URL, UseAzureAD: true, APIVersion: "2025-01-01-preview"},
			Options{AzureCredential: cred})
		for range 3 {
			if _, err := p.Chat(ctx, ai.ChatRequest{Model: "dep", Messages: msgs}); err != nil {
				t.Fatal(err)
			}
		}
		req := rc.last(t)
		if req.Header.Get("Authorization") != "Bearer entra-token" || req.Header.Get("api-key") != "" {
			t.Errorf("headers = %v", req.Header)
		}
		if req.Query.Get("api-version") != "2025-01-01-preview" {
			t.Errorf("api-version = %s", req.Query.Get("api-version"))
		}
		if cred.calls.Load() != 1 {
			t.Errorf("token should be cached, got %d calls", cred.calls.Load())
		}
	})

	t.Run("v1 api", func(t *testing.T) {
		p := mustNew(t, config.ProviderConfig{Type: "azure-openai", Endpoint: srv.URL + "/openai", APIKey: "k", APIVersion: "v1"}, Options{})
		if _, err := p.Chat(ctx, ai.ChatRequest{Model: "gpt-5-mini", Messages: msgs}); err != nil {
			t.Fatal(err)
		}
		req := rc.last(t)
		if req.Path != "/openai/v1/chat/completions" || req.Query.Get("api-version") != "" || req.Body["model"] != "gpt-5-mini" {
			t.Errorf("req = %s %v %v", req.Path, req.Query, req.Body)
		}
	})
}

func TestBadRequests(t *testing.T) {
	p := mustNew(t, config.ProviderConfig{BaseURL: "http://127.0.0.1:1"}, Options{})
	ctx := context.Background()
	if _, err := p.Chat(ctx, ai.ChatRequest{Messages: []ai.Message{{Role: "user"}}}); !errors.Is(err, ai.ErrBadRequest) {
		t.Errorf("missing model: %v", err)
	}
	if _, err := p.Chat(ctx, ai.ChatRequest{Model: "m", Messages: []ai.Message{{Role: "tool"}}}); !errors.Is(err, ai.ErrBadRequest) {
		t.Errorf("bad role: %v", err)
	}
	if _, err := p.Embed(ctx, ai.EmbedRequest{Model: "m"}); !errors.Is(err, ai.ErrBadRequest) {
		t.Errorf("empty input: %v", err)
	}
	// Unreachable upstream → UpstreamError with status 0.
	_, err := p.Chat(ctx, ai.ChatRequest{Model: "m", Messages: []ai.Message{{Role: "user", Content: []ai.Part{{Type: "text", Text: "x"}}}}})
	var ue *ai.UpstreamError
	if !errors.As(err, &ue) || ue.Status != 0 {
		t.Errorf("got %v", err)
	}
}

func TestCloudflareWorkersAI(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString(pngBytes)
	rc, srv := newServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/ai/v1/chat/completions"):
			writeJSON(w, 200, chatOK("beep boop", "stop"))
		case strings.HasSuffix(r.URL.Path, "/ai/v1/embeddings"):
			writeJSON(w, 200, map[string]any{"data": []any{map[string]any{"index": 0, "embedding": []float64{0.3}}}})
		case strings.Contains(r.URL.Path, "/ai/run/@cf/black-forest-labs/flux-1-schnell"):
			writeJSON(w, 200, map[string]any{"result": map[string]any{"image": b64}, "success": true, "errors": []any{}, "messages": []any{}})
		case strings.Contains(r.URL.Path, "/ai/run/@cf/stabilityai/stable-diffusion-xl-base-1.0"):
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(pngBytes)
		case strings.Contains(r.URL.Path, "/ai/run/@cf/broken/model"):
			writeJSON(w, 400, map[string]any{"success": false, "errors": []any{map[string]any{"code": 5006, "message": "AiError: Bad input: prompt too long"}}})
		default:
			http.NotFound(w, r)
		}
	})
	p := mustNew(t, config.ProviderConfig{Type: "openai", APIKey: "cf-token",
		BaseURL: "https://api.cloudflare.com/client/v4/accounts/acc123/ai/v1"}, Options{HTTPClient: rewriteClient(srv)})
	ctx := context.Background()

	resp, err := p.Chat(ctx, ai.ChatRequest{Model: "@cf/meta/llama-3.1-8b-instruct", MaxTokens: 300, User: "u_1",
		Messages: []ai.Message{{Role: "user", Content: []ai.Part{{Type: "text", Text: "Make some robot noises"}}}}})
	if err != nil || resp.Content != "beep boop" {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	req := rc.last(t)
	if req.Path != "/client/v4/accounts/acc123/ai/v1/chat/completions" || req.Header.Get("Authorization") != "Bearer cf-token" {
		t.Errorf("req = %s %v", req.Path, req.Header)
	}
	if req.Body["max_tokens"] != float64(300) || req.Body["user"] != nil || req.Body["model"] != "@cf/meta/llama-3.1-8b-instruct" {
		t.Errorf("body = %v", req.Body)
	}

	if _, err := p.Embed(ctx, ai.EmbedRequest{Model: "@cf/baai/bge-large-en-v1.5", Input: []string{"I love matcha"}}); err != nil {
		t.Fatal(err)
	}
	if req := rc.last(t); req.Path != "/client/v4/accounts/acc123/ai/v1/embeddings" {
		t.Errorf("embed path = %s", req.Path)
	}

	img, err := p.Image(ctx, ai.ImageRequest{Model: "@cf/black-forest-labs/flux-1-schnell", Prompt: "cyberpunk cat", N: 2, Size: "1024x1024"})
	if err != nil {
		t.Fatal(err)
	}
	if len(img.Images) != 2 || img.Images[0].MediaType != "image/png" || string(img.Images[0].Data) != string(pngBytes) {
		t.Fatalf("images = %+v", img)
	}
	req = rc.last(t)
	if req.Path != "/client/v4/accounts/acc123/ai/run/@cf/black-forest-labs/flux-1-schnell" || req.Body["prompt"] != "cyberpunk cat" {
		t.Errorf("run req = %s %v", req.Path, req.Body)
	}
	if _, ok := req.Body["width"]; ok {
		t.Errorf("flux does not take width: %v", req.Body)
	}

	img, err = p.Image(ctx, ai.ImageRequest{Model: "@cf/stabilityai/stable-diffusion-xl-base-1.0", Prompt: "x", Size: "768x512"})
	if err != nil || len(img.Images) != 1 || img.Images[0].MediaType != "image/png" {
		t.Fatalf("raw image: %+v %v", img, err)
	}
	if b := rc.last(t).Body; b["width"] != float64(768) || b["height"] != float64(512) {
		t.Errorf("sdxl body = %v", b)
	}

	_, err = p.Image(ctx, ai.ImageRequest{Model: "@cf/broken/model", Prompt: "x"})
	var ue *ai.UpstreamError
	if !errors.As(err, &ue) || ue.Status != 400 || ue.Message != "AiError: Bad input: prompt too long" {
		t.Fatalf("got %#v", err)
	}
	if _, err := p.Image(ctx, ai.ImageRequest{Model: "@cf/../../secrets", Prompt: "x"}); !errors.Is(err, ai.ErrBadRequest) {
		t.Errorf("path traversal: %v", err)
	}
}

func TestCloudflareAIGateway(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString(pngBytes)
	rc, srv := newServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		if strings.Contains(r.URL.Path, "/workers-ai/@cf/") {
			writeJSON(w, 200, map[string]any{"result": map[string]any{"image": b64}})
			return
		}
		writeJSON(w, 200, chatOK("via gateway", "stop"))
	})
	ctx := context.Background()
	msgs := []ai.Message{{Role: "user", Content: []ai.Part{{Type: "text", Text: "hi"}}}}

	// OpenAI through AI Gateway: OpenAI semantics (max_completion_tokens), gateway auth header.
	p := mustNew(t, config.ProviderConfig{Type: "openai", APIKey: "sk-openai",
		BaseURL: "https://gateway.ai.cloudflare.com/v1/acct/gw/openai",
		Headers: map[string]string{"cf-aig-authorization": "Bearer gw-token"}}, Options{HTTPClient: rewriteClient(srv)})
	if _, err := p.Chat(ctx, ai.ChatRequest{Model: "gpt-5-mini", MaxTokens: 10, Messages: msgs}); err != nil {
		t.Fatal(err)
	}
	req := rc.last(t)
	if req.Path != "/v1/acct/gw/openai/chat/completions" || req.Header.Get("Cf-Aig-Authorization") != "Bearer gw-token" ||
		req.Header.Get("Authorization") != "Bearer sk-openai" || req.Body["max_completion_tokens"] != float64(10) {
		t.Errorf("req = %s %v %v", req.Path, req.Header, req.Body)
	}

	// Workers AI through AI Gateway: chat on /workers-ai/v1, images on /workers-ai/<model>.
	p = mustNew(t, config.ProviderConfig{Type: "openai", APIKey: "cf-token",
		BaseURL: "https://gateway.ai.cloudflare.com/v1/acct/gw/workers-ai/v1"}, Options{HTTPClient: rewriteClient(srv)})
	if _, err := p.Chat(ctx, ai.ChatRequest{Model: "@cf/meta/llama-3.3-70b-instruct-fp8-fast", Messages: msgs}); err != nil {
		t.Fatal(err)
	}
	if req := rc.last(t); req.Path != "/v1/acct/gw/workers-ai/v1/chat/completions" {
		t.Errorf("chat path = %s", req.Path)
	}
	if _, err := p.Image(ctx, ai.ImageRequest{Model: "@cf/black-forest-labs/flux-1-schnell", Prompt: "x"}); err != nil {
		t.Fatal(err)
	}
	if req := rc.last(t); req.Path != "/v1/acct/gw/workers-ai/@cf/black-forest-labs/flux-1-schnell" {
		t.Errorf("image path = %s", req.Path)
	}
}

func TestWorkersAIRunBase(t *testing.T) {
	tests := map[string]string{
		"https://api.cloudflare.com/client/v4/accounts/abc/ai/v1":  "https://api.cloudflare.com/client/v4/accounts/abc/ai/run/",
		"https://api.cloudflare.com/client/v4/accounts/abc/ai/v1/": "https://api.cloudflare.com/client/v4/accounts/abc/ai/run/",
		"https://gateway.ai.cloudflare.com/v1/a/g/workers-ai/v1":   "https://gateway.ai.cloudflare.com/v1/a/g/workers-ai/",
		"https://gateway.ai.cloudflare.com/v1/a/g/openai":          "",
		"https://api.openai.com/v1":                                "",
		"https://evil.example.com/client/v4/accounts/abc/ai/v1":    "",
	}
	for in, want := range tests {
		u, _ := url.Parse(in)
		if got := workersAIRunBase(u); got != want {
			t.Errorf("workersAIRunBase(%s) = %q, want %q", in, got, want)
		}
	}
}

type failingCred struct{}

func (failingCred) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{}, errors.New("DefaultAzureCredential: failed\n\tManagedIdentityCredential: IMDS response body SECRET-ish details")
}

func TestAzureEntraErrorIsGeneric(t *testing.T) {
	rc, srv := newServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		writeJSON(w, 200, chatOK("x", "stop"))
	})
	p := mustNew(t, config.ProviderConfig{Type: "azure-openai", Endpoint: srv.URL, UseAzureAD: true}, Options{AzureCredential: failingCred{}})
	_, err := p.Chat(context.Background(), ai.ChatRequest{Model: "dep", Messages: []ai.Message{{Role: "user", Content: []ai.Part{{Type: "text", Text: "x"}}}}})
	var ue *ai.UpstreamError
	if !errors.As(err, &ue) || ue.Status != http.StatusUnauthorized {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(ue.Message, "SECRET") || strings.Contains(ue.Message, "IMDS") {
		t.Errorf("credential chain details leaked to the client: %q", ue.Message)
	}
	if rc.count() != 0 {
		t.Errorf("request sent without a token")
	}
}

func TestAzureEndpointForms(t *testing.T) {
	for _, tc := range []struct {
		endpoint, version, op, want string
	}{
		{"https://r.openai.azure.com", "2024-10-21", "chat/completions",
			"https://r.openai.azure.com/openai/deployments/dep%201/chat/completions?api-version=2024-10-21"},
		{"https://r.openai.azure.com/openai/", "2024-10-21", "embeddings",
			"https://r.openai.azure.com/openai/deployments/dep%201/embeddings?api-version=2024-10-21"},
		{"https://r.openai.azure.com/openai/v1", "v1", "chat/completions",
			"https://r.openai.azure.com/openai/v1/chat/completions"},
		{"https://proxy.corp/v1", "2024-10-21", "chat/completions",
			"https://proxy.corp/v1/openai/deployments/dep%201/chat/completions?api-version=2024-10-21"},
		{"https://r.services.ai.azure.com", "preview", "images/generations",
			"https://r.services.ai.azure.com/openai/v1/images/generations?api-version=preview"},
	} {
		p := mustNew(t, config.ProviderConfig{Type: "azure-openai", Endpoint: tc.endpoint, APIVersion: tc.version, APIKey: "k"}, Options{})
		if got := p.endpoint(tc.op, "dep 1"); got != tc.want {
			t.Errorf("endpoint(%s, %s) = %s, want %s", tc.endpoint, tc.version, got, tc.want)
		}
	}
}

// closeCounter is a transport that counts response bodies opened and closed.
type closeCounter struct {
	opened, closed atomic.Int32
}

type countedBody struct {
	io.ReadCloser
	once sync.Once
	c    *closeCounter
}

func (b *countedBody) Close() error {
	b.once.Do(func() { b.c.closed.Add(1) })
	return b.ReadCloser.Close()
}

func (c *closeCounter) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	c.opened.Add(1)
	resp.Body = &countedBody{ReadCloser: resp.Body, c: c}
	return resp, nil
}

// TestResponseBodiesClosed checks that every call closes the upstream
// response body, on success, on upstream errors and on invalid responses.
func TestResponseBodiesClosed(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString(pngBytes)
	_, srv := newServer(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		model, _ := body["model"].(string)
		switch {
		case model == "fail":
			writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": map[string]any{"message": "slow down"}})
		case model == "garbage":
			_, _ = io.WriteString(w, "{not json")
		case body["stream"] == true:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n")
		case r.URL.Path == "/chat/completions":
			writeJSON(w, http.StatusOK, map[string]any{"model": model, "choices": []any{
				map[string]any{"message": map[string]any{"role": "assistant", "content": "hi"}, "finish_reason": "stop"}}})
		case r.URL.Path == "/images/generations":
			writeJSON(w, http.StatusOK, map[string]any{"data": []any{map[string]any{"b64_json": b64}}})
		case r.URL.Path == "/embeddings":
			writeJSON(w, http.StatusOK, map[string]any{"data": []any{map[string]any{"index": 0, "embedding": []float64{1}}}})
		default:
			http.NotFound(w, r)
		}
	})
	cc := &closeCounter{}
	p := mustNew(t, config.ProviderConfig{BaseURL: srv.URL, APIKey: "k"}, Options{HTTPClient: &http.Client{Transport: cc}})
	ctx := context.Background()
	msgs := []ai.Message{{Role: "user", Content: []ai.Part{{Type: "text", Text: "hi"}}}}
	for _, model := range []string{"gpt-4o", "fail", "garbage"} {
		wantErr := model != "gpt-4o"
		if _, err := p.Chat(ctx, ai.ChatRequest{Model: model, Messages: msgs}); (err != nil) != wantErr {
			t.Errorf("Chat(%s): err = %v", model, err)
		}
		if _, err := p.ChatStream(ctx, ai.ChatRequest{Model: model, Messages: msgs}, func(string) error { return nil }); (err != nil) != wantErr {
			t.Errorf("ChatStream(%s): err = %v", model, err)
		}
		if _, err := p.Image(ctx, ai.ImageRequest{Model: model, Prompt: "a cat"}); (err != nil) != wantErr {
			t.Errorf("Image(%s): err = %v", model, err)
		}
		if _, err := p.Embed(ctx, ai.EmbedRequest{Model: model, Input: []string{"a"}}); (err != nil) != wantErr {
			t.Errorf("Embed(%s): err = %v", model, err)
		}
	}
	if o, c := cc.opened.Load(), cc.closed.Load(); o == 0 || o != c {
		t.Fatalf("response bodies: %d opened, %d closed", o, c)
	}
}
