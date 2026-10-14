package server

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/thasami/backyard/internal/auth"
)

func parsePreamble(t *testing.T, body string) map[string]any {
	t.Helper()
	line, _, ok := strings.Cut(body, "\n")
	if !ok {
		t.Fatalf("no preamble line in %q", body)
	}
	js, ok := strings.CutPrefix(line, "globalThis.__BACKYARD__ = ")
	if !ok || !strings.HasSuffix(js, ";") {
		t.Fatalf("bad preamble %q", line)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSuffix(js, ";")), &m); err != nil {
		t.Fatalf("preamble JSON: %v", err)
	}
	return m
}

func TestSDKPreamble(t *testing.T) {
	host := newHarness(t)
	path := newHarness(t, pathMode())
	tests := []struct {
		name string
		h    *harness
		url  string
		mode string
		site any
		body string
	}{
		{"host site js", host, "https://foo.backyard.test/__backyard/backyard.js", "host", "foo", "/* classic sdk */"},
		{"host site mjs", host, "https://foo.backyard.test/__backyard/backyard.mjs", "host", "foo", "export default"},
		{"host hub", host, "https://backyard.test/__backyard/backyard.js", "host", nil, "/* classic sdk */"},
		{"path site", path, "https://backyard.test/s/foo/__backyard/backyard.js", "path", "foo", "/* classic sdk */"},
		{"path root", path, "https://backyard.test/__backyard/backyard.mjs", "path", nil, "export default"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := tc.h.get(tc.url)
			expectStatus(t, w, 200)
			if ct := w.Header().Get("Content-Type"); ct != "text/javascript; charset=utf-8" {
				t.Fatalf("Content-Type = %q", ct)
			}
			if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
				t.Fatalf("Cache-Control = %q", cc)
			}
			body := w.Body.String()
			m := parsePreamble(t, body)
			want := map[string]any{"v": float64(1), "mode": tc.mode, "site": tc.site, "prefix": "/s/", "hub": "https://backyard.test"}
			for k, v := range want {
				if m[k] != v {
					t.Errorf("preamble[%s] = %v, want %v", k, m[k], v)
				}
			}
			if len(m) != len(want) {
				t.Errorf("preamble has extra keys: %v", m)
			}
			if !strings.Contains(body, tc.body) {
				t.Errorf("body lacks SDK: %q", body)
			}
		})
	}
}

func TestSDKTypesAndCaching(t *testing.T) {
	h := newHarness(t)
	w := h.get("https://foo.backyard.test/__backyard/backyard.d.ts")
	expectStatus(t, w, 200)
	if w.Body.String() != "declare const backyard: any;\n" {
		t.Fatalf("d.ts body = %q (must have no preamble)", w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type = %q", ct)
	}
	w = h.get("https://foo.backyard.test/__backyard/backyard.js")
	etag := w.Header().Get("ETag")
	w = h.get("https://foo.backyard.test/__backyard/backyard.js", "If-None-Match", etag)
	expectStatus(t, w, 304)
	// A different site gets a different preamble and ETag.
	w = h.get("https://bar.backyard.test/__backyard/backyard.js", "If-None-Match", etag)
	expectStatus(t, w, 200)
	w = h.serve(req{method: "HEAD", url: "https://foo.backyard.test/__backyard/backyard.js"})
	expectStatus(t, w, 200)
	if w.Body.Len() != 0 || w.Header().Get("Content-Length") == "" {
		t.Fatalf("HEAD: body %d, length %q", w.Body.Len(), w.Header().Get("Content-Length"))
	}
	w = h.serve(req{method: "POST", url: "https://foo.backyard.test/__backyard/backyard.js"})
	expectStatus(t, w, 405)
}

func TestSDKNeedsNoAuth(t *testing.T) {
	h := newHarness(t)
	h.auth.set(nil, auth.ErrUnauthenticated)
	w := h.get("https://foo.backyard.test/__backyard/backyard.js")
	expectStatus(t, w, 200)
	h.auth.set(nil, errors.New("boom"))
	w = h.get("https://backyard.test/__backyard/backyard.mjs")
	expectStatus(t, w, 200)
}

func TestSDKPreambleEscaping(t *testing.T) {
	h := newHarness(t)
	h.srv.publicURL = "https://backyard.test/</script><script>alert(1)</script>"
	pre := string(h.srv.sdkPreamble("foo"))
	if strings.Contains(pre, "</script>") {
		t.Fatalf("preamble not escaped: %s", pre)
	}
}
