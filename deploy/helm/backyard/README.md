# Backyard Helm chart

Runs [Backyard](https://github.com/thasami/backyard) on Kubernetes: drop a folder of HTML, get
an internal URL, with a zero-config database + realtime, files, AI, warehouse, websockets and
identity API for every site.

* **Defaults** — one replica, site files and an embedded SQLite database on a
  PersistentVolumeClaim (no cloud bucket, no database server), no login.
* **Production** — several replicas with S3/GCS/Azure Blob (or a ReadWriteMany volume),
  PostgreSQL or DynamoDB, Redis or Postgres pub/sub, OIDC SSO, ingress with a wildcard
  certificate, autoscaling, PodDisruptionBudget and a ServiceMonitor:
  [`values-production.yaml`](values-production.yaml).

Requirements: Kubernetes ≥ 1.25, Helm 3.8+ or Helm 4. The image is
`ghcr.io/thasami/backyard` (distroless, non-root uid 65532, read-only root filesystem).

## Install

```sh
helm upgrade --install backyard deploy/helm/backyard \
  --namespace backyard --create-namespace \
  --set config.server.base_domain=backyard.acme.com

kubectl -n backyard port-forward svc/backyard 8080:80
curl http://127.0.0.1:8080/__backyard/healthz
```

For real use, expose it with an ingress (below), point DNS for `<domain>` **and**
`*.<domain>` at the ingress controller, and turn on SSO.

Deploying sites needs a token. With SSO, people get one with `backyard login`. For CI (or with
`auth.mode: none`) add static tokens; only their SHA-256 goes into the values:

```sh
TOKEN="byd_$(openssl rand -hex 24)"; backyard token hash "$TOKEN"
```

```yaml
config:
  auth:
    tokens:
      - {name: deploy, sha256: <hash>, admin: true}
```

Then `backyard login --server https://<domain> --token "$TOKEN"`.

## Configuration model

Everything under `config:` is rendered verbatim as `backyard.yaml` (a ConfigMap mounted at
`/etc/backyard/backyard.yaml`) and validated strictly when the server starts — unknown keys are
errors, so typos fail fast. `backyard config example` prints every option with comments; check a
values file before deploying with:

```sh
helm template backyard deploy/helm/backyard -f my-values.yaml --show-only templates/configmap.yaml \
  | go run ./scripts/checkconfig -configmap -        # from a checkout of the repository
```

Keep credentials out of `config`: reference environment variables instead
(`dsn: "${DATABASE_URL}"`, `client_secret: "${OIDC_CLIENT_SECRET}"`) and provide them through

* `secret.env` — a chart-managed Secret (with `BACKYARD_SECRET` generated on install and kept on
  upgrades), or
* `existingSecret` — a Secret you manage (External Secrets, Sealed Secrets, Vault, …). All its
  keys become environment variables; it must contain `BACKYARD_SECRET` (≥ 32 characters).
  Use this with Argo CD / Flux, which render charts without cluster access (a generated secret
  would change on every render).

The chart refuses to render combinations that would break at runtime: several replicas with
SQLite, in-memory pub/sub or a ReadWriteOnce volume, `storage.fs.root` different from
`persistence.mountPath`, or a missing `base_domain`. It also refuses `auth.mode: header` set up
so that any pod could sign in as anyone (see [Identity-aware proxies](#identity-aware-proxies)).

## Values

| Key | Default | Description |
|---|---|---|
| `image.repository` / `image.tag` / `image.digest` | `ghcr.io/thasami/backyard` / appVersion / `""` | Image; a digest wins over the tag |
| `replicaCount` | `1` | Replicas (ignored with autoscaling) |
| `strategy` | `{}` | Empty: `Recreate` with a ReadWriteOnce volume, else rolling (maxUnavailable 0) |
| `config` | fs + sqlite + memory pub/sub, `auth.mode: none` | The server configuration (`backyard.yaml`) |
| `ports.http` / `ports.metrics` | `8080` / `9090` | Must match `config.server.listen` / `config.telemetry.metrics_listen` |
| `args` | `[serve, --config, /etc/backyard/backyard.yaml]` | Container arguments (`gateway` runs only the deploy gateway) |
| `existingSecret` | `""` | Use an existing Secret for the environment |
| `secret.create` / `secret.backyardSecret` / `secret.env` | `true` / generated / `{}` | Chart-managed Secret |
| `extraEnv` / `extraEnvFrom` | `[]` | More environment (supports `valueFrom`) |
| `persistence.enabled` | `true` | PVC for fs storage and SQLite (`emptyDir` when disabled) |
| `persistence.existingClaim` / `storageClass` / `accessModes` / `size` | `""` / default / `[ReadWriteOnce]` / `10Gi` | Volume settings (`storageClass: "-"` = no class) |
| `persistence.retain` | `true` | Keep the PVC on `helm uninstall` |
| `persistence.mountPath` | `/var/lib/backyard` | Must equal `config.storage.fs.root` |
| `service.type` / `service.port` | `ClusterIP` / `80` | Service |
| `ingress.enabled` / `className` / `annotations` | `false` / `""` / `{}` | Ingress |
| `ingress.hosts` | `[]` | Empty: `<base_domain>` and `*.<base_domain>` (host mode) |
| `ingress.tls` | `[]` | TLS sections (one wildcard certificate covers everything) |
| `networkPolicy.enabled` | `false` | NetworkPolicy admitting only `networkPolicy.from` to the HTTP port (needs an enforcing CNI) |
| `networkPolicy.from` / `metricsFrom` | the `ingress-nginx` namespace / `[]` | Peers allowed to reach the HTTP / metrics port |
| `serviceAccount.create` / `annotations` | `true` / `{}` | IRSA, GKE / Azure Workload Identity annotations |
| `serviceAccount.automountServiceAccountToken` | `false` | The server does not use the Kubernetes API |
| `podSecurityContext` / `securityContext` | non-root 65532, read-only root, no capabilities, RuntimeDefault seccomp | Pod hardening |
| `resources` | requests 100m / 128Mi, limit 1Gi | Resources |
| `goMemLimitFromResources` | `true` | Sets `GOMEMLIMIT` from the memory limit |
| `startupProbe` / `livenessProbe` / `readinessProbe` | `/__backyard/healthz`, `/__backyard/readyz` | Probes (readiness pings the database and storage) |
| `tmp.medium` / `tmp.sizeLimit` | `""` / `2Gi` | Writable `/tmp` |
| `autoscaling.*` | disabled, 2–10, 70% CPU | HorizontalPodAutoscaler |
| `podDisruptionBudget.*` | disabled, `minAvailable: 1` | PodDisruptionBudget |
| `serviceMonitor.*` | disabled, 30s | Prometheus Operator ServiceMonitor (`/metrics` on the metrics port) |
| `podAnnotations` / `podLabels` / `nodeSelector` / `tolerations` / `affinity` / `topologySpreadConstraints` / `priorityClassName` | empty | Scheduling |
| `extraVolumes` / `extraVolumeMounts` / `initContainers` / `extraContainers` | `[]` | Extensions (e.g. a people directory CSV for `identity.directory`) |

## Ingress and TLS

Sites are subdomains, so the ingress needs `<domain>` and `*.<domain>` (the chart adds both by
default) and a certificate covering both — a wildcard needs DNS-01 with cert-manager:

```yaml
ingress:
  enabled: true
  className: nginx
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt-dns
    # websockets + streaming AI responses + deploy uploads
    nginx.ingress.kubernetes.io/proxy-read-timeout: "3600"
    nginx.ingress.kubernetes.io/proxy-send-timeout: "3600"
    nginx.ingress.kubernetes.io/proxy-buffering: "off"
    nginx.ingress.kubernetes.io/proxy-body-size: "100m"
  tls:
    - secretName: backyard-tls
      hosts: [backyard.acme.com, "*.backyard.acme.com"]
```

Without wildcard DNS/TLS, set `config.server.url_mode: path` (sites at `/s/<site>/`); the
ingress then only needs the base domain. `config.server.trusted_proxies` (default: the private
ranges, i.e. any pod) is where `X-Forwarded-For`, `-Proto` and `-Host` are honored from, so client
IPs in logs and per-IP rate limits are the real ones. Narrow it to your ingress controller's pod
network if you can.

## Identity-aware proxies

With an identity-aware proxy in front (Google IAP, AWS ALB OIDC, oauth2-proxy, Pomerium,
Cloudflare Access) use `auth.mode: iap | alb | header` instead of OIDC; the deploy gateway path
`/__backyard/api/deploy/*` (and `/__backyard/api/tokens` with an `Authorization` header, for
`backyard token create`) must then be reachable by CLIs (see the main documentation).

`iap`, `alb` and `header` with `auth.header.jwt_header` verify a signed assertion, so no peer can
forge an identity just by sending headers. Plain `header` mode (for example oauth2-proxy's
`X-Forwarded-Email` without `jwt_header`) trusts the identity headers of **every peer in
`config.server.trusted_proxies`**:
with the default private ranges, any pod in the cluster (and any host that can route to pod IPs)
could send `X-Forwarded-Email: admin@acme.com` straight to the Service and be signed in as that
admin. The chart therefore refuses to render header mode without `jwt_header` unless one of these
holds:

* **`networkPolicy.enabled: true`** (recommended), with `networkPolicy.from` admitting only the
  peers that set the identity headers: the ingress controller (oauth2-proxy through
  `auth-url` / `auth-response-headers`) or the oauth2-proxy / Pomerium pods when they proxy
  upstream. Every route through those peers must set or strip the identity headers itself; the
  NetworkPolicy only keeps everyone else out. It needs a CNI that enforces NetworkPolicy, and it
  never blocks a pod's own node (the kubelet, `hostNetwork` pods), so prefer a `trusted_proxies`
  range that covers the pod network but not the nodes' addresses.
* `config.server.trusted_proxies` lists only the proxy's own addresses: single IPs, IPv4 ranges of
  /24 or longer, IPv6 ranges of /120 or longer (for example a proxy outside the cluster).
* Or verify a signed JWT instead: `auth.header.jwt_header` + `jwks_url` (Pomerium, Cloudflare
  Access and other proxies that forward a signed assertion; see
  [authentication](../../../docs/authentication.md#header-trusted-identity-headers)).

```yaml
config:
  auth:
    mode: header
    header:
      email_header: X-Forwarded-Email
      groups_header: X-Forwarded-Groups
networkPolicy:
  enabled: true
  from:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: ingress-nginx
      podSelector:
        matchLabels:
          app.kubernetes.io/name: ingress-nginx
```

## Claude connector

The [Claude connector](../../../docs/claude-connector.md) lets people publish sites from Claude
with their company sign-in. Turn it on in `config` (it needs an https `public_url` without a
path, which the default `https://<base_domain>` is):

```yaml
config:
  mcp:
    enabled: true
ingress:
  annotations:
    nginx.ingress.kubernetes.io/proxy-body-size: "10m"   # or more; the example above uses 100m
```

- **Body size.** Publish calls carry up to 9 MiB (`mcp.max_publish_bytes` is 4 MiB of file
  content, the body limit 2 × that + 1 MiB). ingress-nginx allows only 1 MiB by default, so set
  `proxy-body-size` to at least `10m`; other controllers have similar limits.
- **Identity-aware proxies.** With IAP, ALB OIDC, oauth2-proxy, Pomerium or Cloudflare Access in
  front, exempt the four machine paths from the proxy's login — Claude calls them without a
  browser session, and they never read identity headers:
  `/__backyard/mcp`, `/__backyard/oauth/*`, `/.well-known/oauth-authorization-server` and
  `/.well-known/oauth-protected-resource/__backyard/mcp`. Keep `/__backyard/auth/authorize`
  behind the proxy: its login is the connector's sign-in. Recipes per proxy:
  [Claude connector](../../../docs/claude-connector.md#machine-paths-and-identity-proxies).
- **Reachability.** Hosted Claude calls the hub from `160.79.104.0/21` (IPv4), and Backyard
  fetches Claude's client metadata from `https://claude.ai` (without egress, add
  `clients: []` to `config.mcp`). An internal-only ingress works only with connectors added
  locally in Claude Code.

## Owners-only site listing

By default the hub, `backyard sites list` and `backyard_list_sites` show every site to everyone.
To show each person only the sites they own (and admins every site), set
[`sites.listing`](../../../docs/deploying-sites.md#owners-only-site-listing) in `config`, at
install or with any later `helm upgrade` (the config checksum rolls the pods):

```yaml
config:
  sites:
    listing: owners
```

It implies `sites.ownership: owner` (an explicit `ownership: none` next to it makes the server
refuse to start), and it hides sites from the directory without restricting who can open them.
Name admins with `config.auth.admins` or `admin_groups`. The configuration is strict and servers
older than this option reject the key, so set it with (or after) the chart version that brings
the new image, and keep `image.tag` at that version or later.

## Cloud storage without keys

| Cloud | `config.storage` | Service account annotation |
|---|---|---|
| AWS | `type: s3`, `s3: {bucket, region}` | `eks.amazonaws.com/role-arn: arn:aws:iam::<acct>:role/<role>` |
| GCP | `type: gcs`, `gcs: {bucket}` | `iam.gke.io/gcp-service-account: <sa>@<project>.iam.gserviceaccount.com` |
| Azure | `type: azure`, `azure: {account, container}` | `azure.workload.identity/client-id: <client-id>` + `podLabels: {azure.workload.identity/use: "true"}` |

With bucket storage, set `persistence.enabled: false`. The Terraform modules in
`deploy/terraform/{aws,gcp,azure}` create the buckets, databases and identities.

## Operations

* **Upgrades** — `helm upgrade` rolls the pods when `config` or `secret.env` change. Database
  migrations run on start (`database.postgres.auto_migrate`).
* **Backups** — fs storage: snapshot the PVC (VolumeSnapshot / Velero); SQLite is consistent in a
  crash-consistent snapshot thanks to its WAL. Postgres/S3: use the managed services' backups.
* **Secret rotation** — changing `BACKYARD_SECRET` signs everyone out and revokes all API tokens.
* **Uninstall** — `helm uninstall backyard` keeps the data PVC (`persistence.retain`); delete it
  with `kubectl delete pvc backyard-data` when you are sure.

## Development

```sh
make helm-lint                    # helm lint --strict + template, and the chart's guard tests
make validate-configs             # renders the ConfigMaps and checks them against the config schema
go test ./deploy/helm/...         # the guard and NetworkPolicy tests alone (skipped without helm)
```
