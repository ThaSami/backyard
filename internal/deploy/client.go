package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/thasami/backyard/internal/identity"
	"github.com/thasami/backyard/internal/sites"
	"github.com/thasami/backyard/internal/version"
)

const (
	// DefaultReadFileMax caps Client.ReadFile when maxBytes <= 0.
	DefaultReadFileMax = 32 << 20
	// maxResponseJSON caps JSON responses read by the client.
	maxResponseJSON = 128 << 20
	// maxErrorBody caps error bodies read by the client.
	maxErrorBody = 64 << 10
	// maxAttempts is the number of tries (1 + retries) for retryable calls.
	maxAttempts = 4
	// maxRetryAfter is the longest Retry-After the client waits for; longer
	// throttling (e.g. an exhausted hourly deploy quota) fails immediately.
	maxRetryAfter = 15 * time.Second
)

// ErrFileTooLarge is returned by Client.ReadFile when a file exceeds maxBytes.
var ErrFileTooLarge = errors.New("deploy: file exceeds the read limit")

// Client talks to a deploy gateway.
//
// A Client is safe for concurrent use once configured.
type Client struct {
	BaseURL   string // gateway base URL G
	Token     string // bearer token ("" for anonymous)
	HTTP      *http.Client
	UserAgent string
}

// NewClient returns a client with sane timeouts: 30 s to connect, 15 s for
// TLS handshakes and 15 min for response headers (what the gateway allows a
// create, commit or refresh of a large site), no overall deadline (uploads
// may be long; cancel via the context). Cookies are kept
// for the client's lifetime so a guest identity stays stable between the
// create and commit calls of an anonymous deploy.
func NewClient(baseURL, token string) *Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	tr.TLSHandshakeTimeout = 15 * time.Second
	tr.ResponseHeaderTimeout = longRequestTimeout
	tr.IdleConnTimeout = 90 * time.Second
	tr.MaxIdleConnsPerHost = 32
	jar, _ := cookiejar.New(nil)
	return &Client{
		BaseURL:   strings.TrimSuffix(strings.TrimSpace(baseURL), "/"),
		Token:     strings.TrimSpace(token),
		HTTP:      &http.Client{Transport: tr, Jar: jar},
		UserAgent: "backyard-cli/" + version.Version,
	}
}

// APIError is returned for non-2xx gateway responses.
type APIError struct {
	Status   int
	Code     string
	Message  string
	LoginURL string
}

func (e *APIError) Error() string { return e.Code + ": " + e.Message }

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) base() string { return strings.TrimSuffix(c.BaseURL, "/") }

