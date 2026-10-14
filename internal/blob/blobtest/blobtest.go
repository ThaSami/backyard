// Package blobtest is a conformance test-suite for blob.Store
// implementations. Every backend runs it from its own tests:
//
//	func TestConformance(t *testing.T) {
//		blobtest.Run(t, func(t *testing.T) blob.Store { return newIsolatedStore(t) })
//	}
//
// newStore is called once per subtest and must return an empty store that no
// other call shares (a fresh directory, bucket or key prefix). Run closes each
// store when its subtest ends, so factories should not.
//
// The suite covers reads, writes, overwrites (ETag changes), ranges, listing
// with and without delimiter, deletion, key validation, unicode keys,
// content types, concurrency, cancellation, ~1000-object listings and
// deletes (crossing remote page and batch sizes), and, when the store
// supports it, presigned PUT/GET round trips over net/http (the download
// URL must also work without any extra headers, as a redirect target).
//
// Options declare what a backend's presigned uploads enforce, so the suite
// also checks that wrong-size (PresignEnforcesSize) or wrong-content
// (PresignEnforcesSHA256) bodies are rejected without storing anything:
//
//	blobtest.Run(t, newStore, blobtest.PresignEnforcesSize())
package blobtest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/blob"
)

// Option declares a capability of the store under test.
type Option func(*suite)

type suite struct {
	presignSize    bool
	presignSHA256  bool
	presignDigests bool
}

// PresignEnforcesSize declares that the storage endpoint rejects a PUT to a
// presigned URL whose body size differs from PresignPutOptions.Size (S3 and
// GCS do; Azure SAS URLs cannot).
func PresignEnforcesSize() Option { return func(s *suite) { s.presignSize = true } }

// PresignEnforcesSHA256 declares that the storage endpoint also rejects a
// body of the right size whose SHA-256 differs from PresignPutOptions.SHA256
// (S3 with flexible checksums).
func PresignEnforcesSHA256() Option { return func(s *suite) { s.presignSHA256 = true } }

// ReportsPresignedDigests declares that the store implements
// blob.DigestReporter and reports the SHA-256 of an object uploaded through
// a presigned URL that declared one (S3 with flexible checksums).
func ReportsPresignedDigests() Option { return func(s *suite) { s.presignDigests = true } }

