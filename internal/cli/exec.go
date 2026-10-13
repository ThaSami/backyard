package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Command is an external process invocation.
type Command struct {
	Name string
	Args []string
	// Dir is the working directory ("" = current).
	Dir string
	// Env holds extra KEY=VALUE pairs added to the inherited environment.
	Env    []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Argv returns the full argument vector.
func (c Command) Argv() []string { return append([]string{c.Name}, c.Args...) }

// String renders the command as a shell-like line (for display only).
func (c Command) String() string {
	parts := make([]string, 0, len(c.Args)+1)
	for _, p := range c.Argv() {
		parts = append(parts, shellQuote(p))
	}
	return strings.Join(parts, " ")
}

// shellQuote quotes s for display in a POSIX shell.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:=@,+%", r)) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Runner executes external commands.
type Runner interface {
	Run(ctx context.Context, c Command) error
}

// execRunner runs commands with os/exec. Cancelling the context interrupts
// the process (then kills it after 10 s).
type execRunner struct{}

func (execRunner) Run(ctx context.Context, c Command) error {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Dir = c.Dir
	if len(c.Env) > 0 {
		cmd.Env = append(os.Environ(), c.Env...)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = c.Stdin, c.Stdout, c.Stderr
	if runtime.GOOS != "windows" {
		cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	}
	cmd.WaitDelay = 10 * time.Second
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ctx.Err() == nil {
		return &ExitError{Code: ee.ExitCode(), Err: fmt.Errorf("%s failed: %w", c.Name, err)}
	}
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// lookPath finds an executable in PATH.
func lookPath(name string) (string, error) { return exec.LookPath(name) }

// browserCommand returns the command that opens raw with the platform's
// URL handler, or with $BROWSER when set (a ":"-separated list on Unix whose
// first entry is used; "%s" in it is replaced by the URL).
func browserCommand(raw string) Command {
	if env := strings.TrimSpace(os.Getenv("BROWSER")); env != "" {
		if runtime.GOOS != "windows" {
			env, _, _ = strings.Cut(env, string(os.PathListSeparator))
		}
		if fields := strings.Fields(env); len(fields) > 0 {
			args := make([]string, 0, len(fields))
			substituted := false
			for _, f := range fields[1:] {
				if strings.Contains(f, "%s") {
					f = strings.ReplaceAll(f, "%s", raw)
					substituted = true
				}
				args = append(args, f)
			}
			if !substituted {
				args = append(args, raw)
			}
			return Command{Name: fields[0], Args: args}
		}
	}
	switch runtime.GOOS {
	case "darwin":
		return Command{Name: "open", Args: []string{raw}}
	case "windows":
		return Command{Name: "rundll32", Args: []string{"url.dll,FileProtocolHandler", raw}}
	}
	name := "xdg-open"
	for _, cand := range []string{"xdg-open", "wslview", "sensible-browser"} {
		if _, err := exec.LookPath(cand); err == nil {
			name = cand
			break
		}
	}
	return Command{Name: name, Args: []string{raw}}
}

// browserGrace is how long openInBrowser waits for the launcher to report a
// failure before assuming the browser started (a variable for tests).
var browserGrace = 3 * time.Second

// openInBrowser opens an http(s) URL in the user's browser. The launcher
// runs detached: it is never killed (a browser started in the foreground,
// e.g. BROWSER=firefox without a running instance, keeps running after the
// CLI exits), but a launcher that fails within a few seconds (no browser
// available) is reported as an error so callers can print the URL instead.
func openInBrowser(ctx context.Context, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("refusing to open %q: not an http(s) URL", raw)
	}
	c := browserCommand(u.String())
	cmd := exec.Command(c.Name, c.Args...)
	// Browser chatter must not mix with the CLI's output; stdio is /dev/null.
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", c.Name, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t := time.NewTimer(browserGrace)
	defer t.Stop()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("%s: %w", c.Name, err)
		}
		return nil
	case <-t.C:
		return nil // still running: a foreground browser; leave it alone
	case <-ctx.Done():
		return nil
	}
}

// isTerminal reports whether v is an *os.File connected to a terminal.
func isTerminal(v any) bool {
	f, ok := v.(*os.File)
	if !ok || f == nil {
		return false
	}
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}
