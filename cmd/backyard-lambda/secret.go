package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
)

// server.secret from AWS Secrets Manager.
//
// When BACKYARD_SECRET_ARN names a secret, its value becomes server.secret
// at cold start, so the secret never sits in the function's environment.
// The value is read through the AWS Parameters and Secrets Lambda Extension
// when its layer is configured (PARAMETERS_SECRETS_EXTENSION_HTTP_PORT set,
// with AWS_SESSION_TOKEN as its auth token), otherwise directly from the
// Secrets Manager API (GetSecretValue, SigV4-signed with the function's
// role). Either way the role needs secretsmanager:GetSecretValue on the
// secret (and kms:Decrypt when it uses a customer managed key).
//
// The secret is either the plain value, or a JSON object (a key/value
// secret) with the value under "BACKYARD_SECRET" or "secret".

const (
	secretARNEnv     = "BACKYARD_SECRET_ARN"
	extensionPortEnv = "PARAMETERS_SECRETS_EXTENSION_HTTP_PORT"
	// secretFetchTimeout bounds the whole lookup (Lambda's init phase is
	// itself limited to 10 s).
	secretFetchTimeout = 8 * time.Second
	// maxSecretResponse bounds a GetSecretValue response (secrets are at
	// most 64 KiB, base64 and JSON framing included).
	maxSecretResponse = 256 << 10
	// maxErrorDetail bounds error bodies quoted in error messages.
	maxErrorDetail = 512
)

// secretFetcher returns the value of the secret named by a Secrets Manager
// ARN (or name) and describes where it was read from.
type secretFetcher func(ctx context.Context, secretID string) (value, source string, err error)

// fetchSecret reads a secret through the extension when it is configured,
// else from the Secrets Manager API.
func fetchSecret(ctx context.Context, secretID string) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, secretFetchTimeout)
	defer cancel()
	client := &http.Client{Timeout: 5 * time.Second}
	if port := strings.TrimSpace(os.Getenv(extensionPortEnv)); port != "" {
		token := os.Getenv("AWS_SESSION_TOKEN")
		if token == "" {
			return "", "", fmt.Errorf("%s is set but AWS_SESSION_TOKEN (the extension's auth token) is not", extensionPortEnv)
		}
		v, err := extensionGetSecret(ctx, client, "http://localhost:"+port, token, secretID)
		return v, "secrets-extension", err
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return "", "", fmt.Errorf("load AWS config: %w", err)
	}
	region := regionFromARN(secretID)
	if region == "" {
		region = awsCfg.Region
	}
	if region == "" {
		return "", "", errors.New("no AWS region (use a full secret ARN or set AWS_REGION)")
	}
	endpoint, err := secretsManagerEndpoint(secretID, region, os.Getenv)
	if err != nil {
		return "", "", err
	}
	v, err := apiGetSecret(ctx, client, endpoint, region, awsCfg.Credentials, secretID)
	return v, "secretsmanager-api", err
}

// getSecretValueOutput is the part of a GetSecretValue response we use (the
// extension returns the API's response verbatim).
type getSecretValueOutput struct {
	SecretString *string `json:"SecretString"`
	SecretBinary *string `json:"SecretBinary"` // base64
}

// extensionGetSecret reads a secret from the Parameters and Secrets Lambda
// Extension at base. The extension may still be starting at cold start, so
// connection errors and 5xx responses are retried until ctx ends.
func extensionGetSecret(ctx context.Context, client *http.Client, base, token, secretID string) (string, error) {
	u := strings.TrimSuffix(base, "/") + "/secretsmanager/get?secretId=" + url.QueryEscape(secretID)
	backoff := 50 * time.Millisecond
	for attempt := 1; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("X-Aws-Parameters-Secrets-Token", token)
		status, body, err := do(client, req)
		retry := err != nil || status >= 500 || status == http.StatusTooManyRequests
		if !retry {
			if status != http.StatusOK {
				return "", fmt.Errorf("secrets extension: HTTP %d: %s", status, errorDetail(body))
			}
			return decodeSecret(body)
		}
		if err == nil {
			err = fmt.Errorf("HTTP %d: %s", status, errorDetail(body))
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("secrets extension at %s: %w (after %d attempts: %w)", base, err, attempt, ctx.Err())
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, time.Second)
	}
}

