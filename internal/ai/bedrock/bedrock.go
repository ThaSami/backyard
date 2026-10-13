// Package bedrock implements ai.Provider for Amazon Bedrock using the AWS
// SDK for Go v2 (bedrockruntime).
//
// Chat uses the model-agnostic Converse / ConverseStream APIs, so any Bedrock
// text model works (Anthropic Claude, Amazon Nova, Meta Llama, Mistral,
// Cohere, …); images are sent as bytes. Embeddings use InvokeModel with
// Amazon Titan Text Embeddings (amazon.titan-embed-*) or Cohere Embed
// (cohere.embed-*). Image generation uses InvokeModel with Amazon Nova Canvas
// / Titan Image Generator (TEXT_IMAGE task) or Stability AI models.
//
// Credentials come from the default AWS chain (environment, shared config,
// IAM role, IRSA, ECS task role…); an api_key is used as a Bedrock API key
// (bearer token) instead. region is required unless AWS_REGION is set;
// base_url overrides the endpoint (VPC endpoints, tests).
package bedrock

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/auth/bearer"

	"github.com/thasami/backyard/internal/ai"
	"github.com/thasami/backyard/internal/ai/internal/claudemodel"
	"github.com/thasami/backyard/internal/ai/internal/httpx"
	"github.com/thasami/backyard/internal/ai/internal/imagefetch"
	"github.com/thasami/backyard/internal/config"
)

const (
	cohereEmbedBatch   = 96
	titanConcurrency   = 4
	novaMaxImagesPerGo = 5
)

// Options are optional dependencies.
type Options struct {
	// HTTPClient performs upstream requests (default: the AWS SDK's client).
	HTTPClient *http.Client
	// Logger receives debug logs (default: slog.Default()).
	Logger *slog.Logger
	// FetchImage downloads URL images (Converse needs bytes). Default: an
	// SSRF-guarded fetcher.
	FetchImage imagefetch.Func
	// AWSConfig replaces config.LoadDefaultConfig (tests, custom credential
	// chains). Region from the provider config still takes precedence.
	AWSConfig *aws.Config
}

// Provider is an ai.Provider for Amazon Bedrock. It is safe for concurrent use.
type Provider struct {
	name   string
	effort string // default effort for Claude models
	client *bedrockruntime.Client
	log    *slog.Logger
	fetch  imagefetch.Func
}

var _ ai.Provider = (*Provider)(nil)

// New creates a Bedrock provider. Loading the AWS configuration reads only
// local sources (environment, shared files); credentials are resolved per
// request.
func New(ctx context.Context, name string, pc config.ProviderConfig, opts Options) (*Provider, error) {
	if pc.Effort != "" && !ai.ValidEffort(pc.Effort) {
		return nil, fmt.Errorf("ai: provider %s: invalid effort %q (want low, medium, high, xhigh or max)", name, pc.Effort)
	}
	var cfg aws.Config
	if opts.AWSConfig != nil {
		cfg = opts.AWSConfig.Copy()
	} else {
		var load []func(*awsconfig.LoadOptions) error
		if pc.Region != "" {
			load = append(load, awsconfig.WithRegion(pc.Region))
		}
		var err error
		cfg, err = awsconfig.LoadDefaultConfig(ctx, load...)
		if err != nil {
			return nil, fmt.Errorf("ai: provider %s: load AWS config: %w", name, err)
		}
	}
	if pc.Region != "" {
		cfg.Region = pc.Region
	}
	if cfg.Region == "" {
		return nil, fmt.Errorf("ai: provider %s: region is required for bedrock (or set AWS_REGION)", name)
	}
	if opts.HTTPClient != nil {
		cfg.HTTPClient = opts.HTTPClient
	}
	client := bedrockruntime.NewFromConfig(cfg, func(o *bedrockruntime.Options) {
		if pc.BaseURL != "" {
			o.BaseEndpoint = aws.String(strings.TrimRight(pc.BaseURL, "/"))
		}
		if pc.APIKey != "" {
			o.BearerAuthTokenProvider = bearer.StaticTokenProvider{Token: bearer.Token{Value: pc.APIKey}}
			o.AuthSchemePreference = []string{"httpBearerAuth"}
		}
	})
	p := &Provider{name: name, effort: pc.Effort, client: client, log: opts.Logger, fetch: opts.FetchImage}
	if p.log == nil {
		p.log = slog.Default()
	}
	if p.fetch == nil {
		p.fetch = imagefetch.New().Fetch
	}
	return p, nil
}

