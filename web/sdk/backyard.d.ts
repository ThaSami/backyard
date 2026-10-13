/**
 * Backyard client SDK — TypeScript declarations (normative, SPEC §5.6).
 *
 * Every Backyard site gets a zero-config backend. Include the SDK with an
 * ABSOLUTE path (works in host mode `https://<site>.<domain>/` and path mode
 * `https://<domain>/s/<site>/` alike):
 *
 * ```html
 * <script src="/__backyard/backyard.js"></script>          <!-- defines window.backyard -->
 * <script type="module">
 *   import backyard, { db, ai } from "/__backyard/backyard.mjs";
 * </script>
 * ```
 *
 * Everything is scoped to the current site: collections, files and rooms of
 * one site are invisible to other sites. All secrets (AI keys, warehouse
 * credentials) stay on the server; the browser only talks to `/__backyard/api`.
 *
 * Conventions:
 * - Every method returns a Promise unless documented as synchronous.
 * - Failures reject with {@link Backyard.BackyardError} (`err.code` tells what happened).
 * - On `401` with a login URL the SDK sends the top-level page to the login
 *   page (disable with `backyard.configure({ autoLogin: false })`).
 * - Every state-changing request carries the `X-Backyard: 1` CSRF header.
 *
 * Quick tour:
 * ```js
 * const me = await backyard.me();                       // who is viewing
 * const posts = backyard.db.collection("posts");
 * const post = await posts.create({ title: "Hello", votes: 0 });
 * await posts.update(post.id, { votes: post.votes + 1 });
 * const top = await posts.where("votes", ">=", 3).orderBy("votes", "desc").limit(10).get();
 * const stop = posts.watch((docs) => render(docs));     // live query
 * const { url } = await backyard.files.upload(file);
 * const { content } = await backyard.ai.chat("Summarize: " + text);
 * const room = backyard.socket.room("lobby", { state: { name: me.name } });
 * room.on("members", (members) => renderPresence(members));
 * ```
 */
declare namespace Backyard {
  // ---------------------------------------------------------------------------
  // Basics

  /** Any JSON value. */
  type JSONValue = string | number | boolean | null | JSONValue[] | { [key: string]: JSONValue };

  /** Plain document data (any JSON object). */
  type Data = Record<string, any>;

  /**
   * Error codes. The first group comes from the server (SPEC §5); the second
   * is produced by the SDK itself (`status` is `0` for those).
   */
  type ErrorCode =
    | "bad_request"
    | "unauthenticated"
    | "forbidden"
    | "not_found"
    | "conflict"
    | "too_large"
    | "rate_limited"
    | "unsupported"
    | "upstream_error"
    | "internal"
    /** The network request failed (offline, DNS, CORS, connection reset). */
    | "network_error"
    /** The request was aborted via an AbortSignal or `stream.abort()`. */
    | "aborted"
    /** The server answered 2xx with a body that is not JSON (e.g. a proxy login page). */
    | "bad_response"
    | (string & {});

  /**
   * The error every SDK call rejects with.
   *
   * ```js
   * try { await posts.create({ title }, { id: slug }); }
   * catch (err) {
   *   if (err instanceof backyard.BackyardError && err.code === "conflict") alert("Slug taken");
   *   else throw err;
   * }
   * ```
   */
  class BackyardError extends Error {
    constructor(code: ErrorCode, message?: string, options?: { status?: number; loginUrl?: string; retryAfter?: number; cause?: unknown });
    /** Always `"BackyardError"`. */
    readonly name: "BackyardError";
    /** Machine-readable error code, e.g. `"not_found"`, `"rate_limited"`. */
    code: ErrorCode;
    /** HTTP status (`0` for client-side failures such as `network_error`). */
    status: number;
    /** Human-readable explanation. */
    message: string;
    /** Present on `401` responses when a browser login is possible. */
    loginUrl?: string;
    /** Seconds to wait before retrying (from `Retry-After`, on `429`/`503`). */
    retryAfter?: number;
  }

