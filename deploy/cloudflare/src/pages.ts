/** Built-in HTML pages: errors, "site not found", the CLI token page. */
import { HttpError, escapeHTML } from "./http";

const STYLE = `:root{color-scheme:light dark;--bg:#f7f7f5;--fg:#1d1d1b;--muted:#6b6b66;--card:#fff;--line:#e4e4df;--accent:#2f6f4f}
@media (prefers-color-scheme:dark){:root{--bg:#161615;--fg:#ededea;--muted:#a3a39c;--card:#1f1f1d;--line:#30302d;--accent:#6fbf8f}}
*{box-sizing:border-box}
body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;background:var(--bg);color:var(--fg);font:16px/1.5 system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;padding:16px}
main{max-width:520px;width:100%;background:var(--card);border:1px solid var(--line);border-radius:12px;padding:32px}
.code{font:600 13px/1 ui-monospace,SFMono-Regular,Menlo,monospace;color:var(--muted);letter-spacing:.04em}
h1{font-size:22px;margin:8px 0 12px}
p{margin:0 0 20px;color:var(--muted);overflow-wrap:anywhere}
.actions{display:flex;gap:12px;flex-wrap:wrap}
a.btn,button.btn{display:inline-block;padding:8px 16px;border-radius:8px;text-decoration:none;border:1px solid var(--line);color:var(--fg);background:none;font:inherit;cursor:pointer}
a.btn.primary,button.btn.primary{background:var(--accent);border-color:var(--accent);color:#fff}
pre{white-space:pre-wrap;word-break:break-all;background:var(--bg);border:1px solid var(--line);border-radius:8px;padding:12px;font:13px/1.4 ui-monospace,Menlo,monospace;margin:0 0 16px}`;

/** Data of the generic page. */
export interface PageData {
  status: number;
  title: string;
  message: string;
  actionURL?: string;
  actionText?: string;
  hubURL?: string;
}

/** Default heading for an HTTP status. */
export function defaultTitle(status: number): string {
  switch (status) {
    case 401:
      return "Sign in required";
    case 403:
      return "Access denied";
    case 404:
      return "Page not found";
    case 405:
      return "Method not allowed";
    case 413:
      return "Too large";
    case 429:
      return "Slow down";
    case 501:
      return "Not available";
    case 502:
    case 504:
      return "Upstream error";
    case 503:
      return "Unavailable";
  }
  return status >= 500 ? "Something went wrong" : "Bad request";
}

/** Renders the generic page. */
export function renderPage(d: PageData): string {
  const action = d.actionURL
    ? `<a class="btn primary" href="${escapeHTML(d.actionURL)}">${escapeHTML(d.actionText ?? "Continue")}</a>`
    : "";
  const hub = d.hubURL ? `<a class="btn" href="${escapeHTML(d.hubURL)}">Go to the Backyard hub</a>` : "";
  return `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>${escapeHTML(d.title)} · Backyard</title>
<style>${STYLE}</style>
</head>
<body>
<main>
<div class="code">${d.status}</div>
<h1>${escapeHTML(d.title)}</h1>
<p>${escapeHTML(d.message)}</p>
<div class="actions">${action}${hub}</div>
</main>
</body>
</html>
`;
}

/** HTML response for an error (browser clients). */
export function errorPage(err: HttpError, hubURL: string, method = "GET"): Response {
  const html = renderPage({
    status: err.status,
    title: err.title ?? defaultTitle(err.status),
    message: err.message,
    hubURL: hubURL + "/",
    ...(err.loginURL ? { actionURL: err.loginURL, actionText: "Sign in" } : {}),
  });
  const h = new Headers({ "Content-Type": "text/html; charset=utf-8", "Cache-Control": "no-store" });
  if (err.retryAfter !== undefined && err.retryAfter > 0) h.set("Retry-After", String(Math.ceil(err.retryAfter)));
  return new Response(method === "HEAD" ? null : html, { status: err.status, headers: h });
}

/** The built-in page of a site without any files (SPEC §3.4). */
export function siteNotFound(site: string): HttpError {
  return new HttpError(
    404,
    "not_found",
    `There is no site named ${JSON.stringify(site)} yet. Deploy a folder to create it, or browse the existing sites on the hub.`,
    { title: "Site not found" },
  );
}

