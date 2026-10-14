package router

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/thasami/backyard/internal/ai"
	"github.com/thasami/backyard/internal/ai/internal/imagefetch"
)

func TestRawModelIDValidation(t *testing.T) {
	cfg := baseConfig()
	cfg.AllowRawModels = true
	f := newFixture(t, cfg)
	ctx := context.Background()
	good := []string{
		"oai/gpt-5", "oai/llama3.2:3b", "oai/@cf/meta/llama-3.1-8b-instruct", "oai/meta-llama/Llama-3.1-8B-Instruct",
		"claude/claude-opus-4-5@20251101", "oai/arn:aws:bedrock:us-east-1:123456789012:inference-profile/us.anthropic.claude-opus-4-8-v1:0",
		"oai/openai/gpt-4o:online", "oai/accounts/fireworks/models/llama-v3p1-8b-instruct",
	}
	for _, alias := range good {
		if _, err := f.svc.Chat(ctx, alias, ai.ChatRequest{Messages: user("x")}); err != nil {
			t.Errorf("%q rejected: %v", alias, err)
		}
	}
	bad := []string{
		"claude/../../../projects/other/locations/us/publishers/google/models/gemini",
		"claude/x/../y", "claude/./x", "claude/x//y", "claude/x/", "claude/x?y=1", "claude/x#y",
		"claude/x y", "claude/x\ny", "claude/x%2F..", "claude/-x", "claude/" + strings.Repeat("a", maxRawModelLen+1),
	}
	before := len(f.claude.chats)
	for _, alias := range bad {
		if _, err := f.svc.Chat(ctx, alias, ai.ChatRequest{Messages: user("x")}); !errors.Is(err, ai.ErrBadRequest) {
			t.Errorf("%q: err = %v, want ErrBadRequest", alias, err)
		}
	}
	if len(f.claude.chats) != before {
		t.Errorf("invalid raw model ids reached the provider")
	}
	// Configured aliases are not subject to raw-id rules, and raw ids stay
	// unknown when raw models are disabled.
	cfg.AllowRawModels = false
	f2 := newFixture(t, cfg)
	if _, err := f2.svc.Chat(ctx, "claude/x/../y", ai.ChatRequest{Messages: user("x")}); !errors.Is(err, ai.ErrUnknownModel) {
		t.Errorf("raw disabled: %v", err)
	}
}

func TestKeyedUserIDs(t *testing.T) {
	cfg := baseConfig()
	p := &fakeProvider{name: "claude"}
	o := &fakeProvider{name: "oai"}
	svc, err := New(context.Background(), cfg, Options{
		Providers: map[string]ai.Provider{"claude": p, "oai": o},
		UserIDKey: []byte("server-secret-0123456789abcdef0123456789"),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := svc.Chat(ctx, "", ai.ChatRequest{Messages: user("x"), User: "alice@example.com"}); err != nil {
		t.Fatal(err)
	}
	got := p.lastChat(t).User
	want := ai.KeyedUserID([]byte("server-secret-0123456789abcdef0123456789"), "alice@example.com")
	if got != want || got == ai.OpaqueUserID("alice@example.com") || strings.Contains(got, "alice") {
		t.Errorf("user id = %q, want keyed %q", got, want)
	}
	if _, _, err := svc.Embed(ctx, "", ai.EmbedRequest{Input: []string{"a"}, User: "alice@example.com"}); err != nil {
		t.Fatal(err)
	}
	o.mu.Lock()
	eu := o.embeds[len(o.embeds)-1].User
	o.mu.Unlock()
	if eu != want {
		t.Errorf("embed user id = %q, want %q", eu, want)
	}
}

func TestRemoteImageBudget(t *testing.T) {
	// Each image is just under the per-image limit; three of them exceed the
	// per-request budget for remote images.
	img := append(append([]byte{}, pngBytes...), bytes.Repeat([]byte{0}, imagefetch.MaxBytes-len(pngBytes)-1)...)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(img)
	}))
	defer srv.Close()

	cfg := baseConfig()
	f := newFixture(t, cfg)
	var fetchErrs []error
	f.claude.chatFn = func(ctx context.Context, req ai.ChatRequest) (*ai.ChatResponse, error) {
		for range 4 {
			_, _, err := imagefetch.Get(ctx, srv.Client(), srv.URL+"/x.png")
			fetchErrs = append(fetchErrs, err)
		}
		return &ai.ChatResponse{Content: "ok", StopReason: ai.StopEndTurn}, nil
	}
	if _, err := f.svc.Chat(context.Background(), "smart", ai.ChatRequest{Messages: user("x")}); err != nil {
		t.Fatal(err)
	}
	ok := 0
	for _, err := range fetchErrs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, imagefetch.ErrBudgetExceeded):
			t.Errorf("unexpected fetch error: %v", err)
		}
	}
	if want := MaxRemoteImageBytes / imagefetch.MaxBytes; ok != want {
		t.Errorf("%d downloads succeeded, want %d (budget %d bytes)", ok, want, MaxRemoteImageBytes)
	}
}