  // ---------------------------------------------------------------------------
  // Context

  /** Where the page runs (resolved synchronously when the script loads; SPEC §2.1). */
  interface SiteInfo {
    /** Site name (`"lunch-vote"`), or `null` on the hub. */
    readonly name: string | null;
    /** Absolute URL of the site root, e.g. `https://lunch-vote.backyard.acme.com/`. */
    readonly url: string;
    /** Absolute API base, e.g. `https://lunch-vote.backyard.acme.com/__backyard`. */
    readonly apiBase: string;
    /** `"host"`: one subdomain per site; `"path"`: sites under `/s/<site>/`. */
    readonly mode: "host" | "path";
  }

  /** Options for {@link Backyard.SDK.configure}. */
  interface ConfigureOptions {
    /**
     * Override the API base (escape hatch, e.g. for a local proxy). Relative
     * URLs resolve against the page; `null`/`""` restores the default. The
     * same can be set before the script loads with
     * `window.BACKYARD_CONFIG = { apiBase: "…" }`.
     */
    apiBase?: string | null;
    /**
     * Redirect the top-level page to the login page when the API answers
     * `401` with a login URL (default `true`). Set `false` to handle
     * `err.code === "unauthenticated"` / `err.loginUrl` yourself.
     */
    autoLogin?: boolean;
  }

  // ---------------------------------------------------------------------------
  // Identity

  /** The signed-in person (or an anonymous guest when the server runs with `auth.mode: none`). */
  interface User {
    /** Stable id: lowercased email, else `"sub:<issuer-sub>"`, `"token:<name>"` for static tokens, or `"anon:<hex>"` for guests. */
    id: string;
    email?: string;
    /** Display name (`"Guest 1a2b"` for anonymous users). */
    name: string;
    /** Job title, from the IdP or the company directory. */
    title?: string;
    team?: string;
    /** Slack handle, e.g. `"@alice"`. */
    slack?: string;
    avatar_url?: string;
    groups?: string[];
    /** `true` for guests (no company login). */
    anonymous: boolean;
    /** Backyard administrator. */
    admin: boolean;
    /** Extra mapped claims, e.g. `{ location: "Ottawa" }`. */
    extra?: Record<string, any>;
  }

  /** The public part of a {@link User}, embedded in realtime messages and presence. */
  interface UserSummary {
    id: string;
    name: string;
    email?: string;
    avatar_url?: string;
  }

  interface IdentityAPI {
    /** The current user. Cached after the first successful call. */
    me(): Promise<User>;
    /**
     * Looks up a person who has used Backyard (or is in the company
     * directory) by email. Resolves `null` when unknown.
     */
    lookup(email: string): Promise<User | null>;
  }

  // ---------------------------------------------------------------------------
  // Database

  /** System metadata of every document (read-only; never write it). */
  interface DocMeta {
    /** RFC 3339 timestamp. */
    created_at: string;
    updated_at: string;
    /** User id of the creator / last writer. */
    created_by: string;
    updated_by: string;
    /** Starts at 1, incremented by every write. */
    version: number;
  }

  /**
   * A stored document: your data, flattened, plus `id` and `_meta`.
   * `{ id: "01J…", title: "Hello", status: "draft", _meta: { created_at, … } }`
   */
  type Doc<T extends Data = Data> = T & { id: string; _meta: DocMeta };

  /**
   * An array of documents with a non-enumerable `hasMore` flag: more results
   * exist after this page. A page can hold fewer than `limit` documents and
   * still have `hasMore` (the Cloudflare edition ends a page at about 8 MiB
   * of documents), so to page, advance `offset` by the number of documents
   * received, not by `limit`.
   */
  type DocList<T extends Data = Data> = Doc<T>[] & { readonly hasMore: boolean };

