package bedrock

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

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/aws/smithy-go"

	"github.com/thasami/backyard/internal/ai"
	"github.com/thasami/backyard/internal/config"
)

var pngBytes = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89\x00\x00\x00\rIDATx\x9cc\xf8\x0f\x00\x00\x01\x01\x00\x05\x18\xd8N\x00\x00\x00\x00IEND\xaeB`\x82")

type captured struct {
	Path   string
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
		f.reqs = append(f.reqs, captured{r.URL.Path, r.Header.Clone(), body})
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
		t.Fatal("no request")
	}
	return f.reqs[len(f.reqs)-1]
}

func (f *fake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func newProvider(t *testing.T, f *fake, pc config.ProviderConfig) *Provider {
	t.Helper()
	cfg := aws.Config{
		Region:           "us-east-1",
		Credentials:      credentials.NewStaticCredentialsProvider("AKIDTEST", "SECRET", ""),
		RetryMaxAttempts: 1,
	}
	pc.BaseURL = f.srv.URL
	p, err := New(context.Background(), "br", pc, Options{AWSConfig: &cfg,
		FetchImage: func(context.Context, string) ([]byte, string, error) { return pngBytes, "image/png", nil }})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func ptr[T any](v T) *T { return &v }

func converseOK(text, stop string) map[string]any {
	return map[string]any{
		"output":     map[string]any{"message": map[string]any{"role": "assistant", "content": []any{map[string]any{"text": text}}}},
		"stopReason": stop,
		"usage":      map[string]any{"inputTokens": 9, "outputTokens": 4, "totalTokens": 13},
		"metrics":    map[string]any{"latencyMs": 12},
	}
}

func TestNewRequiresRegion(t *testing.T) {
	cfg := aws.Config{}
	if _, err := New(context.Background(), "br", config.ProviderConfig{}, Options{AWSConfig: &cfg}); err == nil {
		t.Fatal("missing region must fail")
	}
	p, err := New(context.Background(), "br", config.ProviderConfig{Region: "eu-west-1"}, Options{AWSConfig: &cfg})
	if err != nil || p.Name() != "br" {
		t.Fatalf("p=%v err=%v", p, err)
	}
}

func TestConverse(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		writeJSON(w, 200, converseOK("Hi from Nova", "end_turn"))
	})
	p := newProvider(t, f, config.ProviderConfig{})
	resp, err := p.Chat(context.Background(), ai.ChatRequest{
		Model:  "amazon.nova-pro-v1:0",
		System: "Be nice.",
		Messages: []ai.Message{
			{Role: "user", Content: []ai.Part{{Type: "text", Text: "Hello"}, {Type: "image", Data: pngBytes, MediaType: "image/png"}}},
			{Role: "assistant", Content: []ai.Part{{Type: "text", Text: "Yes?"}}},
			{Role: "user", Content: []ai.Part{{Type: "image", URL: "https://example.com/x.png"}, {Type: "text", Text: "And this?"}}},
		},
		MaxTokens:   200,
		Temperature: ptr(1.5),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "Hi from Nova" || resp.StopReason != ai.StopEndTurn || resp.Model != "amazon.nova-pro-v1:0" {
		t.Fatalf("resp = %+v", resp)
	}
	if resp.Usage != (ai.Usage{InputTokens: 9, OutputTokens: 4}) {
		t.Fatalf("usage = %+v", resp.Usage)
	}
	req := f.last(t)
	if req.Path != "/model/amazon.nova-pro-v1:0/converse" {
		t.Errorf("path = %s", req.Path)
	}
	if !strings.HasPrefix(req.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AKIDTEST/") {
		t.Errorf("request not SigV4-signed: %q", req.Header.Get("Authorization"))
	}
	b := req.Body
	ic := b["inferenceConfig"].(map[string]any)
	if ic["maxTokens"] != float64(200) || ic["temperature"] != float64(1) {
		t.Errorf("inferenceConfig = %v", ic)
	}
	if sys := b["system"].([]any)[0].(map[string]any)["text"]; sys != "Be nice." {
		t.Errorf("system = %v", sys)
	}
	msgs := b["messages"].([]any)
	if len(msgs) != 3 || msgs[1].(map[string]any)["role"] != "assistant" {
		t.Fatalf("messages = %v", msgs)
	}
	img := msgs[0].(map[string]any)["content"].([]any)[1].(map[string]any)["image"].(map[string]any)
	if img["format"] != "png" || img["source"].(map[string]any)["bytes"] != base64.StdEncoding.EncodeToString(pngBytes) {
		t.Errorf("image = %v", img)
	}
	fetched := msgs[2].(map[string]any)["content"].([]any)[0].(map[string]any)["image"].(map[string]any)
	if fetched["format"] != "png" {
		t.Errorf("fetched image = %v", fetched)
	}
	if _, ok := b["additionalModelRequestFields"]; ok {
		t.Errorf("no additional fields for non-Claude models")
	}
}

func TestConverseClaudeRules(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		writeJSON(w, 200, converseOK("```json\n{\"x\":1}\n```", "max_tokens"))
	})
	p := newProvider(t, f, config.ProviderConfig{})
	resp, err := p.Chat(context.Background(), ai.ChatRequest{
		Model: "us.anthropic.claude-opus-4-8-v1:0", JSON: true, Effort: "high", Temperature: ptr(0.2),
		Messages: []ai.Message{{Role: "user", Content: []ai.Part{{Type: "text", Text: "json"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != `{"x":1}` || resp.StopReason != ai.StopMaxTokens {
		t.Fatalf("resp = %+v", resp)
	}
	b := f.last(t).Body
	if _, ok := b["inferenceConfig"]; ok {
		t.Errorf("temperature must be dropped for Opus 4.8: %v", b["inferenceConfig"])
	}
	amf := b["additionalModelRequestFields"].(map[string]any)
	if amf["output_config"].(map[string]any)["effort"] != "high" {
		t.Errorf("additionalModelRequestFields = %v", amf)
	}
	if sys := b["system"].([]any)[0].(map[string]any)["text"].(string); !strings.Contains(sys, "JSON") {
		t.Errorf("JSON instruction missing: %q", sys)
	}

	// Legacy Claude keeps temperature.
	if _, err := p.Chat(context.Background(), ai.ChatRequest{Model: "anthropic.claude-3-5-sonnet-20241022-v2:0", Temperature: ptr(0.3),
		Messages: []ai.Message{{Role: "user", Content: []ai.Part{{Type: "text", Text: "x"}}}}}); err != nil {
		t.Fatal(err)
	}
	if ic := f.last(t).Body["inferenceConfig"].(map[string]any); fmt.Sprint(ic["temperature"]) != "0.3" {
		t.Errorf("inferenceConfig = %v", ic)
	}
	if _, ok := f.last(t).Body["additionalModelRequestFields"]; ok {
		t.Errorf("Claude 3.5 must not get output_config")
	}

	// Opus 4.5 needs the effort beta flag (passed as anthropic_beta) and
	// tops out at "high".
	if _, err := p.Chat(context.Background(), ai.ChatRequest{Model: "global.anthropic.claude-opus-4-5-20251101-v1:0", Effort: "xhigh",
		Messages: []ai.Message{{Role: "user", Content: []ai.Part{{Type: "text", Text: "x"}}}}}); err != nil {
		t.Fatal(err)
	}
	amf = f.last(t).Body["additionalModelRequestFields"].(map[string]any)
	if amf["output_config"].(map[string]any)["effort"] != "high" || fmt.Sprint(amf["anthropic_beta"]) != "[effort-2025-11-24]" {
		t.Errorf("Opus 4.5 additionalModelRequestFields = %v", amf)
	}
	// Opus 4.8 does not.
	if _, err := p.Chat(context.Background(), ai.ChatRequest{Model: "us.anthropic.claude-opus-4-8-v1:0", Effort: "low",
		Messages: []ai.Message{{Role: "user", Content: []ai.Part{{Type: "text", Text: "x"}}}}}); err != nil {
		t.Fatal(err)
	}
	if amf = f.last(t).Body["additionalModelRequestFields"].(map[string]any); amf["anthropic_beta"] != nil {
		t.Errorf("unexpected anthropic_beta for Opus 4.8: %v", amf)
	}
}

func TestConverseErrors(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		w.Header().Set("X-Amzn-ErrorType", "ThrottlingException")
		writeJSON(w, 429, map[string]any{"message": "Too many requests, please wait before trying again."})
	})
	p := newProvider(t, f, config.ProviderConfig{})
	_, err := p.Chat(context.Background(), ai.ChatRequest{Model: "m", Messages: []ai.Message{{Role: "user", Content: []ai.Part{{Type: "text", Text: "x"}}}}})
	var ue *ai.UpstreamError
	if !errors.As(err, &ue) || ue.Status != 429 || !strings.Contains(ue.Message, "Too many requests") || ue.Provider != "br" {
		t.Fatalf("got %#v", err)
	}
	if ue.RetryAfter() != 0 {
		t.Fatalf("RetryAfter = %v without a Retry-After header", ue.RetryAfter())
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Fatal("error leaks secret")
	}
	for i, req := range []ai.ChatRequest{
		{Messages: []ai.Message{{Role: "user", Content: []ai.Part{{Type: "text", Text: "x"}}}}},
		{Model: "m"},
		{Model: "m", Messages: []ai.Message{{Role: "system", Content: []ai.Part{{Type: "text", Text: "x"}}}}},
		{Model: "m", Messages: []ai.Message{{Role: "user", Content: []ai.Part{{Type: "image", Data: []byte("BM.."), MediaType: "image/bmp"}}}}},
	} {
		if _, err := p.Chat(context.Background(), req); !errors.Is(err, ai.ErrBadRequest) {
			t.Errorf("case %d: %v", i, err)
		}
	}
}

// TestConverseErrorRetryAfter: a Retry-After sent with a throttling error
// reaches the *ai.UpstreamError (for the HTTP layer's 429 Retry-After).
func TestConverseErrorRetryAfter(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		w.Header().Set("X-Amzn-ErrorType", "ThrottlingException")
		w.Header().Set("Retry-After", "9")
		writeJSON(w, 429, map[string]any{"message": "Rate exceeded"})
	})
	p := newProvider(t, f, config.ProviderConfig{})
	msgs := []ai.Message{{Role: "user", Content: []ai.Part{{Type: "text", Text: "x"}}}}
	_, err := p.Chat(context.Background(), ai.ChatRequest{Model: "m", Messages: msgs})
	var ra interface{ RetryAfter() time.Duration }
	if !errors.As(err, &ra) || ra.RetryAfter() != 9*time.Second {
		t.Fatalf("chat: got %v", err)
	}
	_, err = p.ChatStream(context.Background(), ai.ChatRequest{Model: "m", Messages: msgs}, nil)
	if !errors.As(err, &ra) || ra.RetryAfter() != 9*time.Second {
		t.Fatalf("stream: got %v", err)
	}
	_, err = p.Embed(context.Background(), ai.EmbedRequest{Model: "amazon.titan-embed-text-v2:0", Input: []string{"x"}})
	if !errors.As(err, &ra) || ra.RetryAfter() != 9*time.Second {
		t.Fatalf("invoke: got %v", err)
	}
}

func TestBearerAPIKey(t *testing.T) {
	// Bedrock API keys (bearer tokens) are only sent over HTTPS.
	var auth string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		writeJSON(w, 200, converseOK("ok", "end_turn"))
	}))
	defer srv.Close()
	cfg := aws.Config{Region: "us-east-1", RetryMaxAttempts: 1}
	p, err := New(context.Background(), "br", config.ProviderConfig{APIKey: "bedrock-api-key-123", BaseURL: srv.URL},
		Options{AWSConfig: &cfg, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Chat(context.Background(), ai.ChatRequest{Model: "m", Messages: []ai.Message{{Role: "user", Content: []ai.Part{{Type: "text", Text: "x"}}}}}); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer bedrock-api-key-123" {
		t.Errorf("authorization = %q", auth)
	}
}

