package gemini

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
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"

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
		t.Fatal("no request")
	}
	return f.reqs[len(f.reqs)-1]
}

func (f *fake) all() []captured {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]captured(nil), f.reqs...)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func genResponse(text, finish string) map[string]any {
	return map[string]any{
		"candidates": []any{map[string]any{
			"content": map[string]any{"role": "model", "parts": []any{
				map[string]any{"text": "thinking...", "thought": true},
				map[string]any{"text": text},
			}},
			"finishReason": finish,
		}},
		"usageMetadata": map[string]any{"promptTokenCount": 12, "candidatesTokenCount": 4, "thoughtsTokenCount": 3, "totalTokenCount": 19},
		"modelVersion":  "gemini-2.5-flash-001",
	}
}

func studio(t *testing.T, f *fake) *Provider {
	t.Helper()
	p, err := New("gem", config.ProviderConfig{Type: "gemini", APIKey: "AIza-test", BaseURL: f.srv.URL + "/v1beta",
		Headers: map[string]string{"X-Extra": "1"}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func ptr[T any](v T) *T { return &v }

func TestNew(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GOOGLE_API_KEY", "")
	if _, err := New("g", config.ProviderConfig{}, Options{}); err == nil {
		t.Error("AI Studio without key must fail")
	}
	t.Setenv("GEMINI_API_KEY", "from-env")
	p, err := New("g", config.ProviderConfig{}, Options{})
	if err != nil || p.apiKey != "from-env" || p.baseURL != aiStudioBaseURL {
		t.Errorf("env key: %v %+v", err, p)
	}
	p, err = New("g", config.ProviderConfig{ProjectID: "proj"}, Options{})
	if err != nil || !p.vertex || p.location != "global" || p.baseURL != "https://aiplatform.googleapis.com/v1" {
		t.Errorf("vertex defaults: %v %+v", err, p)
	}
	if _, err := New("g", config.ProviderConfig{APIKey: "k", BaseURL: "::bad"}, Options{}); err == nil {
		t.Error("bad base URL must fail")
	}
}

func TestModelURL(t *testing.T) {
	mk := func(pc config.ProviderConfig) *Provider {
		p, err := New("g", pc, Options{})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	tests := []struct {
		name string
		p    *Provider
		proj string
		want string
	}{
		{"studio", mk(config.ProviderConfig{APIKey: "k"}), "",
			"https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent"},
		{"vertex global", mk(config.ProviderConfig{ProjectID: "p1"}), "p1",
			"https://aiplatform.googleapis.com/v1/projects/p1/locations/global/publishers/google/models/gemini-2.5-flash:generateContent"},
		{"vertex region", mk(config.ProviderConfig{ProjectID: "p1", Location: "europe-west4"}), "p1",
			"https://europe-west4-aiplatform.googleapis.com/v1/projects/p1/locations/europe-west4/publishers/google/models/gemini-2.5-flash:generateContent"},
		{"vertex express", mk(config.ProviderConfig{Platform: "vertex", APIKey: "k"}), "",
			"https://aiplatform.googleapis.com/v1/publishers/google/models/gemini-2.5-flash:generateContent"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.p.modelURL(tt.proj, "models/gemini-2.5-flash", "generateContent", ""); got != tt.want {
				t.Errorf("got  %s\nwant %s", got, tt.want)
			}
		})
	}
}

func TestChatStudio(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		writeJSON(w, 200, genResponse("Bonjour", "STOP"))
	})
	p := studio(t, f)
	resp, err := p.Chat(context.Background(), ai.ChatRequest{
		Model:  "gemini-2.5-flash",
		System: "Translate to French.",
		Messages: []ai.Message{
			{Role: "user", Content: []ai.Part{{Type: "text", Text: "Hello"}}},
			{Role: "assistant", Content: []ai.Part{{Type: "text", Text: "Bonjour"}}},
			{Role: "user", Content: []ai.Part{{Type: "text", Text: "Again"}, {Type: "image", Data: pngBytes, MediaType: "image/png"}}},
		},
		MaxTokens:   256,
		Temperature: ptr(0.4),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "Bonjour" || resp.StopReason != ai.StopEndTurn || resp.Model != "gemini-2.5-flash-001" {
		t.Fatalf("resp = %+v", resp)
	}
	if resp.Usage != (ai.Usage{InputTokens: 12, OutputTokens: 7}) {
		t.Fatalf("usage = %+v", resp.Usage)
	}
	req := f.last(t)
	if req.Path != "/v1beta/models/gemini-2.5-flash:generateContent" {
		t.Errorf("path = %s", req.Path)
	}
	if req.Header.Get("X-Goog-Api-Key") != "AIza-test" || req.Header.Get("X-Extra") != "1" || req.Header.Get("Authorization") != "" {
		t.Errorf("headers = %v", req.Header)
	}
	b := req.Body
	contents := b["contents"].([]any)
	if len(contents) != 3 || contents[1].(map[string]any)["role"] != "model" {
		t.Fatalf("contents = %v", contents)
	}
	img := contents[2].(map[string]any)["parts"].([]any)[1].(map[string]any)["inlineData"].(map[string]any)
	if img["mimeType"] != "image/png" || img["data"] != base64.StdEncoding.EncodeToString(pngBytes) {
		t.Errorf("inlineData = %v", img)
	}
	si := b["systemInstruction"].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"]
	if si != "Translate to French." {
		t.Errorf("systemInstruction = %v", si)
	}
	gc := b["generationConfig"].(map[string]any)
	if gc["maxOutputTokens"] != float64(256) || gc["temperature"] != 0.4 || gc["responseMimeType"] != nil {
		t.Errorf("generationConfig = %v", gc)
	}
}

func TestChatJSONAndURLImage(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		writeJSON(w, 200, genResponse("```json\n{\"a\":1}\n```", "STOP"))
	})
	p := studio(t, f)
	var fetched string
	p.fetch = func(ctx context.Context, u string) ([]byte, string, error) {
		fetched = u
		return pngBytes, "image/png", nil
	}
	resp, err := p.Chat(context.Background(), ai.ChatRequest{Model: "models/gemini-2.5-pro", JSON: true,
		Messages: []ai.Message{{Role: "user", Content: []ai.Part{{Type: "image", URL: "https://example.com/a.png"}, {Type: "text", Text: "json pls"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != `{"a":1}` || fetched != "https://example.com/a.png" {
		t.Fatalf("content=%q fetched=%q", resp.Content, fetched)
	}
	req := f.last(t)
	if req.Path != "/v1beta/models/gemini-2.5-pro:generateContent" {
		t.Errorf("path = %s", req.Path)
	}
	if gc := req.Body["generationConfig"].(map[string]any); gc["responseMimeType"] != "application/json" {
		t.Errorf("generationConfig = %v", gc)
	}
	si, _ := req.Body["systemInstruction"].(map[string]any)
	if si == nil || !strings.Contains(fmt.Sprint(si["parts"]), "single valid JSON object") {
		t.Errorf("JSON mode should add the JSON-object instruction, systemInstruction = %v", req.Body["systemInstruction"])
	}

	p.fetch = func(context.Context, string) ([]byte, string, error) { return nil, "", errors.New("forbidden") }
	_, err = p.Chat(context.Background(), ai.ChatRequest{Model: "m",
		Messages: []ai.Message{{Role: "user", Content: []ai.Part{{Type: "image", URL: "http://169.254.169.254/"}}}}})
	if !errors.Is(err, ai.ErrBadRequest) {
		t.Fatalf("want bad request, got %v", err)
	}
}

func TestChatBlockedAndErrors(t *testing.T) {
	ctx := context.Background()
	msgs := []ai.Message{{Role: "user", Content: []ai.Part{{Type: "text", Text: "x"}}}}

	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		writeJSON(w, 200, map[string]any{"promptFeedback": map[string]any{"blockReason": "SAFETY"},
			"usageMetadata": map[string]any{"promptTokenCount": 5}})
	})
	resp, err := studio(t, f).Chat(ctx, ai.ChatRequest{Model: "m", Messages: msgs})
	if err != nil || resp.StopReason != ai.StopRefusal || resp.Content != "" || resp.Usage.InputTokens != 5 {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}

	f = newFake(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		writeJSON(w, 400, map[string]any{"error": map[string]any{"code": 400, "message": "API key not valid. Please pass a valid API key.", "status": "INVALID_ARGUMENT"}})
	})
	_, err = studio(t, f).Chat(ctx, ai.ChatRequest{Model: "m", Messages: msgs})
	var ue *ai.UpstreamError
	if !errors.As(err, &ue) || ue.Status != 400 || !strings.HasPrefix(ue.Message, "API key not valid") || ue.Provider != "gem" {
		t.Fatalf("got %v", err)
	}

	p := studio(t, f)
	for i, req := range []ai.ChatRequest{
		{Messages: msgs},
		{Model: "m"},
		{Model: "m", Messages: []ai.Message{{Role: "system", Content: []ai.Part{{Type: "text", Text: "x"}}}}},
		{Model: "m", Messages: []ai.Message{{Role: "user", Content: []ai.Part{{Type: "image", Data: []byte("plain text")}}}}},
	} {
		if _, err := p.Chat(ctx, req); !errors.Is(err, ai.ErrBadRequest) {
			t.Errorf("case %d: want bad request, got %v", i, err)
		}
	}
}

func TestFinishReasons(t *testing.T) {
	tests := map[string]string{
		"STOP": ai.StopEndTurn, "MAX_TOKENS": ai.StopMaxTokens, "SAFETY": ai.StopRefusal,
		"RECITATION": ai.StopRefusal, "PROHIBITED_CONTENT": ai.StopRefusal, "OTHER": ai.StopOther,
		"MALFORMED_FUNCTION_CALL": ai.StopOther, "": ai.StopOther,
	}
	for in, want := range tests {
		if got := mapFinishReason(in); got != want {
			t.Errorf("%s -> %s, want %s", in, got, want)
		}
	}
}

func sseHandler(chunks ...string) func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
	return func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\r\n\r\n", c)
			w.(http.Flusher).Flush()
		}
	}
}

func TestChatStream(t *testing.T) {
	f := newFake(t, sseHandler(
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"Hel"}]}}],"modelVersion":"gemini-2.5-flash"}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"lo"}]}}],"modelVersion":"gemini-2.5-flash"}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"!"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":3}}`,
	))
	p := studio(t, f)
	var deltas []string
	resp, err := p.ChatStream(context.Background(), ai.ChatRequest{Model: "gemini-2.5-flash",
		Messages: []ai.Message{{Role: "user", Content: []ai.Part{{Type: "text", Text: "hi"}}}}},
		func(s string) error { deltas = append(deltas, s); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(deltas, "|") != "Hel|lo|!" || resp.Content != "Hello!" || resp.StopReason != ai.StopEndTurn {
		t.Fatalf("deltas=%v resp=%+v", deltas, resp)
	}
	if resp.Usage != (ai.Usage{InputTokens: 3, OutputTokens: 3}) {
		t.Fatalf("usage = %+v", resp.Usage)
	}
	req := f.last(t)
	if req.Path != "/v1beta/models/gemini-2.5-flash:streamGenerateContent" || req.Query != "alt=sse" {
		t.Errorf("url = %s?%s", req.Path, req.Query)
	}
}

func TestChatStreamErrors(t *testing.T) {
	ctx := context.Background()
	req := ai.ChatRequest{Model: "m", Messages: []ai.Message{{Role: "user", Content: []ai.Part{{Type: "text", Text: "x"}}}}}
	t.Run("error chunk", func(t *testing.T) {
		f := newFake(t, sseHandler(`{"error":{"code":503,"message":"The model is overloaded.","status":"UNAVAILABLE"}}`))
		_, err := studio(t, f).ChatStream(ctx, req, nil)
		var ue *ai.UpstreamError
		if !errors.As(err, &ue) || ue.Message != "The model is overloaded." {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("truncated", func(t *testing.T) {
		f := newFake(t, sseHandler(`{"candidates":[{"content":{"parts":[{"text":"a"}]}}]}`))
		_, err := studio(t, f).ChatStream(ctx, req, nil)
		var ue *ai.UpstreamError
		if !errors.As(err, &ue) || !strings.Contains(ue.Message, "unexpectedly") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("blocked", func(t *testing.T) {
		f := newFake(t, sseHandler(`{"promptFeedback":{"blockReason":"PROHIBITED_CONTENT"}}`))
		resp, err := studio(t, f).ChatStream(ctx, req, nil)
		if err != nil || resp.StopReason != ai.StopRefusal {
			t.Fatalf("resp=%+v err=%v", resp, err)
		}
	})
	t.Run("abort", func(t *testing.T) {
		f := newFake(t, sseHandler(`{"candidates":[{"content":{"parts":[{"text":"a"}]},"finishReason":"STOP"}]}`))
		stop := errors.New("stop")
		_, err := studio(t, f).ChatStream(ctx, req, func(string) error { return stop })
		if !errors.Is(err, stop) {
			t.Fatalf("got %v", err)
		}
	})
}

type countingTS struct {
	mu    sync.Mutex
	calls int
}

func (c *countingTS) Token() (*oauth2.Token, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return &oauth2.Token{AccessToken: "ya29.vertex", Expiry: time.Now().Add(time.Hour)}, nil
}

func TestVertex(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		switch {
		case strings.HasSuffix(r.URL.Path, ":generateContent"):
			writeJSON(w, 200, genResponse("vertex says hi", "STOP"))
		case strings.HasSuffix(r.URL.Path, ":streamGenerateContent"):
			sseHandler(`{"candidates":[{"content":{"parts":[{"text":"v"}]},"finishReason":"STOP"}]}`)(w, r, body)
		case strings.HasSuffix(r.URL.Path, ":predict"):
			inst := body["instances"].([]any)
			preds := make([]any, len(inst))
			for i := range inst {
				preds[i] = map[string]any{"embeddings": map[string]any{"values": []float64{float64(i), 1}, "statistics": map[string]any{"token_count": 2, "truncated": false}}}
			}
			writeJSON(w, 200, map[string]any{"predictions": preds})
		}
	})
	ts := &countingTS{}
	p, err := New("vtx", config.ProviderConfig{Type: "gemini", ProjectID: "proj-9", Location: "us-central1", BaseURL: f.srv.URL + "/v1"},
		Options{TokenSource: ts})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	msgs := []ai.Message{{Role: "user", Content: []ai.Part{{Type: "text", Text: "hi"}}}}
	resp, err := p.Chat(ctx, ai.ChatRequest{Model: "gemini-2.5-flash", Messages: msgs})
	if err != nil || resp.Content != "vertex says hi" {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	req := f.last(t)
	if req.Path != "/v1/projects/proj-9/locations/us-central1/publishers/google/models/gemini-2.5-flash:generateContent" {
		t.Errorf("path = %s", req.Path)
	}
	if req.Header.Get("Authorization") != "Bearer ya29.vertex" || req.Header.Get("X-Goog-Api-Key") != "" {
		t.Errorf("headers = %v", req.Header)
	}
	if _, err := p.ChatStream(ctx, ai.ChatRequest{Model: "gemini-2.5-flash", Messages: msgs}, nil); err != nil {
		t.Fatal(err)
	}
	if req := f.last(t); !strings.HasSuffix(req.Path, ":streamGenerateContent") || req.Query != "alt=sse" {
		t.Errorf("stream url = %s?%s", req.Path, req.Query)
	}

	// text-embedding models are batched; gemini-embedding is one per request.
	before := len(f.all())
	er, err := p.Embed(ctx, ai.EmbedRequest{Model: "text-embedding-005", Input: []string{"a", "b", "c"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(er.Embeddings) != 3 || er.Usage.InputTokens != 6 || len(f.all())-before != 1 {
		t.Fatalf("embed = %+v calls=%d", er, len(f.all())-before)
	}
	if req := f.last(t); req.Path != "/v1/projects/proj-9/locations/us-central1/publishers/google/models/text-embedding-005:predict" ||
		req.Body["instances"].([]any)[0].(map[string]any)["content"] != "a" {
		t.Errorf("embed req = %+v", req)
	}
	before = len(f.all())
	if _, err := p.Embed(ctx, ai.EmbedRequest{Model: "gemini-embedding-001", Input: []string{"a", "b"}}); err != nil {
		t.Fatal(err)
	}
	if n := len(f.all()) - before; n != 2 {
		t.Errorf("gemini-embedding should send one instance per call, got %d calls", n)
	}
}

func TestEmbedStudio(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		reqs := body["requests"].([]any)
		embs := make([]any, len(reqs))
		for i := range reqs {
			embs[i] = map[string]any{"values": []float64{float64(i)}}
		}
		writeJSON(w, 200, map[string]any{"embeddings": embs})
	})
	p := studio(t, f)
	input := make([]string, 150)
	for i := range input {
		input[i] = fmt.Sprintf("text %d", i)
	}
	resp, err := p.Embed(context.Background(), ai.EmbedRequest{Model: "text-embedding-004", Input: input})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Embeddings) != 150 || resp.Embeddings[100][0] != 0 || resp.Embeddings[99][0] != 99 {
		t.Fatalf("embeddings misordered: %d", len(resp.Embeddings))
	}
	reqs := f.all()
	if len(reqs) != 2 {
		t.Fatalf("want 2 batches, got %d", len(reqs))
	}
	r0 := reqs[0]
	if r0.Path != "/v1beta/models/text-embedding-004:batchEmbedContents" {
		t.Errorf("path = %s", r0.Path)
	}
	first := r0.Body["requests"].([]any)[0].(map[string]any)
	if first["model"] != "models/text-embedding-004" || first["content"].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"] != "text 0" {
		t.Errorf("request = %v", first)
	}
}

func TestImagen(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString(pngBytes)
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		n := int(body["parameters"].(map[string]any)["sampleCount"].(float64))
		preds := []any{}
		for range n {
			preds = append(preds, map[string]any{"bytesBase64Encoded": b64, "mimeType": "image/png"})
		}
		writeJSON(w, 200, map[string]any{"predictions": preds})
	})
	p := studio(t, f)
	resp, err := p.Image(context.Background(), ai.ImageRequest{Model: "imagen-4.0-generate-001", Prompt: "a fox", Size: "1792x1024", N: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Images) != 2 || resp.Images[0].MediaType != "image/png" || string(resp.Images[0].Data) != string(pngBytes) {
		t.Fatalf("resp = %+v", resp)
	}
	req := f.last(t)
	if req.Path != "/v1beta/models/imagen-4.0-generate-001:predict" {
		t.Errorf("path = %s", req.Path)
	}
	if req.Body["instances"].([]any)[0].(map[string]any)["prompt"] != "a fox" {
		t.Errorf("instances = %v", req.Body["instances"])
	}
	if ar := req.Body["parameters"].(map[string]any)["aspectRatio"]; ar != "16:9" {
		t.Errorf("aspectRatio = %v", ar)
	}

	filtered := newFake(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		writeJSON(w, 200, map[string]any{"predictions": []any{map[string]any{"raiFilteredReason": "blocked by safety filter"}}})
	})
	_, err = studio(t, filtered).Image(context.Background(), ai.ImageRequest{Model: "imagen-3.0-generate-002", Prompt: "x"})
	var ue *ai.UpstreamError
	if !errors.As(err, &ue) || !strings.Contains(ue.Message, "safety") {
		t.Fatalf("got %v", err)
	}
}

func TestGeminiImageModel(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString(pngBytes)
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		mods := body["generationConfig"].(map[string]any)["responseModalities"].([]any)
		if len(mods) == 1 {
			writeJSON(w, 400, map[string]any{"error": map[string]any{"code": 400, "message": "Model does not support the requested response modalities: image", "status": "INVALID_ARGUMENT"}})
			return
		}
		writeJSON(w, 200, map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"parts": []any{
			map[string]any{"text": "Here is your image"},
			map[string]any{"inlineData": map[string]any{"mimeType": "image/png", "data": b64}},
		}}, "finishReason": "STOP"}}})
	})
	p := studio(t, f)
	resp, err := p.Image(context.Background(), ai.ImageRequest{Model: "gemini-2.0-flash-preview-image-generation", Prompt: "a fox", N: 2, Size: "1024x1024"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Images) != 2 {
		t.Fatalf("images = %d", len(resp.Images))
	}
	reqs := f.all()
	if len(reqs) != 3 { // one rejected IMAGE-only attempt, then one call per image
		t.Fatalf("calls = %d", len(reqs))
	}
	gc := reqs[2].Body["generationConfig"].(map[string]any)
	if gc["imageConfig"].(map[string]any)["aspectRatio"] != "1:1" {
		t.Errorf("generationConfig = %v", gc)
	}

	// No image in the answer → upstream error with the finish reason.
	f2 := newFake(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		writeJSON(w, 200, map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"parts": []any{}}, "finishReason": "IMAGE_SAFETY"}}})
	})
	_, err = studio(t, f2).Image(context.Background(), ai.ImageRequest{Model: "gemini-2.5-flash-image", Prompt: "x"})
	var ue *ai.UpstreamError
	if !errors.As(err, &ue) || !strings.Contains(ue.Message, "IMAGE_SAFETY") {
		t.Fatalf("got %v", err)
	}
}