// Name implements ai.Provider.
func (p *Provider) Name() string { return p.name }

// ---------------------------------------------------------------------------
// Chat (Converse)

// Chat implements ai.Provider using Converse.
func (p *Provider) Chat(ctx context.Context, req ai.ChatRequest) (*ai.ChatResponse, error) {
	in, err := p.converseInput(ctx, req)
	if err != nil {
		return nil, err
	}
	out, err := p.client.Converse(ctx, in)
	if err != nil {
		return nil, p.convertErr(ctx, err)
	}
	return converseResponse(out, req), nil
}

// ChatStream implements ai.Provider using ConverseStream.
func (p *Provider) ChatStream(ctx context.Context, req ai.ChatRequest, onDelta func(string) error) (*ai.ChatResponse, error) {
	in, err := p.converseInput(ctx, req)
	if err != nil {
		return nil, err
	}
	out, err := p.client.ConverseStream(ctx, &bedrockruntime.ConverseStreamInput{
		ModelId:                      in.ModelId,
		Messages:                     in.Messages,
		System:                       in.System,
		InferenceConfig:              in.InferenceConfig,
		AdditionalModelRequestFields: in.AdditionalModelRequestFields,
	})
	if err != nil {
		return nil, p.convertErr(ctx, err)
	}
	stream := out.GetStream()
	defer stream.Close()

	var (
		text    strings.Builder
		resp    = &ai.ChatResponse{Model: req.Model, StopReason: ai.StopOther}
		stopped bool
	)
	for ev := range stream.Events() {
		switch v := ev.(type) {
		case *types.ConverseStreamOutputMemberContentBlockDelta:
			if d, ok := v.Value.Delta.(*types.ContentBlockDeltaMemberText); ok && d.Value != "" {
				text.WriteString(d.Value)
				if onDelta != nil {
					if err := onDelta(d.Value); err != nil {
						return nil, err
					}
				}
			}
		case *types.ConverseStreamOutputMemberMessageStop:
			resp.StopReason = mapStopReason(v.Value.StopReason)
			stopped = true
		case *types.ConverseStreamOutputMemberMetadata:
			resp.Usage = usageOf(v.Value.Usage)
		}
	}
	if err := stream.Err(); err != nil {
		// A transport error after messageStop (e.g. the connection dropping
		// while reading the trailing metadata/EOF) does not invalidate a
		// complete answer; service exceptions always fail the request.
		var ae smithy.APIError
		if !stopped || errors.As(err, &ae) || ctx.Err() != nil {
			return nil, p.convertErr(ctx, err)
		}
		p.log.Debug("ai: ignoring transport error after end of stream",
			slog.String("provider", p.name), slog.String("model", req.Model), slog.Any("error", err))
	}
	if !stopped {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("ai: %s: %w", p.name, ctxErr)
		}
		return nil, &ai.UpstreamError{Provider: p.name, Message: "stream ended unexpectedly"}
	}
	resp.Content = text.String()
	if req.JSON {
		resp.Content = ai.StripCodeFences(resp.Content)
	}
	return resp, nil
}

