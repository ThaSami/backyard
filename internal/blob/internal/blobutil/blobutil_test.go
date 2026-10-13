package blobutil

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/thasami/backyard/internal/blob"
)

func TestCheckKey(t *testing.T) {
	for _, tc := range []struct {
		key string
		ok  bool
	}{
		{"index.html", true},
		{"sites/foo/index.html", true},
		{"sites/foo/.well-known/x", true},
		{"a/..b/c", true},
		{"日本語/ファイル.txt", true},
		{"a b+c=d&e?f#g%h", true},
		{strings.Repeat("k", 1024), true},
		{"", false},
		{"/abs", false},
		{"a//b", false},
		{"a/./b", false},
		{"a/../b", false},
		{"..", false},
		{".", false},
		{"dir/", false},
		{"a\\b", false},
		{"a\x00b", false},
		{"a\tb", false},
		{"a\nb", false},
		{"a\x7fb", false},
		{"\xff", false},
		{strings.Repeat("k", 1025), false},
	} {
		err := CheckKey(tc.key)
		if (err == nil) != tc.ok {
			t.Errorf("CheckKey(%q) = %v, want ok=%v", tc.key, err, tc.ok)
		}
		if err != nil && !errors.Is(err, blob.ErrInvalidKey) {
			t.Errorf("CheckKey(%q) error does not wrap ErrInvalidKey: %v", tc.key, err)
		}
	}
	// Long keys are truncated in error messages.
	if err := CheckKey(strings.Repeat("x", 5000)); err == nil || len(err.Error()) > 300 {
		t.Errorf("long key error = %v", err)
	}
}

func TestCheckPrefixAndDelimiter(t *testing.T) {
	for _, tc := range []struct {
		prefix string
		ok     bool
	}{
		{"", true},
		{"sites/", true},
		{"sites/fo", true},
		{"a", true},
		{"/", false},
		{"/sites/", false},
		{"a//", false},
		{"../", false},
		{"a\nb/", false},
	} {
		if err := CheckPrefix(tc.prefix); (err == nil) != tc.ok {
			t.Errorf("CheckPrefix(%q) = %v, want ok=%v", tc.prefix, err, tc.ok)
		}
	}
	if err := CheckDeletePrefix(""); !errors.Is(err, blob.ErrInvalidKey) {
		t.Errorf("CheckDeletePrefix(\"\") = %v", err)
	}
	if err := CheckDeletePrefix("sites/foo/"); err != nil {
		t.Errorf("CheckDeletePrefix(sites/foo/) = %v", err)
	}
	for _, d := range []string{"", "/"} {
		if err := CheckDelimiter(d); err != nil {
			t.Errorf("CheckDelimiter(%q) = %v", d, err)
		}
	}
	if err := CheckDelimiter("|"); !errors.Is(err, blob.ErrNotSupported) {
		t.Errorf("CheckDelimiter(|) = %v", err)
	}
	if CheckRange(0) != nil || CheckRange(-1) == nil {
		t.Error("CheckRange")
	}
}