type esEvent struct {
	kind    string // event | exception
	typ     string
	payload string
}

func writeEventStream(t *testing.T, w http.ResponseWriter, events []esEvent) {
	w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
	w.WriteHeader(200)
	enc := eventstream.NewEncoder()
	for _, e := range events {
		typeHeader := ":event-type"
		if e.kind == "exception" {
			typeHeader = ":exception-type"
		}
		msg := eventstream.Message{
			Headers: eventstream.Headers{
				{Name: ":message-type", Value: eventstream.StringValue(e.kind)},
				{Name: typeHeader, Value: eventstream.StringValue(e.typ)},
				{Name: ":content-type", Value: eventstream.StringValue("application/json")},
			},
			Payload: []byte(e.payload),
		}
		if err := enc.Encode(w, msg); err != nil {
			t.Errorf("encode: %v", err)
			return
		}
		w.(http.Flusher).Flush()
	}
}

func TestConverseStream(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		writeEventStream(t, w, []esEvent{
			{"event", "messageStart", `{"role":"assistant"}`},
			{"event", "contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"Hel"}}`},
			{"event", "contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"lo"}}`},
			{"event", "contentBlockStop", `{"contentBlockIndex":0}`},
			{"event", "messageStop", `{"stopReason":"end_turn"}`},
			{"event", "metadata", `{"usage":{"inputTokens":5,"outputTokens":2,"totalTokens":7},"metrics":{"latencyMs":40}}`},
		})
	})
	p := newProvider(t, f, config.ProviderConfig{})
	var deltas []string
	resp, err := p.ChatStream(context.Background(), ai.ChatRequest{Model: "meta.llama3-70b-instruct-v1:0", MaxTokens: 50,
		Messages: []ai.Message{{Role: "user", Content: []ai.Part{{Type: "text", Text: "hi"}}}}},
		func(s string) error { deltas = append(deltas, s); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(deltas, "|") != "Hel|lo" || resp.Content != "Hello" || resp.StopReason != ai.StopEndTurn {
		t.Fatalf("deltas=%v resp=%+v", deltas, resp)
	}
	if resp.Usage != (ai.Usage{InputTokens: 5, OutputTokens: 2}) {
		t.Fatalf("usage = %+v", resp.Usage)
	}
	if req := f.last(t); req.Path != "/model/meta.llama3-70b-instruct-v1:0/converse-stream" || req.Body["inferenceConfig"].(map[string]any)["maxTokens"] != float64(50) {
		t.Errorf("req = %+v", req)
	}
}

// closingWriterTo behaves like the SDK's request body wrapper: once closed,
// Read and WriteTo report io.EOF.
type closingWriterTo struct {
	r      *strings.Reader
	mu     sync.Mutex
	closed bool
}

func (b *closingWriterTo) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return 0, io.EOF
	}
	return b.r.Read(p)
}

