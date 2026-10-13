// Package router maps client-facing model aliases to configured providers and
// implements ai.Service.
//
// Clients choose aliases from ai.models ("smart", "fast", "image", "embed");
// an empty alias selects the default of the requested kind
// (ai.default_model, ai.default_image_model, ai.default_embedding_model).
// "provider/model-id" is accepted only when ai.allow_raw_models is true (and
// the model id must look like one: no empty, "." or ".." path segments, no
// query or whitespace characters). The router validates and normalises
// requests (role:"system" messages are hoisted into the system prompt,
// consecutive same-role turns are merged, images are checked), applies the
// max_tokens caps, prepends ai.system_prompt, applies per-alias effort, bounds
// every upstream call with ai.request_timeout (streaming and image
// generation: at least 10 minutes) and reports the alias as the response
// model.
//
// Limits on max_tokens: the cap is ai.max_tokens, lowered by a per-alias
// max_tokens when that is smaller (an alias cannot exceed the global cap;
// raise ai.max_tokens for long-output aliases and give the others their own
// lower caps); a request without max_tokens gets min(4096, cap). An alias's
// effort overrides the request's. When an upstream reports no token usage,
// the router fills in an estimate (about four bytes per token, a flat cost
// per image) so per-user token limits cannot be bypassed.
//
// Untrusted input is contained: image URLs that a provider needs as inline
// bytes are downloaded by an SSRF-guarded fetcher within a per-request byte
// budget (MaxRemoteImageBytes); end-user ids are replaced by keyed opaque ids
// (Options.UserIDKey) before they reach a provider; generated images get
// their media type from their content, and non-raster formats are refused.
package router

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/thasami/backyard/internal/ai"
	"github.com/thasami/backyard/internal/ai/anthropic"
	"github.com/thasami/backyard/internal/ai/bedrock"
	"github.com/thasami/backyard/internal/ai/gemini"
	"github.com/thasami/backyard/internal/ai/internal/imagefetch"
	"github.com/thasami/backyard/internal/ai/openai"
	"github.com/thasami/backyard/internal/config"
)

// Model kinds.
const (
	KindChat      = "chat"
	KindImage     = "image"
	KindEmbedding = "embedding"
)

// Request limits enforced before calling a provider.
const (
	// MaxImageBytes is the largest inline image accepted in a chat message.
	MaxImageBytes = 20 << 20
	// MaxImagesPerRequest bounds the number of images in one chat request.
	MaxImagesPerRequest = 50
	// MaxImageCount bounds ImageRequest.N.
	MaxImageCount = 10
	// MaxImagePromptChars bounds ImageRequest.Prompt.
	MaxImagePromptChars = 32000
	// MaxEmbedInputs bounds the number of strings in one embedding request.
	MaxEmbedInputs = 2048
	// MaxEmbedBytes bounds the total size of an embedding request's input.
	MaxEmbedBytes = 8 << 20
	// MaxRemoteImageBytes bounds the total size of the image URLs one chat
	// request makes Backyard download (for providers that need inline
	// bytes: Bedrock, Gemini, Claude on Vertex AI/Bedrock, Ollama and other
	// self-hosted OpenAI-compatible servers).
	MaxRemoteImageBytes = 64 << 20

	defaultMaxTokens      = 8192
	defaultResponseTokens = 4096
	defaultTimeout        = 120 * time.Second
	minLongTimeout        = 10 * time.Minute
	maxURLLen             = 8192
)

// Options are optional dependencies.
type Options struct {
	HTTPClient *http.Client // used by HTTP-based providers (tests inject fakes)
	Logger     *slog.Logger
	// Providers are prebuilt providers by key. They take precedence over
	// building a provider from cfg.Providers and may also add keys that are
	// not in cfg.Providers (tests, embedding Backyard with custom providers).
	Providers map[string]ai.Provider
	// UserIDKey keys the hash that turns end-user ids (emails) into the
	// opaque ids sent to providers for abuse monitoring (ai.KeyedUserID).
	// Pass the server secret so providers cannot confirm a guessed email;
	// when empty an unkeyed hash (ai.OpaqueUserID) is used.
	UserIDKey []byte
}