  /**
   * Query operators (SPEC §5.2). Comparisons only match values of the same
   * JSON type (`"3"` never equals `3`); a missing field never matches.
   * - `in` / `not-in`: value is an array of up to 30 scalars.
   * - `contains`: the field is an array containing the (scalar) value.
   * The aliases `"="` and `"array-contains"` are accepted too.
   */
  type Op = "==" | "!=" | "<" | "<=" | ">" | ">=" | "in" | "not-in" | "contains";

  /** A JSON scalar usable in queries. */
  type Scalar = string | number | boolean | null;

  /** One filter: `field` is a dot path into your data (`"author.name"`) or `id` / `_meta.created_at` / `_meta.updated_at` / `_meta.created_by` / `_meta.updated_by`. */
  interface WhereClause {
    field: string;
    op: Op;
    value: Scalar | Scalar[];
  }

  interface OrderClause {
    field: string;
    dir: "asc" | "desc";
  }

  /**
   * The wire query object accepted by {@link Collection.find}. `orderBy` may
   * be used instead of `order_by` and accepts `"field"` / `"field:desc"`
   * strings; `where` entries may be `[field, op, value]` tuples.
   */
  interface QueryObject {
    where?: Array<WhereClause | [string, Op, Scalar | Scalar[]]>;
    order_by?: OrderClause[];
    orderBy?: string | string[] | OrderClause | OrderClause[];
    /** Default 100, max `database.max_list` (default 1000). */
    limit?: number;
    offset?: number;
  }

  /** Options for {@link Collection.list}. */
  interface ListOptions {
    /** Default 100, max 1000. */
    limit?: number;
    offset?: number;
    /** Field to sort by (or several). Default: creation time, then id. */
    orderBy?: string | string[];
    /** Direction for `orderBy` (default `"asc"`). `{ order: "desc" }` alone = newest first. */
    order?: "asc" | "desc";
  }

  /**
   * A realtime change event (SPEC §6 `db` frame).
   *
   * The server may send a create or update without its document (`"doc": null`
   * plus the id) when the event would be too large to broadcast: the Go
   * server does so for documents of about 1 MiB or more, possible when
   * `database.max_doc_bytes` is raised. {@link Collection.subscribe} then
   * re-reads the document (`GET /db/{collection}/{id}`) before delivering the
   * event, so `doc` is the current document, which may already include later
   * changes.
   */
  interface ChangeEvent<T extends Data = Data> {
    op: "create" | "update" | "delete";
    collection: string;
    /** Id of the changed document. */
    id: string;
    /**
     * The document after the change. `null` for deletes, and for a create or
     * update whose document the server sent without content and the SDK could
     * not re-read (the error goes to {@link SubscribeHandlers.onError}).
     */
    doc: Doc<T> | null;
  }

  /**
   * Callbacks for {@link Collection.subscribe}. Events are delivered in the
   * order they arrive. `onCreate` and `onUpdate` always get a full document:
   * when an event arrives without one (see {@link ChangeEvent}), the SDK
   * re-reads it first and later events wait for that read.
   */
  interface SubscribeHandlers<T extends Data = Data> {
    /**
     * A document was created. Never called with `null`: if the document has
     * to be re-read and is already gone, the event is delivered as a delete;
     * if the re-read fails, `onCreate` is skipped and `onError` is called.
     */
    onCreate?(doc: Doc<T>): void;
    /** A document was changed. Never called with `null` (same rules as `onCreate`). */
    onUpdate?(doc: Doc<T>): void;
    /**
     * A document was deleted. Can also be called for a create or update
     * whose document was deleted before the SDK could re-read it, so it may
     * name a document `onCreate` never reported, or be called twice for one
     * delete: remove by id and ignore unknown ids.
     */
    onDelete?(id: string): void;
    /** Called for every event (after the specific callback). */
    onChange?(event: ChangeEvent<T>): void;
    /**
     * The server rejected the subscription (e.g. invalid collection name),
     * or the document of a create or update that arrived without one could
     * not be re-read. In that case only `onChange` gets the event, with
     * `doc: null`; re-fetch (or use {@link Collection.watch}) to catch up.
     */
    onError?(error: BackyardError): void;
  }

