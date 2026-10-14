package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/blob"
	"github.com/thasami/backyard/internal/blob/fs"
	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/identity"
	"github.com/thasami/backyard/internal/sites"
)

// publishStores runs fn once on the in-memory store and once on fs storage.
func publishStores(t *testing.T, fn func(t *testing.T, newEnv func(...envOption) *env)) {
	t.Run("memory", func(t *testing.T) {
		fn(t, func(opts ...envOption) *env { return newEnv(t, opts...) })
	})
	t.Run("fs", func(t *testing.T) {
		fn(t, func(opts ...envOption) *env {
			store, err := fs.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			return newEnv(t, append([]envOption{withStore(store)}, opts...)...)
		})
	})
}

// pub builds a publish request for u from path → content pairs.
func pub(u *identity.User, site string, files ...string) PublishRequest {
	req := PublishRequest{User: u, Site: site, ClientIP: "203.0.113.9", ClientName: "Claude", GrantID: "0123abcd0123abcd0123abcd0123abcd"}
	for i := 0; i+1 < len(files); i += 2 {
		req.Files = append(req.Files, PublishFile{Path: files[i], Data: []byte(files[i+1])})
	}
	return req
}

func (e *env) publish(req PublishRequest) *PublishResult {
	e.t.Helper()
	res, perr := e.gw.Publish(context.Background(), req)
	if perr != nil {
		e.t.Fatalf("Publish: %s %s", perr.ID, perr.Message)
	}
	return res
}

func (e *env) publishErr(req PublishRequest, id, fragment string) *PublishError {
	e.t.Helper()
	res, perr := e.gw.Publish(context.Background(), req)
	if perr == nil {
		e.t.Fatalf("Publish succeeded (%+v), want %s", res, id)
	}
	if perr.ID != id || !strings.Contains(perr.Message, fragment) {
		e.t.Fatalf("Publish error = %s %q, want %s containing %q", perr.ID, perr.Message, id, fragment)
	}
	return perr
}

func (e *env) manifest(site string) map[string]sites.ManifestFile {
	e.t.Helper()
	man, err := e.reg.GetManifest(context.Background(), site)
	if err != nil {
		e.t.Fatal(err)
	}
	out := map[string]sites.ManifestFile{}
	for _, f := range man.Files {
		out[f.Path] = f
	}
	return out
}

func (e *env) meta(site string) *sites.Meta {
	e.t.Helper()
	m, err := e.reg.GetMeta(context.Background(), site)
	if err != nil {
		e.t.Fatal(err)
	}
	return m
}

func (e *env) putObject(site, p, content string) {
	e.t.Helper()
	if _, err := e.blobStore.Put(context.Background(), sites.SiteKey(site, p), strings.NewReader(content), int64(len(content)), "text/plain"); err != nil {
		e.t.Fatal(err)
	}
}

func TestPublishMergeDeleteReplace(t *testing.T) {
	publishStores(t, func(t *testing.T, mk func(...envOption) *env) {
		e := mk()
		res := e.publish(pub(alice, "lunch", "index.html", "<title>Lunch</title>", "app.js", "1"))
		if !res.Created || res.Files != 2 || res.Uploaded != 2 || res.Unchanged != 0 || res.Deleted != 0 || res.Bytes != 21 ||
			res.URL != "https://lunch.backyard.test/" || len(res.Warnings) != 0 {
			t.Fatalf("first publish = %+v", res)
		}

		// Merge: listed files are written, the others kept.
		res = e.publish(pub(alice, "lunch", "style.css", "body{}", "app.js", "2"))
		if res.Created || res.Files != 3 || res.Uploaded != 2 || res.Deleted != 0 {
			t.Fatalf("merge = %+v", res)
		}
		if got := e.siteKeys("lunch"); !slices.Equal(got, []string{"app.js", "index.html", "style.css"}) {
			t.Fatalf("files after merge = %v", got)
		}
		if string(e.object("lunch", "app.js")) != "2" {
			t.Fatal("app.js not replaced")
		}

		// Delete, with a warning for a path that does not exist.
		req := pub(alice, "lunch")
		req.Delete = []string{"style.css", "nope.txt"}
		res = e.publish(req)
		if res.Files != 2 || res.Deleted != 1 || res.Uploaded != 0 ||
			!slices.Equal(res.Warnings, []string{`"nope.txt" in delete does not exist.`}) {
			t.Fatalf("delete = %+v", res)
		}
		if e.object("lunch", "style.css") != nil {
			t.Fatal("style.css not deleted")
		}

		// Replace: the site holds exactly the given files.
		req = pub(alice, "lunch", "index.html", "<title>New</title>")
		req.Replace = true
		res = e.publish(req)
		if res.Files != 1 || res.Deleted != 1 || res.Bytes != 18 {
			t.Fatalf("replace = %+v", res)
		}
		if got := e.siteKeys("lunch"); !slices.Equal(got, []string{"index.html"}) {
			t.Fatalf("files after replace = %v", got)
		}
		man := e.manifest("lunch")
		if len(man) != 1 || man["index.html"].SHA256 != sha256Hex([]byte("<title>New</title>")) {
			t.Fatalf("manifest after replace = %+v", man)
		}
		m := e.meta("lunch")
		if m.FileCount != 1 || m.Bytes != 18 || m.Title != "New" || m.UpdatedBy != alice.ID || !strings.HasPrefix(m.DeployID, "d_") {
			t.Fatalf("meta after replace = %+v", m)
		}
	})
}

