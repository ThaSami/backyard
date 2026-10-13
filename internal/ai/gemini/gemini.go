// Package gemini implements ai.Provider for Google's Gemini models over raw
// HTTP, either through the Gemini API in Google AI Studio (api_key,
// https://generativelanguage.googleapis.com/v1beta) or through Vertex AI
// (project_id + location with Application Default Credentials; location
// "global" uses https://aiplatform.googleapis.com). Vertex AI express mode
// (platform "vertex" with an api_key and no project) is supported as well.
//
// Chat uses generateContent / streamGenerateContent?alt=sse with
// systemInstruction, inline image data and generationConfig
// (maxOutputTokens, temperature, responseMimeType "application/json" for JSON
// mode). Embeddings use batchEmbedContents (AI Studio) or the :predict
// endpoint of Vertex text-embedding models. Images use Imagen's :predict
// endpoint for "imagen-*" models and generateContent with
// responseModalities ["IMAGE"] for Gemini image models.
package gemini

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/thasami/backyard/internal/ai"
	"github.com/thasami/backyard/internal/ai/internal/httpx"
	"github.com/thasami/backyard/internal/ai/internal/imagefetch"
	"github.com/thasami/backyard/internal/ai/internal/sse"
	"github.com/thasami/backyard/internal/config"
)

const (
	aiStudioBaseURL       = "https://generativelanguage.googleapis.com/v1beta"
	cloudPlatformScope    = "https://www.googleapis.com/auth/cloud-platform"
	defaultVertexLocation = "global"
	aiStudioEmbedBatch    = 100
	vertexEmbedBatch      = 32
)

// Options are optional dependencies.
type Options struct {
	// HTTPClient performs upstream requests (default: httpx.DefaultClient()).
	HTTPClient *http.Client
	// Logger receives debug logs (default: slog.Default()).
	Logger *slog.Logger
	// TokenSource overrides Application Default Credentials for Vertex AI.
	TokenSource oauth2.TokenSource
	// FetchImage downloads URL images (Gemini needs inline bytes). Default:
	// an SSRF-guarded fetcher.
	FetchImage imagefetch.Func
}

// Provider is an ai.Provider for Gemini. It is safe for concurrent use.
type Provider struct {
	name     string
	vertex   bool
	express  bool // Vertex AI express mode (API key, no project)
	apiKey   string
	location string
	baseURL  string
	headers  map[string]string
	client   *http.Client
	log      *slog.Logger
	fetch    imagefetch.Func

	mu      sync.Mutex
	project string
	ts      oauth2.TokenSource
}

var _ ai.Provider = (*Provider)(nil)

