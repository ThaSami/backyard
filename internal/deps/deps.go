//go:build deps

// Package deps pins every third-party module Backyard uses so that go.mod and
// go.sum stay complete while packages are developed in parallel. It is never
// compiled into the binary (build tag "deps").
package deps

import (
	_ "cloud.google.com/go/bigquery"
	_ "cloud.google.com/go/storage"
	_ "github.com/Azure/azure-sdk-for-go/sdk/azcore"
	_ "github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	_ "github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	_ "github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/sas"
	_ "github.com/anthropics/anthropic-sdk-go"
	_ "github.com/anthropics/anthropic-sdk-go/bedrock"
	_ "github.com/anthropics/anthropic-sdk-go/vertex"
	_ "github.com/aws/aws-lambda-go/events"
	_ "github.com/aws/aws-lambda-go/lambda"
	_ "github.com/aws/aws-sdk-go-v2/aws"
	_ "github.com/aws/aws-sdk-go-v2/config"
	_ "github.com/aws/aws-sdk-go-v2/credentials"
	_ "github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	_ "github.com/aws/aws-sdk-go-v2/service/athena"
	_ "github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	_ "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	_ "github.com/aws/aws-sdk-go-v2/service/s3"
	_ "github.com/coder/websocket"
	_ "github.com/coreos/go-oidc/v3/oidc"
	_ "github.com/golang-jwt/jwt/v5"
	_ "github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/modelcontextprotocol/go-sdk/mcp"
	_ "github.com/prometheus/client_golang/prometheus"
	_ "github.com/prometheus/client_golang/prometheus/promhttp"
	_ "github.com/redis/go-redis/v9"
	_ "github.com/spf13/cobra"
	_ "golang.org/x/oauth2"
	_ "golang.org/x/oauth2/google"
	_ "golang.org/x/time/rate"
	_ "google.golang.org/api/iterator"
	_ "google.golang.org/api/option"
	_ "gopkg.in/yaml.v3"
	_ "modernc.org/sqlite"

	// test-only fakes
	_ "github.com/fsouza/fake-gcs-server/fakestorage"
	_ "github.com/johannesboyne/gofakes3"
	_ "github.com/johannesboyne/gofakes3/backend/s3mem"
)
