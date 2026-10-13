package identitytest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/jackc/pgx/v5"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/docstore"
	ddb "github.com/thasami/backyard/internal/docstore/dynamodb"
	"github.com/thasami/backyard/internal/docstore/memory"
	"github.com/thasami/backyard/internal/docstore/postgres"
	"github.com/thasami/backyard/internal/docstore/sqlite"
	"github.com/thasami/backyard/internal/identity"
)

func suffix() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

type staticDir map[string]*identity.User

func (d staticDir) Lookup(_ context.Context, email string) (*identity.User, error) {
	return d[email].Clone(), nil
}

// exercise runs the user cache scenario against store.
func exercise(t *testing.T, store docstore.Store) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	alice := &identity.User{
		ID: "alice+backyard@acme.com", Email: "Alice+Backyard@Acme.com", Name: "Alice Smith",
		Title: "Staff Engineer", Team: "Platform", Slack: "@alice", AvatarURL: "https://img.example/a.png",
		Groups: []string{"eng", "backyard-admins"}, Admin: true,
		Extra: map[string]any{"location": "Ottawa", "floor": float64(3)},
	}
	cache := identity.NewUserCache(store, nil)
	cache.Touch(ctx, alice)

	got, err := cache.Lookup(ctx, "ALICE+backyard@acme.com")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	want := &identity.User{
		ID: "alice+backyard@acme.com", Email: "alice+backyard@acme.com", Name: "Alice Smith",
		Title: "Staff Engineer", Team: "Platform", Slack: "@alice", AvatarURL: "https://img.example/a.png",
		Groups: []string{"eng", "backyard-admins"}, Extra: map[string]any{"location": "Ottawa", "floor": float64(3)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Lookup\n got  %+v\n want %+v", got, want)
	}

	// The document lives in the reserved namespace under the lowercased email.
	doc, err := store.Get(ctx, identity.SystemSite, identity.UsersCollection, "alice+backyard@acme.com")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if doc.CreatedBy != "alice+backyard@acme.com" || doc.Data["admin"] != nil {
		t.Fatalf("doc = %+v", doc)
	}

	// A new instance (no throttle state) overwrites the profile.
	alice2 := alice.Clone()
	alice2.Name, alice2.Title = "Alice S.", ""
	identity.NewUserCache(store, nil).Touch(ctx, alice2)
	got, err = cache.Lookup(ctx, "alice+backyard@acme.com")
	if err != nil || got.Name != "Alice S." || got.Title != "" {
		t.Fatalf("after update = %+v, %v", got, err)
	}

	// Anonymous and token principals are never stored.
	cache.Touch(ctx, &identity.User{ID: "anon:0123456789abcdef", Name: "Guest", Anonymous: true})
	cache.Touch(ctx, &identity.User{ID: "token:ci", Name: "ci", Email: "ci@acme.com"})
	if _, err := store.Get(ctx, identity.SystemSite, identity.UsersCollection, "ci@acme.com"); !errors.Is(err, docstore.ErrNotFound) {
		t.Fatalf("token principal stored: %v", err)
	}

	// Directory first, cache second.
	dir := staticDir{"alice+backyard@acme.com": {ID: "alice+backyard@acme.com", Email: "alice+backyard@acme.com", Title: "Director"}}
	got, err = identity.NewUserCache(store, dir).Lookup(ctx, "alice+backyard@acme.com")
	if err != nil || got.Title != "Director" || got.Name != "Alice S." {
		t.Fatalf("with directory = %+v, %v", got, err)
	}
	if u, err := cache.Lookup(ctx, "nobody@acme.com"); u != nil || err != nil {
		t.Fatalf("unknown = %+v, %v", u, err)
	}
}

func TestUserCacheMemory(t *testing.T) {
	exercise(t, memory.New())
}

func TestUserCacheSQLite(t *testing.T) {
	ctx := context.Background()
	s, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "backyard.db"))
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	defer s.Close()
	exercise(t, s)
}

func TestUserCachePostgres(t *testing.T) {
	dsn := os.Getenv("BACKYARD_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("BACKYARD_TEST_POSTGRES_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	defer conn.Close(context.Background())
	schema := "identity_test_" + suffix()
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer ccancel()
		c, err := pgx.Connect(cctx, dsn)
		if err != nil {
			return
		}
		defer c.Close(cctx)
		_, _ = c.Exec(cctx, `DROP SCHEMA IF EXISTS `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
	})
	s, err := postgres.Open(ctx, config.PostgresConfig{DSN: dsn, Schema: schema, MaxConns: 4})
	if err != nil {
		t.Fatalf("postgres: %v", err)
	}
	defer s.Close()
	exercise(t, s)
}

func TestUserCacheDynamoDB(t *testing.T) {
	endpoint := os.Getenv("BACKYARD_TEST_DYNAMODB_ENDPOINT")
	if endpoint == "" {
		t.Skip("BACKYARD_TEST_DYNAMODB_ENDPOINT not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client := dynamodb.New(dynamodb.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(endpoint),
		Credentials:  credentials.NewStaticCredentialsProvider("local", "local", ""),
	})
	if _, err := client.ListTables(ctx, &dynamodb.ListTablesInput{Limit: aws.Int32(1)}); err != nil {
		t.Skipf("dynamodb local unreachable: %v", err)
	}
	table := "identity-test-" + suffix()
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer ccancel()
		_, _ = client.DeleteTable(cctx, &dynamodb.DeleteTableInput{TableName: aws.String(table)})
	})
	s, err := ddb.New(ctx, client, config.DynamoDBConfig{Table: table, CreateTable: true, MaxScan: 1000})
	if err != nil {
		t.Fatalf("dynamodb: %v", err)
	}
	defer s.Close()
	exercise(t, s)
}
