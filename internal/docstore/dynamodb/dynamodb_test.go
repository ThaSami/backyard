package dynamodb_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awsddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/docstore"
	"github.com/thasami/backyard/internal/docstore/conformance"
	"github.com/thasami/backyard/internal/docstore/dynamodb"
)

// testEndpoint returns the DynamoDB Local endpoint or skips the test.
func testEndpoint(t *testing.T) string {
	t.Helper()
	ep := os.Getenv("BACKYARD_TEST_DYNAMODB_ENDPOINT")
	if ep == "" {
		t.Skip("BACKYARD_TEST_DYNAMODB_ENDPOINT not set")
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(ep)
	if err != nil {
		t.Skipf("DynamoDB endpoint unreachable: %v", err)
	}
	_ = resp.Body.Close()
	return ep
}

func testClient(endpoint string) *awsddb.Client {
	return awsddb.New(awsddb.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(endpoint),
		Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
	})
}

// testTable returns a unique table name that is deleted after the test.
func testTable(t *testing.T, client *awsddb.Client) string {
	t.Helper()
	name := fmt.Sprintf("backyard-test-%08x", rand.Uint32())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := client.DeleteTable(ctx, &awsddb.DeleteTableInput{TableName: aws.String(name)}); err != nil {
			t.Logf("cleanup delete table %s: %v", name, err)
		}
	})
	return name
}

