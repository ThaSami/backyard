// Package openai implements ai.Provider for the OpenAI Chat Completions,
// Images and Embeddings APIs and for compatible servers: provider types
// "openai" (api.openai.com or any OpenAI-compatible base_url such as vLLM,
// LiteLLM, LM Studio or OpenRouter), "ollama" (OpenAI-compatible endpoint of a
// local Ollama) and "azure-openai" (Azure OpenAI / Azure AI Foundry, API key or
// Entra ID authentication).
//
// Cloudflare is supported through base_url: Workers AI's OpenAI-compatible
// endpoint (https://api.cloudflare.com/client/v4/accounts/<id>/ai/v1, API
// token as api_key) for chat and embeddings, with image generation routed to
// the native Workers AI run endpoint (…/ai/run/<model>, e.g.
// @cf/black-forest-labs/flux-1-schnell); and AI Gateway URLs
// (https://gateway.ai.cloudflare.com/v1/<account>/<gateway>/openai,
// …/workers-ai/v1 or …/compat), with gateway auth via headers
// (cf-aig-authorization).
//
// Image URLs in chat messages are passed through to OpenAI and Azure OpenAI,
// which download them from the public internet. Every other upstream (Ollama,
// vLLM, LiteLLM, LM Studio, Workers AI, …) gets the image inline as a data:
// URL, downloaded by Backyard's SSRF-guarded fetcher: many of these servers
// cannot fetch URLs at all, and a self-hosted server that can would do so
// from inside its own network.
//
// It uses plain net/http and encoding/json. Differences between compatible
// servers are handled adaptively: when an upstream rejects max_tokens vs
// max_completion_tokens, temperature, stream_options or response_format with
// a 400, the request is retried once with the parameter adjusted and the
// adjustment is remembered for that model.
package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"

	"github.com/thasami/backyard/internal/ai"
	"github.com/thasami/backyard/internal/ai/internal/httpx"
	"github.com/thasami/backyard/internal/ai/internal/imagefetch"
	"github.com/thasami/backyard/internal/ai/internal/sse"
	"github.com/thasami/backyard/internal/config"
)

// Provider types handled by this package.
const (
	TypeOpenAI      = "openai"
	TypeOllama      = "ollama"
	TypeAzureOpenAI = "azure-openai"
)

const (
	defaultOpenAIBaseURL = "https://api.openai.com/v1"
	defaultOllamaBaseURL = "http://localhost:11434/v1"
	defaultAzureVersion  = "2024-10-21"
	azureScope           = "https://cognitiveservices.azure.com/.default"
)

// Options are optional dependencies.
type Options struct {
	// HTTPClient performs upstream requests (default: httpx.DefaultClient()).
	HTTPClient *http.Client
	// Logger receives debug logs (default: slog.Default()).
	Logger *slog.Logger
	// FetchImage downloads URL images for upstreams that only accept inline
	// images (ollama). Default: an SSRF-guarded fetcher.
	FetchImage imagefetch.Func
	// AzureCredential overrides the Entra ID credential used when
	// use_azure_ad is set (default: azidentity.NewDefaultAzureCredential).
	AzureCredential azcore.TokenCredential
}

// Provider is an ai.Provider for OpenAI-compatible APIs. It is safe for
// concurrent use.
type Provider struct {
	name       string
	typ        string
	baseURL    string // openai/ollama: API base incl. /v1; azure: resource endpoint
	apiKey     string
	apiVersion string
	headers    map[string]string
	org        string
	project    string
	client     *http.Client
	log        *slog.Logger
	fetch      imagefetch.Func
	tokens     *tokenCache // azure-openai with use_azure_ad
	cloudflare bool        // Workers AI or AI Gateway base URL
	official   bool        // api.openai.com, directly or through AI Gateway
	runBase    string      // Workers AI native run endpoint (image models), "" otherwise

	quirks     sync.Map     // model id -> quirks
	quirkCount atomic.Int64 // entries in quirks (bounded by maxQuirkModels)
}

// maxQuirkModels bounds the per-model parameter adjustments remembered by a
// provider (raw model ids are client-chosen, so the set is open-ended).
const maxQuirkModels = 512

var _ ai.Provider = (*Provider)(nil)

