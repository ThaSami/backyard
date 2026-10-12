package blob

import (
	"path"
	"strings"
)

// contentTypes is a fixed table so behaviour does not depend on the host's
// /etc/mime.types. Text types carry an explicit UTF-8 charset.
var contentTypes = map[string]string{
	".html":        "text/html; charset=utf-8",
	".htm":         "text/html; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".js":          "text/javascript; charset=utf-8",
	".mjs":         "text/javascript; charset=utf-8",
	".cjs":         "text/javascript; charset=utf-8",
	".json":        "application/json",
	".map":         "application/json",
	".webmanifest": "application/manifest+json",
	".txt":         "text/plain; charset=utf-8",
	".md":          "text/markdown; charset=utf-8",
	".csv":         "text/csv; charset=utf-8",
	".tsv":         "text/tab-separated-values; charset=utf-8",
	".xml":         "application/xml",
	".svg":         "image/svg+xml",
	".png":         "image/png",
	".jpg":         "image/jpeg",
	".jpeg":        "image/jpeg",
	".gif":         "image/gif",
	".webp":        "image/webp",
	".avif":        "image/avif",
	".ico":         "image/x-icon",
	".bmp":         "image/bmp",
	".woff":        "font/woff",
	".woff2":       "font/woff2",
	".ttf":         "font/ttf",
	".otf":         "font/otf",
	".mp4":         "video/mp4",
	".webm":        "video/webm",
	".mov":         "video/quicktime",
	".mp3":         "audio/mpeg",
	".wav":         "audio/wav",
	".ogg":         "audio/ogg",
	".m4a":         "audio/mp4",
	".pdf":         "application/pdf",
	".zip":         "application/zip",
	".gz":          "application/gzip",
	".wasm":        "application/wasm",
	".glb":         "model/gltf-binary",
	".gltf":        "model/gltf+json",
	".ts":          "text/plain; charset=utf-8", // TypeScript source, not MPEG-TS
	".tsx":         "text/plain; charset=utf-8",
	".jsx":         "text/javascript; charset=utf-8",
	".yaml":        "text/yaml; charset=utf-8",
	".yml":         "text/yaml; charset=utf-8",
	".ics":         "text/calendar; charset=utf-8",
}

// ContentTypeByExt returns the content type for a file name's extension, or "".
func ContentTypeByExt(name string) string {
	return contentTypes[strings.ToLower(path.Ext(name))]
}