func TestPublishUnchangedAndAdoption(t *testing.T) {
	publishStores(t, func(t *testing.T, mk func(...envOption) *env) {
		e := mk()
		e.publish(pub(alice, "u", "index.html", "hi", "a.txt", "a"))
		// An object written out of band (direct sync) is adopted into the
		// manifest with its real digest.
		e.putObject("u", "extra.txt", "out of band")
		puts := 0
		if e.store != nil {
			puts = e.store.PutCount()
		}
		res := e.publish(pub(alice, "u", "index.html", "hi", "a.txt", "a"))
		if res.Uploaded != 0 || res.Unchanged != 2 || res.Files != 3 {
			t.Fatalf("republish = %+v", res)
		}
		if e.store != nil && e.store.PutCount()-puts != 2 { // manifest + meta only
			t.Fatalf("republishing unchanged files stored %d objects, want 2 (manifest and meta)", e.store.PutCount()-puts)
		}
		man := e.manifest("u")
		if mf := man["extra.txt"]; mf.SHA256 != sha256Hex([]byte("out of band")) || mf.Size != 11 || mf.ETag == "" {
			t.Fatalf("adopted entry = %+v", mf)
		}
		if m := e.meta("u"); m.FileCount != 3 || m.Bytes != 14 {
			t.Fatalf("meta = %+v", m)
		}
		// A file replaced out of band (same size, other bytes) is written
		// again, not trusted as unchanged.
		e.putObject("u", "a.txt", "b")
		if res := e.publish(pub(alice, "u", "a.txt", "a")); res.Uploaded != 1 || res.Unchanged != 0 {
			t.Fatalf("publish over an out-of-band change = %+v", res)
		}
		if string(e.object("u", "a.txt")) != "a" {
			t.Fatal("a.txt not rewritten")
		}
	})
}

func TestPublishTreeConflicts(t *testing.T) {
	publishStores(t, func(t *testing.T, mk func(...envOption) *env) {
		e := mk()
		e.publish(pub(alice, "t", "index.html", "x", "docs", "a file"))
		// A kept file where a new file needs a folder.
		e.publishErr(pub(alice, "t", "docs/index.html", "y"), "E14", "both a file and a folder: docs.")
		// Deleting the file in the same call clears the way (on fs the
		// conflicting file goes first).
		req := pub(alice, "t", "docs/index.html", "y")
		req.Delete = []string{"docs"}
		res := e.publish(req)
		if res.Files != 2 || res.Deleted != 1 {
			t.Fatalf("publish = %+v", res)
		}
		if got := e.siteKeys("t"); !slices.Equal(got, []string{"docs/index.html", "index.html"}) {
			t.Fatalf("files = %v", got)
		}
		// Replace clears the reverse conflict (a folder where a file goes).
		req = pub(alice, "t", "index.html", "x", "docs", "a file again")
		req.Replace = true
		if res := e.publish(req); res.Files != 2 || res.Deleted != 1 {
			t.Fatalf("replace = %+v", res)
		}
		if string(e.object("t", "docs")) != "a file again" {
			t.Fatal("docs not written")
		}
		// Conflicts inside the request itself.
		req = pub(alice, "t", "a", "1", "a/b", "2")
		req.Replace = true
		e.publishErr(req, "E14", "both a file and a folder: a.")
	})
}