// New creates a provider from its config. pc.Type must be openai, ollama or
// azure-openai (empty means openai).
func New(name string, pc config.ProviderConfig, opts Options) (*Provider, error) {
	typ := pc.Type
	if typ == "" {
		typ = TypeOpenAI
	}
	p := &Provider{
		name:       name,
		typ:        typ,
		apiKey:     pc.APIKey,
		apiVersion: pc.APIVersion,
		headers:    pc.Headers,
		org:        pc.Organization,
		project:    pc.OpenAIProject,
		client:     opts.HTTPClient,
		log:        opts.Logger,
		fetch:      opts.FetchImage,
	}
	if p.client == nil {
		p.client = httpx.DefaultClient()
	}
	if p.log == nil {
		p.log = slog.Default()
	}
	if p.fetch == nil {
		p.fetch = imagefetch.New().Fetch
	}
	var base string
	switch typ {
	case TypeOpenAI:
		base = firstNonEmpty(pc.BaseURL, defaultOpenAIBaseURL)
	case TypeOllama:
		base = firstNonEmpty(pc.BaseURL, defaultOllamaBaseURL)
	case TypeAzureOpenAI:
		base = firstNonEmpty(pc.Endpoint, pc.BaseURL)
		if base == "" {
			return nil, fmt.Errorf("ai: provider %s: endpoint is required for azure-openai", name)
		}
		if p.apiVersion == "" {
			p.apiVersion = defaultAzureVersion
		}
		if pc.UseAzureAD {
			p.tokens = &tokenCache{cred: opts.AzureCredential}
		} else if pc.APIKey == "" {
			return nil, fmt.Errorf("ai: provider %s: azure-openai needs api_key or use_azure_ad", name)
		}
	default:
		return nil, fmt.Errorf("ai: provider %s: unsupported type %q for the openai provider", name, typ)
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("ai: provider %s: invalid base URL %q", name, base)
	}
	p.baseURL = strings.TrimRight(base, "/")
	host := strings.ToLower(u.Hostname())
	p.cloudflare = host == cfAPIHost || host == cfGatewayHost
	p.official = typ == TypeOpenAI && (host == "api.openai.com" ||
		(host == cfGatewayHost && strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/openai")))
	p.runBase = workersAIRunBase(u)
	return p, nil
}

// Name implements ai.Provider.
func (p *Provider) Name() string { return p.name }

// ---------------------------------------------------------------------------
// Wire types

type chatRequest struct {
	Model               string          `json:"model,omitempty"`
	Messages            []chatMessage   `json:"messages"`
	MaxTokens           *int            `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int            `json:"max_completion_tokens,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	ResponseFormat      *responseFormat `json:"response_format,omitempty"`
	Stream              bool            `json:"stream,omitempty"`
	StreamOptions       *streamOptions  `json:"stream_options,omitempty"`
	User                string          `json:"user,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string, or []contentPart when images are present
}

type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

type responseFormat struct {
	Type string `json:"type"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

func (u *usage) toAI() ai.Usage {
	if u == nil {
		return ai.Usage{}
	}
	return ai.Usage{InputTokens: u.PromptTokens, OutputTokens: u.CompletionTokens}
}

type chatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content json.RawMessage `json:"content"`
			Refusal *string         `json:"refusal"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *usage `json:"usage"`
}

type chatChunk struct {
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content json.RawMessage `json:"content"`
			Refusal *string         `json:"refusal"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *usage          `json:"usage"`
	Error json.RawMessage `json:"error"`
}

// ---------------------------------------------------------------------------
// Adaptive request parameters

const (
	fieldMaxTokens           = "max_tokens"
	fieldMaxCompletionTokens = "max_completion_tokens"
)

// quirks records which optional parameters a model accepts.
type quirks struct {
	maxField         string
	noTemperature    bool
	noStreamOptions  bool
	noResponseFormat bool
}

func (p *Provider) quirksFor(model string) quirks {
	if v, ok := p.quirks.Load(model); ok {
		return v.(quirks)
	}
	return quirks{maxField: p.defaultMaxField()}
}

// defaultMaxField picks the max-tokens parameter name: OpenAI itself and
// Azure OpenAI use max_completion_tokens (required by reasoning models);
// other compatible servers generally only know max_tokens.
func (p *Provider) defaultMaxField() string {
	if p.typ == TypeAzureOpenAI || p.official {
		return fieldMaxCompletionTokens
	}
	return fieldMaxTokens
}

// rememberQuirks stores the adjusted parameters for model, up to
// maxQuirkModels distinct models.
func (p *Provider) rememberQuirks(model string, q quirks) {
	if _, ok := p.quirks.Load(model); ok {
		p.quirks.Store(model, q)
		return
	}
	if p.quirkCount.Add(1) > maxQuirkModels {
		p.quirkCount.Add(-1)
		return
	}
	if _, loaded := p.quirks.LoadOrStore(model, q); loaded {
		p.quirkCount.Add(-1)
		p.quirks.Store(model, q)
	}
}

// fetchesImageURLs reports whether the upstream downloads image URLs itself
// (OpenAI and Azure OpenAI, from the public internet).
func (p *Provider) fetchesImageURLs() bool {
	return p.typ == TypeAzureOpenAI || p.official
}

// sendUser reports whether the end-user id is forwarded ("user" field).
// Ollama and Cloudflare endpoints do not use it.
func (p *Provider) sendUser() bool {
	return p.typ != TypeOllama && !p.cloudflare
}

var unsupportedWords = []string{"unsupported", "not supported", "unrecognized", "unknown", "extra inputs",
	"not allowed", "not permitted", "does not support", "only the default", "invalid parameter"}

// adjust returns quirks that avoid the parameter rejected by a 400 error, or
// false when the error is not about an optional parameter (or the parameter
// was already adjusted during this call).
func adjust(q quirks, msg string, used map[string]bool) (quirks, string, bool) {
	m := strings.ToLower(msg)
	unsupported := false
	for _, w := range unsupportedWords {
		if strings.Contains(m, w) {
			unsupported = true
			break
		}
	}
	if !unsupported {
		return q, "", false
	}
	switch {
	case strings.Contains(m, "stream_options") && !q.noStreamOptions && !used["stream_options"]:
		q.noStreamOptions = true
		used["stream_options"] = true
		return q, "stream_options", true
	case strings.Contains(m, "response_format") && !q.noResponseFormat && !used["response_format"]:
		q.noResponseFormat = true
		used["response_format"] = true
		return q, "response_format", true
	case strings.Contains(m, "temperature") && !q.noTemperature && !used["temperature"]:
		q.noTemperature = true
		used["temperature"] = true
		return q, "temperature", true
	case !used["max"] && q.maxField == fieldMaxCompletionTokens && strings.Contains(m, fieldMaxCompletionTokens):
		q.maxField = fieldMaxTokens
		used["max"] = true
		return q, fieldMaxCompletionTokens, true
	case !used["max"] && q.maxField == fieldMaxTokens && strings.Contains(m, fieldMaxTokens):
		q.maxField = fieldMaxCompletionTokens
		used["max"] = true
		return q, fieldMaxTokens, true
	}
	return q, "", false
}

// ---------------------------------------------------------------------------
// Chat

// Chat implements ai.Provider.
func (p *Provider) Chat(ctx context.Context, req ai.ChatRequest) (*ai.ChatResponse, error) {
	return p.chat(ctx, req, nil)
}

// ChatStream implements ai.Provider.
func (p *Provider) ChatStream(ctx context.Context, req ai.ChatRequest, onDelta func(string) error) (*ai.ChatResponse, error) {
	if onDelta == nil {
		onDelta = func(string) error { return nil }
	}
	return p.chat(ctx, req, onDelta)
}

func (p *Provider) chat(ctx context.Context, req ai.ChatRequest, onDelta func(string) error) (*ai.ChatResponse, error) {
	if req.Model == "" {
		return nil, fmt.Errorf("%w: model is required", ai.ErrBadRequest)
	}
	msgs, err := p.convertMessages(ctx, req)
	if err != nil {
		return nil, err
	}
	stream := onDelta != nil
	q := p.quirksFor(req.Model)
	used := map[string]bool{}
	for attempt := 0; ; attempt++ {
		body := p.buildChat(req, msgs, q, stream)
		resp, err := p.post(ctx, p.endpoint("chat/completions", req.Model), body)
		if err != nil {
			var ue *ai.UpstreamError
			if attempt < 4 && errors.As(err, &ue) && (ue.Status == http.StatusBadRequest || ue.Status == http.StatusUnprocessableEntity) {
				if nq, param, ok := adjust(q, ue.Message, used); ok {
					p.log.LogAttrs(ctx, slog.LevelDebug, "ai: retrying without unsupported parameter",
						slog.String("provider", p.name), slog.String("model", req.Model), slog.String("param", param))
					q = nq
					p.rememberQuirks(req.Model, q)
					continue
				}
			}
			return nil, err
		}
		var out *ai.ChatResponse
		if stream {
			out, err = p.readStream(ctx, resp, onDelta)
		} else {
			out, err = p.readChat(resp)
		}
		if err != nil {
			return nil, err
		}
		if out.Model == "" {
			out.Model = req.Model
		}
		if req.JSON {
			out.Content = ai.StripCodeFences(out.Content)
		}
		return out, nil
	}
}

func (p *Provider) buildChat(req ai.ChatRequest, msgs []chatMessage, q quirks, stream bool) chatRequest {
	body := chatRequest{Messages: msgs, Stream: stream}
	if !p.azureDeployments() {
		body.Model = req.Model
	}
	if req.MaxTokens > 0 {
		n := req.MaxTokens
		if q.maxField == fieldMaxCompletionTokens {
			body.MaxCompletionTokens = &n
		} else {
			body.MaxTokens = &n
		}
	}
	if req.Temperature != nil && !q.noTemperature {
		t := *req.Temperature
		body.Temperature = &t
	}
	if req.JSON && !q.noResponseFormat {
		body.ResponseFormat = &responseFormat{Type: "json_object"}
	}
	if stream && !q.noStreamOptions {
		body.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	if req.User != "" && p.sendUser() {
		body.User = req.User
	}
	return body
}

func (p *Provider) convertMessages(ctx context.Context, req ai.ChatRequest) ([]chatMessage, error) {
	out := make([]chatMessage, 0, len(req.Messages)+1)
	system := req.System
	if req.JSON {
		system = ai.SystemWithJSON(system)
	}
	if system != "" {
		out = append(out, chatMessage{Role: "system", Content: system})
	}
	for _, m := range req.Messages {
		if m.Role != "user" && m.Role != "assistant" {
			return nil, fmt.Errorf("%w: unsupported role %q", ai.ErrBadRequest, m.Role)
		}
		hasImage := false
		for _, part := range m.Content {
			if part.Type == "image" {
				hasImage = true
			}
		}
		if !hasImage {
			out = append(out, chatMessage{Role: m.Role, Content: ai.TextOf(m.Content)})
			continue
		}
		if m.Role != "user" {
			return nil, fmt.Errorf("%w: images are only supported in user messages", ai.ErrBadRequest)
		}
		parts := make([]contentPart, 0, len(m.Content))
		for _, part := range m.Content {
			switch part.Type {
			case "text":
				parts = append(parts, contentPart{Type: "text", Text: part.Text})
			case "image":
				u, err := p.imageURL(ctx, part)
				if err != nil {
					return nil, err
				}
				parts = append(parts, contentPart{Type: "image_url", ImageURL: &imageURL{URL: u}})
			default:
				return nil, fmt.Errorf("%w: unsupported content type %q", ai.ErrBadRequest, part.Type)
			}
		}
		out = append(out, chatMessage{Role: m.Role, Content: parts})
	}
	return out, nil
}

// imageURL returns the image_url value for a part: a data: URL for inline
// bytes, or the remote URL for upstreams that fetch images themselves
// (others get it downloaded with the SSRF-guarded fetcher and inlined).
func (p *Provider) imageURL(ctx context.Context, part ai.Part) (string, error) {
	if len(part.Data) > 0 {
		return dataURL(part.MediaType, part.Data), nil
	}
	if part.URL == "" {
		return "", fmt.Errorf("%w: image part needs data or url", ai.ErrBadRequest)
	}
	if p.fetchesImageURLs() || strings.HasPrefix(part.URL, "data:") {
		return part.URL, nil
	}
	data, mt, err := p.fetch(ctx, part.URL)
	if err != nil {
		return "", imagefetch.RequestError(ctx, ai.ErrBadRequest, err)
	}
	return dataURL(mt, data), nil
}

func dataURL(mediaType string, data []byte) string {
	if mediaType == "" {
		mediaType = http.DetectContentType(data)
	}
	return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data)
}

func (p *Provider) readChat(resp *http.Response) (*ai.ChatResponse, error) {
	var cr chatResponse
	if err := httpx.DecodeJSON(p.name, resp, &cr); err != nil {
		return nil, err
	}
	if len(cr.Choices) == 0 {
		return nil, &ai.UpstreamError{Provider: p.name, Message: "response has no choices"}
	}
	ch := cr.Choices[0]
	out := &ai.ChatResponse{
		Content:    contentText(ch.Message.Content),
		Model:      cr.Model,
		StopReason: mapFinishReason(ch.FinishReason),
		Usage:      cr.Usage.toAI(),
	}
	if ch.Message.Refusal != nil && *ch.Message.Refusal != "" {
		if out.Content == "" {
			out.Content = *ch.Message.Refusal
		}
		out.StopReason = ai.StopRefusal
	}
	return out, nil
}

func (p *Provider) readStream(ctx context.Context, resp *http.Response, onDelta func(string) error) (*ai.ChatResponse, error) {
	defer resp.Body.Close()
	var (
		content  strings.Builder
		out      = &ai.ChatResponse{StopReason: ai.StopOther}
		finished bool
		refused  bool
	)
	r := sse.NewReader(resp.Body)
	for {
		ev, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, fmt.Errorf("ai: %s: %w", p.name, ctxErr)
			}
			return nil, &ai.UpstreamError{Provider: p.name, Message: "stream: " + err.Error()}
		}
		data := strings.TrimSpace(ev.Data)
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			finished = true
			break
		}
		var chunk chatChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return nil, &ai.UpstreamError{Provider: p.name, Message: "invalid stream chunk: " + err.Error()}
		}
		if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
			return nil, &ai.UpstreamError{Provider: p.name, Message: httpx.ErrorMessage([]byte(`{"error":` + string(chunk.Error) + `}`))}
		}
		if chunk.Model != "" {
			out.Model = chunk.Model
		}
		if chunk.Usage != nil {
			out.Usage = chunk.Usage.toAI()
		}
		for i, ch := range chunk.Choices {
			if i > 0 {
				break // n=1 only
			}
			text := contentText(ch.Delta.Content)
			if ch.Delta.Refusal != nil && *ch.Delta.Refusal != "" {
				text += *ch.Delta.Refusal
				refused = true
			}
			if text != "" {
				content.WriteString(text)
				if err := onDelta(text); err != nil {
					return nil, err
				}
			}
			if ch.FinishReason != nil && *ch.FinishReason != "" {
				out.StopReason = mapFinishReason(*ch.FinishReason)
				finished = true
			}
		}
	}
	if !finished {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("ai: %s: %w", p.name, ctxErr)
		}
		return nil, &ai.UpstreamError{Provider: p.name, Message: "stream ended unexpectedly"}
	}
	if refused {
		out.StopReason = ai.StopRefusal
	}
	out.Content = content.String()
	return out, nil
}

