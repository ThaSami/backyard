package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/ai"
	"github.com/thasami/backyard/internal/config"
)

var pngBytes = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89\x00\x00\x00\rIDATx\x9cc\xf8\x0f\x00\x00\x01\x01\x00\x05\x18\xd8N\x00\x00\x00\x00IEND\xaeB`\x82")

// fakeProvider records requests and returns canned responses.
type fakeProvider struct {
	name string

	mu        sync.Mutex
	chats     []ai.ChatRequest
	images    []ai.ImageRequest
	embeds    []ai.EmbedRequest
	deadlines []time.Duration

	chatFn  func(ctx context.Context, req ai.ChatRequest) (*ai.ChatResponse, error)
	imageFn func(ctx context.Context, req ai.ImageRequest) (*ai.ImageResponse, error)
	embedFn func(ctx context.Context, req ai.EmbedRequest) (*ai.EmbedResponse, error)
}

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) record(ctx context.Context) {
	if dl, ok := ctx.Deadline(); ok {
		f.deadlines = append(f.deadlines, time.Until(dl))
	}
}

func (f *fakeProvider) Chat(ctx context.Context, req ai.ChatRequest) (*ai.ChatResponse, error) {
	f.mu.Lock()
	f.chats = append(f.chats, req)
	f.record(ctx)
	fn := f.chatFn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, req)
	}
	return &ai.ChatResponse{Content: "ok", Model: req.Model, StopReason: ai.StopEndTurn, Usage: ai.Usage{InputTokens: 1, OutputTokens: 2}}, nil
}

func (f *fakeProvider) ChatStream(ctx context.Context, req ai.ChatRequest, onDelta func(string) error) (*ai.ChatResponse, error) {
	resp, err := f.Chat(ctx, req)
	if err != nil {
		return nil, err
	}
	for _, w := range strings.SplitAfter(resp.Content, " ") {
		if err := onDelta(w); err != nil {
			return nil, err
		}
	}
	return resp, nil
}

func (f *fakeProvider) Image(ctx context.Context, req ai.ImageRequest) (*ai.ImageResponse, error) {
	f.mu.Lock()
	f.images = append(f.images, req)
	f.record(ctx)
	fn := f.imageFn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, req)
	}
	out := &ai.ImageResponse{}
	for range req.N {
		out.Images = append(out.Images, ai.Image{Data: pngBytes, MediaType: "image/png"})
	}
	return out, nil
}

func (f *fakeProvider) Embed(ctx context.Context, req ai.EmbedRequest) (*ai.EmbedResponse, error) {
	f.mu.Lock()
	f.embeds = append(f.embeds, req)
	f.record(ctx)
	fn := f.embedFn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, req)
	}
	out := &ai.EmbedResponse{}
	for range req.Input {
		out.Embeddings = append(out.Embeddings, []float64{1, 2})
	}
	return out, nil
}

func (f *fakeProvider) lastChat(t *testing.T) ai.ChatRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.chats) == 0 {
		t.Fatal("no chat request")
	}
	return f.chats[len(f.chats)-1]
}

func boolPtr(b bool) *bool { return &b }

func ptr[T any](v T) *T { return &v }

// baseConfig has two providers and a mix of aliases.
func baseConfig() config.AIConfig {
	return config.AIConfig{
		DefaultModel: "smart",
		Providers: map[string]config.ProviderConfig{
			"claude": {Type: "anthropic"},
			"oai":    {Type: "openai"},
		},
		Models: map[string]config.ModelConfig{
			"smart":  {Provider: "claude", Model: "claude-opus-5-5", Kind: "chat", Description: "Best quality"},
			"fast":   {Provider: "claude", Model: "claude-haiku-5-5", MaxTokens: 1000, Effort: "low"},
			"long":   {Provider: "claude", Model: "claude-sonnet-5-5", MaxTokens: 32000},
			"gpt":    {Provider: "oai", Model: "gpt-5"},
			"image":  {Provider: "oai", Model: "gpt-image-1", Kind: "image"},
			"embed":  {Provider: "oai", Model: "text-embedding-3-small", Kind: "embedding"},
			"embed2": {Provider: "oai", Model: "text-embedding-3-large", Kind: "embedding"},
		},
		MaxTokens: 8192,
	}
}

type fixture struct {
	svc    ai.Service
	claude *fakeProvider
	oai    *fakeProvider
}

func newFixture(t *testing.T, cfg config.AIConfig) fixture {
	t.Helper()
	f := fixture{claude: &fakeProvider{name: "claude"}, oai: &fakeProvider{name: "oai"}}
	svc, err := New(context.Background(), cfg, Options{Providers: map[string]ai.Provider{"claude": f.claude, "oai": f.oai}})
	if err != nil {
		t.Fatal(err)
	}
	f.svc = svc
	return f
}

func user(s string) []ai.Message {
	return []ai.Message{{Role: "user", Content: []ai.Part{{Type: "text", Text: s}}}}
}

func TestDisabled(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.AIConfig
	}{
		{"no models", config.AIConfig{}},
		{"explicitly disabled", func() config.AIConfig { c := baseConfig(); c.Enabled = boolPtr(false); return c }()},
		{"providers without models", config.AIConfig{Providers: map[string]config.ProviderConfig{"oai": {Type: "openai"}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, err := New(context.Background(), tt.cfg, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if svc.Enabled() {
				t.Fatal("should be disabled")
			}
			ctx := context.Background()
			if _, err := svc.Chat(ctx, "", ai.ChatRequest{Messages: user("x")}); !errors.Is(err, ai.ErrDisabled) {
				t.Errorf("Chat: %v", err)
			}
			if _, err := svc.ChatStream(ctx, "", ai.ChatRequest{Messages: user("x")}, nil); !errors.Is(err, ai.ErrDisabled) {
				t.Errorf("ChatStream: %v", err)
			}
			if _, _, err := svc.Image(ctx, "", ai.ImageRequest{Prompt: "x"}); !errors.Is(err, ai.ErrDisabled) {
				t.Errorf("Image: %v", err)
			}
			if _, _, err := svc.Embed(ctx, "", ai.EmbedRequest{Input: []string{"x"}}); !errors.Is(err, ai.ErrDisabled) {
				t.Errorf("Embed: %v", err)
			}
			def, models := svc.Models()
			if def != "" || models == nil || len(models) != 0 {
				t.Errorf("Models() = %q %v", def, models)
			}
		})
	}
}

func TestModels(t *testing.T) {
	f := newFixture(t, baseConfig())
	if !f.svc.Enabled() {
		t.Fatal("should be enabled")
	}
	def, models := f.svc.Models()
	if def != "smart" {
		t.Errorf("default = %q", def)
	}
	var names []string
	for _, m := range models {
		names = append(names, m.Name)
		if m.Model != "" {
			t.Errorf("model ids must be hidden by default: %+v", m)
		}
	}
	if strings.Join(names, ",") != "embed,embed2,fast,gpt,image,long,smart" {
		t.Errorf("names = %v", names)
	}
	smart := models[len(models)-1]
	if smart.Kind != "chat" || smart.Provider != "claude" || smart.Description != "Best quality" {
		t.Errorf("smart = %+v", smart)
	}
	// The returned slice is a copy.
	models[0].Name = "mutated"
	if _, again := f.svc.Models(); again[0].Name != "embed" {
		t.Error("Models must return a copy")
	}

	cfg := baseConfig()
	cfg.ExposeModelIDs = true
	f = newFixture(t, cfg)
	_, models = f.svc.Models()
	for _, m := range models {
		if m.Model == "" {
			t.Errorf("expose_model_ids: %+v", m)
		}
	}
}

func TestAliasResolution(t *testing.T) {
	f := newFixture(t, baseConfig())
	ctx := context.Background()
	tests := []struct {
		alias     string
		wantErr   error
		wantModel string
		wantAlias string
	}{
		{"", nil, "claude-opus-5-5", "smart"},
		{"smart", nil, "claude-opus-5-5", "smart"},
		{" fast ", nil, "claude-haiku-5-5", "fast"},
		{"gpt", nil, "gpt-5", "gpt"},
		{"nope", ai.ErrUnknownModel, "", ""},
		{"claude/claude-opus-4-8", ai.ErrUnknownModel, "", ""}, // raw models disabled
		{"image", ai.ErrBadRequest, "", ""},                    // kind mismatch
		{"embed", ai.ErrBadRequest, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.alias, func(t *testing.T) {
			resp, err := f.svc.Chat(ctx, tt.alias, ai.ChatRequest{Messages: user("hi")})
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("want %v, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if resp.Model != tt.wantAlias {
				t.Errorf("response model = %q, want alias %q", resp.Model, tt.wantAlias)
			}
			var got string
			if strings.HasPrefix(tt.wantModel, "claude") {
				got = f.claude.lastChat(t).Model
			} else {
				got = f.oai.lastChat(t).Model
			}
			if got != tt.wantModel {
				t.Errorf("provider model = %q, want %q", got, tt.wantModel)
			}
		})
	}
}

func TestRawModels(t *testing.T) {
	cfg := baseConfig()
	cfg.AllowRawModels = true
	f := newFixture(t, cfg)
	ctx := context.Background()
	resp, err := f.svc.Chat(ctx, "claude/claude-opus-4-8", ai.ChatRequest{Messages: user("hi")})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Model != "claude/claude-opus-4-8" || f.claude.lastChat(t).Model != "claude-opus-4-8" {
		t.Errorf("resp=%+v provider model=%q", resp, f.claude.lastChat(t).Model)
	}
	// Model ids may contain slashes themselves.
	if _, err := f.svc.Chat(ctx, "oai/meta-llama/llama-3-70b", ai.ChatRequest{Messages: user("hi")}); err != nil {
		t.Fatal(err)
	}
	if got := f.oai.lastChat(t).Model; got != "meta-llama/llama-3-70b" {
		t.Errorf("model = %q", got)
	}
	for _, bad := range []string{"unknown/model", "claude/", "/x"} {
		if _, err := f.svc.Chat(ctx, bad, ai.ChatRequest{Messages: user("hi")}); !errors.Is(err, ai.ErrUnknownModel) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	// Raw ids work for images and embeddings too.
	if _, alias, err := f.svc.Image(ctx, "oai/dall-e-3", ai.ImageRequest{Prompt: "x"}); err != nil || alias != "oai/dall-e-3" {
		t.Errorf("image raw: %q %v", alias, err)
	}
}

func TestRawOnlyConfig(t *testing.T) {
	p := &fakeProvider{name: "oai"}
	svc, err := New(context.Background(), config.AIConfig{AllowRawModels: true}, Options{Providers: map[string]ai.Provider{"oai": p}})
	if err != nil {
		t.Fatal(err)
	}
	if !svc.Enabled() {
		t.Fatal("raw-only config with a provider should be enabled")
	}
	if _, err := svc.Chat(context.Background(), "", ai.ChatRequest{Messages: user("x")}); !errors.Is(err, ai.ErrUnknownModel) {
		t.Errorf("no default chat model: %v", err)
	}
	if _, err := svc.Chat(context.Background(), "oai/gpt-5", ai.ChatRequest{Messages: user("x")}); err != nil {
		t.Errorf("raw: %v", err)
	}
}

func TestMaxTokens(t *testing.T) {
	cfg := baseConfig()
	cfg.MaxTokens = 2048
	f := newFixture(t, cfg)
	ctx := context.Background()
	tests := []struct {
		alias string
		req   int
		want  int
	}{
		{"smart", 0, 2048},      // default min(4096, global cap)
		{"smart", 100, 100},     // under cap
		{"smart", 100000, 2048}, // clamped to global cap
		{"fast", 0, 1000},       // alias cap below default
		{"fast", 5000, 1000},    // clamped to alias cap
		{"long", 0, 2048},       // an alias cap above the global cap does not raise it
		{"long", 20000, 2048},   // global cap still applies (SPEC §5.4)
		{"long", 50000, 2048},
	}
	for _, tt := range tests {
		if _, err := f.svc.Chat(ctx, tt.alias, ai.ChatRequest{Messages: user("x"), MaxTokens: tt.req}); err != nil {
			t.Fatal(err)
		}
		if got := f.claude.lastChat(t).MaxTokens; got != tt.want {
			t.Errorf("%s max_tokens=%d: got %d, want %d", tt.alias, tt.req, got, tt.want)
		}
	}
	if _, err := f.svc.Chat(ctx, "smart", ai.ChatRequest{Messages: user("x"), MaxTokens: -1}); !errors.Is(err, ai.ErrBadRequest) {
		t.Errorf("negative max_tokens: %v", err)
	}

	// With a high global cap the alias cap is the binding one.
	cfg.MaxTokens = 64000
	f = newFixture(t, cfg)
	for _, tt := range []struct {
		alias     string
		req, want int
	}{
		{"long", 0, 4096}, {"long", 20000, 20000}, {"long", 50000, 32000}, {"smart", 50000, 50000}, {"fast", 5000, 1000},
	} {
		if _, err := f.svc.Chat(ctx, tt.alias, ai.ChatRequest{Messages: user("x"), MaxTokens: tt.req}); err != nil {
			t.Fatal(err)
		}
		if got := f.claude.lastChat(t).MaxTokens; got != tt.want {
			t.Errorf("global 64000, %s max_tokens=%d: got %d, want %d", tt.alias, tt.req, got, tt.want)
		}
	}

	// Defaults when the config has none.
	cfg.MaxTokens = 0
	f = newFixture(t, cfg)
	_, _ = f.svc.Chat(ctx, "smart", ai.ChatRequest{Messages: user("x"), MaxTokens: 1 << 20})
	if got := f.claude.lastChat(t).MaxTokens; got != defaultMaxTokens {
		t.Errorf("unset global cap: %d", got)
	}
}

func TestPrepareChat(t *testing.T) {
	cfg := baseConfig()
	cfg.SystemPrompt = "  Company policy: be kind.  "
	f := newFixture(t, cfg)
	ctx := context.Background()
	msgs := []ai.Message{
		{Role: "system", Content: []ai.Part{{Type: "text", Text: "From messages."}}},
		{Role: "User", Content: []ai.Part{{Type: "text", Text: "Hello"}}},
		{Role: "user", Content: []ai.Part{{Type: "text", Text: "Are you there?"}, {Type: "image", Data: pngBytes}}},
		{Role: "assistant", Content: []ai.Part{{Type: "text", Text: ""}}}, // empty → dropped
		{Role: "user", Content: []ai.Part{{Type: "image", URL: "data:image/png;base64,iVBORw0KGgo="}}},
		{Role: "assistant", Content: []ai.Part{{Text: "Yes."}}}, // empty type means text
		{Role: "user", Content: []ai.Part{{Type: "image", URL: "https://example.com/a.jpg", MediaType: "image/jpg"}}},
	}
	orig := fmt.Sprintf("%+v", msgs)
	_, err := f.svc.Chat(ctx, "smart", ai.ChatRequest{
		System: "Be brief.", Messages: msgs, Temperature: ptr(0.7), JSON: true, User: "alice@acme.com", Effort: "HIGH",
	})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%+v", msgs) != orig {
		t.Error("caller's messages were mutated")
	}
	got := f.claude.lastChat(t)
	if got.System != "Company policy: be kind.\n\nBe brief.\n\nFrom messages." {
		t.Errorf("system = %q", got.System)
	}
	if len(got.Messages) != 3 {
		t.Fatalf("messages = %+v", got.Messages)
	}
	first := got.Messages[0]
	if first.Role != "user" || len(first.Content) != 3 || first.Content[0].Text != "Hello\n\nAre you there?" {
		t.Errorf("merged user turn = %+v", first)
	}
	if first.Content[1].Type != "image" || first.Content[1].MediaType != "image/png" {
		t.Errorf("sniffed image = %+v", first.Content[1])
	}
	if first.Content[2].Type != "image" || len(first.Content[2].Data) == 0 || first.Content[2].URL != "" {
		t.Errorf("data URL image should be decoded: %+v", first.Content[2])
	}
	if got.Messages[1].Role != "assistant" || got.Messages[1].Content[0].Text != "Yes." {
		t.Errorf("assistant = %+v", got.Messages[1])
	}
	if u := got.Messages[2].Content[0]; u.URL != "https://example.com/a.jpg" || u.MediaType != "image/jpeg" {
		t.Errorf("url image = %+v", u)
	}
	if got.Temperature == nil || *got.Temperature != 0.7 || !got.JSON || got.Effort != "high" {
		t.Errorf("req = %+v", got)
	}
	if got.User == "" || strings.Contains(got.User, "alice") {
		t.Errorf("user id should be opaque: %q", got.User)
	}
}

func TestEffortOverride(t *testing.T) {
	f := newFixture(t, baseConfig())
	ctx := context.Background()
	if _, err := f.svc.Chat(ctx, "fast", ai.ChatRequest{Messages: user("x"), Effort: "max"}); err != nil {
		t.Fatal(err)
	}
	if got := f.claude.lastChat(t).Effort; got != "low" {
		t.Errorf("alias effort must override the request: %q", got)
	}
	if _, err := f.svc.Chat(ctx, "smart", ai.ChatRequest{Messages: user("x")}); err != nil {
		t.Fatal(err)
	}
	if got := f.claude.lastChat(t).Effort; got != "" {
		t.Errorf("no effort configured: %q", got)
	}
	if _, err := f.svc.Chat(ctx, "smart", ai.ChatRequest{Messages: user("x"), Effort: "ludicrous"}); !errors.Is(err, ai.ErrBadRequest) {
		t.Errorf("invalid effort: %v", err)
	}
}

func TestChatValidation(t *testing.T) {
	f := newFixture(t, baseConfig())
	ctx := context.Background()
	big := make([]byte, MaxImageBytes+1)
	copy(big, pngBytes)
	manyImages := make([]ai.Part, MaxImagesPerRequest+1)
	for i := range manyImages {
		manyImages[i] = ai.Part{Type: "image", Data: pngBytes, MediaType: "image/png"}
	}
	tests := []struct {
		name string
		req  ai.ChatRequest
	}{
		{"no messages", ai.ChatRequest{}},
		{"only system", ai.ChatRequest{Messages: []ai.Message{{Role: "system", Content: []ai.Part{{Type: "text", Text: "x"}}}}}},
		{"all empty", ai.ChatRequest{Messages: []ai.Message{{Role: "user"}, {Role: "assistant", Content: []ai.Part{{Type: "text"}}}}}},
		{"bad role", ai.ChatRequest{Messages: []ai.Message{{Role: "tool", Content: []ai.Part{{Type: "text", Text: "x"}}}}}},
		{"missing role", ai.ChatRequest{Messages: []ai.Message{{Content: []ai.Part{{Type: "text", Text: "x"}}}}}},
		{"bad part type", ai.ChatRequest{Messages: []ai.Message{{Role: "user", Content: []ai.Part{{Type: "audio"}}}}}},
		{"image without source", ai.ChatRequest{Messages: []ai.Message{{Role: "user", Content: []ai.Part{{Type: "image"}}}}}},
		{"image too big", ai.ChatRequest{Messages: []ai.Message{{Role: "user", Content: []ai.Part{{Type: "image", Data: big, MediaType: "image/png"}}}}}},
		{"bad media type", ai.ChatRequest{Messages: []ai.Message{{Role: "user", Content: []ai.Part{{Type: "image", Data: pngBytes, MediaType: "image/tiff"}}}}}},
		{"unknown bytes", ai.ChatRequest{Messages: []ai.Message{{Role: "user", Content: []ai.Part{{Type: "image", Data: []byte("hello world")}}}}}},
		{"file url", ai.ChatRequest{Messages: []ai.Message{{Role: "user", Content: []ai.Part{{Type: "image", URL: "file:///etc/passwd"}}}}}},
		{"relative url", ai.ChatRequest{Messages: []ai.Message{{Role: "user", Content: []ai.Part{{Type: "image", URL: "/a.png"}}}}}},
		{"bad data url", ai.ChatRequest{Messages: []ai.Message{{Role: "user", Content: []ai.Part{{Type: "image", URL: "data:image/png;base64,@@"}}}}}},
		{"assistant image", ai.ChatRequest{Messages: []ai.Message{{Role: "assistant", Content: []ai.Part{{Type: "image", Data: pngBytes}}}}}},
		{"system image", ai.ChatRequest{Messages: []ai.Message{{Role: "system", Content: []ai.Part{{Type: "image", Data: pngBytes}}}, {Role: "user", Content: []ai.Part{{Type: "text", Text: "x"}}}}}},
		{"too many images", ai.ChatRequest{Messages: []ai.Message{{Role: "user", Content: manyImages}}}},
		{"temperature high", ai.ChatRequest{Messages: user("x"), Temperature: ptr(2.5)}},
		{"temperature negative", ai.ChatRequest{Messages: user("x"), Temperature: ptr(-0.1)}},
		{"temperature NaN", ai.ChatRequest{Messages: user("x"), Temperature: ptr(math.NaN())}},
		{"untyped data", ai.ChatRequest{Messages: []ai.Message{{Role: "user", Content: []ai.Part{{Data: pngBytes}}}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := f.svc.Chat(ctx, "smart", tt.req); !errors.Is(err, ai.ErrBadRequest) {
				t.Fatalf("want ErrBadRequest, got %v", err)
			}
			if _, err := f.svc.ChatStream(ctx, "smart", tt.req, nil); !errors.Is(err, ai.ErrBadRequest) {
				t.Fatalf("stream: want ErrBadRequest, got %v", err)
			}
		})
	}
	if n := len(f.claude.chats); n != 0 {
		t.Errorf("invalid requests reached the provider %d times", n)
	}
}

func TestChatStream(t *testing.T) {
	f := newFixture(t, baseConfig())
	f.claude.chatFn = func(ctx context.Context, req ai.ChatRequest) (*ai.ChatResponse, error) {
		return &ai.ChatResponse{Content: "one two three", Model: req.Model, StopReason: ai.StopEndTurn}, nil
	}
	var deltas []string
	resp, err := f.svc.ChatStream(context.Background(), "", ai.ChatRequest{Messages: user("x")}, func(s string) error {
		deltas = append(deltas, s)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(deltas, "") != "one two three" || resp.Model != "smart" {
		t.Errorf("deltas=%v resp=%+v", deltas, resp)
	}
	// Streaming gets the long timeout.
	if dl := f.claude.deadlines[len(f.claude.deadlines)-1]; dl < 9*time.Minute {
		t.Errorf("stream deadline = %s", dl)
	}
	// nil onDelta is tolerated.
	if _, err := f.svc.ChatStream(context.Background(), "", ai.ChatRequest{Messages: user("x")}, nil); err != nil {
		t.Fatal(err)
	}
	stop := errors.New("client went away")
	_, err = f.svc.ChatStream(context.Background(), "", ai.ChatRequest{Messages: user("x")}, func(string) error { return stop })
	if !errors.Is(err, stop) {
		t.Errorf("onDelta error must be returned: %v", err)
	}
}

func TestJSONFencesStripped(t *testing.T) {
	f := newFixture(t, baseConfig())
	f.claude.chatFn = func(ctx context.Context, req ai.ChatRequest) (*ai.ChatResponse, error) {
		return &ai.ChatResponse{Content: "```json\n{\"a\": 1}\n```", StopReason: ""}, nil
	}
	resp, err := f.svc.Chat(context.Background(), "smart", ai.ChatRequest{Messages: user("x"), JSON: true})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != `{"a": 1}` || resp.StopReason != ai.StopOther || resp.Model != "smart" {
		t.Errorf("resp = %+v", resp)
	}
}