func newStore(t *testing.T, maxScan int) *dynamodb.Store {
	t.Helper()
	ep := testEndpoint(t)
	client := testClient(ep)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s, err := dynamodb.New(ctx, client, config.DynamoDBConfig{Table: testTable(t, client), CreateTable: true, MaxScan: maxScan})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestConformance(t *testing.T) {
	s := newStore(t, 0)
	conformance.Run(t, func(t *testing.T) docstore.Store { return s })
}

func TestOpenWithEndpoint(t *testing.T) {
	ep := testEndpoint(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	client := testClient(ep)
	table := testTable(t, client)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s, err := dynamodb.Open(ctx, config.DynamoDBConfig{Table: table, Endpoint: ep, Region: "eu-west-1", CreateTable: true})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	if err := s.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	// Opening again on the existing table is fine.
	s2, err := dynamodb.Open(ctx, config.DynamoDBConfig{Table: table, Endpoint: ep, Region: "eu-west-1", CreateTable: true})
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	if _, err := s2.Create(ctx, "site", "c", "x", map[string]any{"a": 1}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "site", "c", "x"); err != nil {
		t.Fatal(err)
	}
}

func TestPingMissingTable(t *testing.T) {
	ep := testEndpoint(t)
	ctx := context.Background()
	s, err := dynamodb.New(ctx, testClient(ep), config.DynamoDBConfig{Table: fmt.Sprintf("missing-%08x", rand.Uint32())})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Ping(ctx); err == nil {
		t.Error("Ping on a missing table succeeded")
	}
}

func TestMaxScanLimitsQuery(t *testing.T) {
	s := newStore(t, 5)
	ctx := context.Background()
	for i := range 8 {
		if _, err := s.Create(ctx, "site", "c", fmt.Sprintf("d%d", i), map[string]any{"i": i}, ""); err != nil {
			t.Fatal(err)
		}
	}
	q := docstore.Query{OrderBy: []docstore.Order{{Field: "i", Dir: "desc"}}}
	p, err := s.Query(ctx, "site", "c", q)
	if err != nil {
		t.Fatal(err)
	}
	// Only the first five documents by id (d0..d4) are considered.
	var got []string
	for _, d := range p.Docs {
		got = append(got, d.ID)
	}
	if strings.Join(got, ",") != "d4,d3,d2,d1,d0" {
		t.Errorf("truncated query = %v", got)
	}
	if n, err := s.Count(ctx, "site"); err != nil || n != 8 {
		t.Errorf("Count = %d, %v; want 8", n, err)
	}
}

func TestItemTooLarge(t *testing.T) {
	s := newStore(t, 0)
	_, err := s.Create(context.Background(), "site", "c", "big", map[string]any{"x": strings.Repeat("a", 420*1024)}, "")
	if err == nil {
		t.Fatal("Create of a 420 KiB document succeeded")
	}
	t.Logf("oversized item: %v", err)
}

func TestDeleteSiteManyDocuments(t *testing.T) {
	s := newStore(t, 0)
	ctx := context.Background()
	for i := range 60 {
		if _, err := s.Create(ctx, "big-site", fmt.Sprintf("c%d", i%4), "", map[string]any{"i": i}, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteSite(ctx, "big-site"); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Count(ctx, "big-site"); err != nil || n != 0 {
		t.Errorf("Count after DeleteSite = %d, %v", n, err)
	}
	for i := range 4 {
		p, err := s.Query(ctx, "big-site", fmt.Sprintf("c%d", i), docstore.Query{})
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Docs) != 0 {
			t.Errorf("collection c%d still has %d docs", i, len(p.Docs))
		}
	}
}

// hookTransport runs a hook (once, when armed) before forwarding the first
// request for the given DynamoDB operation.
type hookTransport struct {
	op string
	mu sync.Mutex
	fn func()
}

func (h *hookTransport) arm(fn func()) {
	h.mu.Lock()
	h.fn = fn
	h.mu.Unlock()
}

func (h *hookTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.Header.Get("X-Amz-Target"), "."+h.op) {
		h.mu.Lock()
		fn := h.fn
		h.fn = nil
		h.mu.Unlock()
		if fn != nil {
			fn()
		}
	}
	return http.DefaultTransport.RoundTrip(r)
}

// Update is optimistic: it reads the item, merges and writes it back on
// condition. A delete and re-create of the document between the read and the
// write starts the version over and must not let the write through: the
// update applies to the new document, as on every other backend, instead of
// bringing back the deleted one.
func TestUpdateAfterDeleteAndRecreate(t *testing.T) {
	ep := testEndpoint(t)
	plain := testClient(ep)
	table := testTable(t, plain)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	other, err := dynamodb.New(ctx, plain, config.DynamoDBConfig{Table: table, CreateTable: true})
	if err != nil {
		t.Fatal(err)
	}
	hook := &hookTransport{op: "PutItem"}
	hooked := awsddb.New(awsddb.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(ep),
		Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
		HTTPClient:   &http.Client{Transport: hook},
	})
	s, err := dynamodb.New(ctx, hooked, config.DynamoDBConfig{Table: table})
	if err != nil {
		t.Fatal(err)
	}

	type op func(id string) error
	create := func(data map[string]any, actor string) op {
		return func(id string) error { _, err := other.Create(ctx, "site", "c", id, data, actor); return err }
	}
	put := func(data map[string]any, actor string) op {
		return func(id string) error { _, err := other.Put(ctx, "site", "c", id, data, actor); return err }
	}
	del := func(id string) error { _, err := other.Delete(ctx, "site", "c", id); return err }
	run := func(id string, ops []op) error {
		for _, o := range ops {
			if err := o(id); err != nil {
				return err
			}
		}
		return nil
	}
	old, fresh := map[string]any{"title": "old", "secret": 1}, map[string]any{"title": "fresh"}

	for _, tc := range []struct {
		name     string
		setup    []op // by alice, before the update
		recreate []op // by bob, between the update's read and its write
		wantVer  int64
	}{
		{"create", []op{create(old, "alice")}, []op{del, create(fresh, "bob")}, 2},
		// The re-created document is back at version 2, like the item read.
		{"put twice", []op{put(old, "alice"), put(old, "alice")}, []op{del, put(fresh, "bob"), put(fresh, "bob")}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := "doc-" + strings.ReplaceAll(tc.name, " ", "-")
			if err := run(id, tc.setup); err != nil {
				t.Fatal(err)
			}
			fired, hookErr := false, error(nil)
			hook.arm(func() { fired, hookErr = true, run(id, tc.recreate) })
			got, err := s.Update(ctx, "site", "c", id, map[string]any{"votes": 1}, "carol")
			if err != nil {
				t.Fatalf("Update: %v", err)
			}
			if !fired || hookErr != nil {
				t.Fatalf("re-creating the document during the update: ran=%v err=%v", fired, hookErr)
			}
			stored, err := other.Get(ctx, "site", "c", id)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]any{"title": "fresh", "votes": 1.0}
			for what, d := range map[string]*docstore.Doc{"returned": got, "stored": stored} {
				if !reflect.DeepEqual(d.Data, want) || d.Version != tc.wantVer || d.CreatedBy != "bob" || d.UpdatedBy != "carol" {
					t.Errorf("%s document = %v version %d created_by %q updated_by %q; want %v version %d created_by bob updated_by carol",
						what, d.Data, d.Version, d.CreatedBy, d.UpdatedBy, want, tc.wantVer)
				}
			}
		})
	}
}

