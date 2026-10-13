package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/thasami/backyard/internal/sites"
	"github.com/thasami/backyard/internal/sites/sitestest"
)

// presignEnv returns an env whose store hands out presigned upload URLs to
// a local storage endpoint (which enforces the signed size and digest
// unless the store is made lax).
func presignEnv(t *testing.T, opts ...envOption) *env {
	t.Helper()
	e := newEnv(t, opts...)
	ps := httptest.NewServer(e.store.PresignHandler())
	t.Cleanup(ps.Close)
	e.store.EnablePresign(ps.URL)
	return e
}

// upload PUTs body to an upload URL with exactly its headers and no
// credentials, returning the status.
func (e *env) upload(up Upload, body string) int {
	e.t.Helper()
	req, err := http.NewRequest(up.Method, up.URL, strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	for k, v := range up.Headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// The gateway presigns each upload for the declared size and SHA-256, so
// storage that enforces them (S3) rejects any other body.
func TestPresignedUploadsBindDeclaredSizeAndDigest(t *testing.T) {
	e := presignEnv(t)
	files := map[string]string{"index.html": "<title>Bound</title>"}
	cr := e.createDeploy("alice", "bound", files)
	up := uploadFor(t, cr, "index.html")
	if strings.HasPrefix(up.URL, e.srv.URL) {
		t.Fatalf("upload not presigned: %+v", up)
	}
	for _, tc := range []struct {
		what   string
		body   string
		status int
	}{
		{"larger body", "<title>Bound</title><script>evil()</script>", http.StatusForbidden},
		{"smaller body", "<title>", http.StatusForbidden},
		{"same size, other bytes", "<title>BOUND</title>", http.StatusBadRequest},
	} {
		if got := e.upload(up, tc.body); got != tc.status {
			t.Errorf("%s: storage status %d, want %d", tc.what, got, tc.status)
		}
	}
	if b := e.object("bound", "index.html"); b != nil {
		t.Fatalf("a rejected upload was stored: %q", b)
	}
	resp, body := e.commit("alice", "bound", cr.DeployID, entriesFor(files))
	wantStatus(t, resp, body, http.StatusConflict, CodeConflict)
	if got := e.upload(up, files["index.html"]); got != http.StatusOK {
		t.Fatalf("declared upload: storage status %d", got)
	}
	resp, body = e.commit("alice", "bound", cr.DeployID, entriesFor(files))
	wantStatus(t, resp, body, http.StatusOK, "")
}

// Azure SAS URLs cannot bind a size: a larger body reaches storage, and the
// commit must fail and delete it rather than leave unverified bytes live.
func TestCommitDeletesWrongSizeUploads(t *testing.T) {
	e := presignEnv(t)
	e.store.SetLaxPresign(true)
	ctx := context.Background()
	v1 := map[string]string{"index.html": "<title>V1</title>", "a.txt": "aaaa", "keep.txt": "keep"}
	dir := t.TempDir()
	writeTree(t, dir, v1)
	c := e.client("alice")
	if _, err := c.Deploy(ctx, "lax", dir, DeployOptions{}); err != nil {
		t.Fatal(err)
	}
	before, err := e.reg.GetMeta(ctx, "lax")
	if err != nil {
		t.Fatal(err)
	}

	v2 := map[string]string{"index.html": "<title>V1</title>", "a.txt": "AAAA", "keep.txt": "keep", "new.txt": "nn"}
	cr := e.createDeploy("alice", "lax", v2)
	if len(cr.Uploads) != 2 {
		t.Fatalf("uploads = %+v", cr.Uploads)
	}
	if got := e.upload(uploadFor(t, cr, "a.txt"), "AAAA and a lot more"); got != http.StatusOK {
		t.Fatalf("lax storage rejected the upload: %d", got)
	}
	if got := e.upload(uploadFor(t, cr, "new.txt"), strings.Repeat("n", 1000)); got != http.StatusOK {
		t.Fatalf("lax storage rejected the upload: %d", got)
	}
	resp, body := e.commit("alice", "lax", cr.DeployID, entriesFor(v2))
	wantStatus(t, resp, body, http.StatusConflict, CodeConflict)
	msg := errCode(t, body).Error.Message
	for _, want := range []string{
		"2 file(s) were not uploaded correctly",
		"a.txt (19 bytes in storage, expected 4; deleted)",
		"new.txt (1000 bytes in storage, expected 2; deleted)",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not contain %q", msg, want)
		}
	}
	for _, p := range []string{"a.txt", "new.txt"} {
		if b := e.object("lax", p); b != nil {
			t.Errorf("%s still stored after the failed commit: %q", p, b)
		}
	}
	if got := string(e.object("lax", "keep.txt")); got != "keep" {
		t.Errorf("keep.txt = %q", got)
	}
	after, err := e.reg.GetMeta(ctx, "lax")
	if err != nil || after.DeployID != before.DeployID || after.FileCount != 3 {
		t.Errorf("meta changed by a failed commit: %+v (%v)", after, err)
	}
	if logs := e.logs.String(); !strings.Contains(logs, `"action":"deploy.reject_uploads"`) || !strings.Contains(logs, `"wrong_size":2`) {
		t.Errorf("rejection not audited:\n%s", logs)
	}

	// A correct deploy recovers the site.
	writeTree(t, dir, v2)
	res, err := c.Deploy(ctx, "lax", dir, DeployOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Site.FileCount != 4 || string(e.object("lax", "a.txt")) != "AAAA" || string(e.object("lax", "new.txt")) != "nn" {
		t.Errorf("redeploy = %+v", res)
	}
}

// A listed file whose stored object is still the version the previous
// manifest verified was simply not re-uploaded: the commit fails but keeps
// the live file.
func TestCommitKeepsPreviouslyCommittedFile(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"a.txt": "aaaa"})
	if _, err := e.client("alice").Deploy(ctx, "kept", dir, DeployOptions{}); err != nil {
		t.Fatal(err)
	}
	v2 := map[string]string{"a.txt": "aaaaa"}
	cr := e.createDeploy("alice", "kept", v2)
	resp, body := e.commit("alice", "kept", cr.DeployID, entriesFor(v2))
	wantStatus(t, resp, body, http.StatusConflict, CodeConflict)
	msg := errCode(t, body).Error.Message
	if !strings.Contains(msg, "a.txt (4 bytes in storage, expected 5)") || strings.Contains(msg, "deleted") {
		t.Errorf("message = %q", msg)
	}
	if got := string(e.object("kept", "a.txt")); got != "aaaa" {
		t.Errorf("a.txt = %q, want the committed version", got)
	}
	if strings.Contains(e.logs.String(), "deploy.reject_uploads") {
		t.Error("a kept file was audited as a rejected upload")
	}
}

