package router

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/ai"
	"github.com/thasami/backyard/internal/config"
)

// TestLiveProviders runs a short chat + stream against real upstreams when
// credentials are present in the environment. It is skipped otherwise.
//
//	BACKYARD_TEST_ANTHROPIC_API_KEY   [BACKYARD_TEST_ANTHROPIC_MODEL, default claude-haiku-5-5]
//	BACKYARD_TEST_OPENAI_API_KEY      [BACKYARD_TEST_OPENAI_MODEL, default gpt-5-mini]
//	BACKYARD_TEST_GEMINI_API_KEY      [BACKYARD_TEST_GEMINI_MODEL, default gemini-2.5-flash]
//	BACKYARD_TEST_OLLAMA_URL          [BACKYARD_TEST_OLLAMA_MODEL, default llama3.2] e.g. http://127.0.0.1:11434/v1
//	BACKYARD_TEST_CLOUDFLARE_ACCOUNT_ID + BACKYARD_TEST_CLOUDFLARE_API_TOKEN
//	                                  [BACKYARD_TEST_CLOUDFLARE_MODEL, default @cf/meta/llama-3.1-8b-instruct]
func TestLiveProviders(t *testing.T) {
	type live struct {
		name  string
		pc    config.ProviderConfig
		model string
	}
	env := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	var cases []live
	if k := os.Getenv("BACKYARD_TEST_ANTHROPIC_API_KEY"); k != "" {
		cases = append(cases, live{"anthropic", config.ProviderConfig{Type: "anthropic", APIKey: k},
			env("BACKYARD_TEST_ANTHROPIC_MODEL", "claude-haiku-5-5")})
	}
	if k := os.Getenv("BACKYARD_TEST_OPENAI_API_KEY"); k != "" {
		cases = append(cases, live{"openai", config.ProviderConfig{Type: "openai", APIKey: k},
			env("BACKYARD_TEST_OPENAI_MODEL", "gpt-5-mini")})
	}
	if k := os.Getenv("BACKYARD_TEST_GEMINI_API_KEY"); k != "" {
		cases = append(cases, live{"gemini", config.ProviderConfig{Type: "gemini", APIKey: k},
			env("BACKYARD_TEST_GEMINI_MODEL", "gemini-2.5-flash")})
	}
	if u := os.Getenv("BACKYARD_TEST_OLLAMA_URL"); u != "" {
		cases = append(cases, live{"ollama", config.ProviderConfig{Type: "ollama", BaseURL: u},
			env("BACKYARD_TEST_OLLAMA_MODEL", "llama3.2")})
	}
	if acct, tok := os.Getenv("BACKYARD_TEST_CLOUDFLARE_ACCOUNT_ID"), os.Getenv("BACKYARD_TEST_CLOUDFLARE_API_TOKEN"); acct != "" && tok != "" {
		cases = append(cases, live{"workers-ai", config.ProviderConfig{Type: "openai", APIKey: tok,
			BaseURL: "https://api.cloudflare.com/client/v4/accounts/" + acct + "/ai/v1"},
			env("BACKYARD_TEST_CLOUDFLARE_MODEL", "@cf/meta/llama-3.1-8b-instruct")})
	}
	if len(cases) == 0 {
		t.Skip("no live AI provider credentials in the environment")
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc, err := New(context.Background(), config.AIConfig{
				Providers: map[string]config.ProviderConfig{c.name: c.pc},
				Models:    map[string]config.ModelConfig{"live": {Provider: c.name, Model: c.model}},
			}, Options{})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			req := ai.ChatRequest{Messages: user("Reply with the single word: pong"), MaxTokens: 2000}
			resp, err := svc.Chat(ctx, "live", req)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.ToLower(resp.Content), "pong") || resp.Model != "live" {
				t.Errorf("chat: %+v", resp)
			}
			var streamed strings.Builder
			resp, err = svc.ChatStream(ctx, "live", req, func(s string) error { streamed.WriteString(s); return nil })
			if err != nil {
				t.Fatal(err)
			}
			if streamed.String() != resp.Content || resp.StopReason == "" {
				t.Errorf("stream: deltas %q vs content %q (%s)", streamed.String(), resp.Content, resp.StopReason)
			}
			jresp, err := svc.Chat(ctx, "live", ai.ChatRequest{Messages: user(`Return {"ok": true}`), JSON: true, MaxTokens: 2000})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(jresp.Content, "{") {
				t.Errorf("json mode: %q", jresp.Content)
			}
		})
	}
}