// apiGetSecret calls Secrets Manager's GetSecretValue (JSON 1.1 protocol)
// at endpoint, signed with SigV4.
func apiGetSecret(ctx context.Context, client *http.Client, endpoint, region string, creds aws.CredentialsProvider, secretID string) (string, error) {
	if creds == nil {
		return "", errors.New("no AWS credentials")
	}
	c, err := creds.Retrieve(ctx)
	if err != nil {
		return "", fmt.Errorf("AWS credentials: %w", err)
	}
	payload, err := json.Marshal(map[string]string{"SecretId": secretID})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(endpoint, "/")+"/", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "secretsmanager.GetSecretValue")
	sum := sha256.Sum256(payload)
	if err := v4.NewSigner().SignHTTP(ctx, c, req, hex.EncodeToString(sum[:]), "secretsmanager", region, time.Now()); err != nil {
		return "", fmt.Errorf("sign request: %w", err)
	}
	status, body, err := do(client, req)
	if err != nil {
		return "", fmt.Errorf("secrets manager: %w", err)
	}
	if status != http.StatusOK {
		var e struct {
			Type    string `json:"__type"`
			Message string `json:"message"`
			Msg     string `json:"Message"`
		}
		if json.Unmarshal(body, &e) == nil && e.Type != "" {
			if _, typ, ok := strings.Cut(e.Type, "#"); ok {
				e.Type = typ
			}
			return "", fmt.Errorf("secrets manager: HTTP %d %s: %s", status, e.Type, e.Message+e.Msg)
		}
		return "", fmt.Errorf("secrets manager: HTTP %d: %s", status, errorDetail(body))
	}
	return decodeSecret(body)
}

// do sends req and reads a bounded response body.
func do(client *http.Client, req *http.Request) (int, []byte, error) {
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSecretResponse+1))
	if err != nil {
		return 0, nil, err
	}
	if len(body) > maxSecretResponse {
		return 0, nil, errors.New("response too large")
	}
	return resp.StatusCode, body, nil
}

// errorDetail renders an error body for messages (bounded, one line).
func errorDetail(body []byte) string {
	s := strings.Join(strings.Fields(string(body)), " ")
	if len(s) > maxErrorDetail {
		s = s[:maxErrorDetail] + "…"
	}
	return s
}

// decodeSecret extracts the secret from a GetSecretValue response.
func decodeSecret(body []byte) (string, error) {
	var out getSecretValueOutput
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("decode GetSecretValue response: %w", err)
	}
	var v string
	switch {
	case out.SecretString != nil:
		v = *out.SecretString
	case out.SecretBinary != nil:
		b, err := base64.StdEncoding.DecodeString(*out.SecretBinary)
		if err != nil {
			return "", fmt.Errorf("decode SecretBinary: %w", err)
		}
		v = string(b)
	}
	return secretValue(v)
}

// secretValue returns the secret held by a raw secret string: the string
// itself, or for a JSON object its "BACKYARD_SECRET" (or "secret") member.
func secretValue(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if strings.HasPrefix(v, "{") {
		var m map[string]any
		if err := json.Unmarshal([]byte(v), &m); err == nil {
			for _, k := range []string{"BACKYARD_SECRET", "secret"} {
				if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
					return strings.TrimSpace(s), nil
				}
			}
			return "", errors.New(`the secret is a JSON object without a "BACKYARD_SECRET" (or "secret") string member`)
		}
	}
	if v == "" {
		return "", errors.New("the secret is empty")
	}
	return v, nil
}

// regionFromARN returns the region of a Secrets Manager ARN
// (arn:<partition>:secretsmanager:<region>:<account>:secret:<name>), or ""
// for a plain secret name.
func regionFromARN(id string) string {
	parts := strings.SplitN(id, ":", 6)
	if len(parts) < 6 || parts[0] != "arn" || parts[2] != "secretsmanager" {
		return ""
	}
	return parts[3]
}

// secretsManagerEndpoint picks the API endpoint: AWS_ENDPOINT_URL_SECRETS_MANAGER
// or AWS_ENDPOINT_URL when set, else the regional (or FIPS, with
// AWS_USE_FIPS_ENDPOINT=true) endpoint of the ARN's partition.
func secretsManagerEndpoint(secretID, region string, getenv func(string) string) (string, error) {
	for _, k := range []string{"AWS_ENDPOINT_URL_SECRETS_MANAGER", "AWS_ENDPOINT_URL"} {
		if v := strings.TrimSpace(getenv(k)); v != "" {
			if u, err := url.Parse(v); err != nil || u.Scheme == "" || u.Host == "" {
				return "", fmt.Errorf("%s is not an absolute URL", k)
			}
			return v, nil
		}
	}
	if strings.ContainsAny(region, "/?#@:") || region == "" {
		return "", fmt.Errorf("invalid AWS region %q", region)
	}
	suffix := "amazonaws.com"
	if parts := strings.SplitN(secretID, ":", 3); len(parts) == 3 && parts[0] == "arn" && parts[1] == "aws-cn" {
		suffix = "amazonaws.com.cn"
	}
	host := "secretsmanager"
	if strings.EqualFold(strings.TrimSpace(getenv("AWS_USE_FIPS_ENDPOINT")), "true") {
		host = "secretsmanager-fips"
	}
	return "https://" + host + "." + region + "." + suffix, nil
}