// New creates a Gemini provider. Vertex AI is used when project_id is set
// (or platform is "vertex"); otherwise AI Studio with api_key (falling back
// to the GEMINI_API_KEY / GOOGLE_API_KEY environment variables).
func New(name string, pc config.ProviderConfig, opts Options) (*Provider, error) {
	p := &Provider{
		name:     name,
		vertex:   pc.ProjectID != "" || pc.Platform == "vertex",
		apiKey:   pc.APIKey,
		project:  pc.ProjectID,
		location: pc.Location,
		headers:  pc.Headers,
		client:   opts.HTTPClient,
		log:      opts.Logger,
		fetch:    opts.FetchImage,
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
	if p.vertex {
		if p.location == "" {
			p.location = defaultVertexLocation
		}
		if !locationRE.MatchString(p.location) {
			return nil, fmt.Errorf("ai: provider %s: invalid location %q", name, p.location)
		}
		p.express = p.project == "" && p.apiKey != ""
		p.ts = opts.TokenSource
		p.baseURL = pc.BaseURL
		if p.baseURL == "" {
			p.baseURL = vertexHost(p.location) + "/v1"
		}
	} else {
		if p.apiKey == "" {
			p.apiKey = firstNonEmpty(os.Getenv("GEMINI_API_KEY"), os.Getenv("GOOGLE_API_KEY"))
		}
		if p.apiKey == "" {
			return nil, fmt.Errorf("ai: provider %s: gemini needs api_key (AI Studio) or project_id (Vertex AI)", name)
		}
		p.baseURL = firstNonEmpty(pc.BaseURL, aiStudioBaseURL)
	}
	u, err := url.Parse(p.baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("ai: provider %s: invalid base URL %q", name, p.baseURL)
	}
	p.baseURL = strings.TrimRight(p.baseURL, "/")
	return p, nil
}

// locationRE matches Google Cloud locations ("global", "us-central1"); the
// location becomes part of the API host name.
var locationRE = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// vertexHost returns the Vertex AI API host for a location.
func vertexHost(location string) string {
	if location == "global" {
		return "https://aiplatform.googleapis.com"
	}
	return "https://" + location + "-aiplatform.googleapis.com"
}

// Name implements ai.Provider.
func (p *Provider) Name() string { return p.name }

// ---------------------------------------------------------------------------
// Wire types

type content struct {
	Role  string `json:"role,omitempty"`
	Parts []part `json:"parts"`
}

type part struct {
	Text       string `json:"text,omitempty"`
	InlineData *blob  `json:"inlineData,omitempty"`
	Thought    bool   `json:"thought,omitempty"`
}

type blob struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type generationConfig struct {
	MaxOutputTokens    int          `json:"maxOutputTokens,omitempty"`
	Temperature        *float64     `json:"temperature,omitempty"`
	ResponseMimeType   string       `json:"responseMimeType,omitempty"`
	ResponseModalities []string     `json:"responseModalities,omitempty"`
	ImageConfig        *imageConfig `json:"imageConfig,omitempty"`
}

type imageConfig struct {
	AspectRatio string `json:"aspectRatio,omitempty"`
}

type generateRequest struct {
	Contents          []content         `json:"contents"`
	SystemInstruction *content          `json:"systemInstruction,omitempty"`
	GenerationConfig  *generationConfig `json:"generationConfig,omitempty"`
}

type usageMetadata struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	ThoughtsTokenCount   int `json:"thoughtsTokenCount"`
}

type generateResponse struct {
	Candidates []struct {
		Content      content `json:"content"`
		FinishReason string  `json:"finishReason"`
	} `json:"candidates"`
	PromptFeedback *struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
	UsageMetadata *usageMetadata  `json:"usageMetadata"`
	ModelVersion  string          `json:"modelVersion"`
	Error         json.RawMessage `json:"error"`
}

func (u *usageMetadata) toAI() ai.Usage {
	if u == nil {
		return ai.Usage{}
	}
	return ai.Usage{InputTokens: u.PromptTokenCount, OutputTokens: u.CandidatesTokenCount + u.ThoughtsTokenCount}
}

// ---------------------------------------------------------------------------
// Chat

// Chat implements ai.Provider.
func (p *Provider) Chat(ctx context.Context, req ai.ChatRequest) (*ai.ChatResponse, error) {
	body, err := p.buildGenerate(ctx, req)
	if err != nil {
		return nil, err
	}
	var gr generateResponse
	if err := p.postJSON(ctx, req.Model, "generateContent", body, &gr); err != nil {
		return nil, err
	}
	out := &ai.ChatResponse{Model: firstNonEmpty(gr.ModelVersion, req.Model), Usage: gr.UsageMetadata.toAI(), StopReason: ai.StopOther}
	if len(gr.Candidates) == 0 {
		if gr.PromptFeedback != nil && gr.PromptFeedback.BlockReason != "" {
			out.StopReason = ai.StopRefusal
			return out, nil
		}
		return nil, &ai.UpstreamError{Provider: p.name, Message: "response has no candidates"}
	}
	c := gr.Candidates[0]
	out.Content = textOf(c.Content.Parts)
	out.StopReason = mapFinishReason(c.FinishReason)
	if req.JSON {
		out.Content = ai.StripCodeFences(out.Content)
	}
	return out, nil
}

