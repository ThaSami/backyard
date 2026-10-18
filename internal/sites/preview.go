package sites

import (
	"net/url"
	"path"
	"strings"
	"unicode/utf8"
)

// A site's preview (SiteMeta.preview, SPEC §7.2) is an image the site ships
// itself, chosen at every write like the title: the og:image of the root
// index.html when it names a usable file of the site, else the first
// PreviewNames file. The hub shows it through
// GET /__backyard/api/sites/{site}/preview. The Cloudflare edition's
// src/preview.ts holds the same values and rules.

// PreviewNames are the root files tried, in order, when the og:image of
// index.html is missing or unusable.
var PreviewNames = []string{
	"preview.png", "preview.jpg", "preview.jpeg", "preview.webp",
	"preview.avif", "preview.gif", "preview.svg",
}

// MaxPreviewBytes is the largest file that can be a preview.
const MaxPreviewBytes = 1 << 20

// PreviewCSP is the Content-Security-Policy of a served preview: an SVG on
// the hub's origin must never run script or load anything, and a
// navigation to it stays sandboxed.
const PreviewCSP = "sandbox; default-src 'none'; img-src data:; style-src 'unsafe-inline'"

// PreviewTypes maps the (lowercased) extensions a preview may have, without
// the dot, to the Content-Type it is served with.
var PreviewTypes = map[string]string{
	"png":  "image/png",
	"jpg":  "image/jpeg",
	"jpeg": "image/jpeg",
	"gif":  "image/gif",
	"webp": "image/webp",
	"avif": "image/avif",
	"svg":  "image/svg+xml",
}

// maxPreviewRef bounds the length of an og:image reference.
const maxPreviewRef = 1024

// PreviewType returns the Content-Type of a preview path by its extension,
// or "" when the extension is not one of PreviewTypes.
func PreviewType(p string) string {
	ext := strings.TrimPrefix(path.Ext(p), ".")
	if ext == "" {
		return ""
	}
	return PreviewTypes[strings.ToLower(ext)]
}

// ResolvePreviewRef turns the og:image reference of a site's root
// index.html into a site path. It accepts only references to the site's
// own files: relative ("img/a.png", "./a.png") or site-root relative
// ("/a.png"), without a query or fragment. It refuses references that are
// empty or longer than 1024 bytes, have a URL scheme, start with "//" or
// contain "\", have an invalid percent escape, or decode to something
// with a control character, invalid UTF-8, or an empty, ".", ".." or
// dot-prefixed segment.
func ResolvePreviewRef(ref string) (string, bool) {
	if ref == "" || len(ref) > maxPreviewRef || hasScheme(ref) ||
		strings.HasPrefix(ref, "//") || strings.ContainsRune(ref, '\\') {
		return "", false
	}
	if i := strings.IndexAny(ref, "?#"); i >= 0 {
		ref = ref[:i]
	}
	ref = strings.TrimPrefix(ref, "/")
	for strings.HasPrefix(ref, "./") {
		ref = ref[2:]
	}
	p, err := url.PathUnescape(ref)
	if err != nil || p == "" || !utf8.ValidString(p) {
		return "", false
	}
	for i := 0; i < len(p); i++ {
		if c := p[i]; c < 0x20 || c == 0x7f {
			return "", false
		}
	}
	for seg := range strings.SplitSeq(p, "/") {
		if seg == "" || seg[0] == '.' { // also "." and ".."
			return "", false
		}
	}
	return p, true
}

// hasScheme reports whether s starts with a URL scheme
// (^[A-Za-z][A-Za-z0-9+.-]*:).
func hasScheme(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z':
		case i > 0 && ('0' <= c && c <= '9' || c == '+' || c == '.' || c == '-'):
		case i > 0 && c == ':':
			return true
		default:
			return false
		}
	}
	return false
}

// ChoosePreview returns the preview of a site that holds files (its
// manifest entries after a write): ogImage, the og:image reference of its
// root index.html ("" when none), when it resolves to one of files with a
// PreviewTypes extension and at most MaxPreviewBytes; else the first of
// PreviewNames among files within that size; else "".
func ChoosePreview(ogImage string, files []ManifestFile) string {
	sizes := make(map[string]int64, len(files))
	for _, f := range files {
		sizes[f.Path] = f.Size
	}
	usable := func(p string) bool {
		n, ok := sizes[p]
		return ok && n <= MaxPreviewBytes
	}
	if p, ok := ResolvePreviewRef(ogImage); ok && PreviewType(p) != "" && usable(p) {
		return p
	}
	for _, name := range PreviewNames {
		if usable(name) {
			return name
		}
	}
	return ""
}