func (p *Provider) converseInput(ctx context.Context, req ai.ChatRequest) (*bedrockruntime.ConverseInput, error) {
	if req.Model == "" {
		return nil, fmt.Errorf("%w: model is required", ai.ErrBadRequest)
	}
	msgs := make([]types.Message, 0, len(req.Messages))
	for i, m := range req.Messages {
		var role types.ConversationRole
		switch m.Role {
		case "user":
			role = types.ConversationRoleUser
		case "assistant":
			role = types.ConversationRoleAssistant
		default:
			return nil, fmt.Errorf("%w: message %d: unsupported role %q", ai.ErrBadRequest, i, m.Role)
		}
		blocks := make([]types.ContentBlock, 0, len(m.Content))
		for _, part := range m.Content {
			switch part.Type {
			case "text":
				// Converse rejects blank text blocks.
				if strings.TrimSpace(part.Text) != "" {
					blocks = append(blocks, &types.ContentBlockMemberText{Value: part.Text})
				}
			case "image":
				img, err := p.imageBlock(ctx, part)
				if err != nil {
					return nil, err
				}
				blocks = append(blocks, img)
			default:
				return nil, fmt.Errorf("%w: unsupported content type %q", ai.ErrBadRequest, part.Type)
			}
		}
		if len(blocks) == 0 {
			return nil, fmt.Errorf("%w: message %d has no content", ai.ErrBadRequest, i)
		}
		msgs = append(msgs, types.Message{Role: role, Content: blocks})
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("%w: no messages", ai.ErrBadRequest)
	}
	in := &bedrockruntime.ConverseInput{ModelId: aws.String(req.Model), Messages: msgs}
	system := req.System
	if req.JSON {
		system = ai.SystemWithJSON(system)
	}
	if system != "" {
		in.System = []types.SystemContentBlock{&types.SystemContentBlockMemberText{Value: system}}
	}
	ic := &types.InferenceConfiguration{}
	if req.MaxTokens > 0 {
		ic.MaxTokens = aws.Int32(int32(min(req.MaxTokens, 1<<30)))
	}
	isClaude := claudemodel.Base(req.Model) != ""
	if req.Temperature != nil && (!isClaude || claudemodel.AcceptsSampling(req.Model)) {
		ic.Temperature = aws.Float32(float32(min(max(*req.Temperature, 0), 1)))
	}
	if ic.MaxTokens != nil || ic.Temperature != nil {
		in.InferenceConfig = ic
	}
	if isClaude {
		effort := req.Effort
		if effort == "" {
			effort = p.effort
		}
		if e := claudemodel.EffortFor(req.Model, effort); e != "" {
			fields := map[string]any{"output_config": map[string]any{"effort": e}}
			if claudemodel.NeedsEffortBeta(req.Model) {
				fields["anthropic_beta"] = []string{claudemodel.EffortBeta}
			}
			in.AdditionalModelRequestFields = document.NewLazyDocument(fields)
		}
	}
	return in, nil
}

var imageFormats = map[string]types.ImageFormat{
	"image/png":  types.ImageFormatPng,
	"image/jpeg": types.ImageFormatJpeg,
	"image/jpg":  types.ImageFormatJpeg,
	"image/gif":  types.ImageFormatGif,
	"image/webp": types.ImageFormatWebp,
}

