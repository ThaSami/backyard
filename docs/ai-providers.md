# AI providers

Every site can call `backyard.ai.chat`, `stream`, `image` and `embed` without holding a key:
the server proxies the request to a provider with **its** credentials, enforces per-user rate
limits and token budgets, and never exposes the key to the browser.

The configuration has two layers:

- **`ai.providers`** — upstream accounts (an Anthropic key, a Bedrock region, a Vertex
  project…);
- **`ai.models`** — **aliases** that pages use (`smart`, `fast`, `image`, `embed`), each
  pointing at a provider and a model id.

Pages only ever name aliases, so you can switch vendors or upgrade models without touching any
site. Raw `"<provider>/<model-id>"` names are accepted only with `ai.allow_raw_models: true`.

```yaml
ai:
  default_model: smart
  providers:
    anthropic:
      type: anthropic
      api_key: ${ANTHROPIC_API_KEY}
    openai:
      type: openai
      api_key: ${OPENAI_API_KEY}
  models:
    smart: { provider: anthropic, model: claude-opus-5-5,  description: Most capable model for complex tasks }
    fast:  { provider: anthropic, model: claude-haiku-5-5, description: Fast and inexpensive }
    image: { provider: openai, model: gpt-image-1, kind: image }
    embed: { provider: openai, model: text-embedding-3-small, kind: embedding }
```

This is the default of `backyard config example`.

## Aliases

| Field | Meaning |
|---|---|
| `provider` | Key of `ai.providers`. |
| `model` | Provider model id (or deployment name on Azure OpenAI). |
| `kind` | `chat` (default), `image` or `embedding` — which endpoint the alias serves. |
| `description` | Shown to pages by `backyard.ai.models()` and in the hub. |
| `max_tokens` | Per-alias output cap, lower than `ai.max_tokens`. |
| `effort` | Claude effort (`low`, `medium`, `high`, `xhigh`, `max`) for this alias. |

Defaults when a request names no model: `ai.default_model` (else the first chat alias,
preferring `default`, `smart`, `chat`, `fast`), `ai.default_image_model` (`image`…) and
`ai.default_embedding_model` (`embed`…). `GET /__backyard/api/ai/models` lists the aliases with
their kind and provider; provider model ids are hidden unless `ai.expose_model_ids: true`.

Global settings: `ai.max_tokens` (8192; requests without `max_tokens` get min(4096, cap)),
`ai.request_timeout` (120 s; streams and image generation get at least 10 minutes),
`ai.system_prompt` (prepended to every system prompt), `ai.enabled`. Per-user limits live in
`ratelimit.ai_per_minute` (20) and `ratelimit.ai_tokens_per_day` (2,000,000).

## Capabilities

| Provider type | Chat + streaming | Image input | Image generation | Embeddings |
|---|---|---|---|---|
| `anthropic` (API, Vertex AI, Bedrock) | yes | yes | — | — |
| `openai` (OpenAI and compatible servers) | yes | yes | yes | yes |
| `ollama` | yes | yes (model permitting) | — | yes |
| `azure-openai` | yes | yes | yes | yes |
| `bedrock` (Converse API) | yes (any text model) | yes | Nova Canvas, Titan Image, Stability | Titan Text Embeddings, Cohere Embed |
| `gemini` (AI Studio, Vertex AI) | yes | yes | Imagen, Gemini image models | yes |

Calling a capability a provider lacks answers `501 unsupported`. An image in a chat message is
inline data (base64 or a `data:` URL), the URL of one of the site's own uploads, or an absolute
public URL:

- **Uploads of the site** — the `url` that `backyard.files.upload` (or `backyard.ai.image`)
  returns, site-relative or on the site's own origin — are read by Backyard from storage and
  sent to the provider as bytes, so the provider never needs to reach your internal site. The
  type is detected from the bytes (png, jpeg, gif or webp). Another site's file, an unknown file
  or any other site-relative URL is a `400`.
- **Other URLs** are passed as URLs to OpenAI, Azure OpenAI and the first-party Claude API;
  every other provider gets the bytes, downloaded by Backyard's SSRF-guarded fetcher (public
  addresses only).

Images are limited to 20 MiB each, and the images Backyard loads from URLs (uploads and
downloads together) to 64 MiB per request. Images a page holds that are not uploads (a canvas,
a picked file) go as data URLs. Generated images are stored as site files. End-user ids are sent
to providers as keyed, opaque hashes.

