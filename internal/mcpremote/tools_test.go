package mcpremote

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/auth"
	"github.com/thasami/backyard/internal/blob"
	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/deploy"
	"github.com/thasami/backyard/internal/docstore"
	"github.com/thasami/backyard/internal/docstore/memory"
	"github.com/thasami/backyard/internal/sites"
)

// png1x1 is a 1×1 transparent PNG.
var png1x1, _ = base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==")

func TestDocs(t *testing.T) {
	e := newTenv(t)
	if text := e.toolOK(aliceToken, "backyard_docs", nil, nil); !strings.Contains(text, "Backyard") {
		t.Fatalf("skill = %.200q", text)
	}
	if text := e.toolOK(aliceToken, "backyard_docs", map[string]any{"topic": "api"}, nil); !strings.Contains(text, "backyard.db") {
		t.Fatalf("api = %.200q", text)
	}
	var topics []string
	e = newTenv(t, tenvOption{opts: func(o *Options) {
		o.DocsFor = func(topic string) (string, error) {
			topics = append(topics, topic)
			if topic == "deploy" {
				return "", errors.New("gone")
			}
			return "# " + topic, nil
		}
	}})
	if text := e.toolOK(aliceToken, "backyard_docs", map[string]any{}, nil); text != "# skill" {
		t.Fatalf("default topic = %q", text)
	}
	e.toolErr(aliceToken, "backyard_docs", map[string]any{"topic": "deploy"}, "Internal error (request ")
	if !slices.Equal(topics, []string{"skill", "deploy"}) {
		t.Fatalf("topics = %v", topics)
	}
}

type whoamiOut struct {
	User struct {
		ID        string  `json:"id"`
		Email     *string `json:"email"`
		Name      string  `json:"name"`
		Admin     bool    `json:"admin"`
		Anonymous bool    `json:"anonymous"`
	} `json:"user"`
	HubURL          string `json:"hub_url"`
	SiteURLTemplate string `json:"site_url_template"`
	CanPublish      bool   `json:"can_publish"`
	Limits          struct {
		MaxPublishBytes int64 `json:"max_publish_bytes"`
		MaxFiles        int   `json:"max_files"`
		MaxFileBytes    int64 `json:"max_file_bytes"`
		MaxSiteBytes    int64 `json:"max_site_bytes"`
		MaxReadBytes    int64 `json:"max_read_bytes"`
	} `json:"limits"`
	Connection struct {
		Client    string `json:"client"`
		ExpiresAt string `json:"expires_at"`
	} `json:"connection"`
}

func TestWhoami(t *testing.T) {
	e := newTenv(t, tenvOption{cfg: func(c *config.Config) { c.Auth.Admins = []string{bob.Email} }})
	var w whoamiOut
	text := e.toolOK(aliceToken, "backyard_whoami", nil, &w)
	if !strings.HasPrefix(text, `{"user":{"id":"alice@acme.com","email":"alice@acme.com","name":"Alice","admin":false,"anonymous":false},"hub_url":`) {
		t.Fatalf("whoami = %s", text)
	}
	if w.HubURL != hubURL || w.SiteURLTemplate != "https://{site}.backyard.test/" || !w.CanPublish ||
		w.Limits.MaxPublishBytes != 4194304 || w.Limits.MaxFiles != 10000 || w.Limits.MaxFileBytes != 100<<20 ||
		w.Limits.MaxSiteBytes != 500<<20 || w.Limits.MaxReadBytes != 131072 ||
		w.Connection.Client != "Claude" || w.Connection.ExpiresAt != "2023-12-14T22:13:20Z" {
		t.Fatalf("whoami = %s", text)
	}
	// Admin is recomputed from the configuration.
	e.toolOK(bobToken, "backyard_whoami", nil, &w)
	if !w.User.Admin {
		t.Fatal("auth.admins not applied")
	}
	// A guest without anonymous deploys cannot publish; email is omitted.
	w = whoamiOut{}
	e.toolOK(guestToken, "backyard_whoami", nil, &w)
	if !w.User.Anonymous || w.User.Admin || w.CanPublish || w.User.Email != nil {
		t.Fatalf("guest whoami = %+v", w)
	}
	e = newTenv(t, tenvOption{cfg: func(c *config.Config) {
		c.Auth.Mode, c.Deploy.AllowAnonymous = auth.ModeNone, true
		c.Server.URLMode = "path"
	}})
	e.toolOK(guestToken, "backyard_whoami", nil, &w)
	if !w.CanPublish || w.SiteURLTemplate != hubURL+"/s/{site}/" {
		t.Fatalf("guest whoami with anonymous deploys = %+v", w)
	}
	e = newTenv(t, tenvOption{noGateway: true})
	if e.toolOK(aliceToken, "backyard_whoami", nil, &w); w.CanPublish {
		t.Fatal("can_publish without the embedded gateway")
	}
}

type publishOut struct {
	Site      string   `json:"site"`
	URL       string   `json:"url"`
	Created   bool     `json:"created"`
	Files     int      `json:"files"`
	Uploaded  int      `json:"uploaded"`
	Unchanged int      `json:"unchanged"`
	Deleted   int      `json:"deleted"`
	Bytes     int64    `json:"bytes"`
	Warnings  []string `json:"warnings"`
}

