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
`persistence.mountPath`, or a missing `base_domain`.

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
ingress then only needs the base domain. Keep `config.server.trusted_proxies` matching your
ingress controller's pod network so client IPs and `X-Forwarded-Proto` are honored.

With an identity-aware proxy in front (Google IAP, AWS ALB OIDC, oauth2-proxy, Pomerium,
Cloudflare Access) use `auth.mode: iap | alb | header` instead of OIDC; the deploy gateway path
`/__backyard/api/deploy/*` must then be reachable by CLIs (see the main documentation).

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
make helm-lint                    # helm lint --strict + template (default and production values)
make validate-configs             # renders the ConfigMaps and checks them against the config schema
```