## Anthropic (Claude)

```yaml
ai:
  providers:
    anthropic:
      type: anthropic
      api_key: ${ANTHROPIC_API_KEY}     # or leave it out and set ANTHROPIC_API_KEY
      # base_url: https://gateway.ai.cloudflare.com/v1/<account>/<gateway>/anthropic
      # effort: high
      # refusal_fallback: true          # server-side refusal fallbacks (first-party API)
  models:
    smart: { provider: anthropic, model: claude-opus-5-5 }
    fast:  { provider: anthropic, model: claude-haiku-5-5 }
```

Backyard follows the current Claude API rules per model: `temperature` is only sent to models
that accept it, `effort` is sent when the request, alias or provider sets one, and JSON mode
adds a system instruction and strips code fences.

### Claude on Google Vertex AI

```yaml
ai:
  providers:
    claude-vertex:
      type: anthropic
      platform: vertex
      project_id: acme-ai            # default: the ADC project
      location: global               # default
  models:
    smart: { provider: claude-vertex, model: claude-opus-5-5 }
```

Credentials: Application Default Credentials (Workload Identity, the Cloud Run service
account). Use the model id exactly as Model Garden lists it.

### Claude on Amazon Bedrock

```yaml
ai:
  providers:
    claude-bedrock:
      type: anthropic
      platform: bedrock              # Bedrock's Messages API
      region: us-east-1
  models:
    smart: { provider: claude-bedrock, model: anthropic.claude-opus-5-5 }
    fast:  { provider: claude-bedrock, model: anthropic.claude-haiku-5-5 }
```

