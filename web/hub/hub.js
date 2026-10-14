/*
 * Backyard hub UI: site directory, drag-and-drop deploys, CLI and agent setup.
 * No frameworks, no dependencies. Works with the Go server and the Cloudflare
 * Worker edition (same /__backyard/api endpoints, SPEC §2, §7).
 *
 * Pure helpers are exposed as globalThis.BackyardHub (unit-tested in Node);
 * the UI boots only in a browser page that has <body id="hub">.
 */
(function () {
  "use strict";

  const API = "/__backyard/api";
  const GATEWAY = API + "/deploy";
  const TOKEN_KEY = "backyard.deployToken";
  const REMEMBER_KEY = "backyard.rememberToken";
  const VIEW_KEY = "backyard.hubView";
  const OIDC_MODES = ["oidc", "okta", "entra", "azuread", "google", "auth0", "keycloak"];
  const SITE_NAME_RE = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;
  const DEFAULT_RESERVED = ["www", "api", "admin", "hub", "auth", "static", "assets", "cdn", "docs", "mail", "backyard", "s"];
  const RESERVED_PATH_PREFIX = "__backyard/";
  const MAX_PATH_BYTES = 1024;

  /** Built-in deploy ignore rules (SPEC §13; same as internal/deploy.DefaultIgnore). */
  const DEFAULT_IGNORE = [".git/", ".backyard.json", ".backyardignore", "node_modules/", ".claude/", ".cursor/", ".github/", "CLAUDE.md", "AGENTS.md", ".DS_Store", ".*", "!.well-known/"];

  // ===========================================================================
  // Pure helpers

  /** Returns "" when name is a valid site name, else a human-readable reason. */
  function validateSiteName(name, reserved) {
    if (!name) return "Enter a site name.";
    if (name.length > 63) return "Site names are at most 63 characters.";
    if (/[A-Z]/.test(name)) return "Use lowercase letters only.";
    if (!SITE_NAME_RE.test(name)) {
      if (/^-|-$/.test(name)) return "Site names cannot start or end with a dash.";
      return "Use lowercase letters, digits and dashes only.";
    }
    if ((reserved || DEFAULT_RESERVED).indexOf(name) >= 0) return `"${name}" is reserved; pick another name.`;
    return "";
  }

  /** Turns a folder name into a valid site name ("" if nothing usable remains). */
  function suggestSiteName(folder) {
    let s = String(folder || "")
      .normalize("NFKD")
      .replace(/[̀-ͯ]/g, "")
      .toLowerCase()
      .replace(/[^a-z0-9]+/g, "-")
      .replace(/^-+|-+$/g, "");
    if (s.length > 63) s = s.slice(0, 63).replace(/-+$/, "");
    if (DEFAULT_RESERVED.indexOf(s) >= 0) s = s + "-site";
    return SITE_NAME_RE.test(s) ? s : "";
  }

  /** Escapes a character for use outside a RegExp character class ("u" mode). */
  function escapeRegExp(s) {
    return s.replace(/[.*+?^${}()|[\]\\/]/g, "\\$&");
  }

  /** Escapes a character for use inside a RegExp character class ("u" mode). */
  function escapeClassChar(c) {
    return /[\\\]\[^-]/.test(c) ? "\\" + c : c;
  }

  /** Compiles one glob segment with Go path.Match semantics into a RegExp (null if invalid). */
  function globToRegExp(glob) {
    let re = "^";
    let i = 0;
    while (i < glob.length) {
      const c = glob[i];
      if (c === "*") {
        re += "[^/]*";
        i++;
      } else if (c === "?") {
        re += "[^/]";
        i++;
      } else if (c === "\\") {
        if (i + 1 >= glob.length) return null;
        re += escapeRegExp(glob[i + 1]);
        i += 2;
      } else if (c === "[") {
        let j = i + 1;
        let cls = "[";
        if (glob[j] === "^" || glob[j] === "!") {
          cls += "^";
          j++;
        }
        let n = 0;
        for (;;) {
          if (j >= glob.length) return null; // unterminated class
          if (glob[j] === "]" && n > 0) break;
          let lo = glob[j];
          if (lo === "\\") {
            j++;
            if (j >= glob.length) return null;
            lo = glob[j];
          }
          j++;
          cls += escapeClassChar(lo);
          if (glob[j] === "-" && j + 1 < glob.length && glob[j + 1] !== "]") {
            j++;
            let hi = glob[j];
            if (hi === "\\") {
              j++;
              if (j >= glob.length) return null;
              hi = glob[j];
            }
            j++;
            if (hi < lo) return null;
            cls += "-" + escapeClassChar(hi);
          }
          n++;
        }
        re += cls + "]";
        i = j + 1;
      } else {
        re += escapeRegExp(c);
        i++;
      }
    }
    try {
      return new RegExp(re + "$", "u");
    } catch (_) {
      return null;
    }
  }

  /** Parses one gitignore line (same rules as internal/deploy.Matcher). */
  function parseIgnoreLine(raw) {
    let line = String(raw).replace(/\r$/, "");
    while (line.endsWith(" ") && !line.endsWith("\\ ")) line = line.slice(0, -1);
    if (!line || line[0] === "#") return null;
    const rule = { negate: false, dirOnly: false, segs: [] };
    if (line[0] === "!") {
      rule.negate = true;
      line = line.slice(1);
    }
    if (line.endsWith("/")) {
      rule.dirOnly = true;
      line = line.replace(/\/+$/, "");
    }
    if (!line) return null;
    const anchored = line.indexOf("/") >= 0;
    for (let s of line.replace(/^\//, "").split("/")) {
      if (!s) continue;
      if (s !== "**" && s.indexOf("**") >= 0) s = s.replace(/\*{2,}/g, "*");
      if (s === "**") {
        rule.segs.push("**");
        continue;
      }
      const re = globToRegExp(s);
      if (!re) return null;
      rule.segs.push(re);
    }
    if (!rule.segs.length) return null;
    if (!anchored) rule.segs.unshift("**");
    return rule;
  }

  function matchSegments(pat, segs, pi, si) {
    while (pi < pat.length) {
      if (pat[pi] === "**") {
        if (pi === pat.length - 1) return si < segs.length;
        for (let k = si; k <= segs.length; k++) if (matchSegments(pat, segs, pi + 1, k)) return true;
        return false;
      }
      if (si >= segs.length || !pat[pi].test(segs[si])) return false;
      pi++;
      si++;
    }
    return si === segs.length;
  }

  /**
   * Compiles gitignore-style patterns. ignored(rel, isDir) also considers
   * parent directories (a path inside an excluded directory is excluded).
   */
  function compileIgnore(patterns) {
    const rules = [];
    for (const p of patterns || []) {
      const r = parseIgnoreLine(p);
      if (r) rules.push(r);
    }
    const match = (segs, isDir) => {
      let ignored = false;
      for (const r of rules) {
        if (r.dirOnly && !isDir) continue;
        if (r.negate !== ignored) continue;
        if (matchSegments(r.segs, segs, 0, 0)) ignored = !r.negate;
      }
      return ignored;
    };
    return {
      /** Whether rel itself matches (parents not considered). */
      matches(rel, isDir) {
        const s = String(rel).replace(/^\/+|\/+$/g, "");
        return !!s && rules.length > 0 && match(s.split("/"), !!isDir);
      },
      ignored(rel, isDir) {
        const s = String(rel).replace(/^\/+|\/+$/g, "");
        if (!s || !rules.length) return false;
        const segs = s.split("/");
        for (let i = 1; i < segs.length; i++) if (match(segs.slice(0, i), true)) return true;
        return match(segs, !!isDir);
      },
    };
  }

  function utf8Length(s) {
    let n = 0;
    for (const ch of s) {
      const c = ch.codePointAt(0);
      n += c < 0x80 ? 1 : c < 0x800 ? 2 : c < 0x10000 ? 3 : 4;
    }
    return n;
  }

  /** Returns "" when p is a deployable site-relative path, else why not (same rules as the gateway). */
  function validatePath(p) {
    if (!p) return "empty path";
    if (utf8Length(p) > MAX_PATH_BYTES) return "path longer than 1024 bytes";
    if (/[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?:^|[^\uD800-\uDBFF])[\uDC00-\uDFFF]/.test(p)) return "path is not valid UTF-8";
    if (p[0] === "/") return "path must be relative (no leading /)";
    if (p.indexOf("\\") >= 0) return 'path may not contain "\\"';
    if (p === "__backyard" || p.startsWith(RESERVED_PATH_PREFIX)) return 'path may not start with "__backyard/"';
    // eslint-disable-next-line no-control-regex
    if (/[\u0000-\u001f\u007f]/.test(p)) return "path contains a control character";
    for (const seg of p.split("/")) {
      if (seg === "") return "path contains an empty segment";
      if (seg === "." || seg === "..") return 'path may not contain "." or ".." segments';
    }
    return "";
  }

  // --- SHA-256 (fallback for insecure contexts where crypto.subtle is missing)

  const K256 = new Int32Array([
    0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5, 0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3,
    0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174, 0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
    0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967, 0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13,
    0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85, 0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
    0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3, 0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208,
    0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
  ]);

  /** Computes the hex SHA-256 of bytes (Uint8Array or ArrayBuffer) in pure JS. */
  function sha256Hex(data) {
    const bytes = data instanceof Uint8Array ? data : new Uint8Array(data);
    const H = new Int32Array([0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19]);
    const W = new Int32Array(64);
    const block = (buf, off) => {
      for (let t = 0; t < 16; t++) {
        const o = off + t * 4;
        W[t] = (buf[o] << 24) | (buf[o + 1] << 16) | (buf[o + 2] << 8) | buf[o + 3];
      }
      for (let t = 16; t < 64; t++) {
        const a = W[t - 15];
        const b = W[t - 2];
        const s0 = ((a >>> 7) | (a << 25)) ^ ((a >>> 18) | (a << 14)) ^ (a >>> 3);
        const s1 = ((b >>> 17) | (b << 15)) ^ ((b >>> 19) | (b << 13)) ^ (b >>> 10);
        W[t] = (W[t - 16] + s0 + W[t - 7] + s1) | 0;
      }
      let a = H[0],
        b = H[1],
        c = H[2],
        d = H[3],
        e = H[4],
        f = H[5],
        g = H[6],
        h = H[7];
      for (let t = 0; t < 64; t++) {
        const S1 = ((e >>> 6) | (e << 26)) ^ ((e >>> 11) | (e << 21)) ^ ((e >>> 25) | (e << 7));
        const t1 = (h + S1 + ((e & f) ^ (~e & g)) + K256[t] + W[t]) | 0;
        const S0 = ((a >>> 2) | (a << 30)) ^ ((a >>> 13) | (a << 19)) ^ ((a >>> 22) | (a << 10));
        const t2 = (S0 + ((a & b) ^ (a & c) ^ (b & c))) | 0;
        h = g;
        g = f;
        f = e;
        e = (d + t1) | 0;
        d = c;
        c = b;
        b = a;
        a = (t1 + t2) | 0;
      }
      H[0] = (H[0] + a) | 0;
      H[1] = (H[1] + b) | 0;
      H[2] = (H[2] + c) | 0;
      H[3] = (H[3] + d) | 0;
      H[4] = (H[4] + e) | 0;
      H[5] = (H[5] + f) | 0;
      H[6] = (H[6] + g) | 0;
      H[7] = (H[7] + h) | 0;
    };
    const n = bytes.length;
    const full = Math.floor(n / 64);
    for (let i = 0; i < full; i++) block(bytes, i * 64);
    const rem = n - full * 64;
    const tail = new Uint8Array(rem < 56 ? 64 : 128);
    tail.set(bytes.subarray(full * 64));
    tail[rem] = 0x80;
    const hi = Math.floor(n / 0x20000000);
    const lo = (n * 8) >>> 0;
    const L = tail.length;
    tail[L - 8] = hi >>> 24;
    tail[L - 7] = hi >>> 16;
    tail[L - 6] = hi >>> 8;
    tail[L - 5] = hi;
    tail[L - 4] = lo >>> 24;
    tail[L - 3] = lo >>> 16;
    tail[L - 2] = lo >>> 8;
    tail[L - 1] = lo;
    block(tail, 0);
    if (L === 128) block(tail, 64);
    let out = "";
    for (let i = 0; i < 8; i++) out += (H[i] >>> 0).toString(16).padStart(8, "0");
    return out;
  }

  function toHex(buf) {
    const b = new Uint8Array(buf);
    let s = "";
    for (let i = 0; i < b.length; i++) s += b[i].toString(16).padStart(2, "0");
    return s;
  }

  /** Hashes a File/Blob: Web Crypto when available (secure contexts), else pure JS. */
  async function hashBlob(blob) {
    const buf = await blob.arrayBuffer();
    const c = globalThis.crypto;
    if (c && c.subtle && typeof c.subtle.digest === "function") {
      try {
        return toHex(await c.subtle.digest("SHA-256", buf));
      } catch (_) {
        /* fall back */
      }
    }
    return sha256Hex(new Uint8Array(buf));
  }

  /** Formats a byte count ("1.2 MB"). */
  function formatBytes(n) {
    if (!(n >= 0)) return "";
    if (n < 1000) return n + " B";
    const units = ["KB", "MB", "GB", "TB"];
    let v = n;
    let u = -1;
    do {
      v /= 1000;
      u++;
    } while (v >= 1000 && u < units.length - 1);
    const rounded = v >= 100 ? Math.round(v) : v >= 10 ? Math.round(v * 10) / 10 : Math.round(v * 100) / 100;
    return rounded + " " + units[u];
  }

  const TIME_UNITS = [
    ["year", 365 * 86400],
    ["month", 30 * 86400],
    ["week", 7 * 86400],
    ["day", 86400],
    ["hour", 3600],
    ["minute", 60],
  ];

  /** "3 hours ago" style relative time (locale default: the browser's). */
  function relativeTime(date, now, locale) {
    const t = date instanceof Date ? date.getTime() : Date.parse(date);
    if (isNaN(t)) return "";
    const diff = (t - (now === undefined ? Date.now() : now)) / 1000;
    if (Math.abs(diff) < 45) return "just now";
    let rtf = null;
    try {
      rtf = new Intl.RelativeTimeFormat(locale, { numeric: "auto" });
    } catch (_) {
      rtf = null;
    }
    for (const [unit, secs] of TIME_UNITS) {
      if (Math.abs(diff) >= secs || unit === "minute") {
        const v = Math.round(diff / secs);
        if (rtf) return rtf.format(v, unit);
        const abs = Math.abs(v);
        return abs + " " + unit + (abs === 1 ? "" : "s") + (v < 0 ? " ago" : " from now");
      }
    }
    return "";
  }

  function trimSlash(s) {
    return String(s || "").replace(/\/+$/, "");
  }

  /** The URL a site will have (SPEC §2): host mode subdomain or path mode /s/<name>/. */
  function siteURLFor(info, name, origin) {
    const base = trimSlash((info && info.public_url) || origin || "");
    if (!base || !name) return "";
    if (info && info.url_mode === "path") return base + "/s/" + name + "/";
    let u;
    try {
      u = new URL(base);
    } catch (_) {
      return "";
    }
    const host = (info && info.base_domain) || u.hostname;
    return u.protocol + "//" + name + "." + host + (u.port ? ":" + u.port : "") + "/";
  }

  function shellQuote(s) {
    return /^[A-Za-z0-9_@%+=:,./-]+$/.test(s) ? s : "'" + String(s).replace(/'/g, "'\\''") + "'";
  }

  /** The CLI commands shown on the hub, built from GET /info. */
  function cliCommands(info, origin) {
    info = info || {};
    const auth = info.auth || {};
    const server = trimSlash(info.public_url || origin || "");
    const embedded = server + GATEWAY;
    const gateway = trimSlash(info.gateway_url || "");
    let login = "backyard login --server " + shellQuote(server);
    if (gateway && gateway !== embedded) login += " --gateway " + shellQuote(gateway);
    let note;
    const mode = auth.mode || "none";
    if (mode === "none") {
      note = auth.anonymous_deploy
        ? "This server accepts deploys without credentials."
        : "You will be asked for the deploy token (on the Cloudflare edition: the DEPLOY_TOKEN printed by setup).";
    } else if (auth.device) {
      note = "Opens your browser to sign in. On a headless machine add --device (or --no-browser).";
    } else {
      note = "Opens your browser to sign in. On a headless machine add --no-browser.";
    }
    return {
      install: "curl -fsSL https://raw.githubusercontent.com/thasami/backyard/main/scripts/install.sh | sh",
      installGo: "go install github.com/thasami/backyard/cmd/backyard@latest",
      login,
      loginNote: note,
      deploy: "backyard init my-site && cd my-site && backyard deploy",
      skill: "backyard skill install --global",
      mcp: "claude mcp add backyard -- backyard mcp",
    };
  }

  /** Extracts a deploy token handed over in the URL fragment (#token=…). */
  function tokenFromHash(hash) {
    const m = /(?:^#|&)token=([^&]+)/.exec(String(hash || ""));
    if (!m) return "";
    try {
      return decodeURIComponent(m[1]).trim();
    } catch (_) {
      return "";
    }
  }

  /** A readable label for a User (SPEC §4): "Name (id)", or just the id. */
  function userLabel(u) {
    if (!u || typeof u !== "object") return "an unknown account";
    const id = typeof u.id === "string" ? u.id.trim() : "";
    const name = typeof u.name === "string" ? u.name.trim() : "";
    if (u.anonymous) return "an anonymous guest" + (id ? " (" + id + ")" : "");
    if (name && id && name !== id) return name + " (" + id + ")";
    return id || name || "an unknown account";
  }

  /**
   * The confirmation asked before a deploy token handed over in a link
   * (#token=…) is used: a link must never switch the identity deploys and
   * deletes run as without the visitor's consent. owner is the token's User
   * (GET G/me with the token), or null when the server could not tell
   * (problem says why); me is the visitor's own User, if known.
   * Returns { title, body }.
   */
  function handedTokenPrompt(owner, me, problem) {
    const signedIn = !!me && !me.anonymous && !!me.id;
    if (!owner) {
      return {
        title: "Use a deploy token from a link?",
        body:
          "The link you opened carries a deploy token, but Backyard could not check whose it is" +
          (problem ? " (" + problem + ")" : "") +
          ". If you use it, deploys and deletes from this page use that token" +
          (signedIn ? " instead of your own sign-in (" + userLabel(me) + ")." : "."),
      };
    }
    const who = userLabel(owner);
    let body = "The link you opened carries the deploy token of " + who + ". ";
    if (signedIn && owner.id === me.id) body += "It is a token of your own account: deploys and deletes from this page use it instead of your sign-in.";
    else if (signedIn) body += "If you use it, deploys and deletes from this page run as " + who + ", not as you (" + userLabel(me) + ").";
    else body += "If you use it, deploys and deletes from this page run as " + who + ".";
    return { title: "Use the deploy token of " + who + "?", body };
  }

  /** Returns url if it is an http(s) URL or a same-origin absolute path, else "#". */
  function safeHref(url) {
    const u = String(url || "").trim();
    return /^(https?:\/\/|\/(?![/\\]))/i.test(u) ? u : "#";
  }

  /** Whether a login URL from the server may be followed (never "//host" or javascript:). */
  function isSafeRedirect(url) {
    return !!url && safeHref(url) !== "#";
  }

  /** The hub's login URL that returns to href after signing in (SPEC §8). */
  function loginURLFor(href) {
    return "/__backyard/auth/login?rd=" + encodeURIComponent(String(href));
  }

  /**
   * Whether a same-origin JSON fetch (made with redirect: "manual") was
   * answered by a login flow rather than the API: an identity-aware proxy or
   * the server redirected (opaqueredirect; followed, it would end at the IdP
   * as a CORS failure) or served an HTML login page in place of JSON.
   */
  function isLoginResponse(res) {
    if (!res) return false;
    if (res.type === "opaqueredirect") return true;
    const ct = String((res.headers && res.headers.get && res.headers.get("Content-Type")) || "").toLowerCase();
    return !!res.ok && ct.indexOf("text/html") >= 0;
  }

  /**
   * Whether the deploy token should be persisted by default: only in host
   * mode. In path mode every site shares the hub's origin and could read
   * localStorage; when the server info is unknown, err on the safe side.
   */
  function rememberByDefault(info) {
    return !!info && info.url_mode !== "path";
  }

  function initialsOf(name) {
    const parts = String(name || "")
      .replace(/@.*$/, "")
      .split(/[\s._-]+/)
      .filter(Boolean);
    const s = parts.length > 1 ? parts[0][0] + parts[parts.length - 1][0] : (parts[0] || "?").slice(0, 2);
    return s.toUpperCase();
  }

  // --- File trees: drag-and-drop entries, <input webkitdirectory>, plain files

  /** A virtual directory wrapping a list of entries. */
  function virtualDir(name, entries) {
    return { isDirectory: true, isFile: false, name: name || "", _entries: entries };
  }

  /** Builds a virtual entry tree from [{path, file}] ("/"-separated paths). */
  function treeFromPaths(items) {
    const root = virtualDir("", []);
    for (const { path, file } of items) {
      const segs = String(path).split("/").filter(Boolean);
      if (!segs.length) continue;
      let dir = root;
      for (let i = 0; i < segs.length - 1; i++) {
        let next = dir._entries.find((e) => e.isDirectory && e.name === segs[i]);
        if (!next) {
          next = virtualDir(segs[i], []);
          dir._entries.push(next);
        }
        dir = next;
      }
      dir._entries.push({ isFile: true, isDirectory: false, name: segs[segs.length - 1], _file: file });
    }
    return root;
  }

  async function readDir(dir) {
    if (dir._entries) return dir._entries;
    const reader = dir.createReader();
    const all = [];
    for (;;) {
      const batch = await new Promise((resolve, reject) => reader.readEntries(resolve, reject));
      if (!batch.length) return all;
      all.push(...batch);
    }
  }

  function fileOf(entry) {
    if (entry._file) return Promise.resolve(entry._file);
    return new Promise((resolve, reject) => entry.file(resolve, reject));
  }

  /**
   * Collects deployable files from a directory tree, applying the default
   * ignore rules, <root>/.backyardignore and .backyard.json ("dir", "ignore").
   * Ignored directories are never descended into.
   */
  async function collectTree(root) {
    const top = await readDir(root);
    let config = {};
    const configWarnings = [];
    const cfgEntry = top.find((e) => e.isFile && e.name === ".backyard.json");
    if (cfgEntry) {
      try {
        const parsed = JSON.parse(await (await fileOf(cfgEntry)).text());
        if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) config = parsed;
      } catch (err) {
        configWarnings.push(".backyard.json is not valid JSON; ignoring it");
      }
    }
    let deployRoot = root;
    let rootEntries = top;
    const sub = typeof config.dir === "string" ? config.dir.replace(/^\.\/?|\/+$/g, "") : "";
    if (sub) {
      let dir = root;
      for (const seg of sub.split("/").filter(Boolean)) {
        const next = (await readDir(dir)).find((e) => e.isDirectory && e.name === seg);
        if (!next) throw new Error(`.backyard.json sets "dir": "${config.dir}", but the dropped folder has no such directory.`);
        dir = next;
      }
      deployRoot = dir;
      rootEntries = await readDir(dir);
    }
    const patterns = DEFAULT_IGNORE.slice();
    const ign = rootEntries.find((e) => e.isFile && e.name === ".backyardignore");
    if (ign) patterns.push(...(await (await fileOf(ign)).text()).split("\n"));
    if (Array.isArray(config.ignore)) patterns.push(...config.ignore.filter((p) => typeof p === "string"));
    const matcher = compileIgnore(patterns);

    const files = [];
    const ignored = [];
    const invalid = [];
    const walk = async (entries, prefix) => {
      for (const e of entries) {
        const rel = prefix + e.name;
        if (e.isDirectory) {
          if (matcher.matches(rel, true)) {
            ignored.push(rel + "/");
            continue;
          }
          await walk(await readDir(e), rel + "/");
        } else if (e.isFile) {
          if (matcher.matches(rel, false)) {
            ignored.push(rel);
            continue;
          }
          const why = validatePath(rel);
          if (why) {
            invalid.push({ path: rel, reason: why });
            continue;
          }
          files.push({ path: rel, file: await fileOf(e) });
        }
      }
    };
    await walk(rootEntries, "");
    files.sort((a, b) => (a.path < b.path ? -1 : a.path > b.path ? 1 : 0));
    ignored.sort();
    return { files, ignored, invalid, config, warnings: configWarnings, dir: sub };
  }

  // --- Deploy protocol (SPEC §7.3)

  class HubError extends Error {
    constructor(message, opts) {
      super(message);
      this.name = "HubError";
      Object.assign(this, opts || {});
    }
  }

  async function errorFromResponse(res, what) {
    let body = null;
    try {
      body = await res.json();
    } catch (_) {
      body = null;
    }
    const e = (body && body.error) || {};
    const message = (typeof e.message === "string" && e.message) || `${what} failed (HTTP ${res.status})`;
    const ra = res.headers && res.headers.get ? res.headers.get("Retry-After") : null;
    return new HubError(message, {
      status: res.status,
      code: e.code || "",
      loginUrl: e.login_url || "",
      retryAfter: ra && /^\d+$/.test(ra) ? parseInt(ra, 10) : undefined,
    });
  }

  function gatewayHeaders(token, json) {
    const h = { Accept: "application/json", "X-Backyard": "1" };
    if (json) h["Content-Type"] = "application/json";
    if (token) h.Authorization = "Bearer " + token;
    return h;
  }

  async function gatewayCall(fetchImpl, url, init, what) {
    let res;
    try {
      res = await fetchImpl(url, Object.assign({ credentials: "same-origin" }, init));
    } catch (err) {
      throw new HubError(`${what} failed: network error (${(err && err.message) || err})`, { status: 0, code: "network_error", retryable: true });
    }
    if (!res.ok) throw await errorFromResponse(res, what);
    if (res.status === 204) return null;
    const text = await res.text();
    if (!text) return null;
    try {
      return JSON.parse(text);
    } catch (_) {
      throw new HubError(`${what} failed: the server did not return JSON (is a login proxy in the way?)`, { status: res.status, code: "bad_response" });
    }
  }

  const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

  /** Upload failures worth retrying: network errors, 429 and 5xx. */
  const retryableUpload = (err) => !!err && (err.retryable || err.status === 429 || err.status >= 500);

  /**
   * API calls worth retrying (create and commit are idempotent): network
   * errors, 429 and gateway/availability errors, but not 500 (deterministic).
   */
  const retryableCall = (err) => !!err && (err.retryable || err.status === 429 || err.status === 502 || err.status === 503 || err.status === 504);

  /** Runs fn up to attempts times while retryable(err), backing off (or honouring Retry-After). */
  async function withRetry(fn, attempts, delayMs, retryable) {
    const shouldRetry = retryable || retryableUpload;
    let last;
    for (let i = 0; i < attempts; i++) {
      try {
        return await fn(i);
      } catch (err) {
        last = err;
        if (!shouldRetry(err) || i === attempts - 1) throw err;
        const wait = err.retryAfter !== undefined ? Math.min(err.retryAfter * 1000, 30000) : delayMs * Math.pow(3, i);
        await sleep(wait);
      }
    }
    throw last;
  }

  /**
   * Deploys files ([{path, size, sha256, file}]) to site through the gateway:
   * create → PUT uploads (given headers, with progress) → commit.
   * opts: { gateway, site, files, token, message, fetch, upload, onProgress, concurrency, retryDelay }
   * upload({url, method, headers, file, onProgress(loadedBytes)}) → Promise.
   * Resolves with { site, url, uploaded, unchanged, deleted, bytes }.
   */
  async function deploySite(opts) {
    const fetchImpl = opts.fetch || ((...a) => globalThis.fetch(...a));
    const upload = opts.upload;
    const progress = opts.onProgress || function () {};
    const gateway = trimSlash(opts.gateway);
    const base = gateway + "/sites/" + encodeURIComponent(opts.site);
    const entries = opts.files.map((f) => ({ path: f.path, size: f.size, sha256: f.sha256 }));
    const byPath = new Map(opts.files.map((f) => [f.path, f]));
    const retryDelay = opts.retryDelay === undefined ? 1000 : opts.retryDelay;

    progress({ phase: "plan" });
    const createBody = JSON.stringify({ files: entries, message: opts.message || "Deployed from the hub" });
    const plan = await withRetry(
      () => gatewayCall(fetchImpl, base + "/deploys", { method: "POST", headers: gatewayHeaders(opts.token, true), body: createBody }, "Starting the deploy"),
      3,
      retryDelay,
      retryableCall,
    );
    if (!plan || typeof plan.deploy_id !== "string") throw new HubError("Starting the deploy failed: unexpected response", { code: "bad_response" });
    const uploads = Array.isArray(plan.uploads) ? plan.uploads : [];
    let total = 0;
    for (const u of uploads) {
      const f = byPath.get(u.path);
      if (!f) throw new HubError(`The server asked for an unknown file: ${u.path}`, { code: "bad_response" });
      total += f.size;
    }
    const loaded = new Map();
    let doneFiles = 0;
    const report = () => {
      let sum = 0;
      for (const v of loaded.values()) sum += v;
      progress({ phase: "upload", loaded: Math.min(sum, total), total, files: doneFiles, fileCount: uploads.length });
    };
    report();
    let resolveBase = gateway;
    try {
      resolveBase = new URL(gateway + "/", globalThis.location ? globalThis.location.href : undefined).href;
    } catch (_) {
      /* keep */
    }
    let next = 0;
    const worker = async () => {
      while (next < uploads.length) {
        const u = uploads[next++];
        const f = byPath.get(u.path);
        let url = u.url;
        try {
          url = new URL(u.url, resolveBase).href;
        } catch (_) {
          /* keep as given */
        }
        await withRetry(
          () => {
            loaded.set(u.path, 0);
            return upload({
              url,
              method: u.method || "PUT",
              headers: u.headers || {},
              file: f.file,
              onProgress: (n) => {
                loaded.set(u.path, Math.min(n, f.size));
                report();
              },
            });
          },
          3,
          retryDelay,
        );
        loaded.set(u.path, f.size);
        doneFiles++;
        report();
      }
    };
    const n = Math.max(1, Math.min(opts.concurrency || 4, uploads.length || 1));
    await Promise.all(Array.from({ length: n }, worker));

    progress({ phase: "commit" });
    const commitURL = base + "/deploys/" + encodeURIComponent(plan.deploy_id) + "/commit";
    const commitBody = JSON.stringify({ files: entries });
    const result = await withRetry(
      () => gatewayCall(fetchImpl, commitURL, { method: "POST", headers: gatewayHeaders(opts.token, true), body: commitBody }, "Finishing the deploy"),
      3,
      retryDelay,
      retryableCall,
    );
    progress({ phase: "done" });
    return {
      site: result && result.site,
      url: (result && (result.url || (result.site && result.site.url))) || "",
      uploaded: uploads.length,
      unchanged: Array.isArray(plan.unchanged) ? plan.unchanged.length : 0,
      deleted: (result && result.deleted) || 0,
      bytes: total,
    };
  }

  /** Bytes of file content hashed at the same time (bounds browser memory). */
  const HASH_BUDGET = 64 * 1024 * 1024;

  /**
   * Hashes files ([{path, file}]) concurrently while keeping at most
   * opts.budget bytes in memory (a larger file is hashed alone). Resolves
   * with [{path, size, sha256, file}] in input order.
   * opts: { concurrency (4), budget, hash (blob → hex), onProgress(done, total) }.
   */
  async function hashFiles(files, opts) {
    opts = opts || {};
    const hash = opts.hash || hashBlob;
    const budget = opts.budget || HASH_BUDGET;
    const onProgress = opts.onProgress || function () {};
    const out = new Array(files.length);
    let next = 0;
    let done = 0;
    let inFlight = 0;
    let waiters = [];
    const acquire = async (n) => {
      while (inFlight > 0 && inFlight + n > budget) await new Promise((resolve) => waiters.push(resolve));
      inFlight += n;
    };
    const release = (n) => {
      inFlight -= n;
      const w = waiters;
      waiters = [];
      for (const resolve of w) resolve();
    };
    const worker = async () => {
      while (next < files.length) {
        const i = next++;
        const f = files[i];
        const cost = Math.max(1, Math.min(f.file.size, budget));
        await acquire(cost);
        try {
          out[i] = { path: f.path, size: f.file.size, sha256: await hash(f.file), file: f.file };
        } finally {
          release(cost);
        }
        onProgress(++done, files.length);
      }
    };
    await Promise.all(Array.from({ length: Math.max(1, Math.min(opts.concurrency || 4, files.length)) }, worker));
    return out;
  }

  /**
   * Request headers a page may not set (Fetch "forbidden request-header"):
   * the browser computes them itself (e.g. Content-Length from the body), and
   * trying to set them only logs a console error.
   */
  const FORBIDDEN_HEADER_RE =
    /^(accept-charset|accept-encoding|access-control-request-headers|access-control-request-method|connection|content-length|cookie|cookie2|date|dnt|expect|host|keep-alive|origin|referer|set-cookie|te|trailer|transfer-encoding|upgrade|via|proxy-.*|sec-.*)$/i;

  /**
   * The headers of an upload instruction as [name, value] pairs, exactly as
   * the gateway gave them (presigned URLs sign some of them, e.g. S3
   * x-amz-*, Azure x-ms-blob-type, Content-Type): names keep their case,
   * values are not altered. Names differing only in case collapse to the last
   * one (a browser would join them into one invalid value); forbidden headers
   * are left to the browser.
   */
  function uploadHeaders(headers) {
    const out = new Map();
    for (const [name, value] of Object.entries(headers || {})) {
      if (value === undefined || value === null || !/^[!#$%&'*+.^_`|~0-9A-Za-z-]+$/.test(name) || FORBIDDEN_HEADER_RE.test(name)) continue;
      out.set(name.toLowerCase(), [name, Array.isArray(value) ? value.join(",") : String(value)]);
    }
    return Array.from(out.values());
  }

  /** The origin of url resolved against base ("" when it cannot be parsed). */
  function originOf(url, base) {
    try {
      return new URL(url, base || undefined).origin;
    } catch (_) {
      return "";
    }
  }

  /**
   * Explains a failed upload PUT that never got an HTTP response. To a
   * presigned storage URL on another origin that is almost always the
   * bucket's CORS configuration (the browser hides the actual reason).
   */
  function uploadNetworkError(fileName, url, pageOrigin, headerNames) {
    const target = originOf(url, pageOrigin);
    const name = fileName || "a file";
    if (target && pageOrigin && target !== pageOrigin) {
      let host = target;
      try {
        host = new URL(target).host;
      } catch (_) {
        /* keep the origin */
      }
      const allowed = (headerNames || []).filter((h) => !/^content-length$/i.test(h));
      return new HubError(
        `Uploading ${name} to ${host} failed: network or CORS error. ` +
          `If it keeps failing, the bucket CORS must allow PUT from ${pageOrigin}` +
          (allowed.length ? ` with the headers ${allowed.join(", ")}` : "") +
          ": ask your Backyard admin (deploying with the backyard CLI is not affected).",
        { status: 0, code: "cors", retryable: true },
      );
    }
    return new HubError(`Network error while uploading ${name}`, { status: 0, code: "network_error", retryable: true });
  }

  /** Extracts a readable reason from an upload error body (Backyard JSON or S3/GCS/Azure XML). */
  function uploadErrorDetail(text) {
    const body = String(text || "");
    try {
      const j = JSON.parse(body);
      if (j && j.error && typeof j.error.message === "string" && j.error.message) return j.error.message;
    } catch (_) {
      /* not JSON */
    }
    const tag = (t) => {
      const m = new RegExp("<" + t + ">([^<]{1,500})</" + t + ">").exec(body);
      return m ? m[1].trim() : "";
    };
    const message = tag("Message");
    const code = tag("Code");
    if (message && code) return `${message} (${code})`;
    return message || code;
  }

  /**
   * Creates the upload function used by deploySite: an XMLHttpRequest PUT
   * (for upload progress) with exactly the given headers and the raw bytes.
   * The body is sent as a type-less Blob, so the browser never adds a
   * Content-Type the presigned signature did not include; no cookies are sent
   * to cross-origin URLs (withCredentials stays false).
   * env: { XMLHttpRequest, origin (the hub origin), stallMs, setTimeout, clearTimeout }.
   */
  function createUploader(env) {
    const XHR = env.XMLHttpRequest;
    const stallMs = env.stallMs || 120000;
    const setT = env.setTimeout || setTimeout;
    const clearT = env.clearTimeout || clearTimeout;
    return function upload({ url, method, headers, file, onProgress }) {
      return new Promise((resolve, reject) => {
        const xhr = new XHR();
        const name = (file && file.name) || "";
        const label = name || "a file";
        const pairs = uploadHeaders(headers);
        let stall = null;
        let done = false;
        const settle = (fn, arg) => {
          if (done) return;
          done = true;
          clearT(stall);
          fn(arg);
        };
        const watchdog = () => {
          clearT(stall);
          stall = setT(() => {
            xhr.onload = xhr.onerror = xhr.ontimeout = xhr.onabort = null;
            try {
              xhr.abort();
            } catch (_) {
              /* ignore */
            }
            settle(reject, new HubError(`Uploading ${label} stalled`, { status: 0, retryable: true }));
          }, stallMs);
        };
        xhr.open(method || "PUT", url);
        for (const [k, v] of pairs) {
          try {
            xhr.setRequestHeader(k, v);
          } catch (_) {
            /* refused by the browser: it sets this header itself */
          }
        }
        xhr.upload.onprogress = (e) => {
          watchdog();
          if (onProgress) onProgress(e.loaded);
        };
        xhr.upload.onload = watchdog; // body sent: now waiting for the response
        xhr.onabort = () => settle(reject, new HubError("The upload was aborted", { status: 0, retryable: true }));
        xhr.onload = () => {
          if (xhr.status >= 200 && xhr.status < 300) return settle(resolve);
          if (xhr.status === 0) return settle(reject, uploadNetworkError(name, url, env.origin, pairs.map((p) => p[0])));
          let detail = "";
          try {
            detail = uploadErrorDetail(xhr.responseText);
          } catch (_) {
            detail = "";
          }
          const ra = xhr.getResponseHeader("Retry-After");
          settle(
            reject,
            new HubError(detail ? `Uploading ${label} failed: ${detail} (HTTP ${xhr.status})` : `Uploading ${label} failed (HTTP ${xhr.status})`, {
              status: xhr.status,
              retryAfter: ra && /^\d+$/.test(ra) ? +ra : undefined,
            }),
          );
        };
        xhr.onerror = () => settle(reject, uploadNetworkError(name, url, env.origin, pairs.map((p) => p[0])));
        xhr.ontimeout = xhr.onerror;
        watchdog();
        const body = file && typeof file.slice === "function" ? file.slice(0, file.size) : file;
        xhr.send(body);
      });
    };
  }

  /** Deletes a site through the gateway. */
  function deleteSite(opts) {
    const fetchImpl = opts.fetch || ((...a) => globalThis.fetch(...a));
    return gatewayCall(
      fetchImpl,
      trimSlash(opts.gateway) + "/sites/" + encodeURIComponent(opts.site),
      { method: "DELETE", headers: gatewayHeaders(opts.token, false) },
      "Deleting the site",
    );
  }

  /**
   * Asks the deploy gateway whose token this is (GET G/me with the token as
   * the bearer, SPEC §7.2), before a token handed over in a link is used.
   * Resolves with the User; rejects with a HubError (status 401 or 403: the
   * token was rejected).
   */
  async function tokenOwner(opts) {
    const token = String(opts.token || "");
    // Only visible ASCII can be a bearer token (anything else cannot even be sent).
    if (!/^[\x21-\x7e]+$/.test(token)) throw new HubError("not a valid deploy token", { status: 401, code: "unauthenticated" });
    const fetchImpl = opts.fetch || ((...a) => globalThis.fetch(...a));
    const u = await gatewayCall(fetchImpl, trimSlash(opts.gateway) + "/me", { method: "GET", headers: gatewayHeaders(token, false) }, "Checking the deploy token");
    if (!u || typeof u !== "object" || typeof u.id !== "string" || !u.id) {
      throw new HubError("Checking the deploy token failed: the server did not return an account", { status: 200, code: "bad_response" });
    }
    return u;
  }

  /** Filters and sorts sites for the directory (newest first). */
  function filterSites(sites, query) {
    const q = String(query || "")
      .trim()
      .toLowerCase();
    const terms = q ? q.split(/\s+/) : [];
    const out = (sites || []).filter((s) => {
      if (!terms.length) return true;
      const hay = [s.name, s.title, s.description, s.updated_by, s.created_by, (s.owners || []).join(" ")].join(" ").toLowerCase();
      return terms.every((t) => hay.indexOf(t) >= 0);
    });
    const key = (s) => Date.parse(s.updated_at || s.created_at || "") || 0;
    out.sort((a, b) => key(b) - key(a) || String(a.name).localeCompare(String(b.name)));
    return out;
  }

  const helpers = {
    DEFAULT_IGNORE,
    DEFAULT_RESERVED,
    validateSiteName,
    suggestSiteName,
    globToRegExp,
    compileIgnore,
    validatePath,
    sha256Hex,
    hashBlob,
    formatBytes,
    relativeTime,
    siteURLFor,
    cliCommands,
    tokenFromHash,
    userLabel,
    handedTokenPrompt,
    tokenOwner,
    safeHref,
    isSafeRedirect,
    loginURLFor,
    isLoginResponse,
    rememberByDefault,
    initialsOf,
    treeFromPaths,
    virtualDir,
    collectTree,
    hashFiles,
    deploySite,
    uploadHeaders,
    uploadErrorDetail,
    createUploader,
    deleteSite,
    filterSites,
    HubError,
  };
  globalThis.BackyardHub = helpers;

  if (typeof document === "undefined" || !document.body || document.body.id !== "hub") return;

  // ===========================================================================
  // UI

  const $ = (id) => document.getElementById(id);
  const state = {
    info: null,
    me: null,
    sites: null,
    query: "",
    view: "grid",
    selection: null,
    nameTouched: false,
    busy: false,
    token: "",
    remember: null, // null: not decided yet (depends on the URL mode)
    tokenRequired: false, // a deploy was rejected for lack of a (valid) token
    tokenWanted: false, // a delete was rejected for lack of a (valid) token
    fresh: "",
    deleting: "",
  };

  function store(key, value) {
    try {
      if (value) localStorage.setItem(key, value);
      else localStorage.removeItem(key);
    } catch (_) {
      /* storage unavailable */
    }
  }
  function load(key) {
    try {
      return localStorage.getItem(key) || "";
    } catch (_) {
      return "";
    }
  }

  function el(tag, attrs, ...children) {
    const node = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs || {})) {
      if (v === undefined || v === null || v === false) continue;
      if (k === "class") node.className = v;
      else if (k === "text") node.textContent = v;
      else if (k.startsWith("on")) node.addEventListener(k.slice(2), v);
      else node.setAttribute(k, v === true ? "" : String(v));
    }
    for (const c of children) if (c !== null && c !== undefined && c !== false) node.append(c);
    return node;
  }

  function icon(paths) {
    const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
    svg.setAttribute("viewBox", "0 0 24 24");
    svg.setAttribute("class", "icon");
    svg.setAttribute("aria-hidden", "true");
    for (const d of paths) {
      const p = document.createElementNS("http://www.w3.org/2000/svg", "path");
      p.setAttribute("d", d);
      svg.append(p);
    }
    return svg;
  }
  const ICON_COPY = ["M9 9h10v10H9z", "M5 15V5h10"];
  const ICON_TRASH = ["M4 7h16", "M10 11v6M14 11v6", "M6 7l1 12a2 2 0 0 0 2 2h6a2 2 0 0 0 2-2l1-12", "M9 7V4h6v3"];

  let toastTimer = null;
  function toast(msg) {
    const t = $("toast");
    t.textContent = msg;
    t.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => (t.hidden = true), 2600);
  }

  async function copyText(text) {
    let copied = false;
    try {
      await navigator.clipboard.writeText(text);
      copied = true;
    } catch (_) {
      // No clipboard permission (or an insecure context): the legacy way.
      const ta = el("textarea", { readonly: true, class: "visually-hidden" });
      ta.value = text;
      document.body.append(ta);
      ta.select();
      try {
        copied = document.execCommand("copy") === true;
      } catch (__) {
        copied = false;
      }
      ta.remove();
    }
    // Never claim success when nothing reached the clipboard.
    toast(copied ? "Copied to clipboard" : "Could not copy: select the text and copy it yourself");
  }

  /** Same key and window as the docs page: one automatic re-login per 30 s (no loops). */
  const RELOGIN_KEY = "backyard.reloginAt";

  /** Sends the browser to url to sign in again, unless that was just tried. */
  function relogin(url) {
    let last = 0;
    try {
      last = Number(sessionStorage.getItem(RELOGIN_KEY)) || 0;
      if (last && Date.now() - last >= 0 && Date.now() - last < 30000) return false;
      sessionStorage.setItem(RELOGIN_KEY, String(Date.now()));
    } catch (_) {
      /* storage unavailable: redirect anyway */
    }
    location.href = url;
    return true;
  }

  async function getJSON(path) {
    const res = await fetch(path, { credentials: "same-origin", redirect: "manual", headers: { Accept: "application/json" } });
    if (isLoginResponse(res)) {
      // The session expired behind a proxy (or the server sent us to log in).
      const loginUrl = loginURLFor(location.href);
      relogin(loginUrl);
      throw new HubError("Your session has expired; sign in again.", { status: 401, code: "unauthenticated", loginUrl });
    }
    if (!res.ok) {
      const err = await errorFromResponse(res, "GET " + path);
      if (res.status === 401 && isSafeRedirect(err.loginUrl)) location.href = err.loginUrl;
      throw err;
    }
    return res.json();
  }

  // --- header, CLI and agent panels

  function serverName() {
    return (state.info && state.info.name) || "Backyard";
  }

  function renderHeader() {
    const name = serverName();
    for (const n of document.querySelectorAll("[data-server-name]")) n.textContent = name;
    document.title = name;
    const v = state.info && state.info.version;
    $("footer-version").textContent = (name === "Backyard" ? "Backyard" : name + " · Backyard") + (v ? " v" + v : "");

    const me = state.me;
    if (!me) return;
    $("user").hidden = false;
    const display = me.name || me.email || me.id;
    $("user-name").textContent = display;
    $("user").title = [me.name, me.email || me.id].filter(Boolean).join(" · ");
    const avatar = $("user-avatar");
    avatar.textContent = "";
    if (me.avatar_url && /^https?:\/\//i.test(me.avatar_url)) {
      const img = el("img", { src: me.avatar_url, alt: "", referrerpolicy: "no-referrer" });
      img.addEventListener("error", () => {
        img.remove();
        avatar.textContent = initialsOf(display);
      });
      avatar.append(img);
    } else {
      avatar.textContent = initialsOf(display);
    }
    $("user-admin").hidden = !me.admin;
    const mode = state.info && state.info.auth && state.info.auth.mode;
    const logout = $("logout");
    if (mode === "access") logout.href = "/cdn-cgi/access/logout"; // Cloudflare Access (Workers edition)
    logout.hidden = !(mode && (OIDC_MODES.indexOf(mode) >= 0 || mode === "access")) || me.anonymous;
  }

  function renderCLI() {
    const cmds = cliCommands(state.info, location.origin);
    $("cmd-install").textContent = cmds.install;
    $("cmd-install-go").textContent = cmds.installGo;
    $("cmd-login").textContent = cmds.login;
    $("login-note").textContent = cmds.loginNote;
    $("cmd-deploy").textContent = cmds.deploy;
    $("cmd-skill").textContent = cmds.skill;
    $("cmd-mcp").textContent = cmds.mcp;
  }

  function addCopyButtons(root) {
    for (const box of root.querySelectorAll(".cmd")) {
      if (box.querySelector(".copy")) continue;
      const code = box.querySelector("code");
      const btn = el("button", { type: "button", class: "icon-btn copy", "aria-label": "Copy command", title: "Copy", onclick: () => copyText(code.textContent) }, icon(ICON_COPY));
      box.append(btn);
    }
  }

  // --- deploy token (servers without user logins, e.g. the Cloudflare edition)

  /** Whether the visitor is a guest (no company login). */
  function isGuest() {
    return !state.me || !!state.me.anonymous;
  }

  /** Whether deploying needs a deploy token (by server policy, or a deploy was rejected with 401). */
  function needsToken() {
    if (state.tokenRequired) return true;
    const auth = (state.info && state.info.auth) || {};
    if (auth.anonymous_deploy) return false;
    const mode = auth.mode || "none";
    return mode === "none" && isGuest();
  }

  /**
   * Whether to offer the token box: when a token is set or deploying needs
   * one, after a delete was rejected with 401, and for guests in general
   * (deleting a site always needs a real identity or a token).
   */
  function offerToken() {
    return !!state.token || needsToken() || state.tokenWanted || (isGuest() && !!state.info);
  }

  function remembering() {
    return state.remember === null ? rememberByDefault(state.info) : state.remember;
  }

  /** Persists (or forgets) the deploy token according to the "remember" choice. */
  function saveToken() {
    store(TOKEN_KEY, remembering() ? state.token : "");
  }

  function renderTokenBox() {
    const box = $("token-box");
    box.hidden = !offerToken();
    $("token-hint").textContent = needsToken()
      ? "This server needs a deploy token for publishing (ask the admin, e.g. the output of the setup script)."
      : isGuest()
        ? "Deploying needs no token on this server; a deploy token is only needed to delete sites."
        : "Optional: deploys and deletes from this page use the token instead of your sign-in.";
    if ($("deploy-token").value !== state.token) $("deploy-token").value = state.token;
    $("remember-token").checked = remembering();
    $("remember-hint").hidden = !(state.info && state.info.url_mode === "path");
    if (!box.hidden && !state.token && needsToken()) box.open = true;
  }

  // --- site name & selection

  function existingSite(name) {
    return (state.sites || []).find((s) => s.name === name) || null;
  }

  function renderName() {
    const input = $("site-name");
    const name = input.value.trim();
    const err = name ? validateSiteName(name) : "";
    input.setAttribute("aria-invalid", err ? "true" : "false");
    $("site-url").textContent = name && !err ? siteURLFor(state.info, name, location.origin) : "";
    const hint = $("site-hint");
    hint.classList.toggle("error", !!err);
    if (err) hint.textContent = err;
    else if (!name) hint.textContent = "Lowercase letters, digits and dashes.";
    else {
      const ex = existingSite(name);
      hint.textContent = ex
        ? `Updates the existing site${ex.updated_by ? " (last deployed by " + ex.updated_by + ", " + relativeTime(ex.updated_at) + ")" : ""}.`
        : "A new site will be created.";
    }
    updateDeployButton();
  }

  function updateDeployButton() {
    const name = $("site-name").value.trim();
    const ok = !state.busy && !!state.selection && state.selection.files.length > 0 && !state.selection.blocking && !validateSiteName(name);
    $("deploy-btn").disabled = !ok;
    const note = $("deploy-note");
    if (state.busy) note.textContent = "";
    else if (!state.selection) note.textContent = "Choose a folder to deploy.";
    else if (validateSiteName(name)) note.textContent = "Enter a site name.";
    else note.textContent = "";
  }

  function showError(msg) {
    const box = $("deploy-error");
    box.textContent = msg || "";
    box.hidden = !msg;
  }

  function limitProblems(files) {
    const limits = (state.info && state.info.limits) || {};
    const total = files.reduce((n, f) => n + f.file.size, 0);
    const out = [];
    if (limits.max_files && files.length > limits.max_files) out.push(`${files.length} files exceed the limit of ${limits.max_files}.`);
    if (limits.max_site_bytes && total > limits.max_site_bytes) out.push(`The site is ${formatBytes(total)}; the limit is ${formatBytes(limits.max_site_bytes)}.`);
    if (limits.max_file_bytes) {
      const big = files.filter((f) => f.file.size > limits.max_file_bytes);
      if (big.length) out.push(`${big.length === 1 ? big[0].path + " is" : big.length + " files are"} larger than ${formatBytes(limits.max_file_bytes)}.`);
    }
    return out;
  }

  let selectSeq = 0;

  async function selectTree(root, rootName) {
    if (state.busy) return;
    const seq = ++selectSeq; // a newer selection supersedes one still being read
    showError("");
    $("result").hidden = true;
    $("selection").hidden = false;
    $("selection-summary").textContent = "Reading files…";
    $("selection-files").hidden = true;
    $("selection-ignored").hidden = true;
    state.selection = null;
    updateDeployButton();
    let res;
    try {
      res = await collectTree(root);
    } catch (err) {
      if (seq !== selectSeq) return;
      $("selection").hidden = true;
      showError((err && err.message) || "Could not read the folder.");
      return;
    }
    if (seq !== selectSeq || state.busy) return;
    const nameInput = $("site-name");
    if (!state.nameTouched || !nameInput.value.trim()) {
      const fromConfig = typeof res.config.site === "string" && !validateSiteName(res.config.site) ? res.config.site : "";
      const suggestion = fromConfig || suggestSiteName(rootName);
      if (suggestion) nameInput.value = suggestion;
    }
    const problems = [];
    if (res.invalid.length) problems.push(...res.invalid.slice(0, 5).map((f) => `Cannot deploy ${f.path}: ${f.reason}.`));
    problems.push(...limitProblems(res.files));
    if (!res.files.length) problems.push("No files to deploy (everything is ignored or the folder is empty).");
    state.selection = Object.assign(res, { blocking: problems.length > 0, rootName });

    const total = res.files.reduce((n, f) => n + f.file.size, 0);
    const parts = [`${res.files.length} file${res.files.length === 1 ? "" : "s"}`, formatBytes(total)];
    if (rootName) parts.unshift(rootName + (res.dir ? "/" + res.dir : ""));
    $("selection-summary").textContent = parts.join(" · ");
    const list = $("file-list");
    list.textContent = "";
    for (const f of res.files.slice(0, 500)) list.append(el("li", { title: f.path, text: f.path + "  ·  " + formatBytes(f.file.size) }));
    if (res.files.length > 500) list.append(el("li", { text: `… and ${res.files.length - 500} more` }));
    $("selection-files").hidden = res.files.length === 0;
    const ign = $("ignored-list");
    ign.textContent = "";
    for (const p of res.ignored.slice(0, 200)) ign.append(el("li", { text: p }));
    $("ignored-summary").textContent = `Ignored (${res.ignored.length})`;
    $("selection-ignored").hidden = res.ignored.length === 0;

    const warnings = res.warnings.slice();
    if (res.files.length && !res.files.some((f) => f.path === "index.html")) warnings.push("There is no index.html at the top level, so the site root will show a 404 page.");
    showError(problems.concat(warnings).join(" "));
    renderName();
  }

  function selectFileList(fileList, fromDirectoryPicker) {
    const files = Array.from(fileList || []);
    if (!files.length) return;
    let rootName = "";
    let items = files.map((f) => ({ path: f.webkitRelativePath || f.name, file: f }));
    if (fromDirectoryPicker && items[0].path.indexOf("/") > 0) {
      rootName = items[0].path.split("/")[0];
      items = items.map((it) => ({ path: it.path.slice(rootName.length + 1), file: it.file }));
    }
    selectTree(treeFromPaths(items), rootName);
  }

  function onDrop(ev) {
    ev.preventDefault();
    setDragging(false);
    if (state.busy) return;
    const dt = ev.dataTransfer;
    if (!dt) return;
    // Entries must be obtained synchronously inside the drop event.
    const entries = [];
    for (const item of Array.from(dt.items || [])) {
      if (item.kind !== "file") continue;
      const entry = typeof item.webkitGetAsEntry === "function" ? item.webkitGetAsEntry() : null;
      if (entry) entries.push(entry);
    }
    if (entries.length) {
      if (entries.length === 1 && entries[0].isDirectory) selectTree(entries[0], entries[0].name);
      else selectTree(virtualDir("", entries), "");
    } else if (dt.files && dt.files.length) {
      selectFileList(dt.files, false);
    }
  }

  let dragDepth = 0;
  function setDragging(on) {
    $("dropzone").classList.toggle("dragging", on);
  }
  function isFileDrag(ev) {
    return !!ev.dataTransfer && Array.from(ev.dataTransfer.types || []).indexOf("Files") >= 0;
  }

  // --- deploying

  function setProgress(pct, text) {
    $("progress").hidden = false;
    const v = Math.max(0, Math.min(100, Math.round(pct)));
    $("progress-fill").style.width = v + "%";
    $("progress-bar").setAttribute("aria-valuenow", String(v));
    $("progress-text").textContent = text;
  }

  /** PUTs deploy uploads with exactly the given headers (see createUploader). */
  const xhrUpload = createUploader({ XMLHttpRequest, origin: location.origin, stallMs: 120000 });

  async function onDeploy(ev) {
    ev.preventDefault();
    const name = $("site-name").value.trim();
    const err = validateSiteName(name);
    if (err) {
      renderName();
      $("site-name").focus();
      return;
    }
    if (!state.selection || state.busy) return;
    state.busy = true;
    updateDeployButton();
    showError("");
    $("result").hidden = true;
    const files = state.selection.files;
    try {
      setProgress(0, "Preparing files…");
      const hashed = await hashFiles(files, { onProgress: (d, n) => setProgress((d / n) * 10, `Preparing files… ${d} of ${n}`) });
      const res = await deploySite({
        gateway: GATEWAY,
        site: name,
        files: hashed,
        token: state.token,
        message: "Deployed from the hub" + (state.selection.rootName ? " (" + state.selection.rootName + ")" : ""),
        upload: xhrUpload,
        onProgress: (p) => {
          if (p.phase === "plan") setProgress(10, "Comparing with the live site…");
          else if (p.phase === "upload") {
            const pct = p.total ? 10 + (p.loaded / p.total) * 85 : 95;
            setProgress(pct, p.fileCount ? `Uploading ${p.files} of ${p.fileCount} changed files · ${formatBytes(p.loaded)} of ${formatBytes(p.total)}` : "Nothing changed; finishing…");
          } else if (p.phase === "commit") setProgress(97, "Publishing…");
          else if (p.phase === "done") setProgress(100, "Done");
        },
      });
      const url = safeHref(res.url || siteURLFor(state.info, name, location.origin));
      const summary = [`${res.uploaded} uploaded`, `${res.unchanged} unchanged`];
      if (res.deleted) summary.push(`${res.deleted} removed`);
      setProgress(100, "Deployed · " + summary.join(" · "));
      $("result-title").textContent = `Deployed ${name}!`;
      for (const a of [$("result-link"), $("result-open")]) a.href = url;
      $("result-link").textContent = url;
      $("result").hidden = false;
      $("result").focus();
      state.fresh = name;
      state.tokenRequired = false;
      toast("Deployed " + name);
      loadSites();
    } catch (e) {
      $("progress").hidden = true;
      if (e.status === 401 && isSafeRedirect(e.loginUrl)) {
        location.href = e.loginUrl;
        return;
      }
      if (e.status === 401) {
        state.tokenRequired = true;
        renderTokenBox();
        $("token-box").open = true;
        showError(state.token ? "The deploy token was rejected. Check it and try again." : "This server requires a deploy token. Paste it below and deploy again.");
        $("deploy-token").focus();
      } else if (e.status === 403) {
        showError(e.message || "You are not allowed to deploy this site.");
      } else {
        showError(e.message || "The deploy failed.");
      }
    } finally {
      state.busy = false;
      updateDeployButton();
    }
  }

  // --- site directory

  /**
   * Whether to offer deleting site (the gateway decides): with a deploy token,
   * as an admin, or as a signed-in owner (any signed-in user when the site has
   * no owners). Guests can never delete sites without a token.
   */
  function canDelete(site) {
    if (state.token) return true;
    const me = state.me;
    if (!me || me.anonymous) return false;
    if (me.admin) return true;
    const owners = site.owners || [];
    return owners.length === 0 || owners.indexOf(me.id) >= 0 || (!!me.email && owners.indexOf(me.email) >= 0);
  }

  function siteItem(s) {
    const url = safeHref(s.url || siteURLFor(state.info, s.name, location.origin));
    const title = s.title || s.name;
    const time = s.updated_at || s.created_at;
    const actions = el(
      "div",
      { class: "site-actions" },
      el("button", { type: "button", class: "icon-btn", "aria-label": `Copy link to ${s.name}`, title: "Copy link", onclick: () => copyText(url) }, icon(ICON_COPY)),
      canDelete(s) ? el("button", { type: "button", class: "icon-btn danger", "aria-label": `Delete ${s.name}`, title: "Delete", onclick: () => openDelete(s.name) }, icon(ICON_TRASH)) : null,
    );
    const meta = el(
      "div",
      { class: "site-meta" },
      s.updated_by ? el("span", { class: "who", title: s.updated_by, text: s.updated_by }) : null,
      s.updated_by && time ? el("span", { "aria-hidden": "true", text: "·" }) : null,
      time ? el("time", { datetime: time, title: new Date(time).toLocaleString(), text: relativeTime(time) }) : null,
      actions,
    );
    return el(
      "li",
      { class: "site" + (state.fresh === s.name ? " fresh" : ""), "data-site": s.name },
      el("h3", { class: "site-title" }, el("a", { href: url, text: title })),
      el("span", { class: "site-name", text: s.name }),
      url !== "#" ? el("span", { class: "site-url", title: url, text: url.replace(/^https?:\/\//i, "").replace(/\/$/, "") }) : null,
      s.description ? el("p", { class: "site-desc", text: s.description }) : null,
      meta,
    );
  }

  function renderSites() {
    const list = $("site-list");
    const empty = $("sites-empty");
    if (state.sites === null) return;
    list.classList.remove("skeleton");
    list.removeAttribute("aria-busy");
    list.classList.toggle("as-list", state.view === "list");
    const shown = filterSites(state.sites, state.query);
    list.textContent = "";
    for (const s of shown) list.append(siteItem(s));
    const total = state.sites.length;
    $("site-count").textContent = total ? (shown.length === total ? `(${total})` : `(${shown.length} of ${total})`) : "";
    $("search-status").textContent = state.query ? `${shown.length} site${shown.length === 1 ? "" : "s"} found` : "";
    empty.hidden = shown.length > 0;
    if (!shown.length) empty.textContent = total ? `No sites match “${state.query}”.` : "No sites yet. Drop a folder above to create the first one.";
    list.hidden = shown.length === 0;
  }

  async function loadSites() {
    try {
      const res = await getJSON(API + "/sites");
      state.sites = Array.isArray(res && res.sites) ? res.sites : [];
    } catch (err) {
      state.sites = state.sites || [];
      if (!(err.status === 401 && err.loginUrl)) banner("Could not load the site list: " + (err.message || err));
    }
    renderSites();
    renderName();
  }

  function setView(view) {
    state.view = view === "list" ? "list" : "grid";
    store(VIEW_KEY, state.view);
    $("view-grid").setAttribute("aria-pressed", String(state.view === "grid"));
    $("view-list").setAttribute("aria-pressed", String(state.view === "list"));
    renderSites();
  }

  function refreshTimes() {
    for (const t of document.querySelectorAll("time[datetime]")) t.textContent = relativeTime(t.getAttribute("datetime"));
  }

  function banner(msg) {
    const b = $("banner");
    b.textContent = msg;
    b.hidden = !msg;
  }

  // --- delete dialog

  function openDelete(name) {
    state.deleting = name;
    const dlg = $("delete-dialog");
    $("delete-name").textContent = name;
    $("delete-confirm").value = "";
    $("delete-error").textContent = "";
    $("delete-submit").disabled = true;
    if (typeof dlg.showModal === "function") dlg.showModal();
    else dlg.setAttribute("open", "");
    $("delete-confirm").focus();
  }

  function closeDelete() {
    const dlg = $("delete-dialog");
    if (typeof dlg.close === "function") dlg.close();
    else dlg.removeAttribute("open");
  }

  async function onDeleteSubmit(ev) {
    ev.preventDefault();
    const name = state.deleting;
    if (!name || $("delete-confirm").value.trim() !== name) return;
    $("delete-submit").disabled = true;
    try {
      await deleteSite({ gateway: GATEWAY, site: name, token: state.token });
      closeDelete();
      toast("Deleted " + name);
      state.sites = (state.sites || []).filter((s) => s.name !== name);
      renderSites();
      loadSites();
    } catch (e) {
      if (e.status === 401 && isSafeRedirect(e.loginUrl)) {
        location.href = e.loginUrl;
        return;
      }
      if (e.status === 401) {
        // Offer the token box (behind the dialog) for the next attempt.
        state.tokenWanted = true;
        renderTokenBox();
        $("token-box").open = true;
        $("delete-error").textContent = state.token
          ? "The deploy token was rejected. Check it under “Deploy token” and try again."
          : "Deleting sites needs a deploy token: cancel, paste it under “Deploy token”, then try again.";
      } else {
        $("delete-error").textContent = e.message || "Deleting failed.";
      }
      $("delete-submit").disabled = false;
    }
  }

  // --- a deploy token handed over in a link (#token=…)

  /** The token from the link while its confirmation dialog is open. */
  let handoff = "";

  /**
   * Asks before a token from a link is used: a link (anyone can send one)
   * must not silently switch whom deploys and deletes run as. check is the
   * settled tokenOwner() result. A rejected token is never offered.
   */
  function offerHandedToken(token, check) {
    const err = check.ok ? null : check.err || {};
    if (err && (err.status === 401 || err.status === 403)) {
      banner("The deploy token in the link you opened was rejected by the server, so it is not used.");
      return;
    }
    const prompt = handedTokenPrompt(check.ok ? check.user : null, state.me, err ? String(err.message || "unknown error") : "");
    $("handoff-title").textContent = prompt.title;
    $("handoff-body").textContent = prompt.body;
    handoff = token;
    const dlg = $("handoff-dialog");
    if (typeof dlg.showModal === "function") dlg.showModal();
    else dlg.setAttribute("open", "");
    $("handoff-ignore").focus(); // not using it is the default
  }

  /** Closes the token dialog; only an explicit "use" adopts (and may persist) the token. */
  function closeHandoff(use) {
    const token = handoff;
    handoff = "";
    const dlg = $("handoff-dialog");
    if (dlg.open) {
      if (typeof dlg.close === "function") dlg.close();
      else dlg.removeAttribute("open");
    }
    if (!token) return;
    if (!use) {
      toast("The deploy token from the link was not used");
      return;
    }
    state.token = token;
    saveToken();
    renderTokenBox();
    renderSites(); // delete buttons depend on the token
    toast(remembering() ? "Deploy token saved in this browser" : "Deploy token set for this visit");
  }

  // --- wiring

  function wire() {
    addCopyButtons(document);
    const nameInput = $("site-name");
    nameInput.addEventListener("input", () => {
      const pos = nameInput.selectionStart;
      const lowered = nameInput.value.toLowerCase().replace(/\s+/g, "-");
      if (lowered !== nameInput.value) {
        nameInput.value = lowered;
        try {
          nameInput.setSelectionRange(pos, pos);
        } catch (_) {
          /* ignore */
        }
      }
      state.nameTouched = true;
      renderName();
    });
    $("deploy-form").addEventListener("submit", onDeploy);
    $("pick-folder").addEventListener("click", () => $("folder-input").click());
    $("pick-files").addEventListener("click", () => $("files-input").click());
    $("folder-input").addEventListener("change", (e) => {
      selectFileList(e.target.files, true);
      e.target.value = "";
    });
    $("files-input").addEventListener("change", (e) => {
      selectFileList(e.target.files, false);
      e.target.value = "";
    });
    $("clear-selection").addEventListener("click", () => {
      if (state.busy) return;
      selectSeq++;
      state.selection = null;
      $("selection").hidden = true;
      showError("");
      updateDeployButton();
    });

    document.addEventListener("dragenter", (e) => {
      if (!isFileDrag(e)) return;
      e.preventDefault();
      dragDepth++;
      setDragging(true);
    });
    document.addEventListener("dragover", (e) => {
      if (!isFileDrag(e)) return;
      e.preventDefault();
      e.dataTransfer.dropEffect = state.busy ? "none" : "copy";
    });
    document.addEventListener("dragleave", () => {
      dragDepth = Math.max(0, dragDepth - 1);
      if (!dragDepth) setDragging(false);
    });
    document.addEventListener("drop", (e) => {
      dragDepth = 0;
      if (isFileDrag(e)) onDrop(e);
    });

    const tokenInput = $("deploy-token");
    tokenInput.addEventListener("input", () => {
      const had = !!state.token;
      state.token = tokenInput.value.trim();
      saveToken();
      if (had !== !!state.token) renderSites(); // delete buttons depend on the token
    });
    $("remember-token").addEventListener("change", (e) => {
      state.remember = e.target.checked;
      store(REMEMBER_KEY, state.remember ? "1" : "0");
      saveToken();
    });
    $("forget-token").addEventListener("click", () => {
      state.token = "";
      store(TOKEN_KEY, "");
      tokenInput.value = "";
      renderTokenBox();
      renderSites();
      toast("Token removed from this browser");
    });

    $("result-copy").addEventListener("click", () => copyText($("result-link").href));

    const search = $("search");
    search.addEventListener("input", () => {
      state.query = search.value;
      renderSites();
    });
    search.addEventListener("keydown", (e) => {
      if (e.key === "Escape") {
        search.value = "";
        state.query = "";
        renderSites();
      }
    });
    document.addEventListener("keydown", (e) => {
      const t = e.target;
      const typing = t && (t.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName));
      if (e.key === "/" && !typing && !e.metaKey && !e.ctrlKey && !e.altKey) {
        e.preventDefault();
        search.focus();
      }
    });
    $("view-grid").addEventListener("click", () => setView("grid"));
    $("view-list").addEventListener("click", () => setView("list"));

    $("delete-confirm").addEventListener("input", (e) => {
      $("delete-submit").disabled = e.target.value.trim() !== state.deleting;
    });
    $("delete-form").addEventListener("submit", onDeleteSubmit);
    $("delete-cancel").addEventListener("click", closeDelete);

    $("handoff-form").addEventListener("submit", (e) => {
      e.preventDefault();
      closeHandoff(true);
    });
    $("handoff-ignore").addEventListener("click", () => closeHandoff(false));
    $("handoff-dialog").addEventListener("close", () => closeHandoff(false)); // Escape
  }

  async function boot() {
    // A deploy token can be handed over in the URL fragment (never sent to the
    // server). Anyone can send such a link, so the token is used (and
    // persisted, see saveToken) only once the visitor confirms whose it is.
    const handed = tokenFromHash(location.hash);
    if (handed) {
      try {
        history.replaceState(null, "", location.pathname + location.search);
      } catch (_) {
        /* ignore */
      }
    }
    const rememberChoice = load(REMEMBER_KEY);
    state.remember = rememberChoice === "1" ? true : rememberChoice === "0" ? false : null;
    state.token = load(TOKEN_KEY);
    state.view = load(VIEW_KEY) === "list" ? "list" : "grid";
    $("view-grid").setAttribute("aria-pressed", String(state.view === "grid"));
    $("view-list").setAttribute("aria-pressed", String(state.view === "list"));
    wire();
    renderCLI();
    updateDeployButton();
    // The token already in use needs no confirmation.
    const check =
      handed && handed !== state.token
        ? tokenOwner({ gateway: GATEWAY, token: handed }).then(
            (user) => ({ ok: true, user }),
            (err) => ({ ok: false, err }),
          )
        : null;
    const [info, me] = await Promise.allSettled([getJSON(API + "/info"), getJSON(API + "/me")]);
    if (info.status === "fulfilled") state.info = info.value;
    else banner("Server info is unavailable (" + ((info.reason && info.reason.message) || "error") + "). Deploys from the hub may not work.");
    if (me.status === "fulfilled") state.me = me.value;
    renderHeader();
    renderCLI();
    renderTokenBox();
    renderName();
    const sites = loadSites();
    if (check) offerHandedToken(handed, await check);
    await sites;
    setInterval(refreshTimes, 60000);
  }

  boot();
})();