func (b *closingWriterTo) WriteTo(w io.Writer) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return 0, io.EOF
	}
	return b.r.WriteTo(w)
}

func (b *closingWriterTo) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	return nil
}

type captureDo struct{ req *http.Request }

func (c *captureDo) Do(r *http.Request) (*http.Response, error) {
	c.req = r
	return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
}

// TestReadOnlyBodyClient pins why requests go through readOnlyBodyClient: the
// SDK closes the request body when the response arrives, and net/http's last
// read of it (io.Copy to check for bytes beyond Content-Length) must then see
// a plain end of body, not an io.EOF error that makes the transport drop the
// connection and the response being read.
func TestReadOnlyBodyClient(t *testing.T) {
	raw := &closingWriterTo{r: strings.NewReader("body")}
	_ = raw.Close()
	if _, err := io.Copy(io.Discard, raw); !errors.Is(err, io.EOF) {
		t.Fatalf("precondition: a closed WriterTo body fails io.Copy with io.EOF, got %v", err)
	}

	body := &closingWriterTo{r: strings.NewReader("body")}
	next := &captureDo{}
	req, _ := http.NewRequest(http.MethodPost, "http://bedrock.test/model/m/converse-stream", body)
	if _, err := (readOnlyBodyClient{next: next}).Do(req); err != nil {
		t.Fatal(err)
	}
	sent := next.req.Body
	if _, ok := sent.(io.WriterTo); ok {
		t.Fatal("the transport must not see io.WriterTo on the request body")
	}
	if got, err := io.ReadAll(io.LimitReader(sent, 4)); err != nil || string(got) != "body" {
		t.Fatalf("read %q, %v", got, err)
	}
	_ = body.Close() // what the SDK does once the response arrives
	if n, err := io.Copy(io.Discard, sent); n != 0 || err != nil {
		t.Fatalf("the length check after close = (%d, %v), want (0, nil)", n, err)
	}

	noBody, _ := http.NewRequest(http.MethodGet, "http://bedrock.test/", nil)
	noBody.Body = http.NoBody
	if _, err := (readOnlyBodyClient{next: next}).Do(noBody); err != nil || next.req.Body != http.NoBody {
		t.Fatalf("http.NoBody must pass unchanged (%v)", err)
	}
}

