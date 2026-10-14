package charttest

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// chartDir is the chart, relative to this package.
const chartDir = "../backyard"

// headerMode is an auth.mode header configuration without JWT verification
// (oauth2-proxy's X-Forwarded-Email), on top of the chart's default
// trusted_proxies (every private range).
const headerMode = `
config:
  auth:
    mode: header
    header:
      email_header: X-Forwarded-Email
`

func helmBinary(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("helm")
	if err != nil {
		if v := os.Getenv("BACKYARD_TEST_HELM"); v != "" && v != "0" {
			t.Fatalf("BACKYARD_TEST_HELM is set but helm is not on PATH: %v", err)
		}
		t.Skip("helm is not installed (set BACKYARD_TEST_HELM=1 to require it)")
	}
	return p
}

// render runs `helm template` with the given values overlays (YAML texts,
// applied in order) and returns the rendered manifests, or the error output
// when helm fails.
func render(t *testing.T, overlays ...string) ([]map[string]any, error) {
	t.Helper()
	helm := helmBinary(t)
	args := []string{"template", "backyard", chartDir, "--namespace", "backyard"}
	for i, o := range overlays {
		p := filepath.Join(t.TempDir(), "values"+string(rune('a'+i))+".yaml")
		if err := os.WriteFile(p, []byte(o), 0o600); err != nil {
			t.Fatal(err)
		}
		args = append(args, "-f", p)
	}
	cmd := exec.Command(helm, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, errors.New(strings.TrimSpace(stderr.String()))
	}
	var docs []map[string]any
	dec := yaml.NewDecoder(&stdout)
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("parse helm output: %v", err)
		}
		if doc != nil {
			docs = append(docs, doc)
		}
	}
	return docs, nil
}

func ofKind(docs []map[string]any, kind string) []map[string]any {
	var out []map[string]any
	for _, d := range docs {
		if d["kind"] == kind {
			out = append(out, d)
		}
	}
	return out
}

// get walks a decoded manifest: string keys index maps, ints index lists.
func get(v any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				return nil
			}
			v = m[k]
		case int:
			l, ok := v.([]any)
			if !ok || k >= len(l) {
				return nil
			}
			v = l[k]
		}
	}
	return v
}

