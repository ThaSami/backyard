package server

import (
	"bytes"
	"html/template"
	"net/http"
	"strconv"
)

// pageData feeds the built-in HTML page template.
type pageData struct {
	Status     int
	Title      string
	Message    string
	ActionURL  string // optional primary button (e.g. sign in)
	ActionText string
	HubURL     string
	RequestID  string
}

var pageTmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>{{.Title}} · Backyard</title>
<style>
:root{color-scheme:light dark;--bg:#f7f7f5;--fg:#1d1d1b;--muted:#6b6b66;--card:#fff;--line:#e4e4df;--accent:#2f6f4f}
@media (prefers-color-scheme:dark){:root{--bg:#161615;--fg:#ededea;--muted:#a3a39c;--card:#1f1f1d;--line:#30302d;--accent:#6fbf8f}}
*{box-sizing:border-box}
body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;background:var(--bg);color:var(--fg);font:16px/1.5 system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;padding:16px}
main{max-width:480px;width:100%;background:var(--card);border:1px solid var(--line);border-radius:12px;padding:32px}
.code{font:600 13px/1 ui-monospace,SFMono-Regular,Menlo,monospace;color:var(--muted);letter-spacing:.04em}
h1{font-size:22px;margin:8px 0 12px}
p{margin:0 0 20px;color:var(--muted);overflow-wrap:anywhere}
.actions{display:flex;gap:12px;flex-wrap:wrap}
a.btn{display:inline-block;padding:8px 16px;border-radius:8px;text-decoration:none;border:1px solid var(--line);color:var(--fg)}
a.btn.primary{background:var(--accent);border-color:var(--accent);color:#fff}
.rid{margin-top:24px;font:12px ui-monospace,Menlo,monospace;color:var(--muted)}
</style>
</head>
<body>
<main>
<div class="code">{{.Status}}</div>
<h1>{{.Title}}</h1>
<p>{{.Message}}</p>
<div class="actions">
{{- if .ActionURL}}<a class="btn primary" href="{{.ActionURL}}">{{.ActionText}}</a>{{end}}
{{- if .HubURL}}<a class="btn" href="{{.HubURL}}">Go to the Backyard hub</a>{{end}}
</div>
{{- if .RequestID}}<div class="rid">request id: {{.RequestID}}</div>{{end}}
</main>
</body>
</html>
`))

// defaultTitle returns a human heading for an HTTP status.
func defaultTitle(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "Sign in required"
	case http.StatusForbidden:
		return "Access denied"
	case http.StatusNotFound:
		return "Page not found"
	case http.StatusMethodNotAllowed:
		return "Method not allowed"
	case http.StatusRequestEntityTooLarge:
		return "Too large"
	case http.StatusTooManyRequests:
		return "Slow down"
	case http.StatusNotImplemented:
		return "Not available"
	case http.StatusBadGateway, http.StatusGatewayTimeout:
		return "Upstream error"
	case http.StatusServiceUnavailable:
		return "Unavailable"
	}
	if status >= 500 {
		return "Something went wrong"
	}
	return http.StatusText(status)
}

// pageForError builds page data for an API error.
func pageForError(e *apiError, hubURL string) pageData {
	d := pageData{Status: e.Status, Title: e.Title, Message: e.Message, HubURL: hubURL + "/"}
	if d.Title == "" {
		d.Title = defaultTitle(e.Status)
	}
	if e.LoginURL != "" {
		d.ActionURL, d.ActionText = e.LoginURL, "Sign in"
	}
	return d
}

// writePage renders the built-in HTML page.
func (s *Server) writePage(w http.ResponseWriter, r *http.Request, status int, d pageData) {
	if d.RequestID == "" && status >= 500 {
		d.RequestID = info(r).requestID
	}
	var buf bytes.Buffer
	if err := pageTmpl.Execute(&buf, d); err != nil {
		s.log.ErrorContext(r.Context(), "render page", "error", err)
		buf.Reset()
		buf.WriteString(http.StatusText(status))
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(buf.Len()))
	if h.Get("Cache-Control") == "" {
		h.Set("Cache-Control", "no-store")
	}
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(buf.Bytes())
	}
}

// siteNotFound is the built-in page for a site without any files.
func siteNotFound(site string) *apiError {
	e := errNotFound("There is no site named %q yet. Deploy a folder to create it, or browse the existing sites on the hub.", site)
	e.Title = "Site not found"
	return e
}

// pageNotFound is the built-in 404 page of an existing site.
func pageNotFound() *apiError {
	e := errNotFound("The page you are looking for does not exist on this site.")
	e.Title = "Page not found"
	return e
}
