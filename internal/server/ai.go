package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/thasami/backyard/internal/ai"
	"github.com/thasami/backyard/internal/docstore"
	"github.com/thasami/backyard/internal/ratelimit"
)

// chatBodyLimit bounds /ai/chat bodies (inline base64 images).
const chatBodyLimit = 32 << 20

// sseHeartbeat is the interval of keep-alive comments on idle streams (a
// variable for tests).
var sseHeartbeat = 15 * time.Second

type chatBody struct {
	Messages    []chatMessage `json:"messages"`
	Model       string        `json:"model"`
	System      string        `json:"system"`
	MaxTokens   int           `json:"max_tokens"`
	Temperature *float64      `json:"temperature"`
	JSON        bool          `json:"json"`
	Stream      bool          `json:"stream"`
}

type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type chatPart struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	Data      string `json:"data"`
	MediaType string `json:"media_type"`
	URL       string `json:"url"`
}

// aiEnabled writes 501 when AI is not configured.
func (s *Server) aiEnabled(w http.ResponseWriter, r *http.Request) bool {
	if s.deps.AI == nil || !s.deps.AI.Enabled() {
		s.writeAPIError(w, r, errUnsupported("AI is not configured on this server"))
		return false
	}
	return true
}

// aiGate checks AI availability, the AI rate limit and the daily token
// budget, and extends the connection's write deadline for slow models.
func (s *Server) aiGate(w http.ResponseWriter, r *http.Request) bool {
	if !s.aiEnabled(w, r) || !s.allow(w, r, ratelimit.AI) {
		return false
	}
	extendDeadlines(w, 0, s.aiCallBudget())
	if ok, wait := s.limiter.CheckTokens(rateKey(r)); !ok {
		s.metrics.rateLimited.WithLabelValues("ai_tokens").Inc()
		e := newError(http.StatusTooManyRequests, codeRateLimited, "daily AI token budget exhausted; it resets at midnight UTC")
		e.RetryAfter = wait
		s.writeAPIError(w, r, e)
		return false
	}
	return true
}

// aiCallBudget is how long an AI request may take end to end: the router's
// upstream timeout (image generation: at least 10 minutes) plus a margin.
func (s *Server) aiCallBudget() time.Duration {
	return max(s.cfg.AI.RequestTimeout.D(), 10*time.Minute) + time.Minute
}

// aiError maps ai errors to API errors.
func aiError(err error) *apiError {
	var ue *ai.UpstreamError
	switch {
	case errors.Is(err, ai.ErrBadRequest):
		return errBadRequest("%s", strings.TrimPrefix(err.Error(), "ai: bad request: "))
	case errors.Is(err, ai.ErrUnknownModel):
		return errNotFound("%s", strings.TrimPrefix(err.Error(), "ai: "))
	case errors.Is(err, ai.ErrUnsupported):
		return errUnsupported("%s", strings.TrimPrefix(err.Error(), "ai: "))
	case errors.Is(err, ai.ErrDisabled):
		return errUnsupported("AI is not configured on this server")
	case errors.As(err, &ue):
		return aiUpstreamError(err, ue)
	case errors.Is(err, context.DeadlineExceeded):
		return errContext(err, "AI request")
	case errors.Is(err, context.Canceled):
		return errContext(err, "AI request")
	}
	e := newError(http.StatusBadGateway, codeUpstream, "AI request failed")
	e.Err = err
	return e
}

