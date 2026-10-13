package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/thasami/backyard/internal/version"
)

func (a *cliApp) newTokenCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Create API tokens and hash static tokens",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(a.newTokenCreateCommand(), a.newTokenHashCommand())
	return cmd
}

// tokenCreateResponse is the body of POST /__backyard/api/tokens.
type tokenCreateResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (a *cliApp) newTokenCreateCommand() *cobra.Command {
	var (
		name string
		ttl  string
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Mint a personal API token (e.g. for CI)",
		Long: `Mint a personal API token acting as you, e.g. for CI pipelines (BACKYARD_TOKEN).
The token is printed on stdout; store it as a secret.`,
		Example: `  backyard token create --name github-actions --ttl 168h`,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d, err := parseTTL(ttl)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), apiTimeout)
			defer cancel()
			s, err := a.session(ctx, a.overrides())
			if err != nil {
				return err
			}
			if s.Server == "" {
				return errors.New("no server URL configured (tokens are minted by the hub): run `backyard login --server <url>`")
			}
			if s.Token == "" {
				return errors.New("not logged in: run `backyard login` first")
			}
			res, err := a.createToken(ctx, s.Server, s.Token, name, d)
			if err != nil {
				return explain(err, s)
			}
			if a.g.json {
				return a.printJSON(res)
			}
			a.outf("%s\n", res.Token)
			if !res.ExpiresAt.IsZero() {
				a.errf("Token %q expires %s. Store it as a secret (e.g. BACKYARD_TOKEN).\n",
					name, res.ExpiresAt.Local().Format("2006-01-02 15:04 MST"))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "cli", "token name (shown in audit logs)")
	cmd.Flags().StringVar(&ttl, "ttl", "720h", "token lifetime, e.g. 720h or 90d")
	return cmd
}

// parseTTL parses a Go duration or a number of days ("90d").
func parseTTL(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	var d time.Duration
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.ParseFloat(days, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid --ttl %q", s)
		}
		d = time.Duration(n * float64(24*time.Hour))
	} else {
		v, err := time.ParseDuration(s)
		if err != nil {
			return 0, fmt.Errorf("invalid --ttl %q (use e.g. 720h or 30d)", s)
		}
		d = v
	}
	if d < time.Hour {
		return 0, fmt.Errorf("--ttl must be at least 1h")
	}
	return d, nil
}

// createToken calls POST <server>/__backyard/api/tokens.
func (a *cliApp) createToken(ctx context.Context, server, bearer, name string, ttl time.Duration) (*tokenCreateResponse, error) {
	body, err := json.Marshal(map[string]any{"name": name, "ttl_hours": int(math.Ceil(ttl.Hours()))})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(server, "/")+"/__backyard/api/tokens", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Backyard", "1")
	req.Header.Set("User-Agent", "backyard-cli/"+version.Version)
	hc := a.deps.httpClient
	if hc == nil {
		hc = &http.Client{Timeout: apiTimeout}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("create token: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("create token: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, apiErrorFrom(resp.StatusCode, b)
	}
	var out tokenCreateResponse
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("create token: decode response: %w", err)
	}
	if out.Token == "" {
		return nil, errors.New("create token: the server returned no token")
	}
	return &out, nil
}

func (a *cliApp) newTokenHashCommand() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "hash [token]",
		Short: "Print the sha256 of a static token for auth.tokens",
		Long: `Print the hex sha256 of a static bearer token and the backyard.yaml snippet that
accepts it. Pass "-" (or nothing) to read the token from stdin and keep it out of
your shell history.`,
		Example: `  openssl rand -hex 32 | tee ci-token.txt | backyard token hash --name ci`,
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			tok := ""
			if len(args) == 1 && args[0] != "-" {
				tok = args[0]
			} else {
				if a.deps.stdinTTY() {
					a.errf("Token: ")
				}
				line, err := a.readSecret(cmd.Context())
				if err != nil && !errors.Is(err, io.EOF) {
					return fmt.Errorf("read token: %w", err)
				}
				tok = line
			}
			tok = strings.TrimSpace(tok)
			if tok == "" {
				return errors.New("empty token")
			}
			sum := sha256.Sum256([]byte(tok))
			h := hex.EncodeToString(sum[:])
			if a.g.json {
				return a.printJSON(map[string]string{"name": name, "sha256": h})
			}
			a.outf("%s\n\n# backyard.yaml\nauth:\n  tokens:\n    - name: %s\n      sha256: %s\n", h, yamlScalar(name), h)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "ci", "token name for the YAML snippet")
	return cmd
}

// yamlScalar quotes s when it is not a plain YAML scalar.
func yamlScalar(s string) string {
	plain := s != ""
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			plain = false
			break
		}
	}
	if plain {
		return s
	}
	b, _ := json.Marshal(s)
	return string(b)
}
