package backends

import (
	"context"
	"io"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fsouza/fake-gcs-server/fakestorage"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"github.com/thasami/backyard/internal/config"
)

func TestOpen(t *testing.T) {
	ctx := context.Background()

	s3backend := s3mem.New()
	if err := s3backend.CreateBucket("bkt"); err != nil {
		t.Fatal(err)
	}
	s3srv := httptest.NewServer(gofakes3.New(s3backend, gofakes3.WithLogger(gofakes3.DiscardLog())).Server())
	defer s3srv.Close()

	gcsSrv, err := fakestorage.NewServerWithOptions(fakestorage.Options{Scheme: "http", Host: "127.0.0.1", Writer: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	defer gcsSrv.Stop()
	gcsSrv.CreateBucketWithOpts(fakestorage.CreateBucketOpts{Name: "bkt"})

	for _, tc := range []struct {
		name      string
		cfg       config.StorageConfig
		wantType  string
		roundTrip bool
	}{
		{"default fs", config.StorageConfig{FS: config.FSConfig{Root: filepath.Join(t.TempDir(), "d")}}, "fs", true},
		{"fs", config.StorageConfig{Type: "fs", FS: config.FSConfig{Root: t.TempDir()}}, "fs", true},
		{"s3", config.StorageConfig{Type: "S3", S3: config.S3Config{
			Bucket: "bkt", Region: "us-east-1", Endpoint: s3srv.URL, AccessKeyID: "k", SecretAccessKey: "s",
		}}, "s3", true},
		{"gcs", config.StorageConfig{Type: "gcs", GCS: config.GCSConfig{Bucket: "bkt", Endpoint: gcsSrv.URL() + "/storage/v1/"}}, "gcs", true},
		{"azure", config.StorageConfig{Type: "azure", Azure: config.AzureConfig{
			Container: "c", ConnectionString: "UseDevelopmentStorage=true",
		}}, "azure", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := Open(ctx, tc.cfg)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer s.Close()
			if got := s.Describe().Type; got != tc.wantType {
				t.Errorf("Describe().Type = %q, want %q", got, tc.wantType)
			}
			if !tc.roundTrip {
				return
			}
			if _, err := s.Put(ctx, "k.txt", strings.NewReader("v"), 1, ""); err != nil {
				t.Fatalf("Put: %v", err)
			}
			if info, err := s.Stat(ctx, "k.txt"); err != nil || info.Size != 1 {
				t.Errorf("Stat = %+v, %v", info, err)
			}
		})
	}

	for _, cfg := range []config.StorageConfig{
		{Type: "ftp"},
		{Type: "fs"},
		{Type: "s3"},
		{Type: "gcs"},
		{Type: "azure"},
	} {
		if s, err := Open(ctx, cfg); err == nil {
			s.Close()
			t.Errorf("Open(%+v) succeeded", cfg)
		}
	}
}