func TestTimeouts(t *testing.T) {
	cfg := baseConfig()
	cfg.RequestTimeout = config.Duration(50 * time.Millisecond)
	f := newFixture(t, cfg)
	block := func(ctx context.Context, req ai.ChatRequest) (*ai.ChatResponse, error) {
		<-ctx.Done()
		return nil, fmt.Errorf("upstream: %w", ctx.Err())
	}
	f.claude.chatFn = block

	_, err := f.svc.Chat(context.Background(), "smart", ai.ChatRequest{Messages: user("x")})
	var ue *ai.UpstreamError
	if !errors.As(err, &ue) || ue.Status != http.StatusGatewayTimeout || ue.Provider != "claude" {
		t.Fatalf("want 504 upstream error, got %#v", err)
	}

	// Caller cancellation is not an upstream failure.
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	_, err = f.svc.Chat(ctx, "smart", ai.ChatRequest{Messages: user("x")})
	if !errors.Is(err, context.Canceled) || errors.As(err, &ue) {
		t.Fatalf("want context.Canceled, got %v", err)
	}

	// A caller deadline shorter than ours is reported as the caller's.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel2()
	_, err = f.svc.Chat(ctx2, "smart", ai.ChatRequest{Messages: user("x")})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want caller deadline, got %v", err)
	}
}

