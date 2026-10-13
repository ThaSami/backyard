package server

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"testing"

	"github.com/thasami/backyard/internal/config"
)

const filesURL = "https://foo.backyard.test/__backyard/api/files"

type part struct {
	field, filename, ctype, content string
}

func multipartBody(t *testing.T, parts ...part) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range parts {
		hdr := textproto.MIMEHeader{}
		disp := fmt.Sprintf(`form-data; name=%q`, p.field)
		if p.filename != "" {
			disp += fmt.Sprintf(`; filename=%q`, p.filename)
		}
		hdr.Set("Content-Disposition", disp)
		if p.ctype != "" {
			hdr.Set("Content-Type", p.ctype)
		}
		w, err := mw.CreatePart(hdr)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(w, p.content)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, mw.FormDataContentType()
}

func (h *harness) upload(url string, parts ...part) *httpResponse {
	h.t.Helper()
	body, ct := multipartBody(h.t, parts...)
	w := h.serve(req{method: "POST", url: url, body: body, hdr: map[string]string{"X-Backyard": "1", "Content-Type": ct}})
	return &httpResponse{w.Code, w.Header(), w.Body.Bytes()}
}

type httpResponse struct {
	code   int
	header http.Header
	body   []byte
}

type fileJSON struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	URL         string `json:"url"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
	CreatedAt   string `json:"created_at"`
	CreatedBy   string `json:"created_by"`
}

func TestFilesLifecycle(t *testing.T) {
	h := newHarness(t)
	res := h.upload(filesURL, part{field: "file", filename: "hello world.txt", ctype: "text/plain", content: "hello files"})
	if res.code != http.StatusCreated {
		t.Fatalf("upload = %d %s", res.code, res.body)
	}
	var fi fileJSON
	mustJSON(t, res.body, &fi)
	if len(fi.ID) != 26 || fi.Name != "hello world.txt" || fi.Size != 11 || fi.ContentType != "text/plain" ||
		fi.CreatedBy != "alice@acme.com" || fi.CreatedAt == "" {
		t.Fatalf("FileInfo = %+v", fi)
	}
	if want := "/__backyard/files/" + fi.ID + "/hello%20world.txt"; fi.URL != want {
		t.Fatalf("url = %q, want %q", fi.URL, want)
	}
	key := "uploads/foo/" + fi.ID + "/hello world.txt"
	if string(h.blob.bytesOf(key)) != "hello files" {
		t.Fatalf("blob keys = %v", h.blob.keys())
	}

	// Serve the bytes.
	w := h.get("https://foo.backyard.test" + fi.URL)
	expectStatus(t, w, 200)
	if w.Body.String() != "hello files" {
		t.Fatalf("body = %q", w.Body.String())
	}
	hd := w.Header()
	if hd.Get("Content-Type") != "text/plain; charset=utf-8" || hd.Get("X-Content-Type-Options") != "nosniff" ||
		hd.Get("Content-Disposition") != `inline; filename="hello world.txt"` || hd.Get("ETag") == "" {
		t.Fatalf("headers = %v", hd)
	}
	if hd.Get("Content-Security-Policy") != "" {
		t.Fatal("text/plain must not be sandboxed")
	}
	w = h.get("https://foo.backyard.test"+fi.URL, "Range", "bytes=6-")
	expectStatus(t, w, 206)
	if w.Body.String() != "files" {
		t.Fatalf("range = %q", w.Body.String())
	}
	w = h.get("https://foo.backyard.test"+fi.URL, "If-None-Match", hd.Get("ETag"))
	expectStatus(t, w, 304)
	// The name segment is cosmetic; the id selects the file.
	w = h.get("https://foo.backyard.test/__backyard/files/" + fi.ID + "/other.bin")
	expectStatus(t, w, 200)
	// Another site cannot read it.
	w = h.get("https://bar.backyard.test" + fi.URL)
	expectStatus(t, w, 404)

	// Metadata API.
	w = h.api("GET", filesURL+"/"+fi.ID, nil)
	expectStatus(t, w, 200)
	var got fileJSON
	mustJSON(t, w.Body.Bytes(), &got)
	if got != fi {
		t.Fatalf("get = %+v, want %+v", got, fi)
	}
	w = h.api("GET", filesURL+"/01NOPE", nil)
	expectError(t, w, 404, "not_found")

	// Delete removes bytes and metadata.
	w = h.api("DELETE", filesURL+"/"+fi.ID, nil)
	expectStatus(t, w, http.StatusNoContent)
	if h.blob.has(key) {
		t.Fatal("blob not deleted")
	}
	w = h.get("https://foo.backyard.test" + fi.URL)
	expectStatus(t, w, 404)
	w = h.api("DELETE", filesURL+"/"+fi.ID, nil)
	expectStatus(t, w, http.StatusNoContent)
	// _files is never visible through the db API.
	w = h.api("GET", dbURL, nil)
	if strings.Contains(w.Body.String(), "_files") {
		t.Fatalf("collections = %s", w.Body.String())
	}
}

func mustJSON(t *testing.T, b []byte, v any) {
	t.Helper()
	if err := jsonUnmarshal(b, v); err != nil {
		t.Fatalf("json %q: %v", b, err)
	}
}

func TestFilesUploadVariants(t *testing.T) {
	h := newHarness(t, withConfig(func(c *config.Config) { c.Files.MaxUploadBytes = 64 }))
	tests := []struct {
		name   string
		parts  []part
		status int
		check  func(t *testing.T, fi fileJSON)
	}{
		{"name before file", []part{{field: "name", content: "custom.png"}, {field: "file", filename: "x.bin", content: "\x89PNG\r\n\x1a\n"}}, 201,
			func(t *testing.T, fi fileJSON) {
				if fi.Name != "custom.png" || fi.ContentType != "image/png" {
					t.Fatalf("fi = %+v", fi)
				}
			}},
		{"name after file", []part{{field: "file", filename: "orig.txt", content: "abc"}, {field: "name", content: "renamed.txt"}}, 201,
			func(t *testing.T, fi fileJSON) {
				if fi.Name != "renamed.txt" || !strings.HasSuffix(fi.URL, "/renamed.txt") {
					t.Fatalf("fi = %+v", fi)
				}
			}},
		{"path traversal name", []part{{field: "file", filename: "../../etc/passwd", content: "x"}}, 201,
			func(t *testing.T, fi fileJSON) {
				if fi.Name != "passwd" {
					t.Fatalf("name = %q", fi.Name)
				}
			}},
		{"windows path", []part{{field: "file", filename: `C:\Users\a\pic.jpg`, content: "x"}}, 201,
			func(t *testing.T, fi fileJSON) {
				if fi.Name != "pic.jpg" || fi.ContentType != "image/jpeg" {
					t.Fatalf("fi = %+v", fi)
				}
			}},
		{"sniffed type", []part{{field: "file", filename: "noext", ctype: "application/octet-stream", content: "%PDF-1.4 hello"}}, 201,
			func(t *testing.T, fi fileJSON) {
				if fi.ContentType != "application/pdf" {
					t.Fatalf("content type = %q", fi.ContentType)
				}
			}},
		{"unicode name", []part{{field: "file", filename: "résumé ✓.txt", content: "x"}}, 201,
			func(t *testing.T, fi fileJSON) {
				if fi.Name != "résumé ✓.txt" || !strings.Contains(fi.URL, "r%C3%A9sum%C3%A9") {
					t.Fatalf("fi = %+v", fi)
				}
			}},
		{"too large", []part{{field: "file", filename: "big.txt", content: strings.Repeat("x", 65)}}, 413, nil},
		{"exactly max", []part{{field: "file", filename: "max.txt", content: strings.Repeat("x", 64)}}, 201, nil},
		{"missing file", []part{{field: "name", content: "x"}}, 400, nil},
		{"two files", []part{{field: "file", filename: "a", content: "a"}, {field: "file", filename: "b", content: "b"}}, 400, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := len(h.blob.keys())
			res := h.upload(filesURL, tc.parts...)
			if res.code != tc.status {
				t.Fatalf("status = %d, want %d: %s", res.code, tc.status, res.body)
			}
			if tc.status != 201 {
				if len(h.blob.keys()) != before {
					t.Fatalf("failed upload left blobs: %v", h.blob.keys())
				}
				return
			}
			var fi fileJSON
			mustJSON(t, res.body, &fi)
			if tc.check != nil {
				tc.check(t, fi)
			}
		})
	}
	// Not multipart.
	w := h.api("POST", filesURL, map[string]any{"file": "x"})
	expectError(t, w, 400, "bad_request")
}

func TestFilesListNewestFirst(t *testing.T) {
	h := newHarness(t)
	var ids []string
	for i := range 3 {
		res := h.upload(filesURL, part{field: "file", filename: fmt.Sprintf("f%d.txt", i), content: "x"})
		var fi fileJSON
		mustJSON(t, res.body, &fi)
		ids = append(ids, fi.ID)
	}
	w := h.api("GET", filesURL, nil)
	expectStatus(t, w, 200)
	var list struct {
		Files   []fileJSON `json:"files"`
		HasMore bool       `json:"has_more"`
	}
	mustJSON(t, w.Body.Bytes(), &list)
	if len(list.Files) != 3 || list.Files[0].ID != ids[2] || list.Files[2].ID != ids[0] {
		t.Fatalf("list = %+v", list.Files)
	}
	w = h.api("GET", filesURL+"?limit=1&offset=1", nil)
	mustJSON(t, w.Body.Bytes(), &list)
	if len(list.Files) != 1 || list.Files[0].ID != ids[1] || !list.HasMore {
		t.Fatalf("paged = %+v has_more=%v", list.Files, list.HasMore)
	}
	w = h.api("GET", "https://empty.backyard.test/__backyard/api/files", nil)
	if strings.TrimSpace(w.Body.String()) != `{"files":[],"has_more":false}` {
		t.Fatalf("empty list = %s", w.Body.String())
	}
}

func TestFilesActiveContentSandboxed(t *testing.T) {
	h := newHarness(t)
	res := h.upload(filesURL, part{field: "file", filename: "x.html", content: "<script>alert(1)</script>"})
	var fi fileJSON
	mustJSON(t, res.body, &fi)
	w := h.get("https://foo.backyard.test" + fi.URL)
	expectStatus(t, w, 200)
	if w.Header().Get("Content-Security-Policy") != "sandbox" {
		t.Fatalf("html upload not sandboxed: %v", w.Header())
	}
	res = h.upload(filesURL, part{field: "file", filename: "x.svg", content: "<svg/>"})
	mustJSON(t, res.body, &fi)
	w = h.get("https://foo.backyard.test" + fi.URL)
	if w.Header().Get("Content-Security-Policy") != "sandbox" {
		t.Fatalf("svg upload not sandboxed: %v", w.Header())
	}
}

func TestFilesPathModeURL(t *testing.T) {
	h := newHarness(t, pathMode())
	body, ct := multipartBody(t, part{field: "file", filename: "a.txt", content: "abc"})
	w := h.serve(req{method: "POST", url: "https://backyard.test/s/foo/__backyard/api/files", body: body,
		hdr: map[string]string{"X-Backyard": "1", "Content-Type": ct, "Origin": "https://backyard.test"}})
	expectStatus(t, w, 201)
	var fi fileJSON
	mustJSON(t, w.Body.Bytes(), &fi)
	if !strings.HasPrefix(fi.URL, "/s/foo/__backyard/files/"+fi.ID+"/") {
		t.Fatalf("url = %q", fi.URL)
	}
	w = h.get("https://backyard.test" + fi.URL)
	expectStatus(t, w, 200)
	if w.Body.String() != "abc" {
		t.Fatalf("body = %q", w.Body.String())
	}
}

func TestFilesUploadRateLimit(t *testing.T) {
	h := newHarness(t, withConfig(func(c *config.Config) { c.RateLimit.UploadsPerMinute = 1 }))
	if res := h.upload(filesURL, part{field: "file", filename: "a", content: "a"}); res.code != 201 {
		t.Fatalf("first upload = %d", res.code)
	}
	res := h.upload(filesURL, part{field: "file", filename: "b", content: "b"})
	if res.code != http.StatusTooManyRequests || res.header.Get("Retry-After") == "" {
		t.Fatalf("second upload = %d %v", res.code, res.header)
	}
}

func TestSanitizeFileName(t *testing.T) {
	tests := []struct{ in, want string }{
		{"a.txt", "a.txt"},
		{"../../x", "x"},
		{`..\..\y.png`, "y.png"},
		{"..", "file"},
		{".", "file"},
		{"", "file"},
		{"  spaced  .txt ", "spaced  .txt"},
		{"a\x00b\nc.txt", "a_b_c.txt"},
		{`quo"te<>.txt`, "quo_te__.txt"},
		{"...hidden", "hidden"},
		{"\u202egnp.exe", "_gnp.exe"},
		{strings.Repeat("a", 300) + ".txt", strings.Repeat("a", 196) + ".txt"},
		{strings.Repeat("é", 150), strings.Repeat("é", 100)},
	}
	for _, tc := range tests {
		if got := sanitizeFileName(tc.in); got != tc.want {
			t.Errorf("sanitizeFileName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