// contentText extracts text from a message content value, which is a string,
// null, or (for some compatible servers) an array of content parts.
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, part := range parts {
			if part.Type == "text" || part.Type == "output_text" {
				b.WriteString(part.Text)
			}
		}
		return b.String()
	}
	return ""
}

func mapFinishReason(r string) string {
	switch r {
	case "stop", "end_turn", "eos":
		return ai.StopEndTurn
	case "length", "max_tokens":
		return ai.StopMaxTokens
	case "content_filter", "refusal":
		return ai.StopRefusal
	case "stop_sequence":
		return ai.StopStopSequence
	default:
		return ai.StopOther
	}
}

// ---------------------------------------------------------------------------
// Images

type imagesRequest struct {
	Model          string `json:"model,omitempty"`
	Prompt         string `json:"prompt"`
	N              int    `json:"n,omitempty"`
	Size           string `json:"size,omitempty"`
	ResponseFormat string `json:"response_format,omitempty"`
	User           string `json:"user,omitempty"`
}

type imagesResponse struct {
	Data []struct {
		B64JSON string `json:"b64_json"`
		URL     string `json:"url"`
	} `json:"data"`
	OutputFormat string `json:"output_format"`
}

// Image implements ai.Provider via POST /images/generations.
func (p *Provider) Image(ctx context.Context, req ai.ImageRequest) (*ai.ImageResponse, error) {
	if req.Model == "" || strings.TrimSpace(req.Prompt) == "" {
		return nil, fmt.Errorf("%w: model and prompt are required", ai.ErrBadRequest)
	}
	n := max(req.N, 1)
	if p.runBase != "" {
		return p.workersAIImage(ctx, req, n)
	}
	model := strings.ToLower(req.Model)
	perCall := n
	if strings.HasPrefix(model, "dall-e-3") {
		perCall = 1 // dall-e-3 only supports n=1
	}
	out := &ai.ImageResponse{}
	for len(out.Images) < n {
		body := imagesRequest{Prompt: req.Prompt, N: min(perCall, n-len(out.Images)), Size: req.Size}
		if !p.azureDeployments() {
			body.Model = req.Model
		}
		if strings.HasPrefix(model, "dall-e") {
			body.ResponseFormat = "b64_json" // gpt-image-* always return base64
		}
		if req.User != "" && p.sendUser() {
			body.User = req.User
		}
		resp, err := p.post(ctx, p.endpoint("images/generations", req.Model), body)
		if err != nil {
			return nil, err
		}
		var ir imagesResponse
		if err := httpx.DecodeJSON(p.name, resp, &ir); err != nil {
			return nil, err
		}
		if len(ir.Data) == 0 {
			return nil, &ai.UpstreamError{Provider: p.name, Message: "no images returned"}
		}
		for _, d := range ir.Data {
			img, err := p.decodeImage(ctx, d.B64JSON, d.URL, ir.OutputFormat)
			if err != nil {
				return nil, err
			}
			out.Images = append(out.Images, img)
		}
	}
	if len(out.Images) > n {
		out.Images = out.Images[:n]
	}
	return out, nil
}

