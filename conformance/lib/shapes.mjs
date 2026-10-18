// Assertions for the wire shapes of SPEC §4–§7.
import assert from "node:assert/strict";

/** RFC 3339 UTC like Go's time.RFC3339Nano (no trailing zeros in the fraction). */
export const RFC3339_UTC = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d*[1-9])?Z$/;
/** Document/file timestamps: at most microsecond precision. */
export const RFC3339_MICRO = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d{0,5}[1-9])?Z$/;
/** Presence joined_at: exactly three fractional digits. */
export const JOINED_AT = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$/;
/** Generated ids: 26-char Crockford base32 ULIDs. */
export const ULID = /^[0-9A-HJKMNP-TV-Z]{26}$/;

/** Asserts a User (SPEC §4). */
export function assertUser(u, what = "user") {
  assert.ok(u && typeof u === "object" && !Array.isArray(u), `${what}: not an object: ${JSON.stringify(u)}`);
  assert.equal(typeof u.id, "string", `${what}.id`);
  assert.ok(u.id.length > 0, `${what}.id is empty`);
  assert.equal(typeof u.name, "string", `${what}.name: ${JSON.stringify(u)}`);
  assert.equal(typeof u.anonymous, "boolean", `${what}.anonymous: ${JSON.stringify(u)}`);
  assert.equal(typeof u.admin, "boolean", `${what}.admin: ${JSON.stringify(u)}`);
  for (const k of ["email", "title", "team", "slack", "avatar_url"]) {
    if (k in u) assert.equal(typeof u[k], "string", `${what}.${k}: ${JSON.stringify(u)}`);
  }
  if ("groups" in u) assert.ok(Array.isArray(u.groups), `${what}.groups: ${JSON.stringify(u)}`);
  if (u.anonymous) {
    assert.match(u.id, /^anon:[0-9a-f]{16}$/, `${what}: anonymous id`);
    assert.match(u.name, /^Guest [0-9a-f]{4}$/, `${what}: anonymous name`);
    assert.equal(u.id.slice(5, 9), u.name.slice(6), `${what}: guest name derives from the id`);
    assert.equal(u.admin, false, `${what}: anonymous users are never admins`);
  }
  return u;
}

/** Asserts a UserSummary (SPEC §6) of user. */
export function assertSummary(s, user, what = "user summary") {
  assert.ok(s && typeof s === "object", `${what}: ${JSON.stringify(s)}`);
  assert.equal(s.id, user.id, `${what}.id`);
  assert.equal(s.name, user.name, `${what}.name`);
  for (const k of Object.keys(s)) assert.ok(["id", "name", "email", "avatar_url"].includes(k), `${what}: unexpected key ${k}`);
  if (user.email) assert.equal(s.email, user.email, `${what}.email`);
}

/** The SiteMeta keys in the order both editions emit them (SPEC §7.2). */
export const SITE_META_KEYS = ["name", "url", "title", "description", "preview", "created_at", "created_by", "updated_at", "updated_by", "deploy_id", "file_count", "bytes", "owners"];

/** Extensions a SiteMeta.preview may have (SPEC §7.2). */
export const PREVIEW_EXTENSIONS = ["png", "jpg", "jpeg", "gif", "webp", "avif", "svg"];

