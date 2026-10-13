# Data warehouse

`backyard.warehouse` lets internal pages chart company data — KPIs, funnels, usage — without
holding database credentials. The server runs **read-only** queries against BigQuery,
PostgreSQL-compatible engines (Redshift, AlloyDB, Aurora, Neon, CockroachDB…) or Athena, with
its own credentials, row caps, timeouts and per-user rate limits.

```js
const { rows } = await backyard.warehouse.named("weekly_active_users", { team: "growth" });
const r = await backyard.warehouse.query("SELECT day, n FROM dau WHERE team = ? ORDER BY day", ["growth"]);
const { sources, queries } = await backyard.warehouse.sources();
// r = { columns: [{ name: "day", type: "DATE" }, …], rows: [{ day: "2026-01-01", n: 120 }, …], row_count, truncated }
```

The warehouse is off until `warehouse.sources` has an entry (`501 unsupported`); it is not
available on the Cloudflare edition.

## Configuration

```yaml
warehouse:
  # allow_raw_sql: false       # true: pages may send their own SELECT statements
  # max_rows: 10000
  # timeout: 60s
  sources:
    default:
      type: bigquery
      project_id: acme-analytics
      location: US
      max_bytes_billed: 10000000000      # 10 GB per query
    redshift:
      type: postgres
      dsn: ${REDSHIFT_DSN}               # a read-only role!
      description: Finance warehouse
    lake:
      type: athena
      region: us-east-1
      workgroup: backyard
      database: analytics
      output_location: s3://acme-athena-results/backyard/
  queries:
    weekly_active_users:
      source: default
      description: Weekly active users for a team
      params: [team]
      sql: |
        SELECT day, users FROM analytics.wau
        WHERE team = @team
        ORDER BY day
```

Every key is listed in the [configuration reference](configuration.md#warehouse).

## Named queries vs raw SQL

**Named queries** are the safe default: the SQL is written by whoever operates Backyard, pages
only choose a name and pass parameters. `@name` placeholders are bound from the `params` object
(never interpolated); every declared parameter is required and unknown ones are rejected
(`400`). Named queries are not subject to the statement guard below, so they can do anything
the source's role allows — keep that role read-only.

**Raw SQL** (`warehouse.allow_raw_sql: true`) lets pages send their own statements. Positional
`?` placeholders (outside literals, quoted identifiers and comments) are translated per engine
(`$1` for Postgres, `?` for BigQuery and Athena), and `params` must be an array of scalars with
one value per placeholder. A statement guard enforces a **single** statement starting with
`SELECT` or `WITH` (after whitespace, comments and parentheses), using the engine's lexical
rules — for Postgres sources the check must pass under every quoting variant
Postgres-compatible engines use. Constructs engines disagree on (nested block comments, NUL
bytes, invalid UTF-8) are rejected. `warehouse.allow_any_statement: true` disables the guard;
don't.

Remember that with raw SQL **everyone who can open any Backyard site can query everything the
source's credentials can read**. Scope the role accordingly, or keep raw SQL off.

## Read-only by construction

Defense in depth, in this order of importance:

1. **A read-only role at the source.** This is the real boundary.
   - BigQuery: a service account with `roles/bigquery.jobUser` on the billing project and
     `roles/bigquery.dataViewer` only on the datasets pages may see. Cap cost with
     `max_bytes_billed`.
   - PostgreSQL / Redshift: a role with `SELECT` on the allowed schemas and nothing else
     (`CREATE ROLE backyard_ro LOGIN …; GRANT USAGE ON SCHEMA analytics TO backyard_ro; GRANT
     SELECT ON ALL TABLES IN SCHEMA analytics TO backyard_ro;`). `READ ONLY` transactions do
     not stop side effects of functions such as `dblink_exec` or `pg_terminate_backend`, so do
     not grant those.
   - Athena: an IAM role limited to `athena:StartQueryExecution`/`GetQueryExecution`/
     `GetQueryResults`/`StopQueryExecution`, Glue read access to the allowed databases, read
     access to the data in S3 and write access to the results location only.
2. **Read-only transactions** (Postgres sources): every query runs in its own `READ ONLY`
   transaction that is always rolled back, over the extended protocol (one statement, no
   multi-statement strings that could `COMMIT`), with `statement_timeout` from the deadline.
3. **The statement guard** for raw SQL (above).
4. **Caps:** `warehouse.max_rows` (10,000 rows, then `truncated: true`; Postgres cancels larger
   results server-side), `warehouse.timeout` (60 s; BigQuery jobs and Athena queries are
   cancelled when the caller goes away), `ratelimit.warehouse_per_minute` (30 per user).

SQL text and parameter values are never logged.

## Results

`{columns: [{name, type}], rows: [{column: value}], row_count, truncated}`. `type` is the
engine's type name (`INT64`, `DATE`, `int4`, `varchar`…). Values are JSON-friendly:

| Engine type | JSON |
|---|---|
| integers | number |
| floats | number (`"NaN"`, `"Infinity"` as strings) |
| NUMERIC / DECIMAL | number when lossless, else the exact decimal as a string |
| timestamps | RFC 3339 string in UTC |
| dates | `"YYYY-MM-DD"` |
| bytes | base64 string |
| JSON / STRUCT / ARRAY | decoded value (Athena keeps its display format for array/map/row — `CAST` to JSON for structured values) |

Pages should convert big numbers with `Number()` before charting.

## Errors

| Situation | Response |
|---|---|
| No warehouse configured | `501 unsupported` |
| Raw SQL disabled, or the statement guard rejects it | `403 forbidden` |
| Unknown source or named query | `404 not_found` |
| Missing / unknown parameters, placeholder count mismatch | `400 bad_request` |
| Query failed or timed out at the engine | `502` / `504 upstream_error` |
| Over `warehouse_per_minute` | `429 rate_limited` |

`GET /__backyard/api/warehouse` lists sources (name, type, description) and named queries
(name, description, params, source) — never SQL or credentials. Metrics:
`backyard_warehouse_queries_total{source,result}`.
