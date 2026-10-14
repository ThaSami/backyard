// Package httpx holds HTTP helpers shared by the raw-HTTP AI providers
// (openai, gemini): JSON requests, bounded response decoding and conversion
// of upstream error bodies into *ai.UpstreamError.
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/thasami/backyard/internal/ai"
)

// MaxResponseBytes bounds a non-streaming JSON response body. Image
// generation responses carry base64 images and can be tens of MiB.
const MaxResponseBytes = 256 << 20

// maxErrorBytes bounds how much of an error body is read.
const maxErrorBytes = 64 << 10

// maxMessageLen bounds the error message surfaced to callers.
const maxMessageLen = 1000

// NewJSONRequest builds a POST request with a JSON body.
func NewJSONRequest(ctx context.Context, url string, body any) (*http.Request, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	return req, nil
}

// Do sends req with client and converts transport failures into
// *ai.UpstreamError (context cancellation is returned unchanged so callers
// can tell a timeout from an upstream failure). Non-2xx responses are read,
// closed and returned as *ai.UpstreamError.
func Do(client *http.Client, provider string, req *http.Request) (*http.Response, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, TransportError(req.Context(), provider, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		return nil, ErrorFromResponse(provider, resp)
	}
	return resp, nil
}

// DoJSON sends req like Do and decodes the bounded JSON response into out
// like DecodeJSON. The response body is always closed, so callers of a
// non-streaming endpoint never hold an *http.Response.
func DoJSON(client *http.Client, provider string, req *http.Request, out any) error {
	resp, err := Do(client, provider, req)
	if err != nil {
		return err
	}
	return DecodeJSON(provider, resp, out)
}

// TransportError converts a client.Do error. Context errors are passed
// through (wrapped) so errors.Is(err, context.DeadlineExceeded) works.
func TransportError(ctx context.Context, provider string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("ai: %s: %w", provider, ctxErr)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("ai: %s: %w", provider, err)
	}
	return &ai.UpstreamError{Provider: provider, Message: "request failed: " + ErrorText(err)}
}

// ErrorText returns err's message without the request URL that *url.Error
// embeds (Post "https://host/path?query": …). Upstream error messages are
// shown to end users, and endpoint URLs can reveal internal hosts, Azure
// deployment names or Vertex project ids. The result is truncated.
func ErrorText(err error) string {
	if err == nil {
		return ""
	}
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return Truncate(ue.Err.Error())
	}
	return Truncate(err.Error())
}

// DecodeJSON decodes a bounded JSON response body into out and closes it.
func DecodeJSON(provider string, resp *http.Response, out any) error {
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		if resp.Request != nil {
			if ctxErr := resp.Request.Context().Err(); ctxErr != nil {
				return fmt.Errorf("ai: %s: %w", provider, ctxErr)
			}
		}
		return &ai.UpstreamError{Provider: provider, Message: "read response: " + ErrorText(err)}
	}
	if len(b) > MaxResponseBytes {
		return &ai.UpstreamError{Provider: provider, Message: "response too large"}
	}
	if err := json.Unmarshal(b, out); err != nil {
		return &ai.UpstreamError{Provider: provider, Message: "invalid JSON response: " + err.Error()}
	}
	return nil
}

// ErrorFromResponse reads (a bounded prefix of) an error response body and
// returns it as an *ai.UpstreamError. It does not close the body. The
// error's RetryDelay comes from the response headers (see RetryAfter) or,
// when they give none, from a google.rpc.RetryInfo detail in the body.
func ErrorFromResponse(provider string, resp *http.Response) *ai.UpstreamError {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBytes))
	msg := ErrorMessage(b)
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	if msg == "" {
		msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	retry := RetryAfter(resp.StatusCode, resp.Header, time.Now())
	if retry == 0 {
		retry = RetryInfoDelay(b)
	}
	return &ai.UpstreamError{Provider: provider, Status: resp.StatusCode, Message: msg, RetryDelay: retry}
}

// ErrorDetail is the structured part of common provider error bodies.
type ErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   string `json:"param"`
	Code    any    `json:"code"`   // string (OpenAI) or number (Gemini)
	Status  string `json:"status"` // Gemini: e.g. INVALID_ARGUMENT
}

// ParseErrorDetail extracts the {"error": {...}} object of an error body
// (also accepting a top-level array of such objects, as Gemini streams use).
func ParseErrorDetail(body []byte) (ErrorDetail, bool) {
	body = bytes.TrimSpace(body)
	if len(body) > 0 && body[0] == '[' {
		var arr []json.RawMessage
		if json.Unmarshal(body, &arr) == nil && len(arr) > 0 {
			body = arr[0]
		}
	}
	var env struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &env) != nil || len(env.Error) == 0 {
		return ErrorDetail{}, false
	}
	var d ErrorDetail
	if json.Unmarshal(env.Error, &d) == nil && d.Message != "" {
		return d, true
	}
	var s string
	if json.Unmarshal(env.Error, &s) == nil && s != "" {
		return ErrorDetail{Message: s}, true
	}
	return ErrorDetail{}, false
}

// ErrorMessage extracts a human-readable message from an error body.
func ErrorMessage(body []byte) string {
	if d, ok := ParseErrorDetail(body); ok {
		return Truncate(d.Message)
	}
	var alt struct {
		Message string `json:"message"`
		Detail  any    `json:"detail"`
		// Cloudflare API v4 envelope: {"success":false,"errors":[{"code":…,"message":…}]}
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if json.Unmarshal(bytes.TrimSpace(body), &alt) == nil {
		if alt.Message != "" {
			return Truncate(alt.Message)
		}
		if s, ok := alt.Detail.(string); ok && s != "" {
			return Truncate(s)
		}
		var msgs []string
		for _, e := range alt.Errors {
			if e.Message != "" {
				msgs = append(msgs, e.Message)
			}
		}
		if len(msgs) > 0 {
			return Truncate(strings.Join(msgs, "; "))
		}
	}
	return Truncate(strings.TrimSpace(string(body)))
}

// Truncate shortens s to a bounded, valid UTF-8 message.
func Truncate(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxMessageLen {
		return s
	}
	s = s[:maxMessageLen]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "…"
}

// DefaultClient returns an HTTP client for upstream AI APIs. It sets no
// overall timeout: the router bounds every call with a context deadline
// (ai.request_timeout, which operators may raise well beyond the usual few
// minutes for slow non-streaming generations), so the response-header
// timeout is only a backstop for callers without a deadline.
func DefaultClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 30 * time.Minute
	tr.MaxIdleConnsPerHost = 32
	return &http.Client{Transport: tr}
}
