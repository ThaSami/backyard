package httpx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/ai"
)

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	hdr := func(kv ...string) http.Header {
		h := http.Header{}
		for i := 0; i+1 < len(kv); i += 2 {
			h.Set(kv[i], kv[i+1])
		}
		return h
	}
	tests := []struct {
		name   string
		status int
		h      http.Header
		want   time.Duration
	}{
		{"none", 429, hdr(), 0},
		{"nil header", 429, nil, 0},
		{"retry-after seconds", 429, hdr("Retry-After", "20"), 20 * time.Second},
		{"retry-after fractional", 429, hdr("Retry-After", "1.5"), 1500 * time.Millisecond},
		{"retry-after zero", 429, hdr("Retry-After", "0"), 0},
		{"retry-after http-date", 429, hdr("Retry-After", now.Add(90*time.Second).Format(http.TimeFormat)), 90 * time.Second},
		{"retry-after past date", 429, hdr("Retry-After", now.Add(-time.Minute).Format(http.TimeFormat)), 0},
		{"retry-after garbage", 429, hdr("Retry-After", "soon"), 0},
		{"retry-after negative", 429, hdr("Retry-After", "-5"), 0},
		{"retry-after NaN", 429, hdr("Retry-After", "NaN"), 0},
		{"retry-after huge clamps", 429, hdr("Retry-After", "1e300"), MaxRetryDelay},
		{"retry-after on 503", 503, hdr("Retry-After", "7"), 7 * time.Second},
		{"retry-after-ms wins", 429, hdr("Retry-After-Ms", "350", "Retry-After", "1"), 350 * time.Millisecond},
		{"retry-after-ms garbage falls back", 429, hdr("Retry-After-Ms", "x", "Retry-After", "2"), 2 * time.Second},
		{"retry-after beats reset headers", 429, hdr("Retry-After", "3", "X-Ratelimit-Reset-Tokens", "6m0s"), 3 * time.Second},

		// OpenAI: the exhausted bucket decides.
		{"openai tokens exhausted", 429, hdr(
			"X-Ratelimit-Remaining-Requests", "4999", "X-Ratelimit-Reset-Requests", "12ms",
			"X-Ratelimit-Remaining-Tokens", "0", "X-Ratelimit-Reset-Tokens", "6m0s"), 6 * time.Minute},
		{"openai requests exhausted", 429, hdr(
			"X-Ratelimit-Remaining-Requests", "0", "X-Ratelimit-Reset-Requests", "1s",
			"X-Ratelimit-Remaining-Tokens", "90000", "X-Ratelimit-Reset-Tokens", "6m0s"), time.Second},
		{"openai none exhausted: latest", 429, hdr(
			"X-Ratelimit-Reset-Requests", "1s", "X-Ratelimit-Reset-Tokens", "1m30.5s"), 90*time.Second + 500*time.Millisecond},
		{"reset headers ignored unless 429", 400, hdr("X-Ratelimit-Remaining-Tokens", "0", "X-Ratelimit-Reset-Tokens", "6m0s"), 0},
		{"reset headers ignored on 503", 503, hdr("X-Ratelimit-Reset", "30"), 0},

		// Generic x-ratelimit-reset.
		{"generic delta seconds", 429, hdr("X-Ratelimit-Reset", "30"), 30 * time.Second},
		{"generic unix seconds", 429, hdr("X-Ratelimit-Reset", strconv.FormatInt(now.Add(45*time.Second).Unix(), 10)), 45 * time.Second},
		{"generic unix millis", 429, hdr("X-Ratelimit-Reset", strconv.FormatInt(now.Add(2500*time.Millisecond).UnixMilli(), 10)), 2500 * time.Millisecond},
		{"generic unix past", 429, hdr("X-Ratelimit-Reset", strconv.FormatInt(now.Add(-time.Hour).Unix(), 10)), 0},
		{"generic garbage", 429, hdr("X-Ratelimit-Reset", "later"), 0},

		// Anthropic: RFC 3339 timestamps.
		{"anthropic tokens exhausted", 429, hdr(
			"Anthropic-Ratelimit-Requests-Remaining", "10", "Anthropic-Ratelimit-Requests-Reset", now.Add(2*time.Second).Format(time.RFC3339),
			"Anthropic-Ratelimit-Input-Tokens-Remaining", "0", "Anthropic-Ratelimit-Input-Tokens-Reset", now.Add(40*time.Second).Format(time.RFC3339)),
			40 * time.Second},
		{"anthropic fractional", 429, hdr("Anthropic-Ratelimit-Tokens-Reset", now.Add(1500*time.Millisecond).Format(time.RFC3339Nano)), 1500 * time.Millisecond},
		{"far future clamps", 429, hdr("X-Ratelimit-Reset-Tokens", "1000h"), MaxRetryDelay},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RetryAfter(tt.status, tt.h, now); got != tt.want {
				t.Fatalf("RetryAfter = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRetryInfoDelay(t *testing.T) {
	tests := []struct {
		name, body string
		want       time.Duration
	}{
		{"gemini", `{"error":{"code":429,"message":"quota","status":"RESOURCE_EXHAUSTED","details":[` +
			`{"@type":"type.googleapis.com/google.rpc.QuotaFailure","violations":[]},` +
			`{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"37s"}]}}`, 37 * time.Second},
		{"fractional", `{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"1.250s"}]}}`, 1250 * time.Millisecond},
		{"array", `[{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"5s"}]}}]`, 5 * time.Second},
		{"no retry info", `{"error":{"code":429,"message":"quota"}}`, 0},
		{"bad delay", `{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"soon"}]}}`, 0},
		{"negative delay", `{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"-5s"}]}}`, 0},
		{"other type", `{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.Help","retryDelay":"5s"}]}}`, 0},
		{"huge clamps", `{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"100000h"}]}}`, MaxRetryDelay},
		{"not json", `rate limited`, 0},
		{"empty", ``, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RetryInfoDelay([]byte(tt.body)); got != tt.want {
				t.Fatalf("RetryInfoDelay = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestErrorFromResponseRetryDelay: throttled responses carry the provider's
// retry time on the *ai.UpstreamError, and the error exposes it through the
// RetryAfter() method the HTTP layer looks for in an error chain.
func TestErrorFromResponseRetryDelay(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/openai":
			w.Header().Set("Retry-After-Ms", "1500")
			w.Header().Set("X-Ratelimit-Remaining-Tokens", "0")
			w.Header().Set("X-Ratelimit-Reset-Tokens", "6m0s")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"Rate limit reached"}}`))
		case "/reset":
			w.Header().Set("X-Ratelimit-Remaining-Requests", "0")
			w.Header().Set("X-Ratelimit-Reset-Requests", "2s")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"Rate limit reached"}}`))
		case "/gemini":
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"code":429,"message":"Resource exhausted","status":"RESOURCE_EXHAUSTED",` +
				`"details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"12s"}]}}`))
		case "/plain":
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
		}
	}))
	defer srv.Close()
	for _, tc := range []struct {
		path string
		want time.Duration
	}{
		{"/openai", 1500 * time.Millisecond},
		{"/reset", 2 * time.Second},
		{"/gemini", 12 * time.Second},
		{"/plain", 0},
	} {
		t.Run(tc.path, func(t *testing.T) {
			req, _ := NewJSONRequest(context.Background(), srv.URL+tc.path, map[string]int{})
			_, err := Do(srv.Client(), "p", req)
			var ue *ai.UpstreamError
			if !errors.As(err, &ue) || ue.Status != http.StatusTooManyRequests {
				t.Fatalf("got %v", err)
			}
			if ue.RetryDelay != tc.want {
				t.Fatalf("RetryDelay = %v, want %v", ue.RetryDelay, tc.want)
			}
			var ra interface{ RetryAfter() time.Duration }
			if !errors.As(err, &ra) || ra.RetryAfter() != tc.want {
				t.Fatalf("RetryAfter() via errors.As = %v, want %v", ra, tc.want)
			}
		})
	}
}
