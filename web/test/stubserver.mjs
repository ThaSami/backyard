// A dependency-free stub of the Backyard HTTP + websocket API for browser
// smoke tests of the SDK and the hub UI. It implements just enough of SPEC
// §5–§7 (path mode) to exercise the real browser code paths: fetch with
// cookies, multipart uploads, SSE streaming, websockets (RFC 6455) with
// subscriptions/rooms/presence, and the deploy gateway protocol.
import http from "node:http";
import { createHash, randomBytes } from "node:crypto";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

const WEB = fileURLToPath(new URL("../", import.meta.url));
const read = (p) => readFileSync(WEB + p);

/** Realtime limits (smaller than the real server's, to keep test frames small). */
const MAX_PRESENCE = 1024;
const MAX_MESSAGE = 4096;

const TYPES = { ".html": "text/html; charset=utf-8", ".css": "text/css; charset=utf-8", ".js": "text/javascript; charset=utf-8", ".svg": "image/svg+xml" };

/** Site previews (SiteMeta.preview, SPEC §7.2): the same values as internal/sites/preview.go. */
const PREVIEW_NAMES = ["preview.png", "preview.jpg", "preview.jpeg", "preview.webp", "preview.avif", "preview.gif", "preview.svg"];
const MAX_PREVIEW_BYTES = 1 << 20;
const PREVIEW_CSP = "sandbox; default-src 'none'; img-src data:; style-src 'unsafe-inline'";
const PREVIEW_TYPES = { png: "image/png", jpg: "image/jpeg", jpeg: "image/jpeg", gif: "image/gif", webp: "image/webp", avif: "image/avif", svg: "image/svg+xml" };

const DOCS = {
  api: "# Backyard API\n\nUse `backyard.db`.\n\n## Database\n\nSee [recipes](recipes.md#live-lists).\n\n### collection(name)\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\n## Files\n\n```js\nawait backyard.files.upload(file)\n```\n",
  recipes: "# Recipes\n\n## Live lists\n\nUse `watch`.\n",
  deploy: "# Deploying\n\nRun `backyard deploy`.\n",
  skill: "---\nname: backyard\n---\n# Backyard skill\n\nHello agents.\n",
};

function json(res, status, body, headers = {}) {
  const b = body === undefined ? "" : JSON.stringify(body);
  res.writeHead(status, { "Content-Type": "application/json", "Content-Length": Buffer.byteLength(b), ...headers });
  res.end(b);
}
const apiError = (res, status, code, message, extra = {}) => json(res, status, { error: { code, message, ...extra } });

function readBody(req) {
  return new Promise((resolve, reject) => {
    const chunks = [];
    req.on("data", (c) => chunks.push(c));
    req.on("end", () => resolve(Buffer.concat(chunks)));
    req.on("error", reject);
  });
}

// --- minimal websocket server (text frames only) ------------------------------

