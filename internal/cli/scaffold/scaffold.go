// Package scaffold writes the files of a new Backyard project (`backyard
// init` and the MCP tool backyard_init) and provides SafeJoin, the path
// guard used whenever the CLI writes files whose names come from elsewhere
// (templates, `backyard pull`).
package scaffold

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/sites"
	"github.com/thasami/backyard/templates"
)

// Agents are the agent integrations `backyard init --agents` accepts.
var Agents = []string{"claude", "codex", "cursor", "copilot"}

// Renderer renders the files of a new project (templates.Render).
type Renderer func(templates.InitOptions) (map[string][]byte, error)

// Options configure Init.
type Options struct {
	// Dir is the project directory (created when missing).
	Dir string
	// Site is the site name (validated against config.DefaultReserved).
	Site string
	// Server is the hub URL recorded in generated docs ("" when unknown).
	Server string
	// Agents selects agent files (default: the templates' default).
	Agents []string
	// Template is the template name (default: the templates' default).
	Template string
	// Force overwrites existing files.
	Force bool
	// Render renders the files (default templates.Render).
	Render Renderer
}

// Result reports what Init or Write did. Paths are "/"-separated and
// relative to Dir.
type Result struct {
	Dir         string   `json:"dir"`
	Site        string   `json:"site,omitempty"`
	Created     []string `json:"created"`
	Overwritten []string `json:"overwritten"`
	Skipped     []string `json:"skipped"`
}

// ParseAgents splits a comma-separated agent list, validating each name.
func ParseAgents(list []string) ([]string, error) {
	var out []string
	for _, item := range list {
		for a := range strings.SplitSeq(item, ",") {
			a = strings.ToLower(strings.TrimSpace(a))
			if a == "" {
				continue
			}
			if !slices.Contains(Agents, a) {
				return nil, fmt.Errorf("unknown agent %q (choose from %s)", a, strings.Join(Agents, ", "))
			}
			if !slices.Contains(out, a) {
				out = append(out, a)
			}
		}
	}
	return out, nil
}

// Init validates the site name, renders the project files and writes them
// to o.Dir without overwriting existing files unless o.Force.
func Init(o Options) (*Result, error) {
	if err := sites.ValidateName(o.Site, config.DefaultReserved); err != nil {
		return nil, err
	}
	agents, err := ParseAgents(o.Agents)
	if err != nil {
		return nil, err
	}
	render := o.Render
	if render == nil {
		render = templates.Render
	}
	dir, err := filepath.Abs(o.Dir)
	if err != nil {
		return nil, fmt.Errorf("scaffold: %w", err)
	}
	if st, err := os.Stat(dir); err == nil && !st.IsDir() {
		return nil, fmt.Errorf("scaffold: %s exists and is not a directory", dir)
	}
	skipIndex := false
	if !o.Force {
		if _, err := os.Lstat(filepath.Join(dir, "index.html")); err == nil {
			skipIndex = true // existing project: keep its page
		}
	}
	files, err := render(templates.InitOptions{
		Site: o.Site, Server: o.Server, Agents: agents, Template: o.Template, SkipIndex: skipIndex,
	})
	if err != nil {
		return nil, fmt.Errorf("scaffold: render templates: %w", err)
	}
	res, err := Write(dir, files, o.Force)
	if res != nil {
		res.Site = o.Site
		if skipIndex && !slices.Contains(res.Skipped, "index.html") {
			res.Skipped = append(res.Skipped, "index.html")
			slices.Sort(res.Skipped)
		}
	}
	return res, err
}

// Write writes files (keyed by "/"-separated relative path) under dir,
// creating directories as needed. Existing files are skipped unless force.
// Paths escaping dir, or crossing a symlink inside it, are rejected before
// anything is written.
func Write(dir string, files map[string][]byte, force bool) (*Result, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("scaffold: %w", err)
	}
	names := make([]string, 0, len(files))
	targets := make(map[string]string, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("scaffold: create %s: %w", root, err)
	}
	for _, name := range names {
		target, err := SafeJoin(root, name)
		if err != nil {
			return nil, fmt.Errorf("scaffold: %w", err)
		}
		targets[name] = target
	}
	res := &Result{Dir: root, Created: []string{}, Overwritten: []string{}, Skipped: []string{}}
	for _, name := range names {
		target := targets[name]
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return res, fmt.Errorf("scaffold: create directory for %s: %w", name, err)
		}
		if !force {
			created, err := createExclusive(target, files[name])
			if err != nil {
				return res, fmt.Errorf("scaffold: write %s: %w", name, err)
			}
			if created {
				res.Created = append(res.Created, name)
			} else {
				res.Skipped = append(res.Skipped, name)
			}
			continue
		}
		_, statErr := os.Lstat(target)
		if err := WriteFileAtomic(target, files[name], 0o644); err != nil {
			return res, fmt.Errorf("scaffold: write %s: %w", name, err)
		}
		if statErr == nil {
			res.Overwritten = append(res.Overwritten, name)
		} else {
			res.Created = append(res.Created, name)
		}
	}
	return res, nil
}

// createExclusive creates path with data unless it already exists.
func createExclusive(path string, data []byte) (bool, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, fs.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(path)
		return false, err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return false, err
	}
	return true, nil
}

// WriteFileAtomic writes data to a temporary file next to path and renames
// it into place (replacing, never following, an existing symlink).
func WriteFileAtomic(path string, data []byte, perm fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".backyard-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, perm); err != nil && runtime.GOOS != "windows" {
		return err
	}
	return os.Rename(name, path)
}

// SafeJoin joins a "/"-separated relative path onto root, rejecting
// absolute paths, ".." segments, backslashes, drive letters, and paths whose
// existing parent directories inside root are symlinks or files (which
// could redirect writes outside root). An existing symlink as the final
// element is rejected as well.
func SafeJoin(root, rel string) (string, error) {
	switch {
	case rel == "":
		return "", errors.New("empty path")
	case strings.HasPrefix(rel, "/") || filepath.IsAbs(rel):
		return "", fmt.Errorf("refusing absolute path %q", rel)
	case strings.ContainsRune(rel, '\\'):
		return "", fmt.Errorf(`refusing path %q with "\"`, rel)
	case strings.ContainsRune(rel, 0):
		return "", fmt.Errorf("refusing path %q with NUL", rel)
	case runtime.GOOS == "windows" && strings.ContainsRune(rel, ':'):
		return "", fmt.Errorf("refusing path %q with ':'", rel)
	}
	for seg := range strings.SplitSeq(rel, "/") {
		if seg == ".." {
			return "", fmt.Errorf("refusing path %q outside the target directory", rel)
		}
	}
	clean := path.Clean(rel)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("refusing path %q outside the target directory", rel)
	}
	full := filepath.Join(root, filepath.FromSlash(clean))
	r, err := filepath.Rel(root, full)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) || filepath.IsAbs(r) {
		return "", fmt.Errorf("refusing path %q outside the target directory", rel)
	}
	cur := root
	segs := strings.Split(clean, "/")
	for i, seg := range segs {
		cur = filepath.Join(cur, seg)
		st, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("check %q: %w", rel, err)
		}
		if st.Mode()&fs.ModeSymlink != 0 {
			return "", fmt.Errorf("refusing to write %q through the symlink %s", rel, cur)
		}
		if i < len(segs)-1 && !st.IsDir() {
			return "", fmt.Errorf("cannot write %q: %s is not a directory", rel, cur)
		}
		if i == len(segs)-1 && st.IsDir() {
			return "", fmt.Errorf("cannot write %q: it is a directory", rel)
		}
	}
	return full, nil
}
