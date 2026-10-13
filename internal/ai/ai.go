// Package ai defines provider-neutral types for the AI proxy (backyard.ai).
// Providers live in sub-packages (anthropic, openai, bedrock, gemini) and the
// router package maps client-facing aliases to provider models.
package ai

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrUnsupported: the provider/model does not support this operation.
	ErrUnsupported = errors.New("ai: operation not supported by this model")
	// ErrUnknownModel: alias not configured (or raw models disabled).
	ErrUnknownModel = errors.New("ai: unknown model")
	// ErrDisabled: AI is not configured on this server.
	ErrDisabled = errors.New("ai: disabled")
	// ErrBadRequest: invalid request (empty messages, bad image, …).
	ErrBadRequest = errors.New("ai: bad request")
	// ErrNotLocal is returned by an ImageResolver for an absolute URL that
	// does not point at content the server holds itself; the URL is then
	// handled like any other remote image URL.
	ErrNotLocal = errors.New("ai: not a local image")
)

// UpstreamError wraps an error returned by a provider API.
type UpstreamError struct {
	Provider string
	Status   int // HTTP status from the provider, 0 if not applicable
	Message  string
	// RetryDelay is how long the provider asked callers to wait before
	// retrying (its Retry-After, retry-after-ms or rate-limit reset
	// headers, or Google's RetryInfo), 0 when it did not say.
	RetryDelay time.Duration
}

func (e *UpstreamError) Error() string {
	return "ai: " + e.Provider + " error: " + e.Message
}

// RetryAfter returns RetryDelay (0 when the provider gave no retry time).
// The HTTP layer looks for this method on any error in a chain to set the
// Retry-After of a throttled (429) response.
func (e *UpstreamError) RetryAfter() time.Duration {
	if e == nil || e.RetryDelay < 0 {
		return 0
	}
	return e.RetryDelay
}

// Part is one piece of message content.
type Part struct {
	Type      string `json:"type"`                 // "text" | "image"
	Text      string `json:"text,omitempty"`       // type=text
	Data      []byte `json:"-"`                    // type=image: raw bytes (decoded from base64 "data")
	MediaType string `json:"media_type,omitempty"` // type=image: image/png, image/jpeg, image/gif, image/webp
	URL       string `json:"url,omitempty"`        // type=image: remote image URL (providers may fetch or reject)
}

// Message is one chat turn. Role is "user" or "assistant" (system prompts are
// carried in ChatRequest.System).
type Message struct {
	Role    string `json:"role"`
	Content []Part `json:"content"`
}

// ChatRequest is a provider-level chat request (Model is the provider model id).
type ChatRequest struct {
	Model       string
	System      string
	Messages    []Message
	MaxTokens   int
	Temperature *float64
	JSON        bool   // ask for a single JSON object response
	Effort      string // optional reasoning effort hint (anthropic)
	User        string // end-user id for provider abuse monitoring (optional)

	// ResolveImage, when set, loads images the request references by a URL
	// of the calling site's own content (see ImageResolver). It is used by
	// the Service (router) before the request reaches a provider; providers
	// never see it and ignore it if set.
	ResolveImage ImageResolver
}

// ImageResolver loads an image that a chat request references by URL when
// that URL points at content the server holds itself — typically the
// calling site's uploads (/__backyard/files/{id}/{name}, which is what
// backyard.files.upload returns). The HTTP layer builds one per request,
// bound to the request's site, user and origin; the router calls it:
//
//   - for every site-relative URL ("/…", but not "//host/…"); without a
//     resolver such URLs are rejected as bad requests;
//   - for every absolute http(s) URL; the resolver returns ErrNotLocal for
//     URLs it does not serve (another origin), which are then passed to
//     providers or downloaded by the SSRF-guarded fetcher as before.
//
// ref is the URL exactly as the client sent it (trimmed). maxBytes is the
// most the router accepts for this image (the per-image limit, lowered by
// what is left of the request's image byte budget); the resolver should
// check the stored size before reading and must not return more. Errors
// wrapping ErrBadRequest (unknown file, another site's file, too large)
// become 400 responses; context errors are passed through; any other error
// (a storage failure) is returned to the caller wrapped. The media type is
// detected from the bytes (png, jpeg, gif or webp; anything else is a bad
// request), so a stored content type is neither needed nor trusted.
type ImageResolver func(ctx context.Context, ref string, maxBytes int64) ([]byte, error)

// Usage reports token consumption.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Normalized stop reasons.
const (
	StopEndTurn      = "end_turn"
	StopMaxTokens    = "max_tokens"
	StopStopSequence = "stop_sequence"
	StopRefusal      = "refusal"
	StopOther        = "other"
)

// ChatResponse is the complete result of a chat request.
type ChatResponse struct {
	Content    string `json:"content"`
	Model      string `json:"model"` // provider model id (router replaces with alias)
	StopReason string `json:"stop_reason"`
	Usage      Usage  `json:"usage"`
}

// ImageRequest asks for generated images.
type ImageRequest struct {
	Model  string
	Prompt string
	Size   string // e.g. "1024x1024"; provider-specific values passed through
	N      int
	User   string
}

// Image is one generated image.
type Image struct {
	Data      []byte
	MediaType string
}

// ImageResponse holds generated images.
type ImageResponse struct {
	Images []Image
}

// EmbedRequest asks for embeddings.
type EmbedRequest struct {
	Model string
	Input []string
	User  string
}

// EmbedResponse holds one vector per input, in order.
type EmbedResponse struct {
	Embeddings [][]float64
	Usage      Usage
}

// Provider is an upstream AI API. Implementations must be safe for concurrent use.
type Provider interface {
	// Name returns the configured provider key (e.g. "anthropic").
	Name() string
	// Chat performs a non-streaming completion.
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
	// ChatStream streams text deltas to onDelta and returns the final response
	// (Content contains the full text). If onDelta returns an error the stream
	// is aborted and that error returned.
	ChatStream(ctx context.Context, req ChatRequest, onDelta func(text string) error) (*ChatResponse, error)
	// Image generates images or returns ErrUnsupported.
	Image(ctx context.Context, req ImageRequest) (*ImageResponse, error)
	// Embed computes embeddings or returns ErrUnsupported.
	Embed(ctx context.Context, req EmbedRequest) (*EmbedResponse, error)
}

// ModelInfo describes a client-visible alias.
type ModelInfo struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"` // chat | image | embedding
	Provider    string `json:"provider"`
	Model       string `json:"model,omitempty"` // only if ai.expose_model_ids
	Description string `json:"description,omitempty"`
}

// Service is what the HTTP layer uses: alias resolution + provider dispatch.
// Implemented by router.Router.
type Service interface {
	// Chat resolves req.Model (an alias, "" = default, or "provider/model" if
	// raw models are allowed) and dispatches. The response's Model is the alias.
	Chat(ctx context.Context, alias string, req ChatRequest) (*ChatResponse, error)
	ChatStream(ctx context.Context, alias string, req ChatRequest, onDelta func(string) error) (*ChatResponse, error)
	Image(ctx context.Context, alias string, req ImageRequest) (*ImageResponse, string, error) // returns alias used
	Embed(ctx context.Context, alias string, req EmbedRequest) (*EmbedResponse, string, error)
	// Models lists configured aliases (sorted by name) and the default chat alias.
	Models() (defaultAlias string, models []ModelInfo)
	// Enabled reports whether any model is configured.
	Enabled() bool
}