// Run executes the conformance suite against stores built by newStore.
func Run(t *testing.T, newStore func(t *testing.T) blob.Store, opts ...Option) {
	t.Helper()
	var cfg suite
	for _, o := range opts {
		o(&cfg)
	}
	tests := []struct {
		name string
		fn   func(t *testing.T, s blob.Store)
	}{
		{"PutGetStat", testPutGetStat},
		{"Overwrite", testOverwrite},
		{"PutUnknownSize", testPutUnknownSize},
		{"PutSizeMismatch", testPutSizeMismatch},
		{"EmptyObject", testEmptyObject},
		{"GetRange", testGetRange},
		{"NotFound", testNotFound},
		{"Delete", testDelete},
		{"List", testList},
		{"ListDelimiter", testListDelimiter},
		{"DeletePrefix", testDeletePrefix},
		{"ManyObjects", testManyObjects},
		{"InvalidKeys", testInvalidKeys},
		{"LargeObject", testLargeObject},
		{"UnicodeKeys", testUnicodeKeys},
		{"ContentTypes", testContentTypes},
		{"ConcurrentPuts", testConcurrentPuts},
		{"ContextCanceled", testContextCanceled},
		{"PingDescribe", testPingDescribe},
		{"Presign", testPresign},
		{"PresignEnforcement", func(t *testing.T, s blob.Store) { testPresignEnforcement(t, s, cfg) }},
		{"DigestReporter", func(t *testing.T, s blob.Store) { testDigestReporter(t, s, cfg) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			if s == nil {
				t.Fatal("newStore returned nil")
			}
			t.Cleanup(func() {
				if err := s.Close(); err != nil {
					t.Errorf("Close: %v", err)
				}
			})
			tc.fn(t, s)
		})
	}
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func mustPut(t *testing.T, s blob.Store, key string, data []byte, contentType string) *blob.ObjectInfo {
	t.Helper()
	info, err := s.Put(testCtx(t), key, bytes.NewReader(data), int64(len(data)), contentType)
	if err != nil {
		t.Fatalf("Put(%q): %v", key, err)
	}
	if info == nil {
		t.Fatalf("Put(%q) returned nil info", key)
	}
	if info.Key != key {
		t.Errorf("Put(%q).Key = %q", key, info.Key)
	}
	if info.Size != int64(len(data)) {
		t.Errorf("Put(%q).Size = %d, want %d", key, info.Size, len(data))
	}
	if info.ETag == "" {
		t.Errorf("Put(%q).ETag is empty", key)
	}
	return info
}

func mustGet(t *testing.T, s blob.Store, key string) ([]byte, *blob.ObjectInfo) {
	t.Helper()
	rc, info, err := s.Get(testCtx(t), key)
	if err != nil {
		t.Fatalf("Get(%q): %v", key, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("Get(%q) read: %v", key, err)
	}
	if info == nil {
		t.Fatalf("Get(%q) returned nil info", key)
	}
	return data, info
}

func mustStat(t *testing.T, s blob.Store, key string) *blob.ObjectInfo {
	t.Helper()
	info, err := s.Stat(testCtx(t), key)
	if err != nil {
		t.Fatalf("Stat(%q): %v", key, err)
	}
	return info
}

func mustList(t *testing.T, s blob.Store, prefix, delimiter string) *blob.ListResult {
	t.Helper()
	res, err := s.List(testCtx(t), prefix, delimiter)
	if err != nil {
		t.Fatalf("List(%q, %q): %v", prefix, delimiter, err)
	}
	if res == nil {
		t.Fatalf("List(%q, %q) returned nil", prefix, delimiter)
	}
	return res
}

func keysOf(objs []blob.ObjectInfo) []string {
	out := make([]string, 0, len(objs))
	for _, o := range objs {
		out = append(out, o.Key)
	}
	return out
}

func sameInfo(t *testing.T, what string, got, want *blob.ObjectInfo) {
	t.Helper()
	if got.Key != want.Key {
		t.Errorf("%s: Key = %q, want %q", what, got.Key, want.Key)
	}
	if got.Size != want.Size {
		t.Errorf("%s: Size = %d, want %d", what, got.Size, want.Size)
	}
	if got.ETag != want.ETag {
		t.Errorf("%s: ETag = %q, want %q", what, got.ETag, want.ETag)
	}
	if got.ContentType != "" && want.ContentType != "" && got.ContentType != want.ContentType {
		t.Errorf("%s: ContentType = %q, want %q", what, got.ContentType, want.ContentType)
	}
}

func checkModTime(t *testing.T, what string, mod time.Time) {
	t.Helper()
	if mod.IsZero() {
		t.Errorf("%s: ModTime is zero", what)
		return
	}
	if d := time.Since(mod); d > time.Hour || d < -time.Hour {
		t.Errorf("%s: ModTime %v is not close to now", what, mod)
	}
}

func testPutGetStat(t *testing.T, s blob.Store) {
	const key = "a/b/hello.txt"
	data := []byte("hello, backyard\n")
	put := mustPut(t, s, key, data, "text/plain; charset=utf-8")
	checkModTime(t, "Put", put.ModTime)
	if put.ContentType != "text/plain; charset=utf-8" {
		t.Errorf("Put ContentType = %q", put.ContentType)
	}

	got, info := mustGet(t, s, key)
	if !bytes.Equal(got, data) {
		t.Fatalf("Get = %q, want %q", got, data)
	}
	sameInfo(t, "Get vs Put", info, put)
	if info.ContentType != "text/plain; charset=utf-8" {
		t.Errorf("Get ContentType = %q", info.ContentType)
	}
	checkModTime(t, "Get", info.ModTime)

	st := mustStat(t, s, key)
	sameInfo(t, "Stat vs Put", st, put)
	if st.ContentType != "text/plain; charset=utf-8" {
		t.Errorf("Stat ContentType = %q", st.ContentType)
	}
	checkModTime(t, "Stat", st.ModTime)
}

func testOverwrite(t *testing.T, s blob.Store) {
	const key = "ow/file.txt"
	v1 := mustPut(t, s, key, []byte("version-1"), "text/plain; charset=utf-8")
	v2 := mustPut(t, s, key, []byte("version-2"), "text/plain; charset=utf-8") // same size
	if v1.ETag == v2.ETag {
		t.Errorf("ETag did not change on overwrite: %q", v1.ETag)
	}
	got, info := mustGet(t, s, key)
	if string(got) != "version-2" {
		t.Errorf("Get after overwrite = %q", got)
	}
	if info.ETag != v2.ETag {
		t.Errorf("Get ETag = %q, want %q", info.ETag, v2.ETag)
	}
	if st := mustStat(t, s, key); st.ETag != v2.ETag {
		t.Errorf("Stat ETag = %q, want %q", st.ETag, v2.ETag)
	}
	v3 := mustPut(t, s, key, []byte("version-3 is longer"), "text/plain; charset=utf-8")
	if v3.ETag == v2.ETag {
		t.Errorf("ETag did not change on overwrite with new size")
	}
	if st := mustStat(t, s, key); st.Size != int64(len("version-3 is longer")) || st.ETag != v3.ETag {
		t.Errorf("Stat after third write = %+v, want size %d etag %q", st, len("version-3 is longer"), v3.ETag)
	}
}

// onlyReader hides any io.Seeker/io.WriterTo implementation.
type onlyReader struct{ r io.Reader }

func (o onlyReader) Read(p []byte) (int, error) { return o.r.Read(p) }

func testPutUnknownSize(t *testing.T, s blob.Store) {
	data := bytes.Repeat([]byte("0123456789abcdef"), 4096) // 64 KiB
	info, err := s.Put(testCtx(t), "unknown/size.bin", onlyReader{bytes.NewReader(data)}, -1, "application/octet-stream")
	if err != nil {
		t.Fatalf("Put(size=-1): %v", err)
	}
	if info.Size != int64(len(data)) {
		t.Errorf("Put(size=-1).Size = %d, want %d", info.Size, len(data))
	}
	got, _ := mustGet(t, s, "unknown/size.bin")
	if !bytes.Equal(got, data) {
		t.Errorf("Get after unknown-size Put returned %d bytes, want %d", len(got), len(data))
	}
}

func testPutSizeMismatch(t *testing.T, s blob.Store) {
	ctx := testCtx(t)
	for _, tc := range []struct {
		key  string
		data string
		size int64
	}{
		{"size/short.txt", "12345", 10},
		{"size/long.txt", "1234567890", 5},
	} {
		if _, err := s.Put(ctx, tc.key, onlyReader{strings.NewReader(tc.data)}, tc.size, "text/plain; charset=utf-8"); err == nil {
			t.Errorf("Put(%q, %d bytes, size=%d) succeeded, want error", tc.key, len(tc.data), tc.size)
		}
		if _, err := s.Stat(ctx, tc.key); !errors.Is(err, blob.ErrNotFound) {
			t.Errorf("Stat(%q) after failed Put: err = %v, want ErrNotFound", tc.key, err)
		}
	}
}

func testEmptyObject(t *testing.T, s blob.Store) {
	const key = "empty/zero.txt"
	put := mustPut(t, s, key, nil, "text/plain; charset=utf-8")
	got, info := mustGet(t, s, key)
	if len(got) != 0 || info.Size != 0 {
		t.Errorf("Get empty = %q (size %d)", got, info.Size)
	}
	if info.ETag == "" || info.ETag != put.ETag {
		t.Errorf("Get empty ETag = %q, want %q", info.ETag, put.ETag)
	}
	for _, r := range [][2]int64{{0, -1}, {0, 10}, {0, 0}, {5, -1}} {
		rc, ri, err := s.GetRange(testCtx(t), key, r[0], r[1])
		if err != nil {
			t.Errorf("GetRange(empty, %d, %d): %v", r[0], r[1], err)
			continue
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil || len(b) != 0 || ri.Size != 0 {
			t.Errorf("GetRange(empty, %d, %d) = %q, size %d, err %v", r[0], r[1], b, ri.Size, err)
		}
	}
}

func testGetRange(t *testing.T, s blob.Store) {
	const key = "range/digits.txt"
	const data = "0123456789"
	put := mustPut(t, s, key, []byte(data), "text/plain; charset=utf-8")
	for _, tc := range []struct {
		offset, length int64
		want           string
	}{
		{0, -1, data},
		{0, 10, data},
		{0, 1, "0"},
		{2, 3, "234"},
		{5, -1, "56789"},
		{9, 1, "9"},
		{8, 100, "89"},
		{0, 0, ""},
		{3, 0, ""},
		{10, -1, ""},
		{10, 5, ""},
		{25, -1, ""},
	} {
		rc, info, err := s.GetRange(testCtx(t), key, tc.offset, tc.length)
		if err != nil {
			t.Errorf("GetRange(%d, %d): %v", tc.offset, tc.length, err)
			continue
		}
		got, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Errorf("GetRange(%d, %d) read: %v", tc.offset, tc.length, err)
			continue
		}
		if string(got) != tc.want {
			t.Errorf("GetRange(%d, %d) = %q, want %q", tc.offset, tc.length, got, tc.want)
		}
		if info == nil {
			t.Errorf("GetRange(%d, %d) returned nil info", tc.offset, tc.length)
			continue
		}
		if info.Size != int64(len(data)) {
			t.Errorf("GetRange(%d, %d) info.Size = %d, want total %d", tc.offset, tc.length, info.Size, len(data))
		}
		if info.ETag != put.ETag {
			t.Errorf("GetRange(%d, %d) info.ETag = %q, want %q", tc.offset, tc.length, info.ETag, put.ETag)
		}
	}
	if _, _, err := s.GetRange(testCtx(t), key, -1, 2); err == nil {
		t.Error("GetRange with negative offset succeeded")
	}
	if _, _, err := s.GetRange(testCtx(t), "range/missing.txt", 0, 2); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("GetRange(missing) err = %v, want ErrNotFound", err)
	}
}

func testNotFound(t *testing.T, s blob.Store) {
	ctx := testCtx(t)
	mustPut(t, s, "nf/dir/file.txt", []byte("x"), "")
	for _, key := range []string{"nf/missing.txt", "nf/dir", "nf/dir/file.txt/below", "nf/di", "missing"} {
		if _, _, err := s.Get(ctx, key); !errors.Is(err, blob.ErrNotFound) {
			t.Errorf("Get(%q) err = %v, want ErrNotFound", key, err)
		}
		if _, err := s.Stat(ctx, key); !errors.Is(err, blob.ErrNotFound) {
			t.Errorf("Stat(%q) err = %v, want ErrNotFound", key, err)
		}
		if _, _, err := s.GetRange(ctx, key, 1, 1); !errors.Is(err, blob.ErrNotFound) {
			t.Errorf("GetRange(%q) err = %v, want ErrNotFound", key, err)
		}
	}
}

func testDelete(t *testing.T, s blob.Store) {
	ctx := testCtx(t)
	mustPut(t, s, "del/one.txt", []byte("1"), "")
	mustPut(t, s, "del/nested/two.txt", []byte("2"), "")
	if err := s.Delete(ctx, "del/one.txt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Stat(ctx, "del/one.txt"); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("Stat after Delete err = %v, want ErrNotFound", err)
	}
	if _, _, err := s.Get(ctx, "del/one.txt"); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("Get after Delete err = %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, "del/one.txt"); err != nil {
		t.Errorf("second Delete: %v", err)
	}
	if err := s.Delete(ctx, "never/existed.txt"); err != nil {
		t.Errorf("Delete(missing): %v", err)
	}
	// Deleting a "directory" key is a no-op and must not touch its contents.
	if err := s.Delete(ctx, "del/nested"); err != nil {
		t.Errorf("Delete(directory-like key): %v", err)
	}
	if got, _ := mustGet(t, s, "del/nested/two.txt"); string(got) != "2" {
		t.Errorf("nested object changed: %q", got)
	}
	res := mustList(t, s, "del/", "")
	if k := keysOf(res.Objects); !slices.Equal(k, []string{"del/nested/two.txt"}) {
		t.Errorf("List after Delete = %q", k)
	}
}

var listKeys = []string{
	"list/a.txt",
	"list/a.b/x.txt",
	"list/b.txt",
	"list/b/c.txt",
	"list/b/d/e.txt",
	"list/z",
	"listing/other.txt",
	"top.txt",
}

func putListFixture(t *testing.T, s blob.Store) map[string]*blob.ObjectInfo {
	t.Helper()
	infos := map[string]*blob.ObjectInfo{}
	for _, k := range listKeys {
		infos[k] = mustPut(t, s, k, []byte("content of "+k), "")
	}
	return infos
}

func testList(t *testing.T, s blob.Store) {
	infos := putListFixture(t, s)
	for _, tc := range []struct {
		prefix string
		want   []string
	}{
		{"list/", []string{"list/a.b/x.txt", "list/a.txt", "list/b.txt", "list/b/c.txt", "list/b/d/e.txt", "list/z"}},
		{"list/b", []string{"list/b.txt", "list/b/c.txt", "list/b/d/e.txt"}},
		{"list/b/", []string{"list/b/c.txt", "list/b/d/e.txt"}},
		{"list", []string{"list/a.b/x.txt", "list/a.txt", "list/b.txt", "list/b/c.txt", "list/b/d/e.txt", "list/z", "listing/other.txt"}},
		{"list/a.txt", []string{"list/a.txt"}},
		{"nothing/", nil},
		{"list/zz", nil},
		{"", slices.Sorted(slices.Values(listKeys))},
	} {
		res := mustList(t, s, tc.prefix, "")
		if got := keysOf(res.Objects); !slices.Equal(got, tc.want) && !(len(got) == 0 && len(tc.want) == 0) {
			t.Errorf("List(%q, \"\") = %q, want %q", tc.prefix, got, tc.want)
		}
		if len(res.Prefixes) != 0 {
			t.Errorf("List(%q, \"\") Prefixes = %q, want none", tc.prefix, res.Prefixes)
		}
		for _, o := range res.Objects {
			want := infos[o.Key]
			if want == nil {
				t.Errorf("List returned unexpected key %q", o.Key)
				continue
			}
			sameInfo(t, "List entry "+o.Key, &o, want)
			if o.ContentType != "" {
				if st := mustStat(t, s, o.Key); st.ContentType != o.ContentType {
					t.Errorf("List %q ContentType %q != Stat %q", o.Key, o.ContentType, st.ContentType)
				}
			}
			checkModTime(t, "List entry "+o.Key, o.ModTime)
		}
	}
}

func testListDelimiter(t *testing.T, s blob.Store) {
	putListFixture(t, s)
	for _, tc := range []struct {
		prefix   string
		objects  []string
		prefixes []string
	}{
		{"list/", []string{"list/a.txt", "list/b.txt", "list/z"}, []string{"list/a.b/", "list/b/"}},
		{"list/b/", []string{"list/b/c.txt"}, []string{"list/b/d/"}},
		{"list/b", []string{"list/b.txt"}, []string{"list/b/"}},
		{"list/b/d/", []string{"list/b/d/e.txt"}, nil},
		{"", []string{"top.txt"}, []string{"list/", "listing/"}},
		{"list", nil, []string{"list/", "listing/"}},
		{"list/b/d", nil, []string{"list/b/d/"}},
		{"list/a", []string{"list/a.txt"}, []string{"list/a.b/"}},
		{"list/z", []string{"list/z"}, nil},
		{"nothing/", nil, nil},
	} {
		res := mustList(t, s, tc.prefix, "/")
		if got := keysOf(res.Objects); !slices.Equal(got, tc.objects) && !(len(got) == 0 && len(tc.objects) == 0) {
			t.Errorf("List(%q, \"/\") objects = %q, want %q", tc.prefix, got, tc.objects)
		}
		if !slices.Equal(res.Prefixes, tc.prefixes) && !(len(res.Prefixes) == 0 && len(tc.prefixes) == 0) {
			t.Errorf("List(%q, \"/\") prefixes = %q, want %q", tc.prefix, res.Prefixes, tc.prefixes)
		}
	}
	if _, err := s.List(testCtx(t), "list/", "|"); err == nil {
		t.Error("List with unsupported delimiter succeeded")
	}
}

func testDeletePrefix(t *testing.T, s blob.Store) {
	ctx := testCtx(t)
	for _, k := range []string{"dp/a.txt", "dp/b/c.txt", "dp/b/d/e.txt", "dpx/f.txt", "keep/g.txt", "dp.txt"} {
		mustPut(t, s, k, []byte(k), "")
	}
	if err := s.DeletePrefix(ctx, "dp/"); err != nil {
		t.Fatalf("DeletePrefix(dp/): %v", err)
	}
	res := mustList(t, s, "", "")
	if got, want := keysOf(res.Objects), []string{"dp.txt", "dpx/f.txt", "keep/g.txt"}; !slices.Equal(got, want) {
		t.Errorf("after DeletePrefix(dp/) = %q, want %q", got, want)
	}
	res = mustList(t, s, "", "/")
	if want := []string{"dpx/", "keep/"}; !slices.Equal(res.Prefixes, want) {
		t.Errorf("prefixes after DeletePrefix(dp/) = %q, want %q", res.Prefixes, want)
	}
	if err := s.DeletePrefix(ctx, "dp/"); err != nil {
		t.Errorf("repeated DeletePrefix: %v", err)
	}
	if err := s.DeletePrefix(ctx, "nothing/here/"); err != nil {
		t.Errorf("DeletePrefix(missing): %v", err)
	}
	// Partial (non-"/"-terminated) prefixes match key prefixes, not directories.
	if err := s.DeletePrefix(ctx, "dp"); err != nil {
		t.Fatalf("DeletePrefix(dp): %v", err)
	}
	res = mustList(t, s, "", "")
	if got, want := keysOf(res.Objects), []string{"keep/g.txt"}; !slices.Equal(got, want) {
		t.Errorf("after DeletePrefix(dp) = %q, want %q", got, want)
	}
	if err := s.DeletePrefix(ctx, ""); !errors.Is(err, blob.ErrInvalidKey) {
		t.Errorf("DeletePrefix(\"\") err = %v, want ErrInvalidKey", err)
	}
	if got, _ := mustGet(t, s, "keep/g.txt"); string(got) != "keep/g.txt" {
		t.Errorf("unrelated object changed: %q", got)
	}
	// Many objects (more than one listing page on small-page backends).
	for i := range 30 {
		mustPut(t, s, fmt.Sprintf("many/%02d/x.txt", i), []byte("x"), "")
	}
	if err := s.DeletePrefix(ctx, "many/"); err != nil {
		t.Fatalf("DeletePrefix(many/): %v", err)
	}
	if res := mustList(t, s, "many/", ""); len(res.Objects) != 0 {
		t.Errorf("objects left after DeletePrefix(many/): %q", keysOf(res.Objects))
	}
}

var invalidKeys = []string{
	"",
	"/abs",
	"/",
	".",
	"..",
	"../x",
	"a/../b",
	"a/./b",
	"a//b",
	"a\\b",
	"a\x00b",
	"a\nb",
	"a\x7fb",
	"dir/",
	"\xff\xfe.txt",
	strings.Repeat("k", 1025),
}

func testInvalidKeys(t *testing.T, s blob.Store) {
	ctx := testCtx(t)
	for _, key := range invalidKeys {
		name := fmt.Sprintf("%q", key)
		if len(name) > 40 {
			name = name[:40] + "…"
		}
		check := func(op string, err error) {
			t.Helper()
			if !errors.Is(err, blob.ErrInvalidKey) {
				t.Errorf("%s(%s) err = %v, want ErrInvalidKey", op, name, err)
			}
		}
		_, _, err := s.Get(ctx, key)
		check("Get", err)
		_, _, err = s.GetRange(ctx, key, 0, 1)
		check("GetRange", err)
		_, err = s.Stat(ctx, key)
		check("Stat", err)
		_, err = s.Put(ctx, key, strings.NewReader("x"), 1, "text/plain")
		check("Put", err)
		check("Delete", s.Delete(ctx, key))
		_, err = s.PresignPut(ctx, key, blob.PresignPutOptions{ContentType: "text/plain", Size: 1}, time.Minute)
		check("PresignPut", err)
		_, err = s.PresignGet(ctx, key, time.Minute)
		check("PresignGet", err)
	}
	for _, prefix := range []string{"/abs", "a/../", "a//", "../", "a\\b/"} {
		if _, err := s.List(ctx, prefix, ""); !errors.Is(err, blob.ErrInvalidKey) {
			t.Errorf("List(%q) err = %v, want ErrInvalidKey", prefix, err)
		}
		if err := s.DeletePrefix(ctx, prefix); !errors.Is(err, blob.ErrInvalidKey) {
			t.Errorf("DeletePrefix(%q) err = %v, want ErrInvalidKey", prefix, err)
		}
	}
	// Nothing may have been written.
	if res := mustList(t, s, "", ""); len(res.Objects) != 0 {
		t.Errorf("invalid keys created objects: %q", keysOf(res.Objects))
	}
}

func testLargeObject(t *testing.T, s blob.Store) {
	rng := rand.New(rand.NewPCG(1, 2))
	data := make([]byte, 5<<20+123)
	for i := range data {
		data[i] = byte(rng.Uint32())
	}
	want := sha256.Sum256(data)
	for _, tc := range []struct {
		key  string
		size int64
	}{
		{"large/known.bin", int64(len(data))},
		{"large/unknown.bin", -1},
	} {
		info, err := s.Put(testCtx(t), tc.key, onlyReader{bytes.NewReader(data)}, tc.size, "application/octet-stream")
		if err != nil {
			t.Fatalf("Put(%q): %v", tc.key, err)
		}
		if info.Size != int64(len(data)) {
			t.Errorf("Put(%q).Size = %d", tc.key, info.Size)
		}
		got, ginfo := mustGet(t, s, tc.key)
		if sha256.Sum256(got) != want {
			t.Errorf("Get(%q) content mismatch (%d bytes)", tc.key, len(got))
		}
		if ginfo.Size != int64(len(data)) || ginfo.ETag != info.ETag {
			t.Errorf("Get(%q) info = %+v, want size %d etag %q", tc.key, ginfo, len(data), info.ETag)
		}
		off := int64(5<<20 - 10)
		rc, _, err := s.GetRange(testCtx(t), tc.key, off, 20)
		if err != nil {
			t.Fatalf("GetRange(%q): %v", tc.key, err)
		}
		part, err := io.ReadAll(rc)
		rc.Close()
		if err != nil || !bytes.Equal(part, data[off:off+20]) {
			t.Errorf("GetRange(%q) = %x (err %v), want %x", tc.key, part, err, data[off:off+20])
		}
	}
}

var unicodeKeys = []string{
	"unicode/héllo wörld/日本語.txt",
	"unicode/emoji 😀.txt",
	"unicode/special+chars=&%20;,.txt",
	"unicode/q?a#3.txt",
	"unicode/quote'and\"dq.txt",
	"unicode/[brackets]{braces}~!@$^()*.txt",
	"unicode/.well-known/security.txt",
	"unicode/.hidden",
	"unicode/" + strings.Repeat("n", 200) + ".txt",
}

func testUnicodeKeys(t *testing.T, s blob.Store) {
	ctx := testCtx(t)
	for _, k := range unicodeKeys {
		mustPut(t, s, k, []byte("body:"+k), "text/plain; charset=utf-8")
	}
	for _, k := range unicodeKeys {
		got, info := mustGet(t, s, k)
		if string(got) != "body:"+k {
			t.Errorf("Get(%q) = %q", k, got)
		}
		if info.Key != k {
			t.Errorf("Get(%q).Key = %q", k, info.Key)
		}
		if st := mustStat(t, s, k); st.Key != k || st.Size != int64(len("body:"+k)) {
			t.Errorf("Stat(%q) = %+v", k, st)
		}
	}
	res := mustList(t, s, "unicode/", "")
	if got, want := keysOf(res.Objects), slices.Sorted(slices.Values(unicodeKeys)); !slices.Equal(got, want) {
		t.Errorf("List(unicode/) = %q, want %q", got, want)
	}
	res = mustList(t, s, "unicode/", "/")
	if want := []string{"unicode/.well-known/", "unicode/héllo wörld/"}; !slices.Equal(res.Prefixes, want) {
		t.Errorf("List(unicode/, /) prefixes = %q, want %q", res.Prefixes, want)
	}
	for _, k := range unicodeKeys {
		if err := s.Delete(ctx, k); err != nil {
			t.Errorf("Delete(%q): %v", k, err)
		}
		if _, err := s.Stat(ctx, k); !errors.Is(err, blob.ErrNotFound) {
			t.Errorf("Stat(%q) after delete err = %v", k, err)
		}
	}
}

var pngHeader = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89")

func testContentTypes(t *testing.T, s blob.Store) {
	for _, tc := range []struct {
		key, ct string
		data    []byte
		want    string
	}{
		{"ct/page.html", "", []byte("<!doctype html><title>x</title>"), "text/html; charset=utf-8"},
		{"ct/style.css", "", []byte("body{}"), "text/css; charset=utf-8"},
		{"ct/app.js", "", []byte("console.log(1)"), "text/javascript; charset=utf-8"},
		{"ct/data.json", "application/json", []byte(`{"a":1}`), "application/json"},
		{"ct/image", "", pngHeader, "image/png"},
		{"ct/binary", "", []byte{0x00, 0x01, 0x02, 0xfe, 0xff}, "application/octet-stream"},
		{"ct/UPPER.SVG", "", []byte("<svg/>"), "image/svg+xml"},
	} {
		put := mustPut(t, s, tc.key, tc.data, tc.ct)
		if put.ContentType != tc.want {
			t.Errorf("Put(%q).ContentType = %q, want %q", tc.key, put.ContentType, tc.want)
		}
		if st := mustStat(t, s, tc.key); st.ContentType != tc.want {
			t.Errorf("Stat(%q).ContentType = %q, want %q", tc.key, st.ContentType, tc.want)
		}
		if _, info := mustGet(t, s, tc.key); info.ContentType != tc.want {
			t.Errorf("Get(%q).ContentType = %q, want %q", tc.key, info.ContentType, tc.want)
		}
	}
	for _, o := range mustList(t, s, "ct/", "").Objects {
		if o.ContentType == "" {
			continue
		}
		if st := mustStat(t, s, o.Key); st.ContentType != o.ContentType {
			t.Errorf("List %q ContentType = %q, Stat = %q", o.Key, o.ContentType, st.ContentType)
		}
	}
}

func testConcurrentPuts(t *testing.T, s blob.Store) {
	const n = 16
	const size = 64 << 10
	ctx := testCtx(t)
	var wg sync.WaitGroup
	errs := make(chan error, 2*n)
	for i := range n {
		wg.Go(func() {
			data := bytes.Repeat([]byte{byte('a' + i)}, size)
			if _, err := s.Put(ctx, fmt.Sprintf("conc/own/%02d.bin", i), bytes.NewReader(data), size, ""); err != nil {
				errs <- fmt.Errorf("own %d: %w", i, err)
			}
			if _, err := s.Put(ctx, "conc/shared.bin", bytes.NewReader(data), size, ""); err != nil {
				errs <- fmt.Errorf("shared %d: %w", i, err)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	for i := range n {
		got, _ := mustGet(t, s, fmt.Sprintf("conc/own/%02d.bin", i))
		if !bytes.Equal(got, bytes.Repeat([]byte{byte('a' + i)}, size)) {
			t.Errorf("conc/own/%02d.bin has wrong content", i)
		}
	}
	got, _ := mustGet(t, s, "conc/shared.bin")
	if len(got) != size || bytes.Count(got, got[:1]) != size {
		t.Errorf("conc/shared.bin is torn or truncated (%d bytes)", len(got))
	}
	if res := mustList(t, s, "conc/", ""); len(res.Objects) != n+1 {
		t.Errorf("List(conc/) returned %d objects, want %d", len(res.Objects), n+1)
	}
}

func testContextCanceled(t *testing.T, s blob.Store) {
	mustPut(t, s, "ctx/exists.txt", []byte("x"), "")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.Put(ctx, "ctx/new.txt", strings.NewReader("data"), 4, ""); err == nil {
		t.Error("Put with cancelled context succeeded")
	}
	if rc, _, err := s.Get(ctx, "ctx/exists.txt"); err == nil {
		// Some clients only notice cancellation while reading.
		_, rerr := io.ReadAll(rc)
		rc.Close()
		if rerr == nil {
			t.Error("Get with cancelled context succeeded")
		}
	}
	if _, err := s.List(ctx, "ctx/", ""); err == nil {
		t.Error("List with cancelled context succeeded")
	}
	if _, err := s.Stat(ctx, "ctx/exists.txt"); err == nil {
		t.Error("Stat with cancelled context succeeded")
	}
	if err := s.Delete(ctx, "ctx/exists.txt"); err == nil {
		t.Error("Delete with cancelled context succeeded")
	}
	if err := s.DeletePrefix(ctx, "ctx/"); err == nil {
		t.Error("DeletePrefix with cancelled context succeeded")
	}
	// Nothing was deleted or written.
	mustStat(t, s, "ctx/exists.txt")
	if _, err := s.Stat(testCtx(t), "ctx/new.txt"); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("Stat(ctx/new.txt) err = %v, want ErrNotFound", err)
	}
}

// testManyObjects crosses the 1000-entry page size of S3/GCS listings and
// the 1000-key DeleteObjects batch.
func testManyObjects(t *testing.T, s blob.Store) {
	const n = 1050
	ctx := testCtx(t)
	keys := make([]string, n)
	for i := range keys {
		keys[i] = fmt.Sprintf("many/%02d/%04d.txt", i%7, i)
	}
	var (
		wg   sync.WaitGroup
		next atomic.Int64
		mu   sync.Mutex
		errs []error
	)
	for range 16 {
		wg.Go(func() {
			for {
				i := int(next.Add(1)) - 1
				if i >= n {
					return
				}
				if _, err := s.Put(ctx, keys[i], strings.NewReader("x"), 1, "text/plain; charset=utf-8"); err != nil {
					mu.Lock()
					errs = append(errs, fmt.Errorf("Put(%q): %w", keys[i], err))
					mu.Unlock()
					return
				}
			}
		})
	}
	wg.Wait()
	if len(errs) > 0 {
		t.Fatalf("%d puts failed, first: %v", len(errs), errs[0])
	}
	mustPut(t, s, "manyx.txt", []byte("keep"), "")
	res := mustList(t, s, "many/", "")
	if got, want := keysOf(res.Objects), slices.Sorted(slices.Values(keys)); !slices.Equal(got, want) {
		t.Fatalf("List(many/) returned %d keys, want %d (sorted)", len(got), len(want))
	}
	res = mustList(t, s, "many/", "/")
	if len(res.Objects) != 0 || len(res.Prefixes) != 7 || res.Prefixes[0] != "many/00/" || res.Prefixes[6] != "many/06/" {
		t.Errorf("List(many/, /) = %d objects, prefixes %q", len(res.Objects), res.Prefixes)
	}
	if err := s.DeletePrefix(ctx, "many/"); err != nil {
		t.Fatalf("DeletePrefix(many/): %v", err)
	}
	res = mustList(t, s, "", "")
	if got := keysOf(res.Objects); !slices.Equal(got, []string{"manyx.txt"}) {
		t.Errorf("after DeletePrefix(many/): %d objects left (first %q)", len(got), got[:min(len(got), 3)])
	}
}

func testPingDescribe(t *testing.T, s blob.Store) {
	if err := s.Ping(testCtx(t)); err != nil {
		t.Errorf("Ping: %v", err)
	}
	if d := s.Describe(); d.Type == "" {
		t.Errorf("Describe().Type is empty: %+v", d)
	}
}

func headerValue(h map[string]string, name string) string {
	for k, v := range h {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// exact returns upload options pinning data's size and digest.
func exact(contentType string, data []byte) blob.PresignPutOptions {
	return blob.PresignPutOptions{ContentType: contentType, Size: int64(len(data)), SHA256: sha256Hex(data)}
}

func testPresign(t *testing.T, s blob.Store) {
	ctx := testCtx(t)
	const key = "presign/up load ü.txt"
	const ct = "text/plain; charset=utf-8"
	data := []byte("uploaded via presigned URL\n")
	before := time.Now()
	req, err := s.PresignPut(ctx, key, exact(ct, data), 10*time.Minute)
	if errors.Is(err, blob.ErrNotSupported) {
		if _, gerr := s.PresignGet(ctx, key, time.Minute); !errors.Is(gerr, blob.ErrNotSupported) {
			t.Errorf("PresignPut unsupported but PresignGet err = %v", gerr)
		}
		t.Skip("store does not support presigning")
	}
	if err != nil {
		t.Fatalf("PresignPut: %v", err)
	}
	if req.Method != http.MethodPut || req.URL == "" {
		t.Fatalf("PresignPut = %+v", req)
	}
	if got := headerValue(req.Headers, "Content-Type"); got != ct {
		t.Errorf("PresignPut Headers Content-Type = %q, want %q (headers %v)", got, ct, req.Headers)
	}
	if req.ExpiresAt.Before(before.Add(9*time.Minute)) || req.ExpiresAt.After(time.Now().Add(11*time.Minute)) {
		t.Errorf("PresignPut ExpiresAt = %v, want ~10m from now", req.ExpiresAt)
	}
	doHTTP(t, req, data, http.StatusOK, http.StatusCreated, http.StatusNoContent)

	got, info := mustGet(t, s, key)
	if !bytes.Equal(got, data) {
		t.Fatalf("Get after presigned PUT = %q", got)
	}
	if info.ContentType != ct {
		t.Errorf("ContentType after presigned PUT = %q, want %q", info.ContentType, ct)
	}

	greq, err := s.PresignGet(ctx, key, 10*time.Minute)
	if err != nil {
		t.Fatalf("PresignGet: %v", err)
	}
	if greq.Method != http.MethodGet || greq.URL == "" {
		t.Fatalf("PresignGet = %+v", greq)
	}
	body := doHTTP(t, greq, nil, http.StatusOK)
	if !bytes.Equal(body, data) {
		t.Errorf("presigned GET body = %q, want %q", body, data)
	}
	// Download URLs are used as redirect targets, where no extra headers
	// can be sent: the URL alone must work.
	bare := *greq
	bare.Headers = nil
	if body := doHTTP(t, &bare, nil, http.StatusOK); !bytes.Equal(body, data) {
		t.Errorf("presigned GET without headers body = %q, want %q", body, data)
	}

	// Without a content type, the one derived from the key is signed;
	// without a size or digest, any body is accepted.
	req2, err := s.PresignPut(ctx, "presign/page.html", blob.PresignPutOptions{Size: -1}, time.Minute)
	if err != nil {
		t.Fatalf("PresignPut(no content type): %v", err)
	}
	if got := headerValue(req2.Headers, "Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("PresignPut(no content type) Content-Type = %q", got)
	}
	doHTTP(t, req2, []byte("<p>hi</p>"), http.StatusOK, http.StatusCreated, http.StatusNoContent)
	if st := mustStat(t, s, "presign/page.html"); st.ContentType != "text/html; charset=utf-8" || st.Size != 9 {
		t.Errorf("Stat after presigned PUT = %+v", st)
	}

	// Empty objects.
	req3, err := s.PresignPut(ctx, "presign/empty.txt", exact("", nil), time.Minute)
	if err != nil {
		t.Fatalf("PresignPut(empty): %v", err)
	}
	doHTTP(t, req3, []byte{}, http.StatusOK, http.StatusCreated, http.StatusNoContent)
	if st := mustStat(t, s, "presign/empty.txt"); st.Size != 0 {
		t.Errorf("Stat after empty presigned PUT = %+v", st)
	}

	// Invalid options are errors (not ErrNotSupported) and sign nothing.
	for name, opts := range map[string]blob.PresignPutOptions{
		"negative size":                         {Size: -2},
		"malformed sha256":                      {Size: 1, SHA256: "not-hex"},
		"sha256 of a non-empty body for size 0": {Size: 0, SHA256: sha256Hex([]byte("x"))},
	} {
		if r, err := s.PresignPut(ctx, "presign/bad.txt", opts, time.Minute); err == nil || errors.Is(err, blob.ErrNotSupported) {
			t.Errorf("PresignPut(%s) = %+v, %v; want an error", name, r, err)
		}
	}
}

// testPresignEnforcement checks that the storage endpoint rejects bodies
// that differ from what a presigned URL was issued for, as far as the store
// declares it can enforce, and that a rejected upload stores nothing.
func testPresignEnforcement(t *testing.T, s blob.Store, cfg suite) {
	if !cfg.presignSize && !cfg.presignSHA256 {
		t.Skip("store does not declare presigned upload enforcement")
	}
	ctx := testCtx(t)
	data := []byte("exactly these bytes")
	presign := func(key string, opts blob.PresignPutOptions) *blob.PresignedRequest {
		t.Helper()
		req, err := s.PresignPut(ctx, key, opts, 10*time.Minute)
		if errors.Is(err, blob.ErrNotSupported) {
			t.Skip("store does not support presigning")
		}
		if err != nil {
			t.Fatalf("PresignPut(%q): %v", key, err)
		}
		return req
	}
	rejected := func(req *blob.PresignedRequest, body []byte, what string) {
		t.Helper()
		if code, out := sendPresigned(t, req, body); code < 400 {
			t.Errorf("%s: presigned PUT status %d, want a rejection (%s)", what, code, out)
		}
	}

	if cfg.presignSize {
		const key = "enforce/new.txt"
		req := presign(key, exact("text/plain", data))
		rejected(req, append(bytes.Clone(data), "and more"...), "larger body")
		rejected(req, data[:5], "smaller body")
		if _, err := s.Stat(ctx, key); !errors.Is(err, blob.ErrNotFound) {
			t.Errorf("Stat after rejected uploads err = %v, want ErrNotFound", err)
		}
		// The URL still accepts the declared body.
		doHTTP(t, req, data, http.StatusOK, http.StatusCreated, http.StatusNoContent)
		if got, _ := mustGet(t, s, key); !bytes.Equal(got, data) {
			t.Errorf("Get after presigned PUT = %q", got)
		}

		// A rejected upload never replaces an existing object.
		const existing = "enforce/existing.txt"
		mustPut(t, s, existing, []byte("previous"), "text/plain")
		rejected(presign(existing, exact("text/plain", data)), bytes.Repeat(data, 2), "larger body over an existing object")
		if got, _ := mustGet(t, s, existing); string(got) != "previous" {
			t.Errorf("existing object after rejected upload = %q", got)
		}

		// A 0-byte upload URL accepts nothing else.
		empty := presign("enforce/empty.txt", blob.PresignPutOptions{Size: 0})
		rejected(empty, []byte("x"), "body for a 0-byte upload")
		if _, err := s.Stat(ctx, "enforce/empty.txt"); !errors.Is(err, blob.ErrNotFound) {
			t.Errorf("Stat after rejected 0-byte upload err = %v, want ErrNotFound", err)
		}
		doHTTP(t, empty, []byte{}, http.StatusOK, http.StatusCreated, http.StatusNoContent)
	}

	if cfg.presignSHA256 {
		const key = "enforce/digest.txt"
		req := presign(key, exact("text/plain", data))
		forged := bytes.ToUpper(data) // same size, other bytes
		rejected(req, forged, "same-size body with another sha256")
		if _, err := s.Stat(ctx, key); !errors.Is(err, blob.ErrNotFound) {
			t.Errorf("Stat after rejected upload err = %v, want ErrNotFound", err)
		}
		doHTTP(t, req, data, http.StatusOK, http.StatusCreated, http.StatusNoContent)
		if got, _ := mustGet(t, s, key); !bytes.Equal(got, data) {
			t.Errorf("Get after presigned PUT = %q", got)
		}
	}
}

// testDigestReporter checks blob.DigestReporter where a store implements
// it: a reported digest is always that of the object's current content
// (never a stale one after an overwrite), with the object's current info.
func testDigestReporter(t *testing.T, s blob.Store, cfg suite) {
	dr, ok := s.(blob.DigestReporter)
	if !ok {
		if cfg.presignDigests {
			t.Fatal("store declares ReportsPresignedDigests but does not implement blob.DigestReporter")
		}
		t.Skip("store does not implement blob.DigestReporter")
	}
	ctx := testCtx(t)
	_, _, err := dr.StatSHA256(ctx, "digest/missing.txt")
	if errors.Is(err, blob.ErrNotSupported) {
		if cfg.presignDigests {
			t.Fatal("StatSHA256: ErrNotSupported, but the store declares ReportsPresignedDigests")
		}
		t.Skip("store cannot report digests in this configuration")
	}
	if !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("StatSHA256(missing) err = %v, want ErrNotFound", err)
	}
	if _, _, err := dr.StatSHA256(ctx, "../escape"); !errors.Is(err, blob.ErrInvalidKey) {
		t.Errorf("StatSHA256(invalid key) err = %v, want ErrInvalidKey", err)
	}
	check := func(key string, data []byte, required bool) {
		t.Helper()
		info, sum, err := dr.StatSHA256(ctx, key)
		if err != nil {
			t.Fatalf("StatSHA256(%q): %v", key, err)
		}
		sameInfo(t, "StatSHA256("+key+")", info, mustStat(t, s, key))
		switch {
		case sum == "" && required:
			t.Errorf("StatSHA256(%q) reported no digest for a presigned upload with a declared sha256", key)
		case sum != "" && sum != sha256Hex(data):
			t.Errorf("StatSHA256(%q) = %s, want %s (the current content's) or none", key, sum, sha256Hex(data))
		}
	}

	data := []byte("digest me")
	mustPut(t, s, "digest/plain.txt", data, "text/plain")
	check("digest/plain.txt", data, false)

	const key = "digest/presigned.txt"
	req, err := s.PresignPut(ctx, key, exact("text/plain", data), 10*time.Minute)
	if errors.Is(err, blob.ErrNotSupported) {
		if cfg.presignDigests {
			t.Fatal("PresignPut: ErrNotSupported, but the store declares ReportsPresignedDigests")
		}
		return
	}
	if err != nil {
		t.Fatalf("PresignPut(%q): %v", key, err)
	}
	doHTTP(t, req, data, http.StatusOK, http.StatusCreated, http.StatusNoContent)
	check(key, data, cfg.presignDigests)
	// Overwritten (same size) without a declared digest: the old digest
	// must not linger.
	other := bytes.ToUpper(data)
	mustPut(t, s, key, other, "text/plain")
	check(key, other, false)
}

// sendPresigned performs a presigned request with exactly its headers and
// returns the status and body.
func sendPresigned(t *testing.T, pr *blob.PresignedRequest, body []byte) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(testCtx(t), pr.Method, pr.URL, rd)
	if err != nil {
		t.Fatalf("building %s request: %v", pr.Method, err)
	}
	for k, v := range pr.Headers {
		if strings.EqualFold(k, "Host") {
			req.Host = v
			continue
		}
		// net/http sends the real body length whatever the header says.
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s presigned URL: %v", pr.Method, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading presigned %s response: %v", pr.Method, err)
	}
	return resp.StatusCode, out
}

func doHTTP(t *testing.T, pr *blob.PresignedRequest, body []byte, okStatus ...int) []byte {
	t.Helper()
	code, out := sendPresigned(t, pr, body)
	if !slices.Contains(okStatus, code) {
		t.Fatalf("presigned %s status %d: %s", pr.Method, code, out)
	}
	return out
}