func TestPublishLimits(t *testing.T) {
	e := newEnv(t, withConfig(func(c *config.Config) {
		c.Sites.MaxFiles = 3
		c.Sites.MaxSiteBytes = 90
		c.Sites.MaxFileBytes = 50
		c.MCP.MaxPublishBytes = 60
	}))
	e.publish(pub(alice, "l", "index.html", "x", "a", "1", "b", "2"))
	e.publishErr(pub(alice, "l", "c", "3"), "E13", "The site would have 4 files; this Backyard allows 3 per site.")
	req := pub(alice, "l", "c", "3")
	req.Delete = []string{"a"}
	if res := e.publish(req); res.Files != 3 {
		t.Fatalf("publish = %+v", res)
	}
	big := strings.Repeat("x", 45)
	e.publish(pub(alice, "l", "c", big))
	e.publishErr(pub(alice, "l", "b", big), "E13", "The site would be 91 B; this Backyard allows 90 B per site.")
	e.publishErr(pub(alice, "l", "c", strings.Repeat("x", 51)), "E12", "c is 51 B; files may be at most 50 B.")
	e.publishErr(pub(alice, "l", "c", big, "b", big), "E11", "This call carries 90 B of file content; the limit is 60 B per call.")
	// Nothing changed by the refusals.
	if string(e.object("l", "b")) != "2" {
		t.Fatal("a refused publish changed the site")
	}
}

func TestPublishRequestChecks(t *testing.T) {
	e := newEnv(t, withConfig(func(c *config.Config) { c.Sites.Reserved = append(c.Sites.Reserved, "taken") }))
	long := strings.Repeat("a", 64)
	cases := []struct {
		name     string
		req      PublishRequest
		id, frag string
	}{
		{"bad name", pub(alice, "Bad_Name", "index.html", "x"), "E2", `"Bad_Name" is not a valid site name`},
		{"too long", pub(alice, long, "index.html", "x"), "E2", "at most 63 characters"},
		{"default reserved", pub(alice, "api", "index.html", "x"), "E3", `"api" is reserved on this Backyard`},
		{"configured reserved", pub(alice, "taken", "index.html", "x"), "E3", "reserved"},
		{"nothing", pub(alice, "ok"), "E4", "Nothing to publish"},
		{"replace and delete", func() PublishRequest {
			r := pub(alice, "ok", "index.html", "x")
			r.Replace, r.Delete = true, []string{"a"}
			return r
		}(), "E5", `Use either "replace" or "delete", not both.`},
		{"replace nothing", func() PublishRequest {
			r := pub(alice, "ok")
			r.Replace = true
			return r
		}(), "E4", "Nothing to publish"},
		{"dot segment", pub(alice, "ok", "../x", "x"), "E8", `"../x" is not an allowed path: path may not contain "." or ".." segments.`},
		{"git", pub(alice, "ok", ".git/config", "x"), "E8", `".git" segment`},
		{"reserved prefix", pub(alice, "ok", "__backyard/x", "x"), "E8", "__backyard/"},
		{"duplicate", pub(alice, "ok", "a", "1", "a", "2"), "E9", `"a" appears more than once.`},
		{"in delete", func() PublishRequest {
			r := pub(alice, "ok", "a", "1")
			r.Delete = []string{"a"}
			return r
		}(), "E10", `"a" is both in files and in delete.`},
		{"bad delete path", func() PublishRequest {
			r := pub(alice, "ok")
			r.Delete = []string{"/abs"}
			return r
		}(), "E8", `"/abs" is not an allowed path`},
		{"content type", func() PublishRequest {
			r := pub(alice, "ok", "a", "1")
			r.Files[0].ContentType = "text/plain\n"
			return r
		}(), "E1", "Invalid arguments: files[0].content_type"},
		{"too many files", func() PublishRequest {
			r := pub(alice, "ok")
			for i := range MaxPublishList + 1 {
				r.Files = append(r.Files, PublishFile{Path: "f" + string(rune('a'+i%26)) + strings.Repeat("x", i/26)})
			}
			return r
		}(), "E1", "at most 1000"},
	}
	for _, c := range cases {
		e.t = t
		t.Run(c.name, func(t *testing.T) {
			e.t = t
			e.publishErr(c.req, c.id, c.frag)
		})
	}
	e.t = t
	// A merge that would leave no file.
	e.publish(pub(alice, "one", "index.html", "x"))
	req := pub(alice, "one")
	req.Delete = []string{"index.html"}
	e.publishErr(req, "E6", "A site needs at least one file.")
}