type failingDeletes struct{ *sitestest.MemStore }

func (failingDeletes) Delete(context.Context, string) error {
	return errors.New("storage unavailable")
}

// When an object of the wrong size cannot be deleted, the commit still
// fails and says so.
func TestCommitReportsUndeletableObjects(t *testing.T) {
	ms := sitestest.NewMemStore()
	e := newEnv(t, withStore(failingDeletes{ms}))
	files := map[string]string{"a.txt": "aa"}
	cr := e.createDeploy("alice", "stuck", files)
	if _, err := ms.Put(context.Background(), sites.SiteKey("stuck", "a.txt"), strings.NewReader("aaaaaa"), 6, ""); err != nil {
		t.Fatal(err)
	}
	resp, body := e.commit("alice", "stuck", cr.DeployID, entriesFor(files))
	wantStatus(t, resp, body, http.StatusConflict, CodeConflict)
	if msg := errCode(t, body).Error.Message; !strings.Contains(msg, "a.txt (6 bytes in storage, expected 2; could not be deleted)") {
		t.Errorf("message = %q", msg)
	}
	if logs := e.logs.String(); !strings.Contains(logs, "storage unavailable") || !strings.Contains(logs, `"deleted":0`) {
		t.Errorf("deletion failure not logged:\n%s", logs)
	}
}

// The conflict message names at most ten paths.
func TestCommitConflictMessageIsBounded(t *testing.T) {
	e := newEnv(t)
	files := map[string]string{}
	for i := range 12 {
		files[fmt.Sprintf("f%02d.txt", i)] = "x"
	}
	cr := e.createDeploy("alice", "many", files)
	resp, body := e.commit("alice", "many", cr.DeployID, entriesFor(files))
	wantStatus(t, resp, body, http.StatusConflict, CodeConflict)
	msg := errCode(t, body).Error.Message
	if !strings.HasPrefix(msg, "12 file(s) were not uploaded correctly: ") || !strings.HasSuffix(msg, ", … and 2 more") ||
		strings.Count(msg, "(missing)") != 10 {
		t.Errorf("message = %q", msg)
	}
}

// The create audit record counts new paths: uploads that would be orphans
// if the deploy were never committed.
func TestCreateAuditCountsNewFiles(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"a.txt": "a", "b.txt": "b"})
	if _, err := e.client("alice").Deploy(context.Background(), "audit", dir, DeployOptions{}); err != nil {
		t.Fatal(err)
	}
	e.createDeploy("alice", "audit", map[string]string{"a.txt": "A", "b.txt": "b", "c.txt": "c", "d.txt": "d"})
	logs := e.logs.String()
	if !strings.Contains(logs, `"uploads":3,"upload_bytes":3,"new_files":2`) {
		t.Errorf("deploy.create audit lacks new_files:\n%s", logs)
	}
}
