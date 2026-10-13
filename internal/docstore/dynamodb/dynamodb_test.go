package dynamodb_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"strings"
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