func toYAMLValue(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := yaml.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// TestHeaderModeTrust: in auth.mode header without jwt_header every peer in
// server.trusted_proxies may assert any identity, so the chart must not
// render a configuration where those are the whole pod network and nothing
// else keeps other pods away (any workload could sign in as an admin).
func TestHeaderModeTrust(t *testing.T) {
	tests := []struct {
		name    string
		overlay string
		wantErr string // "" = must render
	}{
		{
			name:    "default private ranges",
			wantErr: "10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, fc00::/7",
		},
		{
			name:    "pod network",
			overlay: "config: {server: {trusted_proxies: [10.244.0.0/16]}}",
			wantErr: "unknown at render time: 10.244.0.0/16.",
		},
		{
			name:    "IPv4 range wider than /24",
			overlay: "config: {server: {trusted_proxies: [10.1.2.3, 10.1.2.0/23]}}",
			wantErr: "render time: 10.1.2.0/23.",
		},
		{
			name:    "IPv6 range wider than /120",
			overlay: "config: {server: {trusted_proxies: [\"2001:db8::/119\"]}}",
			wantErr: "render time: 2001:db8::/119.",
		},
		{
			name:    "IPv4-mapped IPv6 range",
			overlay: "config: {server: {trusted_proxies: [\"::ffff:10.0.0.0/104\"]}}",
			wantErr: "::ffff:10.0.0.0/104",
		},
		{
			name:    "environment reference",
			overlay: "config: {server: {trusted_proxies: [\"${PROXY_CIDR}\"]}}",
			wantErr: "${PROXY_CIDR}",
		},
		{
			name:    "templated value",
			overlay: "proxyRange: 10.0.0.0/8\nconfig: {server: {trusted_proxies: [\"{{ .Values.proxyRange }}\"]}}",
			wantErr: "render time: 10.0.0.0/8.",
		},
		{
			name:    "upper-case mode",
			overlay: "config: {auth: {mode: Header}}",
			wantErr: "10.0.0.0/8",
		},
		{
			name:    "no trusted proxies",
			overlay: "config: {server: {trusted_proxies: null}}",
			wantErr: "needs config.server.trusted_proxies",
		},
		{
			name:    "the proxy's own addresses",
			overlay: "config: {server: {trusted_proxies: [10.1.2.3, 10.1.2.0/24, \"2001:db8::10\", \"2001:db8::/120\", 10.9.9.9/32]}}",
		},
		{
			name:    "network policy admits only the ingress controller",
			overlay: "networkPolicy: {enabled: true}",
		},
		{
			name:    "signed JWT",
			overlay: "config: {auth: {header: {jwt_header: X-Pomerium-Jwt-Assertion, jwks_url: \"https://auth.acme.com/.well-known/pomerium/jwks.json\"}}}",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			docs, err := render(t, headerMode, tt.overlay)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("render failed: %v", err)
				}
				if len(ofKind(docs, "Deployment")) != 1 {
					t.Fatal("no Deployment rendered")
				}
				return
			}
			if err == nil {
				t.Fatalf("rendered; want an error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
}

// TestOtherModesKeepTheDefaultProxies: the private-range default stays fine
// where trusted_proxies only governs X-Forwarded-* (client IPs).
func TestOtherModesKeepTheDefaultProxies(t *testing.T) {
	for _, overlay := range []string{
		"",
		"config: {auth: {mode: iap, iap: {audience: /projects/1/global/backendServices/2}}}",
		"config: {auth: {mode: alb, alb: {region: us-east-1}}}",
	} {
		if _, err := render(t, overlay); err != nil {
			t.Errorf("values %q: %v", overlay, err)
		}
	}
}

func TestNetworkPolicy(t *testing.T) {
	ingressNginx := toYAMLValue(t, `
- namespaceSelector:
    matchLabels:
      kubernetes.io/metadata.name: ingress-nginx`)

	t.Run("disabled by default", func(t *testing.T) {
		docs, err := render(t)
		if err != nil {
			t.Fatal(err)
		}
		if n := len(ofKind(docs, "NetworkPolicy")); n != 0 {
			t.Fatalf("%d NetworkPolicies rendered by default", n)
		}
	})

	t.Run("admits only networkPolicy.from to the HTTP port", func(t *testing.T) {
		docs, err := render(t, "networkPolicy: {enabled: true}")
		if err != nil {
			t.Fatal(err)
		}
		nps := ofKind(docs, "NetworkPolicy")
		if len(nps) != 1 {
			t.Fatalf("%d NetworkPolicies, want 1", len(nps))
		}
		np := nps[0]
		if got := get(np, "spec", "policyTypes"); !reflect.DeepEqual(got, []any{"Ingress"}) {
			t.Errorf("policyTypes = %v, want [Ingress] (egress stays open)", got)
		}
		rules, _ := get(np, "spec", "ingress").([]any)
		if len(rules) != 1 {
			t.Fatalf("ingress rules = %v, want one (no metricsFrom)", rules)
		}
		if got := get(rules[0], "from"); !reflect.DeepEqual(got, ingressNginx) {
			t.Errorf("from = %v, want %v", got, ingressNginx)
		}
		if got := get(rules[0], "ports"); !reflect.DeepEqual(got, toYAMLValue(t, "[{protocol: TCP, port: 8080}]")) {
			t.Errorf("ports = %v, want TCP 8080", got)
		}
		// The policy must select the Backyard pods.
		sel, _ := get(np, "spec", "podSelector", "matchLabels").(map[string]any)
		podLabels, _ := get(ofKind(docs, "Deployment")[0], "spec", "template", "metadata", "labels").(map[string]any)
		if len(sel) == 0 {
			t.Fatal("empty podSelector")
		}
		for k, v := range sel {
			if podLabels[k] != v {
				t.Errorf("podSelector %s=%v does not match the pod labels %v", k, v, podLabels)
			}
		}
	})

	t.Run("custom HTTP port and metrics peers", func(t *testing.T) {
		docs, err := render(t, `
ports: {http: 9000}
config: {server: {listen: ":9000"}}
networkPolicy:
  enabled: true
  from: [{podSelector: {matchLabels: {app: oauth2-proxy}}}]
  metricsFrom: [{namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: monitoring}}}]
`)
		if err != nil {
			t.Fatal(err)
		}
		rules, _ := get(ofKind(docs, "NetworkPolicy")[0], "spec", "ingress").([]any)
		if len(rules) != 2 {
			t.Fatalf("ingress rules = %v, want HTTP and metrics", rules)
		}
		if got := get(rules[0], "ports", 0, "port"); got != 9000 {
			t.Errorf("HTTP port = %v, want 9000", got)
		}
		if got := get(rules[0], "from", 0, "podSelector", "matchLabels", "app"); got != "oauth2-proxy" {
			t.Errorf("HTTP peers = %v", get(rules[0], "from"))
		}
		if got := get(rules[1], "ports", 0, "port"); got != 9090 {
			t.Errorf("metrics port = %v, want 9090", got)
		}
		if got := get(rules[1], "from", 0, "namespaceSelector", "matchLabels", "kubernetes.io/metadata.name"); got != "monitoring" {
			t.Errorf("metrics peers = %v", get(rules[1], "from"))
		}
	})

	t.Run("production values", func(t *testing.T) {
		prod, err := os.ReadFile(filepath.Join(chartDir, "values-production.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		docs, err := render(t, string(prod))
		if err != nil {
			t.Fatal(err)
		}
		nps := ofKind(docs, "NetworkPolicy")
		if len(nps) != 1 {
			t.Fatalf("%d NetworkPolicies, want 1", len(nps))
		}
		if rules, _ := get(nps[0], "spec", "ingress").([]any); len(rules) != 2 {
			t.Errorf("ingress rules = %v, want HTTP and metrics (the ServiceMonitor scrapes)", rules)
		}
	})

	for _, tt := range []struct{ name, overlay, wantErr string }{
		{"empty from would admit everyone", "networkPolicy: {enabled: true, from: []}", "needs networkPolicy.from"},
		{"ServiceMonitor without metrics peers", "networkPolicy: {enabled: true}\nserviceMonitor: {enabled: true}", "needs networkPolicy.metricsFrom"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := render(t, tt.overlay)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}