func TestPublishFlow(t *testing.T) {
	e := newTenv(t)
	html := "<!doctype html><title>Lunch poll</title><p>Vote</p>"
	args := map[string]any{"site": "lunch-poll", "files": []any{
		map[string]any{"path": "index.html", "content": html},
		map[string]any{"path": "logo.png", "content": base64.StdEncoding.EncodeToString(png1x1), "encoding": "base64"},
	}}
	var out publishOut
	text := e.toolOK(aliceToken, "backyard_publish", args, &out)
	want := fmt.Sprintf(`{"site":"lunch-poll","url":"https://lunch-poll.backyard.test/","created":true,"files":2,"uploaded":2,"unchanged":0,"deleted":0,"bytes":%d,"warnings":[]}`,
		len(html)+len(png1x1))
	if text != want {
		t.Fatalf("publish = %s\nwant      %s", text, want)
	}
	if string(e.object("lunch-poll", "index.html")) != html || !bytes.Equal(e.object("lunch-poll", "logo.png"), png1x1) {
		t.Fatal("stored bytes differ")
	}
	info, err := e.store.Stat(context.Background(), sites.SiteKey("lunch-poll", "logo.png"))
	if err != nil || info.ContentType != "image/png" {
		t.Fatalf("logo.png: %+v %v", info, err)
	}
	// The same files again: nothing uploaded.
	e.toolOK(aliceToken, "backyard_publish", args, &out)
	if out.Uploaded != 0 || out.Unchanged != 2 || out.Created || out.Files != 2 {
		t.Fatalf("republish = %+v", out)
	}
	// Merge, delete, replace.
	e.toolOK(aliceToken, "backyard_publish", map[string]any{"site": "lunch-poll", "files": files("app.js", "go()")}, &out)
	if out.Files != 3 {
		t.Fatalf("merge = %+v", out)
	}
	e.toolOK(aliceToken, "backyard_publish", map[string]any{"site": "lunch-poll", "files": []any{}, "delete": []string{"logo.png", "gone.txt"}}, &out)
	if out.Files != 2 || out.Deleted != 1 || !slices.Equal(out.Warnings, []string{`"gone.txt" in delete does not exist.`}) {
		t.Fatalf("delete = %+v", out)
	}
	e.toolOK(aliceToken, "backyard_publish", map[string]any{"site": "lunch-poll", "files": files("index.html", "<p>new</p>"), "replace": true}, &out)
	if out.Files != 1 || out.Deleted != 1 {
		t.Fatalf("replace = %+v", out)
	}
	// W1 and W2.
	e.toolOK(aliceToken, "backyard_publish", map[string]any{"site": "media", "files": files("font.woff2", "not really")}, &out)
	if !slices.Equal(out.Warnings, []string{
		"No index.html at the site root: https://media.backyard.test/ answers 404 until one is published.",
		`font.woff2 was sent as text but looks binary; binary files need "encoding": "base64".`,
	}) {
		t.Fatalf("warnings = %q", out.Warnings)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.published) != 6 || e.published[0] != "lunch-poll ok" {
		t.Fatalf("OnPublish calls = %v", e.published)
	}
	// The audit record names the connection.
	logs := e.logs.String()
	for _, frag := range []string{`"action":"deploy.commit"`, `"via":"mcp"`, `"client":"Claude"`, `"grant":"0123abcd"`, `"ip":"203.0.113.9"`} {
		if !strings.Contains(logs, frag) {
			t.Errorf("audit lacks %s", frag)
		}
	}
	if strings.Contains(logs, aliceToken) {
		t.Fatal("the access token was logged")
	}
}

