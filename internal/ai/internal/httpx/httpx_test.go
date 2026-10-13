package httpx

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/ai"
)

func TestErrorMessage(t *testing.T) {
	tests := []struct {
		name, body, want string
	}{
		{"openai", `{"error":{"message":"Invalid key","type":"invalid_request_error","param":null,"code":"invalid_api_key"}}`, "Invalid key"},
		{"gemini", `{"error":{"code":400,"message":"API key not valid","status":"INVALID_ARGUMENT"}}`, "API key not valid"},
		{"gemini array", `[{"error":{"code":429,"message":"quota","status":"RESOURCE_EXHAUSTED"}}]`, "quota"},
		{"ollama", `{"error":"model not found"}`, "model not found"},
		{"aws style", `{"message":"denied"}`, "denied"},
		{"fastapi", `{"detail":"bad"}`, "bad"},
		{"cloudflare", `{"success":false,"errors":[{"code":5006,"message":"AiError: Bad input"},{"code":1,"message":"second"}],"messages":[]}`, "AiError: Bad input; second"},
		{"text", "upstream exploded\n", "upstream exploded"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ErrorMessage([]byte(tt.body)); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseErrorDetail(t *testing.T) {
	d, ok := ParseErrorDetail([]byte(`{"error":{"message":"m","param":"max_tokens","code":"unsupported_parameter"}}`))
	if !ok || d.Param != "max_tokens" || d.Code != "unsupported_parameter" {
		t.Fatalf("got %+v %v", d, ok)
	}
	if _, ok := ParseErrorDetail([]byte(`not json`)); ok {
		t.Fatal("want !ok")
	}
}

func TestTruncate(t *testing.T) {
	long := strings.Repeat("é", 2000)
	got := Truncate(long)
	if len(got) > maxMessageLen+len("…") || !strings.HasSuffix(got, "…") {
		t.Fatalf("len %d", len(got))
	}
}

func TestDo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			_, _ = w.Write([]byte(`{"x":1}`))
		case "/bad":
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
		case "/badjson":
			_, _ = w.Write([]byte(`{`))
		case "/slow":
			time.Sleep(200 * time.Millisecond)
		}
	}))
	defer srv.Close()
	ctx := context.Background()

	req, _ := NewJSONRequest(ctx, srv.URL+"/ok", map[string]int{"a": 1})
	resp, err := Do(srv.Client(), "p", req)
	if err != nil {
		t.Fatal(err)
	}
	var out struct{ X int }
	if err := DecodeJSON("p", resp, &out); err != nil || out.X != 1 {
		t.Fatalf("decode: %v %+v", err, out)
	}

	req, _ = NewJSONRequest(ctx, srv.URL+"/bad", nil)
	_, err = Do(srv.Client(), "p", req)
	var ue *ai.UpstreamError
	if !errors.As(err, &ue) || ue.Status != 429 || ue.Message != "slow down" || ue.Provider != "p" {
		t.Fatalf("got %v", err)
	}

	req, _ = NewJSONRequest(ctx, srv.URL+"/badjson", nil)
	resp, err = Do(srv.Client(), "p", req)
	if err != nil {
		t.Fatal(err)
	}
	if err := DecodeJSON("p", resp, &out); !errors.As(err, &ue) {
		t.Fatalf("want upstream error, got %v", err)
	}

	tctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	req, _ = NewJSONRequest(tctx, srv.URL+"/slow", nil)
	_, err = Do(srv.Client(), "p", req)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline exceeded, got %v", err)
	}

	req, _ = NewJSONRequest(ctx, "http://127.0.0.1:1/unreachable", nil)
	_, err = Do(srv.Client(), "p", req)
	if !errors.As(err, &ue) || ue.Status != 0 {
		t.Fatalf("want transport upstream error, got %v", err)
	}
}

func TestErrorTextStripsURL(t *testing.T) {
	inner := errors.New("dial tcp 10.0.0.7:443: connect: connection refused")
	err := fmt.Errorf("wrapped: %w", &url.Error{Op: "Post", URL: "https://internal-llm.corp/v1/chat?api-version=x", Err: inner})
	got := ErrorText(err)
	if got != inner.Error() {
		t.Errorf("ErrorText = %q, want %q", got, inner.Error())
	}
	if ErrorText(nil) != "" || ErrorText(errors.New("plain")) != "plain" {
		t.Error("ErrorText of nil/plain errors")
	}

	// Transport failures surfaced by Do carry no URL.
	srv := httptest.NewServer(http.NotFoundHandler())
	u := srv.URL + "/deployments/secret-deployment/chat?api-version=2024"
	srv.Close()
	req, _ := NewJSONRequest(context.Background(), u, nil)
	_, err = Do(&http.Client{}, "p", req)
	var ue *ai.UpstreamError
	if !errors.As(err, &ue) || strings.Contains(ue.Message, "secret-deployment") || strings.Contains(ue.Message, "api-version") {
		t.Fatalf("transport error leaks URL: %v", err)
	}
}