func TestCheckPublishDecodeOrder(t *testing.T) {
	e := newEnv(t)
	bad := &PublishError{ID: "E7", Message: "bad base64"}
	var decoded []int
	decode := func(i int) ([]byte, *PublishError) {
		decoded = append(decoded, i)
		if i == 1 {
			return nil, bad
		}
		return []byte("ok"), nil
	}
	// Path checks of a file come before its decoding; its decoding before
	// the checks of the next file.
	req := PublishRequest{User: alice, Site: "u", Files: []PublishFile{{Path: "a"}, {Path: "b"}, {Path: "../c"}}}
	if perr := e.gw.CheckPublish(&req, decode); perr != bad || !slices.Equal(decoded, []int{0, 1}) {
		t.Fatalf("CheckPublish = %v after decoding %v", perr, decoded)
	}
	decoded = nil
	req = PublishRequest{User: alice, Site: "u", Files: []PublishFile{{Path: "a"}, {Path: "../b"}}}
	if perr := e.gw.CheckPublish(&req, decode); perr == nil || perr.ID != "E8" || !slices.Equal(decoded, []int{0}) {
		t.Fatalf("CheckPublish = %v after decoding %v", perr, decoded)
	}
	// A bad site name wins over everything in the files.
	req = PublishRequest{User: alice, Site: "-", Files: []PublishFile{{Path: "b"}}}
	if perr := e.gw.CheckPublish(&req, decode); perr == nil || perr.ID != "E2" {
		t.Fatalf("CheckPublish = %v", perr)
	}
	// Content types default from the extension, then the content.
	req = PublishRequest{User: alice, Site: "u", Files: []PublishFile{{Path: "a.css", Data: []byte("x")}, {Path: "noext", Data: []byte("<html><body>")}}}
	if perr := e.gw.CheckPublish(&req, nil); perr != nil {
		t.Fatal(perr)
	}
	if !strings.HasPrefix(req.Files[0].ContentType, "text/css") || !strings.HasPrefix(req.Files[1].ContentType, "text/html") {
		t.Fatalf("content types = %q, %q", req.Files[0].ContentType, req.Files[1].ContentType)
	}
}

func TestPublishOwnershipAndOverwrite(t *testing.T) {
	e := newEnv(t, withConfig(func(c *config.Config) {
		c.Sites.Ownership = "owner"
		c.Auth.Admins = []string{admin.ID}
	}))
	e.publish(pub(alice, "own", "index.html", "a"))
	if m := e.meta("own"); !slices.Equal(m.Owners, []string{alice.ID}) || m.CreatedBy != alice.ID {
		t.Fatalf("meta after first publish = %+v", m)
	}
	req := pub(bob, "own", "index.html", "b")
	req.Overwrite = true
	e.publishErr(req, "E15", "own belongs to alice@acme.com; you are not an owner.")
	// An admin passes the ownership check but meets the overwrite guard.
	perr := e.publishErr(pub(admin, "own", "index.html", "c"), "E16", "own was last published by alice@acme.com on ")
	if !strings.Contains(perr.Message, ` UTC. Nothing was changed. Ask the user whether to replace it; if so, call again with "overwrite": true`) {
		t.Fatalf("E16 = %q", perr.Message)
	}
	if string(e.object("own", "index.html")) != "a" {
		t.Fatal("the guard changed the site")
	}
	req = pub(admin, "own", "index.html", "c")
	req.Overwrite = true
	e.publish(req)
	if m := e.meta("own"); m.UpdatedBy != admin.ID || !slices.Equal(m.Owners, []string{alice.ID}) {
		t.Fatalf("meta after admin publish = %+v", m)
	}
	// The guard matches the publisher by id or email, case-insensitively.
	e.publish(pub(&identity.User{ID: "sub:42", Email: "ROOT@acme.com", Name: "Root", Admin: true}, "own", "x.txt", "1"))
}

func TestPublishOverwriteGuardChargesNothing(t *testing.T) {
	e := newEnv(t, withConfig(func(c *config.Config) { c.RateLimit.DeploysPerHour = 1 }))
	e.gw.limiter = newActorLimiter(1)
	e.publish(pub(alice, "g", "index.html", "a"))
	e.publishErr(pub(bob, "g", "index.html", "b"), "E16", "last published by alice@acme.com")
	req := pub(bob, "g", "index.html", "b")
	req.Overwrite = true
	e.publish(req) // bob's only token was not spent by the refusal
	e.publishErr(req, "E19", "Publishing limit reached (1 per hour). Try again in 60 minutes.")
}