/** The built-in 404 page of an existing site. */
export function pageNotFound(): HttpError {
  return new HttpError(404, "not_found", "The page you are looking for does not exist on this site.", { title: "Page not found" });
}

/**
 * Security headers of the self-contained auth pages (no framing, no scripts
 * but ours, no forms unless formAction names where one may go).
 */
function authPageHeaders(nonce: string, formAction = "'none'"): Headers {
  return new Headers({
    "Content-Type": "text/html; charset=utf-8",
    "Cache-Control": "no-store, max-age=0",
    Pragma: "no-cache",
    Expires: "0",
    "Referrer-Policy": "no-referrer",
    "X-Frame-Options": "DENY",
    "Content-Security-Policy": `default-src 'none'; style-src 'unsafe-inline'; script-src 'nonce-${nonce}'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action ${formAction}`,
  });
}

/** The confirmation form of a cross-site CLI login (/__backyard/auth/cli). */
export interface CLIConfirmForm {
  title: string;
  message: string;
  detail: string;
  /** Where the form posts (the CLI login endpoint of this host). */
  action: string;
  /** Hidden fields: redirect_uri, state and mode ("" ones are left out) and the CSRF token. */
  redirectURI: string;
  state: string;
  mode: string;
  csrf: string;
  cancelURL: string;
  /**
   * Extra CSP form-action source: the loopback origin the answer of the form
   * redirects to (browsers apply form-action to those redirects too).
   */
  formAction: string;
}

/** Asks the user whether a CLI login that another site may have started may go ahead. */
export function cliConfirmPage(f: CLIConfirmForm): Response {
  const nonce = crypto.randomUUID().replace(/-/g, "");
  const hidden = (name: string, value: string) => (value ? `<input type="hidden" name="${name}" value="${escapeHTML(value)}">\n` : "");
  const html = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<meta name="referrer" content="no-referrer">
<title>${escapeHTML(f.title)} · Backyard</title>
<style>${STYLE}</style>
</head>
<body>
<main>
<h1>${escapeHTML(f.title)}</h1>
<p>${escapeHTML(f.message)}</p>
<p>${escapeHTML(f.detail)}</p>
<form method="post" action="${escapeHTML(f.action)}">
${hidden("redirect_uri", f.redirectURI)}${hidden("state", f.state)}${hidden("mode", f.mode)}${hidden("csrf", f.csrf)}<div class="actions"><button class="btn primary" id="allow" type="submit">Allow</button><a class="btn" id="cancel" href="${escapeHTML(f.cancelURL)}">Cancel</a></div>
</form>
</main>
</body>
</html>
`;
  const formAction = ("'self' " + f.formAction).trim();
  return new Response(html, { status: 200, headers: authPageHeaders(nonce, formAction) });
}

/** Auth-flow error page (/__backyard/auth/*). */
export function authErrorPage(status: number, title: string, message: string): Response {
  const nonce = crypto.randomUUID().replace(/-/g, "");
  return new Response(renderPage({ status, title, message }), { status, headers: authPageHeaders(nonce) });
}

/** The page showing a freshly minted CLI token (/__backyard/auth/cli?mode=display). */
export function tokenPage(opts: { who: string; token: string; expiresAt: string; command: string }): Response {
  const nonce = crypto.randomUUID().replace(/-/g, "");
  const html = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>Your Backyard CLI token · Backyard</title>
<style>${STYLE}</style>
</head>
<body>
<main>
<h1>Your Backyard CLI token</h1>
<p>Signed in as ${escapeHTML(opts.who)}. The token expires ${escapeHTML(opts.expiresAt)}. Paste this command into your terminal:</p>
<pre id="cmd">${escapeHTML(opts.command)}</pre>
<div class="actions"><button class="btn" id="copy" type="button">Copy</button></div>
</main>
<script nonce="${nonce}">
document.getElementById("copy").addEventListener("click", function () {
  navigator.clipboard.writeText(document.getElementById("cmd").textContent).then(function () {
    document.getElementById("copy").textContent = "Copied";
  });
});
</script>
</body>
</html>
`;
  return new Response(html, { status: 200, headers: authPageHeaders(nonce) });
}
