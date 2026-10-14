// The docs page markdown renderer: structure and safety.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadHubScript, plain } from "./helpers.mjs";

const docs = loadHubScript("docs.js").BackyardDocs;
const { renderMarkdown, slugify, safeURL, rewriteDocLink } = docs;
const html = (md, opts) => renderMarkdown(md, opts).html.trim();

test("headings get unique anchors, a TOC entry and the h1 is the title", () => {
  const r = renderMarkdown("# Backyard API\n\n## Database\n\n### `collection(name)`\n\n## Database\n\nSetext two\n---\n");
  assert.equal(r.title, "Backyard API");
  assert.deepEqual(plain(r.headings), [
    { level: 1, text: "Backyard API", id: "backyard-api" },
    { level: 2, text: "Database", id: "database" },
    { level: 3, text: "collection(name)", id: "collectionname" },
    { level: 2, text: "Database", id: "database-1" },
    { level: 2, text: "Setext two", id: "setext-two" },
  ]);
  assert.match(r.html, /<h1 id="backyard-api">Backyard API<\/h1>/);
  assert.match(r.html, /<h2 id="database-1">Database <a class="anchor" href="#database-1" aria-label="Link to this section">#<\/a><\/h2>/);
  assert.match(r.html, /<h3 id="collectionname"><code>collection\(name\)<\/code>/);
  assert.equal(html("## x ##"), '<h2 id="x">x <a class="anchor" href="#x" aria-label="Link to this section">#</a></h2>');
  assert.doesNotMatch(html("## x", { anchors: false }), /anchor/);
});

test("slugify", () => {
  assert.equal(slugify("Hello, World!"), "hello-world");
  assert.equal(slugify("  db.collection(name) → Doc  "), "dbcollectionname-doc");
  assert.equal(slugify("Ünïcode 日本"), "ünïcode-日本");
  assert.equal(slugify("!!!"), "section");
});

test("paragraphs, emphasis, inline code, line breaks", () => {
  assert.equal(html("a **b** *c* _d_ __e__ ~~f~~ `g`"), "<p>a <strong>b</strong> <em>c</em> <em>d</em> <strong>e</strong> <del>f</del> <code>g</code></p>");
  assert.equal(html("snake_case_name and 2*3*4"), "<p>snake_case_name and 2<em>3</em>4</p>");
  assert.equal(html("code `a **b** <i>` and `` x`y ``"), "<p>code <code>a **b** &lt;i&gt;</code> and <code>x`y</code></p>");
  assert.equal(html("line one  \nline two\\\nline three\nsame"), "<p>line one<br>\nline two<br>\nline three\nsame</p>");
  assert.equal(html("\\*not em\\* \\`x\\`"), "<p>*not em* `x`</p>");
  assert.equal(html("one\n\ntwo"), "<p>one</p>\n<p>two</p>");
});

test("links, autolinks and images", () => {
  assert.equal(html("[docs](/docs?doc=api#db \"API\")"), '<p><a href="/docs?doc=api#db" title="API">docs</a></p>');
  assert.equal(
    html("[ext](https://example.com/a_(b))"),
    '<p><a href="https://example.com/a_(b)" target="_blank" rel="noopener noreferrer">ext</a></p>',
  );
  assert.equal(html("[**bold** `code`](x.html)"), '<p><a href="x.html"><strong>bold</strong> <code>code</code></a></p>');
  assert.equal(html("<https://a.b/c?d=1&e=2>"), '<p><a href="https://a.b/c?d=1&amp;e=2" target="_blank" rel="noopener noreferrer">https://a.b/c?d=1&amp;e=2</a></p>');
  assert.equal(html("see https://a.b/c_d."), '<p>see <a href="https://a.b/c_d" target="_blank" rel="noopener noreferrer">https://a.b/c_d</a>.</p>');
  assert.equal(html("![logo](/favicon.svg \"Logo\")"), '<p><img src="/favicon.svg" alt="logo" title="Logo" loading="lazy"></p>');
  assert.equal(html("mail <mailto:a@b.c>"), '<p>mail <a href="mailto:a@b.c">mailto:a@b.c</a></p>');
});

test("links between skill documents map to the docs page", () => {
  const cases = [
    ["references/api.md", "/docs?doc=api"],
    ["./recipes.md#live-lists", "/docs?doc=recipes#live-lists"],
    ["../SKILL.md", "/docs?doc=skill"],
    ["deploy.md", "/docs?doc=deploy"],
    ["https://example.com/x.md", "https://example.com/x.md"],
    ["/llms.txt", "/llms.txt"],
  ];
  for (const [input, want] of cases) assert.equal(rewriteDocLink(input), want, input);
  assert.match(html("[api](references/api.md#files)", { rewriteLink: rewriteDocLink }), /href="\/docs\?doc=api#files"/);
});

test("lists: nested, ordered with start, tasks, tight vs loose, lazy continuation", () => {
  assert.equal(html("- a\n- b\n  - c\n  - d\n- e"), "<ul>\n<li>a</li>\n<li>b\n<ul>\n<li>c</li>\n<li>d</li>\n</ul></li>\n<li>e</li>\n</ul>");
  assert.equal(html("3. three\n4. four"), '<ol start="3">\n<li>three</li>\n<li>four</li>\n</ol>');
  assert.equal(html("1) a\n2) b"), "<ol>\n<li>a</li>\n<li>b</li>\n</ol>");
  assert.equal(html("- [ ] todo\n- [x] done"), '<ul>\n<li class="task"><input type="checkbox" disabled> todo</li>\n<li class="task"><input type="checkbox" disabled checked> done</li>\n</ul>');
  assert.equal(html("- a\n\n- b"), "<ul>\n<li><p>a</p></li>\n<li><p>b</p></li>\n</ul>");
  assert.equal(html("- item\ncontinued\n\npara"), "<ul>\n<li>item\ncontinued</li>\n</ul>\n<p>para</p>");
  assert.equal(
    html("1. Install:\n\n   ```sh\n   npm i\n   ```\n2. Run"),
    '<ol>\n<li><p>Install:</p>\n<div class="code-block"><pre><code class="language-sh" data-lang="sh">npm i</code></pre></div></li>\n<li><p>Run</p></li>\n</ol>',
  );
  assert.equal(html("* x\n+ y"), "<ul>\n<li>x</li>\n</ul>\n<ul>\n<li>y</li>\n</ul>");
  assert.equal(html("text\n- list interrupts"), "<p>text</p>\n<ul>\n<li>list interrupts</li>\n</ul>");
  assert.equal(html("year\n2026. is not a list"), "<p>year\n2026. is not a list</p>");
});

test("code fences keep content verbatim and escaped", () => {
  const md = "```html\n<script>alert('x')</script>\n  indented & <b>\n\n# not a heading\n```\n\n~~~\nplain\n~~~";
  assert.equal(
    html(md),
    '<div class="code-block"><pre><code class="language-html" data-lang="html">&lt;script&gt;alert(&#39;x&#39;)&lt;/script&gt;\n  indented &amp; &lt;b&gt;\n\n# not a heading</code></pre></div>\n' +
      '<div class="code-block"><pre><code>plain</code></pre></div>',
  );
  assert.equal(html("````\n```\nnested\n```\n````"), '<div class="code-block"><pre><code>```\nnested\n```</code></pre></div>');
  assert.equal(html("```\nunclosed"), '<div class="code-block"><pre><code>unclosed</code></pre></div>');
  assert.equal(html('```js" onmouseover="x\ncode\n```'), '<div class="code-block"><pre><code class="language-js" data-lang="js">code</code></pre></div>');
});

test("tables with alignment, escaped pipes and code", () => {
  const md = "| Method | Path | Result |\n|:--|:-:|--:|\n| `GET` | `/db/{c}` | docs \\| 404 |\n| POST | `/a\\|b` |\n\nafter";
  assert.equal(
    html(md),
    '<div class="table-wrap"><table>\n' +
      '<thead><tr><th>Method</th><th class="align-center">Path</th><th class="align-right">Result</th></tr></thead>\n' +
      "<tbody>\n" +
      '<tr><td><code>GET</code></td><td class="align-center"><code>/db/{c}</code></td><td class="align-right">docs | 404</td></tr>\n' +
      '<tr><td>POST</td><td class="align-center"><code>/a|b</code></td><td class="align-right"></td></tr>\n' +
      "</tbody></table></div>\n<p>after</p>",
  );
  assert.equal(html("a | b\n--- | ---\n1 | 2"), '<div class="table-wrap"><table>\n<thead><tr><th>a</th><th>b</th></tr></thead>\n<tbody>\n<tr><td>1</td><td>2</td></tr>\n</tbody></table></div>');
  assert.equal(html("not | a table\n---"), '<h2 id="not-a-table">not | a table <a class="anchor" href="#not-a-table" aria-label="Link to this section">#</a></h2>');
});

test("blockquotes, horizontal rules, comments and front matter", () => {
  assert.equal(html("> **Note**\n> second line\n>\n> - item"), "<blockquote>\n<p><strong>Note</strong>\nsecond line</p>\n<ul>\n<li>item</li>\n</ul>\n</blockquote>");
  assert.equal(html("a\n\n***\n\nb"), "<p>a</p>\n<hr>\n<p>b</p>");
  assert.equal(html("<!-- hidden\ncomment -->\nvisible"), "<p>visible</p>");
  const r = renderMarkdown("---\nname: backyard\ndescription: x\n---\n# Skill\n");
  assert.equal(r.html.trim(), '<h1 id="skill">Skill</h1>');
});

// Every tag the renderer may emit, with its allowed attributes.
const ALLOWED = {
  a: ["href", "title", "target", "rel", "class", "aria-label"],
  img: ["src", "alt", "title", "loading"],
  input: ["type", "disabled", "checked"],
  code: ["class", "data-lang"],
  div: ["class"],
  th: ["class"],
  td: ["class"],
  ol: ["start"],
  li: ["class"],
  h1: ["id"],
  h2: ["id"],
  h3: ["id"],
  h4: ["id"],
  h5: ["id"],
  h6: ["id"],
};
for (const t of ["p", "pre", "ul", "blockquote", "table", "thead", "tbody", "tr", "strong", "em", "del", "br", "hr"]) ALLOWED[t] = [];

/** Fails unless html consists of well-formed allowlisted tags with quoted, allowlisted attributes. */
function assertSafeHTML(out, context) {
  const tagRe = /<\/?([a-z0-9]+)((?:\s+[a-z-]+(?:="[^"<>]*")?)*)\s*>/g;
  const stripped = out.replace(tagRe, (m, name, attrs) => {
    assert.ok(ALLOWED[name], `unexpected <${name}> in ${JSON.stringify(context)} → ${out}`);
    for (const a of attrs.matchAll(/([a-z-]+)(?:="[^"]*")?/g)) {
      assert.ok(ALLOWED[name].includes(a[1]), `unexpected attribute ${a[1]} on <${name}> in ${JSON.stringify(context)} → ${out}`);
    }
    return "";
  });
  assert.doesNotMatch(stripped, /[<>]/, `stray markup in ${JSON.stringify(context)} → ${out}`);
}

test("XSS: raw HTML is escaped and dangerous URLs are dropped", () => {
  const attacks = [
    "<script>alert(1)</script>",
    "<img src=x onerror=alert(1)>",
    "[x](javascript:alert(1))",
    "[x](JaVaScRiPt:alert(1))",
    "[x](java\tscript:alert(1))",
    "[x](  javascript:alert(1))",
    "[x](vbscript:msgbox(1))",
    "[x](data:text/html;base64,PHNjcmlwdD4=)",
    "![x](javascript:alert(1))",
    "![x](data:image/svg+xml,<svg onload=alert(1)>)",
    '[x](https://ok.example "a\\" onmouseover=\\"alert(1)")',
    '[x](https://ok.example/"onmouseover="alert(1))',
    "<javascript:alert(1)>",
    "# <img src=x onerror=alert(1)>",
    "| <b onclick=x> |\n|---|\n| <i>",
    "- <iframe src=//evil>",
    "`</code><script>x</script>`",
    "&lt;script&gt; &#106;avascript:",
    // Backslash escapes and code spans become placeholders; they must not be
    // restored into a URL after it was validated.
    "[x](javascript\\:alert(1))",
    "[x](<javascript\\:alert(1)>)",
    "[x](javascript&#58;alert(1))",
    "![x](javascript\\:alert(1))",
    "[x](java`script:`alert(1))",
    "[x](javascript`:`alert(1))",
    '[x](https://ok.example "`<b>`\\" onclick=\\"x")',
  ];
  for (const md of attacks) {
    const out = html(md);
    assertSafeHTML(out, md);
    assert.doesNotMatch(out, /href="\s*(javascript|vbscript|data):/i, md + " → " + out);
    assert.doesNotMatch(out, /src="\s*(javascript|data):/i, md + " → " + out);
  }
  assert.match(html('[x](https://ok.example "a\\" onmouseover=\\"alert(1)")'), /title="a&quot; onmouseover=&quot;alert\(1\)"/, "quotes stay inside the attribute");
  assert.equal(html("&lt;b&gt;"), "<p>&amp;lt;b&amp;gt;</p>");
  // Escapes inside URLs, titles and alt texts resolve to their plain text.
  assert.equal(html("[x](/a\\_b \"t\\\"\")"), '<p><a href="/a_b" title="t&quot;">x</a></p>');
  assert.equal(html("![a\\*b](/i.png)"), '<p><img src="/i.png" alt="a*b" loading="lazy"></p>');
  assert.equal(html("[x](javascript\\:alert(1))"), "<p>x</p>");
  assert.equal(safeURL("javascript:x"), "");
  assert.equal(safeURL(" https://x "), "https://x");
  assert.equal(safeURL("/relative"), "/relative");
  assert.equal(safeURL("#anchor"), "#anchor");
  assert.equal(safeURL("mailto:a@b"), "mailto:a@b");
});

test("private-use placeholder characters in the input cannot inject markup", () => {
  const out = html("0 [a](b) ");
  assert.equal(out, '<p>0 <a href="b">a</a> </p>');
});

test("CRLF input, tabs and empty documents", () => {
  assert.equal(html("# T\r\n\r\n-\ta\r\n"), '<h1 id="t">T</h1>\n<ul>\n<li>a</li>\n</ul>');
  assert.equal(html(""), "");
  assert.equal(html(null), "");
});

test("expired sessions: login responses, the login URL and the re-login guard", () => {
  const { isLoginResponse, loginURL, shouldRelogin } = docs;
  const h = (ct) => ({ get: (k) => (k.toLowerCase() === "content-type" ? ct : null) });
  // fetch(…, { redirect: "manual" }) of a redirect to the login page / IdP.
  assert.equal(isLoginResponse({ type: "opaqueredirect", status: 0, ok: false, headers: h(null) }), true);
  assert.equal(isLoginResponse({ type: "basic", status: 401, ok: false, headers: h("text/html") }), true);
  // A proxy serving its HTML login page in place of the markdown.
  assert.equal(isLoginResponse({ type: "basic", status: 200, ok: true, headers: h("text/html; charset=utf-8") }), true);
  assert.equal(isLoginResponse({ type: "basic", status: 200, ok: true, redirected: true, url: "https://hub.test/__backyard/auth/login?rd=x", headers: h("text/markdown") }), true);
  assert.equal(isLoginResponse({ type: "basic", status: 200, ok: true, headers: h("text/markdown; charset=utf-8") }), false);
  assert.equal(isLoginResponse({ type: "basic", status: 404, ok: false, headers: h("text/html") }), false);
  assert.equal(isLoginResponse(null), false);

  assert.equal(loginURL("https://backyard.acme.com/docs?doc=api#files"), "/__backyard/auth/login?rd=https%3A%2F%2Fbackyard.acme.com%2Fdocs%3Fdoc%3Dapi%23files");

  const mem = new Map();
  const storage = { getItem: (k) => mem.get(k) ?? null, setItem: (k, v) => mem.set(k, String(v)) };
  assert.equal(shouldRelogin(storage, 1_000_000), true, "first time: go");
  assert.equal(shouldRelogin(storage, 1_010_000), false, "again within 30 s: a loop, stop");
  assert.equal(shouldRelogin(storage, 1_031_000), true, "later: go again");
  assert.equal(shouldRelogin(null, 5), true, "no storage: go");
  const broken = {
    getItem() {
      throw new Error("SecurityError");
    },
    setItem() {
      throw new Error("SecurityError");
    },
  };
  assert.equal(shouldRelogin(broken, 5), true);
});