func TestPublishErrors(t *testing.T) {
	e := newTenv(t, tenvOption{cfg: func(c *config.Config) {
		c.Sites.MaxFiles = 3
		c.Sites.MaxFileBytes = 1 << 20
		c.Sites.Ownership = "owner"
		c.Sites.Reserved = append(c.Sites.Reserved, "taken")
	}})
	pubArgs := func(site string, extra map[string]any, pairs ...string) map[string]any {
		a := map[string]any{"site": site, "files": files(pairs...)}
		for k, v := range extra {
			a[k] = v
		}
		return a
	}
	e.toolOK(aliceToken, "backyard_publish", pubArgs("mine", nil, "index.html", "x"), nil)
	cases := []struct {
		name, token string
		args        map[string]any
		want        string
	}{
		{"E1 schema", aliceToken, map[string]any{"site": "x", "files": "nope"}, "Invalid arguments: "},
		{"E1 content type", aliceToken, map[string]any{"site": "x", "files": []any{map[string]any{"path": "a", "content": "1", "content_type": "a\x01b"}}}, "Invalid arguments: files[0].content_type"},
		{"E3", aliceToken, pubArgs("api", nil, "index.html", "x"), `"api" is reserved on this Backyard; choose another name.`},
		{"E3 configured", aliceToken, pubArgs("taken", nil, "index.html", "x"), "reserved"},
		{"E4", aliceToken, pubArgs("x", nil), "Nothing to publish: give files, delete, or both."},
		{"E5", aliceToken, pubArgs("x", map[string]any{"replace": true, "delete": []string{"a"}}, "a", "1"), `Use either "replace" or "delete", not both.`},
		{"E6", aliceToken, pubArgs("mine", map[string]any{"delete": []string{"index.html"}}), "A site needs at least one file."},
		{"E7", aliceToken, map[string]any{"site": "x", "files": []any{map[string]any{"path": "a.png", "content": "!!", "encoding": "base64"}}}, `files[0] (a.png): "content" is not valid base64.`},
		{"E8", aliceToken, pubArgs("x", nil, "../x", "1"), `"../x" is not an allowed path: path may not contain "." or ".." segments.`},
		{"E8 delete", aliceToken, pubArgs("x", map[string]any{"delete": []string{"a//b"}}, "a", "1"), `"a//b" is not an allowed path: path contains an empty segment.`},
		{"E9", aliceToken, pubArgs("x", nil, "a", "1", "a", "2"), `"a" appears more than once.`},
		{"E10", aliceToken, pubArgs("x", map[string]any{"delete": []string{"a"}}, "a", "1"), `"a" is both in files and in delete.`},
		{"E12", aliceToken, pubArgs("x", nil, "big.txt", strings.Repeat("x", 1<<20+1)), "big.txt is 1.0 MiB; files may be at most 1.0 MiB."},
		{"E13", aliceToken, pubArgs("mine", nil, "a", "1", "b", "2", "c", "3"), "The site would have 4 files; this Backyard allows 3 per site."},
		{"E14", aliceToken, pubArgs("mine", nil, "index.html/x", "1"), "These paths would be both a file and a folder: index.html."},
		{"E15", bobToken, pubArgs("mine", map[string]any{"overwrite": true}, "a", "1"), "mine belongs to alice@acme.com; you are not an owner. Ask an owner to add you, or publish under another name."},
		{"E16", adminToken, pubArgs("mine", nil, "a", "1"), `mine was last published by alice@acme.com on `},
		{"E17", guestToken, pubArgs("x", nil, "a", "1"), "This Backyard has no sign-in and does not accept anonymous publishing, so this connection can only read."},
	}
	for _, c := range cases {
		text := e.toolErr(c.token, "backyard_publish", c.args, c.want)
		if c.name == "E16" && !strings.Contains(text, ` UTC. Nothing was changed. Ask the user whether to replace it; if so, call again with "overwrite": true, otherwise choose another name.`) {
			t.Errorf("E16 = %q", text)
		}
	}
	// E2: the schema's pattern catches bad names first; a name that passes
	// the pattern but not the rules does not exist, so check the text on
	// the gateway path directly.
	e.toolErr(aliceToken, "backyard_publish", pubArgs("Bad", nil, "a", "1"), "Invalid arguments: ")
	if string(e.object("mine", "index.html")) != "x" {
		t.Fatal("a refused publish changed the site")
	}
	// E18: no embedded gateway.
	e = newTenv(t, tenvOption{noGateway: true})
	e.toolErr(aliceToken, "backyard_publish", pubArgs("x", nil, "a", "1"), "Publishing through this Backyard's hub is turned off (deploy.embedded_gateway: false).")
	// E19: deploys_per_hour, keyed by user.
	e = newTenv(t, tenvOption{cfg: func(c *config.Config) { c.RateLimit.DeploysPerHour = 1 }})
	e.toolOK(aliceToken, "backyard_publish", pubArgs("q", nil, "index.html", "x"), nil)
	e.toolErr(aliceToken, "backyard_publish", pubArgs("q", nil, "index.html", "y"), "Publishing limit reached (1 per hour). Try again in 60 minutes.")
	e.toolOK(bobToken, "backyard_publish", pubArgs("r", nil, "index.html", "x"), nil)
}

func TestPublishPerCallCap(t *testing.T) {
	e := newTenv(t)
	limit := int(deploy.MaxPublishBytes(e.cfg))
	// Two files that together are exactly at the cap.
	half := strings.Repeat("a", limit/2)
	e.toolOK(aliceToken, "backyard_publish", map[string]any{"site": "cap", "files": files("index.html", half, "b.txt", half)}, nil)
	// One byte more is refused, naming the per-call limit; base64 counts
	// the decoded bytes.
	over := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, limit/2+1))
	text := e.toolErr(aliceToken, "backyard_publish", map[string]any{"site": "cap", "files": []any{
		map[string]any{"path": "a.txt", "content": half},
		map[string]any{"path": "b.bin", "content": over, "encoding": "base64"},
	}}, "per call")
	if text != "This call carries 4.0 MiB of file content; the limit is 4.0 MiB per call. Send the files over several calls (files you do not list are kept)." {
		t.Fatalf("E11 = %q", text)
	}
}

