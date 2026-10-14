package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/thasami/backyard/internal/clientconfig"
	"github.com/thasami/backyard/internal/deploy"
)

const (
	// infoTimeout bounds GET /info probes.
	infoTimeout = 15 * time.Second
	// apiTimeout bounds simple gateway calls (list, info, delete, me).
	apiTimeout = 60 * time.Second
)

// overrides returns the global flag values as clientconfig overrides.
func (a *cliApp) overrides() clientconfig.Overrides {
	return clientconfig.Overrides{Profile: a.g.profile, Server: a.g.server, Gateway: a.g.gateway, Token: a.g.token}
}

// configPath returns the CLI config file path.
func (a *cliApp) configPath() (string, error) {
	if a.deps.configPath != "" {
		return a.deps.configPath, nil
	}
	return clientconfig.DefaultPath()
}

// session opens the client session (refreshing device tokens) and prints
// its warnings.
func (a *cliApp) session(ctx context.Context, o clientconfig.Overrides) (*clientconfig.Session, error) {
	path, err := a.configPath()
	if err != nil {
		return nil, err
	}
	s, err := clientconfig.OpenSession(ctx, clientconfig.SessionOptions{
		ConfigPath: path, Overrides: o, HTTP: a.deps.httpClient, Now: a.deps.now, Logger: a.log,
	})
	if err != nil {
		return nil, err
	}
	for _, w := range s.Warnings {
		a.warnf("%s", w)
	}
	return s, nil
}

// gatewaySession opens a session that must have a gateway.
func (a *cliApp) gatewaySession(ctx context.Context) (*clientconfig.Session, *deploy.Client, error) {
	s, err := a.session(ctx, a.overrides())
	if err != nil {
		return nil, nil, err
	}
	if err := s.RequireGateway(); err != nil {
		return nil, nil, err
	}
	return s, a.client(s.Gateway, s.Token), nil
}

// client returns a gateway client.
func (a *cliApp) client(gateway, token string) *deploy.Client {
	c := deploy.NewClient(gateway, token)
	if a.deps.httpClient != nil {
		c.HTTP = a.deps.httpClient
	}
	return c
}

// fetchInfo GETs /info with a short timeout.
func (a *cliApp) fetchInfo(ctx context.Context, c *deploy.Client) (*deploy.Info, error) {
	ctx, cancel := context.WithTimeout(ctx, infoTimeout)
	defer cancel()
	info, err := c.Info(ctx)
	if err != nil {
		return nil, err
	}
	if info.Name == "" && info.PublicURL == "" && len(info.Methods) == 0 && info.Auth.Mode == "" {
		return nil, errors.New("the response is not a Backyard server info document")
	}
	return info, nil
}

// explain adds actionable hints to gateway errors.
func explain(err error, s *clientconfig.Session) error {
	if err == nil {
		return nil
	}
	var ae *deploy.APIError
	if errors.As(err, &ae) {
		switch ae.Status {
		case http.StatusUnauthorized:
			hint := "run `backyard login` to sign in"
			if s != nil && s.Token != "" {
				hint = "your token was rejected (expired or revoked?); run `backyard login` again"
			}
			return fmt.Errorf("%w\n  hint: %s", err, hint)
		case http.StatusForbidden:
			return fmt.Errorf("%w\n  hint: your account is not allowed to do this (site ownership or admin rights)", err)
		}
		if strings.Contains(ae.Message, "<html") || strings.Contains(strings.ToLower(ae.Message), "<!doctype") {
			return fmt.Errorf("%w\n  hint: %s", err, proxyHint)
		}
		return err
	}
	if strings.Contains(err.Error(), "decode") && strings.Contains(err.Error(), "invalid character '<'") {
		return fmt.Errorf("%w\n  hint: %s", err, proxyHint)
	}
	return err
}

// proxyHint explains how to reach a gateway behind an identity-aware proxy.
const proxyHint = "the server answered with a web page instead of the API. If Backyard runs behind an " +
	"identity-aware proxy (Google IAP, AWS ALB, Cloudflare Access, oauth2-proxy…) the CLI cannot pass its " +
	"browser login: ask your admin for the proxy-exempt deploy gateway URL (or the standalone gateway/Lambda URL) " +
	"and run `backyard login --server <hub-url> --gateway <gateway-url>`"

