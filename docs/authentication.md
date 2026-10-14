# Authentication

Backyard is meant to sit inside your company's trust bubble: every page view, API call and
websocket is tied to a person your identity provider (IdP) vouched for. `auth.mode` picks who
does the vouching.

| Mode | Who signs people in | Use it when |
|---|---|---|
| `oidc` | Backyard itself, against any OpenID Connect IdP (Okta, Entra ID, Google Workspace, Auth0, Keycloak, Ping, JumpCloud…) | You want one binary and no extra proxy. The most common choice. |
| `iap` | Google Cloud Identity-Aware Proxy in front of a load balancer or Cloud Run | You run on Google Cloud (this is how Shopify runs Quick). |
| `alb` | An AWS Application Load Balancer with `authenticate-oidc` | You run on AWS and prefer the load balancer to own the login. |
| `header` | Any trusted proxy that sets identity headers or a signed JWT: oauth2-proxy, Pomerium, Cloudflare Access, Azure Easy Auth, nginx `auth_request`… | You already have such a proxy. |
| `none` | Nobody — every visitor is an anonymous guest | Laptops, labs, demos. Never on a network outsiders can reach. |

The Cloudflare edition has its own two modes: `none` and Cloudflare Access (see
[below](#cloudflare-access)).

In every mode, the API and the deploy gateway also accept `Authorization: Bearer` tokens:
[Backyard API tokens](#api-tokens) minted for a person, [static tokens](#static-tokens) for CI,
and — when `auth.oidc.issuer` is configured — tokens issued by your IdP to the
[CLI device flow](#device-flow).

## Identity, admins and gates

Whatever the mode, the result is a user profile that pages read with `backyard.me()`:

- `id` — the lowercased email, else `sub:<subject>`; `anon:<hex>` for guests and
  `token:<name>` for static tokens.
- `name`, `title`, `team`, `slack`, `avatar_url`, `groups` and `extra` come from IdP claims
  through `identity.claims` (defaults: `name`, `title`, `department`, `slack`, `picture`,
  `groups`) and an optional people directory (`identity.directory`, a CSV or JSON file). See
  [configuration](configuration.md#identity).
- An `email` claim the IdP marks `email_verified: false` is ignored (the user is then
  identified by `sub`), so unverified addresses can never match `allowed_domains` or `admins`.

Access rules:

```yaml
auth:
  admins: [platform-team@acme.com]        # user ids / emails
  admin_groups: [backyard-admins]         # IdP groups
  allowed_domains: [acme.com]             # optional: only these email domains
  allowed_groups: [employees]             # optional: only these groups
```

People outside `allowed_domains` / `allowed_groups` get `403`. Admins can delete and (in
ownership mode) overwrite any site. Everything else — reading and writing a site's data — is
open to everyone who passes the gates: Backyard deliberately has no per-site ACLs (see
[security](security.md)).

## `oidc`: built-in OpenID Connect login

Backyard runs the authorization-code flow with PKCE and a nonce, verifies the ID token, maps
its claims and sets an encrypted session cookie (`backyard_session`, 12 hours by default) on
`.<base_domain>`, so one login covers the hub and every site. Unauthenticated page loads are
redirected to the IdP; API calls get `401` with a `login_url`, which the SDK follows.

```yaml
auth:
  mode: oidc
  admin_groups: [backyard-admins]
  allowed_domains: [acme.com]
  oidc:
    issuer: https://acme.okta.com/oauth2/default
    client_id: ${OIDC_CLIENT_ID}
    client_secret: ${OIDC_CLIENT_SECRET}
    # scopes: [openid, profile, email]       # add "groups" if your IdP needs a scope for it
    # session_ttl: 12h
    # use_userinfo: true                     # merge userinfo claims (groups not in the ID token)
    # device_client_id: 0oa-native-client    # enables `backyard login --device`
```

Endpoints on the hub: `/__backyard/auth/login?rd=<url>` (start; `rd` must point at the hub or a
site, anything else is replaced by the hub URL), `/__backyard/auth/callback` (the redirect URI)
and `/__backyard/auth/logout` (clears the session and calls the IdP's `end_session_endpoint`
with `post_logout_redirect_uri` = the hub URL when the IdP advertises one). In the other modes
the proxy in front signs people in, so `/__backyard/auth/login?rd=<url>` simply redirects to
`rd` (same rules) and there is no callback or logout endpoint.

**Host mode needs a cookie domain.** The session cookie is set for `.<base_domain>` so that site
hosts receive it. Browsers refuse that for `localhost`, `*.localhost`, IP addresses and
single-label names, and a hub outside `base_domain` could not set it at all — every site visit
would bounce through the IdP forever. So with `auth.mode: oidc` in host mode, the server refuses
to start unless `server.base_domain` is a real domain and the host of `server.public_url` is
that domain or one of its subdomains. For local tests use `server.url_mode: path`, or a domain
whose subdomains resolve to 127.0.0.1 such as `lvh.me`.

**Use https.** Over https the short-lived login state cookie is `__Host-backyard_oauth`, which
only the hub can set, so a page on some site cannot plant its author's login in your browser.
Over plain http it is `backyard_oauth` and the server warns at startup that the login state is
unprotected in host mode. Session cookies that a site's page planted next to yours never sign
you in as someone else: conflicting cookies fail closed (see
[security](security.md#cookies-and-sessions)).

### Okta, step by step

1. **Create the web app.** Okta Admin Console → **Applications → Create App Integration →
   OIDC – OpenID Connect → Web Application**.
2. **Grant type:** Authorization Code (Backyard adds PKCE). Refresh tokens are not needed for
   browser sessions.
3. **Sign-in redirect URI:** `https://backyard.acme.com/__backyard/auth/callback` (your
   `server.public_url` + `/__backyard/auth/callback`). One URI covers every site: the login
   always completes on the hub.
4. **Sign-out redirect URI:** `https://backyard.acme.com/`.
5. **Assignments:** the people or groups that may use Backyard (everyone else is stopped by
   Okta before Backyard sees them).
6. **Issuer:** a custom authorization server such as `https://acme.okta.com/oauth2/default`
   (recommended), or the org server `https://acme.okta.com`.
7. **Groups claim** (for `admin_groups` / `allowed_groups`): **Security → API → Authorization
   Servers → default → Claims → Add Claim**: name `groups`, include in **ID Token**, *Always*,
   value type **Groups**, filter e.g. *Starts with* `backyard` or *Matches regex* `.*`. Add
   `groups` to `auth.oidc.scopes` only if you created a `groups` scope. With the org
   authorization server, set `use_userinfo: true` instead.
8. Put the **Client ID** and **Client secret** into `auth.oidc.client_id` /
   `auth.oidc.client_secret` (via environment variables), restart, and open the hub.

**Device flow app (optional).** For `backyard login --device` (SSH sessions, headless
machines): **Create App Integration → OIDC → Native Application**, enable the **Device
Authorization** and **Refresh Token** grants, assign the same groups, and make sure the
authorization server's access policy rule allows the Device Authorization grant. Put its client
id in `auth.oidc.device_client_id`.

### Microsoft Entra ID

1. **App registrations → New registration**, single tenant. Platform **Web**, redirect URI
   `https://backyard.acme.com/__backyard/auth/callback`; front-channel logout URL
   `https://backyard.acme.com/__backyard/auth/logout` (optional).
2. **Certificates & secrets → New client secret.**
3. **Token configuration → Add optional claim** `email` (ID token), and **Add groups claim**
   (security groups). Entra emits group **object IDs** — use them in `admin_groups` /
   `allowed_groups`, or emit application groups by name.
4. For the device flow: **Authentication → Allow public client flows: Yes**; the same client id
   can be the `device_client_id`.
5. Configure:

   ```yaml
   auth:
     mode: oidc
     oidc:
       issuer: https://login.microsoftonline.com/<tenant-id>/v2.0
       client_id: <application (client) id>
       client_secret: ${ENTRA_CLIENT_SECRET}
       device_client_id: <application (client) id>   # optional
   identity:
     claims:
       email: preferred_username    # only if some users have no mail attribute
   ```

The [Azure Terraform module](../deploy/terraform/azure/README.md#authentication) creates this
app registration for you.

### Google Workspace

Create an OAuth client (**APIs & Services → Credentials → OAuth client ID → Web
application**) with the redirect URI above, then `issuer: https://accounts.google.com`. Google
ID tokens carry no groups: restrict access with `allowed_domains: [acme.com]` and grant admin
rights with `admins`. Or use [IAP](#iap-google-cloud-identity-aware-proxy).

### Auth0, Keycloak and others

Any IdP with OIDC discovery works: register the redirect URI, set `issuer`, `client_id`,
`client_secret`. Keycloak: add a *Group Membership* mapper named `groups` (turn off "full group
path" if you want plain names) and enable *Trust Email* on federated providers so emails are
verified.

## `iap`: Google Cloud Identity-Aware Proxy

IAP authenticates every request at the load balancer (or Cloud Run) and signs an assertion
that Backyard verifies (`X-Goog-IAP-JWT-Assertion`, ES256, issuer
`https://cloud.google.com/iap`).

1. Put Backyard behind an external HTTPS load balancer (or use IAP for Cloud Run directly) with
   a certificate for `backyard.acme.com` and `*.backyard.acme.com`.
2. Enable IAP on the backend service and grant **IAP-secured Web App User**
   (`roles/iap.httpsResourceAccessor`) to `domain:acme.com` or your groups.
3. Find the audience: `/projects/<project-number>/global/backendServices/<backend-service-id>`
   (`gcloud compute backend-services describe <name> --global --format='value(id)'`), or
   `/projects/<project-number>/locations/<region>/services/<service>` for Cloud Run.
4. Configure:

   ```yaml
   auth:
     mode: iap
     iap:
       audience: /projects/123456789/global/backendServices/987654321
   ```

IAP assertions carry the email and subject but no groups; use `allowed_domains`, `admins` and
a directory for profiles. Make sure only the load balancer can reach Backyard (Cloud Run
ingress "internal and load balancing", no public `run.app` URL).

CLIs cannot pass IAP, so the [Google Cloud module](../deploy/terraform/gcp/README.md#iap-notes)
adds a second backend service **without** IAP for `/__backyard/api/deploy/*` and
`/__backyard/api/tokens` (`backyard token create`) requests that carry an `Authorization`
header; Backyard itself rejects those without a valid token.

## `alb`: AWS ALB with Okta (or any OIDC IdP)

The ALB runs the login (`authenticate-oidc`) and forwards a signed `X-Amzn-Oidc-Data` header
(ES256) containing the IdP's userinfo claims.

1. In Okta create a **Web Application** with client authentication **Client secret** (the ALB
   does not support PKCE-only clients).
2. Sign-in redirect URIs: `https://backyard.acme.com/oauth2/idpresponse` and, in host mode,
   `https://*.backyard.acme.com/oauth2/idpresponse` (enable **Allow wildcard \* in sign-in URI
   redirect**): the ALB completes the login on the host the user asked for. IdPs without
   wildcard redirect URIs need `server.url_mode: path`.
3. Make the `groups` claim part of the **userinfo** response (Okta: groups claim on the
   authorization server plus the `groups` scope in the listener rule) if you use group rules.
4. Listener rule: `authenticate-oidc` with the issuer, authorization, token and userinfo
   endpoints from the IdP's discovery document, then forward to Backyard. Exempt
   `/__backyard/healthz`, `/__backyard/readyz` and — for CLIs and CI — `/__backyard/api/deploy/*`
   and `/__backyard/api/tokens` (pass through when there is no ALB session).
5. Configure:

   ```yaml
   auth:
     mode: alb
     alb:
       region: us-east-1
       signer_arn: arn:aws:elasticloadbalancing:us-east-1:111122223333:loadbalancer/app/backyard/abc123
   ```

Always set `signer_arn`, and only let the ALB reach Backyard (security groups). The
[AWS module](../deploy/terraform/aws/README.md#okta-application-setup) sets all of this up, plus an
optional [Lambda deploy gateway](deploying-sites.md#the-aws-lambda-gateway).

## `header`: trusted identity headers

Backyard trusts headers set by a proxy — but **only** from peers listed in
`server.trusted_proxies`, unless it verifies a signed JWT. An identity header that appears more
than once is rejected (a proxy that appends instead of replacing cannot be fooled).

### oauth2-proxy

```yaml
server:
  trusted_proxies: ["10.0.3.17/32"]        # the oauth2-proxy instances only
auth:
  mode: header
  header:
    email_header: X-Forwarded-Email
    name_header: X-Forwarded-Preferred-Username
    groups_header: X-Forwarded-Groups
```

Run oauth2-proxy with `--pass-user-headers`, `--cookie-domain=.backyard.acme.com` and
`--whitelist-domain=.backyard.acme.com` so one login covers every site. For CLIs, route
`/__backyard/api/deploy/*` and `/__backyard/api/tokens` requests that carry `Authorization:
Bearer …` from your edge proxy straight to Backyard. Because that peer is **not** in
`trusted_proxies`, Backyard only accepts the bearer token there and ignores any identity header
a client might add.

`trusted_proxies` is what makes header mode safe: every listed peer can claim any identity.
List only the proxies themselves. On Kubernetes the
[Helm chart](../deploy/helm/backyard/README.md#identity-aware-proxies) refuses to render header
mode without `jwt_header` while `trusted_proxies` covers whole ranges (its default is the
private ranges, i.e. every pod), unless its `networkPolicy` admits only the proxy.

### JWT-signing proxies (Pomerium and others)

```yaml
auth:
  mode: header
  header:
    jwt_header: X-Pomerium-Jwt-Assertion      # the header your proxy signs
    jwks_url: https://backyard.acme.com/.well-known/pomerium/jwks.json
    issuer: <the iss your proxy puts in the JWT>
    audiences: [<the aud your proxy puts in the JWT>]
```

With `jwt_header` the signed JWT is the source of truth (its claims go through
`identity.claims`, so `email`, `name` and `groups` claims are picked up); `trusted_proxies`
becomes optional (if set, the peer must be trusted too). Always set `issuer` and `audiences` —
without them Backyard logs a warning and accepts any JWT signed by those keys. A
`Bearer ` prefix in the header is tolerated.

### Azure App Service / Container Apps Easy Auth

```yaml
auth:
  mode: header
  header:
    email_header: X-MS-CLIENT-PRINCIPAL-NAME
server:
  trusted_proxies: ["0.0.0.0/0", "::/0"]
```

Display name and roles are read from `X-MS-CLIENT-PRINCIPAL`. The platform strips
client-supplied `X-MS-CLIENT-PRINCIPAL*` headers before Easy Auth sets them, which is what makes
trusting every peer acceptable here — but only if the app is reachable exclusively through the
platform's front end (no other ingress, no VNet peers that bypass it).

### Cloudflare Access

In front of the Go server (for example through a Cloudflare Tunnel):

```yaml
auth:
  mode: header
  header:
    jwt_header: Cf-Access-Jwt-Assertion
    jwks_url: https://acme.cloudflareaccess.com/cdn-cgi/access/certs
    issuer: https://acme.cloudflareaccess.com
    audiences: [<application AUD tag>]
```

Create a self-hosted Access application for `backyard.acme.com` and `*.backyard.acme.com` with
your IdP (Okta, Entra ID, Google…), and a **Bypass** policy for `/__backyard/api/deploy/*`,
`/__backyard/api/tokens` and `/__backyard/healthz` so CLIs can deploy and mint tokens with
bearer tokens (drag-and-drop deploys on the hub then also need a token, which the hub asks
for). Access JWTs carry the email but usually no groups.

The [Cloudflare edition](cloudflare.md) supports Access natively (`AUTH_MODE=access`).
`backyard cloudflare access enable --allow-email-domain acme.com` sets it up from the terminal:
with an API token in `BACKYARD_CF_API_TOKEN` it creates the hub application with its allow
policy and the bypass application for the paths the Worker authenticates itself, and without
one it prints the dashboard steps and detects the result. Either way it verifies Access at the
edge before it switches the Worker over (see the
[edition's README](../deploy/cloudflare/README.md#company-login-cloudflare-access)). The
optional [`deploy/terraform/cloudflare`](../deploy/terraform/cloudflare/README.md) module
creates the same as infrastructure as code, plus an Okta identity provider.

## `none`

```yaml
auth:
  mode: none
deploy:
  allow_anonymous: false     # true lets anyone deploy without a token
```

Every visitor gets a stable anonymous identity (`anon:<hex>`, "Guest 1a2b") from a signed
`backyard_anon` cookie. Deploys need a [static token](#static-tokens) unless
`deploy.allow_anonymous` is true (the default in `--dev`). Use it on laptops and isolated lab
networks only.

## API tokens

Personal Backyard API tokens are HS256 JWTs (issuer `backyard`, scope `api`) signed with a key
derived from `server.secret`; they carry the user's id, email, name, groups and admin flag.

- `backyard login` mints one through the browser (see [CLI login](#cli-login)).
- `backyard token create --name ci --ttl 168h` mints another (e.g. for a script); the hub's
  API does the same: `POST /__backyard/api/tokens` with `{"name", "ttl_hours"}`.
- Lifetime: `auth.token_ttl` (default 720h = 30 days), which is also the maximum. A token
  minted with a Backyard API token as the credential (`POST /__backyard/api/tokens` with a
  bearer token, e.g. `backyard token create` after a browser login) expires no later than that
  token: `ttl_hours` and the default are capped, and `expires_at` reports the result. So a token
  cannot extend its own lifetime, and someone removed from the IdP loses access when their
  tokens expire. Tokens minted for a browser session, a proxy identity or an IdP-issued JWT
  (device flow) get the full `auth.token_ttl`.
- Revocation: tokens are stateless — rotating `server.secret` revokes **all** of them (and all
  sessions). Keep lifetimes short for anything automated.
- Guests and static tokens cannot mint API tokens.

## Static tokens

Static tokens are pre-shared secrets for CI and automation that do not belong to a person. Only
their SHA-256 is stored:

```sh
openssl rand -hex 32 | tee ci-token.txt | backyard token hash --name github-actions
```

```yaml
auth:
  tokens:
    - name: github-actions          # requests act as user "token:github-actions"
      sha256: 9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08
      admin: false
```

Give the token to the pipeline as `BACKYARD_TOKEN`. Rotate it by adding the new hash, updating
the pipeline, then removing the old hash.

## Device flow

With `auth.oidc.issuer` and `auth.oidc.device_client_id` set, `GET /__backyard/api/info`
advertises the device flow and `backyard login --device` runs the OAuth device authorization
grant directly against your IdP: it prints a code and URL to open on any device. The CLI keeps
the IdP's token (the access token when it names the client, else the ID token) and refreshes it
with the `offline_access` refresh token. Backyard accepts IdP-issued JWTs whose `aud`, `azp` or
`cid` names `client_id`, `device_client_id` or one of `auth.oidc.api_audiences`, in every mode
where `auth.oidc.issuer` is configured — so the device flow also works behind IAP or an ALB.

## CLI login

`backyard login --server https://backyard.acme.com` reads `/__backyard/api/info` and picks a
flow:

| Server | What happens |
|---|---|
| `oidc`, `iap`, `alb`, `header` | The browser opens `/__backyard/auth/cli?redirect_uri=http://127.0.0.1:<port>/callback&state=…`. The page is authenticated like any other (your IdP or proxy); Backyard mints an API token and redirects it to the CLI's loopback listener. |
| same, with `--no-browser` | Open the printed `/__backyard/auth/cli?mode=display` URL on any device and paste the token shown there. |
| `--device` | OAuth device flow against the IdP (above). |
| `none` with anonymous deploys | Nothing to do; the server URL is saved. |
| `none` without anonymous deploys | Asks for a static deploy token (on the Cloudflare edition: the `DEPLOY_TOKEN` printed by setup, picked up automatically on the machine that ran it). |
| any, `--token <t>` or `--token -` | Stores the given token (stdin with `-`), e.g. in CI. |

The browser hand-off only accepts loopback redirect URIs (`127.0.0.1`, `localhost`, `[::1]`)
and requires a browser session or proxy identity — a bearer token can never renew itself.
Because any web page could navigate a signed-in browser to that URL, a request that arrives as
a cross-site navigation does not mint a token right away: Backyard first asks you to confirm on
its own page, and issues the token only when you do (the same holds for the `--no-browser`
page that displays a token). The URL the CLI opens itself needs no confirmation.

Credentials are stored per profile in `~/.config/backyard/config.json` (mode `0600`).

### Behind an identity-aware proxy

IAP, ALB, Cloudflare Access and oauth2-proxy authenticate browsers, not CLIs. The CLI therefore
logs in through the browser (the proxy is happy) but must deploy with its bearer token on a
path the proxy lets through:

- a **proxy-exempt route** on the hub for `/__backyard/api/deploy/*` — and, for `backyard token
  create`, `/__backyard/api/tokens` with an `Authorization` header — (ALB rule, a GCP backend
  service without IAP, an Access bypass policy; the Terraform modules set these up, and on the
  Cloudflare edition `backyard cloudflare access enable` does), or
- a **standalone gateway** — `backyard gateway` on its own host, or the
  [AWS Lambda gateway](deploying-sites.md#the-aws-lambda-gateway) — advertised to CLIs with
  `deploy.gateway_url`. `backyard login` adopts it automatically, or pass
  `--gateway https://deploy.backyard.acme.com`.