  /** Options for live queries. */
  interface WatchOptions {
    /** Called when (re)fetching the result fails. Default: a console warning. */
    onError?(error: BackyardError): void;
  }

  /** Stops a subscription or live query. Idempotent. */
  type Unsubscribe = () => void;

  /**
   * An immutable, chainable query. Every builder method returns a NEW query.
   * ```js
   * const open = todos.where("done", "==", false);
   * const mine = await open.where("owner", "==", me.id).orderBy("due").limit(20).get();
   * ```
   */
  interface Query<T extends Data = Data> {
    /** The collection this query reads. */
    readonly collection: Collection<T>;
    /** Adds a filter (all filters must match). Throws `bad_request` synchronously for unknown operators. */
    where(field: string, op: Op, value: Scalar | Scalar[]): Query<T>;
    /** Adds a sort key (ties are broken by `id`). Missing/null values sort first ascending, last descending. */
    orderBy(field: string, dir?: "asc" | "desc"): Query<T>;
    limit(n: number): Query<T>;
    offset(n: number): Query<T>;
    /** Runs the query (`POST /db/{c}/query`). */
    get(): Promise<DocList<T>>;
    /** The first match, or `null`. */
    first(): Promise<Doc<T> | null>;
    /** Live query: see {@link Collection.watch}. */
    watch(callback: (docs: DocList<T>) => void, options?: WatchOptions): Unsubscribe;
    /** The wire representation (SPEC §5.2). */
    toJSON(): QueryObject;
  }

  /**
   * A collection of JSON documents, scoped to the current site. Collections
   * are created implicitly by the first write. Names: `^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`.
   */
  interface Collection<T extends Data = Data> {
    readonly name: string;
    /**
     * Creates a document. The id is a generated, time-sortable ULID unless
     * given (`{ id }`, `^[A-Za-z0-9_\-:.]{1,128}$`). Rejects with `conflict`
     * if the id exists.
     */
    create(data: T, options?: { id?: string }): Promise<Doc<T>>;
    /** Reads a document; `null` if it does not exist. */
    get(id: string): Promise<Doc<T> | null>;
    /** Creates or replaces a document (PUT). `id`/`_meta` keys in `data` are ignored. */
    set(id: string, data: T): Promise<Doc<T>>;
    /**
     * Shallow-merges `patch` into an existing document; a `null` value
     * DELETES that key. Rejects with `not_found` if the document is missing.
     */
    update(id: string, patch: Partial<{ [K in keyof T]: T[K] | null }> & Data): Promise<Doc<T>>;
    /** Deletes a document (also succeeds when it does not exist). */
    delete(id: string): Promise<void>;
    /** Lists documents (default: oldest first, 100 per page). */
    list(options?: ListOptions): Promise<DocList<T>>;
    /** Starts a query. */
    where(field: string, op: Op, value: Scalar | Scalar[]): Query<T>;
    /** Starts a query sorted by `field`. */
    orderBy(field: string, dir?: "asc" | "desc"): Query<T>;
    /** Starts a query with a limit. */
    limit(n: number): Query<T>;
    /** Starts a query with an offset. */
    offset(n: number): Query<T>;
    /** Runs a query given as an object (or a {@link Query}). */
    find(query?: QueryObject | Query<T>): Promise<DocList<T>>;
    /**
     * Receives changes made by anyone, in realtime (over the shared
     * websocket). Only live events are delivered: after a reconnect, use
     * {@link watch} (which re-fetches) if you must not miss changes.
     * Passing a function is shorthand for `{ onChange }`. Events arrive in
     * order; a create or update the server sent without its document is
     * re-read first (see {@link SubscribeHandlers}).
     */
    subscribe(handlers: SubscribeHandlers<T> | ((event: ChangeEvent<T>) => void)): Unsubscribe;
    /**
     * Live query: calls `callback` with the full result now, and again after
     * every change to the collection (re-fetched, debounced 50 ms), including
     * after reconnects. Identical consecutive results are delivered once.
     * Without `query` it watches `list()` (first 100 docs, oldest first).
     * ```js
     * const stop = posts.watch(render, posts.orderBy("votes", "desc").limit(50));
     * ```
     */
    watch(callback: (docs: DocList<T>) => void, query?: Query<T> | QueryObject, options?: WatchOptions): Unsubscribe;
    watch(callback: (docs: DocList<T>) => void, options: WatchOptions): Unsubscribe;
  }