// aiUpstreamError maps a provider failure by the provider's HTTP status:
//
//   - 504 (the router's own timeout, or the provider's) and 408 → 504;
//   - 429 (the provider throttles this server's account, which all users
//     share) → 429 with Retry-After when the provider said when to retry,
//     else 503: a 429 without a retry time would read as the caller's own
//     rate limit;
//   - 400 and 422 (the provider rejected the request: an assistant prefill
//     the model does not support, an image it cannot read, …) → 400
//     bad_request, and 413 → 413 too_large;
//   - anything else (bad credentials, provider outages, malformed
//     responses) → 502.
//
// Provider messages are sanitized before they reach the client (see
// safeUpstreamMessage); err itself is logged for 5xx responses.
func aiUpstreamError(err error, ue *ai.UpstreamError) *apiError {
	var e *apiError
	switch ue.Status {
	case http.StatusGatewayTimeout, http.StatusRequestTimeout:
		e = newError(http.StatusGatewayTimeout, codeUpstream, "the AI provider did not respond in time")
	case http.StatusTooManyRequests:
		if d := upstreamRetryAfter(err, ue.Message); d > 0 {
			e = newError(http.StatusTooManyRequests, codeRateLimited, "the AI provider is rate limiting requests; retry later")
			e.RetryAfter = d
		} else {
			e = newError(http.StatusServiceUnavailable, codeUpstream, "the AI provider is rate limiting requests; retry later")
		}
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		e = errBadRequest("the AI provider rejected the request: %s", safeUpstreamMessage(ue.Message, ue.Status))
	case http.StatusRequestEntityTooLarge:
		e = newError(http.StatusRequestEntityTooLarge, codeTooLarge, "the AI provider rejected the request as too large: %s",
			safeUpstreamMessage(ue.Message, ue.Status))
	default:
		e = newError(http.StatusBadGateway, codeUpstream, "AI provider error: %s", safeUpstreamMessage(ue.Message, ue.Status))
	}
	e.Err = err
	return e
}

// retryAfterer is implemented by errors that carry a provider's
// Retry-After. ai.UpstreamError has no such field; a provider or the router
// may put such an error in the chain.
type retryAfterer interface {
	RetryAfter() time.Duration
}

// maxUpstreamRetryAfter bounds a provider's Retry-After passed on to
// clients.
const maxUpstreamRetryAfter = time.Hour

// retryHintRe finds the retry delay providers put into rate-limit messages
// ("Please try again in 1.5s" (OpenAI), "Please retry after 6 seconds"
// (Azure OpenAI)).
var retryHintRe = regexp.MustCompile(`(?i)\b(?:retry after|try again in)\s+(\d{1,6}(?:\.\d{1,3})?)\s*(ms|milliseconds?|s|secs?|seconds?|m|mins?|minutes?)?\b`)

// upstreamRetryAfter returns when a throttled provider accepts requests
// again, or 0 when unknown: from an error in err's chain that implements
// retryAfterer, else from a hint in the provider message.
func upstreamRetryAfter(err error, msg string) time.Duration {
	var d time.Duration
	var ra retryAfterer
	if errors.As(err, &ra) {
		d = ra.RetryAfter()
	}
	if d <= 0 {
		d = retryHint(msg)
	}
	if d <= 0 {
		return 0
	}
	return min(d, maxUpstreamRetryAfter)
}

// retryHint parses the retry delay of a provider message (0 if none).
func retryHint(msg string) time.Duration {
	m := retryHintRe.FindStringSubmatch(msg)
	if m == nil {
		return 0
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0
	}
	unit := time.Second
	switch u := strings.ToLower(m[2]); {
	case strings.HasPrefix(u, "ms") || strings.HasPrefix(u, "milli"):
		unit = time.Millisecond
	case strings.HasPrefix(u, "m"):
		unit = time.Minute
	}
	return time.Duration(v * float64(unit))
}

// maxUpstreamMessage bounds provider messages relayed to clients (bytes).
const maxUpstreamMessage = 300

var (
	// upstreamURLRe matches URLs in provider messages: endpoint URLs can
	// reveal internal hosts, Azure resource names or Vertex project ids.
	upstreamURLRe = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]{1,15}://[^\s"'<>()]+`)
	// upstreamSecretRe matches account identifiers and key-like tokens
	// providers echo in messages ("organization org-…", "sk-…****abcd",
	// AWS access key ids, bearer tokens).
	upstreamSecretRe = regexp.MustCompile(`\b(?:(?:sk|pk|rk|org|proj|key|sess)[-_][A-Za-z0-9*_-]{6,}|A(?:KIA|SIA)[A-Z0-9]{12,}|(?i:bearer)\s+[A-Za-z0-9._~+/=-]{8,})`)
)