// ChatStream implements ai.Provider via streamGenerateContent?alt=sse.
func (p *Provider) ChatStream(ctx context.Context, req ai.ChatRequest, onDelta func(string) error) (*ai.ChatResponse, error) {
	body, err := p.buildGenerate(ctx, req)
	if err != nil {
		return nil, err
	}
	resp, err := p.post(ctx, req.Model, "streamGenerateContent", "alt=sse", body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var (
		text     strings.Builder
		out      = &ai.ChatResponse{Model: req.Model, StopReason: ai.StopOther}
		finished bool
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
		if strings.TrimSpace(ev.Data) == "" {
			continue
		}
		var chunk generateResponse
		if err := json.Unmarshal([]byte(ev.Data), &chunk); err != nil {
			return nil, &ai.UpstreamError{Provider: p.name, Message: "invalid stream chunk: " + err.Error()}
		}
		if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
			return nil, &ai.UpstreamError{Provider: p.name, Message: httpx.ErrorMessage([]byte(`{"error":` + string(chunk.Error) + `}`))}
		}
		if chunk.ModelVersion != "" {
			out.Model = chunk.ModelVersion
		}
		if chunk.UsageMetadata != nil {
			out.Usage = chunk.UsageMetadata.toAI()
		}
		if len(chunk.Candidates) == 0 {
			if chunk.PromptFeedback != nil && chunk.PromptFeedback.BlockReason != "" {
				out.StopReason = ai.StopRefusal
				finished = true
			}
			continue
		}
		c := chunk.Candidates[0]
		if t := textOf(c.Content.Parts); t != "" {
			text.WriteString(t)
			if onDelta != nil {
				if err := onDelta(t); err != nil {
					return nil, err
				}
			}
		}
		if c.FinishReason != "" {
			out.StopReason = mapFinishReason(c.FinishReason)
			finished = true
		}
	}
	if !finished {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("ai: %s: %w", p.name, ctxErr)
		}
		return nil, &ai.UpstreamError{Provider: p.name, Message: "stream ended unexpectedly"}
	}
	out.Content = text.String()
	if req.JSON {
		out.Content = ai.StripCodeFences(out.Content)
	}
	return out, nil
}

func (p *Provider) buildGenerate(ctx context.Context, req ai.ChatRequest) (*generateRequest, error) {
	if req.Model == "" {
		return nil, fmt.Errorf("%w: model is required", ai.ErrBadRequest)
	}
	body := &generateRequest{Contents: make([]content, 0, len(req.Messages))}
	for i, m := range req.Messages {
		role := ""
		switch m.Role {
		case "user":
			role = "user"
		case "assistant":
			role = "model"
		default:
			return nil, fmt.Errorf("%w: message %d: unsupported role %q", ai.ErrBadRequest, i, m.Role)
		}
		parts := make([]part, 0, len(m.Content))
		for _, pt := range m.Content {
			switch pt.Type {
			case "text":
				if pt.Text != "" {
					parts = append(parts, part{Text: pt.Text})
				}
			case "image":
				b, err := p.inlineImage(ctx, pt)
				if err != nil {
					return nil, err
				}
				parts = append(parts, part{InlineData: b})
			default:
				return nil, fmt.Errorf("%w: unsupported content type %q", ai.ErrBadRequest, pt.Type)
			}
		}
		if len(parts) == 0 {
			return nil, fmt.Errorf("%w: message %d has no content", ai.ErrBadRequest, i)
		}
		body.Contents = append(body.Contents, content{Role: role, Parts: parts})
	}
	if len(body.Contents) == 0 {
		return nil, fmt.Errorf("%w: no messages", ai.ErrBadRequest)
	}
	system := req.System
	if req.JSON {
		// responseMimeType guarantees JSON but not an object; the instruction
		// asks for a single object like every other provider.
		system = ai.SystemWithJSON(system)
	}
	if system != "" {
		body.SystemInstruction = &content{Parts: []part{{Text: system}}}
	}
	gc := &generationConfig{MaxOutputTokens: req.MaxTokens}
	if req.Temperature != nil {
		t := min(max(*req.Temperature, 0), 2)
		gc.Temperature = &t
	}
	if req.JSON {
		gc.ResponseMimeType = "application/json"
	}
	if gc.MaxOutputTokens > 0 || gc.Temperature != nil || gc.ResponseMimeType != "" {
		body.GenerationConfig = gc
	}
	return body, nil
}