func TestGeneratedImageTypesComeFromContent(t *testing.T) {
	f := newFixture(t, baseConfig())
	ctx := context.Background()
	jpeg := []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00")
	avif := []byte("\x00\x00\x00\x1cftypavif\x00\x00\x00\x00")
	f.oai.imageFn = func(context.Context, ai.ImageRequest) (*ai.ImageResponse, error) {
		return &ai.ImageResponse{Images: []ai.Image{
			{Data: pngBytes, MediaType: "text/html"}, // declared type ignored
			{Data: jpeg, MediaType: "image/png"},
			{Data: avif},
		}}, nil
	}
	resp, _, err := f.svc.Image(ctx, "", ai.ImageRequest{Prompt: "x", N: 3})
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"image/png", "image/jpeg", "image/avif"} {
		if got := resp.Images[i].MediaType; got != want {
			t.Errorf("image %d: media type %q, want %q", i, got, want)
		}
	}

	for name, data := range map[string][]byte{
		"svg":  []byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`),
		"html": []byte("<html><script>alert(1)</script></html>"),
		"junk": []byte("not an image at all"),
	} {
		f.oai.imageFn = func(context.Context, ai.ImageRequest) (*ai.ImageResponse, error) {
			return &ai.ImageResponse{Images: []ai.Image{{Data: data, MediaType: "image/svg+xml"}}}, nil
		}
		var ue *ai.UpstreamError
		if _, _, err := f.svc.Image(ctx, "", ai.ImageRequest{Prompt: "x"}); !errors.As(err, &ue) {
			t.Errorf("%s accepted as a generated image: %v", name, err)
		}
	}
}

func TestWhitespaceOnlyTurnsDropped(t *testing.T) {
	f := newFixture(t, baseConfig())
	_, err := f.svc.Chat(context.Background(), "", ai.ChatRequest{Messages: []ai.Message{
		{Role: "user", Content: []ai.Part{{Type: "text", Text: "Hi"}}},
		{Role: "assistant", Content: []ai.Part{{Type: "text", Text: "  \n\t"}}},
		{Role: "user", Content: []ai.Part{{Type: "text", Text: "there"}, {Type: "text", Text: " "}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	got := f.claude.lastChat(t).Messages
	if len(got) != 1 || got[0].Role != "user" || ai.TextOf(got[0].Content) != "Hi\n\nthere " {
		t.Fatalf("messages = %+v", got)
	}
	// A conversation of only whitespace is a bad request.
	_, err = f.svc.Chat(context.Background(), "", ai.ChatRequest{Messages: user("   ")})
	if !errors.Is(err, ai.ErrBadRequest) {
		t.Fatalf("err = %v", err)
	}
}

func TestUsageEstimatedWhenMissing(t *testing.T) {
	f := newFixture(t, baseConfig())
	ctx := context.Background()
	f.claude.chatFn = func(context.Context, ai.ChatRequest) (*ai.ChatResponse, error) {
		return &ai.ChatResponse{Content: strings.Repeat("a", 400), StopReason: ai.StopEndTurn}, nil
	}
	resp, err := f.svc.ChatStream(ctx, "", ai.ChatRequest{System: strings.Repeat("s", 40), Messages: []ai.Message{{Role: "user", Content: []ai.Part{
		{Type: "text", Text: strings.Repeat("u", 80)}, {Type: "image", Data: pngBytes, MediaType: "image/png"},
	}}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The global system prompt of baseConfig (if any) adds to the input estimate.
	if resp.Usage.OutputTokens != 100 || resp.Usage.InputTokens < 30+estimatedImageTokens {
		t.Errorf("estimated usage = %+v", resp.Usage)
	}

	// Reported usage is never replaced.
	f.claude.chatFn = func(context.Context, ai.ChatRequest) (*ai.ChatResponse, error) {
		return &ai.ChatResponse{Content: "hello", Usage: ai.Usage{InputTokens: 7, OutputTokens: 1}}, nil
	}
	resp, err = f.svc.Chat(ctx, "", ai.ChatRequest{Messages: user("x")})
	if err != nil || resp.Usage != (ai.Usage{InputTokens: 7, OutputTokens: 1}) {
		t.Fatalf("usage = %+v err = %v", resp.Usage, err)
	}

	// Embeddings without usage get an estimate from the input size.
	er, _, err := f.svc.Embed(ctx, "", ai.EmbedRequest{Input: []string{strings.Repeat("e", 40), "abcd"}})
	if err != nil || er.Usage.InputTokens != 11 {
		t.Fatalf("embed usage = %+v err = %v", er.Usage, err)
	}
}