// route is a resolved alias.
type route struct {
	alias       string
	provider    ai.Provider
	providerKey string
	model       string
	kind        string
	maxTokens   int
	effort      string
}

// Router implements ai.Service. It is immutable after New and safe for
// concurrent use.
type Router struct {
	providers    map[string]ai.Provider
	routes       map[string]route
	defaults     map[string]string // kind -> alias
	infos        []ai.ModelInfo
	allowRaw     bool
	maxTokens    int
	systemPrompt string
	timeout      time.Duration
	longTimeout  time.Duration
	userKey      []byte
	log          *slog.Logger
}

var _ ai.Service = (*Router)(nil)

// New builds providers from cfg.Providers and validates cfg.Models.
// When AI is disabled it returns a Service whose Enabled() is false and whose
// methods return ai.ErrDisabled.
//
// Only providers referenced by a model alias are built (all of them when
// allow_raw_models is set; failures of unreferenced providers are then logged
// and the provider is skipped). Construction does no network I/O: cloud
// credentials (Vertex AI, Bedrock Mantle, Entra ID) are discovered on first
// use.
func New(ctx context.Context, cfg config.AIConfig, opts Options) (ai.Service, error) {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	if cfg.Enabled != nil && !*cfg.Enabled {
		return disabled{}, nil
	}
	if len(cfg.Models) == 0 && !(cfg.AllowRawModels && len(cfg.Providers)+len(opts.Providers) > 0) {
		return disabled{}, nil
	}

	r := &Router{
		providers:    map[string]ai.Provider{},
		routes:       map[string]route{},
		defaults:     map[string]string{},
		allowRaw:     cfg.AllowRawModels,
		maxTokens:    cfg.MaxTokens,
		systemPrompt: strings.TrimSpace(cfg.SystemPrompt),
		timeout:      cfg.RequestTimeout.D(),
		userKey:      append([]byte(nil), opts.UserIDKey...),
		log:          log,
	}
	if r.maxTokens <= 0 {
		r.maxTokens = defaultMaxTokens
	}
	if r.timeout <= 0 {
		r.timeout = defaultTimeout
	}
	r.longTimeout = max(r.timeout, minLongTimeout)

	// Which providers are needed?
	referenced := map[string]bool{}
	for _, m := range cfg.Models {
		referenced[m.Provider] = true
	}
	keys := make([]string, 0, len(cfg.Providers)+len(opts.Providers))
	for k := range cfg.Providers {
		keys = append(keys, k)
	}
	for k := range opts.Providers {
		if _, dup := cfg.Providers[k]; !dup {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	fetch := imagefetch.New().Fetch // shared SSRF-guarded image downloader
	for _, key := range keys {
		if p, ok := opts.Providers[key]; ok && p != nil {
			r.providers[key] = p
			continue
		}
		if !referenced[key] && !cfg.AllowRawModels {
			continue
		}
		p, err := buildProvider(ctx, key, cfg.Providers[key], opts, log, fetch)
		if err != nil {
			if !referenced[key] {
				log.Warn("ai: skipping provider that failed to initialise", slog.String("provider", key), slog.Any("error", err))
				continue
			}
			return nil, err
		}
		r.providers[key] = p
	}

	// Aliases.
	aliases := make([]string, 0, len(cfg.Models))
	for alias := range cfg.Models {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	for _, alias := range aliases {
		m := cfg.Models[alias]
		kind := m.Kind
		if kind == "" {
			kind = KindChat
		}
		switch {
		case strings.TrimSpace(alias) == "" || strings.Contains(alias, "/"):
			return nil, fmt.Errorf("ai: invalid model alias %q (must be non-empty and contain no '/')", alias)
		case kind != KindChat && kind != KindImage && kind != KindEmbedding:
			return nil, fmt.Errorf("ai: model %q: kind must be chat, image or embedding; got %q", alias, m.Kind)
		case m.Model == "":
			return nil, fmt.Errorf("ai: model %q: model id is required", alias)
		case m.MaxTokens < 0:
			return nil, fmt.Errorf("ai: model %q: max_tokens must not be negative", alias)
		case m.Effort != "" && !ai.ValidEffort(m.Effort):
			return nil, fmt.Errorf("ai: model %q: invalid effort %q (want low, medium, high, xhigh or max)", alias, m.Effort)
		}
		p, ok := r.providers[m.Provider]
		if !ok {
			return nil, fmt.Errorf("ai: model %q: provider %q is not defined in ai.providers", alias, m.Provider)
		}
		r.routes[alias] = route{
			alias: alias, provider: p, providerKey: m.Provider, model: m.Model, kind: kind,
			maxTokens: m.MaxTokens, effort: m.Effort,
		}
		info := ai.ModelInfo{Name: alias, Kind: kind, Provider: m.Provider, Description: m.Description}
		if cfg.ExposeModelIDs {
			info.Model = m.Model
		}
		r.infos = append(r.infos, info)
	}

	// Defaults per kind.
	for kind, configured := range map[string]string{
		KindChat: cfg.DefaultModel, KindImage: cfg.DefaultImageModel, KindEmbedding: cfg.DefaultEmbeddingModel,
	} {
		alias := configured
		if alias == "" {
			alias = r.firstOfKind(kind)
		}
		if alias == "" {
			continue
		}
		rt, ok := r.routes[alias]
		if !ok {
			return nil, fmt.Errorf("ai: default %s model %q is not defined in ai.models", kind, alias)
		}
		if rt.kind != kind {
			return nil, fmt.Errorf("ai: default %s model %q is a %s model", kind, alias, rt.kind)
		}
		r.defaults[kind] = alias
	}

	log.Info("ai: router ready", slog.Int("models", len(r.routes)), slog.Int("providers", len(r.providers)),
		slog.String("default", r.defaults[KindChat]), slog.Bool("raw_models", r.allowRaw))
	return r, nil
}

// buildProvider constructs a provider from its config.
func buildProvider(ctx context.Context, key string, pc config.ProviderConfig, opts Options, log *slog.Logger, fetch imagefetch.Func) (ai.Provider, error) {
	typ := pc.Type
	if typ == "" {
		typ = key
	}
	pc.Type = typ
	var (
		p   ai.Provider
		err error
	)
	switch typ {
	case "anthropic":
		p, err = anthropic.New(ctx, key, pc, anthropic.Options{HTTPClient: opts.HTTPClient, Logger: log, FetchImage: fetch})
	case openai.TypeOpenAI, openai.TypeOllama, openai.TypeAzureOpenAI:
		p, err = openai.New(key, pc, openai.Options{HTTPClient: opts.HTTPClient, Logger: log, FetchImage: fetch})
	case "bedrock":
		p, err = bedrock.New(ctx, key, pc, bedrock.Options{HTTPClient: opts.HTTPClient, Logger: log, FetchImage: fetch})
	case "gemini":
		p, err = gemini.New(key, pc, gemini.Options{HTTPClient: opts.HTTPClient, Logger: log, FetchImage: fetch})
	default:
		return nil, fmt.Errorf("ai: provider %q: unknown type %q", key, typ)
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

// firstOfKind picks a default alias: well-known names first, then the
// lexically first alias of that kind (mirrors config defaults).
func (r *Router) firstOfKind(kind string) string {
	pref := map[string][]string{
		KindChat:      {"default", "smart", "chat", "fast"},
		KindImage:     {"image", "images"},
		KindEmbedding: {"embed", "embedding", "embeddings"},
	}[kind]
	for _, name := range pref {
		if rt, ok := r.routes[name]; ok && rt.kind == kind {
			return name
		}
	}
	for _, info := range r.infos { // sorted by name
		if info.Kind == kind {
			return info.Name
		}
	}
	return ""
}

// resolve maps an alias (or "" or "provider/model") to a route of kind.
func (r *Router) resolve(alias, kind string) (route, error) {
	name := strings.TrimSpace(alias)
	if name == "" {
		name = r.defaults[kind]
		if name == "" {
			return route{}, fmt.Errorf("%w: no default %s model is configured", ai.ErrUnknownModel, kind)
		}
	}
	if rt, ok := r.routes[name]; ok {
		if rt.kind != kind {
			return route{}, fmt.Errorf("%w: model %q is a %s model, not a %s model", ai.ErrBadRequest, name, rt.kind, kind)
		}
		return rt, nil
	}
	if key, model, ok := strings.Cut(name, "/"); ok && r.allowRaw {
		p, found := r.providers[key]
		if found && strings.TrimSpace(model) != "" {
			if !validRawModel(model) {
				return route{}, fmt.Errorf("%w: invalid model id %q", ai.ErrBadRequest, model)
			}
			return route{alias: name, provider: p, providerKey: key, model: model, kind: kind}, nil
		}
	}
	return route{}, fmt.Errorf("%w: %q", ai.ErrUnknownModel, name)
}

// maxRawModelLen bounds a client-chosen raw model id.
const maxRawModelLen = 256

// rawModelRE lists the characters provider model ids use: "gpt-5",
// "claude-opus-4-5@20251101", "llama3.2:3b", "@cf/meta/llama-3.1-8b-instruct",
// "meta-llama/Llama-3.1-8B-Instruct", Bedrock ARNs and inference profiles.
var rawModelRE = regexp.MustCompile(`^[A-Za-z0-9@][A-Za-z0-9._:@/+-]*$`)

// validRawModel reports whether a client-chosen model id is safe to hand to
// a provider. Some upstream paths embed the model id (Vertex AI, Azure
// deployments, Workers AI), so path segments must not be empty, "." or "..".
func validRawModel(model string) bool {
	if len(model) > maxRawModelLen || !rawModelRE.MatchString(model) {
		return false
	}
	for seg := range strings.SplitSeq(model, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// Enabled implements ai.Service.
func (r *Router) Enabled() bool { return true }

// Models implements ai.Service.
func (r *Router) Models() (string, []ai.ModelInfo) {
	out := make([]ai.ModelInfo, len(r.infos))
	copy(out, r.infos)
	return r.defaults[KindChat], out
}

// Chat implements ai.Service.
func (r *Router) Chat(ctx context.Context, alias string, req ai.ChatRequest) (*ai.ChatResponse, error) {
	rt, err := r.resolve(alias, KindChat)
	if err != nil {
		return nil, err
	}
	preq, err := r.prepareChat(rt, req)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	cctx = imagefetch.WithBudget(cctx, MaxRemoteImageBytes)
	start := time.Now()
	resp, err := rt.provider.Chat(cctx, preq)
	return r.finishChat(ctx, rt, "chat", start, r.timeout, preq, resp, err)
}

// ChatStream implements ai.Service.
func (r *Router) ChatStream(ctx context.Context, alias string, req ai.ChatRequest, onDelta func(string) error) (*ai.ChatResponse, error) {
	rt, err := r.resolve(alias, KindChat)
	if err != nil {
		return nil, err
	}
	preq, err := r.prepareChat(rt, req)
	if err != nil {
		return nil, err
	}
	if onDelta == nil {
		onDelta = func(string) error { return nil }
	}
	cctx, cancel := context.WithTimeout(ctx, r.longTimeout)
	defer cancel()
	cctx = imagefetch.WithBudget(cctx, MaxRemoteImageBytes)
	start := time.Now()
	resp, err := rt.provider.ChatStream(cctx, preq, onDelta)
	return r.finishChat(ctx, rt, "chat_stream", start, r.longTimeout, preq, resp, err)
}

func (r *Router) finishChat(ctx context.Context, rt route, op string, start time.Time, timeout time.Duration,
	req ai.ChatRequest, resp *ai.ChatResponse, err error) (*ai.ChatResponse, error) {
	if err == nil && resp == nil {
		err = &ai.UpstreamError{Provider: rt.providerKey, Message: "empty response"}
	}
	if err != nil {
		return nil, r.upstreamErr(ctx, rt, op, start, timeout, err)
	}
	if req.JSON {
		resp.Content = ai.StripCodeFences(resp.Content)
	}
	if resp.StopReason == "" {
		resp.StopReason = ai.StopOther
	}
	if resp.Usage == (ai.Usage{}) {
		// Some upstreams report no usage (compatible servers without
		// stream_options support, for instance). Estimate it so per-user
		// token limits and metrics cannot be bypassed.
		resp.Usage = estimateChatUsage(req, resp.Content)
	}
	r.log.Debug("ai: request completed", slog.String("op", op), slog.String("alias", rt.alias),
		slog.String("provider", rt.providerKey), slog.String("model", rt.model),
		slog.Duration("duration", time.Since(start)), slog.String("stop_reason", resp.StopReason),
		slog.Int("input_tokens", resp.Usage.InputTokens), slog.Int("output_tokens", resp.Usage.OutputTokens))
	resp.Model = rt.alias
	return resp, nil
}

// estimatedImageTokens approximates the input cost of one image.
const estimatedImageTokens = 1000

// estimateTokens approximates the token count of n bytes of text (about four
// bytes per token for English; never zero for non-empty text).
func estimateTokens(n int) int {
	if n <= 0 {
		return 0
	}
	return (n + 3) / 4
}

// estimateChatUsage approximates token usage for providers that report none.
func estimateChatUsage(req ai.ChatRequest, content string) ai.Usage {
	in := len(req.System)
	images := 0
	for _, m := range req.Messages {
		for _, p := range m.Content {
			if p.Type == "image" {
				images++
				continue
			}
			in += len(p.Text)
		}
	}
	return ai.Usage{InputTokens: estimateTokens(in) + images*estimatedImageTokens, OutputTokens: estimateTokens(len(content))}
}

// upstreamErr converts our own deadline into a gateway timeout and logs
// upstream failures. Caller cancellation and client errors pass through.
func (r *Router) upstreamErr(ctx context.Context, rt route, op string, start time.Time, timeout time.Duration, err error) error {
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		err = &ai.UpstreamError{Provider: rt.providerKey, Status: http.StatusGatewayTimeout,
			Message: fmt.Sprintf("no response within %s", timeout)}
	}
	attrs := []slog.Attr{
		slog.String("op", op), slog.String("alias", rt.alias), slog.String("provider", rt.providerKey),
		slog.String("model", rt.model), slog.Duration("duration", time.Since(start)), slog.Any("error", err),
	}
	var ue *ai.UpstreamError
	switch {
	case errors.As(err, &ue) && (ue.Status == 0 || ue.Status >= 500 || ue.Status == http.StatusTooManyRequests ||
		ue.Status == http.StatusUnauthorized || ue.Status == http.StatusForbidden):
		r.log.LogAttrs(ctx, slog.LevelWarn, "ai: upstream request failed", attrs...)
	default:
		r.log.LogAttrs(ctx, slog.LevelDebug, "ai: request failed", attrs...)
	}
	return err
}

// Image implements ai.Service.
func (r *Router) Image(ctx context.Context, alias string, req ai.ImageRequest) (*ai.ImageResponse, string, error) {
	rt, err := r.resolve(alias, KindImage)
	if err != nil {
		return nil, "", err
	}
	req.Prompt = strings.TrimSpace(req.Prompt)
	switch {
	case req.Prompt == "":
		return nil, "", fmt.Errorf("%w: prompt is required", ai.ErrBadRequest)
	case len([]rune(req.Prompt)) > MaxImagePromptChars:
		return nil, "", fmt.Errorf("%w: prompt exceeds %d characters", ai.ErrBadRequest, MaxImagePromptChars)
	case req.N < 0 || req.N > MaxImageCount:
		return nil, "", fmt.Errorf("%w: n must be between 1 and %d", ai.ErrBadRequest, MaxImageCount)
	}
	if req.N == 0 {
		req.N = 1
	}
	req.Size = strings.TrimSpace(req.Size)
	if req.Size != "" && !sizeRE.MatchString(req.Size) {
		return nil, "", fmt.Errorf("%w: size must look like 1024x1024, 16:9 or auto", ai.ErrBadRequest)
	}
	req.Model = rt.model
	req.User = ai.KeyedUserID(r.userKey, req.User)

	cctx, cancel := context.WithTimeout(ctx, r.longTimeout)
	defer cancel()
	start := time.Now()
	resp, err := rt.provider.Image(cctx, req)
	if err == nil {
		err = checkImages(rt.providerKey, resp)
	}
	if err != nil {
		return nil, "", r.upstreamErr(ctx, rt, "image", start, r.longTimeout, err)
	}
	if len(resp.Images) > req.N {
		resp.Images = resp.Images[:req.N]
	}
	r.log.Debug("ai: request completed", slog.String("op", "image"), slog.String("alias", rt.alias),
		slog.String("provider", rt.providerKey), slog.String("model", rt.model),
		slog.Duration("duration", time.Since(start)), slog.Int("images", len(resp.Images)))
	return resp, rt.alias, nil
}

var sizeRE = regexp.MustCompile(`^(?i:auto|\d{1,5}\s*x\s*\d{1,5}|\d{1,3}:\d{1,3})$`)

// generatedImageTypes are the formats accepted from image models, keyed by
// the type detected from the image bytes. Generated images are stored and
// served as site files, so the media type always comes from the content (a
// declared type is not trusted) and active formats such as SVG are refused.
var generatedImageTypes = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true, "image/bmp": true, "image/avif": true,
}

func checkImages(provider string, resp *ai.ImageResponse) error {
	if resp == nil || len(resp.Images) == 0 {
		return &ai.UpstreamError{Provider: provider, Message: "no images returned"}
	}
	for i := range resp.Images {
		img := &resp.Images[i]
		if len(img.Data) == 0 {
			return &ai.UpstreamError{Provider: provider, Message: "empty image returned"}
		}
		mt := sniff(img.Data)
		if mt == "" && isAVIF(img.Data) {
			mt = "image/avif"
		}
		if !generatedImageTypes[mt] {
			return &ai.UpstreamError{Provider: provider, Message: "provider returned an unsupported image format"}
		}
		img.MediaType = mt
	}
	return nil
}

// isAVIF reports whether data starts with an ISO-BMFF "ftyp" box of an AVIF
// brand (net/http does not sniff AVIF).
func isAVIF(data []byte) bool {
	if len(data) < 12 || string(data[4:8]) != "ftyp" {
		return false
	}
	brand := string(data[8:12])
	return brand == "avif" || brand == "avis"
}

// Embed implements ai.Service.
func (r *Router) Embed(ctx context.Context, alias string, req ai.EmbedRequest) (*ai.EmbedResponse, string, error) {
	rt, err := r.resolve(alias, KindEmbedding)
	if err != nil {
		return nil, "", err
	}
	if len(req.Input) == 0 {
		return nil, "", fmt.Errorf("%w: input is required", ai.ErrBadRequest)
	}
	if len(req.Input) > MaxEmbedInputs {
		return nil, "", fmt.Errorf("%w: at most %d inputs per request", ai.ErrBadRequest, MaxEmbedInputs)
	}
	total := 0
	for i, s := range req.Input {
		if strings.TrimSpace(s) == "" {
			return nil, "", fmt.Errorf("%w: input %d is empty", ai.ErrBadRequest, i)
		}
		total += len(s)
	}
	if total > MaxEmbedBytes {
		return nil, "", fmt.Errorf("%w: input exceeds %d bytes", ai.ErrBadRequest, MaxEmbedBytes)
	}
	req.Input = slices.Clone(req.Input)
	req.Model = rt.model
	req.User = ai.KeyedUserID(r.userKey, req.User)

	cctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	start := time.Now()
	resp, err := rt.provider.Embed(cctx, req)
	if err == nil && (resp == nil || len(resp.Embeddings) != len(req.Input)) {
		got := 0
		if resp != nil {
			got = len(resp.Embeddings)
		}
		err = &ai.UpstreamError{Provider: rt.providerKey, Message: fmt.Sprintf("got %d embeddings for %d inputs", got, len(req.Input))}
	}
	if err != nil {
		return nil, "", r.upstreamErr(ctx, rt, "embed", start, r.timeout, err)
	}
	if resp.Usage.InputTokens == 0 {
		resp.Usage.InputTokens = estimateTokens(total) // e.g. Gemini batchEmbedContents reports none
	}
	r.log.Debug("ai: request completed", slog.String("op", "embed"), slog.String("alias", rt.alias),
		slog.String("provider", rt.providerKey), slog.String("model", rt.model),
		slog.Duration("duration", time.Since(start)), slog.Int("inputs", len(req.Input)))
	return resp, rt.alias, nil
}

// ---------------------------------------------------------------------------
// Request preparation

// prepareChat validates req and returns the provider-level request for rt.
// The caller's slices are never modified.
func (r *Router) prepareChat(rt route, req ai.ChatRequest) (ai.ChatRequest, error) {
	out := ai.ChatRequest{Model: rt.model, JSON: req.JSON, User: ai.KeyedUserID(r.userKey, req.User)}

	var system []string
	if r.systemPrompt != "" {
		system = append(system, r.systemPrompt)
	}
	if s := strings.TrimSpace(req.System); s != "" {
		system = append(system, s)
	}
	images := 0
	msgs := make([]ai.Message, 0, len(req.Messages))
	for i, m := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		switch role {
		case "system", "developer":
			for _, p := range m.Content {
				if t := strings.ToLower(p.Type); t != "" && t != "text" {
					return out, fmt.Errorf("%w: message %d: system messages may only contain text", ai.ErrBadRequest, i)
				}
			}
			if t := strings.TrimSpace(ai.TextOf(textTyped(m.Content))); t != "" {
				system = append(system, t)
			}
			continue
		case "user", "assistant":
		case "":
			return out, fmt.Errorf("%w: message %d: role is required", ai.ErrBadRequest, i)
		default:
			return out, fmt.Errorf("%w: message %d: role must be user, assistant or system; got %q", ai.ErrBadRequest, i, m.Role)
		}
		parts := make([]ai.Part, 0, len(m.Content))
		for j, p := range m.Content {
			np, keep, err := normalizePart(p)
			if err != nil {
				return out, fmt.Errorf("%w: message %d part %d: %v", ai.ErrBadRequest, i, j, err)
			}
			if !keep {
				continue
			}
			if np.Type == "image" {
				if role != "user" {
					return out, fmt.Errorf("%w: message %d: images are only allowed in user messages", ai.ErrBadRequest, i)
				}
				images++
			}
			parts = append(parts, np)
		}
		if blank(parts) {
			continue // empty or whitespace-only turns are dropped
		}
		if n := len(msgs); n > 0 && msgs[n-1].Role == role {
			msgs[n-1].Content = mergeParts(msgs[n-1].Content, parts)
			continue
		}
		msgs = append(msgs, ai.Message{Role: role, Content: parts})
	}
	if images > MaxImagesPerRequest {
		return out, fmt.Errorf("%w: at most %d images per request", ai.ErrBadRequest, MaxImagesPerRequest)
	}
	if len(msgs) == 0 {
		return out, fmt.Errorf("%w: messages must contain at least one non-empty user or assistant message", ai.ErrBadRequest)
	}
	out.Messages = msgs
	out.System = strings.Join(system, "\n\n")

	// SPEC §5.4: max_tokens is capped by ai.max_tokens; a per-alias
	// max_tokens can only lower the cap (same as the Cloudflare edition).
	limit := r.maxTokens
	if rt.maxTokens > 0 {
		limit = min(limit, rt.maxTokens)
	}
	switch {
	case req.MaxTokens < 0:
		return out, fmt.Errorf("%w: max_tokens must be positive", ai.ErrBadRequest)
	case req.MaxTokens == 0:
		out.MaxTokens = min(defaultResponseTokens, limit)
	default:
		out.MaxTokens = min(req.MaxTokens, limit)
	}

	if req.Temperature != nil {
		t := *req.Temperature
		if math.IsNaN(t) || t < 0 || t > 2 {
			return out, fmt.Errorf("%w: temperature must be between 0 and 2", ai.ErrBadRequest)
		}
		out.Temperature = &t
	}

	switch {
	case rt.effort != "":
		out.Effort = rt.effort
	case req.Effort != "":
		e := strings.ToLower(strings.TrimSpace(req.Effort))
		if !ai.ValidEffort(e) {
			return out, fmt.Errorf("%w: effort must be low, medium, high, xhigh or max", ai.ErrBadRequest)
		}
		out.Effort = e
	}
	return out, nil
}

// blank reports whether parts carry no image and only whitespace text
// (Anthropic rejects whitespace-only text blocks).
func blank(parts []ai.Part) bool {
	for _, p := range parts {
		if p.Type != "text" || strings.TrimSpace(p.Text) != "" {
			return false
		}
	}
	return true
}

// textTyped returns the text parts of a system message (Type "" counts as text).
func textTyped(parts []ai.Part) []ai.Part {
	out := make([]ai.Part, 0, len(parts))
	for _, p := range parts {
		out = append(out, ai.Part{Type: "text", Text: p.Text})
	}
	return out
}

// mergeParts joins two consecutive turns of the same role. Adjacent text is
// joined with a blank line so providers that flatten text keep the turns
// apart.
func mergeParts(prev, next []ai.Part) []ai.Part {
	out := slices.Clone(prev)
	if len(out) > 0 && len(next) > 0 && out[len(out)-1].Type == "text" && next[0].Type == "text" {
		out[len(out)-1].Text += "\n\n" + next[0].Text
		next = next[1:]
	}
	return append(out, next...)
}

var allowedImageTypes = map[string]string{
	"image/png":   "image/png",
	"image/jpeg":  "image/jpeg",
	"image/jpg":   "image/jpeg",
	"image/pjpeg": "image/jpeg",
	"image/gif":   "image/gif",
	"image/webp":  "image/webp",
}

// normalizePart validates one content part. keep is false for parts that
// carry nothing (empty text).
func normalizePart(p ai.Part) (ai.Part, bool, error) {
	switch strings.ToLower(strings.TrimSpace(p.Type)) {
	case "text", "":
		if p.Type == "" && (len(p.Data) > 0 || p.URL != "") {
			return ai.Part{}, false, errors.New("content part type is required")
		}
		if p.Text == "" {
			return ai.Part{}, false, nil
		}
		return ai.Part{Type: "text", Text: p.Text}, true, nil
	case "image", "image_url":
		data, mt := p.Data, p.MediaType
		u := strings.TrimSpace(p.URL)
		if len(data) == 0 && strings.HasPrefix(u, "data:") {
			d, m, ok := ai.ParseDataURL(u)
			if !ok {
				return ai.Part{}, false, errors.New("invalid image data URL")
			}
			data, u = d, ""
			if mt == "" {
				mt = m
			}
		}
		if len(data) > 0 {
			if len(data) > MaxImageBytes {
				return ai.Part{}, false, fmt.Errorf("image exceeds %d MiB", MaxImageBytes>>20)
			}
			mt = strings.ToLower(strings.TrimSpace(mt))
			if i := strings.IndexByte(mt, ';'); i >= 0 {
				mt = strings.TrimSpace(mt[:i])
			}
			if mt == "" {
				mt = sniff(data)
			}
			norm, ok := allowedImageTypes[mt]
			if !ok {
				return ai.Part{}, false, fmt.Errorf("unsupported image type %q (png, jpeg, gif or webp)", mt)
			}
			return ai.Part{Type: "image", Data: data, MediaType: norm}, true, nil
		}
		if u == "" {
			return ai.Part{}, false, errors.New("image needs data or url")
		}
		parsed, err := url.Parse(u)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || len(u) > maxURLLen {
			return ai.Part{}, false, errors.New("image url must be an absolute http(s) URL")
		}
		out := ai.Part{Type: "image", URL: u}
		if mt != "" {
			if norm, ok := allowedImageTypes[strings.ToLower(mt)]; ok {
				out.MediaType = norm
			}
		}
		return out, true, nil
	default:
		return ai.Part{}, false, fmt.Errorf("unsupported content type %q", p.Type)
	}
}

func sniff(data []byte) string {
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
// Disabled service

// disabled is the Service returned when AI is not configured.
type disabled struct{}

func (disabled) Chat(context.Context, string, ai.ChatRequest) (*ai.ChatResponse, error) {
	return nil, ai.ErrDisabled
}

func (disabled) ChatStream(context.Context, string, ai.ChatRequest, func(string) error) (*ai.ChatResponse, error) {
	return nil, ai.ErrDisabled
}

func (disabled) Image(context.Context, string, ai.ImageRequest) (*ai.ImageResponse, string, error) {
	return nil, "", ai.ErrDisabled
}

func (disabled) Embed(context.Context, string, ai.EmbedRequest) (*ai.EmbedResponse, string, error) {
	return nil, "", ai.ErrDisabled
}

func (disabled) Models() (string, []ai.ModelInfo) { return "", []ai.ModelInfo{} }

func (disabled) Enabled() bool { return false }