  interface DatabaseAPI {
    /** Returns a collection handle (synchronous; nothing is created until the first write). */
    collection<T extends Data = Data>(name: string): Collection<T>;
    /** Names of this site's collections. */
    collections(): Promise<string[]>;
  }

  // ---------------------------------------------------------------------------
  // Files

  /** An uploaded file (SPEC §5.3). */
  interface FileInfo {
    id: string;
    name: string;
    /** Site-relative URL serving the bytes, e.g. `/__backyard/files/01J…/photo.png` (Range supported). Usable directly in `<img src>`. */
    url: string;
    size: number;
    content_type: string;
    created_at: string;
    created_by: string;
  }

  /** An array of files with a non-enumerable `hasMore` flag (more files exist past `limit`). */
  type FileInfoList = FileInfo[] & { readonly hasMore: boolean };

  interface FilesAPI {
    /**
     * Uploads a file (multipart, default limit 100 MiB). `name` overrides the
     * file name (required for nameless Blobs to get a useful name).
     * ```js
     * input.onchange = async () => { const f = await backyard.files.upload(input.files[0]); img.src = f.url; };
     * ```
     */
    upload(file: Blob | File, options?: { name?: string; signal?: AbortSignal }): Promise<FileInfo>;
    /** Lists uploads, newest first (default 100 per page). */
    list(options?: { limit?: number; offset?: number }): Promise<FileInfoList>;
    /** File metadata, or `null` if unknown. */
    get(id: string): Promise<FileInfo | null>;
    delete(id: string): Promise<void>;
  }

  // ---------------------------------------------------------------------------
  // AI

  /** A content block of a multimodal message. */
  type ContentBlock =
    | { type: "text"; text: string }
    /** Inline image: base64 `data` (no `data:` prefix) and its media type. */
    | { type: "image"; data: string; media_type: "image/png" | "image/jpeg" | "image/gif" | "image/webp" | (string & {}) }
    /**
     * Image by URL: the `url` of one of this site's uploads (as returned by
     * `files.upload`; the server reads it from storage), a `data:` URL, or an
     * absolute public http(s) URL. Other site-relative URLs are rejected.
     */
    | { type: "image"; url: string };

  interface Message {
    /** `"system"` messages are hoisted into the system prompt. */
    role: "user" | "assistant" | "system";
    content: string | ContentBlock[];
  }

  interface ChatOptions {
    /** Model alias configured on the server (see `ai.models()`); default: the server default. */
    model?: string;
    /** System prompt. */
    system?: string;
    /** Output token cap (capped by the server). */
    maxTokens?: number;
    temperature?: number;
    /** Ask for a JSON object; the parsed value is returned as `result.json`. */
    json?: boolean;
    signal?: AbortSignal;
  }

  interface Usage {
    input_tokens: number;
    output_tokens: number;
  }

  interface ChatResult {
    /** The model's reply text. */
    content: string;
    /** The alias that answered. */
    model: string;
    stop_reason: "end_turn" | "max_tokens" | "stop_sequence" | "refusal" | "other";
    usage: Usage;
    /** With `{ json: true }`: the reply parsed as JSON (tolerates code fences); `undefined` if it is not valid JSON. */
    json?: any;
  }

  interface StreamOptions extends ChatOptions {
    /** Called with every text delta as it arrives. */
    onText?(text: string): void;
  }

