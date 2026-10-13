// Package anthropic implements ai.Provider for Claude using the official
// Anthropic Go SDK (github.com/anthropics/anthropic-sdk-go).
//
// Three platforms are supported (config.ProviderConfig.Platform):
//
//   - "anthropic" (default): the first-party Claude API. api_key, base_url
//     and extra headers apply; when api_key is empty the SDK's environment
//     defaults (ANTHROPIC_API_KEY, `ant auth login` profiles, workload
//     identity federation) are used.
//   - "vertex": Claude on Google Cloud Vertex AI with Application Default
//     Credentials; project_id (or the ADC project) and location (default
//     "global").
//   - "bedrock": Claude on Amazon Bedrock through the Bedrock Mantle Messages
//     endpoint; region (or AWS_REGION), AWS default credentials or an api_key
//     (Bedrock API key). Model ids are passed through as configured
//     (e.g. "anthropic.claude-opus-5-5"). Use the separate "bedrock" provider
//     type for the Converse API.
//
// Request rules follow the current Claude API: temperature is only sent to
// models that accept sampling parameters (Claude 3.x and 4.0–4.6), thinking
// is left at the model default (adaptive on current models; budget_tokens is
// never sent), output_config.effort is sent when the request, alias or
// provider config sets one, and on the first-party API server-side refusal
// fallbacks ("fallbacks": "default") are enabled by default for the models
// that support them. JSON mode appends a system instruction and strips code
// fences from the answer.
package anthropic

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	sdk "github.com/anthropics/anthropic-sdk-go"
	sdkbedrock "github.com/anthropics/anthropic-sdk-go/bedrock"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/vertex"
	"golang.org/x/oauth2/google"

	"github.com/thasami/backyard/internal/ai"
	"github.com/thasami/backyard/internal/ai/internal/claudemodel"
	"github.com/thasami/backyard/internal/ai/internal/httpx"
	"github.com/thasami/backyard/internal/ai/internal/imagefetch"
	"github.com/thasami/backyard/internal/config"
)

// Platforms.
const (
	PlatformAnthropic = "anthropic"
	PlatformVertex    = "vertex"
	PlatformBedrock   = "bedrock"
)

const (
	cloudPlatformScope   = "https://www.googleapis.com/auth/cloud-platform"
	defaultVertexRegion  = "global"
	defaultMaxTokens     = 4096
	defaultRequestBudget = 10 * time.Minute
)

// Options are optional dependencies.
type Options struct {
	// HTTPClient performs upstream requests (default: the SDK's client).
	HTTPClient *http.Client
	// Logger receives debug/info logs (default: slog.Default()).
	Logger *slog.Logger
	// GoogleCredentials overrides Application Default Credentials for the
	// vertex platform.
	GoogleCredentials *google.Credentials
	// FetchImage downloads URL images for platforms without URL image
	// sources (vertex, bedrock). Default: an SSRF-guarded fetcher.
	FetchImage imagefetch.Func
	// MaxRetries overrides the SDK's retry count (default 2) when non-nil.
	MaxRetries *int
}

// Provider is an ai.Provider for Claude. It is safe for concurrent use.
type Provider struct {
	name      string
	platform  string
	effort    string
	fallbacks bool
	log       *slog.Logger
	fetch     imagefetch.Func

	build func(ctx context.Context) (*sdk.BetaMessageService, error)
	mu    sync.Mutex
	svc   *sdk.BetaMessageService
}

var _ ai.Provider = (*Provider)(nil)

