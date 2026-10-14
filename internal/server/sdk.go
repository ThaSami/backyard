package server

import (
	"encoding/json"
	"net/http"

	"github.com/thasami/backyard/skills"
	"github.com/thasami/backyard/web"
)

// Embedded assets, accessed through variables so tests can substitute them.
var (
	sdkClassic     = web.SDKClassic
	sdkModule      = web.SDKModule
	sdkTypes       = web.SDKTypes
	hubFS          = web.Hub
	skillMarkdown  = skills.SkillMarkdown
	skillReference = skills.Reference
	llmsTxt        = skills.LLMSTxt
	llmsFullTxt    = skills.LLMSFullTxt
)

// sdkContext is the per-request SDK preamble (SPEC §2.1).
type sdkContext struct {
	V      int     `json:"v"`
	Mode   string  `json:"mode"`
	Site   *string `json:"site"`
	Prefix string  `json:"prefix"`
	Hub    string  `json:"hub"`
}

// sdkPreamble renders `globalThis.__BACKYARD__ = {...};\n` for site ("" =
// hub / no site).
func (s *Server) sdkPreamble(site string) []byte {
	ctx := sdkContext{V: 1, Mode: "host", Prefix: sitePathPfx, Hub: s.publicURL}
	if s.pathMode {
		ctx.Mode = "path"
	}
	if site != "" {
		ctx.Site = &site
	}
	b, err := json.Marshal(ctx)
	if err != nil {
		b = []byte("{}")
	}
	out := make([]byte, 0, len(b)+32)
	out = append(out, "globalThis.__BACKYARD__ = "...)
	out = append(out, b...)
	out = append(out, ";\n"...)
	return out
}

// serveSDK serves /__backyard/backyard.{js,mjs,d.ts}. The SDK holds no
// secrets and is served without authentication.
func (s *Server) serveSDK(w http.ResponseWriter, r *http.Request, site string) {
	if !s.requireMethods(w, r, http.MethodGet) {
		return
	}
	var (
		body []byte
		ct   = "text/javascript; charset=utf-8"
		pre  []byte
	)
	switch r.URL.Path {
	case sdkJSPath:
		body, pre = sdkClassic(), s.sdkPreamble(site)
	case sdkMJSPath:
		body, pre = sdkModule(), s.sdkPreamble(site)
	default:
		body, ct = sdkTypes(), "text/plain; charset=utf-8"
	}
	data := make([]byte, 0, len(pre)+len(body))
	data = append(data, pre...)
	data = append(data, body...)
	name := r.URL.Path[len(reservedPfx):]
	s.serveObject(w, r, &object{
		name:        name,
		size:        int64(len(data)),
		etag:        contentETag(data),
		contentType: ct,
		data:        data,
	}, serveOptions{cacheControl: "no-cache"})
}