func TestPublishAnonymous(t *testing.T) {
	guestA := &identity.User{ID: "anon:aaaaaaaaaaaaaaaa", Name: "Guest aaaa", Anonymous: true}
	guestB := &identity.User{ID: "anon:bbbbbbbbbbbbbbbb", Name: "Guest bbbb", Anonymous: true}

	e := newEnv(t)
	e.cfg.Deploy.AllowAnonymous = false
	e.publishErr(pub(guestA, "g", "index.html", "x"), "E17", "does not accept anonymous publishing")

	e = newEnv(t, withConfig(func(c *config.Config) {
		c.Deploy.AllowAnonymous = true
		c.Sites.Ownership = "owner"
		c.RateLimit.DeploysPerHour = 2
	}))
	e.gw.limiter = newActorLimiter(2)
	e.publish(pub(guestA, "g", "index.html", "x"))
	if m := e.meta("g"); len(m.Owners) != 0 || m.UpdatedBy != guestA.ID {
		t.Fatalf("a guest became an owner: %+v", m)
	}
	e.publish(pub(guestA, "g", "a", "1"))
	// The quota is keyed user:<id>, guests included: another guest from the
	// same address has its own budget.
	perr := e.publishErr(pub(guestA, "g", "b", "2"), "E19", "Publishing limit reached (2 per hour). Try again in 30 minutes.")
	if perr.RetryAfter <= 0 || perr.RetryAfter > 30*time.Minute {
		t.Fatalf("RetryAfter = %v", perr.RetryAfter)
	}
	e.publish(pub(guestB, "h", "index.html", "y"))
	// Guests meet the overwrite guard too (their ids are stable per grant).
	e.publishErr(pub(guestB, "g", "c", "3"), "E16", "last published by anon:aaaaaaaaaaaaaaaa")
}

func TestPublishTitleAndWarnings(t *testing.T) {
	e := newEnv(t)
	res := e.publish(pub(alice, "w", "app.js", "1"))
	if !slices.Equal(res.Warnings, []string{"No index.html at the site root: https://w.backyard.test/ answers 404 until one is published."}) {
		t.Fatalf("warnings = %q", res.Warnings)
	}
	html := `<html><head><title>Lunch poll</title><meta name="description" content="Vote for lunch"></head></html>`
	if res := e.publish(pub(alice, "w", "index.html", html)); len(res.Warnings) != 0 {
		t.Fatalf("warnings = %q", res.Warnings)
	}
	if m := e.meta("w"); m.Title != "Lunch poll" || m.Description != "Vote for lunch" {
		t.Fatalf("meta = %+v", m)
	}
	// A kept index.html keeps the title (read back from storage).
	e.publish(pub(alice, "w", "style.css", "x"))
	if m := e.meta("w"); m.Title != "Lunch poll" || m.FileCount != 3 {
		t.Fatalf("meta = %+v", m)
	}
}

func TestPublishAudit(t *testing.T) {
	e := newEnv(t)
	e.publish(pub(alice, "aud", "index.html", "x"))
	var rec map[string]any
	for line := range strings.SplitSeq(e.logs.String(), "\n") {
		if strings.Contains(line, `"action":"deploy.commit"`) {
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatal(err)
			}
		}
	}
	want := map[string]any{"audit": true, "actor": alice.ID, "site": "aud", "ip": "203.0.113.9", "via": "mcp",
		"client": "Claude", "grant": "0123abcd", "files": 1.0, "uploaded": 1.0, "deleted": 0.0}
	for k, v := range want {
		if rec[k] != v {
			t.Errorf("audit %s = %v, want %v (record %v)", k, rec[k], v, rec)
		}
	}
	if s, _ := rec["deploy_id"].(string); !strings.HasPrefix(s, "d_") {
		t.Errorf("audit deploy_id = %v", rec["deploy_id"])
	}
}

// opStore records the order of writes and deletions and can fail writes.
type opStore struct {
	blob.Store
	mu      sync.Mutex
	ops     []string
	failPut string // key suffix whose Put fails
}