// safeUpstreamMessage prepares a provider error message for clients: one
// line of printable text without URLs, account ids or key-like tokens,
// at most maxUpstreamMessage bytes (cut at a rune boundary). An empty
// result falls back to the HTTP status text.
func safeUpstreamMessage(msg string, status int) string {
	msg = strings.ToValidUTF8(msg, "")
	msg = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) {
			return ' '
		}
		return r
	}, msg)
	msg = upstreamURLRe.ReplaceAllString(msg, "[url]")
	msg = upstreamSecretRe.ReplaceAllString(msg, "[redacted]")
	msg = strings.Join(strings.Fields(msg), " ")
	if len(msg) > maxUpstreamMessage {
		cut := maxUpstreamMessage
		for cut > 0 && !utf8.RuneStart(msg[cut]) {
			cut--
		}
		msg = strings.TrimSpace(msg[:cut]) + "…"
	}
	if msg == "" {
		msg = strings.ToLower(http.StatusText(status))
	}
	if msg == "" {
		msg = "request failed"
	}
	return msg
}

// aiResult is the metrics label of an AI outcome.
func aiResult(err error) string {
	if err == nil {
		return "ok"
	}
	return aiError(err).Code
}

// modelLabel bounds the cardinality of the model metric label.
func (s *Server) modelLabel(alias string) string {
	if alias == "" {
		return "default"
	}
	if _, ok := s.cfg.AI.Models[alias]; ok {
		return alias
	}
	if strings.Contains(alias, "/") {
		return "raw"
	}
	if s.deps.AI != nil {
		_, models := s.deps.AI.Models()
		for _, m := range models {
			if m.Name == alias {
				return alias
			}
		}
	}
	return "unknown"
}

func (s *Server) observeAI(alias string, err error, usage *ai.Usage) {
	label := s.modelLabel(alias)
	s.metrics.aiRequests.WithLabelValues(label, aiResult(err)).Inc()
	if usage != nil {
		if usage.InputTokens > 0 {
			s.metrics.aiTokens.WithLabelValues(label, "input").Add(float64(usage.InputTokens))
		}
		if usage.OutputTokens > 0 {
			s.metrics.aiTokens.WithLabelValues(label, "output").Add(float64(usage.OutputTokens))
		}
	}
}

// checkChatModel resolves alias like the router would and reports problems
// as API errors before any upstream call, so that a streaming request with
// an unknown model (or a model of the wrong kind) gets a clean 404/400
// instead of an error event after a 200 response.
func (s *Server) checkChatModel(alias string) *apiError {
	def, models := s.deps.AI.Models()
	if alias == "" {
		if def == "" {
			return errNotFound("no default chat model is configured")
		}
		return nil
	}
	for _, m := range models {
		if m.Name != alias {
			continue
		}
		if m.Kind != "" && m.Kind != "chat" {
			return errBadRequest("model %q is a %s model, not a chat model", alias, m.Kind)
		}
		return nil
	}
	if strings.Contains(alias, "/") && s.cfg.AI.AllowRawModels {
		return nil // "provider/model-id": validated by the router
	}
	return errNotFound("unknown model %q", alias)
}

// parseChat converts the wire request into a provider-neutral request.
func parseChat(b *chatBody) (ai.ChatRequest, *apiError) {
	req := ai.ChatRequest{
		System:      strings.TrimSpace(b.System),
		MaxTokens:   b.MaxTokens,
		Temperature: b.Temperature,
		JSON:        b.JSON,
	}
	if b.MaxTokens < 0 {
		return req, errBadRequest("max_tokens must be >= 0")
	}
	if len(b.Messages) == 0 {
		return req, errBadRequest("messages must not be empty")
	}
	var system []string
	if req.System != "" {
		system = append(system, req.System)
	}
	for i, m := range b.Messages {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		parts, e := parseContent(i, m.Content)
		if e != nil {
			return req, e
		}
		switch role {
		case "system":
			for _, p := range parts {
				if p.Type != "text" {
					return req, errBadRequest("message %d: system messages may only contain text", i)
				}
			}
			if t := strings.TrimSpace(ai.TextOf(parts)); t != "" {
				system = append(system, t)
			}
		case "user", "assistant":
			req.Messages = append(req.Messages, ai.Message{Role: role, Content: parts})
		default:
			return req, errBadRequest("message %d: role must be user, assistant or system", i)
		}
	}
	if len(req.Messages) == 0 {
		return req, errBadRequest("messages must contain at least one user message")
	}
	req.System = strings.Join(system, "\n\n")
	return req, nil
}

