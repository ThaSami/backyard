package dynamodb_test

import (
	"context"
	"fmt"
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