// printJSON writes v as indented JSON to stdout.
func (a *cliApp) printJSON(v any) error {
	enc := json.NewEncoder(a.streams.Out)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// outf prints to stdout.
func (a *cliApp) outf(format string, args ...any) { fmt.Fprintf(a.streams.Out, format, args...) }

// errf prints to stderr.
func (a *cliApp) errf(format string, args ...any) { fmt.Fprintf(a.streams.ErrOut, format, args...) }

// warnf prints a warning to stderr.
func (a *cliApp) warnf(format string, args ...any) {
	fmt.Fprintf(a.streams.ErrOut, "Warning: "+format+"\n", args...)
}

// reader returns the shared buffered stdin reader.
func (a *cliApp) reader() *bufio.Reader {
	a.inOnce.Do(func() { a.in = bufio.NewReader(a.streams.In) })
	return a.in
}

// readLine reads one line from stdin (without the newline).
func (a *cliApp) readLine(ctx context.Context) (string, error) {
	type res struct {
		s   string
		err error
	}
	ch := make(chan res, 1)
	go func() {
		s, err := a.reader().ReadString('\n')
		if errors.Is(err, io.EOF) && s != "" {
			err = nil
		}
		ch <- res{strings.TrimRight(s, "\r\n"), err}
	}()
	select {
	case r := <-ch:
		return r.s, r.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// readSecret reads one line like readLine; on a Unix terminal, echo is
// turned off while the user types or pastes (tokens must not end up in the
// terminal's scrollback).
func (a *cliApp) readSecret(ctx context.Context) (string, error) {
	f, ok := a.streams.In.(*os.File)
	if !ok || runtime.GOOS == "windows" || !a.deps.stdinTTY() {
		return a.readLine(ctx)
	}
	stty := func(arg string) error {
		c := exec.CommandContext(context.WithoutCancel(ctx), "stty", arg)
		c.Stdin = f
		return c.Run()
	}
	if err := stty("-echo"); err != nil {
		return a.readLine(ctx) // no stty: fall back to a visible prompt
	}
	defer func() {
		_ = stty("echo")
		a.errf("\n")
	}()
	return a.readLine(ctx)
}

// errNotInteractive is returned when a confirmation is required but stdin
// is not a terminal.
var errNotInteractive = errors.New("confirmation required but stdin is not a terminal; pass -y to confirm")

// confirm asks a yes/no question on stderr (default no).
func (a *cliApp) confirm(ctx context.Context, question string) (bool, error) {
	if !a.deps.stdinTTY() {
		return false, errNotInteractive
	}
	a.errf("%s [y/N] ", question)
	line, err := a.readLine(ctx)
	if err != nil {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	}
	return false, nil
}

// openURL opens u in the browser, printing it as a fallback.
func (a *cliApp) openURL(ctx context.Context, u string) {
	if a.deps.openBrowser == nil {
		return
	}
	if err := a.deps.openBrowser(ctx, u); err != nil {
		a.log.Debug("open browser failed", "error", err)
		a.errf("Could not open a browser (%v); open %s manually.\n", err, u)
	}
}

// cwd returns the working directory.
func (a *cliApp) cwd() (string, error) {
	d, err := a.deps.getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}
	return d, nil
}

// absFrom resolves p against base when relative.
func absFrom(base, p string) string {
	if p == "" {
		return base
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(base, p)
}

// buildDirNames are output folders that make poor site names.
var buildDirNames = []string{"dist", "build", "out", "public", "_site", "output"}

// defaultSiteName derives a site name from a folder, using the parent's
// name for typical build-output folders (dist, build, public…).
func defaultSiteName(dir string) string {
	base := strings.ToLower(filepath.Base(dir))
	if slices.Contains(buildDirNames, base) {
		if n := clientconfig.SiteNameFromDir(filepath.Dir(dir)); n != "" {
			return n
		}
	}
	return clientconfig.SiteNameFromDir(dir)
}

// humanBytes formats a byte count.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// ago formats t relative to now.
func ago(now, t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := now.Sub(t)
	switch {
	case d < 0:
		return t.Local().Format("2006-01-02 15:04")
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
	return t.Local().Format("2006-01-02")
}

// truncate shortens s to n runes with an ellipsis.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

// fileExists reports whether path exists.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