// lostAnswer forwards the first armed request for op whose body contains
// mark, lets DynamoDB apply it, and answers 500 instead, as when the answer
// to a write is lost (DynamoDB: a 500 on a write "may have succeeded"). The
// SDK's retryer then sends the same write again.
type lostAnswer struct {
	op, mark string
	mu       sync.Mutex
	armed    bool
	fired    bool
	sent     int // requests for op
}

func (l *lostAnswer) arm(mark string) {
	l.mu.Lock()
	l.mark, l.armed, l.fired, l.sent = mark, true, false, 0
	l.mu.Unlock()
}

func (l *lostAnswer) RoundTrip(r *http.Request) (*http.Response, error) {
	if !strings.HasSuffix(r.Header.Get("X-Amz-Target"), "."+l.op) || r.Body == nil {
		return http.DefaultTransport.RoundTrip(r)
	}
	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		return nil, err
	}
	r = r.Clone(r.Context())
	r.Body = io.NopCloser(bytes.NewReader(body))
	l.mu.Lock()
	l.sent++
	lose := l.armed && strings.Contains(string(body), l.mark)
	if lose {
		l.armed, l.fired = false, true
	}
	l.mu.Unlock()
	res, err := http.DefaultTransport.RoundTrip(r)
	if err != nil || !lose {
		return res, err
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the write to lose the answer of failed: HTTP %d", res.StatusCode)
	}
	msg := `{"__type":"com.amazonaws.dynamodb.v20120810#InternalServerError","message":"injected: the write was applied"}`
	return &http.Response{
		StatusCode: http.StatusInternalServerError, Status: "500 Internal Server Error",
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header:        http.Header{"Content-Type": {"application/x-amz-json-1.0"}},
		Body:          io.NopCloser(strings.NewReader(msg)),
		ContentLength: int64(len(msg)), Request: r,
	}, nil
}