func TestBase64Variants(t *testing.T) {
	data := []byte{0xfb, 0xff, 0xbf, 0x00, 0x7f}
	std := base64.StdEncoding.EncodeToString(data)
	for name, in := range map[string]string{
		"std padded": std, "std unpadded": strings.TrimRight(std, "="),
		"url padded": base64.URLEncoding.EncodeToString(data), "url unpadded": base64.RawURLEncoding.EncodeToString(data),
	} {
		got, ok := decodeBase64(in)
		if !ok || !bytes.Equal(got, data) {
			t.Errorf("%s %q: %v %x", name, in, ok, got)
		}
	}
	if got, ok := decodeBase64(""); !ok || len(got) != 0 {
		t.Errorf("empty: %v %x", ok, got)
	}
	for _, in := range []string{std[:4] + " " + std[4:], std + "\n", "+/-_", "QQ=", "QQ===", "QUJD=", "Q", "QQ=A", "**"} {
		if _, ok := decodeBase64(in); ok {
			t.Errorf("%q decoded", in)
		}
	}
	// Through the tool: whitespace is E7.
	e := newTenv(t)
	wrapped := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("x"), 100))
	wrapped = wrapped[:76] + "\n" + wrapped[76:]
	e.toolErr(aliceToken, "backyard_publish", map[string]any{"site": "b64", "files": []any{
		map[string]any{"path": "index.html", "content": "ok"},
		map[string]any{"path": "a.bin", "content": wrapped, "encoding": "base64"},
	}}, `files[1] (a.bin): "content" is not valid base64.`)
	e.toolOK(aliceToken, "backyard_publish", map[string]any{"site": "b64", "files": []any{
		map[string]any{"path": "a.bin", "content": base64.RawURLEncoding.EncodeToString(data), "encoding": "base64"},
	}}, nil)
	if !bytes.Equal(e.object("b64", "a.bin"), data) {
		t.Fatal("url-safe base64 stored wrong bytes")
	}
}

func TestPublishStoreFailure(t *testing.T) {
	e := newTenv(t)
	e.h.publish = func(context.Context, deploy.PublishRequest) (*deploy.PublishResult, *deploy.PublishError) {
		return nil, &deploy.PublishError{ID: "E21", Message: "Publishing failed: details"}
	}
	text := e.toolErr(aliceToken, "backyard_publish", map[string]any{"site": "f", "files": files("index.html", "x")}, "Publishing failed: Backyard could not store the files (request ")
	if !strings.HasSuffix(text, "). Publishing the same files again completes it.") {
		t.Fatalf("E21 = %q", text)
	}
	if e.published[0] != "f error" {
		t.Fatalf("OnPublish = %v", e.published)
	}
}

func TestListSites(t *testing.T) {
	e := newTenv(t, tenvOption{cfg: func(c *config.Config) { c.Sites.Ownership = "owner" }})
	e.toolOK(aliceToken, "backyard_publish", map[string]any{"site": "lunch-poll", "files": files("index.html", "<title>Lunch</title><meta name=description content='Vote daily'>")}, nil)
	e.toolOK(bobToken, "backyard_publish", map[string]any{"site": "zeta", "files": files("index.html", "<title>Zeta</title>")}, nil)
	e.putObject("synced", "index.html", []byte("raw")) // no meta document
	type site struct {
		Name        string   `json:"name"`
		URL         string   `json:"url"`
		Title       string   `json:"title"`
		Description string   `json:"description"`
		UpdatedAt   string   `json:"updated_at"`
		UpdatedBy   string   `json:"updated_by"`
		FileCount   int      `json:"file_count"`
		Bytes       int64    `json:"bytes"`
		Owners      []string `json:"owners"`
	}
	var out struct {
		Sites     []site `json:"sites"`
		Total     int    `json:"total"`
		Truncated bool   `json:"truncated"`
	}
	text := e.toolOK(aliceToken, "backyard_list_sites", nil, &out)
	if out.Total != 3 || out.Truncated || len(out.Sites) != 3 {
		t.Fatalf("list = %s", text)
	}
	byName := map[string]site{}
	for _, s := range out.Sites {
		byName[s.Name] = s
	}
	lp := byName["lunch-poll"]
	if lp.URL != "https://lunch-poll.backyard.test/" || lp.Title != "Lunch" || lp.Description != "Vote daily" || lp.UpdatedBy != alice.ID ||
		lp.FileCount != 1 || !slices.Equal(lp.Owners, []string{alice.ID}) || lp.UpdatedAt == "" {
		t.Fatalf("lunch-poll = %+v", lp)
	}
	if strings.Contains(text, `"title":""`) || strings.Contains(text, `"owners":[]`) || strings.Contains(text, `"updated_by":""`) {
		t.Fatalf("empty fields are not omitted: %s", text)
	}
	e.toolOK(aliceToken, "backyard_list_sites", map[string]any{"query": "VOTE"}, &out)
	if out.Total != 1 || out.Sites[0].Name != "lunch-poll" {
		t.Fatalf("query = %+v", out)
	}
	e.toolOK(aliceToken, "backyard_list_sites", map[string]any{"query": "zet"}, &out)
	if out.Total != 1 || out.Sites[0].Name != "zeta" {
		t.Fatalf("query = %+v", out)
	}
	// Without ownership, owners are never shown; at most 200 sites.
	e = newTenv(t)
	for i := range 205 {
		if err := e.reg.PutMeta(context.Background(), &sites.Meta{Name: fmt.Sprintf("s%03d", i), UpdatedAt: time.Unix(int64(i), 0), Owners: []string{"x"}}); err != nil {
			t.Fatal(err)
		}
	}
	text = e.toolOK(aliceToken, "backyard_list_sites", nil, &out)
	if out.Total != 205 || !out.Truncated || len(out.Sites) != 200 || out.Sites[0].Name != "s204" || strings.Contains(text, "owners") {
		t.Fatalf("list: total %d truncated %v len %d first %s", out.Total, out.Truncated, len(out.Sites), out.Sites[0].Name)
	}
}