func (p *Provider) inlineImage(ctx context.Context, pt ai.Part) (*blob, error) {
	data, mt := pt.Data, pt.MediaType
	if len(data) == 0 {
		switch {
		case pt.URL == "":
			return nil, fmt.Errorf("%w: image part needs data or url", ai.ErrBadRequest)
		case strings.HasPrefix(pt.URL, "data:"):
			d, m, ok := ai.ParseDataURL(pt.URL)
			if !ok {
				return nil, fmt.Errorf("%w: invalid image data URL", ai.ErrBadRequest)
			}
			data, mt = d, m
		default:
			d, m, err := p.fetch(ctx, pt.URL)
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
	if !strings.HasPrefix(mt, "image/") {
		return nil, fmt.Errorf("%w: unsupported image type %q", ai.ErrBadRequest, mt)
	}
	return &blob{MimeType: mt, Data: base64.StdEncoding.EncodeToString(data)}, nil
}

func textOf(parts []part) string {
	var b strings.Builder
	for _, pt := range parts {
		if !pt.Thought {
			b.WriteString(pt.Text)
		}
	}
	return b.String()
}

func mapFinishReason(r string) string {
	switch r {
	case "STOP":
		return ai.StopEndTurn
	case "MAX_TOKENS":
		return ai.StopMaxTokens
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII",
		"IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT", "IMAGE_RECITATION":
		return ai.StopRefusal
	default:
		return ai.StopOther
	}
}

// ---------------------------------------------------------------------------
// Images

// Image implements ai.Provider. "imagen-*" models use :predict; other
// models are treated as Gemini image models (generateContent with
// responseModalities IMAGE).
func (p *Provider) Image(ctx context.Context, req ai.ImageRequest) (*ai.ImageResponse, error) {
	if req.Model == "" || strings.TrimSpace(req.Prompt) == "" {
		return nil, fmt.Errorf("%w: model and prompt are required", ai.ErrBadRequest)
	}
	n := max(req.N, 1)
	if strings.HasPrefix(normalizeModel(req.Model), "imagen") {
		return p.imagen(ctx, req, n)
	}
	return p.geminiImage(ctx, req, n)
}

type imagenRequest struct {
	Instances  []map[string]string `json:"instances"`
	Parameters map[string]any      `json:"parameters"`
}

type imagenResponse struct {
	Predictions []struct {
		BytesBase64Encoded string `json:"bytesBase64Encoded"`
		MimeType           string `json:"mimeType"`
		RAIFilteredReason  string `json:"raiFilteredReason"`
	} `json:"predictions"`
}

func (p *Provider) imagen(ctx context.Context, req ai.ImageRequest, n int) (*ai.ImageResponse, error) {
	params := map[string]any{"sampleCount": n}
	if ar := aspectRatio(req.Size, imagenRatios); ar != "" {
		params["aspectRatio"] = ar
	}
	body := imagenRequest{Instances: []map[string]string{{"prompt": req.Prompt}}, Parameters: params}
	var ir imagenResponse
	if err := p.postJSON(ctx, req.Model, "predict", body, &ir); err != nil {
		return nil, err
	}
	out := &ai.ImageResponse{}
	filtered := ""
	for _, pr := range ir.Predictions {
		if pr.BytesBase64Encoded == "" {
			filtered = firstNonEmpty(pr.RAIFilteredReason, filtered)
			continue
		}
		data, err := base64.StdEncoding.DecodeString(pr.BytesBase64Encoded)
		if err != nil {
			return nil, &ai.UpstreamError{Provider: p.name, Message: "invalid base64 image"}
		}
		out.Images = append(out.Images, ai.Image{Data: data, MediaType: imageType(pr.MimeType, data)})
	}
	if len(out.Images) == 0 {
		msg := "no images returned"
		if filtered != "" {
			msg += ": " + filtered
		}
		return nil, &ai.UpstreamError{Provider: p.name, Message: msg}
	}
	return out, nil
}

func (p *Provider) geminiImage(ctx context.Context, req ai.ImageRequest, n int) (*ai.ImageResponse, error) {
	modalities := []string{"IMAGE"}
	out := &ai.ImageResponse{}
	for len(out.Images) < n {
		gc := &generationConfig{ResponseModalities: modalities}
		if ar := aspectRatio(req.Size, geminiRatios); ar != "" {
			gc.ImageConfig = &imageConfig{AspectRatio: ar}
		}
		body := generateRequest{
			Contents:         []content{{Role: "user", Parts: []part{{Text: req.Prompt}}}},
			GenerationConfig: gc,
		}
		var gr generateResponse
		if err := p.postJSON(ctx, req.Model, "generateContent", body, &gr); err != nil {
			// Older image models only accept TEXT+IMAGE output.
			var ue *ai.UpstreamError
			if len(modalities) == 1 && errors.As(err, &ue) && ue.Status == http.StatusBadRequest &&
				strings.Contains(strings.ToLower(ue.Message), "modalit") {
				modalities = []string{"TEXT", "IMAGE"}
				continue
			}
			return nil, err
		}
		got := 0
		for _, c := range gr.Candidates {
			for _, pt := range c.Content.Parts {
				if pt.InlineData == nil || !strings.HasPrefix(pt.InlineData.MimeType, "image/") {
					continue
				}
				data, err := base64.StdEncoding.DecodeString(pt.InlineData.Data)
				if err != nil {
					return nil, &ai.UpstreamError{Provider: p.name, Message: "invalid base64 image"}
				}
				out.Images = append(out.Images, ai.Image{Data: data, MediaType: imageType(pt.InlineData.MimeType, data)})
				got++
			}
		}
		if got == 0 {
			reason := ""
			if len(gr.Candidates) > 0 {
				reason = gr.Candidates[0].FinishReason
			} else if gr.PromptFeedback != nil {
				reason = gr.PromptFeedback.BlockReason
			}
			msg := "no image returned"
			if reason != "" {
				msg += " (" + reason + ")"
			}
			return nil, &ai.UpstreamError{Provider: p.name, Message: msg}
		}
	}
	if len(out.Images) > n {
		out.Images = out.Images[:n]
	}
	return out, nil
}

var (
	imagenRatios = []string{"1:1", "3:4", "4:3", "9:16", "16:9"}
	geminiRatios = []string{"1:1", "2:3", "3:2", "3:4", "4:3", "4:5", "5:4", "9:16", "16:9", "21:9"}
)

// aspectRatio converts a size ("1024x1024", "1792x1024") to the closest
// supported aspect ratio; a size already in "W:H" form is passed through.
func aspectRatio(size string, supported []string) string {
	size = strings.TrimSpace(strings.ToLower(size))
	if size == "" || size == "auto" {
		return ""
	}
	if strings.Contains(size, ":") {
		return size
	}
	ws, hs, ok := strings.Cut(size, "x")
	if !ok {
		return ""
	}
	w, err1 := strconv.Atoi(strings.TrimSpace(ws))
	h, err2 := strconv.Atoi(strings.TrimSpace(hs))
	if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return ""
	}
	target := math.Log(float64(w) / float64(h))
	best, bestDiff := "", math.Inf(1)
	for _, r := range supported {
		a, b, _ := strings.Cut(r, ":")
		x, _ := strconv.Atoi(a)
		y, _ := strconv.Atoi(b)
		if d := math.Abs(math.Log(float64(x)/float64(y)) - target); d < bestDiff {
			best, bestDiff = r, d
		}
	}
	return best
}

func imageType(declared string, data []byte) string {
	if strings.HasPrefix(declared, "image/") {
		return declared
	}
	mt := http.DetectContentType(data)
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = mt[:i]
	}
	if strings.HasPrefix(mt, "image/") {
		return mt
	}
	return "image/png"
}

