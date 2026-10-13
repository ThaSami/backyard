package httpx

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// MaxRetryDelay bounds a parsed retry delay; longer values are clamped (the
// HTTP layer applies its own, lower cap).
const MaxRetryDelay = 24 * time.Hour

// rateLimitResets pairs the rate-limit reset headers providers send with the
// header that reports the bucket's remaining capacity.
var rateLimitResets = []struct{ reset, remaining string }{
	// OpenAI, Azure OpenAI and many compatible servers: "1s", "6m0s", "20ms".
	{"X-Ratelimit-Reset-Requests", "X-Ratelimit-Remaining-Requests"},
	{"X-Ratelimit-Reset-Tokens", "X-Ratelimit-Remaining-Tokens"},
	// Generic: delta-seconds, a Unix time (seconds or milliseconds) or a date.
	{"X-Ratelimit-Reset", "X-Ratelimit-Remaining"},
	// Anthropic: RFC 3339 timestamps.
	{"Anthropic-Ratelimit-Requests-Reset", "Anthropic-Ratelimit-Requests-Remaining"},
	{"Anthropic-Ratelimit-Tokens-Reset", "Anthropic-Ratelimit-Tokens-Remaining"},
	{"Anthropic-Ratelimit-Input-Tokens-Reset", "Anthropic-Ratelimit-Input-Tokens-Remaining"},
	{"Anthropic-Ratelimit-Output-Tokens-Reset", "Anthropic-Ratelimit-Output-Tokens-Remaining"},
}

// RetryAfter returns how long a provider asked callers to wait before
// retrying, from the headers of a failed response, or 0 when it did not
// say. In order of preference:
//
//   - retry-after-ms (OpenAI, Azure OpenAI): milliseconds;
//   - Retry-After: delay-seconds or an HTTP-date (RFC 9110 §10.2.3);
//   - for a 429 only, the rate-limit reset headers (x-ratelimit-reset-requests
//     and -tokens, x-ratelimit-reset, anthropic-ratelimit-*-reset). Those
//     describe every response, so they are not consulted for other
//     statuses. When a bucket reports 0 remaining, the latest reset among
//     the exhausted buckets is used; otherwise the latest reset overall.
//
// Values in the past or that do not parse are ignored; the result is
// clamped to MaxRetryDelay.
func RetryAfter(status int, h http.Header, now time.Time) time.Duration {
	if h == nil {
		return 0
	}
	if d, ok := delayValue(h.Get("Retry-After-Ms"), time.Millisecond); ok {
		return d
	}
	if v := strings.TrimSpace(h.Get("Retry-After")); v != "" {
		if d, ok := delayValue(v, time.Second); ok {
			return d
		}
		if t, err := http.ParseTime(v); err == nil {
			return clampDelay(t.Sub(now))
		}
	}
	if status != http.StatusTooManyRequests {
		return 0
	}
	var exhausted, latest time.Duration
	for _, rl := range rateLimitResets {
		d, ok := resetValue(h.Get(rl.reset), now)
		if !ok {
			continue
		}
		latest = max(latest, d)
		if rem := strings.TrimSpace(h.Get(rl.remaining)); rem != "" {
			if n, err := strconv.ParseFloat(rem, 64); err == nil && n <= 0 {
				exhausted = max(exhausted, d)
			}
		}
	}
	if exhausted > 0 {
		return exhausted
	}
	return latest
}

// delayValue parses a non-negative number of units ("20", "1.5").
func delayValue(v string, unit time.Duration) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(n) || n < 0 {
		return 0, false
	}
	return clampSeconds(n * unit.Seconds()), true
}

// resetValue parses a rate-limit reset header value: a Go-style duration
// ("6m0s", "20ms"), a number (delta-seconds, or a Unix time in seconds or
// milliseconds when it is that large), an RFC 3339 timestamp or an
// HTTP-date. ok is false for empty, unparsable and past values.
func resetValue(v string, now time.Time) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	var d time.Duration
	if n, err := strconv.ParseFloat(v, 64); err == nil {
		switch {
		case math.IsNaN(n) || n < 0:
			return 0, false
		case n >= 1e12: // Unix milliseconds
			d = clampDelay(time.UnixMilli(int64(min(n, 1e15))).Sub(now))
		case n >= 1e9: // Unix seconds
			d = clampDelay(time.Unix(int64(n), 0).Sub(now))
		default:
			d = clampSeconds(n)
		}
	} else if pd, err := time.ParseDuration(v); err == nil {
		d = clampDelay(pd)
	} else if t, err := time.Parse(time.RFC3339, v); err == nil {
		d = clampDelay(t.Sub(now))
	} else if t, err := http.ParseTime(v); err == nil {
		d = clampDelay(t.Sub(now))
	} else {
		return 0, false
	}
	return d, d > 0
}

// clampSeconds converts seconds to a duration within [0, MaxRetryDelay]
// without overflowing.
func clampSeconds(s float64) time.Duration {
	if s >= MaxRetryDelay.Seconds() {
		return MaxRetryDelay
	}
	if s <= 0 {
		return 0
	}
	return time.Duration(s * float64(time.Second))
}

func clampDelay(d time.Duration) time.Duration {
	return min(max(d, 0), MaxRetryDelay)
}

// RetryInfoDelay returns the retryDelay of a google.rpc.RetryInfo detail in
// an error body ({"error": {"details": [{"@type": "…RetryInfo",
// "retryDelay": "37s"}]}}, also inside a top-level array as Gemini streams
// send it), or 0 when there is none. Gemini and Vertex AI report quota
// exhaustion this way instead of with a Retry-After header.
func RetryInfoDelay(body []byte) time.Duration {
	body = bytes.TrimSpace(body)
	if len(body) == 0 || (body[0] != '{' && body[0] != '[') {
		return 0
	}
	if body[0] == '[' {
		var arr []json.RawMessage
		if json.Unmarshal(body, &arr) != nil || len(arr) == 0 {
			return 0
		}
		body = arr[0]
	}
	var env struct {
		Error struct {
			Details []struct {
				Type       string `json:"@type"`
				RetryDelay string `json:"retryDelay"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &env) != nil {
		return 0
	}
	for _, d := range env.Error.Details {
		if !strings.HasSuffix(d.Type, "google.rpc.RetryInfo") || d.RetryDelay == "" {
			continue
		}
		if pd, err := time.ParseDuration(strings.TrimSpace(d.RetryDelay)); err == nil && pd > 0 {
			return clampDelay(pd)
		}
	}
	return 0
}