// A conditional write whose answer is lost is sent again by the SDK; that
// attempt fails its condition on the write the first one applied. Create and
// UpdateIf report it as theirs (it is), not as a conflict with another
// writer: a first code redemption must not look like a replay, nor a lease
// taken over like one taken by someone else.
func TestRetriedConditionalWrites(t *testing.T) {
	ep := testEndpoint(t)
	plain := testClient(ep)
	table := testTable(t, plain)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	other, err := dynamodb.New(ctx, plain, config.DynamoDBConfig{Table: table, CreateTable: true})
	if err != nil {
		t.Fatal(err)
	}
	lose := &lostAnswer{op: "PutItem"}
	s, err := dynamodb.New(ctx, awsddb.New(awsddb.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(ep),
		Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
		HTTPClient:   &http.Client{Transport: lose},
	}), config.DynamoDBConfig{Table: table})
	if err != nil {
		t.Fatal(err)
	}
	retried := func(what string) {
		t.Helper()
		lose.mu.Lock()
		defer lose.mu.Unlock()
		if !lose.fired || lose.sent != 2 {
			t.Fatalf("%s: answer lost %v, %d PutItem requests; want the write sent again once", what, lose.fired, lose.sent)
		}
	}

	lose.arm("attribute_not_exists")
	c, err := s.Create(ctx, "site", "c", "lease", map[string]any{"exp": 1}, "alice")
	retried("Create")
	if err != nil {
		t.Fatalf("Create whose answer was lost: %v", err)
	}
	if got, err := other.Get(ctx, "site", "c", "lease"); err != nil || !got.At(c.Rev()) || !got.UpdatedAt.Equal(c.UpdatedAt) || got.CreatedBy != "alice" {
		t.Fatalf("stored %+v, %v; returned %+v", got, err, c)
	}
	if n, err := other.Count(ctx, "site"); err != nil || n != 1 {
		t.Errorf("Count after the retried Create = %d, %v; want 1", n, err)
	}

	lose.arm("#c0")
	u, err := s.UpdateIf(ctx, "site", "c", "lease", c.Rev(), map[string]any{"exp": 2}, "taker")
	retried("UpdateIf")
	if err != nil {
		t.Fatalf("UpdateIf whose answer was lost: %v", err)
	}
	stored, err := other.Get(ctx, "site", "c", "lease")
	if err != nil {
		t.Fatal(err)
	}
	if u.Version != 2 || u.UpdatedBy != "taker" || !reflect.DeepEqual(u.Data, map[string]any{"exp": 2.0}) ||
		!stored.At(u.Rev()) || !stored.UpdatedAt.Equal(u.UpdatedAt) || !reflect.DeepEqual(stored.Data, u.Data) {
		t.Fatalf("returned %+v, stored %+v", u, stored)
	}

	// A write by someone else is still a conflict, and a document deleted
	// meanwhile is not found, also when the condition fails on the first attempt.
	hook := &hookTransport{op: "PutItem"}
	h, err := dynamodb.New(ctx, awsddb.New(awsddb.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(ep),
		Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
		HTTPClient:   &http.Client{Transport: hook},
	}), config.DynamoDBConfig{Table: table})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		meanwhile func() error
		want      error
	}{
		{"written", func() error {
			_, err := other.Update(ctx, "site", "c", "lease", map[string]any{"exp": 3}, "taker")
			return err
		}, docstore.ErrConflict},
		{"same data, same actor", func() error {
			_, err := other.Update(ctx, "site", "c", "lease", map[string]any{"exp": 4}, "taker")
			return err
		}, docstore.ErrConflict},
		{"deleted", func() error { _, err := other.Delete(ctx, "site", "c", "lease"); return err }, docstore.ErrNotFound},
	} {
		cur, err := other.Get(ctx, "site", "c", "lease")
		if err != nil {
			t.Fatal(err)
		}
		var hookErr error
		hook.arm(func() { hookErr = tc.meanwhile() })
		_, err = h.UpdateIf(ctx, "site", "c", "lease", cur.Rev(), map[string]any{"exp": 4}, "taker")
		if hookErr != nil {
			t.Fatal(hookErr)
		}
		if !errors.Is(err, tc.want) {
			t.Errorf("%s between the read and the write: UpdateIf = %v; want %v", tc.name, err, tc.want)
		}
	}
	// Create of a document someone else created is still a conflict.
	if _, err := other.Create(ctx, "site", "c", "taken", map[string]any{"exp": 1}, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, "site", "c", "taken", map[string]any{"exp": 1}, "alice"); !errors.Is(err, docstore.ErrConflict) {
		t.Errorf("Create of an existing document = %v; want a conflict", err)
	}
}

// twinWrite stores, once armed, the item of the first request for op whose
// body contains mark through another client before forwarding the request,
// with every attribute other than the document's own passed through edit.
// It plays a second replica that wrote the same document in the same
// microsecond, as the same actor, with the same data.
type twinWrite struct {
	op, mark string
	client   *awsddb.Client
	table    string
	edit     func(name string, v types.AttributeValue) (types.AttributeValue, bool)
	mu       sync.Mutex
	err      error
	fired    bool
}

func (w *twinWrite) arm(mark string, edit func(string, types.AttributeValue) (types.AttributeValue, bool)) {
	w.mu.Lock()
	w.mark, w.edit, w.err, w.fired = mark, edit, nil, false
	w.mu.Unlock()
}