function wsAccept(req, socket, handlers) {
  const key = req.headers["sec-websocket-key"];
  const accept = createHash("sha1").update(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").digest("base64");
  socket.write("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + accept + "\r\n\r\n");
  socket.setNoDelay(true);
  const conn = {
    socket,
    open: true,
    send(obj) {
      if (!conn.open) return;
      const payload = Buffer.from(typeof obj === "string" ? obj : JSON.stringify(obj));
      const len = payload.length;
      const head = len < 126 ? Buffer.from([0x81, len]) : len < 65536 ? Buffer.from([0x81, 126, len >> 8, len & 0xff]) : null;
      if (!head) throw new Error("frame too large for the stub");
      socket.write(Buffer.concat([head, payload]));
    },
    kill() {
      conn.open = false;
      socket.destroy();
    },
  };
  let buf = Buffer.alloc(0);
  socket.on("data", (chunk) => {
    buf = Buffer.concat([buf, chunk]);
    for (;;) {
      if (buf.length < 2) return;
      const op = buf[0] & 0x0f;
      const masked = (buf[1] & 0x80) !== 0;
      let len = buf[1] & 0x7f;
      let off = 2;
      if (len === 126) {
        if (buf.length < 4) return;
        len = buf.readUInt16BE(2);
        off = 4;
      } else if (len === 127) {
        if (buf.length < 10) return;
        len = Number(buf.readBigUInt64BE(2));
        off = 10;
      }
      const maskAt = off;
      if (masked) off += 4;
      if (buf.length < off + len) return;
      const payload = Buffer.from(buf.subarray(off, off + len));
      if (masked) for (let i = 0; i < payload.length; i++) payload[i] ^= buf[maskAt + (i % 4)];
      buf = buf.subarray(off + len);
      if (op === 1) handlers.message(conn, payload.toString("utf8"));
      else if (op === 8) {
        if (conn.open) socket.end(Buffer.from([0x88, 0]));
        conn.open = false;
      } else if (op === 9) socket.write(Buffer.concat([Buffer.from([0x8a, payload.length]), payload]));
    }
  });
  const closed = () => {
    if (conn.closedOnce) return;
    conn.closedOnce = true;
    conn.open = false;
    handlers.close(conn);
  };
  socket.on("close", closed);
  socket.on("error", closed);
  handlers.open(conn);
  return conn;
}

// --- server -------------------------------------------------------------------

/**
 * Starts the stub. opts: { info, requireToken, tokens, guest, sites, pages: {"/s/demo/": html},
 * presign: { cors } (deploy uploads go to a cross-origin "bucket"), expiredSession
 * (docs requests redirect to /__backyard/auth/login) }.
 * guest: visitors are anonymous guests (like AUTH_MODE=none on the Cloudflare
 * edition): deleting sites then needs the deploy token.
 * tokens: {"<bearer token>": User} — the accounts of API tokens, as the deploy
 * gateway's GET /me reports them (requireToken is "token:deploy", an admin).
 * maxEventDocBytes: db events of larger documents are sent with "doc": null.
 * mcp: the Claude connector is enabled: /info has mcp_url and the grants API
 * answers (SPEC §8.4.7); mcp.grants seeds the connections (Grant objects,
 * user.id is the owner). Without it the grants API answers 404, as the
 * servers do with the connector off.
 * stored: {"<site>/<path>": string | Buffer} seeds deployed files, e.g. the
 * image a site in opts.sites names as its preview ("preview": "preview.svg";
 * GET /__backyard/api/sites/{site}/preview serves it, 404 when it is missing).
 * A deploy sets SiteMeta.preview to the first PREVIEW_NAMES file it holds.
 * listing: "owners" lists sites like sites.listing: owners (SPEC §7.2
 * "Listing", which implies ownership: deploys record the first deployer as the
 * owner and refuse others but admins); /info reports it as site_listing
 * ("all" otherwise). GET G/sites/{site} answers for every site.
 * Returns { url, state, close() }.
 */
export async function startStub(opts = {}) {
  const state = {
    requests: [],
    docs: new Map(), // collection → Map(id → doc)
    sites: opts.sites ? opts.sites.slice() : [],
    deploys: [],
    uploads: new Map(), // upload token → { site, deploy, path, size, sha256, bytes }
    stored: new Map(Object.entries(opts.stored || {}).map(([k, v]) => [k, Buffer.from(v)])), // "site/path" → Buffer
    conns: new Set(),
    rooms: new Map(), // room → Map(cid → member)
    files: [], // FileInfo, newest first
    logins: [], // rd parameters of /__backyard/auth/login visits
    bucket: [], // requests the presigned "bucket" received: { method, path, headers }
    requireToken: opts.requireToken || "",
    grants: opts.mcp && Array.isArray(opts.mcp.grants) ? opts.mcp.grants.map((g) => ({ ...g })) : [],
    seq: 0,
  };
  let base = "";

  const userFor = (req) => {
    if (opts.guest) return { id: "anon:0123456789abcdef", name: "Guest 0123", groups: [], anonymous: true, admin: false };
    const m = /(?:^|;\s*)user=([^;]+)/.exec(req.headers.cookie || "");
    const email = m ? decodeURIComponent(m[1]) : "alice@acme.com";
    const name = email.split("@")[0].replace(/^./, (c) => c.toUpperCase());
    return { id: email, email, name, groups: [], anonymous: false, admin: email.startsWith("admin") };
  };
  /** The account of a bearer token ("Bearer <token>" or the bare token), or null if unknown. */
  const tokenUser = (auth) => {
    const tok = String(auth || "").replace(/^Bearer\s+/i, "");
    if (!tok) return null;
    if (state.requireToken && tok === state.requireToken) return { id: "token:deploy", name: "deploy", groups: [], anonymous: false, admin: true };
    return (opts.tokens && Object.hasOwn(opts.tokens, tok) && opts.tokens[tok]) || null;
  };
  const summary = (u) => ({ id: u.id, name: u.name, email: u.email });
  const now = () => new Date().toISOString();
  const ownersOnly = opts.listing === "owners";
  /** Whether u is one of names, by id or email (case-insensitive), like Meta.IsOwner. */
  const named = (u, names) => names.some((n) => [u.id, u.email].some((k) => !!k && String(k).toLowerCase() === String(n).toLowerCase()));
  /** Whether the site list shows meta to u (SPEC §7.2 "Listing"). */
  const listedTo = (u, meta) => {
    if (!ownersOnly) return true;
    if (u.anonymous) return false;
    const owners = meta.owners || [];
    return u.admin || named(u, owners) || (owners.length === 0 && !!meta.created_by && named(u, [meta.created_by]));
  };
  const coll = (c) => {
    if (!state.docs.has(c)) state.docs.set(c, new Map());
    return state.docs.get(c);
  };
  const publish = (collection, op, doc, id) => {
    // Like the Go server, a change whose document is too large for one broker
    // message is published without it ("doc": null, SPEC §6).
    if (doc && opts.maxEventDocBytes && Buffer.byteLength(JSON.stringify(doc)) > opts.maxEventDocBytes) doc = null;
    for (const conn of state.conns) {
      const sid = conn.subs.get(collection);
      if (sid !== undefined) conn.send({ t: "db", sub: sid, collection, op, doc, doc_id: id });
    }
  };
  const membersExcept = (room, cid) => [...(state.rooms.get(room) || new Map()).values()].filter((m) => m.cid !== cid);
  const presence = (room, op, member, exceptCID) => {
    for (const conn of state.conns) {
      if (conn.cid !== exceptCID && conn.rooms.has(room)) conn.send({ t: "presence", room, op, member });
    }
  };
  const leaveAll = (conn) => {
    for (const room of conn.rooms) {
      const m = state.rooms.get(room)?.get(conn.cid);
      state.rooms.get(room)?.delete(conn.cid);
      if (m) presence(room, "leave", m, conn.cid);
    }
    conn.rooms.clear();
  };

  const ack = (conn, f) => {
    if (f.id !== undefined && f.id !== null) conn.send({ t: "ack", id: f.id });
  };
  const wsError = (conn, f, code, message) => conn.send({ t: "error", ...(f.id !== undefined && f.id !== null ? { id: f.id } : {}), error: { code, message } });

  const wsHandlers = (user) => ({
    open(conn) {
      conn.cid = "c_" + randomBytes(5).toString("hex");
      conn.user = user;
      conn.subs = new Map();
      conn.rooms = new Set();
      state.conns.add(conn);
      conn.send({ t: "hello", cid: conn.cid, user });
    },
    close(conn) {
      state.conns.delete(conn);
      leaveAll(conn);
    },
    message(conn, text) {
      let f;
      try {
        f = JSON.parse(text);
      } catch {
        return conn.send({ t: "error", error: { code: "bad_request", message: "malformed frame" } });
      }
      state.requests.push({ ws: f });
      switch (f.t) {
        case "ping":
          return conn.send({ t: "pong" });
        case "sub":
          conn.subs.set(f.collection, f.id);
          return conn.send({ t: "ack", id: f.id });
        case "unsub":
          for (const [c, id] of conn.subs) if (id === f.id) conn.subs.delete(c);
          return conn.send({ t: "ack", id: f.id });
        case "join": {
          if (!state.rooms.has(f.room)) state.rooms.set(f.room, new Map());
          const me = { cid: conn.cid, user: summary(conn.user), state: f.state || {}, joined_at: new Date(Date.now() + state.seq++).toISOString() };
          state.rooms.get(f.room).set(conn.cid, me);
          conn.rooms.add(f.room);
          conn.send({ t: "joined", id: f.id, room: f.room, self: me, members: membersExcept(f.room, conn.cid) });
          return presence(f.room, "join", me, conn.cid);
        }
        // Like the Go server and the Worker: leave/state/send frames that
        // carry an id are acknowledged ({"t":"ack","id"}) or answered with an
        // error frame with the same id.
        case "leave": {
          const m = state.rooms.get(f.room)?.get(conn.cid);
          state.rooms.get(f.room)?.delete(conn.cid);
          conn.rooms.delete(f.room);
          if (m) presence(f.room, "leave", m, conn.cid);
          return ack(conn, f);
        }
        case "state": {
          const m = state.rooms.get(f.room)?.get(conn.cid);
          if (!m) return wsError(conn, f, "not_found", `not a member of room ${JSON.stringify(f.room)}`);
          if (JSON.stringify(f.state || {}).length > MAX_PRESENCE) return wsError(conn, f, "too_large", `presence state exceeds ${MAX_PRESENCE} bytes`);
          m.state = f.state || {};
          presence(f.room, "update", m, conn.cid);
          return ack(conn, f);
        }
        case "send": {
          if (!conn.rooms.has(f.room)) return wsError(conn, f, "not_found", `not a member of room ${JSON.stringify(f.room)}`);
          if (f.to && !state.rooms.get(f.room)?.has(f.to)) return wsError(conn, f, "not_found", `no member ${JSON.stringify(f.to)} in room ${JSON.stringify(f.room)}`);
          if (JSON.stringify(f.data ?? null).length > MAX_MESSAGE) return wsError(conn, f, "too_large", "message too large");
          const from = { cid: conn.cid, user: summary(conn.user) };
          for (const other of state.conns) {
            if (!other.rooms.has(f.room)) continue;
            if (f.to && other.cid !== f.to) continue;
            if (other === conn && !f.echo) continue;
            other.send({ t: "msg", room: f.room, from, data: f.data === undefined ? null : f.data });
          }
          return ack(conn, f);
        }
        default:
          return conn.send({ t: "error", id: f.id, error: { code: "bad_request", message: "unknown frame" } });
      }
    },
  });

  async function api(req, res, path, site) {
    const method = req.method;
    const user = userFor(req);
    const body = method === "GET" ? Buffer.alloc(0) : await readBody(req);
    const entry = { method, path, query: new URL(req.url, base).search, site, headers: req.headers, body };
    state.requests.push(entry);
    if (method !== "GET" && method !== "HEAD" && !path.startsWith("/deploy/uploads/") && req.headers["x-backyard"] !== "1") {
      return apiError(res, 403, "forbidden", 'missing "X-Backyard: 1" header');
    }
    let m;
    const jsonBody = () => (body.length ? JSON.parse(body.toString()) : {});
    if (path === "/info") {
      return json(res, 200, {
        name: "Stub Backyard",
        version: "0.1.0",
        public_url: base,
        base_domain: new URL(base).host,
        url_mode: "path",
        gateway_url: base + "/__backyard/api/deploy",
        auth: { mode: "none", anonymous_deploy: !state.requireToken },
        methods: ["api"],
        limits: { max_site_bytes: 524288000, max_files: 10000, max_file_bytes: 104857600 },
        site_listing: ownersOnly ? "owners" : "all",
        ...(opts.mcp ? { mcp_url: base + "/__backyard/mcp" } : {}),
        ...(opts.info || {}),
      });
    }
    // Connector connections (SPEC §8.4.7): the caller's own; other people's for admins.
    if (path === "/mcp/grants" || path.startsWith("/mcp/grants/")) {
      if (!opts.mcp) return apiError(res, 404, "not_found", "the Claude connector is not enabled on this server");
      const who = new URL(req.url, base).searchParams.get("user");
      if (who !== null && who !== user.id && !user.admin) return apiError(res, 403, "forbidden", "only admins can manage other people's connections");
      const owner = who === null ? user.id : who;
      if (path === "/mcp/grants" && method === "GET") return json(res, 200, { grants: state.grants.filter((g) => g.user.id === owner) });
      if (path === "/mcp/grants" && method === "DELETE") {
        const before = state.grants.length;
        state.grants = state.grants.filter((g) => g.user.id !== owner);
        return json(res, 200, { revoked: before - state.grants.length });
      }
      if ((m = /^\/mcp\/grants\/([^/]+)$/.exec(path)) && method === "DELETE") {
        const id = decodeURIComponent(m[1]);
        const g = state.grants.find((x) => x.id === id && (x.user.id === user.id || user.admin));
        if (!g) return apiError(res, 404, "not_found", "no such connection");
        state.grants = state.grants.filter((x) => x !== g);
        res.writeHead(204);
        return res.end();
      }
      return apiError(res, 405, "bad_request", "method not allowed");
    }
    if (path === "/me") return json(res, 200, user);
    if (path === "/sites") return json(res, 200, { sites: state.sites.filter((s) => listedTo(user, s)) });
    // A site's preview image (SPEC §2, §7.2): from the hub's origin, typed by its extension, sandboxed.
    if ((m = /^\/sites\/([a-z0-9-]+)\/preview$/.exec(path))) {
      if (method !== "GET" && method !== "HEAD") return apiError(res, 405, "bad_request", "method not allowed");
      const meta = state.sites.find((s) => s.name === m[1] && listedTo(user, s));
      const ext = meta && meta.preview ? meta.preview.slice(meta.preview.lastIndexOf(".") + 1).toLowerCase() : "";
      const bytes = meta && meta.preview ? state.stored.get(m[1] + "/" + meta.preview) : undefined;
      if (!bytes || !Object.hasOwn(PREVIEW_TYPES, ext)) return apiError(res, 404, "not_found", `site "${m[1]}" has no preview`);
      const etag = '"' + createHash("sha256").update(bytes).digest("hex") + '"';
      const headers = {
        "Content-Type": PREVIEW_TYPES[ext],
        "Content-Security-Policy": PREVIEW_CSP,
        "X-Content-Type-Options": "nosniff",
        "Cache-Control": new URL(req.url, base).searchParams.get("v") ? "private, max-age=31536000, immutable" : "private, no-cache",
        ETag: etag,
      };
      if (req.headers["if-none-match"] === etag) {
        res.writeHead(304, headers);
        return res.end();
      }
      res.writeHead(200, { ...headers, "Content-Length": bytes.length });
      return res.end(method === "HEAD" ? undefined : bytes);
    }
    // GET G/me (SPEC §7.2): a bearer token takes priority over the visitor's identity.
    if (path === "/deploy/me" && method === "GET") {
      if (!req.headers.authorization) {
        if (user.anonymous && state.requireToken) return apiError(res, 401, "unauthenticated", "a token is required (run: backyard login)");
        return json(res, 200, user);
      }
      const tu = tokenUser(req.headers.authorization);
      return tu ? json(res, 200, tu) : apiError(res, 401, "unauthenticated", "invalid or expired token (run: backyard login)");
    }
    // GET G/sites/{site} (SPEC §7.2): any site by name, whatever the listing.
    if ((m = /^\/deploy\/sites\/([a-z0-9-]+)$/.exec(path)) && method === "GET") {
      if (req.headers.authorization && !tokenUser(req.headers.authorization)) return apiError(res, 401, "unauthenticated", "invalid or expired token (run: backyard login)");
      const meta = state.sites.find((s) => s.name === m[1]);
      return meta ? json(res, 200, meta) : apiError(res, 404, "not_found", `site "${m[1]}" not found`);
    }

    // Deploy gateway (SPEC §7.3).
    if ((m = /^\/deploy\/sites\/([a-z0-9-]+)\/deploys$/.exec(path)) && method === "POST") {
      if (state.requireToken && req.headers.authorization !== "Bearer " + state.requireToken) return apiError(res, 401, "unauthenticated", "deploy token required");
      // An owners-only listing implies the ownership policy (SPEC §7.2, §7.3).
      const prev = ownersOnly && state.sites.find((s) => s.name === m[1]);
      const actor = tokenUser(req.headers.authorization) || user;
      if (prev && (prev.owners || []).length && !actor.admin && !named(actor, prev.owners)) {
        return apiError(res, 403, "forbidden", `site ${m[1]} is owned by ${prev.owners.join(", ")}; only owners and admins can change it`);
      }
      const reqBody = jsonBody();
      const deployID = "d" + state.deploys.length;
      const dep = { site: m[1], id: deployID, files: reqBody.files, message: reqBody.message, committed: false };
      state.deploys.push(dep);
      const uploads = [];
      const unchanged = [];
      for (const f of reqBody.files) {
        const prev = state.stored.get(m[1] + "/" + f.path);
        if (prev && createHash("sha256").update(prev).digest("hex") === f.sha256) {
          unchanged.push(f.path);
          continue;
        }
        const tok = randomBytes(6).toString("hex");
        state.uploads.set(tok, { site: m[1], dep, ...f });
        if (bucketBase) {
          // A presigned storage URL on another origin; the signed headers
          // must be sent verbatim (Azure x-ms-blob-type, S3 x-amz-*).
          const headers = { "Content-Type": "application/octet-stream", "x-ms-blob-type": "BlockBlob", "x-amz-meta-sha256": f.sha256 };
          state.uploads.get(tok).headers = headers;
          uploads.push({ path: f.path, url: bucketBase + "/bucket/" + tok + "?sig=stub", method: "PUT", headers });
        } else {
          uploads.push({ path: f.path, url: base + "/__backyard/api/deploy/uploads/" + tok, method: "PUT", headers: { "Content-Type": "application/octet-stream" } });
        }
      }
      return json(res, 200, { deploy_id: deployID, uploads, unchanged, expires_at: new Date(Date.now() + 3600e3).toISOString() });
    }
    if ((m = /^\/deploy\/uploads\/([0-9a-f]+)$/.exec(path)) && method === "PUT") {
      const up = state.uploads.get(m[1]);
      if (!up) return apiError(res, 404, "not_found", "unknown upload");
      if (body.length !== up.size || createHash("sha256").update(body).digest("hex") !== up.sha256) return apiError(res, 400, "bad_request", "size or sha256 mismatch");
      state.stored.set(up.site + "/" + up.path, body);
      up.done = true;
      res.writeHead(200);
      return res.end();
    }
    if ((m = /^\/deploy\/sites\/([a-z0-9-]+)\/deploys\/([^/]+)\/commit$/.exec(path)) && method === "POST") {
      if (state.requireToken && req.headers.authorization !== "Bearer " + state.requireToken) return apiError(res, 401, "unauthenticated", "deploy token required");
      const dep = state.deploys.find((d) => d.id === decodeURIComponent(m[2]) && d.site === m[1]);
      if (!dep) return apiError(res, 404, "not_found", "unknown deploy");
      for (const f of jsonBody().files) if (!state.stored.has(m[1] + "/" + f.path)) return apiError(res, 400, "bad_request", "missing " + f.path);
      dep.committed = true;
      const actor = tokenUser(req.headers.authorization) || user;
      // SiteMeta.preview: only the fallback names (the stub reads no og:image).
      const preview = PREVIEW_NAMES.find((n) => dep.files.some((f) => f.path === n && f.size <= MAX_PREVIEW_BYTES));
      const meta = { name: m[1], url: base + "/s/" + m[1] + "/", title: m[1], ...(preview ? { preview } : {}), created_at: now(), updated_at: now(), updated_by: actor.id, file_count: dep.files.length, bytes: 0, owners: [] };
      if (ownersOnly) {
        // Ownership: the first deployer who is not a guest becomes the owner.
        const prev = state.sites.find((s) => s.name === m[1]);
        meta.created_by = (prev && prev.created_by) || actor.id;
        meta.owners = prev && (prev.owners || []).length ? prev.owners : actor.anonymous ? [] : [actor.id];
      }
      state.sites = state.sites.filter((s) => s.name !== m[1]).concat([meta]);
      return json(res, 200, { site: meta, url: meta.url, deleted: 0 });
    }
    if ((m = /^\/deploy\/sites\/([a-z0-9-]+)$/.exec(path)) && method === "DELETE") {
      const bearer = req.headers.authorization;
      if (state.requireToken && bearer && bearer !== "Bearer " + state.requireToken) return apiError(res, 401, "unauthenticated", "invalid or expired token");
      if (user.anonymous && !bearer) return apiError(res, 401, "unauthenticated", "a token is required to delete sites or change owners");
      state.sites = state.sites.filter((s) => s.name !== m[1]);
      res.writeHead(204);
      return res.end();
    }

    // Site API (SPEC §5).
    if (path === "/db" && method === "GET") return json(res, 200, { collections: [...state.docs.keys()] });
    if ((m = /^\/db\/([^/]+)$/.exec(path))) {
      const c = decodeURIComponent(m[1]);
      if (method === "POST") {
        const data = jsonBody();
        const id = data.id || "01J" + String(state.seq++).padStart(23, "0");
        if (coll(c).has(id)) return apiError(res, 409, "conflict", "document exists");
        delete data.id;
        const doc = { id, ...data, _meta: { created_at: now(), updated_at: now(), created_by: user.id, updated_by: user.id, version: 1 } };
        coll(c).set(id, doc);
        publish(c, "create", doc, id);
        return json(res, 201, doc);
      }
      return json(res, 200, { docs: [...coll(c).values()], has_more: false });
    }
    if ((m = /^\/db\/([^/]+)\/query$/.exec(path)) && method === "POST") {
      const q = jsonBody();
      let docs = [...coll(decodeURIComponent(m[1])).values()];
      for (const w of q.where || []) docs = docs.filter((d) => (w.op === "==" ? d[w.field] === w.value : w.op === ">=" ? d[w.field] >= w.value : true));
      for (const o of (q.order_by || []).slice().reverse()) docs.sort((a, b) => (a[o.field] < b[o.field] ? -1 : a[o.field] > b[o.field] ? 1 : 0) * (o.dir === "desc" ? -1 : 1));
      if (q.limit) docs = docs.slice(q.offset || 0, (q.offset || 0) + q.limit);
      return json(res, 200, { docs, has_more: false });
    }
    if ((m = /^\/db\/([^/]+)\/([^/]+)$/.exec(path))) {
      const c = decodeURIComponent(m[1]);
      const id = decodeURIComponent(m[2]);
      const cur = coll(c).get(id);
      if (method === "GET") return cur ? json(res, 200, cur) : apiError(res, 404, "not_found", "no such document");
      if (method === "DELETE") {
        coll(c).delete(id);
        publish(c, "delete", null, id);
        res.writeHead(204);
        return res.end();
      }
      const data = jsonBody();
      delete data.id;
      if (method === "PATCH" && !cur) return apiError(res, 404, "not_found", "no such document");
      const merged = method === "PATCH" ? { ...cur, ...data } : { id, ...data };
      for (const [k, v] of Object.entries(merged)) if (v === null) delete merged[k];
      merged._meta = { ...(cur ? cur._meta : { created_at: now(), created_by: user.id }), updated_at: now(), updated_by: user.id, version: cur ? cur._meta.version + 1 : 1 };
      coll(c).set(id, merged);
      publish(c, cur ? "update" : "create", merged, id);
      return json(res, 200, merged);
    }
    if (path === "/files" && method === "POST") {
      const ct = req.headers["content-type"] || "";
      if (!/^multipart\/form-data; boundary=/.test(ct)) return apiError(res, 400, "bad_request", "multipart expected");
      const text = body.toString("latin1");
      const name = /name="name"\r\n\r\n([^\r]*)/.exec(text)?.[1] || /filename="([^"]*)"/.exec(text)?.[1] || "file";
      const content = /filename="[^"]*"\r\nContent-Type: [^\r]*\r\n\r\n([\s\S]*?)\r\n--/.exec(text)?.[1] || "";
      const id = "f" + state.seq++;
      const info = { id, name, url: `/s/${site}/__backyard/files/${id}/${encodeURIComponent(name)}`, size: content.length, content_type: "text/plain", created_at: now(), created_by: user.id };
      state.files.unshift(info);
      return json(res, 201, info);
    }
    if (path === "/files" && method === "GET") {
      // Server shape: {"files": [FileInfo], "has_more": bool}, newest first.
      const q = new URL(req.url, base).searchParams;
      const limit = Number(q.get("limit") || 100);
      const offset = Number(q.get("offset") || 0);
      return json(res, 200, { files: state.files.slice(offset, offset + limit), has_more: state.files.length > offset + limit });
    }
    if (path === "/ai/chat" && method === "POST") {
      const b = jsonBody();
      const prompt = typeof b.messages[0].content === "string" ? b.messages[0].content : "";
      if (!b.stream) return json(res, 200, { content: "echo: " + prompt, model: "smart", stop_reason: "end_turn", usage: { input_tokens: 1, output_tokens: 2 } });
      // Like the Go server: headers are flushed at once, keep-alive comments
      // (": ping") come while the model is silent.
      res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache", "X-Accel-Buffering": "no" });
      res.flushHeaders();
      res.write(": ping\n\n");
      const words = ["Hello", " from", " the", " stub", " ✓"];
      let i = 0;
      const tick = () => {
        if (i < words.length) {
          const ev = `data: ${JSON.stringify({ type: "text", text: words[i++] })}\n\n`;
          // Split every event in two writes to exercise chunk reassembly.
          const cut = Math.floor(ev.length / 2);
          res.write(ev.slice(0, cut));
          setTimeout(() => {
            res.write(ev.slice(cut));
            setTimeout(tick, 15);
          }, 5);
        } else {
          res.write(": ping\n\n");
          res.end(`data: ${JSON.stringify({ type: "done", model: "smart", stop_reason: "end_turn", usage: { input_tokens: 3, output_tokens: 5 } })}\n\n`);
        }
      };
      return tick();
    }
    if (path === "/ai/image" && method === "POST") {
      // Server shape: {"model", "images": [{"id", "url", "content_type"}]} (stored as site files).
      const b = jsonBody();
      const images = [];
      for (let i = 0; i < (b.n || 1); i++) {
        const id = "img" + state.seq++;
        const info = { id, name: `image-${i + 1}.png`, url: `/s/${site}/__backyard/files/${id}/image-${i + 1}.png`, size: 68, content_type: "image/png", created_at: now(), created_by: user.id };
        state.files.unshift(info);
        images.push({ id, url: info.url, content_type: info.content_type });
      }
      return json(res, 200, { model: b.model || "image", images });
    }
    if (path === "/ai/embed" && method === "POST") {
      const b = jsonBody();
      const inputs = Array.isArray(b.input) ? b.input : [b.input];
      return json(res, 200, { model: b.model || "embedding", embeddings: inputs.map((t) => [String(t).length, 0.5]), usage: { input_tokens: inputs.length, output_tokens: 0 } });
    }
    if (path === "/ai/models") {
      return json(res, 200, {
        default: "smart",
        models: [
          { name: "smart", kind: "chat", provider: "stub" },
          { name: "image", kind: "image", provider: "stub" },
          { name: "embedding", kind: "embedding", provider: "stub" },
        ],
      });
    }
    if (path === "/warehouse/query") return apiError(res, 501, "unsupported", "the warehouse is not available on this edition");
    return apiError(res, 404, "not_found", "no route " + method + " " + path);
  }

  const server = http.createServer(async (req, res) => {
    try {
      const u = new URL(req.url, base);
      const p = u.pathname;
      let m;
      // SDK (with the per-request preamble, SPEC §2.1).
      if ((m = /^(?:\/s\/([a-z0-9-]+))?\/__backyard\/backyard\.(js|mjs|d\.ts)$/.exec(p))) {
        const file = { js: "sdk/backyard.js", mjs: "sdk/backyard.mjs", "d.ts": "sdk/backyard.d.ts" }[m[2]];
        const pre = m[2] === "d.ts" ? "" : `globalThis.__BACKYARD__ = ${JSON.stringify({ v: 1, mode: "path", site: m[1] || null, prefix: "/s/", hub: base })};\n`;
        res.writeHead(200, { "Content-Type": m[2] === "d.ts" ? "text/plain; charset=utf-8" : "text/javascript; charset=utf-8" });
        return res.end(Buffer.concat([Buffer.from(pre), read(file)]));
      }
      if (opts.expiredSession && /^\/__backyard\/api\/(info|me|sites)$/.test(p)) {
        // An identity-aware proxy whose session expired redirects to its login.
        res.writeHead(302, { Location: "/__backyard/auth/login?rd=" + encodeURIComponent(base + p) });
        return res.end();
      }
      if ((m = /^(?:\/s\/([a-z0-9-]+))?\/__backyard\/api(\/.*)$/.exec(p))) return await api(req, res, m[2], m[1] || null);
      if ((m = /^\/s\/([a-z0-9-]+)\/(.*)$/.exec(p))) {
        const page = (opts.pages || {})[m[2] || "index.html"];
        if (page !== undefined) {
          res.writeHead(200, { "Content-Type": "text/html; charset=utf-8" });
          return res.end(page);
        }
        const stored = state.stored.get(m[1] + "/" + (m[2] || "index.html"));
        if (stored) {
          res.writeHead(200, { "Content-Type": "text/html; charset=utf-8" });
          return res.end(stored);
        }
      }
      if (p === "/__backyard/auth/login") {
        state.logins.push(u.searchParams.get("rd"));
        res.writeHead(200, { "Content-Type": "text/html; charset=utf-8" });
        return res.end("<!doctype html><title>Sign in</title><h1>Sign in</h1>");
      }
      if (opts.expiredSession && /^\/docs\/[a-z]+\.md$/.test(p)) {
        // An expired session: the server sends the browser to log in.
        res.writeHead(302, { Location: "/__backyard/auth/login?rd=" + encodeURIComponent(base + p) });
        return res.end();
      }
      if ((m = /^\/docs\/([a-z]+)\.md$/.exec(p)) && DOCS[m[1]]) {
        res.writeHead(200, { "Content-Type": "text/markdown; charset=utf-8" });
        return res.end(DOCS[m[1]]);
      }
      const hubFile = p === "/" ? "index.html" : p === "/docs" ? "docs.html" : /^\/[a-z]+\.(css|js|svg|html)$/.test(p) ? p.slice(1) : null;
      if (hubFile) {
        let data;
        try {
          data = read("hub/" + hubFile);
        } catch {
          data = null;
        }
        if (data) {
          res.writeHead(200, { "Content-Type": TYPES[hubFile.slice(hubFile.lastIndexOf("."))] });
          return res.end(data);
        }
      }
      if (p === "/llms.txt") {
        res.writeHead(200, { "Content-Type": "text/plain" });
        return res.end("# Backyard\n");
      }
      res.writeHead(404, { "Content-Type": "text/plain" });
      res.end("not found");
    } catch (err) {
      res.writeHead(500, { "Content-Type": "text/plain" });
      res.end(String(err && err.stack));
    }
  });
  server.on("upgrade", (req, socket) => {
    const p = new URL(req.url, base).pathname;
    if (!/^(?:\/s\/[a-z0-9-]+)?\/__backyard\/api\/ws$/.test(p)) return socket.destroy();
    wsAccept(req, socket, wsHandlers(userFor(req)));
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  base = "http://127.0.0.1:" + server.address().port;

  // opts.presign: { cors: boolean } serves uploads from a separate "bucket"
  // origin, with (or without) a CORS configuration allowing the hub.
  let bucket = null;
  let bucketBase = "";
  if (opts.presign) {
    bucket = http.createServer(async (req, res) => {
      const cors = opts.presign.cors
        ? { "Access-Control-Allow-Origin": base, "Access-Control-Allow-Methods": "PUT", "Access-Control-Allow-Headers": req.headers["access-control-request-headers"] || "", "Access-Control-Max-Age": "60" }
        : {};
      const body = await readBody(req);
      const path = new URL(req.url, bucketBase).pathname;
      state.bucket.push({ method: req.method, path, headers: req.headers });
      if (req.method === "OPTIONS") {
        res.writeHead(opts.presign.cors ? 204 : 403, cors);
        return res.end();
      }
      const up = req.method === "PUT" && state.uploads.get(path.replace(/^\/bucket\//, ""));
      if (!up) {
        res.writeHead(404, { ...cors, "Content-Type": "application/xml" });
        return res.end("<Error><Code>NoSuchUpload</Code><Message>unknown upload</Message></Error>");
      }
      for (const [k, v] of Object.entries(up.headers)) {
        if (req.headers[k.toLowerCase()] !== v) {
          res.writeHead(403, { ...cors, "Content-Type": "application/xml" });
          return res.end(`<Error><Code>SignatureDoesNotMatch</Code><Message>header ${k} differs from the signed value</Message></Error>`);
        }
      }
      if (body.length !== up.size || createHash("sha256").update(body).digest("hex") !== up.sha256) {
        res.writeHead(400, { ...cors, "Content-Type": "application/xml" });
        return res.end("<Error><Code>BadDigest</Code><Message>size or sha256 mismatch</Message></Error>");
      }
      state.stored.set(up.site + "/" + up.path, body);
      up.done = true;
      res.writeHead(201, cors);
      res.end();
    });
    await new Promise((resolve) => bucket.listen(0, "127.0.0.1", resolve));
    bucketBase = "http://127.0.0.1:" + bucket.address().port;
  }
  return {
    url: base,
    state,
    /** Drops every websocket without a close handshake (simulated network loss). */
    killSockets() {
      for (const c of [...state.conns]) c.kill();
    },
    close() {
      for (const c of state.conns) c.kill();
      const closing = [new Promise((resolve) => server.close(() => resolve()))];
      if (bucket) closing.push(new Promise((resolve) => bucket.close(() => resolve())));
      return Promise.all(closing);
    },
  };
}
