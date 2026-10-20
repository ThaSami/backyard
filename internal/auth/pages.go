package auth

import (
	"bytes"
	"html/template"
	"net/http"
	"strconv"
	"strings"
)

// pageData feeds pageTemplate.
type pageData struct {
	Title    string
	Message  string
	Detail   string
	LinkURL  string
	LinkText string
	// CLI token display.
	Token     string
	ExpiresAt string
	Command   string
	Nonce     string
	// Confirm renders the CLI login confirmation form.
	Confirm *cliConfirmForm
	// Consent renders the consent page of the MCP connector.
	Consent *consentForm
	// formAction is an extra CSP form-action source for Confirm and Consent
	// (the target of the redirect that answers the form).
	formAction string
}

// consentForm feeds the consent page (SPEC §8.4.5).
type consentForm struct {
	Who        string // email, name or id of the signed-in user ("Guest 1a2b")
	Guest      bool   // mode none: the app acts as this browser's guest
	SignOutURL string // "" where Backyard has no logout
	ClientName string
	Verified   bool   // CIMD: the name comes from an allowlisted document
	Unlisted   bool   // an unlisted metadata document: the name is its client_id
	ClientHost string // CIMD: host of the client_id URL
	Origin     string // origin of the redirect URI
	Loopback   bool   // the redirect URI is an app on this computer
	Host       string // H, the hub host
	Until      string // YYYY-MM-DD (UTC) the connection lasts until
	Fields     []formField
}

// formField is a hidden input of the consent form.
type formField struct{ Name, Value string }

