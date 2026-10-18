package sites

import (
	"slices"
	"strings"
	"testing"
)

func TestPreviewConstants(t *testing.T) {
	// SPEC §7.2 and the Cloudflare edition (src/preview.ts) hold the same
	// values; docs/ pins them against the spec.
	wantNames := []string{"preview.png", "preview.jpg", "preview.jpeg", "preview.webp", "preview.avif", "preview.gif", "preview.svg"}
	if !slices.Equal(PreviewNames, wantNames) {
		t.Errorf("PreviewNames = %v", PreviewNames)
	}
	if MaxPreviewBytes != 1048576 {
		t.Errorf("MaxPreviewBytes = %d", MaxPreviewBytes)
	}
	if PreviewCSP != "sandbox; default-src 'none'; img-src data:; style-src 'unsafe-inline'" {
		t.Errorf("PreviewCSP = %q", PreviewCSP)
	}
	wantTypes := map[string]string{
		"png": "image/png", "jpg": "image/jpeg", "jpeg": "image/jpeg", "gif": "image/gif",
		"webp": "image/webp", "avif": "image/avif", "svg": "image/svg+xml",
	}
	if len(PreviewTypes) != len(wantTypes) {
		t.Errorf("PreviewTypes = %v", PreviewTypes)
	}
	for ext, ct := range wantTypes {
		if PreviewTypes[ext] != ct {
			t.Errorf("PreviewTypes[%q] = %q, want %q", ext, PreviewTypes[ext], ct)
		}
	}
	// Every fallback name has a servable type.
	for _, n := range PreviewNames {
		if PreviewType(n) == "" {
			t.Errorf("PreviewType(%q) is empty", n)
		}
	}
}

func TestPreviewType(t *testing.T) {
	tests := map[string]string{
		"preview.png":   "image/png",
		"img/Cover.JPG": "image/jpeg",
		"a.jpeg":        "image/jpeg",
		"a.Svg":         "image/svg+xml",
		"a.webp":        "image/webp",
		"a.avif":        "image/avif",
		"a.gif":         "image/gif",
		"a.svgz":        "",
		"a.html":        "",
		"a.ico":         "",
		"a.bmp":         "",
		"png":           "",
		"a.png/b":       "",
		"a.png.html":    "",
		"dir.png/cover": "",
		"a.":            "",
	}
	for p, want := range tests {
		if got := PreviewType(p); got != want {
			t.Errorf("PreviewType(%q) = %q, want %q", p, got, want)
		}
	}
}