// ---------------------------------------------------------------------------
// Embeddings

type batchEmbedRequest struct {
	Requests []embedContentRequest `json:"requests"`
}

type embedContentRequest struct {
	Model   string  `json:"model"`
	Content content `json:"content"`
}

type batchEmbedResponse struct {
	Embeddings []struct {
		Values []float64 `json:"values"`
	} `json:"embeddings"`
}

type predictEmbedRequest struct {
	Instances []map[string]string `json:"instances"`
}

type predictEmbedResponse struct {
	Predictions []struct {
		Embeddings struct {
			Values     []float64 `json:"values"`
			Statistics struct {
				TokenCount float64 `json:"token_count"`
			} `json:"statistics"`
		} `json:"embeddings"`
	} `json:"predictions"`
}

// Embed implements ai.Provider.
func (p *Provider) Embed(ctx context.Context, req ai.EmbedRequest) (*ai.EmbedResponse, error) {
	if req.Model == "" || len(req.Input) == 0 {
		return nil, fmt.Errorf("%w: model and input are required", ai.ErrBadRequest)
	}
	out := &ai.EmbedResponse{Embeddings: make([][]float64, 0, len(req.Input))}
	batch := aiStudioEmbedBatch
	if p.vertex {
		batch = vertexEmbedBatch
		if strings.HasPrefix(normalizeModel(req.Model), "gemini-embedding") {
			batch = 1 // Vertex gemini-embedding models take one instance per request
		}
	}
	for start := 0; start < len(req.Input); start += batch {
		chunk := req.Input[start:min(start+batch, len(req.Input))]
		vecs, tokens, err := p.embedChunk(ctx, req.Model, chunk)
		if err != nil {
			return nil, err
		}
		if len(vecs) != len(chunk) {
			return nil, &ai.UpstreamError{Provider: p.name,
				Message: fmt.Sprintf("got %d embeddings for %d inputs", len(vecs), len(chunk))}
		}
		out.Embeddings = append(out.Embeddings, vecs...)
		out.Usage.InputTokens += tokens
	}
	return out, nil
}