func TestSiteFiles(t *testing.T) {
	e := newTenv(t)
	e.toolErr(aliceToken, "backyard_site_files", map[string]any{"site": "nope"}, `There is no site named "nope" on this Backyard.`)
	for _, p := range []string{"b.js", "a/z.css", "index.html"} {
		e.putObject("s1", p, []byte(p))
	}
	text := e.toolOK(aliceToken, "backyard_site_files", map[string]any{"site": "s1"}, nil)
	want := `{"site":"s1","url":"https://s1.backyard.test/","files":[{"path":"a/z.css","size":7},{"path":"b.js","size":4},{"path":"index.html","size":10}],"total":3,"truncated":false}`
	if text != want {
		t.Fatalf("site_files = %s", text)
	}
	for i := range 2001 {
		e.putObject("many", fmt.Sprintf("f%04d", i), []byte("x"))
	}
	var out struct {
		Files     []struct{ Path string } `json:"files"`
		Total     int                     `json:"total"`
		Truncated bool                    `json:"truncated"`
	}
	e.toolOK(aliceToken, "backyard_site_files", map[string]any{"site": "many"}, &out)
	if len(out.Files) != 2000 || out.Total != 2001 || !out.Truncated || out.Files[1999].Path != "f1999" {
		t.Fatalf("many: %d files, total %d, truncated %v", len(out.Files), out.Total, out.Truncated)
	}
}

func TestReadSiteFile(t *testing.T) {
	e := newTenv(t)
	read := func(site, p string) (string, bool) {
		return e.tool(aliceToken, "backyard_read_site_file", map[string]any{"site": site, "path": p})
	}
	e.putObject("r", "index.html", []byte("<p>hi</p>"))
	if text, isErr := read("r", "index.html"); isErr || text != "<p>hi</p>" {
		t.Fatalf("read = %v %q", isErr, text)
	}
	e.toolErr(aliceToken, "backyard_read_site_file", map[string]any{"site": "nope", "path": "index.html"}, `There is no site named "nope" on this Backyard.`)
	e.toolErr(aliceToken, "backyard_read_site_file", map[string]any{"site": "r", "path": "../x"}, `"../x" is not an allowed path: path may not contain "." or ".." segments.`)
	e.toolErr(aliceToken, "backyard_read_site_file", map[string]any{"site": "r", "path": ".git/config"}, `".git/config" is not an allowed path`)
	e.toolErr(aliceToken, "backyard_read_site_file", map[string]any{"site": "r", "path": "logo.PNG"}, "logo.PNG is a binary file and cannot be read as text; link to it by URL instead.")
	e.toolErr(aliceToken, "backyard_read_site_file", map[string]any{"site": "r", "path": "missing.js"}, `r has no file "missing.js".`)
	// The 128 KiB cap.
	e.putObject("r", "max.txt", bytes.Repeat([]byte("a"), maxReadBytes))
	if text, isErr := read("r", "max.txt"); isErr || len(text) != maxReadBytes {
		t.Fatalf("128 KiB: %v %d", isErr, len(text))
	}
	e.putObject("r", "big.txt", bytes.Repeat([]byte("a"), maxReadBytes+1))
	e.toolErr(aliceToken, "backyard_read_site_file", map[string]any{"site": "r", "path": "big.txt"}, "big.txt is 128.0 KiB; files up to 128 KiB can be read.")
	// NUL bytes in the first 8 KiB mean binary; later ones do not.
	e.putObject("r", "nul.txt", append([]byte("abc"), 0, 'd'))
	e.toolErr(aliceToken, "backyard_read_site_file", map[string]any{"site": "r", "path": "nul.txt"}, "nul.txt is a binary file")
	late := append(bytes.Repeat([]byte("a"), binarySniffBytes), 0)
	e.putObject("r", "late.txt", late)
	if _, isErr := read("r", "late.txt"); isErr {
		t.Fatal("a NUL after 8 KiB made the file binary")
	}
	// Invalid UTF-8 is replaced.
	e.putObject("r", "latin1.txt", []byte("caf\xe9!"))
	if text, _ := read("r", "latin1.txt"); text != "caf�!" {
		t.Fatalf("latin1 = %q", text)
	}
}

func TestStoreErrorsAreInternal(t *testing.T) {
	e := newTenv(t)
	e.h.reg = sites.NewRegistry(failingStore{e.store}, e.cfg)
	e.h.store = failingStore{e.store}
	for _, c := range []struct {
		tool string
		args map[string]any
	}{{"backyard_list_sites", map[string]any{}}, {"backyard_site_files", map[string]any{"site": "x"}}, {"backyard_read_site_file", map[string]any{"site": "x", "path": "a"}}} {
		text := e.toolErr(aliceToken, c.tool, c.args, "Internal error (request ")
		if strings.Contains(text, "secret bucket") {
			t.Fatalf("%s leaks the cause: %q", c.tool, text)
		}
	}
}