func (p *Provider) imageBlock(ctx context.Context, part ai.Part) (types.ContentBlock, error) {
	data, mt := part.Data, part.MediaType
	if len(data) == 0 {
		switch {
		case part.URL == "":
			return nil, fmt.Errorf("%w: image part needs data or url", ai.ErrBadRequest)
		case strings.HasPrefix(part.URL, "data:"):
			d, m, ok := ai.ParseDataURL(part.URL)
			if !ok {
				return nil, fmt.Errorf("%w: invalid image data URL", ai.ErrBadRequest)
			}
			data, mt = d, m
		default:
			d, m, err := p.fetch(ctx, part.URL)
			if err != nil {
				return nil, imagefetch.RequestError(ctx, ai.ErrBadRequest, err)
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
	format, ok := imageFormats[strings.ToLower(mt)]
	if !ok {
		return nil, fmt.Errorf("%w: unsupported image type %q (png, jpeg, gif or webp)", ai.ErrBadRequest, mt)
	}
	return &types.ContentBlockMemberImage{Value: types.ImageBlock{
		Format: format,
		Source: &types.ImageSourceMemberBytes{Value: data},
	}}, nil
}

func converseResponse(out *bedrockruntime.ConverseOutput, req ai.ChatRequest) *ai.ChatResponse {
	resp := &ai.ChatResponse{Model: req.Model, StopReason: mapStopReason(out.StopReason), Usage: usageOf(out.Usage)}
	if msg, ok := out.Output.(*types.ConverseOutputMemberMessage); ok {
		var b strings.Builder
		for _, c := range msg.Value.Content {
			if t, ok := c.(*types.ContentBlockMemberText); ok {
				b.WriteString(t.Value)
			}
		}
		resp.Content = b.String()
	}
	if req.JSON {
		resp.Content = ai.StripCodeFences(resp.Content)
	}
	return resp
}

func mapStopReason(r types.StopReason) string {
	switch r {
	case types.StopReasonEndTurn:
		return ai.StopEndTurn
	case types.StopReasonMaxTokens, types.StopReasonModelContextWindowExceeded:
		return ai.StopMaxTokens
	case types.StopReasonStopSequence:
		return ai.StopStopSequence
	case types.StopReasonGuardrailIntervened, types.StopReasonContentFiltered:
		return ai.StopRefusal
	default:
		if r == "refusal" {
			return ai.StopRefusal
		}
		return ai.StopOther
	}
}

func usageOf(u *types.TokenUsage) ai.Usage {
	if u == nil {
		return ai.Usage{}
	}
	in := int(aws.ToInt32(u.InputTokens)) + int(aws.ToInt32(u.CacheReadInputTokens)) + int(aws.ToInt32(u.CacheWriteInputTokens))
	return ai.Usage{InputTokens: in, OutputTokens: int(aws.ToInt32(u.OutputTokens))}
}

// ---------------------------------------------------------------------------
// Embeddings (InvokeModel)

// Embed implements ai.Provider for Titan and Cohere embedding models.
func (p *Provider) Embed(ctx context.Context, req ai.EmbedRequest) (*ai.EmbedResponse, error) {
	if req.Model == "" || len(req.Input) == 0 {
		return nil, fmt.Errorf("%w: model and input are required", ai.ErrBadRequest)
	}
	m := strings.ToLower(req.Model)
	switch {
	case strings.Contains(m, "titan-embed"):
		return p.embedTitan(ctx, req)
	case strings.Contains(m, "cohere.embed"):
		return p.embedCohere(ctx, req)
	default:
		return nil, fmt.Errorf("%w: bedrock embeddings support amazon.titan-embed-* and cohere.embed-* models, not %q", ai.ErrUnsupported, req.Model)
	}
}

func (p *Provider) embedTitan(ctx context.Context, req ai.EmbedRequest) (*ai.EmbedResponse, error) {
	out := &ai.EmbedResponse{Embeddings: make([][]float64, len(req.Input))}
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		firstErr error
		tokens   int
	)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sem := make(chan struct{}, titanConcurrency)
	for i, text := range req.Input {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			var res struct {
				Embedding           []float64 `json:"embedding"`
				InputTextTokenCount int       `json:"inputTextTokenCount"`
			}
			err := p.invoke(ctx, req.Model, map[string]any{"inputText": text}, &res)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
					cancel()
				}
				return
			}
			out.Embeddings[i] = res.Embedding
			tokens += res.InputTextTokenCount
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("ai: %s: %w", p.name, err)
	}
	out.Usage.InputTokens = tokens
	return out, nil
}

func (p *Provider) embedCohere(ctx context.Context, req ai.EmbedRequest) (*ai.EmbedResponse, error) {
	out := &ai.EmbedResponse{Embeddings: make([][]float64, 0, len(req.Input))}
	for start := 0; start < len(req.Input); start += cohereEmbedBatch {
		chunk := req.Input[start:min(start+cohereEmbedBatch, len(req.Input))]
		var res struct {
			Embeddings json.RawMessage `json:"embeddings"`
		}
		body := map[string]any{"texts": chunk, "input_type": "search_document", "truncate": "END"}
		if err := p.invoke(ctx, req.Model, body, &res); err != nil {
			return nil, err
		}
		vecs, err := parseCohereEmbeddings(res.Embeddings)
		if err != nil || len(vecs) != len(chunk) {
			return nil, &ai.UpstreamError{Provider: p.name, Message: fmt.Sprintf("unexpected cohere embeddings response (%d for %d inputs)", len(vecs), len(chunk))}
		}
		out.Embeddings = append(out.Embeddings, vecs...)
	}
	return out, nil
}

