package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
)

const (
	testARN    = "arn:aws:secretsmanager:eu-west-1:111122223333:secret:backyard/server-secret-AbCdEf"
	testSecret = "0123456789abcdef0123456789abcdef0123456789abcdef"
)

func secretJSON(t *testing.T, value string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"ARN": testARN, "Name": "backyard/server-secret", "SecretString": value, "VersionId": "v1"})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestExtensionGetSecret(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/secretsmanager/get" || r.URL.Query().Get("secretId") != testARN {
			http.Error(w, "unexpected request "+r.URL.String(), http.StatusBadRequest)
			return
		}
		if r.Header.Get("X-Aws-Parameters-Secrets-Token") != "session-token" {
			http.Error(w, "missing token", http.StatusForbidden)
			return
		}
		if n == 1 {
			// Still starting up at cold start.
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, secretJSON(t, testSecret))
	}))
	defer ts.Close()
	ctx := context.Background()
	got, err := extensionGetSecret(ctx, ts.Client(), ts.URL, "session-token", testARN)
	if err != nil || got != testSecret {
		t.Fatalf("extensionGetSecret = %q, %v", got, err)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2 (one retry)", calls.Load())
	}

	// Client errors are not retried and never echo the token.
	calls.Store(0)
	_, err = extensionGetSecret(ctx, ts.Client(), ts.URL, "wrong-token", testARN)
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") || strings.Contains(err.Error(), "wrong-token") || calls.Load() != 1 {
		t.Errorf("forbidden: err = %v after %d calls", err, calls.Load())
	}

	// An extension that never comes up fails once the context ends.
	cctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if _, err := extensionGetSecret(cctx, ts.Client(), "http://127.0.0.1:1", "t", testARN); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("unreachable extension: %v", err)
	}
}