  /**
   * A streaming chat completion. It starts immediately and is BOTH an async
   * iterable of text chunks and a handle with a `done` promise:
   * ```js
   * const s = backyard.ai.stream("Write a poem", { onText: (t) => (out.textContent += t) });
   * for await (const chunk of s) console.log(chunk.text);   // optional
   * const { content, usage } = await s.done;
   * ```
   * Breaking out of the loop does not cancel the request; call `abort()`.
   */
  interface ChatStream extends AsyncIterable<{ text: string }> {
    /** Resolves with the final result; rejects with a BackyardError on failure. */
    readonly done: Promise<ChatResult>;
    /** The text received so far. */
    readonly text: string;
    /** Cancels the request (`done` rejects with code `"aborted"`). */
    abort(): void;
  }

  /** A generated image, stored as a site file (see {@link FilesAPI}). */
  interface GeneratedImage {
    /** File id (usable with `backyard.files.get/delete`). */
    id: string;
    /** Site-relative URL serving the bytes, usable directly in `<img src>`. */
    url: string;
    content_type: string;
  }

  interface ImageResult {
    /** File id of the first image. */
    id: string;
    /** URL of the first image (stored as a site file). */
    url: string;
    content_type: string;
    /** The model alias that generated the images. */
    model?: string;
    /** All generated images. */
    images: GeneratedImage[];
  }

  interface ModelInfo {
    /** Alias to pass as `model`. */
    name: string;
    /** What the alias is for: `chat` (chat/stream), `image` (image) or `embedding` (embed). */
    kind: "chat" | "image" | "embedding" | (string & {});
    provider: string;
    /** Provider model id (only when the server exposes model ids). */
    model?: string;
    description?: string;
  }

  interface AIAPI {
    /**
     * One-shot completion. `input` is a prompt string or a message list.
     * ```js
     * const { json } = await backyard.ai.chat(`Extract {name, date} from: ${text}`, { json: true });
     * ```
     */
    chat(input: string | Message[], options?: ChatOptions): Promise<ChatResult>;
    /** Streaming completion; see {@link ChatStream}. Throws `bad_request` synchronously for invalid input. */
    stream(input: string | Message[], options?: StreamOptions): ChatStream;
    /** Generates images (stored as site files). */
    image(prompt: string, options?: { model?: string; size?: string; n?: number; signal?: AbortSignal }): Promise<ImageResult>;
    /** Embeds one string (→ one vector) or several (→ one vector each). */
    embed(input: string, options?: { model?: string; signal?: AbortSignal }): Promise<number[]>;
    embed(input: string[], options?: { model?: string; signal?: AbortSignal }): Promise<number[][]>;
    /** Available model aliases and the default. */
    models(): Promise<{ default: string; models: ModelInfo[] }>;
  }

  // ---------------------------------------------------------------------------
  // Warehouse

  interface WarehouseColumn {
    name: string;
    /** Engine type name, e.g. `"DATE"`, `"INT64"`, `"text"`. */
    type: string;
  }

  interface WarehouseResult<Row = Record<string, any>> {
    columns: WarehouseColumn[];
    rows: Row[];
    row_count: number;
    /** `true` when the server's row cap cut the result. */
    truncated: boolean;
  }

  interface WarehouseInfo {
    sources: Array<{ name: string; type: string; description?: string }>;
    queries: Array<{ name: string; description?: string; params: string[]; source?: string }>;
  }

  /**
   * Read-only analytics queries against the company data warehouse
   * (BigQuery, Postgres/Redshift, Athena). Not available on the Cloudflare
   * edition (rejects with `unsupported`).
   */
  interface WarehouseAPI {
    /**
     * Runs raw SQL (if the server allows it). Use `?` placeholders for
     * positional `params`. Only `SELECT`/`WITH` statements are accepted.
     * ```js
     * const { rows } = await backyard.warehouse.query("SELECT day, n FROM dau WHERE team = ?", ["growth"]);
     * ```
     */
    query<Row = Record<string, any>>(sql: string, params?: Array<string | number | boolean | null>, options?: { source?: string; signal?: AbortSignal }): Promise<WarehouseResult<Row>>;
    /** Runs a named query defined on the server (`@name` params). */
    named<Row = Record<string, any>>(name: string, params?: Record<string, any>): Promise<WarehouseResult<Row>>;
    /** Lists sources and named queries. */
    sources(): Promise<WarehouseInfo>;
  }