func TestConverseStreamErrors(t *testing.T) {
	ctx := context.Background()
	req := ai.ChatRequest{Model: "m", Messages: []ai.Message{{Role: "user", Content: []ai.Part{{Type: "text", Text: "x"}}}}}
	t.Run("exception", func(t *testing.T) {
		f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
			writeEventStream(t, w, []esEvent{
				{"event", "contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"a"}}`},
				{"exception", "throttlingException", `{"message":"Rate exceeded"}`},
			})
			// Like Bedrock, keep the connection open after the exception
			// frame; the client stops reading and closes it.
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		})
		_, err := newProvider(t, f, config.ProviderConfig{}).ChatStream(ctx, req, nil)
		var ue *ai.UpstreamError
		// Under heavy load the SDK's event-stream reader occasionally reports
		// a read error on the closing connection instead of the exception
		// frame; either way the request must fail as an upstream error.
		if !errors.As(err, &ue) || !(ue.Message == "Rate exceeded" || strings.Contains(ue.Message, "decode event")) {
			t.Fatalf("got %#v", err)
		}
	})
	t.Run("guardrail", func(t *testing.T) {
		f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
			writeEventStream(t, w, []esEvent{{"event", "messageStop", `{"stopReason":"guardrail_intervened"}`}})
		})
		resp, err := newProvider(t, f, config.ProviderConfig{}).ChatStream(ctx, req, nil)
		if err != nil || resp.StopReason != ai.StopRefusal {
			t.Fatalf("resp=%+v err=%v", resp, err)
		}
	})
	t.Run("truncated", func(t *testing.T) {
		f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
			writeEventStream(t, w, []esEvent{{"event", "contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"a"}}`}})
		})
		_, err := newProvider(t, f, config.ProviderConfig{}).ChatStream(ctx, req, nil)
		var ue *ai.UpstreamError
		// Depending on timing the SDK reports EOF (→ "ended unexpectedly")
		// or a read error on the closed connection; both must fail.
		if !errors.As(err, &ue) || !(strings.Contains(ue.Message, "unexpectedly") || strings.Contains(ue.Message, "decode event")) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("abort", func(t *testing.T) {
		f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
			writeEventStream(t, w, []esEvent{
				{"event", "contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"a"}}`},
				{"event", "messageStop", `{"stopReason":"end_turn"}`},
			})
		})
		stop := errors.New("stop")
		_, err := newProvider(t, f, config.ProviderConfig{}).ChatStream(ctx, req, func(string) error { return stop })
		if !errors.Is(err, stop) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestStopReasons(t *testing.T) {
	tests := map[string]string{
		"end_turn": ai.StopEndTurn, "max_tokens": ai.StopMaxTokens, "stop_sequence": ai.StopStopSequence,
		"guardrail_intervened": ai.StopRefusal, "content_filtered": ai.StopRefusal, "tool_use": ai.StopOther,
		"model_context_window_exceeded": ai.StopMaxTokens, "refusal": ai.StopRefusal,
	}
	for in, want := range tests {
		if got := mapStopReason(typesStopReason(in)); got != want {
			t.Errorf("%s -> %s, want %s", in, got, want)
		}
	}
}

func TestEmbedTitan(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		text := body["inputText"].(string)
		writeJSON(w, 200, map[string]any{"embedding": []float64{float64(len(text)), 0.5}, "inputTextTokenCount": 2})
	})
	p := newProvider(t, f, config.ProviderConfig{})
	resp, err := p.Embed(context.Background(), ai.EmbedRequest{Model: "amazon.titan-embed-text-v2:0", Input: []string{"a", "bb", "ccc", "dddd", "eeeee", "ffffff"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Embeddings) != 6 || f.count() != 6 || resp.Usage.InputTokens != 12 {
		t.Fatalf("resp=%+v calls=%d", resp, f.count())
	}
	for i, e := range resp.Embeddings {
		if e[0] != float64(i+1) {
			t.Errorf("embedding %d out of order: %v", i, e)
		}
	}
	if req := f.last(t); req.Path != "/model/amazon.titan-embed-text-v2:0/invoke" {
		t.Errorf("path = %s", req.Path)
	}
}

func TestEmbedTitanError(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		w.Header().Set("X-Amzn-ErrorType", "ValidationException")
		writeJSON(w, 400, map[string]any{"message": "Malformed input request"})
	})
	p := newProvider(t, f, config.ProviderConfig{})
	_, err := p.Embed(context.Background(), ai.EmbedRequest{Model: "amazon.titan-embed-text-v2:0", Input: []string{"a", "b"}})
	var ue *ai.UpstreamError
	if !errors.As(err, &ue) || ue.Status != 400 || ue.Message != "Malformed input request" {
		t.Fatalf("got %#v", err)
	}
}

func TestEmbedCohere(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		texts := body["texts"].([]any)
		vecs := make([][]float64, len(texts))
		for i := range texts {
			vecs[i] = []float64{float64(i)}
		}
		if strings.Contains(r.URL.Path, "v4") {
			writeJSON(w, 200, map[string]any{"id": "x", "embeddings": map[string]any{"float": vecs}})
			return
		}
		writeJSON(w, 200, map[string]any{"id": "x", "embeddings": vecs, "texts": texts})
	})
	p := newProvider(t, f, config.ProviderConfig{})
	input := make([]string, 100)
	for i := range input {
		input[i] = "t"
	}
	resp, err := p.Embed(context.Background(), ai.EmbedRequest{Model: "cohere.embed-english-v3", Input: input})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Embeddings) != 100 || f.count() != 2 || resp.Embeddings[96][0] != 0 {
		t.Fatalf("embeddings=%d calls=%d", len(resp.Embeddings), f.count())
	}
	if b := f.last(t).Body; b["input_type"] != "search_document" {
		t.Errorf("body = %v", b)
	}
	resp, err = p.Embed(context.Background(), ai.EmbedRequest{Model: "cohere.embed-v4:0", Input: []string{"a", "b"}})
	if err != nil || len(resp.Embeddings) != 2 || resp.Embeddings[1][0] != 1 {
		t.Fatalf("v4: resp=%+v err=%v", resp, err)
	}
	if _, err := p.Embed(context.Background(), ai.EmbedRequest{Model: "amazon.nova-pro-v1:0", Input: []string{"a"}}); !errors.Is(err, ai.ErrUnsupported) {
		t.Errorf("non-embedding model: %v", err)
	}
}