func TestResolvePreviewRef(t *testing.T) {
	ok := []struct{ ref, want string }{
		{"preview.png", "preview.png"},
		{"img/cover.png", "img/cover.png"},
		{"/img/cover.png", "img/cover.png"},
		{"./img/cover.png", "img/cover.png"},
		{"././a.png", "a.png"},
		{"/./a.png", "a.png"},
		{"/././a.png", "a.png"},
		{"a.png?v=2", "a.png"},
		{"a.png#frag", "a.png"},
		{"a.png#x?y", "a.png"},
		{"a.png?x#y", "a.png"},
		{"img/my%20cover.png", "img/my cover.png"},
		{"%69mg/a.png", "img/a.png"},
		{"img%2Fa.png", "img/a.png"},
		{"caf%C3%A9.png", "café.png"},
		{"café.png", "café.png"},
		{"a b.png", "a b.png"},
		{"a+b.png", "a+b.png"},
		{"img/a:b.png", "img/a:b.png"}, // a "/" before the colon: no scheme
		{"1a:b.png", "1a:b.png"},       // a scheme starts with a letter
		{"a_b:c.png", "a_b:c.png"},     // "_" is not a scheme character
		{"-a:b.png", "-a:b.png"},
		{"a.png%3Fv=1", "a.png?v=1"}, // an escaped "?" is part of the name
		{"x..png", "x..png"},
		{"a/b./c.png", "a/b./c.png"},
		{"__backyard/x.png", "__backyard/x.png"}, // not a site file; ChoosePreview drops it
		{strings.Repeat("a", 1020) + ".png", strings.Repeat("a", 1020) + ".png"},
		{"Preview.PNG", "Preview.PNG"},
		{"preview", "preview"}, // the extension is ChoosePreview's check
	}
	for _, tc := range ok {
		got, valid := ResolvePreviewRef(tc.ref)
		if !valid || got != tc.want {
			t.Errorf("ResolvePreviewRef(%q) = (%q, %v), want (%q, true)", tc.ref, got, valid, tc.want)
		}
	}
	bad := []struct{ why, ref string }{
		{"empty", ""},
		{"longer than 1024 bytes", strings.Repeat("a", 1021) + ".png"},
		{"scheme https", "https://example.com/a.png"},
		{"scheme http upper", "HTTP://example.com/a.png"},
		{"scheme data", "data:image/png;base64,AAAA"},
		{"scheme javascript", "javascript:alert(1)"},
		{"scheme with digits and +.-", "a1+.-b:x.png"},
		{"scheme drive letter", "c:/a.png"},
		{"scheme single letter", "x:a.png"},
		{"scheme without slashes", "blob:a.png"},
		{"scheme-like file name", "a:b.png"},
		{"scheme-like file name with dot", "cover.png:x"},
		{"protocol-relative", "//cdn.example.com/a.png"},
		{"protocol-relative after nothing else", "///a.png"},
		{"backslash", `img\a.png`},
		{"backslash leading", `\a.png`},
		{"backslash after query", `a.png?x\y`},
		{"query only", "?v=1"},
		{"fragment only", "#x"},
		{"root only", "/"},
		{"dot slash only", "./"},
		{"dot slash then query", "./?x"},
		{"invalid escape", "a%2.png"},
		{"invalid escape at end", "a.png%"},
		{"invalid escape non-hex", "a%zz.png"},
		{"escaped NUL", "a%00.png"},
		{"escaped newline", "a%0A.png"},
		{"escaped DEL", "a%7F.png"},
		{"raw tab", "a\t.png"},
		{"raw control", "a\x01.png"},
		{"raw DEL", "a\x7f.png"},
		{"escaped invalid UTF-8", "a%FF.png"},
		{"empty segment", "img//a.png"},
		{"empty segment after decoding", "img%2F%2Fa.png"},
		{"trailing slash", "img/"},
		{"leading slash after one removed", "/.//a.png"},
		{"dot segment", "img/./a.png"},
		{"dotdot segment", "../a.png"},
		{"dotdot inside", "img/../a.png"},
		{"dotdot after dot slash", "./../a.png"},
		{"escaped dotdot", "%2E%2E/a.png"},
		{"escaped slash dotdot", "img%2F..%2Fa.png"},
		{"dotfile", ".hidden.png"},
		{"dot directory", ".well-known/a.png"},
		{"dot directory nested", "img/.cache/a.png"},
		{"escaped leading dot", "%2Ehidden.png"},
		{"lone dot", "."},
		{"lone dotdot", ".."},
		{"two leading slashes after root removal", "/%2Fa.png"},
	}
	for _, tc := range bad {
		if got, valid := ResolvePreviewRef(tc.ref); valid {
			t.Errorf("%s: ResolvePreviewRef(%q) = %q, want rejection", tc.why, tc.ref, got)
		}
	}
	// The length bound is on the reference as written (1024 bytes is fine).
	if _, valid := ResolvePreviewRef(strings.Repeat("a", 1020) + ".png"); !valid {
		t.Error("1024-byte reference rejected")
	}
	if _, valid := ResolvePreviewRef("/" + strings.Repeat("a", 1020) + ".png"); valid {
		t.Error("1025-byte reference accepted")
	}
}