// Info fetches GET /info.
func (c *Client) Info(ctx context.Context) (*Info, error) {
	var out Info
	if err := c.doJSON(ctx, http.MethodGet, "/info", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Me fetches GET /me: the identity behind the client's token (anonymous
// guests in auth mode none without a token).
func (c *Client) Me(ctx context.Context) (*identity.User, error) {
	var out identity.User
	if err := c.doJSON(ctx, http.MethodGet, "/me", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListSites fetches GET /sites.
func (c *Client) ListSites(ctx context.Context) ([]sites.Meta, error) {
	var out struct {
		Sites []sites.Meta `json:"sites"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/sites", nil, &out); err != nil {
		return nil, err
	}
	return out.Sites, nil
}

// GetSite fetches GET /sites/{site}.
func (c *Client) GetSite(ctx context.Context, site string) (*sites.Meta, error) {
	var out sites.Meta
	if err := c.doJSON(ctx, http.MethodGet, sitePath(site), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SiteFiles fetches GET /sites/{site}/files.
func (c *Client) SiteFiles(ctx context.Context, site string) ([]SiteFile, error) {
	var out struct {
		Files []SiteFile `json:"files"`
	}
	if err := c.doJSON(ctx, http.MethodGet, sitePath(site)+"/files", nil, &out); err != nil {
		return nil, err
	}
	return out.Files, nil
}

// ReadFile fetches GET /sites/{site}/files/{path} (maxBytes caps the read; <=0 = 32 MiB).
func (c *Client) ReadFile(ctx context.Context, site, path string, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultReadFileMax
	}
	if why := ValidatePath(path); why != "" {
		return nil, fmt.Errorf("deploy: invalid path %q: %s", path, why)
	}
	var out []byte
	err := c.withRetry(ctx, retryGateway, func() (*http.Response, error) {
		req, err := c.newRequest(ctx, http.MethodGet, sitePath(site)+"/files/"+escapePath(path), nil)
		if err != nil {
			return nil, err
		}
		return c.httpClient().Do(req)
	}, func(resp *http.Response) error {
		if resp.ContentLength > maxBytes {
			return fmt.Errorf("%w: %s is %d bytes (limit %d)", ErrFileTooLarge, path, resp.ContentLength, maxBytes)
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
		if err != nil {
			return fmt.Errorf("deploy: read %s: %w", path, err)
		}
		if int64(len(b)) > maxBytes {
			return fmt.Errorf("%w: %s exceeds %d bytes", ErrFileTooLarge, path, maxBytes)
		}
		out = b
		return nil
	})
	return out, err
}

// DeleteSite calls DELETE /sites/{site}.
func (c *Client) DeleteSite(ctx context.Context, site string) error {
	return c.doJSON(ctx, http.MethodDelete, sitePath(site), nil, nil)
}

// Refresh calls POST /sites/{site}/refresh.
func (c *Client) Refresh(ctx context.Context, site string) (*sites.Meta, error) {
	var out sites.Meta
	if err := c.doJSON(ctx, http.MethodPost, sitePath(site)+"/refresh", struct{}{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetOwners calls POST /sites/{site}/owners.
func (c *Client) SetOwners(ctx context.Context, site string, add, remove []string) (*sites.Meta, error) {
	var out sites.Meta
	if err := c.doJSON(ctx, http.MethodPost, sitePath(site)+"/owners", OwnersRequest{Add: add, Remove: remove}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// createDeploy calls POST /sites/{site}/deploys.
func (c *Client) createDeploy(ctx context.Context, site string, req CreateDeployRequest) (*CreateDeployResponse, error) {
	var out CreateDeployResponse
	if err := c.doJSON(ctx, http.MethodPost, sitePath(site)+"/deploys", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// commitDeploy calls POST /sites/{site}/deploys/{id}/commit.
func (c *Client) commitDeploy(ctx context.Context, site, id string, req CommitRequest) (*CommitResponse, error) {
	var out CommitResponse
	if err := c.doJSON(ctx, http.MethodPost, sitePath(site)+"/deploys/"+url.PathEscape(id)+"/commit", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// sitePath returns "/sites/{site}" with the name escaped.
func sitePath(site string) string { return "/sites/" + url.PathEscape(site) }

// escapePath escapes each "/"-separated segment.
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// newRequest builds a gateway request with auth, CSRF and UA headers.
func (c *Client) newRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	var rd io.Reader = http.NoBody
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base()+path, rd)
	if err != nil {
		return nil, fmt.Errorf("deploy: build request: %w", err)
	}
	c.decorate(req)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// decorate adds the headers sent to the gateway itself.
func (c *Client) decorate(req *http.Request) {
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		req.Header.Set("X-Backyard", "1")
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
}

// doJSON performs a JSON API call with retries for transient failures.
func (c *Client) doJSON(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("deploy: encode request: %w", err)
		}
		body = b
	}
	return c.withRetry(ctx, retryGateway, func() (*http.Response, error) {
		req, err := c.newRequest(ctx, method, path, body)
		if err != nil {
			return nil, err
		}
		return c.httpClient().Do(req)
	}, func(resp *http.Response) error {
		if out == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
			return nil
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseJSON)).Decode(out); err != nil {
			return fmt.Errorf("deploy: decode %s %s response: %w", method, path, err)
		}
		return nil
	})
}

// retryPolicy decides whether a status is worth retrying.
type retryPolicy func(status int) bool

// retryGateway retries throttling and gateway/proxy failures only: the
// gateway's own 500s are deterministic.
func retryGateway(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// retryUpload retries throttling, timeouts and every 5xx (object stores
// return 500/503 for transient failures).
func retryUpload(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusRequestTimeout || status >= 500
}

// withRetry runs do up to maxAttempts times. Network errors and statuses
// accepted by policy are retried with exponential backoff (honouring
// Retry-After); a 2xx response is passed to handle; any other status becomes
// an *APIError.
func (c *Client) withRetry(ctx context.Context, policy retryPolicy, do func() (*http.Response, error), handle func(*http.Response) error) error {
	var (
		lastErr    error
		retryAfter time.Duration
	)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			wait := backoff(attempt)
			if retryAfter > 0 {
				wait = retryAfter
			}
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return errors.Join(ctx.Err(), lastErr)
			case <-t.C:
			}
		}
		resp, err := do()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			lastErr, retryAfter = fmt.Errorf("deploy: %w", err), 0
			if isRetryableNetErr(err) {
				continue
			}
			return lastErr
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			err := handle(resp)
			resp.Body.Close()
			return err
		}
		ae, ra := readAPIError(resp)
		resp.Body.Close()
		lastErr, retryAfter = ae, ra
		if !policy(resp.StatusCode) || ra > maxRetryAfter {
			return ae // not transient, or not worth waiting for
		}
	}
	return lastErr
}

// readAPIError converts a non-2xx response to *APIError and its
// Retry-After delay (0 when absent).
func readAPIError(resp *http.Response) (*APIError, time.Duration) {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	e := &APIError{Status: resp.StatusCode}
	var eb ErrorBody
	if json.Unmarshal(b, &eb) == nil && eb.Error.Code != "" {
		e.Code, e.Message, e.LoginURL = eb.Error.Code, eb.Error.Message, eb.Error.LoginURL
	} else {
		e.Code = codeForStatus(resp.StatusCode)
		msg := strings.Join(strings.Fields(string(b)), " ")
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		e.Message = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, truncate(msg, 300))
	}
	var wait time.Duration
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(ra)); err == nil && secs >= 0 {
			wait = time.Duration(secs) * time.Second
		} else if t, err := http.ParseTime(ra); err == nil {
			wait = time.Until(t)
		}
	}
	return e, wait
}

// retryBase is the first retry delay (a variable so tests can shorten it).
var retryBase = 500 * time.Millisecond

// backoff returns the delay before retry number attempt (1-based):
// 0.5 s, 1 s, 2 s … capped at 16× the base (8 s), with ±25 % jitter.
func backoff(attempt int) time.Duration {
	d := retryBase << (attempt - 1)
	d = min(d, 16*retryBase)
	jitter := time.Duration(rand.Int64N(int64(d)/2)) - d/4
	return d + jitter
}

// isRetryableNetErr reports whether a transport error is likely transient.
func isRetryableNetErr(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var pe *permanentError
	if errors.As(err, &pe) {
		return false
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		// Malformed URLs, unsupported schemes and TLS certificate problems
		// will not fix themselves.
		msg := ue.Err.Error()
		if strings.Contains(msg, "unsupported protocol scheme") || strings.Contains(msg, "certificate") {
			return false
		}
	}
	return true
}