func TestNormPrefixAndTTL(t *testing.T) {
	for in, want := range map[string]string{"": "", "/": "", " a ": "a/", "a/b": "a/b/", "/a/b/": "a/b/"} {
		if got := NormPrefix(in); got != want {
			t.Errorf("NormPrefix(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[time.Duration]time.Duration{
		0:                   DefaultPresignTTL,
		-time.Second:        DefaultPresignTTL,
		time.Minute:         time.Minute,
		30 * 24 * time.Hour: MaxPresignTTL,
	} {
		if got := ClampTTL(in); got != want {
			t.Errorf("ClampTTL(%v) = %v, want %v", in, got, want)
		}
	}
	if got := PresignContentType("a.css", ""); got != "text/css; charset=utf-8" {
		t.Errorf("PresignContentType = %q", got)
	}
	if got := PresignContentType("a.css", "x/y"); got != "x/y" {
		t.Errorf("PresignContentType explicit = %q", got)
	}
	if got := PresignContentType("noext", ""); got != "application/octet-stream" {
		t.Errorf("PresignContentType noext = %q", got)
	}
}

func TestCheckPresignPut(t *testing.T) {
	const helloSHA = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	const emptySHA = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	spec, err := CheckPresignPut("a.css", blob.PresignPutOptions{Size: 5, SHA256: strings.ToUpper(helloSHA)})
	if err != nil {
		t.Fatal(err)
	}
	if spec.ContentType != "text/css; charset=utf-8" || spec.Size != 5 || hex.EncodeToString(spec.SHA256) != helloSHA {
		t.Errorf("spec = %+v", spec)
	}
	spec, err = CheckPresignPut("a.bin", blob.PresignPutOptions{ContentType: "x/y", Size: -1})
	if err != nil || spec.SHA256 != nil || spec.Size != -1 || spec.ContentType != "x/y" {
		t.Errorf("unknown size = %+v, %v", spec, err)
	}
	if _, err := CheckPresignPut("e", blob.PresignPutOptions{Size: 0, SHA256: emptySHA}); err != nil {
		t.Errorf("empty body: %v", err)
	}
	for name, opts := range map[string]blob.PresignPutOptions{
		"negative size":         {Size: -2},
		"short sha":             {Size: 1, SHA256: "abcd"},
		"non-hex sha":           {Size: 1, SHA256: strings.Repeat("z", 64)},
		"long sha":              {Size: 1, SHA256: helloSHA + "00"},
		"non-empty sha, size 0": {Size: 0, SHA256: helloSHA},
	} {
		if _, err := CheckPresignPut("k", opts); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestETags(t *testing.T) {
	mod := time.Unix(1700000000, 123456789)
	a := SyntheticETag(10, mod)
	if a == "" || a != SyntheticETag(10, mod) {
		t.Fatalf("SyntheticETag not stable: %q", a)
	}
	if a == SyntheticETag(11, mod) || a == SyntheticETag(10, mod.Add(time.Nanosecond)) {
		t.Error("SyntheticETag does not change with size/mtime")
	}
	for in, want := range map[string]string{`"abc"`: "abc", `W/"abc"`: "abc", "abc": "abc", ` "x-2" `: "x-2", `""`: ""} {
		if got := CleanETag(in); got != want {
			t.Errorf("CleanETag(%q) = %q, want %q", in, got, want)
		}
	}
	if got := ETagOr(`"e"`, 1, mod); got != "e" {
		t.Errorf("ETagOr = %q", got)
	}
	if got := ETagOr("", 1, mod); got != SyntheticETag(1, mod) {
		t.Errorf("ETagOr fallback = %q", got)
	}
}

func TestParseContentRangeTotal(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
		ok   bool
	}{
		{"bytes 0-9/100", 100, true},
		{"bytes 5-5/6", 6, true},
		{"bytes */100", 100, true},
		{"bytes 0-9/*", 0, false},
		{"", 0, false},
		{"bytes 0-9/-1", 0, false},
	} {
		got, ok := ParseContentRangeTotal(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("ParseContentRangeTotal(%q) = %d, %v", tc.in, got, ok)
		}
	}
}

func TestExactSize(t *testing.T) {
	for _, tc := range []struct {
		name    string
		r       io.Reader
		size    int64
		want    string
		wantErr error
	}{
		{"exact", strings.NewReader("hello"), 5, "hello", nil},
		{"exact one byte reads", iotest.OneByteReader(strings.NewReader("hello")), 5, "hello", nil},
		{"exact data+EOF", iotest.DataErrReader(strings.NewReader("hello")), 5, "hello", nil},
		{"zero", strings.NewReader(""), 0, "", nil},
		{"short", strings.NewReader("hel"), 5, "", io.ErrUnexpectedEOF},
		{"long", strings.NewReader("hello!"), 5, "", ErrSizeMismatch},
		{"zero but data", strings.NewReader("x"), 0, "", ErrSizeMismatch},
		{"unknown", strings.NewReader("anything"), -1, "anything", nil},
		{"read error", iotest.ErrReader(errors.New("boom")), 5, "", nil},
	} {
		got, err := io.ReadAll(ExactSize(tc.r, tc.size))
		switch {
		case tc.name == "read error":
			if err == nil || err.Error() != "boom" {
				t.Errorf("%s: err = %v", tc.name, err)
			}
		case tc.wantErr != nil:
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("%s: err = %v, want %v", tc.name, err, tc.wantErr)
			}
		default:
			if err != nil || string(got) != tc.want {
				t.Errorf("%s: got %q, %v", tc.name, got, err)
			}
		}
	}
	if !errors.Is(fmtShort(), ErrSizeMismatch) {
		t.Error("short read should also match ErrSizeMismatch")
	}
}

func fmtShort() error {
	_, err := io.ReadAll(ExactSize(strings.NewReader("a"), 2))
	return err
}

func TestContextReader(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := ContextReader(ctx, strings.NewReader("abc"))
	buf := make([]byte, 1)
	if _, err := r.Read(buf); err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := r.Read(buf); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
	plain := strings.NewReader("x")
	if ContextReader(context.Background(), plain) != io.Reader(plain) {
		t.Error("background context should not wrap")
	}
}

var png = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")

func TestBodyContentType(t *testing.T) {
	for _, tc := range []struct {
		key, ct string
		data    []byte
		want    string
	}{
		{"a.html", "", []byte("x"), "text/html; charset=utf-8"},
		{"a.html", "text/plain", []byte("x"), "text/plain"},
		{"image", "", png, "image/png"},
		{"empty", "", nil, "application/octet-stream"},
		{"big", "", bytes.Repeat([]byte("<html>"), 1000), "text/html; charset=utf-8"},
		{"a.css", "  text/css  ", []byte("x"), "text/css"},
		// Unusable caller-supplied types fall back to detection.
		{"a.html", "text/plain\r\nX-Injected: 1", []byte("x"), "text/html; charset=utf-8"},
		{"image", "imäge/png", png, "image/png"},
		{"a.txt", strings.Repeat("x", 300), []byte("x"), "text/plain; charset=utf-8"},
	} {
		b := NewBody(bytes.NewReader(tc.data))
		got, err := b.ContentType(tc.key, tc.ct)
		if err != nil || got != tc.want {
			t.Errorf("ContentType(%q, %q) = %q, %v; want %q", tc.key, tc.ct, got, err, tc.want)
		}
		// Sniffing must not consume the body.
		rest, err := io.ReadAll(b.Reader())
		if err != nil || !bytes.Equal(rest, tc.data) && !(len(rest) == 0 && len(tc.data) == 0) {
			t.Errorf("body after sniff = %d bytes, %v; want %d", len(rest), err, len(tc.data))
		}
	}
	b := NewBody(iotest.ErrReader(errors.New("boom")))
	if _, err := b.ContentType("noext", ""); err == nil {
		t.Error("ContentType on failing reader succeeded")
	}
}

func TestNextChunk(t *testing.T) {
	for _, tc := range []struct {
		name   string
		n      int
		max    int64
		chunks []int
	}{
		{"empty", 0, 10, []int{0}},
		{"smaller", 5, 10, []int{5}},
		{"exact", 10, 10, []int{10}},
		{"one more", 11, 10, []int{10, 1}},
		{"multiple", 25, 10, []int{10, 10, 5}},
		{"multiple exact", 30, 10, []int{10, 10, 10}},
	} {
		data := bytes.Repeat([]byte("x"), tc.n)
		b := NewBody(iotest.HalfReader(bytes.NewReader(data)))
		var got []int
		for {
			chunk, last, err := b.NextChunk(tc.max, int64(tc.n))
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			got = append(got, len(chunk))
			if last {
				break
			}
			if len(got) > 100 {
				t.Fatalf("%s: runaway", tc.name)
			}
		}
		if !slices.Equal(got, tc.chunks) {
			t.Errorf("%s: chunks = %v, want %v", tc.name, got, tc.chunks)
		}
	}
	b := NewBody(io.MultiReader(strings.NewReader("abc"), iotest.ErrReader(errors.New("boom"))))
	if _, _, err := b.NextChunk(10, -1); err == nil {
		t.Error("NextChunk on failing reader succeeded")
	}
}

func TestForEach(t *testing.T) {
	items := make([]int, 100)
	for i := range items {
		items[i] = i
	}
	var sum, active, peak atomic.Int64
	err := ForEach(context.Background(), items, 4, func(_ context.Context, i int) error {
		n := active.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		active.Add(-1)
		sum.Add(int64(i))
		return nil
	})
	if err != nil || sum.Load() != 4950 {
		t.Errorf("ForEach err=%v sum=%d", err, sum.Load())
	}
	if peak.Load() > 4 {
		t.Errorf("concurrency peaked at %d", peak.Load())
	}
	var calls atomic.Int64
	err = ForEach(context.Background(), items, 8, func(_ context.Context, i int) error {
		calls.Add(1)
		if i == 50 {
			return errors.New("fifty")
		}
		return nil
	})
	if err == nil || err.Error() != "fifty" || calls.Load() != 100 {
		t.Errorf("ForEach error handling: err=%v calls=%d", err, calls.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ForEach(ctx, items, 2, func(context.Context, int) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Errorf("ForEach cancelled err = %v", err)
	}
	if err := ForEach(context.Background(), []int(nil), 4, func(context.Context, int) error { return errors.New("x") }); err != nil {
		t.Errorf("ForEach(empty) = %v", err)
	}
}

func TestSortListResultAndHelpers(t *testing.T) {
	res := &blob.ListResult{
		Objects:  []blob.ObjectInfo{{Key: "b/c"}, {Key: "b.txt"}, {Key: "a"}},
		Prefixes: []string{"z/", "b/", "z/", "a/"},
	}
	SortListResult(res)
	var keys []string
	for _, o := range res.Objects {
		keys = append(keys, o.Key)
	}
	if !slices.Equal(keys, []string{"a", "b.txt", "b/c"}) || !slices.Equal(res.Prefixes, []string{"a/", "b/", "z/"}) {
		t.Errorf("SortListResult = %v %v", keys, res.Prefixes)
	}
	if ListableKey("dir/") || !ListableKey("dir/x") {
		t.Error("ListableKey")
	}
	rc := LimitedReadCloser(io.NopCloser(strings.NewReader("abcdef")), 3)
	b, _ := io.ReadAll(rc)
	if string(b) != "abc" || rc.Close() != nil {
		t.Errorf("LimitedReadCloser = %q", b)
	}
	rc = LimitedReadCloser(io.NopCloser(strings.NewReader("abcdef")), -1)
	if b, _ := io.ReadAll(rc); string(b) != "abcdef" {
		t.Errorf("LimitedReadCloser(-1) = %q", b)
	}
	if b, _ := io.ReadAll(EmptyBody()); len(b) != 0 {
		t.Error("EmptyBody not empty")
	}
}

func TestPublicEndpoint(t *testing.T) {
	for in, want := range map[string]string{
		"":                                       "",
		"https://user:pass@minio.local:9000/":    "https://minio.local:9000/",
		"http://127.0.0.1:10000/acct?sv=1&sig=x": "http://127.0.0.1:10000/acct",
		"https://x.r2.cloudflarestorage.com":     "https://x.r2.cloudflarestorage.com",
		"not a url":                              "",
	} {
		if got := PublicEndpoint(in); got != want {
			t.Errorf("PublicEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCheckFullKey(t *testing.T) {
	for _, tc := range []struct {
		prefix, key string
		ok          bool
	}{
		{"", strings.Repeat("k", 1024), true},
		{"p/", strings.Repeat("k", 1022), true},
		{"p/", strings.Repeat("k", 1023), false},
		{"tenant/backyard/", "sites/foo/index.html", true},
		{"p/", "../x", false},
		{"p/", "", false},
	} {
		err := CheckFullKey(tc.prefix, tc.key)
		if (err == nil) != tc.ok {
			t.Errorf("CheckFullKey(%q, %d-byte key) = %v, want ok=%v", tc.prefix, len(tc.key), err, tc.ok)
		}
		if err != nil && !errors.Is(err, blob.ErrInvalidKey) {
			t.Errorf("CheckFullKey error does not wrap ErrInvalidKey: %v", err)
		}
	}
}

func TestCleanContentType(t *testing.T) {
	for in, want := range map[string]string{
		"":                            "",
		"text/html; charset=utf-8":    "text/html; charset=utf-8",
		" application/json ":          "application/json",
		"text/plain\r\nSet-Cookie: x": "",
		"text/plain\x00":              "",
		"text/plain\x7f":              "",
		"tëxt/plain":                  "",
		strings.Repeat("a", 257):      "",
	} {
		if got := CleanContentType(in); got != want {
			t.Errorf("CleanContentType(%q) = %q, want %q", in, got, want)
		}
	}
	if got := PresignContentType("a.js", "bad\nvalue"); got != "text/javascript; charset=utf-8" {
		t.Errorf("PresignContentType with bad type = %q", got)
	}
	if got := PresignContentType("a.js", "application/javascript"); got != "application/javascript" {
		t.Errorf("PresignContentType = %q", got)
	}
}

func TestRedactURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://user:pass@minio.local:9000/x?sig=secret#frag": "https://minio.local:9000/x",
		"not a url":       "not%20a%20url",
		"http://[::1:bad": "(unparseable URL)",
	} {
		if got := RedactURL(in); got != want {
			t.Errorf("RedactURL(%q) = %q, want %q", in, got, want)
		}
	}
}
