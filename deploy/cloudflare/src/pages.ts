/**
 * Built-in HTML pages: errors, "site not found", the CLI token page and the
 * consent page of the Claude connector (SPEC §8.4.5).
 */
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
 * Security headers of the self-contained auth pages: no framing, no scripts
 * but ours (none at all without a nonce), no forms unless formAction names
 * where one may go (its sources: "'self'" and the origin a form's answer
 * redirects to, see formActionSource).
 */
export function authPageHeaders(nonce: string, formAction = "'none'"): Headers {
  const script = nonce ? ` script-src 'nonce-${nonce}';` : "";
  return new Headers({
    "Content-Type": "text/html; charset=utf-8",
    "Cache-Control": "no-store, max-age=0",
    Pragma: "no-cache",
    Expires: "0",
    "Referrer-Policy": "no-referrer",
    "X-Frame-Options": "DENY",
    "Content-Security-Policy": `default-src 'none'; style-src 'unsafe-inline';${script} img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action ${formAction}`,
  });
}

/**
 * The CSP form-action source of the origin a form's answer redirects to
 * (browsers apply form-action to those redirects too): "https://claude.ai",
 * "http://127.0.0.1:9"; "http:" for an IPv6 host such as [::1], which CSP
 * cannot name; "" for none.
 */
export function formActionSource(u: URL | null): string {
  if (!u) return "";
  const scheme = u.protocol.slice(0, -1).toLowerCase();
  return u.hostname.includes(":") ? scheme + ":" : `${scheme}://${u.host.toLowerCase()}`;
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

/** Extras of an auth-flow error page. */
export interface AuthErrorExtras {
  /** A second sentence (what to do about it). */
  detail?: string;
  /** A link, e.g. "Sign out". */
  linkURL?: string;
  linkText?: string;
  /** Allow header of a 405. */
  allow?: string;
  /** Retry-After seconds. */
  retryAfter?: number;
}

/** Auth-flow error page (/__backyard/auth/*). */
export function authErrorPage(status: number, title: string, message: string, extras: AuthErrorExtras = {}): Response {
  const nonce = crypto.randomUUID().replace(/-/g, "");
  const d: PageData = { status, title, message: extras.detail ? `${message} ${extras.detail}` : message };
  if (extras.linkURL) {
    d.actionURL = extras.linkURL;
    d.actionText = extras.linkText ?? "Continue";
  }
  const h = authPageHeaders(nonce);
  if (extras.allow) h.set("Allow", extras.allow);
  if (extras.retryAfter) h.set("Retry-After", String(Math.ceil(extras.retryAfter)));
  return new Response(renderPage(d), { status, headers: h });
}

/** What the consent page of the Claude connector shows (SPEC §8.4.5). */
export interface ConsentForm {
  /** The signed-in user: email, else name, else id ("Guest 1a2b" in mode none). */
  who: string;
  /** Mode none: the app acts as this browser's guest. */
  guest: boolean;
  /** "Not you? Sign out" target ("" where there is no logout). */
  signOutURL: string;
  /** The display name (SPEC §8.4.3): unlisted metadata documents are named by their client_id. */
  clientName: string;
  /** A client metadata document vouches for the name (CIMD); else the app named itself. */
  verified: boolean;
  /** An unlisted metadata document: the name is the address the app sent, not verified. */
  unlisted: boolean;
  /** Host of the client metadata URL (verified clients). */
  clientHost: string;
  /** Origin the approval is sent to (the redirect URI's). */
  origin: string;
  /** The redirect URI is an app on this computer (loopback). */
  loopback: boolean;
  /** H, the hub host. */
  hubHost: string;
  /** YYYY-MM-DD (UTC) the connection lasts until. */
  until: string;
  /** Hidden fields, in order (csrf last). */
  fields: [string, string][];
  /** Extra CSP form-action source: the redirect URI's origin (formActionSource). */
  formAction: string;
}

/** Arming of the consent buttons: they appear after 0.8 s (double-clickjacking). */
const CONSENT_STYLE = `#consent{display:flex;flex-wrap:wrap;gap:12px;margin-top:8px}
#consent button{visibility:hidden;animation: by-arm 0s linear 0.8s forwards;padding:8px 16px;border-radius:8px;border:1px solid var(--line);font:inherit;cursor:pointer;background:none;color:var(--fg)}
#consent #allow{background:var(--accent);border-color:var(--accent);color:#fff}
@keyframes by-arm { to { visibility: visible } }
.warn{border-left:3px solid var(--accent);padding-left:10px}
strong{color:var(--fg)}`;

/**
 * The consent page of /__backyard/auth/authorize: shown on every
 * authorization, before any code exists. It has no script; its form posts
 * back to the authorize endpoint with a CSRF token.
 */
export function consentPage(f: ConsentForm): Response {
  const e = escapeHTML;
  const user = f.guest
    ? `<strong>${e(f.who)}</strong>. This Backyard has no sign-in; the app will act as this browser's guest.`
    : `You are signed in as <strong>${e(f.who)}</strong>.`;
  const signOut = f.signOutURL ? ` <a href="${e(f.signOutURL)}">Not you? Sign out</a>` : "";
  const app = f.verified
    ? `<strong>${e(f.clientName)}</strong> — verified by <strong>${e(f.clientHost)}</strong> wants to use Backyard as you.`
    : f.unlisted
      ? `<strong>${e(f.clientName)}</strong> (address provided by the app, not verified) wants to use Backyard as you.`
      : `<strong>"${e(f.clientName)}"</strong> (name provided by the app, not verified) wants to use Backyard as you.`;
  const back = f.loopback
    ? `Backyard will send the approval to <strong>an app on this computer (${e(f.origin)})</strong>. Only continue if you just started the sign-in from an app on this computer.`
    : `Backyard will send the approval to <strong>${e(f.origin)}</strong>.`;
  const hidden = f.fields.map(([name, value]) => `<input type="hidden" name="${e(name)}" value="${e(value)}">\n`).join("");
  const html = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<meta name="referrer" content="no-referrer">
<title>Connect an app to Backyard · Backyard</title>
<style>${STYLE}
${CONSENT_STYLE}</style>
</head>
<body>
<main>
<h1>Connect an app to Backyard</h1>
<p id="consent-user">${user}${signOut}</p>
<p id="consent-app">${app}</p>
<p id="consent-return">${back}</p>
<p>The app can see the sites on ${e(f.hubHost)} and read their files, and publish or change sites as you. Published sites are visible to everyone who can sign in to this Backyard.</p>
<p>This connection lasts until ${e(f.until)} unless you disconnect it on the Backyard hub.</p>
<p class="warn">Only allow this if you started connecting ${e(f.clientName)} to Backyard yourself just now. Anyone who sent you this link would get this access.</p>
<form id="consent" method="post" action="/__backyard/auth/authorize">
${hidden}<button type="submit" name="decision" value="allow" id="allow">Allow</button>
<button type="submit" name="decision" value="deny" id="deny">Cancel</button>
</form>
</main>
</body>
</html>
`;
  // No script at all: no nonce, so no script-src.
  return new Response(html, { status: 200, headers: authPageHeaders("", ("'self' " + f.formAction).trim()) });
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