// parseCohereEmbeddings accepts both response shapes: a plain array (v3,
// embedding_types unset) and {"float": [...]} (v4 / embedding_types set).
func parseCohereEmbeddings(raw json.RawMessage) ([][]float64, error) {
	var plain [][]float64
	if err := json.Unmarshal(raw, &plain); err == nil {
		return plain, nil
	}
	var typed struct {
		Float [][]float64 `json:"float"`
	}
	if err := json.Unmarshal(raw, &typed); err != nil {
		return nil, err
	}
	return typed.Float, nil
}

// ---------------------------------------------------------------------------
// Images (InvokeModel)

// Image implements ai.Provider for Amazon Nova Canvas / Titan Image Generator
// and Stability AI models.
func (p *Provider) Image(ctx context.Context, req ai.ImageRequest) (*ai.ImageResponse, error) {
	if req.Model == "" || strings.TrimSpace(req.Prompt) == "" {
		return nil, fmt.Errorf("%w: model and prompt are required", ai.ErrBadRequest)
	}
	n := max(req.N, 1)
	m := strings.ToLower(req.Model)
	switch {
	case strings.Contains(m, "nova-canvas") || strings.Contains(m, "titan-image"):
		return p.imageAmazon(ctx, req, n)
	case strings.Contains(m, "stability."):
		return p.imageStability(ctx, req, n)
	default:
		return nil, fmt.Errorf("%w: bedrock image generation supports amazon.nova-canvas, amazon.titan-image-generator and stability.* models, not %q", ai.ErrUnsupported, req.Model)
	}
}

type amazonImageResponse struct {
	Images []string `json:"images"`
	Error  string   `json:"error"`
}

func (p *Provider) imageAmazon(ctx context.Context, req ai.ImageRequest, n int) (*ai.ImageResponse, error) {
	w, h := parseSize(req.Size)
	out := &ai.ImageResponse{}
	for len(out.Images) < n {
		count := min(n-len(out.Images), novaMaxImagesPerGo)
		body := map[string]any{
			"taskType":          "TEXT_IMAGE",
			"textToImageParams": map[string]any{"text": req.Prompt},
			"imageGenerationConfig": map[string]any{
				"numberOfImages": count,
				"width":          w,
				"height":         h,
				"quality":        "standard",
			},
		}
		var res amazonImageResponse
		if err := p.invoke(ctx, req.Model, body, &res); err != nil {
			return nil, err
		}
		if res.Error != "" {
			return nil, &ai.UpstreamError{Provider: p.name, Message: httpx.Truncate(res.Error)}
		}
		if len(res.Images) == 0 {
			return nil, &ai.UpstreamError{Provider: p.name, Message: "no images returned"}
		}
		for _, b64 := range res.Images {
			img, err := p.decodeImage(b64)
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

func (p *Provider) imageStability(ctx context.Context, req ai.ImageRequest, n int) (*ai.ImageResponse, error) {
	out := &ai.ImageResponse{}
	for range n {
		body := map[string]any{"prompt": req.Prompt, "output_format": "png"}
		if ar := stabilityAspect(req.Size); ar != "" {
			body["aspect_ratio"] = ar
		}
		var res struct {
			Images        []string  `json:"images"`
			FinishReasons []*string `json:"finish_reasons"`
		}
		if err := p.invoke(ctx, req.Model, body, &res); err != nil {
			return nil, err
		}
		for _, fr := range res.FinishReasons {
			if fr != nil && *fr != "" {
				return nil, &ai.UpstreamError{Provider: p.name, Message: "image generation stopped: " + httpx.Truncate(*fr)}
			}
		}
		if len(res.Images) == 0 {
			return nil, &ai.UpstreamError{Provider: p.name, Message: "no images returned"}
		}
		img, err := p.decodeImage(res.Images[0])
		if err != nil {
			return nil, err
		}
		out.Images = append(out.Images, img)
	}
	return out, nil
}

func (p *Provider) decodeImage(b64 string) (ai.Image, error) {
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return ai.Image{}, &ai.UpstreamError{Provider: p.name, Message: "invalid base64 image"}
	}
	mt := http.DetectContentType(data)
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = mt[:i]
	}
	if !strings.HasPrefix(mt, "image/") {
		mt = "image/png"
	}
	return ai.Image{Data: data, MediaType: mt}, nil
}

// parseSize parses "WxH" (default 1024x1024) and snaps it to the Nova Canvas
// constraints: multiples of 16 between 320 and 4096.
func parseSize(size string) (int, int) {
	w, h := 1024, 1024
	if ws, hs, ok := strings.Cut(strings.ToLower(strings.TrimSpace(size)), "x"); ok {
		if a, err := strconv.Atoi(strings.TrimSpace(ws)); err == nil && a > 0 {
			w = a
		}
		if b, err := strconv.Atoi(strings.TrimSpace(hs)); err == nil && b > 0 {
			h = b
		}
	}
	snap := func(v int) int {
		v = (v + 8) / 16 * 16
		return min(max(v, 320), 4096)
	}
	return snap(w), snap(h)
}

// stabilityAspect maps "WxH" or "W:H" to a Stability aspect_ratio value.
func stabilityAspect(size string) string {
	size = strings.TrimSpace(size)
	if size == "" {
		return ""
	}
	if strings.Contains(size, ":") {
		return size
	}
	w, h := parseSize(size)
	ratios := []struct {
		name string
		v    float64
	}{{"1:1", 1}, {"16:9", 16.0 / 9}, {"9:16", 9.0 / 16}, {"3:2", 1.5}, {"2:3", 2.0 / 3}, {"5:4", 1.25}, {"4:5", 0.8}, {"21:9", 21.0 / 9}, {"9:21", 9.0 / 21}}
	target := float64(w) / float64(h)
	best, diff := "1:1", 1e9
	for _, r := range ratios {
		d := r.v/target + target/r.v
		if d < diff {
			best, diff = r.name, d
		}
	}
	return best
}

// ---------------------------------------------------------------------------
// Transport helpers

func (p *Provider) invoke(ctx context.Context, model string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("ai: %s: encode request: %w", p.name, err)
	}
	res, err := p.client.InvokeModel(ctx, &bedrockruntime.InvokeModelInput{
		ModelId:     aws.String(model),
		Body:        b,
		ContentType: aws.String("application/json"),
		Accept:      aws.String("application/json"),
	})
	if err != nil {
		return p.convertErr(ctx, err)
	}
	if err := json.Unmarshal(res.Body, out); err != nil {
		return &ai.UpstreamError{Provider: p.name, Message: "invalid JSON response: " + err.Error()}
	}
	return nil
}