// failingStore fails every listing and read.
type failingStore struct{ blob.Store }

var errBucket = errors.New("secret bucket unreachable")

func (failingStore) List(context.Context, string, string) (*blob.ListResult, error) {
	return nil, errBucket
}
func (failingStore) Stat(context.Context, string) (*blob.ObjectInfo, error) { return nil, errBucket }
func (failingStore) Get(context.Context, string) (io.ReadCloser, *blob.ObjectInfo, error) {
	return nil, nil, errBucket
}

func TestLeaseContention(t *testing.T) {
	e := newTenv(t)
	entered, proceed := make(chan struct{}), make(chan struct{})
	var first sync.Mutex
	held := false
	e.h.publish = func(ctx context.Context, req deploy.PublishRequest) (*deploy.PublishResult, *deploy.PublishError) {
		first.Lock()
		wait := !held && req.Site == "busy"
		held = held || wait
		first.Unlock()
		if wait {
			close(entered)
			<-proceed
		}
		return e.gw.Publish(ctx, req)
	}
	done := make(chan string)
	go func() {
		text, _ := e.tool(aliceToken, "backyard_publish", map[string]any{"site": "busy", "files": files("index.html", "a")})
		done <- text
	}()
	<-entered
	e.toolErr(aliceToken, "backyard_publish", map[string]any{"site": "busy", "files": files("b.txt", "b")},
		"Another publish to busy is in progress; call backyard_publish again when it has finished.")
	// Other sites are not blocked.
	e.toolOK(bobToken, "backyard_publish", map[string]any{"site": "free", "files": files("index.html", "x")}, nil)
	close(proceed)
	if text := <-done; !strings.Contains(text, `"created":true`) {
		t.Fatalf("first publish = %s", text)
	}
	// Released: the next publish goes through and keeps the first's file.
	var out publishOut
	e.toolOK(aliceToken, "backyard_publish", map[string]any{"site": "busy", "files": files("b.txt", "b")}, &out)
	if out.Files != 2 {
		t.Fatalf("second publish = %+v", out)
	}
	if _, err := e.docs.Get(context.Background(), docstore.SystemSite, lockCollection, "busy"); !errors.Is(err, docstore.ErrNotFound) {
		t.Fatalf("lease not released: %v", err)
	}
}

func TestLeaseExpiryAndRelease(t *testing.T) {
	e := newTenv(t)
	now := time.Unix(1_800_000_000, 0)
	e.h.now = func() time.Time { return now }
	ctx := context.Background()
	// A live lease of another replica blocks.
	if _, err := e.docs.Create(ctx, docstore.SystemSite, lockCollection, "s1", map[string]any{"exp": now.Unix() + 10}, "x"); err != nil {
		t.Fatal(err)
	}
	e.toolErr(aliceToken, "backyard_publish", map[string]any{"site": "s1", "files": files("index.html", "x")}, "Another publish to s1 is in progress")
	if doc, err := e.docs.Get(ctx, docstore.SystemSite, lockCollection, "s1"); err != nil || leaseExp(doc) != now.Unix()+10 {
		t.Fatalf("a refused publish touched the lease: %v %v", doc, err)
	}
	// An expired one is taken over, then released.
	now = now.Add(11 * time.Second)
	e.toolOK(aliceToken, "backyard_publish", map[string]any{"site": "s1", "files": files("index.html", "x")}, nil)
	if _, err := e.docs.Get(ctx, docstore.SystemSite, lockCollection, "s1"); !errors.Is(err, docstore.ErrNotFound) {
		t.Fatalf("lease not released: %v", err)
	}
	// Refusals inside the gateway release the lease too.
	e.toolErr(bobToken, "backyard_publish", map[string]any{"site": "s1", "files": files("index.html", "y")}, "overwrite")
	if _, err := e.docs.Get(ctx, docstore.SystemSite, lockCollection, "s1"); !errors.Is(err, docstore.ErrNotFound) {
		t.Fatalf("lease not released after a refusal: %v", err)
	}
	// A lease another publish holds is not released by us, even with the
	// same exp (taken in the same second).
	exp := now.Add(leaseTTL).Unix()
	if _, err := e.docs.Create(ctx, docstore.SystemSite, lockCollection, "s2", map[string]any{"exp": exp, "holder": "other"}, "other"); err != nil {
		t.Fatal(err)
	}
	e.h.releaseLease(ctx, "s2", "mine")
	if _, err := e.docs.Get(ctx, docstore.SystemSite, lockCollection, "s2"); err != nil {
		t.Fatalf("another publish's lease was released: %v", err)
	}
	// Overwrite with the user's agreement.
	e.toolOK(bobToken, "backyard_publish", map[string]any{"site": "s1", "files": files("index.html", "y"), "overwrite": true}, nil)
}

// pausingDocs holds the first write a lease taker makes after reading the
// lease (the takeover claim, or the lease Update or Delete) of whoever arms
// it until proceed is closed.
type pausingDocs struct {
	docstore.Store
	armed            atomic.Bool
	blocked, proceed chan struct{}
}

