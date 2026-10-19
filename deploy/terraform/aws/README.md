# Backyard on AWS (Terraform)

This module runs [Backyard](../../../docs/spec/SPEC.md) on AWS: you drop a folder of
HTML into it and get an internal URL, and every site gets a zero-config backend
(database with realtime, files, AI, warehouse, websockets, identity). The
deployment is production-shaped: ECS Fargate behind an HTTPS Application Load
Balancer, S3 for sites, DynamoDB or RDS PostgreSQL for `backyard.db`, Secrets
Manager for credentials, and optionally Claude on Amazon Bedrock and a Lambda
deploy gateway.

> Looking for a free deployment? The Cloudflare edition
> ([`deploy/cloudflare`](../../cloudflare), Terraform in
> [`deploy/terraform/cloudflare`](../cloudflare); modelled on
> [topmass/openquick](https://github.com/topmass/openquick)) runs on the Workers
> free plan with the same CLI, SDK and API. A single VM or Kubernetes cluster
> without a bucket also works (`deploy/docker-compose`, `deploy/helm`).

## Architecture

```
                         Route 53: backyard.acme.com, *.backyard.acme.com
                                         │
 browsers ──HTTPS──▶ Application Load Balancer (ACM cert, HTTP→HTTPS, optional WAF)
                       │  auth_mode = alb: authenticate-oidc (Okta) on every request,
                       │  except /__backyard/healthz|readyz, /__backyard/api/deploy/*,
                       │  /__backyard/api/tokens and the Claude connector's machine paths
                       ▼
               ECS Fargate service "backyard serve" (private subnets, autoscaling)
                 │          │              │                 │
                 ▼          ▼              ▼                 ▼
              S3 bucket   DynamoDB      ElastiCache        Amazon Bedrock
              sites/      (or RDS       Valkey pub/sub     (Claude via the
              uploads/    PostgreSQL)   (>1 task, DynamoDB) Messages API)
                 ▲
                 │ presigned PUTs
 CLI / CI ──HTTPS──▶ Lambda function URL: standalone deploy gateway (optional)
```

| Component | Resources |
|---|---|
| Network | New 2–4 AZ VPC with public and private subnets, NAT gateway(s), S3/DynamoDB gateway endpoints — or your existing VPC and subnets |
| Entry | ALB, HTTPS listener (TLS 1.3 policy, HSTS), HTTP→HTTPS redirect, ACM certificate for `domain` and `*.domain`, Route 53 ALIAS records |
| Compute | ECS cluster (Container Insights), Fargate task definition and service, deployment circuit breaker with rollback, CPU target-tracking autoscaling, optional Fargate Spot and ECS Exec |
| Storage | S3 bucket: private, encrypted (SSE-S3 or SSE-KMS), TLS-only policy, versioning with noncurrent-version expiry, CORS for presigned uploads from the hub UI |
| Database | `database = "dynamodb"` (default): on-demand table `pk`/`sk` with point-in-time recovery. `database = "postgres"`: RDS PostgreSQL 16, encrypted, private, automated backups |
| Pub/sub | Chosen automatically: in-memory for a single task, ElastiCache for Valkey (TLS + AUTH) for several tasks on DynamoDB, LISTEN/NOTIFY on PostgreSQL |
| Secrets | Secrets Manager: `BACKYARD_SECRET` (generated), OIDC client secret, AI provider keys, `DATABASE_URL`, `REDIS_URL` — injected as ECS secrets |
| AI | Optional Bedrock permissions and a default AI config: `smart` = Claude Opus 5.5, `fast` = Claude Haiku 5.5, `embed` = Titan Text Embeddings v2, `image` = Nova Canvas (where available) |
| Deploy gateway | Optional Lambda (`provided.al2023`, arm64) with a function URL, advertised to CLIs as `deploy.gateway_url` |
| Logs | CloudWatch log groups with retention for the service and the gateway |
| IAM | Least-privilege task role (bucket, table, Bedrock), execution role (logs, the module's secrets), gateway role (bucket, logs, reading the `BACKYARD_SECRET` secret), optional `*-deployer` policy for `aws s3 sync` deploys |

Backyard's configuration (`backyard.yaml`) is rendered from
[`templates/backyard.yaml.tftpl`](templates/backyard.yaml.tftpl) and passed as
`BACKYARD_CONFIG_YAML`. It contains no secret values — only `${VAR}` references
that Backyard expands from the environment at startup. Inspect it with
`terraform output backyard_config_yaml`.

## Prerequisites

* Terraform **>= 1.6** (the test suite needs >= 1.7) and AWS provider **>= 6.28**
  (6.x).
* AWS credentials allowed to create the resources above (VPC, ELB, ECS, IAM roles
  and policies, S3, DynamoDB or RDS, ElastiCache, Secrets Manager, ACM, Route 53,
  Lambda, CloudWatch Logs).
* A domain for Backyard, e.g. `backyard.acme.com`. Ideally a public Route 53 hosted
  zone containing it (then certificates and DNS are fully automatic); any other DNS
  provider works with one extra step (see [DNS without Route 53](#dns-without-route-53)).
  ACM validates certificates through public DNS only: with a private zone (internal
  ALB), also pass `certificate_arn`.
  In host mode sites use `*.backyard.acme.com`, so the wildcard must be yours too.
* For the Lambda gateway: the package `dist/backyard-lambda.zip`, built from the
  repository root with `make lambda` (Go toolchain required), or
  `enable_lambda_gateway = false`.
* For AI on Bedrock: Claude models enabled for the account in the
  [Bedrock console](https://console.aws.amazon.com/bedrock/home#/modelaccess)
  (Claude Opus 5.5 and Haiku 5.5 may require an access request).
* For SSO: an Okta (or other OIDC) administrator to create the application.

## Quickstart

```sh
git clone https://github.com/thasami/backyard && cd backyard
make lambda                               # builds dist/backyard-lambda.zip
cd deploy/terraform/aws/examples/basic
cp terraform.tfvars.example terraform.tfvars
$EDITOR terraform.tfvars                  # see the examples below
terraform init
terraform apply                           # ~10-15 min (RDS adds ~10 min)
terraform output cli_login_command
```

Then, on a developer machine:

```sh
backyard login --server https://backyard.acme.com --gateway https://xxxx.lambda-url.us-east-1.on.aws
backyard init lunch-vote && cd lunch-vote
backyard deploy                           # https://lunch-vote.backyard.acme.com/
```

To use the module from your own configuration:

```hcl
module "backyard" {
  source = "git::https://github.com/thasami/backyard.git//deploy/terraform/aws?ref=v0.3.0"

  domain          = "backyard.acme.com"
  route53_zone_id = "Z0123456789ABCDEFGHIJ"
  auth_mode       = "oidc"
  oidc_issuer     = "https://acme.okta.com/oauth2/default"
  oidc_client_id  = "0oa1example2web3client"
  # oidc_client_secret from TF_VAR_oidc_client_secret or a secrets manager
  lambda_zip_path = "${path.root}/backyard-lambda.zip"
}
```

### `terraform.tfvars` for each auth mode

**`auth_mode = "none"`** — no login, anonymous identities. Only for labs, demos
and networks you trust: anyone who can reach the ALB can use Backyard and mint
deploy tokens (`terraform plan` warns when the ALB is public and open to the world).

```hcl
domain          = "backyard.lab.acme.com"
route53_zone_id = "Z0123456789ABCDEFGHIJ"
auth_mode       = "none"
internal        = true                    # or keep it public but narrow allowed_cidrs:
# allowed_cidrs = ["203.0.113.0/24"]      # office / VPN egress
```

**`auth_mode = "oidc"`** — Backyard's built-in OpenID Connect login (Okta,
Entra ID, Google, Auth0, Keycloak…). Sessions are cookies on `.backyard.acme.com`,
so one login covers the hub and every site.

```hcl
domain             = "backyard.acme.com"
route53_zone_id    = "Z0123456789ABCDEFGHIJ"
auth_mode          = "oidc"
oidc_issuer        = "https://acme.okta.com/oauth2/default"
oidc_client_id     = "0oa1example2web3client"
oidc_client_secret = "…"                  # better: export TF_VAR_oidc_client_secret
oidc_device_client_id = "0oa1example2native3cli"   # optional: backyard login --device
oidc_scopes        = ["openid", "profile", "email", "groups"]
admin_groups       = ["backyard-admins"]
allowed_domains    = ["acme.com"]
```

**`auth_mode = "alb"`** — the load balancer authenticates every request with
`authenticate-oidc` and Backyard verifies the signed `X-Amzn-Oidc-Data` header
(`auth.alb.signer_arn` is pinned to this ALB). Health probes bypass
authentication; `/__backyard/api/deploy/*` and `/__backyard/api/tokens` are
authenticated only when the browser already has an ALB session (hub drag-and-drop
deploys) and otherwise passed through so CLIs and CI can deploy and run
`backyard token create` with Backyard bearer tokens, which Backyard verifies (a
token minted with a token never outlives it).

```hcl
domain             = "backyard.acme.com"
route53_zone_id    = "Z0123456789ABCDEFGHIJ"
auth_mode          = "alb"
oidc_issuer        = "https://acme.okta.com/oauth2/default"
oidc_client_id     = "0oa1example2web3client"
oidc_client_secret = "…"
oidc_scopes        = ["openid", "profile", "email", "groups"]
admin_groups       = ["backyard-admins"]
# url_mode = "path"   # if your IdP cannot accept a wildcard redirect URI (see below)
```

The ALB itself calls the IdP's token and userinfo endpoints, so an internal ALB
needs a NAT route from its subnets. The authorization, token and userinfo endpoints are read from
`<oidc_issuer>/.well-known/openid-configuration` at plan time; set
`oidc_endpoints` to skip discovery (e.g. when Terraform runs without internet access).

In ALB mode the listener rules exempt these paths from `authenticate-oidc`:

| Rule | Priority | Paths | ALB login |
|---|---|---|---|
| `public` | 10 | `/__backyard/healthz`, `/__backyard/readyz` | never |
| `mcp` | 15 | `/__backyard/mcp`, `/__backyard/oauth/*`, `/.well-known/oauth-authorization-server`, `/.well-known/oauth-protected-resource/__backyard/mcp` | never (the [Claude connector](#claude-connector)'s machine paths) |
| `deploy` | 20 | `/__backyard/api/deploy`, `/__backyard/api/deploy/*`, `/__backyard/api/tokens` | only when the browser already has an ALB session |
| default | — | everything else, `/__backyard/auth/authorize` included | always |

## Okta application setup

Run `terraform output oidc_redirect_uris` to get the exact URIs for your settings.

### `auth_mode = "oidc"`

1. Okta Admin Console → **Applications → Create App Integration → OIDC – OpenID
   Connect → Web Application**.
2. **Grant type**: Authorization Code (Backyard also uses PKCE). Refresh tokens are not needed.
3. **Sign-in redirect URI**: `https://backyard.acme.com/__backyard/auth/callback`.
4. **Sign-out redirect URI**: `https://backyard.acme.com/`.
5. **Assignments**: the groups that may use Backyard.
6. Copy the **Client ID** and **Client secret** into `oidc_client_id` /
   `oidc_client_secret`. The issuer is your authorization server, e.g.
   `https://acme.okta.com/oauth2/default` (custom server, recommended) or
   `https://acme.okta.com` (org server).
7. Groups (for `admin_groups` / `allowed_groups`): in **Security → API →
   Authorization Servers → default → Claims**, add a `groups` claim (ID token,
   *Always*, filter e.g. *Matches regex* `.*` or *Starts with* `backyard`) and
   add `"groups"` to `oidc_scopes` if you also created a `groups` scope. With the
   org authorization server, set `oidc_use_userinfo = true` instead.

Optional CLI device flow (`backyard login --device`, for SSH sessions and
headless machines): create a second integration of type **Native Application**
with the **Device Authorization** grant (and Refresh Token), assign the same
groups and set its client id as `oidc_device_client_id`. The Lambda gateway and the
embedded gateway then accept the IdP's access tokens for that client.

### `auth_mode = "alb"`

1. Create a **Web Application** as above.
2. **Sign-in redirect URIs**: `https://backyard.acme.com/oauth2/idpresponse` and,
   in host mode, `https://*.backyard.acme.com/oauth2/idpresponse` — the ALB
   completes the login on the host the user requested, so every site subdomain
   needs a redirect URI. In the Okta app enable **Allow wildcard * in sign-in URI
   redirect**. Identity providers without wildcard redirect URIs (e.g. Entra ID
   web apps) need `url_mode = "path"` in ALB mode.
3. Client authentication: **Client secret** (the ALB does not support PKCE-only clients).
4. Set `oidc_issuer`, `oidc_client_id` and `oidc_client_secret`. The secret is
   stored in the listener configuration (and therefore in the Terraform state).
5. The ALB forwards the IdP's userinfo claims; include `groups` in the userinfo
   response (Okta: groups claim on the authorization server, `groups` scope) if you
   use `admin_groups` or `allowed_groups`.

CLI login works the same way in both modes: `backyard login` opens
`/__backyard/auth/cli` in the browser (authenticated by Okta or the ALB) and
receives a Backyard API token on a loopback port.

## DNS without Route 53

Without `route53_zone_id` the certificate must be validated by records you create
at your DNS provider, before the HTTPS listener can use it:

```sh
terraform apply -target='module.backyard.aws_acm_certificate.this[0]'
terraform output certificate_validation_records    # create these CNAMEs
terraform apply                                     # waits up to certificate_validation_timeout
terraform output dns_records                        # point the domain and *.domain at the ALB
```

Alternatively issue the certificate yourself and pass `certificate_arn` (it must
cover `domain` and, in host mode, `*.domain`).

## Existing VPC

```hcl
vpc_id             = "vpc-0123456789abcdef0"
public_subnet_ids  = ["subnet-aaa", "subnet-bbb"]   # internet-facing ALB
private_subnet_ids = ["subnet-ccc", "subnet-ddd"]   # tasks, RDS, ElastiCache (and an internal ALB)
```

Private subnets need egress to the internet (NAT or proxy) for the container
registry, the IdP, Bedrock and AWS APIs — or VPC endpoints for each AWS service
plus a mirrored image. The subnets must belong to `vpc_id` (checked at plan time).
The CIDR blocks of the ALB's subnets become `server.trusted_proxies`, so Backyard
honours the ALB's `X-Forwarded-For` / `X-Forwarded-Proto` but nothing a client
elsewhere in the VPC sends.

## AI

With `enable_bedrock = true` (default) and no `ai_config`, Backyard gets:

| Alias | Model | Provider |
|---|---|---|
| `smart` (default) | `anthropic.claude-opus-5-5` | Claude in Amazon Bedrock (Messages API, `bedrock-mantle`) |
| `fast` | `anthropic.claude-haiku-5-5` | same |
| `embed` | `amazon.titan-embed-text-v2:0` | Bedrock InvokeModel |
| `image` | `amazon.nova-canvas-v1:0` (us-east-1, eu-west-1, ap-northeast-1) | Bedrock InvokeModel |

The task role is granted `bedrock:InvokeModel` and
`bedrock:InvokeModelWithResponseStream` (which also authorise Converse and
ConverseStream) on foundation models and this account's inference profiles,
`bedrock-mantle:CreateInference` on the account's Bedrock projects for the
Messages API endpoint and, unless disabled, the Marketplace permissions needed to
enable a model on first use. Narrow the model
list with `bedrock_model_arns`; change the models with `bedrock_*_model`.

To use other providers, pass the body of Backyard's `ai:` section as `ai_config`
and the credentials as `ai_api_keys` (stored in Secrets Manager, injected as
environment variables). In Terraform strings a literal `${VAR}` is written `$${VAR}`:

```hcl
ai_api_keys = { ANTHROPIC_API_KEY = "sk-ant-…" }    # or TF_VAR_ai_api_keys='{"ANTHROPIC_API_KEY":"…"}'
ai_config   = <<-EOT
  default_model: smart
  providers:
    anthropic:
      type: anthropic
      api_key: "$${ANTHROPIC_API_KEY}"
  models:
    smart: { provider: anthropic, model: claude-opus-5-5, description: "Best model" }
    fast:  { provider: anthropic, model: claude-haiku-5-5 }
EOT
```

## More configuration

`config_overrides` adds the Backyard config sections the module does not generate
(`identity`, `warehouse`, `sites`, `files`, `ratelimit`, `realtime`, `mcp`). Secrets they
need come from `extra_secrets` (Secrets Manager or SSM parameter ARNs, the
execution role is granted access) and extra AWS permissions from
`task_role_policy_arns`:

```hcl
config_overrides = <<-EOT
  sites:
    ownership: owner
  warehouse:
    allow_raw_sql: false
    sources:
      default:
        type: athena
        region: us-east-1
        workgroup: analytics
        database: events
      redshift:
        type: postgres
        dsn: "$${WAREHOUSE_DSN}"
    queries:
      weekly_active_users:
        sql: "SELECT day, users FROM wau WHERE team = @team"
        params: [team]
EOT
task_role_policy_arns = [aws_iam_policy.backyard_athena.arn]
extra_secrets         = { WAREHOUSE_DSN = "arn:aws:secretsmanager:us-east-1:123456789012:secret:wh-AbCdEf:dsn::" }
```

Other useful inputs: `admins`, `static_tokens` (CI tokens, stored as SHA-256),
`allowed_cidrs`, `allowed_security_group_ids`, `internal`, `waf_web_acl_arn`,
`kms_key_arn` (one customer-managed key for S3, DynamoDB, RDS, ElastiCache,
Secrets Manager and the Lambda environment), `cpu_architecture = "ARM64"`,
`use_fargate_spot`, `enable_execute_command`, `log_retention_days`. See
[`variables.tf`](variables.tf) for every input and [`outputs.tf`](outputs.tf) for
the outputs (`hub_url`, `gateway_url`, `bucket`, `cli_login_command`,
`alb_dns_name`, `certificate_validation_records`, `dns_records`, …).

## Claude connector

The [Claude connector](../../../docs/claude-connector.md) lets people publish sites from Claude
with their Okta sign-in. Turn it on in the ECS service's configuration:

```hcl
config_overrides = <<-EOT
  mcp:
    enabled: true
EOT
```

The connector URL is `https://<domain>/__backyard/mcp` (output `hub_url` +
`/__backyard/mcp`). The Lambda gateway never gets the `mcp` section: only the hub serves the
connector.

- **`auth_mode = "oidc"`**: nothing else to do. Backyard keeps using the Okta application and
  its redirect URI `/__backyard/auth/callback`.
- **`auth_mode = "alb"`**: the listener rule `mcp` (priority 15, between `public` and
  `deploy`) forwards the four machine paths — `/__backyard/mcp`, `/__backyard/oauth/*`,
  `/.well-known/oauth-authorization-server` and
  `/.well-known/oauth-protected-resource/__backyard/mcp` — without `authenticate-oidc`, because
  Claude calls them without a browser session. The rule exists whether or not the connector is
  on (Backyard answers `404` there until it is); after upgrading the module, run
  `terraform apply`. `/__backyard/auth/authorize` stays behind the ALB login, which is the
  connector's sign-in. On site hosts Backyard answers the machine paths with `404` and still
  demands the ALB's signed header for a site's own `/.well-known` files.
- **Network**: hosted Claude reaches the hub from `160.79.104.0/21` (IPv4), so an internet-facing
  ALB whose `allowed_cidrs` restrict sources must include that range (an `internal = true` ALB
  works only with connectors added locally in Claude Code). By default Backyard fetches Claude's
  client metadata from `https://claude.ai` at the first connect; the tasks reach it through the
  NAT gateway. Without egress, add `clients: []` to the `mcp` section.
- **AWS WAF** (`waf_web_acl_arn`): the managed rule `SizeRestrictions_BODY` (Core rule set)
  blocks large request bodies; exclude `/__backyard/mcp` from it (publish calls carry up to
  9 MiB), and allow `160.79.104.0/21` if the web ACL restricts sources.

## Scaling and pub/sub

The service runs `desired_count` tasks and, with `enable_autoscaling`, scales up to
`autoscaling_max_capacity` on average CPU (`autoscaling_cpu_target`). Realtime
(websocket) fan-out between tasks needs a broker, chosen by `pubsub = "auto"`:

| database | max tasks | pub/sub |
|---|---|---|
| dynamodb | 1 | memory (no extra resources) |
| dynamodb | > 1 | ElastiCache for Valkey (`redis_node_type`, `redis_num_nodes`) |
| postgres | any | PostgreSQL LISTEN/NOTIFY |

For the smallest deployment set `desired_count = 1` and `enable_autoscaling = false`.

With PostgreSQL every task holds a pool of up to `db_max_connections_per_task`
(default 10) connections plus one for LISTEN/NOTIFY, and a rolling deployment
briefly doubles the task count. Keep
`2 × autoscaling_max_capacity × (db_max_connections_per_task + 1)` below the
instance's `max_connections` (about 80 on `db.t4g.micro`), or pick a larger
`db_instance_class`.

## Security notes

* **Terraform state contains secrets** (the generated `BACKYARD_SECRET`, database
  and Valkey passwords, the OIDC client secret in ALB mode, AI keys). Use an
  encrypted remote backend with restricted access.
* The Lambda gateway never receives `BACKYARD_SECRET` itself: its environment
  holds `BACKYARD_SECRET_ARN`, the ARN of the Secrets Manager secret the ECS
  service reads, and the function fetches the value at cold start with the gateway
  role (`secretsmanager:GetSecretValue` on that secret only, plus `kms:Decrypt`
  through Secrets Manager with `kms_key_arn`). The function makes that call to the
  regional Secrets Manager endpoint over the internet (it runs outside the VPC).
* The Lambda function URL is public (`authorization_type = NONE` plus the
  resource-based policy granting `lambda:InvokeFunctionUrl` and
  `lambda:InvokeFunction` via the URL) because Backyard verifies bearer tokens
  itself; it stays reachable from the internet even with `internal = true`.
  Disable it with `enable_lambda_gateway = false` if that is not acceptable — CLIs
  then use the embedded gateway behind the ALB. For the same reason
  `allow_anonymous_deploys` requires `enable_lambda_gateway = false`.
* The ALB answers `400` to requests carrying an `X-Forwarded-Host` header: it
  passes that header through from clients unchanged, and Backyard honours it from
  the ALB when mapping hosts to sites. Set `reject_forwarded_host_header = false`
  only if a proxy in front of the ALB sets it deliberately.
* Rotating `BACKYARD_SECRET` (`terraform apply -replace=module.backyard.random_password.backyard_secret`)
  invalidates sessions and API tokens. The apply also updates the Lambda gateway's
  configuration (`BACKYARD_SECRET_VERSION_ID`), so its next cold start reads the new value;
  ECS tasks read the secret when they start, so redeploy the service afterwards
  (`aws ecs update-service --cluster <name> --service <name> --force-new-deployment`).
* With Bedrock, the first invocation of a third-party model (Claude) subscribes the
  account to it through AWS Marketplace. The task role may do that only via
  Bedrock (`aws:CalledViaLast`); set `bedrock_allow_marketplace_subscribe = false`
  if an administrator manages model access instead.
* `deletion_protection = true` (default) protects the DynamoDB table and RDS
  instance; set it to `false` and apply before `terraform destroy`. RDS takes a
  final snapshot unless `db_final_snapshot = false`, and the S3 bucket is kept
  unless `bucket_force_destroy = true`.

## Costs

Approximate monthly cost in us-east-1 with the defaults (two x86 tasks of 1 vCPU /
2 GiB, DynamoDB, Valkey, one NAT gateway), before usage-based charges:

| Item | ≈ USD / month |
|---|---|
| Fargate: 2 × (1 vCPU, 2 GiB) | 72 (ARM64: 58; Fargate Spot: up to −70 %) |
| Application Load Balancer + LCUs | 18–25 |
| NAT gateway (+ $0.045/GB processed) | 33 |
| ElastiCache for Valkey `cache.t4g.micro` | 9 |
| Public IPv4 addresses (ALB, NAT) | 11 |
| Secrets Manager (2–5 secrets) | 1–2 |
| CloudWatch Logs, Container Insights | 3–15 (volume dependent) |
| DynamoDB on demand, S3, Lambda, Route 53 | usually < 5 for internal tools |
| **Total** | **≈ 150–170** |

`database = "postgres"` replaces DynamoDB and Valkey with `db.t4g.micro`
(≈ $12 + $2.30 for 20 GiB gp3; Multi-AZ doubles it). Bedrock is billed per token.
A minimal setup (`desired_count = 1`, `enable_autoscaling = false`,
`tasks_in_public_subnets = true`, `cpu = 512`, `memory = 1024`,
`cpu_architecture = "ARM64"`, `container_insights = false`) costs roughly
$45–50 per month, mostly the ALB.

## Upgrades

* **Backyard releases**: pin `image` to a version tag and change it to upgrade;
  ECS performs a rolling deployment and rolls back automatically if the new tasks
  fail their health checks. With a mutable tag (`:latest`) run
  `aws ecs update-service --cluster <name> --service <name> --force-new-deployment`.
* **Lambda gateway**: rebuild `dist/backyard-lambda.zip` (`make lambda`) and apply;
  the package hash triggers the update.
* **Configuration changes** (any module input that affects `backyard.yaml`) create a
  new task definition revision and a rolling deployment. Secrets are read when a
  task starts, so changed secret values need a new deployment.
* **PostgreSQL** minor versions are upgraded automatically in the maintenance window;
  major upgrades are not (`allow_major_version_upgrade = false`).
* Switching `database` or `pubsub` does not migrate data; switching `url_mode`
  replaces the certificate (create-before-destroy, no downtime).
* The module requires AWS provider 6.28 or newer within 6.x
  (`data.aws_region.current.region`, `aws_lambda_permission.invoked_via_function_url`,
  current ECS/ELB/Lambda arguments). Run `terraform init -upgrade` after changing
  module versions.

## Testing

```sh
terraform fmt -recursive -check
terraform init -backend=false && terraform validate
terraform test          # offline: every provider is mocked (Terraform >= 1.7)

# Load the backyard.yaml every run renders (ECS service and Lambda gateway)
# with Backyard's own config loader, in the task's or function's environment:
cd ../../.. && BACKYARD_TEST_TERRAFORM=1 go test ./deploy/terraform/aws/configcheck/
```

CI runs both (`make tf-configcheck` runs the configcheck tests of every
module), so a key that `internal/config` no longer accepts fails CI instead of
leaving an ECS task that never starts.

With Terraform 1.6 (no mock providers) use
`terraform init -backend=false -test-directory=none && terraform validate -no-tests`.