// parseContent accepts a string or an array of parts.
func parseContent(i int, raw json.RawMessage) ([]ai.Part, *apiError) {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "null" {
		return nil, errBadRequest("message %d: content is required", i)
	}
	if raw[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return nil, errBadRequest("message %d: invalid content", i)
		}
		return []ai.Part{{Type: "text", Text: text}}, nil
	}
	var parts []chatPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, errBadRequest("message %d: content must be a string or an array of parts", i)
	}
	if len(parts) == 0 {
		return nil, errBadRequest("message %d: content is empty", i)
	}
	out := make([]ai.Part, 0, len(parts))
	for j, p := range parts {
		switch strings.ToLower(p.Type) {
		case "text":
			out = append(out, ai.Part{Type: "text", Text: p.Text})
		case "image", "image_url":
			img, e := parseImagePart(i, j, p)
			if e != nil {
				return nil, e
			}
			out = append(out, img)
		default:
			return nil, errBadRequest("message %d part %d: unknown type %q (text or image)", i, j, p.Type)
		}
	}
	return out, nil
}

func parseImagePart(i, j int, p chatPart) (ai.Part, *apiError) {
	part := ai.Part{Type: "image", MediaType: strings.ToLower(strings.TrimSpace(p.MediaType))}
	switch {
	case p.Data != "":
		if data, mt, ok := ai.ParseDataURL(p.Data); ok {
			part.Data = data
			if part.MediaType == "" {
				part.MediaType = mt
			}
		} else {
			data, err := decodeBase64(p.Data)
			if err != nil {
				return part, errBadRequest("message %d part %d: image data must be base64", i, j)
			}
			part.Data = data
		}
		if part.MediaType == "" {
			part.MediaType = http.DetectContentType(part.Data)
		}
	case p.URL != "":
		if data, mt, ok := ai.ParseDataURL(p.URL); ok {
			part.Data = data
			if part.MediaType == "" {
				part.MediaType = mt
			}
		} else {
			part.URL = p.URL
		}
	default:
		return part, errBadRequest("message %d part %d: image needs data or url", i, j)
	}
	return part, nil
}

// decodeBase64 accepts standard and URL-safe alphabets, padded or not.
func decodeBase64(s string) ([]byte, error) {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, s)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, errors.New("invalid base64")
}

// handleAIChat serves POST /ai/chat (JSON or server-sent events).
func (s *Server) handleAIChat(w http.ResponseWriter, r *http.Request) {
	if !s.aiGate(w, r) {
		return
	}
	var body chatBody
	if e := decodeJSON(w, r, chatBodyLimit, &body, false); e != nil {
		s.writeAPIError(w, r, e)
		return
	}
	req, e := parseChat(&body)
	if e != nil {
		s.writeAPIError(w, r, e)
		return
	}
	if e := s.checkChatModel(body.Model); e != nil {
		err := ai.ErrUnknownModel
		if e.Status == http.StatusBadRequest {
			err = ai.ErrBadRequest
		}
		s.observeAI(body.Model, err, nil)
		s.writeAPIError(w, r, e)
		return
	}
	ri := info(r)
	req.User = ri.user.ID
	if body.Stream {
		s.streamChat(w, r, body.Model, req)
		return
	}
	resp, err := s.deps.AI.Chat(r.Context(), body.Model, req)
	if err != nil {
		s.observeAI(body.Model, err, nil)
		s.writeAPIError(w, r, aiError(err))
		return
	}
	s.observeAI(resp.Model, nil, &resp.Usage)
	s.limiter.AddTokens(rateKey(r), int64(resp.Usage.InputTokens+resp.Usage.OutputTokens))
	writeJSON(w, http.StatusOK, resp)
}