func TestAspectRatio(t *testing.T) {
	tests := []struct{ size, want string }{
		{"", ""}, {"auto", ""}, {"1024x1024", "1:1"}, {"1792x1024", "16:9"}, {"1024x1792", "9:16"},
		{"1536x1024", "4:3"}, {"1024x1536", "3:4"}, {"16:9", "16:9"}, {"axb", ""}, {"0x10", ""}, {"big", ""},
	}
	for _, tt := range tests {
		if got := aspectRatio(tt.size, imagenRatios); got != tt.want {
			t.Errorf("aspectRatio(%q) = %q, want %q", tt.size, got, tt.want)
		}
	}
	if got := aspectRatio("1536x1024", geminiRatios); got != "3:2" {
		t.Errorf("gemini 1536x1024 = %q", got)
	}
}

func TestVertexCredentialErrors(t *testing.T) {
	p, err := New("vtx", config.ProviderConfig{Platform: "vertex", APIKey: ""}, Options{TokenSource: &countingTS{}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Chat(context.Background(), ai.ChatRequest{Model: "m", Messages: []ai.Message{{Role: "user", Content: []ai.Part{{Type: "text", Text: "x"}}}}})
	var ue *ai.UpstreamError
	if !errors.As(err, &ue) || !strings.Contains(ue.Message, "project_id") {
		t.Fatalf("got %v", err)
	}
}

func TestCloudflareAIGatewayBaseURL(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		writeJSON(w, 200, genResponse("via gateway", "STOP"))
	})
	p, err := New("gem-gw", config.ProviderConfig{APIKey: "AIza", BaseURL: f.srv.URL + "/v1/acct/gw/google-ai-studio/v1beta/"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := p.Chat(context.Background(), ai.ChatRequest{Model: "gemini-2.5-flash",
		Messages: []ai.Message{{Role: "user", Content: []ai.Part{{Type: "text", Text: "x"}}}}})
	if err != nil || resp.Content != "via gateway" {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	if got := f.last(t).Path; got != "/v1/acct/gw/google-ai-studio/v1beta/models/gemini-2.5-flash:generateContent" {
		t.Errorf("path = %s", got)
	}
}

type failingTS struct{}

func (failingTS) Token() (*oauth2.Token, error) {
	return nil, errors.New("oauth2: cannot fetch token: 400\nResponse: {\"error\":\"invalid_grant\",\"detail\":\"SECRET-ish\"}")
}

func TestVertexTokenErrorIsGeneric(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		writeJSON(w, 200, genResponse("x", "STOP"))
	})
	p, err := New("vtx", config.ProviderConfig{ProjectID: "p1", BaseURL: f.srv.URL + "/v1"}, Options{TokenSource: failingTS{}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Chat(context.Background(), ai.ChatRequest{Model: "m", Messages: []ai.Message{{Role: "user", Content: []ai.Part{{Type: "text", Text: "x"}}}}})
	var ue *ai.UpstreamError
	if !errors.As(err, &ue) || ue.Status != http.StatusUnauthorized {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(ue.Message, "SECRET") || strings.Contains(ue.Message, "invalid_grant") {
		t.Errorf("token endpoint details leaked to the client: %q", ue.Message)
	}
	f.mu.Lock()
	n := len(f.reqs)
	f.mu.Unlock()
	if n != 0 {
		t.Errorf("request sent without a token")
	}
}

func TestVertexLocationValidation(t *testing.T) {
	for _, loc := range []string{"evil.example.com/x", "us-central1.attacker", "US", "a b", "-x", "x-"} {
		if _, err := New("g", config.ProviderConfig{ProjectID: "p", Location: loc}, Options{}); err == nil {
			t.Errorf("location %q accepted", loc)
		}
	}
	for _, loc := range []string{"global", "us-central1", "europe-west4", "us"} {
		if _, err := New("g", config.ProviderConfig{ProjectID: "p", Location: loc}, Options{}); err != nil {
			t.Errorf("location %q rejected: %v", loc, err)
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

// TestResponseBodiesClosed checks that every non-streaming call closes the
// upstream response body, on success, on upstream errors and on invalid
// responses.
func TestResponseBodiesClosed(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString(pngBytes)
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		switch {
		case strings.Contains(r.URL.Path, "fail"):
			writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": map[string]any{"code": 429, "message": "slow down"}})
		case strings.Contains(r.URL.Path, "garbage"):
			_, _ = io.WriteString(w, "{not json")
		case strings.HasSuffix(r.URL.Path, ":generateContent") && strings.Contains(r.URL.Path, "image"):
			writeJSON(w, http.StatusOK, map[string]any{"candidates": []any{map[string]any{"content": map[string]any{
				"parts": []any{map[string]any{"inlineData": map[string]any{"mimeType": "image/png", "data": b64}}}}}}})
		case strings.HasSuffix(r.URL.Path, ":generateContent"):
			writeJSON(w, http.StatusOK, genResponse("hi", "STOP"))
		case strings.HasSuffix(r.URL.Path, ":predict"):
			writeJSON(w, http.StatusOK, map[string]any{"predictions": []any{map[string]any{"bytesBase64Encoded": b64, "mimeType": "image/png"}}})
		case strings.HasSuffix(r.URL.Path, ":batchEmbedContents"):
			writeJSON(w, http.StatusOK, map[string]any{"embeddings": []any{map[string]any{"values": []float64{1}}}})
		default:
			http.NotFound(w, r)
		}
	})
	cc := &closeCounter{}
	p, err := New("gem", config.ProviderConfig{Type: "gemini", APIKey: "AIza-test", BaseURL: f.srv.URL + "/v1beta"},
		Options{HTTPClient: &http.Client{Transport: cc}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, model := range []string{"gemini-2.5-flash", "fail", "garbage"} {
		_, err := p.Chat(ctx, ai.ChatRequest{Model: model, Messages: []ai.Message{{Role: "user", Content: []ai.Part{{Type: "text", Text: "hi"}}}}})
		if (err != nil) != (model != "gemini-2.5-flash") {
			t.Errorf("Chat(%s): err = %v", model, err)
		}
		_, err = p.Embed(ctx, ai.EmbedRequest{Model: model, Input: []string{"a"}})
		if (err != nil) != (model != "gemini-2.5-flash") {
			t.Errorf("Embed(%s): err = %v", model, err)
		}
	}
	for _, model := range []string{"imagen-4.0-generate-001", "gemini-2.5-flash-image", "imagen-fail", "gemini-image-garbage"} {
		_, err := p.Image(ctx, ai.ImageRequest{Model: model, Prompt: "a fox"})
		wantErr := strings.Contains(model, "fail") || strings.Contains(model, "garbage")
		if (err != nil) != wantErr {
			t.Errorf("Image(%s): err = %v", model, err)
		}
	}
	if o, c := cc.opened.Load(), cc.closed.Load(); o == 0 || o != c {
		t.Fatalf("response bodies: %d opened, %d closed", o, c)
	}
}

// TestThrottledRetryInfo: Gemini reports when to retry a quota error in a
// google.rpc.RetryInfo detail, not a header; it reaches the
// *ai.UpstreamError (Retry-After, when sent, takes precedence).
func TestThrottledRetryInfo(t *testing.T) {
	ctx := context.Background()
	msgs := []ai.Message{{Role: "user", Content: []ai.Part{{Type: "text", Text: "x"}}}}
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		if strings.Contains(r.URL.Path, "with-header") {
			w.Header().Set("Retry-After", "3")
		}
		writeJSON(w, 429, map[string]any{"error": map[string]any{
			"code": 429, "message": "You exceeded your current quota. Please retry in 21.5s.", "status": "RESOURCE_EXHAUSTED",
			"details": []any{
				map[string]any{"@type": "type.googleapis.com/google.rpc.QuotaFailure", "violations": []any{}},
				map[string]any{"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": "21.5s"},
			},
		}})
	})
	p := studio(t, f)
	for _, tc := range []struct {
		model  string
		stream bool
		want   time.Duration
	}{
		{"m", false, 21500 * time.Millisecond},
		{"m", true, 21500 * time.Millisecond},
		{"with-header", false, 3 * time.Second},
	} {
		var err error
		if tc.stream {
			_, err = p.ChatStream(ctx, ai.ChatRequest{Model: tc.model, Messages: msgs}, nil)
		} else {
			_, err = p.Chat(ctx, ai.ChatRequest{Model: tc.model, Messages: msgs})
		}
		var ue *ai.UpstreamError
		if !errors.As(err, &ue) || ue.Status != 429 || ue.RetryAfter() != tc.want {
			t.Errorf("%s stream=%v: got %#v, want RetryAfter %v", tc.model, tc.stream, err, tc.want)
		}
	}
}