// Cloudflare hosts.
const (
	cfAPIHost     = "api.cloudflare.com"
	cfGatewayHost = "gateway.ai.cloudflare.com"
)

// workersAIRunBase derives the native Workers AI run endpoint from an
// OpenAI-compatible Workers AI base URL:
//
//	https://api.cloudflare.com/client/v4/accounts/<id>/ai/v1      → …/ai/run/
//	https://gateway.ai.cloudflare.com/v1/<acct>/<gw>/workers-ai/v1 → …/workers-ai/
func workersAIRunBase(u *url.URL) string {
	host := strings.ToLower(u.Hostname())
	path := strings.TrimRight(u.Path, "/")
	root := u.Scheme + "://" + u.Host
	switch {
	case host == cfAPIHost && strings.HasSuffix(path, "/ai/v1"):
		return root + strings.TrimSuffix(path, "/v1") + "/run/"
	case host == cfGatewayHost && strings.HasSuffix(path, "/workers-ai/v1"):
		return root + strings.TrimSuffix(path, "/v1") + "/"
	}
	return ""
}

// workersAIImage generates images with a Workers AI text-to-image model via
// POST <run base>/<model> {"prompt": …}. The response is either raw image
// bytes or a JSON envelope {"result": {"image": "<base64>"}}.
func (p *Provider) workersAIImage(ctx context.Context, req ai.ImageRequest, n int) (*ai.ImageResponse, error) {
	segs := strings.Split(strings.Trim(req.Model, "/"), "/")
	for i, seg := range segs {
		if seg == "" || seg == "." || seg == ".." {
			return nil, fmt.Errorf("%w: invalid model id %q", ai.ErrBadRequest, req.Model)
		}
		segs[i] = url.PathEscape(seg)
	}
	endpoint := p.runBase + strings.Join(segs, "/")
	body := map[string]any{"prompt": req.Prompt}
	if strings.Contains(strings.ToLower(req.Model), "stable-diffusion") {
		if w, h, ok := parseWxH(req.Size); ok {
			body["width"], body["height"] = w, h
		}
	}
	out := &ai.ImageResponse{}
	for range n {
		resp, err := p.post(ctx, endpoint, body)
		if err != nil {
			return nil, err
		}
		img, err := p.readWorkersAIImage(resp)
		if err != nil {
			return nil, err
		}
		out.Images = append(out.Images, img)
	}
	return out, nil
}