/** Asserts a SiteMeta (SPEC §7.2). */
export function assertSiteMeta(m, { name, url, what = "SiteMeta" } = {}) {
  assert.ok(m && typeof m === "object", `${what}: ${JSON.stringify(m)}`);
  if (name !== undefined) assert.equal(m.name, name, `${what}.name`);
  assert.equal(typeof m.name, "string", `${what}.name`);
  if (url !== undefined) assert.equal(m.url, url, `${what}.url`);
  assert.equal(typeof m.url, "string", `${what}.url`);
  assert.match(m.created_at, RFC3339_UTC, `${what}.created_at`);
  assert.match(m.updated_at, RFC3339_UTC, `${what}.updated_at`);
  assert.ok(Date.parse(m.updated_at) >= Date.parse(m.created_at), `${what}: updated_at before created_at`);
  assert.ok(Number.isInteger(m.file_count) && m.file_count >= 0, `${what}.file_count`);
  assert.ok(Number.isInteger(m.bytes) && m.bytes >= 0, `${what}.bytes`);
  assert.ok(Array.isArray(m.owners), `${what}.owners must be an array: ${JSON.stringify(m)}`);
  for (const k of ["title", "description", "preview", "created_by", "updated_by", "deploy_id"]) {
    if (!(k in m)) continue;
    assert.equal(typeof m[k], "string", `${what}.${k}`);
    assert.notEqual(m[k], "", `${what}.${k} is omitted when empty`);
  }
  if (m.deploy_id !== undefined) assert.match(m.deploy_id, /^d_[0-9a-f]+$/, `${what}.deploy_id`);
  if (m.preview !== undefined) {
    assert.ok(!m.preview.startsWith("/") && !m.preview.split("/").some((seg) => seg === "" || seg.startsWith(".")), `${what}.preview is a site path: ${m.preview}`);
    const ext = m.preview.includes(".") ? m.preview.slice(m.preview.lastIndexOf(".") + 1).toLowerCase() : "";
    assert.ok(PREVIEW_EXTENSIONS.includes(ext), `${what}.preview has an image extension: ${m.preview}`);
  }
  const known = Object.keys(m).filter((k) => SITE_META_KEYS.includes(k));
  assert.deepEqual(known, SITE_META_KEYS.filter((k) => known.includes(k)), `${what}: the keys in the order of SPEC §7.2`);
  return m;
}

/** Asserts a flattened document (SPEC §5.2). */
export function assertDoc(d, { id, data, by, version, what = "doc" } = {}) {
  assert.ok(d && typeof d === "object" && !Array.isArray(d), `${what}: ${JSON.stringify(d)}`);
  assert.equal(typeof d.id, "string", `${what}.id`);
  if (id !== undefined) assert.equal(d.id, id, `${what}.id`);
  const m = d._meta;
  assert.ok(m && typeof m === "object", `${what}._meta: ${JSON.stringify(d)}`);
  assert.deepEqual(Object.keys(m).sort(), ["created_at", "created_by", "updated_at", "updated_by", "version"], `${what}._meta keys`);
  assert.match(m.created_at, RFC3339_MICRO, `${what}._meta.created_at`);
  assert.match(m.updated_at, RFC3339_MICRO, `${what}._meta.updated_at`);
  assert.ok(Date.parse(m.updated_at) >= Date.parse(m.created_at), `${what}: updated_at before created_at`);
  assert.ok(Number.isInteger(m.version) && m.version >= 1, `${what}._meta.version`);
  if (version !== undefined) assert.equal(m.version, version, `${what}._meta.version`);
  if (by !== undefined) {
    assert.equal(m.updated_by, by, `${what}._meta.updated_by`);
  }
  if (data !== undefined) {
    const { id: _i, _meta: _m, ...rest } = d;
    // Through JSON, so that -0 and 0 compare equal (JSON has one zero).
    const norm = (v) => JSON.parse(JSON.stringify(v));
    assert.deepEqual(norm(rest), norm(data), `${what}: data`);
  }
  return d;
}

/** Asserts a FileInfo (SPEC §5.3). */
export function assertFileInfo(f, { name, size, prefix = "", by, what = "FileInfo" } = {}) {
  assert.ok(f && typeof f === "object", `${what}: ${JSON.stringify(f)}`);
  assert.deepEqual(Object.keys(f).sort(), ["content_type", "created_at", "created_by", "id", "name", "size", "url"], `${what} keys`);
  assert.match(f.id, /^[A-Za-z0-9_\-:.]{1,128}$/, `${what}.id`);
  if (name !== undefined) assert.equal(f.name, name, `${what}.name`);
  if (size !== undefined) assert.equal(f.size, size, `${what}.size`);
  if (by !== undefined) assert.equal(f.created_by, by, `${what}.created_by`);
  assert.equal(typeof f.content_type, "string", `${what}.content_type`);
  assert.match(f.created_at, RFC3339_MICRO, `${what}.created_at`);
  const head = `${prefix}/__backyard/files/${f.id}/`;
  assert.ok(f.url.startsWith(head), `${what}.url ${f.url} should start with ${head}`);
  const last = f.url.slice(head.length);
  assert.ok(!last.includes("/"), `${what}.url: the name must be escaped: ${f.url}`);
  assert.equal(decodeURIComponent(last), f.name, `${what}.url ends with the escaped name`);
  return f;
}
