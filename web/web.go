// Package web embeds the browser assets: the client SDK (web/sdk/) served at
// /__backyard/backyard.{js,mjs,d.ts} on every host and the hub UI (web/hub/).
// The files are complete and standalone so the Cloudflare Worker can serve
// identical bytes; the server prepends the per-request preamble (SPEC §2.1).
//
// The SDK source of truth is web/sdk/src/core.js; web/sdk/build.mjs generates
// backyard.js (classic script) and backyard.mjs (ES module) from it, and both
// outputs are committed. After editing core.js run:
//
//	node web/sdk/build.mjs
//	cd web && npm test
//
// npm test (Node 22, no dependencies) checks that the builds are in sync and
// runs the SDK, hub and docs unit tests. With playwright-core and Chromium
// (PLAYWRIGHT_CORE=…/playwright-core/index.mjs) it also runs the real-browser
// tests against web/test/stubserver.mjs, and with BACKYARD_E2E_URL (plus
// BACKYARD_E2E_TOKEN when deploys need a token) web/test/e2e.test.mjs deploys
// a site through the hub of a real server (Go or the Cloudflare Worker) and
// exercises the SDK end to end.
package web

import (
	"embed"
	"fmt"
	"io/fs"
)

var (
	//go:embed sdk/backyard.js
	sdkClassic []byte

	//go:embed sdk/backyard.mjs
	sdkModule []byte

	//go:embed sdk/backyard.d.ts
	sdkTypes []byte

	// hubFiles holds web/hub/** (files starting with "." or "_" are excluded).
	//
	//go:embed hub
	hubFiles embed.FS

	hubFS = mustSub(hubFiles, "hub")
)

// SDKClassic returns backyard.js (classic script defining window.backyard).
// The returned slice is shared: callers must not modify it.
func SDKClassic() []byte { return sdkClassic }

// SDKModule returns backyard.mjs (ES module; default export + named exports).
// The returned slice is shared: callers must not modify it.
func SDKModule() []byte { return sdkModule }

// SDKTypes returns backyard.d.ts. The returned slice is shared: callers must
// not modify it.
func SDKTypes() []byte { return sdkTypes }

// Hub returns the hub UI files (index.html at the root, plus assets). The
// server serves them at "/" on the hub host; "/docs" serves docs.html.
func Hub() fs.FS { return hubFS }

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		// Unreachable: dir is a constant embedded directory.
		panic(fmt.Sprintf("web: embedded %s: %v", dir, err))
	}
	return sub
}