// sseWriter serializes server-sent events (deltas and heartbeats).
type sseWriter struct {
	mu  sync.Mutex
	w   http.ResponseWriter
	rc  *http.ResponseController
	err error
}

func (sw *sseWriter) write(b []byte) error {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	if sw.err != nil {
		return sw.err
	}
	if _, err := sw.w.Write(b); err != nil {
		sw.err = err
		return err
	}
	if err := sw.rc.Flush(); err != nil {
		sw.err = err
	}
	return sw.err
}

func (sw *sseWriter) event(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode event: %w", err)
	}
	buf := make([]byte, 0, len(b)+8)
	buf = append(buf, "data: "...)
	buf = append(buf, b...)
	buf = append(buf, "\n\n"...)
	return sw.write(buf)
}

type sseText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type sseDone struct {
	Type       string   `json:"type"`
	Model      string   `json:"model"`
	StopReason string   `json:"stop_reason"`
	Usage      ai.Usage `json:"usage"`
}

type sseError struct {
	Type  string      `json:"type"`
	Error errorDetail `json:"error"`
}

// streamChat runs a streaming completion as text/event-stream.
func (s *Server) streamChat(w http.ResponseWriter, r *http.Request, alias string, req ai.ChatRequest) {
	rc := http.NewResponseController(w)
	// The router bounds the upstream call (streaming: at most 10 minutes);
	// the write deadline covers the whole stream with a margin, so a client
	// that stops reading cannot pin this goroutine forever.
	_ = rc.SetWriteDeadline(time.Now().Add(s.aiCallBudget()))
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	sw := &sseWriter{w: w, rc: rc}
	// Send the headers right away; keep-alive comments (": ping") follow
	// only when the model is silent for sseHeartbeat.
	if err := rc.Flush(); err != nil {
		return
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(sseHeartbeat)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if sw.write([]byte(": ping\n\n")) != nil {
					return
				}
			}
		}
	}()
	resp, err := s.deps.AI.ChatStream(r.Context(), alias, req, func(text string) error {
		if text == "" {
			return nil
		}
		return sw.event(sseText{Type: "text", Text: text})
	})
	close(stop)
	<-done
	if err != nil {
		s.observeAI(alias, err, nil)
		e := aiError(err)
		if e.Status >= 500 && !errors.Is(err, context.Canceled) {
			s.log.WarnContext(r.Context(), "AI stream failed", "error", err, "request_id", info(r).requestID)
		}
		_ = sw.event(sseError{Type: "error", Error: errorDetail{Code: e.Code, Message: e.Message}})
		return
	}
	s.observeAI(resp.Model, nil, &resp.Usage)
	s.limiter.AddTokens(rateKey(r), int64(resp.Usage.InputTokens+resp.Usage.OutputTokens))
	_ = sw.event(sseDone{Type: "done", Model: resp.Model, StopReason: resp.StopReason, Usage: resp.Usage})
}

type imageBody struct {
	Prompt string `json:"prompt"`
	Model  string `json:"model"`
	Size   string `json:"size"`
	N      int    `json:"n"`
}

type imageOut struct {
	ID          string `json:"id"`
	URL         string `json:"url"`
	ContentType string `json:"content_type"`
}