func (w *twinWrite) RoundTrip(r *http.Request) (*http.Response, error) {
	if !strings.HasSuffix(r.Header.Get("X-Amz-Target"), "."+w.op) || r.Body == nil {
		return http.DefaultTransport.RoundTrip(r)
	}
	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		return nil, err
	}
	r = r.Clone(r.Context())
	r.Body = io.NopCloser(bytes.NewReader(body))
	w.mu.Lock()
	edit := w.edit
	if edit == nil || !strings.Contains(string(body), w.mark) {
		w.mu.Unlock()
		return http.DefaultTransport.RoundTrip(r)
	}
	w.edit, w.fired = nil, true
	w.mu.Unlock()
	var in struct{ Item map[string]map[string]string }
	err = json.Unmarshal(body, &in)
	twin := map[string]types.AttributeValue{}
	for name, v := range in.Item {
		var av types.AttributeValue
		if s, ok := v["S"]; ok {
			av = &types.AttributeValueMemberS{Value: s}
		} else if n, ok := v["N"]; ok {
			av = &types.AttributeValueMemberN{Value: n}
		} else {
			err = errors.Join(err, fmt.Errorf("attribute %s: %v", name, v))
			continue
		}
		switch name {
		case "pk", "sk", "data", "created_at", "updated_at", "created_by", "updated_by", "version":
			twin[name] = av
		default:
			if av, ok := edit(name, av); ok {
				twin[name] = av
			}
		}
	}
	if err == nil {
		_, err = w.client.PutItem(r.Context(), &awsddb.PutItemInput{TableName: aws.String(w.table), Item: twin})
	}
	w.mu.Lock()
	w.err = err
	w.mu.Unlock()
	return http.DefaultTransport.RoundTrip(r)
}

// Two replicas can write the same document in the same microsecond (each
// keeps docstore.Now() increasing only within its process), as the same
// actor and with the same data: two takers of a lease, two redemptions of a
// code. A conditional write that fails on the other one's is a conflict, not
// a write of its own the SDK sent again, and Create leaves the counter alone.
func TestConditionalWriteRacedByIdenticalWrite(t *testing.T) {
	ep := testEndpoint(t)
	plain := testClient(ep)
	table := testTable(t, plain)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	other, err := dynamodb.New(ctx, plain, config.DynamoDBConfig{Table: table, CreateTable: true})
	if err != nil {
		t.Fatal(err)
	}
	twin := &twinWrite{op: "PutItem", client: plain, table: table}
	s, err := dynamodb.New(ctx, awsddb.New(awsddb.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(ep),
		Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
		HTTPClient:   &http.Client{Transport: twin},
	}), config.DynamoDBConfig{Table: table})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		edit func(string, types.AttributeValue) (types.AttributeValue, bool)
	}{
		{"another replica", func(_ string, v types.AttributeValue) (types.AttributeValue, bool) {
			if s, ok := v.(*types.AttributeValueMemberS); ok {
				return &types.AttributeValueMemberS{Value: s.Value + "-other"}, true
			}
			return &types.AttributeValueMemberN{Value: v.(*types.AttributeValueMemberN).Value + "1"}, true
		}},
		{"an older version", func(string, types.AttributeValue) (types.AttributeValue, bool) { return nil, false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := "lease-" + strings.ReplaceAll(tc.name, " ", "-")
			fired := func(what string) {
				t.Helper()
				twin.mu.Lock()
				defer twin.mu.Unlock()
				if !twin.fired || twin.err != nil {
					t.Fatalf("%s: the twin write ran %v: %v", what, twin.fired, twin.err)
				}
			}
			before, err := other.Count(ctx, "site")
			if err != nil {
				t.Fatal(err)
			}
			twin.arm("attribute_not_exists", tc.edit)
			_, err = s.Create(ctx, "site", "c", id, map[string]any{"exp": 1}, "alice")
			fired("Create")
			if !errors.Is(err, docstore.ErrConflict) {
				t.Errorf("Create raced by an identical write = %v; want a conflict", err)
			}
			if n, err := other.Count(ctx, "site"); err != nil || n != before {
				t.Errorf("Count after the losing Create = %d, %v; want %d", n, err, before)
			}

			cur, err := other.Get(ctx, "site", "c", id)
			if err != nil {
				t.Fatal(err)
			}
			twin.arm("#c0", tc.edit)
			_, err = s.UpdateIf(ctx, "site", "c", id, cur.Rev(), map[string]any{"exp": 2}, "taker")
			fired("UpdateIf")
			if !errors.Is(err, docstore.ErrConflict) {
				t.Errorf("UpdateIf raced by an identical write = %v; want a conflict", err)
			}
		})
	}
}