func (p *Provider) embedChunk(ctx context.Context, model string, input []string) ([][]float64, int, error) {
	if p.vertex {
		body := predictEmbedRequest{Instances: make([]map[string]string, len(input))}
		for i, s := range input {
			body.Instances[i] = map[string]string{"content": s}
		}
		var pr predictEmbedResponse
		if err := p.postJSON(ctx, model, "predict", body, &pr); err != nil {
			return nil, 0, err
		}
		vecs := make([][]float64, len(pr.Predictions))
		tokens := 0
		for i, pred := range pr.Predictions {
			vecs[i] = pred.Embeddings.Values
			tokens += int(pred.Embeddings.Statistics.TokenCount)
		}
		return vecs, tokens, nil
	}
	m := "models/" + normalizeModel(model)
	body := batchEmbedRequest{Requests: make([]embedContentRequest, len(input))}
	for i, s := range input {
		body.Requests[i] = embedContentRequest{Model: m, Content: content{Parts: []part{{Text: s}}}}
	}
	var br batchEmbedResponse
	if err := p.postJSON(ctx, model, "batchEmbedContents", body, &br); err != nil {
		return nil, 0, err
	}
	vecs := make([][]float64, len(br.Embeddings))
	for i, e := range br.Embeddings {
		vecs[i] = e.Values
	}
	return vecs, 0, nil
}

// ---------------------------------------------------------------------------
// Transport

// normalizeModel strips resource prefixes from a model id.
func normalizeModel(model string) string {
	m := strings.TrimSpace(model)
	if i := strings.LastIndex(m, "models/"); i >= 0 {
		m = m[i+len("models/"):]
	}
	return m
}

