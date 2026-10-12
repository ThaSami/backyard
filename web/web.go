// Package web embeds the browser assets: the client SDK (web/sdk/) served at
// /__backyard/backyard.{js,mjs,d.ts} on every host and the hub UI (web/hub/).
// The files are complete and standalone so the Cloudflare Worker can serve
// identical bytes; the server prepends the per-request preamble (SPEC §2.1).
package web

import "io/fs"

// STUBS: replaced by the web implementation.

// SDKClassic returns backyard.js (classic script defining window.backyard).
func SDKClassic() []byte { panic("web: not implemented") }

// SDKModule returns backyard.mjs (ES module; default export + named exports).
func SDKModule() []byte { panic("web: not implemented") }

// SDKTypes returns backyard.d.ts.
func SDKTypes() []byte { panic("web: not implemented") }

// Hub returns the hub UI files (index.html at the root, plus assets). The
// server serves them at "/" on the hub host; "/docs" serves docs.html.
func Hub() fs.FS { panic("web: not implemented") }