func (p *pausingDocs) pause(collection string) {
	if (collection == lockCollection || collection == takeoverCollection) && p.armed.CompareAndSwap(true, false) {
		close(p.blocked)
		<-p.proceed
	}
}

func (p *pausingDocs) Create(ctx context.Context, site, collection, id string, data map[string]any, actor string) (*docstore.Doc, error) {
	if collection == takeoverCollection {
		p.pause(collection)
	}
	return p.Store.Create(ctx, site, collection, id, data, actor)
}

func (p *pausingDocs) Update(ctx context.Context, site, collection, id string, patch map[string]any, actor string) (*docstore.Doc, error) {
	p.pause(collection)
	return p.Store.Update(ctx, site, collection, id, patch, actor)
}

func (p *pausingDocs) Delete(ctx context.Context, site, collection, id string) (bool, error) {
	p.pause(collection)
	return p.Store.Delete(ctx, site, collection, id)
}

// leaseRace sets up two replicas over one store holding the expired lease
// of a dead holder, and starts r2 taking it over: it returns once r2 has
// read the lease and is about to write. go resumes r2 and returns its
// result.
func leaseRace(t *testing.T) (docs *pausingDocs, r1, r2 *tenv, resume func() (func(), *deploy.PublishError)) {
	t.Helper()
	docs = &pausingDocs{Store: memory.New(), blocked: make(chan struct{}), proceed: make(chan struct{})}
	r1 = newTenv(t, tenvOption{opts: func(o *Options) { o.Docs = docs }})
	r2 = newTenv(t, tenvOption{opts: func(o *Options) { o.Docs = docs }})
	ctx := context.Background()
	if _, err := docs.Create(ctx, docstore.SystemSite, lockCollection, "s", map[string]any{"exp": time.Now().Unix() - 60, "holder": "dead"}, "x"); err != nil {
		t.Fatal(err)
	}
	docs.armed.Store(true)
	type result struct {
		release func()
		err     *deploy.PublishError
	}
	slow := make(chan result)
	go func() {
		release, err := r2.h.acquireLease(ctx, "s", "bob")
		slow <- result{release, err}
	}()
	<-docs.blocked
	return docs, r1, r2, func() (func(), *deploy.PublishError) {
		close(docs.proceed)
		r := <-slow
		return r.release, r.err
	}
}

// wantNoLease fails unless site has no lease and a new publish gets one.
func wantNoLease(t *testing.T, e *tenv, docs docstore.Store, site string) {
	t.Helper()
	if d, err := docs.Get(context.Background(), docstore.SystemSite, lockCollection, site); !errors.Is(err, docstore.ErrNotFound) {
		t.Fatalf("a lease nobody holds is left behind: %+v %v", d, err)
	}
	release, err := e.h.acquireLease(context.Background(), site, "carol")
	if err != nil {
		t.Fatalf("the next publish = %v", err)
	}
	release()
}

// Two replicas taking over one expired lease at once: exactly one gets it,
// the loser writes nothing (it neither removes nor renames the winner's
// lease), and the winner's release frees the site.
func TestLeaseContestedTakeover(t *testing.T) {
	docs, r1, r2, resume := leaseRace(t)
	ctx := context.Background()
	// r2 read the expired lease and is about to take it over; r1 does first.
	release1, err1 := r1.h.acquireLease(ctx, "s", "alice")
	_, err2 := resume()
	if err1 != nil || err2 == nil || err2.ID != "E20" {
		t.Fatalf("r1 = %v, r2 = %v; want r1 to hold the lease and r2 E20", err1, err2)
	}
	if d, err := docs.Get(ctx, docstore.SystemSite, lockCollection, "s"); err != nil || d.Version != 2 {
		t.Fatalf("the lease r1 holds = %+v, %v; want it untouched by r2", d, err)
	}
	// While r1 holds it, nobody else gets it.
	if _, err := r2.h.acquireLease(ctx, "s", "bob"); err == nil || err.ID != "E20" {
		t.Fatalf("a third publish got the lease: %v", err)
	}
	release1()
	wantNoLease(t, r2, docs, "s")
}

// A taker that read the expired lease before another took it over, published
// and released it, and a third publish made a new lease: the stale taker
// leaves the new lease alone, so the third publish's release frees the site.
func TestLeaseStaleTakeover(t *testing.T) {
	docs, r1, r2, resume := leaseRace(t)
	ctx := context.Background()
	release1, err := r1.h.acquireLease(ctx, "s", "alice")
	if err != nil {
		t.Fatal(err)
	}
	release1()
	release3, err := r1.h.acquireLease(ctx, "s", "carol")
	if err != nil {
		t.Fatalf("a fresh lease = %v", err)
	}
	if _, err := resume(); err == nil || err.ID != "E20" {
		t.Fatalf("the stale taker = %v, want E20", err)
	}
	if d, err := docs.Get(ctx, docstore.SystemSite, lockCollection, "s"); err != nil || d.Version != 1 {
		t.Fatalf("the third publish's lease = %+v, %v; want it untouched", d, err)
	}
	release3()
	wantNoLease(t, r2, docs, "s")
}

