package auth

import (
	"bytes"
	"html/template"
	"net/http"
	"strconv"
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
}

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
p { margin:8px 0; }
.muted { color:var(--muted); font-size:.92rem; }
a { color:var(--accent); }
textarea, pre { width:100%; font:13px/1.45 ui-monospace,SFMono-Regular,Menlo,monospace; background:var(--code);
  color:var(--fg); border:1px solid var(--border); border-radius:8px; padding:10px; word-break:break-all; white-space:pre-wrap; }
textarea { height:120px; resize:vertical; }
button { margin-top:8px; font:inherit; padding:6px 14px; border-radius:8px; border:1px solid var(--border);
  background:var(--accent); color:var(--card); cursor:pointer; }
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
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'nonce-"+d.Nonce+
		"'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// renderError renders an error page with an optional retry link.
func renderError(w http.ResponseWriter, status int, title, message, linkURL, linkText string) {
	renderPage(w, status, pageData{Title: title, Message: message, LinkURL: linkURL, LinkText: linkText})
}