// New creates a Claude provider. The first-party client is built eagerly;
// Vertex and Bedrock clients are built on first use (credential discovery
// may need network access) and retried on later requests if that fails.
func New(ctx context.Context, name string, pc config.ProviderConfig, opts Options) (*Provider, error) {
	platform := pc.Platform
	if platform == "" {
		platform = PlatformAnthropic
	}
	if pc.Effort != "" && !ai.ValidEffort(pc.Effort) {
		return nil, fmt.Errorf("ai: provider %s: invalid effort %q (want low, medium, high, xhigh or max)", name, pc.Effort)
	}
	if platform == PlatformVertex && pc.Location != "" && !locationRE.MatchString(pc.Location) {
		return nil, fmt.Errorf("ai: provider %s: invalid location %q", name, pc.Location)
	}
	p := &Provider{
		name:      name,
		platform:  platform,
		effort:    pc.Effort,
		fallbacks: platform == PlatformAnthropic && (pc.RefusalFallback == nil || *pc.RefusalFallback),
		log:       opts.Logger,
		fetch:     opts.FetchImage,
	}
	if p.log == nil {
		p.log = slog.Default()
	}

	var common []option.RequestOption
	if opts.HTTPClient != nil {
		common = append(common, option.WithHTTPClient(opts.HTTPClient))
	} else if platform == PlatformAnthropic && pc.APIKey != "" {
		// WithoutEnvironmentDefaults (below) would otherwise leave the SDK on
		// http.DefaultClient, which has no response-header timeout.
		common = append(common, option.WithHTTPClient(httpx.DefaultClient()))
	}
	for k, v := range pc.Headers {
		common = append(common, option.WithHeader(k, v))
	}
	if opts.MaxRetries != nil {
		common = append(common, option.WithMaxRetries(max(*opts.MaxRetries, 0)))
	}

	switch platform {
	case PlatformAnthropic:
		o := common
		if pc.APIKey != "" {
			o = append(o, option.WithoutEnvironmentDefaults(), option.WithAPIKey(pc.APIKey))
		}
		if pc.BaseURL != "" {
			o = append(o, option.WithBaseURL(pc.BaseURL))
		}
		client := sdk.NewClient(o...)
		p.svc = &client.Beta.Messages
	case PlatformVertex:
		creds := opts.GoogleCredentials
		region := pc.Location
		if region == "" {
			region = defaultVertexRegion
		}
		p.build = func(ctx context.Context) (*sdk.BetaMessageService, error) {
			c := creds
			if c == nil {
				var err error
				c, err = google.FindDefaultCredentials(ctx, cloudPlatformScope)
				if err != nil {
					return nil, fmt.Errorf("find Google application default credentials: %w", err)
				}
			}
			project := pc.ProjectID
			if project == "" {
				project = c.ProjectID
			}
			if project == "" {
				return nil, errors.New("project_id is required for the vertex platform (none found in credentials)")
			}
			vopt, err := vertexOption(ctx, region, project, c)
			if err != nil {
				return nil, err
			}
			// HTTPClient must precede the Vertex option so it is wrapped with
			// OAuth; a base_url override must follow it.
			o := append(append([]option.RequestOption{}, common...), vopt)
			if pc.BaseURL != "" {
				o = append(o, option.WithBaseURL(pc.BaseURL))
			}
			client := sdk.NewClient(o...)
			return &client.Beta.Messages, nil
		}
	case PlatformBedrock:
		p.build = func(ctx context.Context) (*sdk.BetaMessageService, error) {
			client, err := sdkbedrock.NewMantleClient(ctx, sdkbedrock.MantleClientConfig{
				APIKey:    pc.APIKey,
				AWSRegion: pc.Region,
				BaseURL:   pc.BaseURL,
			}, common...)
			if err != nil {
				return nil, fmt.Errorf("create Bedrock client: %w", err)
			}
			return &client.Beta.Messages, nil
		}
	default:
		return nil, fmt.Errorf("ai: provider %s: unknown platform %q (want anthropic, vertex or bedrock)", name, platform)
	}
	if platform != PlatformAnthropic && p.fetch == nil {
		p.fetch = imagefetch.New().Fetch
	}
	return p, nil
}