// handleAIImage serves POST /ai/image; images are stored as site files.
func (s *Server) handleAIImage(w http.ResponseWriter, r *http.Request) {
	if !s.aiGate(w, r) {
		return
	}
	var body imageBody
	if e := decodeJSON(w, r, smallBodyLimit, &body, false); e != nil {
		s.writeAPIError(w, r, e)
		return
	}
	if strings.TrimSpace(body.Prompt) == "" {
		s.writeAPIError(w, r, errBadRequest("prompt is required"))
		return
	}
	if body.N < 0 {
		s.writeAPIError(w, r, errBadRequest("n must be >= 0"))
		return
	}
	if body.N == 0 {
		body.N = 1
	}
	ri := info(r)
	resp, alias, err := s.deps.AI.Image(r.Context(), body.Model, ai.ImageRequest{
		Prompt: body.Prompt, Size: body.Size, N: body.N, User: ri.user.ID,
	})
	if alias == "" {
		alias = body.Model
	}
	s.observeAI(alias, err, nil)
	if err != nil {
		s.writeAPIError(w, r, aiError(err))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	out := make([]imageOut, 0, len(resp.Images))
	for i, img := range resp.Images {
		mt := img.MediaType
		if mt == "" {
			mt = http.DetectContentType(img.Data)
		}
		name := fmt.Sprintf("image-%d%s", i+1, imageExt(mt))
		m, err := s.storeBytes(ctx, ri.site, ri.user, name, mt, img.Data)
		if err != nil {
			s.writeAPIError(w, r, s.storageOrDBError(err))
			return
		}
		out = append(out, imageOut{ID: m.id, URL: s.fileURL(ri.site, m.id, m.name), ContentType: m.contentType})
	}
	writeJSON(w, http.StatusOK, map[string]any{"model": alias, "images": out})
}

// storageOrDBError maps errors of storeBytes (blob write + metadata doc).
func (s *Server) storageOrDBError(err error) *apiError {
	if errors.Is(err, errUploadTooLarge) {
		return newError(http.StatusRequestEntityTooLarge, codeTooLarge, "file exceeds the maximum upload size of %d bytes", s.maxUpload())
	}
	for _, sentinel := range []error{docstore.ErrNotFound, docstore.ErrConflict, docstore.ErrTooLarge, docstore.ErrQuota, docstore.ErrInvalid} {
		if errors.Is(err, sentinel) {
			return s.dbError(err)
		}
	}
	return s.storageError(err)
}

func imageExt(mt string) string {
	switch strings.ToLower(mt) {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	}
	return ".bin"
}

type embedBody struct {
	Input json.RawMessage `json:"input"`
	Model string          `json:"model"`
}

// handleAIEmbed serves POST /ai/embed.
func (s *Server) handleAIEmbed(w http.ResponseWriter, r *http.Request) {
	if !s.aiGate(w, r) {
		return
	}
	var body embedBody
	if e := decodeJSON(w, r, 16<<20, &body, false); e != nil {
		s.writeAPIError(w, r, e)
		return
	}
	var inputs []string
	raw := strings.TrimSpace(string(body.Input))
	switch {
	case raw == "" || raw == "null":
		s.writeAPIError(w, r, errBadRequest("input is required"))
		return
	case raw[0] == '"':
		var one string
		if err := json.Unmarshal(body.Input, &one); err != nil {
			s.writeAPIError(w, r, errBadRequest("input must be a string or an array of strings"))
			return
		}
		inputs = []string{one}
	default:
		if err := json.Unmarshal(body.Input, &inputs); err != nil {
			s.writeAPIError(w, r, errBadRequest("input must be a string or an array of strings"))
			return
		}
		if len(inputs) == 0 {
			s.writeAPIError(w, r, errBadRequest("input must not be empty"))
			return
		}
	}
	resp, alias, err := s.deps.AI.Embed(r.Context(), body.Model, ai.EmbedRequest{Input: inputs, User: info(r).user.ID})
	if alias == "" {
		alias = body.Model
	}
	if err != nil {
		s.observeAI(alias, err, nil)
		s.writeAPIError(w, r, aiError(err))
		return
	}
	s.observeAI(alias, nil, &resp.Usage)
	s.limiter.AddTokens(rateKey(r), int64(resp.Usage.InputTokens+resp.Usage.OutputTokens))
	emb := resp.Embeddings
	if emb == nil {
		emb = [][]float64{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"model": alias, "embeddings": emb, "usage": resp.Usage})
}

// handleAIModels serves GET /ai/models.
func (s *Server) handleAIModels(w http.ResponseWriter, r *http.Request) {
	if !s.aiEnabled(w, r) {
		return
	}
	def, models := s.deps.AI.Models()
	if models == nil {
		models = []ai.ModelInfo{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"default": def, "models": models})
}