func TestChoosePreview(t *testing.T) {
	files := func(kv ...any) []ManifestFile {
		var out []ManifestFile
		for i := 0; i+1 < len(kv); i += 2 {
			out = append(out, ManifestFile{Path: kv[i].(string), Size: int64(kv[i+1].(int))})
		}
		return out
	}
	const max = MaxPreviewBytes
	tests := []struct {
		name  string
		og    string
		files []ManifestFile
		want  string
	}{
		{"nothing", "", files("index.html", 10), ""},
		{"no files", "a.png", nil, ""},
		{"og relative", "img/cover.png", files("index.html", 10, "img/cover.png", 5), "img/cover.png"},
		{"og root relative", "/img/cover.png", files("img/cover.png", 5), "img/cover.png"},
		{"og dot slash", "./cover.webp", files("cover.webp", 5), "cover.webp"},
		{"og percent-decoded", "my%20cover.svg", files("my cover.svg", 5), "my cover.svg"},
		{"og with query", "cover.png?v=3", files("cover.png", 5), "cover.png"},
		{"og beats preview.png", "cover.jpg", files("cover.jpg", 5, "preview.png", 5), "cover.jpg"},
		{"og case of extension", "Cover.JPEG", files("Cover.JPEG", 5, "preview.png", 5), "Cover.JPEG"},
		{"og path is case-sensitive", "cover.png", files("Cover.png", 5, "preview.svg", 5), "preview.svg"},
		{"og at the size limit", "cover.png", files("cover.png", max), "cover.png"},
		{"og too large", "cover.png", files("cover.png", max+1, "preview.png", 5), "preview.png"},
		{"og missing file", "cover.png", files("preview.gif", 5), "preview.gif"},
		{"og unsupported extension", "cover.bmp", files("cover.bmp", 5, "preview.png", 5), "preview.png"},
		{"og no extension", "cover", files("cover", 5), ""},
		{"og absolute URL", "https://example.com/preview.png", files("preview.png", 5), "preview.png"},
		{"og absolute URL to own path", "https://site.example.com/cover.png", files("cover.png", 5), ""},
		{"og protocol-relative", "//example.com/cover.png", files("cover.png", 5), ""},
		{"og dotdot", "../cover.png", files("cover.png", 5), ""},
		{"og dot segment", "img/./cover.png", files("img/cover.png", 5), ""},
		{"og dotfile", ".cover.png", files(".cover.png", 5, "preview.png", 1), "preview.png"},
		{"og directory", "img", files("img/a.png", 5), ""},
		{"fallback order png", "", files("preview.svg", 1, "preview.gif", 1, "preview.avif", 1, "preview.webp", 1, "preview.jpeg", 1, "preview.jpg", 1, "preview.png", 1), "preview.png"},
		{"fallback order jpg", "", files("preview.svg", 1, "preview.gif", 1, "preview.avif", 1, "preview.webp", 1, "preview.jpeg", 1, "preview.jpg", 1), "preview.jpg"},
		{"fallback order jpeg", "", files("preview.svg", 1, "preview.gif", 1, "preview.avif", 1, "preview.webp", 1, "preview.jpeg", 1), "preview.jpeg"},
		{"fallback order webp", "", files("preview.svg", 1, "preview.gif", 1, "preview.avif", 1, "preview.webp", 1), "preview.webp"},
		{"fallback order avif", "", files("preview.svg", 1, "preview.gif", 1, "preview.avif", 1), "preview.avif"},
		{"fallback order gif", "", files("preview.svg", 1, "preview.gif", 1), "preview.gif"},
		{"fallback svg", "", files("preview.svg", 1), "preview.svg"},
		{"fallback skips too large", "", files("preview.png", max+1, "preview.svg", max), "preview.svg"},
		{"fallback all too large", "", files("preview.png", max+1, "preview.svg", max+1), ""},
		{"fallback empty file", "", files("preview.png", 0), "preview.png"},
		{"fallback exact lowercase", "", files("Preview.png", 1, "PREVIEW.SVG", 1), ""},
		{"fallback only at the root", "", files("img/preview.png", 1), ""},
		{"fallback other names ignored", "", files("preview.ico", 1, "preview.bmp", 1, "thumbnail.png", 1), ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ChoosePreview(tc.og, tc.files); got != tc.want {
				t.Errorf("ChoosePreview(%q, %v) = %q, want %q", tc.og, tc.files, got, tc.want)
			}
		})
	}
}

func FuzzResolvePreviewRef(f *testing.F) {
	for _, s := range []string{"a.png", "/a.png", "./a.png", "%2E%2E/a", "https://x/a.png", "//x", `a\b`, "a?b#c", "%"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, ref string) {
		p, ok := ResolvePreviewRef(ref)
		if !ok {
			return
		}
		// Whatever is accepted is a well-formed relative site path.
		if p == "" || p[0] == '/' || strings.ContainsAny(p, "\\") {
			t.Fatalf("ResolvePreviewRef(%q) = %q", ref, p)
		}
		for seg := range strings.SplitSeq(p, "/") {
			if seg == "" || seg[0] == '.' {
				t.Fatalf("ResolvePreviewRef(%q) = %q: bad segment %q", ref, p, seg)
			}
		}
		for i := 0; i < len(p); i++ {
			if p[i] < 0x20 || p[i] == 0x7f {
				t.Fatalf("ResolvePreviewRef(%q) = %q: control character", ref, p)
			}
		}
	})
}
