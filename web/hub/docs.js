/*
 * Backyard docs page: renders /docs/<name>.md with a small, safe markdown
 * renderer (no dependencies). All text is HTML-escaped; raw HTML in the
 * markdown is shown as text; only http(s), mailto and relative URLs survive.
 *
 * renderMarkdown is exposed as globalThis.BackyardDocs.renderMarkdown (unit
 * tested in Node); the UI boots only on a page with <body id="docs-page">.
 */
(function () {
  "use strict";

  // Private-use characters delimit placeholders for already-rendered inline
  // HTML; they are stripped from the input first, so text cannot forge one.
  const PH_OPEN = "\uE000";
  const PH_CLOSE = "\uE001";
  const PH_RE = /\uE000(\d+)\uE001/g;

  function esc(s) {
    return String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);
  }

  function decodeEntities(s) {
    return String(s).replace(/&(amp|lt|gt|quot|#39);/g, (m, e) => ({ amp: "&", lt: "<", gt: ">", quot: '"', "#39": "'" })[e]);
  }

  /** Returns a URL safe to put in href/src, or "" (javascript:, data:, vbscript: … are rejected). */
  function safeURL(url) {
    // eslint-disable-next-line no-control-regex
    const u = String(url).replace(/[\u0000- \u007f]/g, "");
    if (!u) return "";
    const scheme = /^([a-z][a-z0-9+.-]*):/i.exec(u);
    if (scheme && !/^(https?|mailto)$/i.test(scheme[1])) return "";
    return u;
  }

  /** GitHub-style heading slug. */
  function slugify(text) {
    const s = String(text)
      .toLowerCase()
      .trim()
      .replace(/[^\p{L}\p{N}\s_-]/gu, "")
      .replace(/\s+/g, "-")
      .replace(/-+/g, "-")
      .replace(/^-|-$/g, "");
    return s || "section";
  }

  // --- inline -----------------------------------------------------------------

  function formatText(s) {
    s = esc(s);
    s = s.replace(/\*\*(?=\S)([\s\S]*?\S)\*\*/g, "<strong>$1</strong>");
    s = s.replace(/(^|[^\p{L}\p{N}_])__(?=\S)([\s\S]*?\S)__(?![\p{L}\p{N}_])/gu, "$1<strong>$2</strong>");
    s = s.replace(/\*(?=[^\s*])([\s\S]*?[^\s*])\*/g, "<em>$1</em>");
    s = s.replace(/(^|[^\p{L}\p{N}_])_(?=[^\s_])([\s\S]*?[^\s_])_(?![\p{L}\p{N}_])/gu, "$1<em>$2</em>");
    s = s.replace(/~~(?=\S)([\s\S]*?\S)~~/g, "<del>$1</del>");
    s = s.replace(/(?: {2,}|\\)\n/g, "<br>\n");
    return s;
  }

  function linkHTML(url, innerHTML, ctx, title) {
    const safe = safeURL(ctx.rewriteLink(url));
    if (!safe) return innerHTML;
    const external = /^(https?:)?\/\//i.test(safe);
    return (
      '<a href="' +
      esc(safe) +
      '"' +
      (title ? ' title="' + esc(title) + '"' : "") +
      (external ? ' target="_blank" rel="noopener noreferrer"' : "") +
      ">" +
      innerHTML +
      "</a>"
    );
  }

  function inline(text, ctx) {
    // store[i] is the HTML a placeholder stands for, raw[i] its source text.
    const store = [];
    const raw = [];
    const ph = (html, source) => {
      raw.push(source === undefined ? "" : source);
      return PH_OPEN + (store.push(html) - 1) + PH_CLOSE;
    };
    // Text with placeholders replaced by their source: URLs, titles and alt
    // texts are validated and escaped on this, never on rendered HTML.
    const plain = (str) => String(str).replace(PH_RE, (m, n) => raw[+n]);
    let s = String(text).replace(/[\uE000\uE001]/g, "");

    // Backslash escapes and code spans, left to right (CommonMark): a
    // backslash escapes punctuation outside code; code span contents are literal.
    {
      let out = "";
      let k = 0;
      while (k < s.length) {
        const c = s[k];
        if (c === "\\" && k + 1 < s.length && /[!-/:-@[-`{-~]/.test(s[k + 1])) {
          out += ph(esc(s[k + 1]), s[k + 1]);
          k += 2;
          continue;
        }
        if (c === "`") {
          let n = 1;
          while (s[k + n] === "`") n++;
          const ticks = "`".repeat(n);
          let end = -1;
          for (let j = s.indexOf(ticks, k + n); j >= 0; j = s.indexOf(ticks, j + 1)) {
            let run = n;
            while (s[j + run] === "`") run++;
            if (s[j - 1] !== "`" && run === n) {
              end = j;
              break;
            }
            j += run - 1;
          }
          if (end < 0) {
            out += ticks;
            k += n;
            continue;
          }
          let code = s.slice(k + n, end).replace(/\n/g, " ");
          if (code.length > 2 && code[0] === " " && code[code.length - 1] === " " && code.trim()) code = code.slice(1, -1);
          out += ph("<code>" + esc(code) + "</code>", s.slice(k, end + n));
          k = end + n;
          continue;
        }
        out += c;
        k++;
      }
      s = out;
    }
    // Autolinks: <https://…>.
    s = s.replace(/<((?:https?|mailto):[^\s<>]+)>/gi, (m, url) => ph(linkHTML(plain(url), esc(plain(url)), ctx)));
    // Images.
    s = s.replace(/!\[([^\]]*)\]\(\s*<?([^\s)<>]+)>?(?:\s+(?:"([^"]*)"|'([^']*)'))?\s*\)/g, (m, alt, url, t1, t2) => {
      const safe = safeURL(plain(url));
      const altText = plain(alt);
      if (!safe) return ph(esc(altText));
      const title = plain(t1 || t2 || "");
      return ph('<img src="' + esc(safe) + '" alt="' + esc(altText) + '"' + (title ? ' title="' + esc(title) + '"' : "") + ' loading="lazy">');
    });
    // Links (label may contain one level of brackets; URL may contain balanced parentheses).
    s = s.replace(
      /\[((?:[^[\]]|\[[^[\]]*\])*)\]\(\s*<?((?:[^\s()<>]|\([^\s()<>]*\))*)>?(?:\s+(?:"([^"]*)"|'([^']*)'))?\s*\)/g,
      (m, label, url, t1, t2) => ph(linkHTML(plain(url), formatText(label), ctx, plain(t1 || t2 || ""))),
    );
    // Bare URLs (GFM autolink literals).
    s = s.replace(/(^|[\s(])((?:https?:\/\/|www\.)[^\s<>\uE000\uE001]*[^\s<>.,:;"')\]*_~?!\uE000\uE001])/g, (m, pre, url) => {
      const u = plain(url);
      return pre + ph(linkHTML(/^www\./i.test(u) ? "https://" + u : u, esc(u), ctx));
    });
    s = formatText(s);
    // Placeholders nest (a link label may hold code spans): resolve repeatedly.
    for (let k = 0; k < 8 && s.indexOf(PH_OPEN) >= 0; k++) s = s.replace(PH_RE, (m, n) => store[+n]);
    return s;
  }

  // --- blocks -----------------------------------------------------------------

  const isBlank = (l) => /^\s*$/.test(l);
  const FENCE_RE = /^( {0,3})(`{3,}|~{3,})[ \t]*([^`\s]*)[^`]*$/;
  const ATX_RE = /^ {0,3}(#{1,6})(?:[ \t]+(.*?))?(?:[ \t]+#+)?[ \t]*$/;
  const HR_RE = /^ {0,3}([-*_])(?:[ \t]*\1){2,}[ \t]*$/;
  const QUOTE_RE = /^ {0,3}>/;

  function expandLeadingTabs(line) {
    const m = /^[ \t]+/.exec(line);
    if (!m || m[0].indexOf("\t") < 0) return line;
    let col = 0;
    for (const c of m[0]) col = c === "\t" ? col + 4 - (col % 4) : col + 1;
    return " ".repeat(col) + line.slice(m[0].length);
  }

  function indentOf(line) {
    return /^ */.exec(line)[0].length;
  }

  function listMarker(line) {
    if (HR_RE.test(line)) return null;
    const m = /^( {0,3})([-*+]|\d{1,9}[.)])(?:([ \t]+)(.*)|$)$/.exec(line);
    if (!m) return null;
    const spaces = m[3] ? m[3].length : 1;
    const content = m[4] || "";
    const pad = spaces > 4 ? 1 : spaces;
    return {
      indent: m[1].length,
      marker: m[2],
      ordered: /\d/.test(m[2]),
      start: parseInt(m[2], 10),
      content: spaces > 4 ? " ".repeat(spaces - 1) + content : content,
      contentIndent: m[1].length + m[2].length + pad,
    };
  }

  function splitRow(line) {
    let s = line.trim();
    if (s.startsWith("|")) s = s.slice(1);
    if (s.endsWith("|") && !s.endsWith("\\|")) s = s.slice(0, -1);
    const cells = [];
    let cur = "";
    let code = 0;
    for (let k = 0; k < s.length; k++) {
      const c = s[k];
      if (c === "\\" && s[k + 1] === "|") {
        cur += code ? "|" : "\\|"; // GFM: an escaped pipe is a literal pipe, even inside code
        k++;
        continue;
      }
      if (c === "`") {
        let n = 1;
        while (s[k + n] === "`") n++;
        if (!code) code = n;
        else if (code === n) code = 0;
        cur += s.slice(k, k + n);
        k += n - 1;
        continue;
      }
      if (c === "|" && !code) {
        cells.push(cur.trim());
        cur = "";
        continue;
      }
      cur += c;
    }
    cells.push(cur.trim());
    return cells;
  }

  function isDelimiterRow(line) {
    if (!line || line.indexOf("-") < 0 || line.indexOf("|") < 0 && !/^\s*:?-+:?\s*$/.test(line)) return false;
    const cells = splitRow(line);
    return cells.length > 0 && cells.every((c) => /^:?-+:?$/.test(c));
  }

  function isTableStart(lines, i) {
    if (i + 1 >= lines.length || lines[i].indexOf("|") < 0 || !isDelimiterRow(lines[i + 1])) return false;
    return splitRow(lines[i]).length === splitRow(lines[i + 1]).length;
  }

  /** Whether line starts a block that can interrupt a paragraph. */
  function interrupts(lines, i) {
    const l = lines[i];
    if (FENCE_RE.test(l) || ATX_RE.test(l) || HR_RE.test(l) || QUOTE_RE.test(l) || /^\s*<!--/.test(l)) return true;
    const mk = listMarker(l);
    if (mk && mk.content.trim() && (!mk.ordered || mk.start === 1)) return true;
    return isTableStart(lines, i);
  }

  function heading(level, text, ctx) {
    const inner = inline(text, ctx);
    const plain = decodeEntities(inner.replace(/<[^>]*>/g, "")).trim();
    let id = slugify(plain);
    const n = ctx.slugs.get(id) || 0;
    ctx.slugs.set(id, n + 1);
    if (n) id += "-" + n;
    ctx.headings.push({ level, text: plain, id });
    const anchor = level >= 2 && ctx.anchors ? ' <a class="anchor" href="#' + id + '" aria-label="Link to this section">#</a>' : "";
    return "<h" + level + ' id="' + id + '">' + inner + anchor + "</h" + level + ">\n";
  }

  function table(lines, i, ctx) {
    const head = splitRow(lines[i]);
    const aligns = splitRow(lines[i + 1]).map((c) => (c.startsWith(":") && c.endsWith(":") ? "center" : c.endsWith(":") ? "right" : c.startsWith(":") ? "left" : ""));
    const cell = (tag, text, k) => "<" + tag + (aligns[k] && aligns[k] !== "left" ? ' class="align-' + aligns[k] + '"' : "") + ">" + inline(text || "", ctx) + "</" + tag + ">";
    let html = '<div class="table-wrap"><table>\n<thead><tr>' + head.map((c, k) => cell("th", c, k)).join("") + "</tr></thead>\n<tbody>\n";
    i += 2;
    while (i < lines.length && !isBlank(lines[i]) && lines[i].indexOf("|") >= 0 && !FENCE_RE.test(lines[i]) && !QUOTE_RE.test(lines[i])) {
      const cells = splitRow(lines[i]);
      html += "<tr>" + head.map((_, k) => cell("td", cells[k], k)).join("") + "</tr>\n";
      i++;
    }
    return [html + "</tbody></table></div>\n", i];
  }

  function list(lines, i, ctx) {
    const first = listMarker(lines[i]);
    const kind = first.ordered ? first.marker.slice(-1) : first.marker;
    const items = [];
    let loose = false;
    while (i < lines.length) {
      const mk = listMarker(lines[i]);
      if (!mk || mk.ordered !== first.ordered || (mk.ordered ? mk.marker.slice(-1) : mk.marker) !== kind || mk.indent >= first.contentIndent) break;
      const body = [mk.content];
      i++;
      let blank = false;
      while (i < lines.length) {
        const l = lines[i];
        if (isBlank(l)) {
          body.push("");
          blank = true;
          i++;
          continue;
        }
        if (indentOf(l) >= mk.contentIndent) {
          body.push(l.slice(mk.contentIndent));
          blank = false;
          i++;
          continue;
        }
        if (blank || listMarker(l) || interrupts(lines, i)) break;
        body.push(l.trim()); // lazy paragraph continuation
        i++;
      }
      let trailing = false;
      while (body.length && body[body.length - 1] === "") {
        body.pop();
        trailing = true;
      }
      // A blank line between two blocks inside an item makes the list loose.
      for (let k = 1; k < body.length - 1; k++) if (body[k] === "" && body[k + 1] !== "" && !/^\s/.test(body[k + 1])) loose = true;
      items.push(body);
      if (trailing) {
        const next = i < lines.length ? listMarker(lines[i]) : null;
        if (next && next.ordered === first.ordered && next.indent < first.contentIndent) loose = true;
      }
    }
    const tag = first.ordered ? "ol" : "ul";
    let html = "<" + tag + (first.ordered && first.start !== 1 ? ' start="' + first.start + '"' : "") + ">\n";
    for (const body of items) {
      let cls = "";
      let prefix = "";
      const task = /^\[([ xX])\][ \t]+/.exec(body[0] || "");
      if (task) {
        cls = ' class="task"';
        prefix = '<input type="checkbox" disabled' + (task[1] !== " " ? " checked" : "") + "> ";
        body[0] = body[0].slice(task[0].length);
      }
      const inner = blocks(body, ctx, !loose).replace(/\n+$/, "");
      html += "<li" + cls + ">" + prefix + inner + "</li>\n";
    }
    return [html + "</" + tag + ">\n", i];
  }

  function codeBlock(code, lang) {
    const l = String(lang || "").replace(/[^A-Za-z0-9_+-]/g, "");
    return '<div class="code-block"><pre><code' + (l ? ' class="language-' + l + '" data-lang="' + l + '"' : "") + ">" + esc(code) + "</code></pre></div>\n";
  }

  function blocks(lines, ctx, tight) {
    let out = "";
    let i = 0;
    while (i < lines.length) {
      const line = lines[i];
      if (isBlank(line)) {
        i++;
        continue;
      }
      if (/^\s*<!--/.test(line)) {
        // HTML comments are dropped.
        while (i < lines.length && lines[i].indexOf("-->") < 0) i++;
        i++;
        continue;
      }
      let m = FENCE_RE.exec(line);
      if (m) {
        const indent = m[1].length;
        const fence = m[2];
        const close = new RegExp("^ {0,3}" + (fence[0] === "`" ? "`" : "~") + "{" + fence.length + ",}[ \\t]*$");
        const body = [];
        i++;
        while (i < lines.length && !close.test(lines[i])) {
          body.push(lines[i].replace(new RegExp("^ {0," + indent + "}"), ""));
          i++;
        }
        i++; // closing fence (or end of input)
        out += codeBlock(body.join("\n"), m[3]);
        continue;
      }
      m = ATX_RE.exec(line);
      if (m) {
        out += heading(m[1].length, m[2] || "", ctx);
        i++;
        continue;
      }
      if (HR_RE.test(line)) {
        out += "<hr>\n";
        i++;
        continue;
      }
      if (QUOTE_RE.test(line)) {
        const inner = [];
        while (i < lines.length && (QUOTE_RE.test(lines[i]) || (!isBlank(lines[i]) && inner.length && !isBlank(inner[inner.length - 1]) && !interrupts(lines, i)))) {
          inner.push(lines[i].replace(/^ {0,3}> ?/, ""));
          i++;
        }
        out += "<blockquote>\n" + blocks(inner, ctx, false) + "</blockquote>\n";
        continue;
      }
      if (listMarker(line)) {
        const [html, next] = list(lines, i, ctx);
        out += html;
        i = next;
        continue;
      }
      if (isTableStart(lines, i)) {
        const [html, next] = table(lines, i, ctx);
        out += html;
        i = next;
        continue;
      }
      // Paragraph, or a setext heading.
      const para = [line.replace(/^\s+/, "")];
      i++;
      let setext = 0;
      while (i < lines.length && !isBlank(lines[i])) {
        if (/^ {0,3}=+[ \t]*$/.test(lines[i])) {
          setext = 1;
          i++;
          break;
        }
        if (/^ {0,3}-+[ \t]*$/.test(lines[i])) {
          setext = 2;
          i++;
          break;
        }
        if (interrupts(lines, i)) break;
        para.push(lines[i].replace(/^\s+/, ""));
        i++;
      }
      const text = para.join("\n").replace(/\s+$/, "");
      if (setext) out += heading(setext, text, ctx);
      else if (tight) out += inline(text, ctx) + "\n";
      else out += "<p>" + inline(text, ctx) + "</p>\n";
    }
    return out;
  }

  /**
   * Renders markdown to safe HTML.
   * opts.rewriteLink(url) → url lets the caller map links (e.g. "api.md" → "/docs?doc=api").
   * Returns { html, headings: [{level, text, id}], title }.
   */
  function renderMarkdown(src, opts) {
    opts = opts || {};
    const ctx = { slugs: new Map(), headings: [], rewriteLink: opts.rewriteLink || ((u) => u), anchors: opts.anchors !== false };
    const lines = String(src == null ? "" : src)
      .replace(/^﻿/, "")
      .replace(/\r\n?/g, "\n")
      .split("\n")
      .map(expandLeadingTabs);
    // YAML front matter (e.g. SKILL.md) is not rendered.
    if (lines[0] === "---") {
      const end = lines.indexOf("---", 1);
      if (end > 0) lines.splice(0, end + 1);
    }
    const html = blocks(lines, ctx, false);
    const h1 = ctx.headings.find((h) => h.level === 1);
    return { html, headings: ctx.headings, title: h1 ? h1.text : "" };
  }

  /** Maps links between the skill documents to the docs page. */
  function rewriteDocLink(url) {
    const m = /^(?:\.{1,2}\/)*(?:references\/)?([A-Za-z0-9_-]+)\.md(#[^\s]*)?$/i.exec(String(url));
    if (!m) return url;
    const name = m[1].toLowerCase() === "skill" ? "skill" : m[1];
    return "/docs?doc=" + encodeURIComponent(name) + (m[2] || "");
  }

  globalThis.BackyardDocs = { renderMarkdown, slugify, safeURL, rewriteDocLink };

  if (typeof document === "undefined" || !document.body || document.body.id !== "docs-page") return;

  // ===========================================================================
  // UI

  const DOC_RE = /^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$/;
  const TITLES = { api: "API reference", recipes: "Recipes", deploy: "Deploying", skill: "Agent skill" };
  const main = document.getElementById("doc");
  const toc = document.getElementById("toc");
  let serverName = "Backyard";
  let observer = null;
  let loadSeq = 0;

  function currentDoc() {
    const d = new URLSearchParams(location.search).get("doc") || "api";
    return DOC_RE.test(d) ? d : "api";
  }

  function markNav(doc) {
    for (const a of document.querySelectorAll("#doc-list a")) {
      if (a.dataset.doc === doc) a.setAttribute("aria-current", "page");
      else a.removeAttribute("aria-current");
    }
  }

  function enhanceCode(root) {
    for (const block of root.querySelectorAll(".code-block")) {
      const code = block.querySelector("code");
      const lang = code && code.dataset.lang;
      if (lang) {
        const label = document.createElement("span");
        label.className = "code-lang";
        label.textContent = lang;
        block.append(label);
      }
      const btn = document.createElement("button");
      btn.type = "button";
      btn.className = "btn small copy";
      btn.textContent = "Copy";
      btn.setAttribute("aria-label", "Copy code");
      btn.addEventListener("click", async () => {
        try {
          await navigator.clipboard.writeText(code.textContent);
          btn.textContent = "Copied";
        } catch (_) {
          btn.textContent = "Press Ctrl+C";
          const range = document.createRange();
          range.selectNodeContents(code);
          const sel = getSelection();
          sel.removeAllRanges();
          sel.addRange(range);
        }
        setTimeout(() => (btn.textContent = "Copy"), 1600);
      });
      block.append(btn);
    }
  }

  function buildTOC(headings) {
    toc.textContent = "";
    const items = headings.filter((h) => h.level === 2 || h.level === 3);
    document.getElementById("toc-title").hidden = items.length === 0;
    for (const h of items) {
      const li = document.createElement("li");
      li.className = "lvl-" + h.level;
      const a = document.createElement("a");
      a.href = "#" + h.id;
      a.textContent = h.text;
      li.append(a);
      toc.append(li);
    }
    if (observer) observer.disconnect();
    if (!("IntersectionObserver" in window) || !items.length) return;
    const links = new Map(Array.from(toc.querySelectorAll("a")).map((a) => [a.getAttribute("href").slice(1), a]));
    observer = new IntersectionObserver(
      (entries) => {
        for (const e of entries) {
          if (!e.isIntersecting) continue;
          for (const a of links.values()) a.removeAttribute("aria-current");
          const a = links.get(e.target.id);
          if (a) a.setAttribute("aria-current", "location");
        }
      },
      { rootMargin: "-70px 0px -70% 0px" },
    );
    for (const id of links.keys()) {
      const target = document.getElementById(id);
      if (target) observer.observe(target);
    }
  }

  function scrollToHash() {
    if (!location.hash) return false;
    let id = location.hash.slice(1);
    try {
      id = decodeURIComponent(id);
    } catch (_) {
      /* keep */
    }
    const target = document.getElementById(id);
    if (!target) return false;
    target.scrollIntoView();
    return true;
  }

  function status(text) {
    main.textContent = "";
    const p = document.createElement("p");
    p.className = "docs-status";
    p.textContent = text;
    main.append(p);
  }

  async function show(doc, fromNavigation) {
    const seq = ++loadSeq;
    markNav(doc);
    status("Loading…");
    let text;
    try {
      const res = await fetch("/docs/" + encodeURIComponent(doc) + ".md", { credentials: "same-origin", headers: { Accept: "text/markdown, text/plain" } });
      if (res.status === 404) throw new Error("This document does not exist.");
      if (!res.ok) throw new Error("Could not load the document (HTTP " + res.status + ").");
      text = await res.text();
    } catch (err) {
      if (seq === loadSeq) status(err.message || "Could not load the document.");
      return;
    }
    if (seq !== loadSeq) return;
    const out = renderMarkdown(text, { rewriteLink: rewriteDocLink });
    main.innerHTML = out.html; // safe: the renderer escapes all text and filters URLs
    enhanceCode(main);
    buildTOC(out.headings);
    document.title = (out.title || TITLES[doc] || doc) + " · " + serverName;
    if (!scrollToHash() && fromNavigation) {
      window.scrollTo(0, 0);
      main.focus({ preventScroll: true });
    }
  }

  document.addEventListener("click", (e) => {
    const a = e.target.closest && e.target.closest("a[href]");
    if (!a || e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || a.target === "_blank") return;
    const url = new URL(a.href, location.href);
    if (url.origin !== location.origin || url.pathname !== "/docs") return;
    const doc = url.searchParams.get("doc") || "api";
    if (!DOC_RE.test(doc)) return;
    const details = document.getElementById("docs-nav-details");
    if (window.matchMedia && window.matchMedia("(max-width: 860px)").matches && details.contains(a)) details.open = false;
    if (doc === currentDoc() && url.search === location.search) {
      if (url.hash) return; // in-page anchor: default behaviour
    }
    e.preventDefault();
    history.pushState(null, "", url.pathname + url.search + url.hash);
    show(doc, true);
  });
  window.addEventListener("popstate", () => show(currentDoc(), false));

  if (window.matchMedia && window.matchMedia("(max-width: 860px)").matches) document.getElementById("docs-nav-details").open = false;

  fetch("/__backyard/api/info", { credentials: "same-origin", headers: { Accept: "application/json" } })
    .then((r) => (r.ok ? r.json() : null))
    .then((info) => {
      if (!info || !info.name) return;
      serverName = info.name;
      for (const n of document.querySelectorAll("[data-server-name]")) n.textContent = info.name;
      document.title = document.title.replace(/· .*$/, "· " + serverName);
    })
    .catch(() => {});

  show(currentDoc(), false);
})();