func TestImageNovaCanvas(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString(pngBytes)
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		n := int(body["imageGenerationConfig"].(map[string]any)["numberOfImages"].(float64))
		imgs := make([]string, n)
		for i := range imgs {
			imgs[i] = b64
		}
		writeJSON(w, 200, map[string]any{"images": imgs})
	})
	p := newProvider(t, f, config.ProviderConfig{})
	resp, err := p.Image(context.Background(), ai.ImageRequest{Model: "amazon.nova-canvas-v1:0", Prompt: "a lighthouse", Size: "1280x720", N: 7})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Images) != 7 || resp.Images[0].MediaType != "image/png" || f.count() != 2 {
		t.Fatalf("images=%d calls=%d", len(resp.Images), f.count())
	}
	b := f.last(t).Body
	if b["taskType"] != "TEXT_IMAGE" || b["textToImageParams"].(map[string]any)["text"] != "a lighthouse" {
		t.Errorf("body = %v", b)
	}
	cfg := b["imageGenerationConfig"].(map[string]any)
	if cfg["width"] != float64(1280) || cfg["height"] != float64(720) || cfg["numberOfImages"] != float64(2) {
		t.Errorf("config = %v", cfg)
	}

	errSrv := newFake(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		writeJSON(w, 200, map[string]any{"images": []string{}, "error": "This request has been blocked by our content filters."})
	})
	_, err = newProvider(t, errSrv, config.ProviderConfig{}).Image(context.Background(), ai.ImageRequest{Model: "amazon.titan-image-generator-v2:0", Prompt: "x"})
	var ue *ai.UpstreamError
	if !errors.As(err, &ue) || !strings.Contains(ue.Message, "content filters") {
		t.Fatalf("got %v", err)
	}
}