  // ---------------------------------------------------------------------------
  // Realtime

  /** A room member (SPEC §6). */
  interface Member<S extends Data = Data> {
    /** Connection id (one per browser tab), e.g. `"c_8f3a…"`. */
    cid: string;
    user: UserSummary;
    /** The member's presence state (≤ 8 KiB JSON object). */
    state: S;
    /** RFC 3339 join time. */
    joined_at: string;
  }

  /** Sender of a room message. */
  interface Sender {
    cid: string;
    user: UserSummary;
  }

  interface RoomOptions<S extends Data = Data> {
    /** Initial presence state, visible to the other members. */
    state?: S;
  }

  interface SendOptions {
    /** Deliver only to this connection id (a member's `cid`). */
    to?: string;
    /** Also deliver the message to yourself (default `false`). */
    echo?: boolean;
  }

  /**
   * A realtime room with presence: broadcast messages and see who is there.
   * Joins automatically (now, or as soon as the socket connects) and rejoins
   * after reconnects with the latest state.
   * ```js
   * const room = backyard.socket.room("canvas", { state: { color: "#f80" } });
   * room.on("message", (data, from) => drawStroke(data, from.user));
   * room.on("members", (members) => renderAvatars(members));
   * canvas.onpointermove = (e) => room.updateState({ x: e.clientX, y: e.clientY });
   * ```
   */
  interface Room<S extends Data = Data> {
    readonly name: string;
    /** The other members (excluding yourself), oldest first (by `joined_at`, then `cid`). Replaced (never mutated) on every change. */
    readonly members: Member<S>[];
    /** Your own membership once joined, else `null`. */
    readonly self: Member<S> | null;
    /** The presence state you publish. */
    readonly state: S;
    /** Whether the server confirmed the membership on the current connection. */
    readonly joined: boolean;
    /** A message from another member (or yourself with `echo`). */
    on(event: "message", handler: (data: any, from: Sender) => void): () => void;
    /**
     * Someone joined, left or changed their state. When you first join,
     * existing members are reported via `"members"` only; after a reconnect,
     * the differences since the disconnect are reported as join/leave/update.
     */
    on(event: "join" | "leave" | "update", handler: (member: Member<S>) => void): () => void;
    /** The full member list (excluding yourself) after any presence change. */
    on(event: "members", handler: (members: Member<S>[]) => void): () => void;
    /**
     * The server rejected joining (e.g. too many rooms), a message (e.g. too
     * large, unknown `to`) or a state update (e.g. state over 8 KiB). Without
     * a handler these errors go to `socket.on("error")`, else to the console.
     */
    on(event: "error", handler: (error: BackyardError) => void): () => void;
    off(event: "message" | "join" | "leave" | "update" | "members" | "error", handler: (...args: any[]) => void): void;
    /** Broadcasts `data` (any JSON) to the room; queued while (re)connecting. Synchronous. */
    send(data: any, options?: SendOptions): void;
    /** Replaces your presence state. Synchronous. */
    setState(state: S): void;
    /** Shallow-merges into your presence state. Synchronous. */
    updateState(partial: Partial<S>): void;
    /** Leaves the room. Calling `socket.room(name)` again creates a new Room. */
    leave(): void;
  }