func TestUpstreamErrorsPassThrough(t *testing.T) {
	f := newFixture(t, baseConfig())
	want := &ai.UpstreamError{Provider: "claude", Status: 429, Message: "rate limited"}
	f.claude.chatFn = func(context.Context, ai.ChatRequest) (*ai.ChatResponse, error) { return nil, want }
	_, err := f.svc.Chat(context.Background(), "smart", ai.ChatRequest{Messages: user("x")})
	var ue *ai.UpstreamError
	if !errors.As(err, &ue) || ue != want {
		t.Fatalf("got %v", err)
	}
	f.claude.chatFn = func(context.Context, ai.ChatRequest) (*ai.ChatResponse, error) { return nil, nil }
	_, err = f.svc.Chat(context.Background(), "smart", ai.ChatRequest{Messages: user("x")})
	if !errors.As(err, &ue) {
		t.Fatalf("nil response must become an upstream error: %v", err)
	}
}

func TestImage(t *testing.T) {
	f := newFixture(t, baseConfig())
	ctx := context.Background()
	resp, alias, err := f.svc.Image(ctx, "", ai.ImageRequest{Prompt: "  a red fox  ", Size: "1024x1024", User: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	if alias != "image" || len(resp.Images) != 1 {
		t.Fatalf("alias=%q resp=%+v", alias, resp)
	}
	got := f.oai.images[len(f.oai.images)-1]
	if got.Model != "gpt-image-1" || got.Prompt != "a red fox" || got.N != 1 || got.Size != "1024x1024" || got.User == "bob" {
		t.Errorf("provider request = %+v", got)
	}
	if dl := f.oai.deadlines[len(f.oai.deadlines)-1]; dl < 9*time.Minute {
		t.Errorf("image deadline = %s", dl)
	}

	for _, tt := range []struct {
		name  string
		alias string
		req   ai.ImageRequest
		want  error
	}{
		{"empty prompt", "", ai.ImageRequest{Prompt: " "}, ai.ErrBadRequest},
		{"too many", "", ai.ImageRequest{Prompt: "x", N: MaxImageCount + 1}, ai.ErrBadRequest},
		{"negative n", "", ai.ImageRequest{Prompt: "x", N: -1}, ai.ErrBadRequest},
		{"bad size", "", ai.ImageRequest{Prompt: "x", Size: "huge"}, ai.ErrBadRequest},
		{"long prompt", "", ai.ImageRequest{Prompt: strings.Repeat("a", MaxImagePromptChars+1)}, ai.ErrBadRequest},
		{"chat alias", "smart", ai.ImageRequest{Prompt: "x"}, ai.ErrBadRequest},
		{"unknown", "nope", ai.ImageRequest{Prompt: "x"}, ai.ErrUnknownModel},
	} {
		if _, _, err := f.svc.Image(ctx, tt.alias, tt.req); !errors.Is(err, tt.want) {
			t.Errorf("%s: want %v, got %v", tt.name, tt.want, err)
		}
	}
	for _, size := range []string{"auto", "1792x1024", "16:9", "512 x 512"} {
		if _, _, err := f.svc.Image(ctx, "", ai.ImageRequest{Prompt: "x", Size: size}); err != nil {
			t.Errorf("size %q: %v", size, err)
		}
	}

	// Provider output is checked and trimmed to n.
	f.oai.imageFn = func(context.Context, ai.ImageRequest) (*ai.ImageResponse, error) {
		return &ai.ImageResponse{Images: []ai.Image{{Data: pngBytes}, {Data: pngBytes}, {Data: pngBytes}}}, nil
	}
	resp, _, err = f.svc.Image(ctx, "", ai.ImageRequest{Prompt: "x", N: 2})
	if err != nil || len(resp.Images) != 2 || resp.Images[0].MediaType != "image/png" {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	f.oai.imageFn = func(context.Context, ai.ImageRequest) (*ai.ImageResponse, error) { return &ai.ImageResponse{}, nil }
	var ue *ai.UpstreamError
	if _, _, err := f.svc.Image(ctx, "", ai.ImageRequest{Prompt: "x"}); !errors.As(err, &ue) {
		t.Errorf("no images: %v", err)
	}
}

func TestEmbed(t *testing.T) {
	f := newFixture(t, baseConfig())
	ctx := context.Background()
	resp, alias, err := f.svc.Embed(ctx, "", ai.EmbedRequest{Input: []string{"a", "b"}})
	if err != nil || alias != "embed" || len(resp.Embeddings) != 2 {
		t.Fatalf("resp=%+v alias=%q err=%v", resp, alias, err)
	}
	if got := f.oai.embeds[len(f.oai.embeds)-1].Model; got != "text-embedding-3-small" {
		t.Errorf("model = %q", got)
	}
	if _, alias, err := f.svc.Embed(ctx, "embed2", ai.EmbedRequest{Input: []string{"a"}}); err != nil || alias != "embed2" {
		t.Errorf("embed2: %q %v", alias, err)
	}
	tooMany := make([]string, MaxEmbedInputs+1)
	for i := range tooMany {
		tooMany[i] = "x"
	}
	for _, tt := range []struct {
		name  string
		alias string
		in    []string
		want  error
	}{
		{"empty", "", nil, ai.ErrBadRequest},
		{"blank string", "", []string{"a", "  "}, ai.ErrBadRequest},
		{"too many", "", tooMany, ai.ErrBadRequest},
		{"too large", "", []string{strings.Repeat("x", MaxEmbedBytes+1)}, ai.ErrBadRequest},
		{"chat alias", "smart", []string{"x"}, ai.ErrBadRequest},
		{"image alias", "image", []string{"x"}, ai.ErrBadRequest},
	} {
		if _, _, err := f.svc.Embed(ctx, tt.alias, ai.EmbedRequest{Input: tt.in}); !errors.Is(err, tt.want) {
			t.Errorf("%s: want %v, got %v", tt.name, tt.want, err)
		}
	}
	f.oai.embedFn = func(context.Context, ai.EmbedRequest) (*ai.EmbedResponse, error) {
		return &ai.EmbedResponse{Embeddings: [][]float64{{1}}}, nil
	}
	var ue *ai.UpstreamError
	if _, _, err := f.svc.Embed(ctx, "", ai.EmbedRequest{Input: []string{"a", "b"}}); !errors.As(err, &ue) {
		t.Errorf("count mismatch: %v", err)
	}
}

func TestDefaultsByKind(t *testing.T) {
	cfg := config.AIConfig{
		Providers: map[string]config.ProviderConfig{"oai": {Type: "openai"}},
		Models: map[string]config.ModelConfig{
			"zeta":    {Provider: "oai", Model: "m1"},
			"alpha":   {Provider: "oai", Model: "m2"},
			"pics":    {Provider: "oai", Model: "img", Kind: "image"},
			"vectors": {Provider: "oai", Model: "emb", Kind: "embedding"},
		},
	}
	p := &fakeProvider{name: "oai"}
	svc, err := New(context.Background(), cfg, Options{Providers: map[string]ai.Provider{"oai": p}})
	if err != nil {
		t.Fatal(err)
	}
	def, _ := svc.Models()
	if def != "alpha" {
		t.Errorf("default chat = %q", def)
	}
	if _, alias, err := svc.Image(context.Background(), "", ai.ImageRequest{Prompt: "x"}); err != nil || alias != "pics" {
		t.Errorf("default image = %q %v", alias, err)
	}
	if _, alias, err := svc.Embed(context.Background(), "", ai.EmbedRequest{Input: []string{"x"}}); err != nil || alias != "vectors" {
		t.Errorf("default embed = %q %v", alias, err)
	}

	// No image model at all.
	delete(cfg.Models, "pics")
	svc, _ = New(context.Background(), cfg, Options{Providers: map[string]ai.Provider{"oai": p}})
	if _, _, err := svc.Image(context.Background(), "", ai.ImageRequest{Prompt: "x"}); !errors.Is(err, ai.ErrUnknownModel) {
		t.Errorf("no image model: %v", err)
	}
}

func TestNewErrors(t *testing.T) {
	fake := map[string]ai.Provider{"oai": &fakeProvider{name: "oai"}}
	mk := func(mut func(*config.AIConfig)) config.AIConfig {
		c := config.AIConfig{
			Providers: map[string]config.ProviderConfig{"oai": {Type: "openai"}},
			Models:    map[string]config.ModelConfig{"m": {Provider: "oai", Model: "gpt"}},
		}
		mut(&c)
		return c
	}
	tests := []struct {
		name string
		cfg  config.AIConfig
	}{
		{"unknown provider", mk(func(c *config.AIConfig) { c.Models["x"] = config.ModelConfig{Provider: "nope", Model: "m"} })},
		{"no model id", mk(func(c *config.AIConfig) { c.Models["x"] = config.ModelConfig{Provider: "oai"} })},
		{"bad kind", mk(func(c *config.AIConfig) {
			c.Models["x"] = config.ModelConfig{Provider: "oai", Model: "m", Kind: "audio"}
		})},
		{"slash alias", mk(func(c *config.AIConfig) { c.Models["a/b"] = config.ModelConfig{Provider: "oai", Model: "m"} })},
		{"bad effort", mk(func(c *config.AIConfig) {
			c.Models["x"] = config.ModelConfig{Provider: "oai", Model: "m", Effort: "huge"}
		})},
		{"negative max", mk(func(c *config.AIConfig) {
			c.Models["x"] = config.ModelConfig{Provider: "oai", Model: "m", MaxTokens: -5}
		})},
		{"unknown default", mk(func(c *config.AIConfig) { c.DefaultModel = "missing" })},
		{"wrong default kind", mk(func(c *config.AIConfig) {
			c.Models["img"] = config.ModelConfig{Provider: "oai", Model: "i", Kind: "image"}
			c.DefaultImageModel = "m"
		})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(context.Background(), tt.cfg, Options{Providers: fake}); err == nil {
				t.Fatal("want error")
			}
		})
	}
	// A provider that cannot be built is fatal when referenced.
	cfg := config.AIConfig{
		Providers: map[string]config.ProviderConfig{"az": {Type: "azure-openai"}}, // no endpoint
		Models:    map[string]config.ModelConfig{"m": {Provider: "az", Model: "dep"}},
	}
	if _, err := New(context.Background(), cfg, Options{}); err == nil {
		t.Fatal("broken referenced provider must fail")
	}
	// …but only logged when unreferenced and raw models are allowed.
	cfg.Models = map[string]config.ModelConfig{"m": {Provider: "oai", Model: "gpt"}}
	cfg.Providers["oai"] = config.ProviderConfig{Type: "openai"}
	cfg.AllowRawModels = true
	svc, err := New(context.Background(), cfg, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Chat(context.Background(), "az/dep", ai.ChatRequest{Messages: user("x")}); !errors.Is(err, ai.ErrUnknownModel) {
		t.Errorf("skipped provider should be unknown: %v", err)
	}
	// Unknown provider type.
	cfg = config.AIConfig{
		Providers: map[string]config.ProviderConfig{"x": {Type: "cohere"}},
		Models:    map[string]config.ModelConfig{"m": {Provider: "x", Model: "c"}},
	}
	if _, err := New(context.Background(), cfg, Options{}); err == nil {
		t.Fatal("unknown provider type must fail")
	}
}

// TestBuildsRealProviders wires every provider type from config (no network
// I/O at construction) and drives one real provider end to end against a
// fake OpenAI-compatible server.
func TestBuildsRealProviders(t *testing.T) {
	var (
		mu   sync.Mutex
		seen []map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		mu.Lock()
		seen = append(seen, body)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"llama3","choices":[{"message":{"role":"assistant","content":"hi there"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`))
	}))
	defer srv.Close()

	cfg := config.AIConfig{
		DefaultModel: "local",
		SystemPrompt: "Org prompt.",
		Providers: map[string]config.ProviderConfig{
			"anthropic": {APIKey: "sk-ant-x"}, // type defaults to the key
			"vertex":    {Type: "anthropic", Platform: "vertex", ProjectID: "p"},
			"mantle":    {Type: "anthropic", Platform: "bedrock", Region: "us-east-1"},
			"ollama":    {Type: "ollama", BaseURL: srv.URL + "/v1"},
			"azure":     {Type: "azure-openai", Endpoint: "https://r.openai.azure.com", APIKey: "k"},
			"bedrock":   {Type: "bedrock", Region: "us-west-2"},
			"gemini":    {Type: "gemini", APIKey: "AIza"},
		},
		Models: map[string]config.ModelConfig{
			"smart":  {Provider: "anthropic", Model: "claude-opus-5-5"},
			"vtx":    {Provider: "vertex", Model: "claude-sonnet-5-5"},
			"mnt":    {Provider: "mantle", Model: "anthropic.claude-opus-5-5"},
			"local":  {Provider: "ollama", Model: "llama3"},
			"az":     {Provider: "azure", Model: "gpt-4o"},
			"nova":   {Provider: "bedrock", Model: "amazon.nova-pro-v1:0"},
			"gem":    {Provider: "gemini", Model: "gemini-2.5-flash"},
			"canvas": {Provider: "bedrock", Model: "amazon.nova-canvas-v1:0", Kind: "image"},
		},
	}
	svc, err := New(context.Background(), cfg, Options{HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := svc.Chat(context.Background(), "", ai.ChatRequest{Messages: user("hello"), MaxTokens: 20})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "hi there" || resp.Model != "local" || resp.Usage.OutputTokens != 2 {
		t.Fatalf("resp = %+v", resp)
	}
	mu.Lock()
	defer mu.Unlock()
	msgs := seen[0]["messages"].([]any)
	if sys := msgs[0].(map[string]any); sys["role"] != "system" || sys["content"] != "Org prompt." {
		t.Errorf("system prompt not prepended: %v", msgs)
	}
	if seen[0]["max_tokens"] != float64(20) {
		t.Errorf("body = %v", seen[0])
	}
	// Claude cannot embed: the error surfaces as unsupported.
	cfg.Models["claude-embed"] = config.ModelConfig{Provider: "anthropic", Model: "claude-opus-5-5", Kind: "embedding"}
	svc, err = New(context.Background(), cfg, Options{HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Embed(context.Background(), "claude-embed", ai.EmbedRequest{Input: []string{"x"}}); !errors.Is(err, ai.ErrUnsupported) {
		t.Errorf("want ErrUnsupported, got %v", err)
	}
}

func TestConcurrentUse(t *testing.T) {
	f := newFixture(t, baseConfig())
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			alias := []string{"smart", "fast", "gpt", ""}[i%4]
			if _, err := f.svc.Chat(context.Background(), alias, ai.ChatRequest{Messages: user("x")}); err != nil {
				t.Error(err)
			}
			f.svc.Models()
		}()
	}
	wg.Wait()
}