func (s *opStore) Put(ctx context.Context, key string, r io.Reader, size int64, ct string) (*blob.ObjectInfo, error) {
	s.mu.Lock()
	s.ops = append(s.ops, "put "+key)
	fail := s.failPut != "" && strings.HasSuffix(key, s.failPut)
	s.mu.Unlock()
	if fail {
		return nil, errors.New("disk on fire")
	}
	return s.Store.Put(ctx, key, r, size, ct)
}

func (s *opStore) Delete(ctx context.Context, key string) error {
	s.mu.Lock()
	s.ops = append(s.ops, "delete "+key)
	s.mu.Unlock()
	return s.Store.Delete(ctx, key)
}

func TestPublishDeletionsLast(t *testing.T) {
	publishStores(t, func(t *testing.T, mk func(...envOption) *env) {
		base := mk()
		ops := &opStore{Store: base.blobStore}
		e := mk(withStore(ops))
		e.publish(pub(alice, "d", "index.html", "x", "a.txt", "a", "b.txt", "b"))
		ops.mu.Lock()
		ops.ops = nil
		ops.mu.Unlock()
		req := pub(alice, "d", "index.html", "y")
		req.Replace = true
		if res := e.publish(req); res.Deleted != 2 {
			t.Fatalf("publish = %+v", res)
		}
		ops.mu.Lock()
		defer ops.mu.Unlock()
		metaAt, firstDelete := -1, -1
		for i, op := range ops.ops {
			if op == "put "+sites.MetaKey("d") {
				metaAt = i
			}
			if strings.HasPrefix(op, "delete ") && firstDelete < 0 {
				firstDelete = i
			}
		}
		if metaAt < 0 || firstDelete < metaAt {
			t.Fatalf("operations %q: deletions must follow the meta switch", ops.ops)
		}
	})
}

func TestPublishStoreFailure(t *testing.T) {
	base := newEnv(t)
	ops := &opStore{Store: base.blobStore, failPut: "/b.txt"}
	e := newEnv(t, withStore(ops))
	e.publish(pub(alice, "f", "index.html", "x"))
	perr := e.publishErr(pub(alice, "f", "a.txt", "a", "b.txt", "b"), "E21", "Publishing failed: Backyard could not store the files")
	if strings.Contains(perr.Message, "disk on fire") {
		t.Fatalf("E21 leaks the cause: %q", perr.Message)
	}
	if !strings.Contains(e.logs.String(), "disk on fire") {
		t.Fatal("the cause was not logged")
	}
	if m := e.manifest("f"); len(m) != 1 {
		t.Fatalf("a failed publish switched the manifest: %v", m)
	}
	// Publishing again completes it.
	ops.failPut = ""
	if res := e.publish(pub(alice, "f", "a.txt", "a", "b.txt", "b")); res.Files != 3 {
		t.Fatalf("retry = %+v", res)
	}
}

func TestPublishLargeFileBinary(t *testing.T) {
	publishStores(t, func(t *testing.T, mk func(...envOption) *env) {
		e := mk()
		data := bytes.Repeat([]byte{0, 1, 2, 0xff}, 1<<18) // 1 MiB of binary
		req := PublishRequest{User: alice, Site: "bin", Files: []PublishFile{{Path: "index.html", Data: []byte("x")}, {Path: "logo.png", Data: data}}}
		if res := e.publish(req); res.Bytes != int64(len(data))+1 {
			t.Fatalf("publish = %+v", res)
		}
		if !bytes.Equal(e.object("bin", "logo.png"), data) {
			t.Fatal("binary content changed")
		}
		info, err := e.blobStore.Stat(context.Background(), sites.SiteKey("bin", "logo.png"))
		if err != nil {
			t.Fatal(err)
		}
		if info.ContentType != "" && info.ContentType != "image/png" {
			t.Fatalf("content type = %q", info.ContentType)
		}
	})
}

func TestFormatBytes(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 131072: "128.0 KiB", 4 << 20: "4.0 MiB", 1288490189: "1.2 GiB"} {
		if got := FormatBytes(n); got != want {
			t.Errorf("FormatBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestInfoMCPURL(t *testing.T) {
	e := newEnv(t)
	if e.gw.Info().MCPURL != "" {
		t.Fatal("mcp_url without MCPURL")
	}
	e.gw.opts.MCPURL = "https://hub.example/__backyard/mcp"
	b, _ := json.Marshal(e.gw.Info())
	if !strings.Contains(string(b), `"mcp_url":"https://hub.example/__backyard/mcp"`) {
		t.Fatalf("info = %s", b)
	}
}