// Takeover claims are pruned once they have expired.
func TestLeaseTakeoverClaimsPruned(t *testing.T) {
	e := newTenv(t)
	now := time.Unix(1_800_000_000, 0)
	e.h.now = func() time.Time { return now }
	ctx := context.Background()
	for i := range 3 {
		if _, err := e.docs.Create(ctx, docstore.SystemSite, lockCollection, "s", map[string]any{"exp": now.Unix() - 1, "holder": "dead"}, "x"); err != nil {
			t.Fatal(err)
		}
		release, err := e.h.acquireLease(ctx, "s", "alice")
		if err != nil {
			t.Fatalf("takeover %d = %v", i, err)
		}
		release()
		now = now.Add(leaseTTL + time.Second)
	}
	q := docstore.Query{}
	if err := q.Normalize(100); err != nil {
		t.Fatal(err)
	}
	page, err := e.docs.Query(ctx, docstore.SystemSite, takeoverCollection, q)
	if err != nil || len(page.Docs) != 1 {
		t.Fatalf("takeover claims = %d (%v), want only the live one", len(page.Docs), err)
	}
}

// A taker that claimed an expired lease and died before it wrote blocks the
// site until its claim expires; then the next taker claims it anew.
func TestLeaseDeadClaimant(t *testing.T) {
	e := newTenv(t)
	now := time.Unix(1_800_000_000, 0)
	e.h.now = func() time.Time { return now }
	ctx := context.Background()
	doc, err := e.docs.Create(ctx, docstore.SystemSite, lockCollection, "s", map[string]any{"exp": now.Unix() - 1, "holder": "dead"}, "x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.docs.Create(ctx, docstore.SystemSite, takeoverCollection, takeoverID("s", doc, 1), map[string]any{"exp": now.Add(leaseTTL).Unix()}, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.h.acquireLease(ctx, "s", "alice"); err == nil || err.ID != "E20" {
		t.Fatalf("with a live claim: %v, want E20", err)
	}
	now = now.Add(leaseTTL + time.Second)
	release, perr := e.h.acquireLease(ctx, "s", "alice")
	if perr != nil {
		t.Fatalf("after the claim expired: %v", perr)
	}
	release()
	wantNoLease(t, e, e.docs, "s")
}

// A publish that ends close to its lease's expiry leaves the lease to expire:
// a taker may already be taking it over.
func TestLeaseReleaseMargin(t *testing.T) {
	e := newTenv(t)
	now := time.Unix(1_800_000_000, 0)
	e.h.now = func() time.Time { return now }
	ctx := context.Background()
	release, err := e.h.acquireLease(ctx, "s", "alice")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(leaseTTL - releaseMargin + time.Second)
	release()
	if _, err := e.docs.Get(ctx, docstore.SystemSite, lockCollection, "s"); err != nil {
		t.Fatalf("a lease about to expire was deleted: %v", err)
	}
}

func TestScrub(t *testing.T) {
	e := newTenv(t)
	codes := []string{
		"byat_" + strings.Repeat("A", 40) + "." + strings.Repeat("b", 43),
		"byrt_eyJnIjoiMDEyMyIsIm4iOjF9.mac-mac-mac-mac",
		"byac_eyJnIjoiMDEyMyIsIngiOjF9.mac_mac_mac_mac",
		"bydc_eyJ2IjoxLCJuIjoiQ2xhdWRlIn0.signature",
	}
	leak := strings.Join(codes, " ") + " " + testSecret
	// A site file quoting credentials, a client name that looks like one,
	// and an unknown tool named like one.
	e.putObject("leaky", "index.html", []byte("<p>"+leak+"</p>"))
	for _, g := range e.auth.grants {
		g.ClientName = codes[3]
	}
	e.toolOK(aliceToken, "backyard_read_site_file", map[string]any{"site": "leaky", "path": "index.html"}, nil)
	e.toolOK(aliceToken, "backyard_whoami", nil, nil)
	e.toolOK(aliceToken, "backyard_publish", map[string]any{"site": "leaky", "files": files("a.txt", leak)}, nil)
	e.toolOK(aliceToken, "backyard_list_sites", nil, nil)
	e.toolErr(aliceToken, "backyard_publish", map[string]any{"site": "leaky", "files": files(codes[0], "x", codes[0], "y")}, "[REDACTED]")
	e.rpc("tools/call", map[string]any{"name": codes[1]})
	e.post(`{"jsonrpc":"2.0","id":"` + codes[2] + `","method":"nope"}`)

	re := regexp.MustCompile(`by(at|rt|ac|dc)_[A-Za-z0-9_.-]{16,}`)
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, b := range e.bodies {
		// The echoed JSON-RPC id is the client's own value.
		b = strings.ReplaceAll(b, `"id":"`+codes[2]+`"`, "")
		if re.MatchString(b) || strings.Contains(b, testSecret) || strings.Contains(b, aliceToken) {
			t.Fatalf("a result carries a credential: %s", b)
		}
	}
	if !strings.Contains(strings.Join(e.bodies, "\n"), "[REDACTED]") {
		t.Fatal("nothing was redacted")
	}
}
