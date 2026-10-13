# Storage and databases

Backyard keeps three kinds of state, each behind a pluggable backend:

| State | Holds | Backends (`type`) |
|---|---|---|
| **Blob storage** (`storage`) | site files, upload bytes, generated images, site metadata and manifests | `fs`, `s3`, `gcs`, `azure` |
| **Document store** (`database`) | `backyard.db` documents, upload metadata, the user cache, audit records | `sqlite`, `postgres`, `dynamodb`, `memory` |
| **Pub/sub** (`pubsub`) | realtime fan-out between server instances (nothing persistent) | `memory`, `postgres`, `redis` |

Pick per environment:

| Setup | storage | database | pubsub | Instances |
|---|---|---|---|---|
| Laptop / `serve --dev` | `fs` | `sqlite` | `memory` | 1 |
| One VM, no cloud bucket (systemd, Docker Compose) | `fs` | `sqlite` or `postgres` | `memory` (or `postgres`) | 1 |
| Kubernetes, small | `fs` on a PVC | `sqlite` | `memory` | 1 |
| AWS | `s3` | `dynamodb` or `postgres` (RDS) | `redis` (ElastiCache/Valkey) or `postgres` | many |
| Google Cloud | `gcs` | `postgres` (Cloud SQL) | `postgres` | many |
| Azure | `azure` | `postgres` (Flexible Server) | `postgres` | many |