Credentials: the AWS chain (ECS task role, IRSA) or an `api_key` (Bedrock API key). This is the
[AWS module](../deploy/terraform/aws/README.md#ai)'s default.

## OpenAI and OpenAI-compatible servers

```yaml
ai:
  providers:
    openai:
      type: openai
      api_key: ${OPENAI_API_KEY}
      # organization: org-…
      # openai_project: proj_…
  models:
    gpt:   { provider: openai, model: gpt-5.4 }
    mini:  { provider: openai, model: gpt-5.4-mini }
    image: { provider: openai, model: gpt-image-1, kind: image }
    embed: { provider: openai, model: text-embedding-3-small, kind: embedding }
```

Any server that speaks the Chat Completions API works through `base_url`: vLLM, LiteLLM, LM
Studio, OpenRouter, an internal gateway. Differences between servers (`max_tokens` vs
`max_completion_tokens`, unsupported `temperature`, `stream_options` or `response_format`) are
detected on the first `400` and remembered per model.

```yaml
ai:
  providers:
    vllm:
      type: openai
      base_url: http://vllm.internal:8000/v1
      api_key: ${VLLM_API_KEY:-none}
      headers: { X-Team: platform }
  models:
    local: { provider: vllm, model: meta-llama/Llama-3.3-70B-Instruct }
```

### Cloudflare Workers AI and AI Gateway

Workers AI's OpenAI-compatible endpoint (an API token as `api_key`); image generation is routed
to the native Workers AI endpoint automatically:

```yaml
ai:
  providers:
    workers-ai:
      type: openai
      base_url: https://api.cloudflare.com/client/v4/accounts/<account-id>/ai/v1
      api_key: ${CLOUDFLARE_API_TOKEN}
  models:
    fast:  { provider: workers-ai, model: "@cf/meta/llama-3.3-70b-instruct-fp8-fast" }
    image: { provider: workers-ai, model: "@cf/black-forest-labs/flux-1-schnell", kind: image }
```

Cloudflare AI Gateway in front of any provider: point `base_url` at
`https://gateway.ai.cloudflare.com/v1/<account>/<gateway>/openai` (or `…/anthropic`,
`…/workers-ai/v1`, `…/compat`) and authenticate the gateway with
`headers: { cf-aig-authorization: "Bearer ${CF_AIG_TOKEN}" }`.

The [Cloudflare edition](cloudflare.md) uses the Workers AI binding natively. Its counterpart
of `ai.enabled: false` is `AI_ENABLED=false`, which `backyard cloudflare setup --no-ai` sets
(and deploys without the binding, for accounts without Workers AI): every AI endpoint then
answers `501 unsupported`, as on the Go server.

## Ollama

```yaml
ai:
  providers:
    ollama:
      type: ollama
      base_url: http://ollama:11434/v1    # default: http://localhost:11434/v1
  models:
    local: { provider: ollama, model: llama3.3 }
    embed: { provider: ollama, model: nomic-embed-text, kind: embedding }
```

Ollama is OpenAI-compatible; nothing leaves your network. Good for laptops (`backyard dev
--config backyard.yaml`) and air-gapped installs.

## Azure OpenAI (and Azure AI Foundry)

```yaml
ai:
  providers:
    azure:
      type: azure-openai
      endpoint: https://acme.openai.azure.com
      use_azure_ad: true                 # managed identity / DefaultAzureCredential, no key
      # api_key: ${AZURE_OPENAI_API_KEY}
      # api_version: 2024-10-21          # default; "v1" uses the v1 API without deployments in the path
  models:
    smart: { provider: azure, model: gpt-5.4 }          # the deployment name
    embed: { provider: azure, model: text-embedding-3-small, kind: embedding }
```

With `use_azure_ad`, the identity needs the *Cognitive Services OpenAI User* role. The
[Azure module](../deploy/terraform/azure/README.md#ai) deploys models keylessly.

## Amazon Bedrock (Converse API)

```yaml
ai:
  providers:
    bedrock:
      type: bedrock
      region: us-east-1               # or AWS_REGION
      # api_key: ${BEDROCK_API_KEY}   # Bedrock API key instead of the AWS chain
  models:
    nova:  { provider: bedrock, model: amazon.nova-pro-v1:0 }
    embed: { provider: bedrock, model: amazon.titan-embed-text-v2:0, kind: embedding }
    image: { provider: bedrock, model: amazon.nova-canvas-v1:0, kind: image }
```

The Converse API works with any Bedrock text model (Claude, Nova, Llama, Mistral, Cohere…,
including cross-region inference profiles such as `us.anthropic.…`). Embeddings support
`amazon.titan-embed-*` and `cohere.embed-*`; images `amazon.nova-canvas*`,
`amazon.titan-image-generator*` and `stability.*`. IAM: `bedrock:InvokeModel` and
`bedrock:InvokeModelWithResponseStream` on the models.

## Google Gemini (AI Studio and Vertex AI)

```yaml
ai:
  providers:
    gemini:                       # Gemini API (Google AI Studio)
      type: gemini
      api_key: ${GEMINI_API_KEY}
    vertex:                       # Vertex AI with Application Default Credentials
      type: gemini
      project_id: acme-ai
      location: global
  models:
    smart: { provider: vertex, model: gemini-3.8-flash }
    image: { provider: vertex, model: gemini-3.1-flash-image, kind: image }
    embed: { provider: vertex, model: gemini-embedding-001, kind: embedding }
```

`project_id` (or `platform: vertex`) selects Vertex AI; Vertex AI express mode is `platform:
vertex` with an `api_key` and no project. Images use Imagen (`imagen-*`) or Gemini image
models; JSON mode sets `responseMimeType: application/json`. The
[Google Cloud module](../deploy/terraform/gcp/README.md) can enable Gemini and Claude on Vertex.

## Errors and quotas

| Situation | Response |
|---|---|
| AI not configured | `501 unsupported` |
| Unknown alias | `404 not_found`; an alias of the wrong kind → `400 bad_request` |
| User over `ai_per_minute` / `ai_tokens_per_day` | `429 rate_limited` with `Retry-After` |
| Request over the per-request limits (below) | `400 bad_request` |
| Provider rejects the request (`400`/`422`: unsupported parameter, unreadable image) | `400 bad_request` with a sanitized message |
| Provider rejects the request as too large (`413`) | `413 too_large` with a sanitized message |
| Provider throttles the server's account (`429`) | `429 rate_limited` with `Retry-After` when the provider says when to retry (at most an hour), else `503 upstream_error` |
| Provider timeout (`504`, `408`, or `ai.request_timeout`) | `504 upstream_error` |
| Provider outage, bad credentials, malformed answer | `502 upstream_error` (details only in the server log) |

The Cloudflare edition maps provider answers the same way. Per-request limits:

| Limit | Go server | Cloudflare edition |
|---|---|---|
| Images per `/ai/image` request (`n`) | 10 | 4 |
| Strings per `/ai/embed` request | 2048 (8 MiB in total) | 100 |

When a provider reports no usage, Backyard estimates tokens so per-user budgets cannot be
bypassed. Metrics: `backyard_ai_requests_total{model,result}` and
`backyard_ai_tokens_total{model,direction}` per alias.