// modelURL returns the method URL for a model, e.g.
// .../models/gemini-2.5-flash:generateContent. project is only used for
// Vertex AI (non-express).
func (p *Provider) modelURL(project, model, method, query string) string {
	m := url.PathEscape(normalizeModel(model))
	var u string
	switch {
	case !p.vertex:
		u = p.baseURL + "/models/" + m + ":" + method
	case p.express:
		u = p.baseURL + "/publishers/google/models/" + m + ":" + method
	default:
		u = p.baseURL + "/projects/" + url.PathEscape(project) + "/locations/" + url.PathEscape(p.location) +
			"/publishers/google/models/" + m + ":" + method
	}
	if query != "" {
		u += "?" + query
	}
	return u
}

// newRequest builds an authenticated POST request for a model method.
func (p *Provider) newRequest(ctx context.Context, model, method, query string, body any) (*http.Request, error) {
	var (
		token   string
		project string
	)
	if p.vertex && !p.express {
		var err error
		token, project, err = p.credentials(ctx)
		if err != nil {
			return nil, err
		}
	}
	req, err := httpx.NewJSONRequest(ctx, p.modelURL(project, model, method, query), body)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	} else {
		req.Header.Set("x-goog-api-key", p.apiKey)
	}
	if strings.Contains(query, "alt=sse") {
		req.Header.Set("Accept", "text/event-stream")
	}
	for k, v := range p.headers {
		req.Header.Set(k, v)
	}
	return req, nil
}

// post sends a request whose response is streamed; the caller closes the
// body. Non-2xx responses are returned as *ai.UpstreamError.
func (p *Provider) post(ctx context.Context, model, method, query string, body any) (*http.Response, error) {
	req, err := p.newRequest(ctx, model, method, query, body)
	if err != nil {
		return nil, err
	}
	return httpx.Do(p.client, p.name, req)
}

// postJSON sends a non-streaming request and decodes the JSON response into
// out, always closing the response body.
func (p *Provider) postJSON(ctx context.Context, model, method string, body, out any) error {
	req, err := p.newRequest(ctx, model, method, "", body)
	if err != nil {
		return err
	}
	return httpx.DoJSON(p.client, p.name, req, out)
}

// credentials returns a Vertex AI access token and the project id,
// discovering Application Default Credentials on first use.
func (p *Provider) credentials(ctx context.Context) (token, project string, err error) {
	p.mu.Lock()
	if p.ts == nil {
		// The token source keeps the context for refreshes; detach it.
		creds, ferr := google.FindDefaultCredentials(context.WithoutCancel(ctx), cloudPlatformScope)
		if ferr != nil {
			p.mu.Unlock()
			p.log.LogAttrs(ctx, slog.LevelError, "ai: cannot find Google application default credentials",
				slog.String("provider", p.name), slog.Any("error", ferr))
			return "", "", &ai.UpstreamError{Provider: p.name, Message: "Google application default credentials not found (see server logs)"}
		}
		p.ts = oauth2.ReuseTokenSource(nil, creds.TokenSource)
		if p.project == "" {
			p.project = creds.ProjectID
		}
	}
	ts, project := p.ts, p.project
	p.mu.Unlock()
	if project == "" {
		return "", "", &ai.UpstreamError{Provider: p.name, Message: "project_id is required for Vertex AI (none found in credentials)"}
	}
	tok, err := ts.Token()
	if err != nil {
		// Token endpoint errors can carry response bodies and identity
		// details; keep them in the server log.
		p.log.LogAttrs(ctx, slog.LevelError, "ai: cannot get Google access token",
			slog.String("provider", p.name), slog.Any("error", err))
		return "", "", &ai.UpstreamError{Provider: p.name, Status: http.StatusUnauthorized,
			Message: "Google authentication failed (see server logs)"}
	}
	return tok.AccessToken, project, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