  interface SocketAPI {
    /**
     * Gets or creates the room `name` (`^[A-Za-z0-9_\-:.]{1,128}$`). Synchronous.
     * Calling it again with the same name returns the same Room (and applies `state` if given).
     */
    room<S extends Data = Data>(name: string, options?: RoomOptions<S>): Room<S>;
    /** Whether the websocket is connected (after the server's hello). */
    readonly connected: boolean;
    /** This tab's connection id once connected, else `null`. */
    readonly cid: string | null;
    /** The user the server identified on the websocket, once connected. */
    readonly user: User | null;
    /** Connection status. `"open"` fires after every (re)connect, `"close"` after every disconnect. */
    on(event: "open", handler: (info: { cid: string; user: User }) => void): () => void;
    on(event: "close", handler: (info: { code?: number; reason?: string }) => void): () => void;
    /** Realtime errors not tied to a subscription or room (e.g. rate limiting), and room errors without a room `"error"` handler. */
    on(event: "error", handler: (error: BackyardError) => void): () => void;
    off(event: "open" | "close" | "error", handler: (...args: any[]) => void): void;
    /** Opens the shared websocket even without rooms or subscriptions (it is otherwise created on first use). */
    connect(): void;
    /** Closes the websocket and stops reconnecting until the socket is used again. Rooms and subscriptions are restored then. */
    disconnect(): void;
  }

  // ---------------------------------------------------------------------------
  // The SDK object

  interface SDK {
    /** SDK version, e.g. `"0.1.0"`. */
    readonly version: string;
    /** Resolves when the context is resolved (immediately). */
    readonly ready: Promise<void>;
    /** Where this page runs. */
    readonly site: SiteInfo;
    /** Adjusts the SDK at runtime. Synchronous. */
    configure(options: ConfigureOptions): void;
    /** Shorthand for `backyard.id.me()`. */
    me(): Promise<User>;
    /** Identity: the current user and people lookup. */
    readonly id: IdentityAPI;
    /** JSON document database with realtime subscriptions and live queries. */
    readonly db: DatabaseAPI;
    /** File uploads. */
    readonly files: FilesAPI;
    /** LLM chat/streaming, image generation, embeddings. */
    readonly ai: AIAPI;
    /** Read-only data warehouse queries. */
    readonly warehouse: WarehouseAPI;
    /** Realtime rooms with presence (one shared websocket per page). */
    readonly socket: SocketAPI;
    /** The error class every call rejects with (for `instanceof` checks). */
    readonly BackyardError: typeof BackyardError;
  }
}

/** Set before loading the SDK to override defaults (see {@link Backyard.ConfigureOptions}). */
declare var BACKYARD_CONFIG: Backyard.ConfigureOptions | undefined;

/** The Backyard SDK, defined by `<script src="/__backyard/backyard.js">`. */
declare var backyard: Backyard.SDK;

interface Window {
  /** The Backyard SDK, defined by `<script src="/__backyard/backyard.js">`. */
  backyard: Backyard.SDK;
  BACKYARD_CONFIG?: Backyard.ConfigureOptions;
}

/**
 * ES module build: `import backyard, { db, ai } from "/__backyard/backyard.mjs"`.
 * (A wildcard pattern, because ambient module names may not start with "/";
 * it matches the absolute path and full URLs ending in it.)
 */
declare module "*/__backyard/backyard.mjs" {
  const backyard: Backyard.SDK;
  export default backyard;
  export const db: Backyard.DatabaseAPI;
  export const files: Backyard.FilesAPI;
  export const ai: Backyard.AIAPI;
  export const warehouse: Backyard.WarehouseAPI;
  export const socket: Backyard.SocketAPI;
  export const id: Backyard.IdentityAPI;
  export const site: Backyard.SiteInfo;
  export const me: () => Promise<Backyard.User>;
  export const configure: (options: Backyard.ConfigureOptions) => void;
  export const ready: Promise<void>;
  export const BackyardError: typeof Backyard.BackyardError;
  export type User = Backyard.User;
  export type Doc<T extends Backyard.Data = Backyard.Data> = Backyard.Doc<T>;
  export type FileInfo = Backyard.FileInfo;
  export type Member<S extends Backyard.Data = Backyard.Data> = Backyard.Member<S>;
}