var pageTemplate = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="referrer" content="no-referrer">
<title>{{.Title}} · Backyard</title>
<style>
:root { --bg:#f7f7f5; --fg:#1d1d1b; --muted:#66655f; --card:#fff; --border:#e2e1dc; --accent:#2f6f4f; --code:#f0efe9; }
@media (prefers-color-scheme: dark) {
  :root { --bg:#151514; --fg:#ecebe6; --muted:#a3a29b; --card:#1e1e1c; --border:#33332f; --accent:#7cc39b; --code:#262624; }
}
* { box-sizing: border-box; }
body { margin:0; min-height:100vh; display:flex; align-items:center; justify-content:center; padding:16px;
  background:var(--bg); color:var(--fg); font:16px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif; }
main { width:100%; max-width:560px; background:var(--card); border:1px solid var(--border); border-radius:12px; padding:28px; }
h1 { margin:0 0 8px; font-size:1.35rem; }
p { margin:8px 0; overflow-wrap:anywhere; }
.muted { color:var(--muted); font-size:.92rem; }
a { color:var(--accent); }
textarea, pre { width:100%; font:13px/1.45 ui-monospace,SFMono-Regular,Menlo,monospace; background:var(--code);
  color:var(--fg); border:1px solid var(--border); border-radius:8px; padding:10px; word-break:break-all; white-space:pre-wrap; }
textarea { height:120px; resize:vertical; }
button { margin-top:8px; font:inherit; padding:6px 14px; border-radius:8px; border:1px solid var(--border);
  background:var(--accent); color:var(--card); cursor:pointer; }
form { margin-top:16px; display:flex; flex-wrap:wrap; align-items:center; gap:16px; }
form button { margin-top:0; }
#deny { background:transparent; color:var(--fg); }
#consent button { visibility:hidden; animation: by-arm 0s linear 0.8s forwards; }
@keyframes by-arm { to { visibility: visible } }
.warn { border-left:3px solid var(--accent); padding-left:10px; }
</style>
</head>
<body>
<main>
<h1>{{.Title}}</h1>
{{if .Message}}<p>{{.Message}}</p>{{end}}
{{if .Detail}}<p class="muted">{{.Detail}}</p>{{end}}
{{if .Token}}
<p>Paste this token into your terminal. It expires {{.ExpiresAt}}.</p>
<textarea id="token" readonly spellcheck="false" aria-label="API token">{{.Token}}</textarea>
<button id="copy" type="button">Copy token</button>
{{if .Command}}<p class="muted">Or run:</p><pre>{{.Command}}</pre>{{end}}
<p class="muted">Treat this token like a password. Anyone holding it can act as you.</p>
<script nonce="{{.Nonce}}">
document.getElementById("copy").addEventListener("click", function () {
  var t = document.getElementById("token");
  t.select();
  if (navigator.clipboard) { navigator.clipboard.writeText(t.value); } else { document.execCommand("copy"); }
  this.textContent = "Copied";
});
</script>
{{end}}
{{with .Confirm}}
<form method="post" action="{{.Action}}">
{{if .RedirectURI}}<input type="hidden" name="redirect_uri" value="{{.RedirectURI}}">{{end}}
{{if .State}}<input type="hidden" name="state" value="{{.State}}">{{end}}
{{if .Mode}}<input type="hidden" name="mode" value="{{.Mode}}">{{end}}
<input type="hidden" name="csrf" value="{{.CSRF}}">
<button id="allow" type="submit">Allow</button>
<a id="cancel" href="{{.CancelURL}}">Cancel</a>
</form>
{{end}}
{{with .Consent}}
<p id="consent-user">{{if .Guest}}<strong>{{.Who}}</strong>. This Backyard has no sign-in; the app will act as this browser's guest.{{else}}You are signed in as <strong>{{.Who}}</strong>.{{end}}{{if .SignOutURL}} <a href="{{.SignOutURL}}">Not you? Sign out</a>{{end}}</p>
<p id="consent-app">{{if .Verified}}<strong>{{.ClientName}}</strong> — verified by <strong>{{.ClientHost}}</strong>{{else if .Unlisted}}<strong>{{.ClientName}}</strong> (address provided by the app, not verified){{else}}<strong>"{{.ClientName}}"</strong> (name provided by the app, not verified){{end}} wants to use Backyard as you.</p>
<p id="consent-return">{{if .Loopback}}Backyard will send the approval to <strong>an app on this computer ({{.Origin}})</strong>. Only continue if you just started the sign-in from an app on this computer.{{else}}Backyard will send the approval to <strong>{{.Origin}}</strong>.{{end}}</p>
<p>The app can see the sites on {{.Host}} and read their files, and publish or change sites as you. Published sites are visible to everyone who can sign in to this Backyard.</p>
<p class="muted">This connection lasts until {{.Until}} unless you disconnect it on the Backyard hub.</p>
<p class="warn">Only allow this if you started connecting {{.ClientName}} to Backyard yourself just now. Anyone who sent you this link would get this access.</p>
<form id="consent" method="post" action="/__backyard/auth/authorize">
{{range .Fields}}<input type="hidden" name="{{.Name}}" value="{{.Value}}">
{{end}}<button type="submit" name="decision" value="allow" id="allow">Allow</button>
<button type="submit" name="decision" value="deny" id="deny">Cancel</button>
</form>
{{end}}
{{if .LinkURL}}<p><a href="{{.LinkURL}}">{{.LinkText}}</a></p>{{end}}
</main>
</body>
</html>
`))

// noStore marks a response as uncacheable.
func noStore(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store, max-age=0")
	h.Set("Pragma", "no-cache")
	h.Set("Expires", "0")
}

// renderPage writes a small self-contained HTML page.
func renderPage(w http.ResponseWriter, status int, d pageData) {
	if d.Nonce == "" {
		d.Nonce = randomString(16)
	}
	var buf bytes.Buffer
	if err := pageTemplate.Execute(&buf, d); err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	h := w.Header()
	noStore(w)
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(buf.Len()))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	formAction := "'none'"
	if d.Confirm != nil || d.Consent != nil {
		formAction = strings.TrimSpace("'self' " + d.formAction)
	}
	if d.Consent != nil {
		// The consent page has no script at all.
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src 'self' data:; "+
			"frame-ancestors 'none'; base-uri 'none'; form-action "+formAction)
	} else {
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'nonce-"+d.Nonce+
			"'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action "+formAction)
	}
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// renderError renders an error page with an optional retry link.
func renderError(w http.ResponseWriter, status int, title, message, linkURL, linkText string) {
	renderPage(w, status, pageData{Title: title, Message: message, LinkURL: linkURL, LinkText: linkText})
}