func TestAPIGetSecret(t *testing.T) {
	var gotAuth, gotToken, gotTarget, gotType string
	var gotBody map[string]string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotToken = r.Header.Get("X-Amz-Security-Token")
		gotTarget = r.Header.Get("X-Amz-Target")
		gotType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		if r.Method != http.MethodPost || r.URL.Path != "/" {
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		if gotBody["SecretId"] == "missing" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"__type":"com.amazonaws.secretsmanager#ResourceNotFoundException","Message":"Secrets Manager can't find the specified secret."}`)
			return
		}
		_, _ = io.WriteString(w, secretJSON(t, testSecret))
	}))
	defer ts.Close()
	creds := credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret-key", "session")
	ctx := context.Background()
	got, err := apiGetSecret(ctx, ts.Client(), ts.URL, "eu-west-1", creds, testARN)
	if err != nil || got != testSecret {
		t.Fatalf("apiGetSecret = %q, %v", got, err)
	}
	day := time.Now().UTC().Format("20060102")
	if !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/"+day+"/eu-west-1/secretsmanager/aws4_request") ||
		!strings.Contains(gotAuth, "x-amz-target") {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotToken != "session" || gotTarget != "secretsmanager.GetSecretValue" || gotType != "application/x-amz-json-1.1" || gotBody["SecretId"] != testARN {
		t.Errorf("request: token %q target %q type %q body %v", gotToken, gotTarget, gotType, gotBody)
	}

	_, err = apiGetSecret(ctx, ts.Client(), ts.URL, "eu-west-1", creds, "missing")
	if err == nil || !strings.Contains(err.Error(), "ResourceNotFoundException") || !strings.Contains(err.Error(), "can't find") {
		t.Errorf("missing secret: %v", err)
	}
	if _, err := apiGetSecret(ctx, ts.Client(), ts.URL, "eu-west-1", nil, testARN); err == nil {
		t.Error("no credentials: no error")
	}
	failing := aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{}, errors.New("no role")
	})
	if _, err := apiGetSecret(ctx, ts.Client(), ts.URL, "eu-west-1", failing, testARN); err == nil || !strings.Contains(err.Error(), "no role") {
		t.Errorf("credential failure: %v", err)
	}
}

func TestSecretValue(t *testing.T) {
	for _, tc := range []struct {
		raw, want, err string
	}{
		{raw: testSecret, want: testSecret},
		{raw: "  " + testSecret + "\n", want: testSecret},
		{raw: `{"BACKYARD_SECRET":"` + testSecret + `","other":"x"}`, want: testSecret},
		{raw: `{"secret":"` + testSecret + `"}`, want: testSecret},
		{raw: `{"password":"x"}`, err: "JSON object"},
		{raw: `{not json but a secret}`, want: `{not json but a secret}`},
		{raw: "   ", err: "empty"},
	} {
		got, err := secretValue(tc.raw)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("secretValue(%q) = %q, %v; want error %q", tc.raw, got, err, tc.err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("secretValue(%q) = %q, %v; want %q", tc.raw, got, err, tc.want)
		}
	}
	bin := fmt.Sprintf(`{"SecretBinary":%q}`, base64.StdEncoding.EncodeToString([]byte(testSecret)))
	if got, err := decodeSecret([]byte(bin)); err != nil || got != testSecret {
		t.Errorf("binary secret = %q, %v", got, err)
	}
	if _, err := decodeSecret([]byte(`{"SecretBinary":"!!"}`)); err == nil {
		t.Error("bad base64: no error")
	}
	if _, err := decodeSecret([]byte(`{}`)); err == nil {
		t.Error("no value: no error")
	}
}

func TestSecretsManagerEndpoint(t *testing.T) {
	env := func(kv ...string) func(string) string {
		m := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return func(k string) string { return m[k] }
	}
	for _, tc := range []struct {
		id, region string
		getenv     func(string) string
		want       string
	}{
		{testARN, "eu-west-1", env(), "https://secretsmanager.eu-west-1.amazonaws.com"},
		{"arn:aws-cn:secretsmanager:cn-north-1:1:secret:x", "cn-north-1", env(), "https://secretsmanager.cn-north-1.amazonaws.com.cn"},
		{testARN, "us-gov-west-1", env("AWS_USE_FIPS_ENDPOINT", "true"), "https://secretsmanager-fips.us-gov-west-1.amazonaws.com"},
		{testARN, "eu-west-1", env("AWS_ENDPOINT_URL", "http://localhost:4566"), "http://localhost:4566"},
		{testARN, "eu-west-1", env("AWS_ENDPOINT_URL", "http://a", "AWS_ENDPOINT_URL_SECRETS_MANAGER", "http://b"), "http://b"},
	} {
		got, err := secretsManagerEndpoint(tc.id, tc.region, tc.getenv)
		if err != nil || got != tc.want {
			t.Errorf("secretsManagerEndpoint(%s, %s) = %q, %v; want %q", tc.id, tc.region, got, err, tc.want)
		}
	}
	if _, err := secretsManagerEndpoint(testARN, "evil.com/x", env()); err == nil {
		t.Error("invalid region accepted")
	}
	if _, err := secretsManagerEndpoint(testARN, "eu-west-1", env("AWS_ENDPOINT_URL", "not a url")); err == nil {
		t.Error("invalid endpoint accepted")
	}
	if got := regionFromARN(testARN); got != "eu-west-1" {
		t.Errorf("regionFromARN = %q", got)
	}
	if got := regionFromARN("backyard/server-secret"); got != "" {
		t.Errorf("regionFromARN(name) = %q", got)
	}
}

// fetchSecret uses the extension when its port is configured.
func TestFetchSecretViaExtension(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Aws-Parameters-Secrets-Token") != "tok" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		_, _ = io.WriteString(w, secretJSON(t, testSecret))
	}))
	defer ts.Close()
	port := ts.URL[strings.LastIndexByte(ts.URL, ':')+1:]
	t.Setenv(extensionPortEnv, port)
	t.Setenv("AWS_SESSION_TOKEN", "tok")
	got, src, err := fetchSecret(context.Background(), testARN)
	if err != nil || got != testSecret || src != "secrets-extension" {
		t.Fatalf("fetchSecret = %q, %q, %v", got, src, err)
	}
	t.Setenv("AWS_SESSION_TOKEN", "")
	if _, _, err := fetchSecret(context.Background(), testARN); err == nil || !strings.Contains(err.Error(), "AWS_SESSION_TOKEN") {
		t.Errorf("without a session token: %v", err)
	}
}

// fetchSecret falls back to the API with the default credential chain.
func TestFetchSecretViaAPI(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Authorization"), "Credential=AKIDENV/") {
			http.Error(w, "unsigned", http.StatusForbidden)
			return
		}
		_, _ = io.WriteString(w, secretJSON(t, testSecret))
	}))
	defer ts.Close()
	isolateAWSEnv(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDENV")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("AWS_ENDPOINT_URL_SECRETS_MANAGER", ts.URL)
	got, src, err := fetchSecret(context.Background(), testARN)
	if err != nil || got != testSecret || src != "secretsmanager-api" {
		t.Fatalf("fetchSecret = %q, %q, %v", got, src, err)
	}
	// A bare secret name needs a region from the environment.
	if _, _, err := fetchSecret(context.Background(), "backyard/server-secret"); err == nil || !strings.Contains(err.Error(), "region") {
		t.Errorf("name without region: %v", err)
	}
	t.Setenv("AWS_REGION", "eu-west-1")
	if got, _, err := fetchSecret(context.Background(), "backyard/server-secret"); err != nil || got != testSecret {
		t.Errorf("name with AWS_REGION = %q, %v", got, err)
	}
}

// isolateAWSEnv keeps the developer's AWS configuration out of a test.
func isolateAWSEnv(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	for _, k := range []string{"AWS_PROFILE", "AWS_DEFAULT_PROFILE", "AWS_REGION", "AWS_DEFAULT_REGION", "AWS_SESSION_TOKEN",
		"AWS_ENDPOINT_URL", "AWS_ENDPOINT_URL_SECRETS_MANAGER", "AWS_USE_FIPS_ENDPOINT", extensionPortEnv,
		"AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN", "AWS_CONTAINER_CREDENTIALS_FULL_URI", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"} {
		t.Setenv(k, "")
	}
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
}

const lambdaYAML = `
server:
  base_domain: backyard.example.com
  public_url: https://backyard.example.com
storage:
  type: s3
  s3:
    bucket: backyard-sites
    region: eu-west-1
auth:
  mode: none
`

// clearConfigEnv isolates loadConfig from the environment and the working
// directory.
func clearConfigEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"BACKYARD_CONFIG_YAML", "BACKYARD_CONFIG", "BACKYARD_SECRET", secretARNEnv,
		"BACKYARD_BASE_DOMAIN", "BACKYARD_PUBLIC_URL", "DATABASE_URL", "PORT"} {
		t.Setenv(k, "")
	}
	t.Chdir(t.TempDir())
}

func TestLoadConfigReadsSecretFromSecretsManager(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("BACKYARD_CONFIG_YAML", lambdaYAML)
	t.Setenv(secretARNEnv, " "+testARN+" ")
	var asked string
	fetch := func(_ context.Context, id string) (string, string, error) {
		asked = id
		return testSecret, "test", nil
	}
	cfg, src, err := loadConfig(context.Background(), fetch)
	if err != nil {
		t.Fatal(err)
	}
	if asked != testARN || src != "test" || cfg.Server.Secret != testSecret || cfg.Storage.S3.Bucket != "backyard-sites" {
		t.Errorf("asked %q, source %q, secret %q, config %+v", asked, src, cfg.Server.Secret, cfg.Server)
	}

	// Fetch failures fail the cold start.
	_, _, err = loadConfig(context.Background(), func(context.Context, string) (string, string, error) {
		return "", "", errors.New("AccessDeniedException")
	})
	if err == nil || !strings.Contains(err.Error(), "AccessDeniedException") || !strings.Contains(err.Error(), testARN) {
		t.Errorf("fetch failure: %v", err)
	}

	// A short secret still fails validation.
	_, _, err = loadConfig(context.Background(), func(context.Context, string) (string, string, error) { return "short", "test", nil })
	if err == nil || !strings.Contains(err.Error(), "server.secret") {
		t.Errorf("short secret: %v", err)
	}
}

func TestLoadConfigSecretSources(t *testing.T) {
	noFetch := func(context.Context, string) (string, string, error) {
		t.Error("Secrets Manager consulted without BACKYARD_SECRET_ARN")
		return "", "", errors.New("unexpected")
	}
	t.Run("BACKYARD_SECRET", func(t *testing.T) {
		clearConfigEnv(t)
		t.Setenv("BACKYARD_CONFIG_YAML", lambdaYAML)
		t.Setenv("BACKYARD_SECRET", testSecret)
		cfg, src, err := loadConfig(context.Background(), noFetch)
		if err != nil || cfg.Server.Secret != testSecret || src != "" {
			t.Errorf("loadConfig = %v, %q, %v", cfg, src, err)
		}
	})
	t.Run("no secret", func(t *testing.T) {
		clearConfigEnv(t)
		t.Setenv("BACKYARD_CONFIG_YAML", lambdaYAML)
		if _, _, err := loadConfig(context.Background(), noFetch); err == nil || !strings.Contains(err.Error(), "server.secret") {
			t.Errorf("loadConfig without a secret: %v", err)
		}
	})
	t.Run("both", func(t *testing.T) {
		clearConfigEnv(t)
		t.Setenv("BACKYARD_CONFIG_YAML", lambdaYAML)
		t.Setenv("BACKYARD_SECRET", testSecret)
		t.Setenv(secretARNEnv, testARN)
		if _, _, err := loadConfig(context.Background(), noFetch); err == nil || !strings.Contains(err.Error(), "set only one") {
			t.Errorf("loadConfig with both: %v", err)
		}
	})
	t.Run("config file", func(t *testing.T) {
		clearConfigEnv(t)
		path := filepath.Join(t.TempDir(), "backyard.yaml")
		if err := os.WriteFile(path, []byte(lambdaYAML), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("BACKYARD_CONFIG", path)
		t.Setenv(secretARNEnv, testARN)
		cfg, _, err := loadConfig(context.Background(), func(context.Context, string) (string, string, error) { return testSecret, "test", nil })
		if err != nil || cfg.Server.BaseDomain != "backyard.example.com" {
			t.Errorf("loadConfig(BACKYARD_CONFIG) = %v, %v", cfg, err)
		}
		t.Setenv("BACKYARD_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))
		if _, _, err := loadConfig(context.Background(), noFetch); err == nil {
			t.Error("missing BACKYARD_CONFIG file: no error")
		}
	})
	t.Run("invalid yaml", func(t *testing.T) {
		clearConfigEnv(t)
		t.Setenv("BACKYARD_CONFIG_YAML", "server: [")
		if _, _, err := loadConfig(context.Background(), noFetch); err == nil {
			t.Error("invalid YAML: no error")
		}
	})
}