func TestImageStability(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString(pngBytes)
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		writeJSON(w, 200, map[string]any{"seeds": []int{1}, "finish_reasons": []any{nil}, "images": []string{b64}})
	})
	p := newProvider(t, f, config.ProviderConfig{})
	resp, err := p.Image(context.Background(), ai.ImageRequest{Model: "stability.sd3-5-large-v1:0", Prompt: "a cat", Size: "1792x1024", N: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Images) != 2 || f.count() != 2 {
		t.Fatalf("images=%d calls=%d", len(resp.Images), f.count())
	}
	if b := f.last(t).Body; b["prompt"] != "a cat" || b["aspect_ratio"] != "16:9" || b["output_format"] != "png" {
		t.Errorf("body = %v", b)
	}
	if _, err := p.Image(context.Background(), ai.ImageRequest{Model: "amazon.nova-pro-v1:0", Prompt: "x"}); !errors.Is(err, ai.ErrUnsupported) {
		t.Errorf("non-image model: %v", err)
	}
	if _, err := p.Image(context.Background(), ai.ImageRequest{Model: "amazon.nova-canvas-v1:0"}); !errors.Is(err, ai.ErrBadRequest) {
		t.Errorf("empty prompt: %v", err)
	}
}

func TestParseSize(t *testing.T) {
	tests := []struct {
		in   string
		w, h int
	}{
		{"", 1024, 1024}, {"1024x1024", 1024, 1024}, {"1280x720", 1280, 720}, {"100x100", 320, 320},
		{"5000x5000", 4096, 4096}, {"1000x1001", 1008, 1008}, {"bogus", 1024, 1024}, {"512X768", 512, 768},
	}
	for _, tt := range tests {
		if w, h := parseSize(tt.in); w != tt.w || h != tt.h {
			t.Errorf("parseSize(%q) = %d,%d want %d,%d", tt.in, w, h, tt.w, tt.h)
		}
	}
	for in, want := range map[string]string{"": "", "1024x1024": "1:1", "1024x1792": "9:16", "3:2": "3:2", "1536x1024": "3:2"} {
		if got := stabilityAspect(in); got != want {
			t.Errorf("stabilityAspect(%q) = %q, want %q", in, got, want)
		}
	}
}