func (p *Provider) readWorkersAIImage(resp *http.Response) (ai.Image, error) {
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, httpx.MaxResponseBytes+1))
	if err != nil {
		return ai.Image{}, &ai.UpstreamError{Provider: p.name, Message: "read image: " + err.Error()}
	}
	if len(data) > httpx.MaxResponseBytes {
		return ai.Image{}, &ai.UpstreamError{Provider: p.name, Message: "image response too large"}
	}
	if mt := sniffImageType(data); mt != "" {
		return ai.Image{Data: data, MediaType: mt}, nil
	}
	var env struct {
		Result struct {
			Image string `json:"image"`
		} `json:"result"`
		Image string `json:"image"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return ai.Image{}, &ai.UpstreamError{Provider: p.name, Message: "unexpected image response"}
	}
	return p.decodeImage(context.Background(), firstNonEmpty(env.Result.Image, env.Image), "", "")
}

// parseWxH parses "1024x768".
func parseWxH(size string) (int, int, bool) {
	ws, hs, ok := strings.Cut(strings.ToLower(strings.TrimSpace(size)), "x")
	if !ok {
		return 0, 0, false
	}
	w, err1 := strconv.Atoi(strings.TrimSpace(ws))
	h, err2 := strconv.Atoi(strings.TrimSpace(hs))
	if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return 0, 0, false
	}
	return w, h, true
}

func (p *Provider) decodeImage(ctx context.Context, b64, rawURL, format string) (ai.Image, error) {
	var data []byte
	switch {
	case b64 != "":
		var err error
		data, err = base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return ai.Image{}, &ai.UpstreamError{Provider: p.name, Message: "invalid base64 image"}
		}
	case rawURL != "":
		// The URL comes from the configured upstream, not from a user.
		d, mt, err := imagefetch.Get(ctx, p.client, rawURL)
		if err != nil {
			return ai.Image{}, &ai.UpstreamError{Provider: p.name, Message: err.Error()}
		}
		return ai.Image{Data: d, MediaType: mt}, nil
	default:
		return ai.Image{}, &ai.UpstreamError{Provider: p.name, Message: "image has neither b64_json nor url"}
	}
	mt := sniffImageType(data)
	if mt == "" {
		mt = map[string]string{"png": "image/png", "jpeg": "image/jpeg", "webp": "image/webp"}[format]
	}
	if mt == "" {
		mt = "image/png"
	}
	return ai.Image{Data: data, MediaType: mt}, nil
}

func sniffImageType(data []byte) string {
	mt := http.DetectContentType(data)
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = mt[:i]
	}
	if strings.HasPrefix(mt, "image/") {
		return mt
	}
	return ""
}

// ---------------------------------------------------------------------------
// Embeddings

type embedRequest struct {
	Model          string   `json:"model,omitempty"`
	Input          []string `json:"input"`
	EncodingFormat string   `json:"encoding_format,omitempty"`
	User           string   `json:"user,omitempty"`
}

type embedResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float64 `json:"embedding"`
	} `json:"data"`
	Usage *usage `json:"usage"`
}

// Embed implements ai.Provider via POST /embeddings.
func (p *Provider) Embed(ctx context.Context, req ai.EmbedRequest) (*ai.EmbedResponse, error) {
	if req.Model == "" || len(req.Input) == 0 {
		return nil, fmt.Errorf("%w: model and input are required", ai.ErrBadRequest)
	}
	body := embedRequest{Input: req.Input, EncodingFormat: "float"}
	if !p.azureDeployments() {
		body.Model = req.Model
	}
	if req.User != "" && p.sendUser() {
		body.User = req.User
	}
	resp, err := p.post(ctx, p.endpoint("embeddings", req.Model), body)
	if err != nil {
		return nil, err
	}
	var er embedResponse
	if err := httpx.DecodeJSON(p.name, resp, &er); err != nil {
		return nil, err
	}
	if len(er.Data) != len(req.Input) {
		return nil, &ai.UpstreamError{Provider: p.name,
			Message: fmt.Sprintf("got %d embeddings for %d inputs", len(er.Data), len(req.Input))}
	}
	sort.SliceStable(er.Data, func(i, j int) bool { return er.Data[i].Index < er.Data[j].Index })
	out := &ai.EmbedResponse{Embeddings: make([][]float64, len(er.Data))}
	for i, d := range er.Data {
		out.Embeddings[i] = d.Embedding
	}
	if er.Usage != nil {
		out.Usage = ai.Usage{InputTokens: er.Usage.PromptTokens}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Transport

// azureDeployments reports whether requests address an Azure deployment by
// URL path (classic API) rather than by "model" in the body (v1 API).
func (p *Provider) azureDeployments() bool {
	return p.typ == TypeAzureOpenAI && !p.azureV1()
}

func (p *Provider) azureV1() bool {
	return p.apiVersion == "v1" || p.apiVersion == "preview"
}

// endpoint returns the URL for an operation ("chat/completions",
// "images/generations", "embeddings").
func (p *Provider) endpoint(op, model string) string {
	if p.typ != TypeAzureOpenAI {
		return p.baseURL + "/" + op
	}
	// Accept the resource endpoint with or without an /openai or /openai/v1
	// suffix (both appear in the Azure portal).
	base := p.baseURL
	if b, ok := strings.CutSuffix(base, "/openai/v1"); ok {
		base = b
	} else {
		base = strings.TrimSuffix(base, "/openai")
	}
	if p.azureV1() {
		u := base + "/openai/v1/" + op
		if p.apiVersion == "preview" {
			u += "?api-version=preview"
		}
		return u
	}
	return base + "/openai/deployments/" + url.PathEscape(model) + "/" + op +
		"?api-version=" + url.QueryEscape(p.apiVersion)
}

func (p *Provider) post(ctx context.Context, endpoint string, body any) (*http.Response, error) {
	req, err := httpx.NewJSONRequest(ctx, endpoint, body)
	if err != nil {
		return nil, err
	}
	if err := p.authorize(ctx, req); err != nil {
		return nil, err
	}
	if b, ok := body.(chatRequest); ok && b.Stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	return httpx.Do(p.client, p.name, req)
}

func (p *Provider) authorize(ctx context.Context, req *http.Request) error {
	switch p.typ {
	case TypeAzureOpenAI:
		if p.tokens != nil {
			tok, err := p.tokens.token(ctx)
			if err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return fmt.Errorf("ai: %s: %w", p.name, ctxErr)
				}
				// Credential-chain errors are long and describe the host's
				// identity setup; keep them in the server log.
				p.log.LogAttrs(ctx, slog.LevelError, "ai: cannot get Entra ID token for Azure OpenAI",
					slog.String("provider", p.name), slog.Any("error", err))
				return &ai.UpstreamError{Provider: p.name, Status: http.StatusUnauthorized,
					Message: "Entra ID authentication failed (see server logs)"}
			}
			req.Header.Set("Authorization", "Bearer "+tok)
		} else {
			req.Header.Set("api-key", p.apiKey)
		}
	default:
		if p.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+p.apiKey)
		}
		if p.org != "" {
			req.Header.Set("OpenAI-Organization", p.org)
		}
		if p.project != "" {
			req.Header.Set("OpenAI-Project", p.project)
		}
	}
	for k, v := range p.headers {
		req.Header.Set(k, v)
	}
	return nil
}

// tokenCache caches Entra ID access tokens for Azure OpenAI.
type tokenCache struct {
	mu   sync.Mutex
	cred azcore.TokenCredential
	tok  azcore.AccessToken
}

func (c *tokenCache) token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tok.Token != "" && time.Until(c.tok.ExpiresOn) > 2*time.Minute {
		return c.tok.Token, nil
	}
	if c.cred == nil {
		cred, err := azidentity.NewDefaultAzureCredential(nil)
		if err != nil {
			return "", fmt.Errorf("default Azure credential: %w", err)
		}
		c.cred = cred
	}
	tok, err := c.cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{azureScope}})
	if err != nil {
		return "", err
	}
	c.tok = tok
	return tok.Token, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