Running more than one instance requires a shared database (`postgres` or `dynamodb`), a shared
blob store (a bucket, or a ReadWriteMany volume for `fs`) and a shared pub/sub (`postgres` or
`redis`) — see [operations](operations.md#scaling-to-several-instances).

## Blob storage

Keys (after an optional per-backend `prefix`):

| Key | Content |
|---|---|
| `sites/<site>/<path>` | site files — exactly what `rsync`, `aws s3 sync`, `gcloud storage rsync` and `az storage blob sync` write |
| `_meta/sites/<site>.json` | site metadata (title, description, owners, last deploy) |
| `_meta/manifests/<site>.json` | the file list with sizes, SHA-256 digests and ETags (for diffed deploys) |
| `uploads/<site>/<id>/<name>` | `backyard.files` uploads and generated images |

A small in-memory LRU cache (`storage.cache_bytes`, 64 MiB; `storage.cache_ttl`, 5 s) fronts
reads of objects up to 1 MiB.

### `fs` — no bucket at all

```yaml
storage:
  type: fs
  fs:
    root: /var/lib/backyard
deploy:
  rsync_target: deploy@backyard.acme.com:/var/lib/backyard/sites
```

Everything lives under one directory: `sites/`, `uploads/`, `_meta/` and, by default, the
SQLite file `backyard.db`. This is the "drop a folder on a VM" option, and the target of
`backyard deploy --method rsync`: any tool that writes into `sites/<site>/` deploys. Writes are
atomic (temporary file + rename); all access is confined to the root (symlinks are only
followed within their own directory subtree). ETags derive from size and modification time, so
rsync'ed changes are detected.

It also works on mounted buckets (gcsfuse, blobfuse2) and on ReadWriteMany volumes (NFS, EFS,
Azure Files) for several instances. It does **not** work on mountpoint-s3 (no renames) — use
`s3` there. The fs store cannot presign URLs, so `api` deploys upload through the gateway's own
signed upload endpoint.

### `s3` — Amazon S3 and S3-compatible stores

```yaml
storage:
  type: s3
  s3:
    bucket: acme-backyard
    region: us-east-1
    # prefix: backyard/
    # sse_kms_key_id: arn:aws:kms:…            # write with SSE-KMS
    # endpoint: https://<account>.r2.cloudflarestorage.com   # R2, MinIO, Ceph, B2…
    # access_key_id: ${S3_ACCESS_KEY_ID}       # default: the AWS credential chain
    # secret_access_key: ${S3_SECRET_ACCESS_KEY}
```

Credentials come from the default AWS chain (IRSA, ECS task role, instance profile,
environment) unless static keys are set. Presigned uploads sign the exact size (a signed
`Content-Length`, or the `Content-MD5` of the empty body for an empty file) and — on AWS — the
SHA-256 (`x-amz-checksum-sha256`), so S3 rejects any other body. With a custom `endpoint`,
path-style addressing is used and only required checksums are sent (many S3-compatible stores
reject the newer checksum headers; `AWS_REQUEST_CHECKSUM_CALCULATION` overrides this). The
bucket needs CORS for `PUT` from the hub origin if you use drag-and-drop deploys.

IAM for the server: `s3:GetObject`, `s3:PutObject`, `s3:DeleteObject`, `s3:ListBucket` on the
bucket (plus KMS permissions with SSE-KMS).

### `gcs` — Google Cloud Storage

```yaml
storage:
  type: gcs
  gcs:
    bucket: acme-backyard
    # credentials_file: /etc/backyard/sa.json   # default: Application Default Credentials
    # signer_email: backyard@acme.iam.gserviceaccount.com
```

On Cloud Run and GKE there is no private key, so upload URLs are signed through the IAM
Credentials API as `signer_email` (default: the detected service account), which needs
`roles/iam.serviceAccountTokenCreator` on itself. Presigned uploads pin the size
(`x-goog-content-length-range`); GCS cannot verify a SHA-256 digest, so a commit reads the
files it has not seen before to record their digest. For drag-and-drop deploys from the hub,
the bucket's CORS rule must allow `PUT` from the hub origin with the `Content-Type` and
`x-goog-content-length-range` headers (the Google Cloud module sets this up).

### `azure` — Azure Blob Storage

```yaml
storage:
  type: azure
  azure:
    account: acmebackyard
    container: backyard
    # connection_string: ${AZURE_STORAGE_CONNECTION_STRING}   # Azurite / shared key
```

Authentication: `connection_string`, else `account` + `account_key`, else
`DefaultAzureCredential` (managed identity, workload identity, Azure CLI). Upload URLs are
service SAS URLs (shared key) or user-delegation SAS URLs (the identity needs *Storage Blob
Delegator*, included in *Storage Blob Data Contributor*). A SAS cannot restrict the uploaded
body at all, so only the commit's size check guards these uploads (see below), and it deletes
objects of the wrong size. Directory placeholder blobs of hierarchical-namespace (ADLS Gen2)
accounts are ignored.

### Upload integrity

Deploys through the gateway (`api`) upload each file to a URL bound to the size and SHA-256
the deploy plan declared, as far as the store can enforce it:

| Store | Upload URL | Enforced by the store |
|---|---|---|
| `fs` (or `deploy.presign: false`) | the gateway's own `…/uploads/<token>` | size and SHA-256 (a mismatching body is never stored) |
| `s3` | presigned PUT | size; SHA-256 on AWS (and on custom endpoints with `AWS_REQUEST_CHECKSUM_CALCULATION=when_supported`) |
| `gcs` | V4 signed URL | size |
| `azure` | SAS URL | nothing |

The commit is the second line of defence: every listed file must exist with its declared size,
or the commit fails with `409 conflict` naming the paths. Where the upload URLs bind the size
(the gateway's own endpoint, S3, GCS), an object of another size can only be the verified
upload of another, overlapping deploy (or a direct sync), so it stays and only this commit
fails. With presigned Azure SAS uploads, which bind nothing, a wrong-size object that is not
the version the previous manifest recorded is deleted, so no unverified object outlives the
commit. For each file whose stored object the previous manifest does not describe, the commit
records the SHA-256 of the bytes stored **now**: S3 reports the checksum it verified itself
(`x-amz-checksum-sha256`, one `HeadObject` per file), other stores are read and hashed. The
manifest therefore never pairs one deploy's digest with another deploy's bytes, and later
diffs stay correct. Uploads write straight to `sites/<site>/<path>` (deploys are not atomic,
like rsync), so the uploads of a deploy that is never committed stay until a later commit
deletes them as unlisted (or the site is deleted). See
[SPEC §7.3](spec/SPEC.md#73-deploy-diffed-rsync-semantics).

## Document store

All backends implement the same JSON document model with exactly the same query semantics
(type-strict comparisons, missing fields never match, a fixed cross-type sort order,
microsecond timestamps) — a shared conformance suite (`internal/docstore/conformance`) runs
against each of them and against the Cloudflare edition. See
[SPEC §5.2](spec/SPEC.md#52-database).

Limits: `database.max_doc_bytes` (256 KiB per document), `database.max_list` (1000 per page)
and `database.max_docs_per_site` (100,000 per site, a soft quota; a negative value means
unlimited).

### `sqlite` — single node, zero setup

```yaml
database:
  type: sqlite
  sqlite:
    path: /var/lib/backyard/backyard.db     # default: <storage.fs.root>/backyard.db
```

Pure Go (no cgo), WAL mode, one writer connection plus a read-only pool. Filters, sorting and
pagination run in SQL. One server instance only — the file must not be shared by several
servers.

### `postgres` — PostgreSQL

```yaml
database:
  type: postgres
  postgres:
    dsn: ${DATABASE_URL}          # postgres://backyard:…@db:5432/backyard?sslmode=require
    # max_conns: 20
    # schema: public
    # auto_migrate: true
```

Works with RDS, Aurora, Cloud SQL, AlloyDB, Azure Database for PostgreSQL Flexible Server,
Neon and self-hosted PostgreSQL. Each instance keeps a pool of up to `max_conns` connections
(default 20; it takes precedence over a `pool_max_conns` parameter in the DSN), plus those of
Postgres pub/sub if you use it (below). Documents live in `<schema>.backyard_docs` (JSONB);
migrations run on start, serialized across instances with an advisory lock and recorded in
`<schema>.backyard_schema_version`. Queries run in SQL with bound parameters. PostgreSQL cannot
store the NUL character in JSONB strings, so such documents are rejected.

The role needs to create tables in the schema when `auto_migrate` is on (otherwise run the
server once with a privileged role).

### `dynamodb` — Amazon DynamoDB

```yaml
database:
  type: dynamodb
  dynamodb:
    table: backyard
    region: us-east-1
    # create_table: true             # create it (on-demand) if missing
    # endpoint: http://localhost:8000  # DynamoDB Local
    # max_scan: 10000
```

One table with string keys `pk` (`<site>#<collection>`) and `sk` (document id). Writes are
conditional and updates use optimistic concurrency. DynamoDB cannot filter inside JSON, so a
query reads up to `max_scan` items of the collection (in id order, at most 64 MiB) and filters,
sorts and paginates in memory: results are exact for collections up to that size, and each query
costs read capacity proportional to the collection. Keep `database.max_doc_bytes` below ~390000
(400 KB item limit). Good for serverless installs with modest collections; prefer Postgres for
large, query-heavy sites.

IAM: `dynamodb:GetItem`, `PutItem`, `UpdateItem`, `DeleteItem`, `Query`, `BatchWriteItem`,
`DescribeTable` (and `CreateTable` with `create_table`).

### `memory`

Maps in process memory; everything is lost on restart. For tests and throwaway demos.

## Pub/sub

Realtime events (database changes, room messages, presence) are delivered to the clients of
the instance that produced them and published to the broker for the others. Delivery is
at-most-once: during a broker outage live events are lost (clients using `watch` re-fetch after
reconnecting).

| Type | Configuration | Notes |
|---|---|---|
| `memory` | — | Single instance only. |
| `postgres` | `pubsub.postgres.dsn` (default: `database.postgres.dsn`), `channel` (`backyard_events`) | LISTEN/NOTIFY; no extra infrastructure when documents are in Postgres. Messages over the 8000-byte NOTIFY limit spill into the `backyard_pubsub_spill` table. Needs a direct (session-mode) connection, not a transaction-pooling proxy. Each instance opens one LISTEN connection plus a publishing pool of 2–4 connections (half the CPU count, clamped); a `pool_max_conns` parameter in the DSN sets that pool's size instead — also when the DSN is inherited from `database.postgres.dsn` (whose document store pool follows `max_conns` regardless). |
| `redis` | `pubsub.redis.url` (`redis://` or `rediss://`) | Redis, Valkey, ElastiCache, Memorystore. Channels are prefixed `backyard:` (or `backyard:<db>:`), so several installs can share a server. |

## Testing backends locally

Contributors run Postgres, Redis, DynamoDB Local and Azurite with
[`deploy/docker-compose/test-services.yml`](../deploy/docker-compose/test-services.yml)
(`make services-up`); see [CONTRIBUTING](../CONTRIBUTING.md#integration-tests).