// locationRE matches Google Cloud locations ("global", "us-east5", "eu").
var locationRE = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// vertexModelRE matches Claude model ids on Vertex AI
// ("claude-opus-4-5@20251101"). The SDK's Vertex middleware interpolates the
// model id into the request path without escaping, so anything else (path
// separators, "..", query characters) is rejected.
var vertexModelRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]*$`)

// vertexOption wraps vertex.WithCredentials, which panics on transport
// setup failures, into an error-returning call.
func vertexOption(ctx context.Context, region, project string, creds *google.Credentials) (opt option.RequestOption, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("configure Vertex AI client: %v", r)
		}
	}()
	return vertex.WithCredentials(ctx, region, project, creds), nil
}

// Name implements ai.Provider.
func (p *Provider) Name() string { return p.name }

func (p *Provider) service(ctx context.Context) (*sdk.BetaMessageService, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.svc != nil {
		return p.svc, nil
	}
	// Credentials capture the context for later token refreshes, so it must
	// outlive this request.
	svc, err := p.build(context.WithoutCancel(ctx))
	if err != nil {
		p.log.LogAttrs(ctx, slog.LevelError, "ai: cannot initialise Claude client",
			slog.String("provider", p.name), slog.String("platform", p.platform), slog.Any("error", err))
		return nil, &ai.UpstreamError{Provider: p.name, Message: "client setup failed: " + httpx.ErrorText(err)}
	}
	p.svc = svc
	return svc, nil
}

// Chat implements ai.Provider.
func (p *Provider) Chat(ctx context.Context, req ai.ChatRequest) (*ai.ChatResponse, error) {
	svc, err := p.service(ctx)
	if err != nil {
		return nil, err
	}
	params, err := p.params(ctx, req)
	if err != nil {
		return nil, err
	}
	// The SDK refuses non-streaming requests with large max_tokens unless a
	// request timeout is given; the context deadline is the real bound.
	msg, err := svc.New(ctx, params, option.WithRequestTimeout(requestTimeout(ctx)))
	if err != nil {
		return nil, p.convertErr(ctx, err)
	}
	return p.response(msg, req), nil
}

// ChatStream implements ai.Provider using the streaming Messages API.
func (p *Provider) ChatStream(ctx context.Context, req ai.ChatRequest, onDelta func(string) error) (*ai.ChatResponse, error) {
	svc, err := p.service(ctx)
	if err != nil {
		return nil, err
	}
	params, err := p.params(ctx, req)
	if err != nil {
		return nil, err
	}
	stream := svc.NewStreaming(ctx, params)
	defer stream.Close()
	var (
		msg     sdk.BetaMessage
		stopped bool
	)
	for stream.Next() {
		ev := stream.Current()
		if err := msg.Accumulate(ev); err != nil {
			return nil, &ai.UpstreamError{Provider: p.name, Message: "invalid stream: " + err.Error()}
		}
		switch ev.Type {
		case "content_block_delta":
			if ev.Delta.Type == "text_delta" && ev.Delta.Text != "" && onDelta != nil {
				if err := onDelta(ev.Delta.Text); err != nil {
					return nil, err
				}
			}
		case "message_stop":
			stopped = true
		}
	}
	if err := stream.Err(); err != nil {
		return nil, p.convertErr(ctx, err)
	}
	if !stopped {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("ai: %s: %w", p.name, ctxErr)
		}
		return nil, &ai.UpstreamError{Provider: p.name, Message: "stream ended unexpectedly"}
	}
	return p.response(&msg, req), nil
}

// Image implements ai.Provider; Claude does not generate images.
func (p *Provider) Image(context.Context, ai.ImageRequest) (*ai.ImageResponse, error) {
	return nil, fmt.Errorf("%w: %s (Claude) cannot generate images", ai.ErrUnsupported, p.name)
}

// Embed implements ai.Provider; Claude has no embeddings endpoint.
func (p *Provider) Embed(context.Context, ai.EmbedRequest) (*ai.EmbedResponse, error) {
	return nil, fmt.Errorf("%w: %s (Claude) has no embeddings API", ai.ErrUnsupported, p.name)
}

// params maps a provider-neutral request onto Messages API parameters.
func (p *Provider) params(ctx context.Context, req ai.ChatRequest) (sdk.BetaMessageNewParams, error) {
	if req.Model == "" {
		return sdk.BetaMessageNewParams{}, fmt.Errorf("%w: model is required", ai.ErrBadRequest)
	}
	if p.platform == PlatformVertex && (!vertexModelRE.MatchString(req.Model) || strings.Contains(req.Model, "..")) {
		return sdk.BetaMessageNewParams{}, fmt.Errorf("%w: invalid model id %q for Vertex AI", ai.ErrBadRequest, req.Model)
	}
	msgs := make([]sdk.BetaMessageParam, 0, len(req.Messages))
	for i, m := range req.Messages {
		var role sdk.BetaMessageParamRole
		switch m.Role {
		case "user":
			role = sdk.BetaMessageParamRoleUser
		case "assistant":
			role = sdk.BetaMessageParamRoleAssistant
		default:
			return sdk.BetaMessageNewParams{}, fmt.Errorf("%w: message %d: unsupported role %q", ai.ErrBadRequest, i, m.Role)
		}
		blocks := make([]sdk.BetaContentBlockParamUnion, 0, len(m.Content))
		trim := -1 // text part to right-trim: a final assistant turn (prefill) may not end in whitespace
		if i == len(req.Messages)-1 && role == sdk.BetaMessageParamRoleAssistant {
			for j := len(m.Content) - 1; j >= 0 && m.Content[j].Type == "text"; j-- {
				if strings.TrimSpace(m.Content[j].Text) != "" {
					trim = j
					break
				}
			}
		}
		for j, part := range m.Content {
			switch part.Type {
			case "text":
				text := part.Text
				if j == trim {
					text = strings.TrimRightFunc(text, unicode.IsSpace)
				}
				// The API rejects text blocks that are empty or whitespace only.
				if strings.TrimSpace(text) != "" {
					blocks = append(blocks, sdk.NewBetaTextBlock(text))
				}
			case "image":
				b, err := p.imageBlock(ctx, part)
				if err != nil {
					return sdk.BetaMessageNewParams{}, err
				}
				blocks = append(blocks, b)
			default:
				return sdk.BetaMessageNewParams{}, fmt.Errorf("%w: unsupported content type %q", ai.ErrBadRequest, part.Type)
			}
		}
		if len(blocks) == 0 {
			return sdk.BetaMessageNewParams{}, fmt.Errorf("%w: message %d has no content", ai.ErrBadRequest, i)
		}
		msgs = append(msgs, sdk.BetaMessageParam{Role: role, Content: blocks})
	}
	if len(msgs) == 0 {
		return sdk.BetaMessageNewParams{}, fmt.Errorf("%w: no messages", ai.ErrBadRequest)
	}

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	params := sdk.BetaMessageNewParams{
		Model:     req.Model, // sdk.Model is an alias of string
		MaxTokens: int64(maxTokens),
		Messages:  msgs,
	}
	system := req.System
	if req.JSON {
		system = ai.SystemWithJSON(system)
	}
	if system != "" {
		params.System = []sdk.BetaTextBlockParam{{Text: system}}
	}
	if req.Temperature != nil && claudemodel.AcceptsSampling(req.Model) {
		params.Temperature = sdk.Float(min(max(*req.Temperature, 0), 1))
	}
	effort := req.Effort
	if effort == "" {
		effort = p.effort
	}
	if e := claudemodel.EffortFor(req.Model, effort); e != "" {
		params.OutputConfig = sdk.BetaOutputConfigParam{Effort: sdk.BetaOutputConfigEffort(e)}
		if claudemodel.NeedsEffortBeta(req.Model) {
			params.Betas = append(params.Betas, sdk.AnthropicBeta(claudemodel.EffortBeta))
		}
	}
	if p.fallbacks && claudemodel.SupportsDefaultFallbacks(req.Model) {
		params.Fallbacks = sdk.BetaFallbacksParamOfDefault()
		params.Betas = append(params.Betas, sdk.AnthropicBetaServerSideFallback2026_07_01)
	}
	if req.User != "" {
		params.Metadata = sdk.BetaMetadataParam{UserID: sdk.String(req.User)}
	}
	return params, nil
}

var imageMediaTypes = map[string]sdk.BetaBase64ImageSourceMediaType{
	"image/png":  sdk.BetaBase64ImageSourceMediaTypeImagePNG,
	"image/jpeg": sdk.BetaBase64ImageSourceMediaTypeImageJPEG,
	"image/jpg":  sdk.BetaBase64ImageSourceMediaTypeImageJPEG,
	"image/gif":  sdk.BetaBase64ImageSourceMediaTypeImageGIF,
	"image/webp": sdk.BetaBase64ImageSourceMediaTypeImageWebP,
}

func (p *Provider) imageBlock(ctx context.Context, part ai.Part) (sdk.BetaContentBlockParamUnion, error) {
	data, mt := part.Data, part.MediaType
	if len(data) == 0 {
		switch {
		case part.URL == "":
			return sdk.BetaContentBlockParamUnion{}, fmt.Errorf("%w: image part needs data or url", ai.ErrBadRequest)
		case strings.HasPrefix(part.URL, "data:"):
			d, m, ok := ai.ParseDataURL(part.URL)
			if !ok {
				return sdk.BetaContentBlockParamUnion{}, fmt.Errorf("%w: invalid image data URL", ai.ErrBadRequest)
			}
			data, mt = d, m
		case p.platform == PlatformAnthropic:
			return sdk.NewBetaImageBlock(sdk.BetaURLImageSourceParam{URL: part.URL}), nil
		default:
			d, m, err := p.fetch(ctx, part.URL)
			if err != nil {
				return sdk.BetaContentBlockParamUnion{}, imagefetch.RequestError(ctx, ai.ErrBadRequest, err)
			}
			data, mt = d, m
		}
	}
	if mt == "" {
		mt = http.DetectContentType(data)
		if i := strings.IndexByte(mt, ';'); i >= 0 {
			mt = mt[:i]
		}
	}
	smt, ok := imageMediaTypes[strings.ToLower(mt)]
	if !ok {
		return sdk.BetaContentBlockParamUnion{}, fmt.Errorf("%w: unsupported image type %q (png, jpeg, gif or webp)", ai.ErrBadRequest, mt)
	}
	return sdk.NewBetaImageBlock(sdk.BetaBase64ImageSourceParam{
		Data:      base64.StdEncoding.EncodeToString(data),
		MediaType: smt,
	}), nil
}

func (p *Provider) response(msg *sdk.BetaMessage, req ai.ChatRequest) *ai.ChatResponse {
	var b strings.Builder
	for _, blk := range msg.Content {
		if blk.Type == "text" {
			b.WriteString(blk.Text)
		}
	}
	content := b.String()
	if req.JSON {
		content = ai.StripCodeFences(content)
	}
	out := &ai.ChatResponse{
		Content:    content,
		Model:      msg.Model,
		StopReason: mapStopReason(msg.StopReason),
		Usage:      usageOf(msg.Usage),
	}
	if out.Model == "" {
		out.Model = req.Model
	}
	if out.StopReason == ai.StopRefusal {
		p.log.Info("ai: request declined by model",
			slog.String("provider", p.name), slog.String("model", out.Model),
			slog.String("category", string(msg.StopDetails.Category)))
	} else if out.Model != req.Model && !strings.Contains(out.Model, req.Model) {
		p.log.Debug("ai: response served by fallback model",
			slog.String("provider", p.name), slog.String("requested", req.Model), slog.String("served", out.Model))
	}
	return out
}

// usageOf totals token usage. With server-side fallbacks, usage.iterations
// lists every attempt (the top-level counts cover only the final one).
func usageOf(u sdk.BetaUsage) ai.Usage {
	if len(u.Iterations) > 0 {
		var total ai.Usage
		for _, it := range u.Iterations {
			total.InputTokens += int(it.InputTokens + it.CacheCreationInputTokens + it.CacheReadInputTokens)
			total.OutputTokens += int(it.OutputTokens)
		}
		return total
	}
	return ai.Usage{
		InputTokens:  int(u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens),
		OutputTokens: int(u.OutputTokens),
	}
}

func mapStopReason(r sdk.BetaStopReason) string {
	switch r {
	case sdk.BetaStopReasonEndTurn:
		return ai.StopEndTurn
	case sdk.BetaStopReasonMaxTokens, sdk.BetaStopReasonModelContextWindowExceeded:
		return ai.StopMaxTokens
	case sdk.BetaStopReasonStopSequence:
		return ai.StopStopSequence
	case sdk.BetaStopReasonRefusal:
		return ai.StopRefusal
	default:
		return ai.StopOther
	}
}

// errorTypeStatus maps Anthropic error types to HTTP statuses for errors
// delivered inside an otherwise successful (200) event stream.
var errorTypeStatus = map[string]int{
	"invalid_request_error": http.StatusBadRequest,
	"authentication_error":  http.StatusUnauthorized,
	"billing_error":         http.StatusPaymentRequired,
	"permission_error":      http.StatusForbidden,
	"not_found_error":       http.StatusNotFound,
	"request_too_large":     http.StatusRequestEntityTooLarge,
	"rate_limit_error":      http.StatusTooManyRequests,
	"api_error":             http.StatusInternalServerError,
	"timeout_error":         http.StatusGatewayTimeout,
	"overloaded_error":      529,
}

// convertErr turns SDK errors into *ai.UpstreamError, keeping context
// cancellation recognisable.
func (p *Provider) convertErr(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("ai: %s: %w", p.name, ctxErr)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("ai: %s: %w", p.name, err)
	}
	var apiErr *sdk.Error
	if errors.As(err, &apiErr) {
		status := apiErr.StatusCode
		if status < 400 {
			status = errorTypeStatus[string(apiErr.Type())]
		}
		msg := httpx.ErrorMessage([]byte(apiErr.RawJSON()))
		if msg == "" {
			msg = http.StatusText(status)
		}
		if msg == "" {
			msg = "request failed"
		}
		ue := &ai.UpstreamError{Provider: p.name, Status: status, Message: msg}
		// The SDK keeps the final attempt's response (for an error event
		// inside a stream: the 200 stream response, whose headers carry no
		// retry time and whose status keeps reset headers out of it).
		if apiErr.Response != nil {
			ue.RetryDelay = httpx.RetryAfter(apiErr.Response.StatusCode, apiErr.Response.Header, time.Now())
		}
		return ue
	}
	return &ai.UpstreamError{Provider: p.name, Message: httpx.ErrorText(err)}
}

// requestTimeout is the per-attempt SDK timeout for non-streaming calls.
func requestTimeout(ctx context.Context) time.Duration {
	if dl, ok := ctx.Deadline(); ok {
		return max(time.Until(dl), time.Second)
	}
	return defaultRequestBudget
}