// errorCodeStatus maps Bedrock exception names to HTTP statuses for errors
// that carry none (event-stream exceptions).
var errorCodeStatus = map[string]int{
	"ValidationException":           http.StatusBadRequest,
	"AccessDeniedException":         http.StatusForbidden,
	"ResourceNotFoundException":     http.StatusNotFound,
	"ThrottlingException":           http.StatusTooManyRequests,
	"ServiceQuotaExceededException": http.StatusTooManyRequests,
	"ModelTimeoutException":         http.StatusGatewayTimeout,
	"ModelNotReadyException":        http.StatusServiceUnavailable,
	"ServiceUnavailableException":   http.StatusServiceUnavailable,
	"InternalServerException":       http.StatusInternalServerError,
	"ModelStreamErrorException":     http.StatusBadGateway,
	"ModelErrorException":           http.StatusBadGateway,
}

func (p *Provider) convertErr(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("ai: %s: %w", p.name, ctxErr)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("ai: %s: %w", p.name, err)
	}
	ue := &ai.UpstreamError{Provider: p.name}
	var re *awshttp.ResponseError
	if errors.As(err, &re) {
		ue.Status = re.HTTPStatusCode()
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		ue.Message = ae.ErrorMessage()
		if ue.Message == "" {
			ue.Message = ae.ErrorCode()
		}
		if ue.Status == 0 {
			ue.Status = errorCodeStatus[ae.ErrorCode()]
		}
	}
	if ue.Message == "" {
		ue.Message = httpx.ErrorText(err) // drops the endpoint URL of transport errors
	}
	ue.Message = httpx.Truncate(ue.Message)
	return ue
}