func typesStopReason(s string) types.StopReason { return types.StopReason(s) }

func TestConvertErr(t *testing.T) {
	p := &Provider{name: "br"}
	ctx := context.Background()
	tests := []struct {
		err    error
		status int
		msg    string
	}{
		{&smithy.GenericAPIError{Code: "ThrottlingException", Message: "Rate exceeded"}, 429, "Rate exceeded"},
		{&smithy.GenericAPIError{Code: "ModelStreamErrorException", Message: "boom"}, 502, "boom"},
		{&smithy.GenericAPIError{Code: "ValidationException"}, 400, "ValidationException"},
		{&smithy.GenericAPIError{Code: "SomethingNew", Message: "x"}, 0, "x"},
		{errors.New("dial tcp: refused"), 0, "dial tcp: refused"},
	}
	for _, tt := range tests {
		err := p.convertErr(ctx, tt.err)
		var ue *ai.UpstreamError
		if !errors.As(err, &ue) || ue.Status != tt.status || ue.Message != tt.msg || ue.Provider != "br" {
			t.Errorf("convertErr(%v) = %#v", tt.err, err)
		}
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := p.convertErr(cctx, errors.New("x")); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled: %v", err)
	}
	if err := p.convertErr(ctx, fmt.Errorf("op: %w", context.DeadlineExceeded)); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("deadline: %v", err)
	}
}
