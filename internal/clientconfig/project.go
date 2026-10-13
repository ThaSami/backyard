package clientconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ProjectFileName is the per-project settings file.
const ProjectFileName = ".backyard.json"

// maxProjectBytes bounds .backyard.json.
const maxProjectBytes = 1 << 20

// Project is the content of .backyard.json.
type Project struct {
	// Site is the site name to deploy to.
	Site string `json:"site,omitempty"`
	// Dir is the folder to deploy, relative to the project file (default ".").
	Dir string `json:"dir,omitempty"`
	// Method is the preferred deploy method (default auto).
	Method string `json:"method,omitempty"`
	// Ignore lists extra gitignore-style patterns excluded from deploys.
	Ignore []string `json:"ignore,omitempty"`
}

// ProjectFile is a project file found on disk.
type ProjectFile struct {
	Project
	// Path is the absolute path of the .backyard.json file.
	Path string
}

// Root returns the directory containing the project file.
func (p *ProjectFile) Root() string { return filepath.Dir(p.Path) }

// DeployDir returns the absolute folder to deploy (Root joined with Dir).
func (p *ProjectFile) DeployDir() string {
	d := strings.TrimSpace(p.Dir)
	if d == "" {
		return p.Root()
	}
	if filepath.IsAbs(d) {
		return filepath.Clean(d)
	}
	return filepath.Join(p.Root(), filepath.FromSlash(d))
}

// FindProject searches start and its parents for .backyard.json. It returns
// (nil, nil) when none is found. Unreadable directories are skipped.
func FindProject(start string) (*ProjectFile, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return nil, fmt.Errorf("clientconfig: %w", err)
	}
	if st, err := os.Stat(dir); err == nil && !st.IsDir() {
		dir = filepath.Dir(dir)
	}
	for {
		p := filepath.Join(dir, ProjectFileName)
		st, err := os.Stat(p)
		switch {
		case err == nil && st.Mode().IsRegular():
			return LoadProject(p)
		case err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, fs.ErrPermission):
			return nil, fmt.Errorf("clientconfig: %w", err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, nil
		}
		dir = parent
	}
}

// LoadProject reads a .backyard.json file.
func LoadProject(path string) (*ProjectFile, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("clientconfig: %w", err)
	}
	b, err := readLimited(abs, maxProjectBytes)
	if err != nil {
		return nil, fmt.Errorf("clientconfig: read %s: %w", abs, err)
	}
	pf := &ProjectFile{Path: abs}
	if len(bytes.TrimSpace(b)) > 0 {
		if err := json.Unmarshal(b, &pf.Project); err != nil {
			return nil, fmt.Errorf("clientconfig: parse %s: %w", abs, err)
		}
	}
	if !ValidMethod(pf.Method) {
		return nil, fmt.Errorf("clientconfig: %s: unknown deploy method %q", abs, pf.Method)
	}
	return pf, nil
}

// SaveProject writes p to dir/.backyard.json (mode 0644).
func SaveProject(dir string, p Project) error {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("clientconfig: encode project: %w", err)
	}
	b = append(b, '\n')
	path := filepath.Join(dir, ProjectFileName)
	if err := writeFileAtomic(path, b, 0o644); err != nil {
		return err
	}
	return nil
}

// SiteNameFromDir derives a valid site name from a directory's base name:
// lowercased, runs of characters outside [a-z0-9] replaced by "-", trimmed
// to 63 characters. It returns "" when nothing usable remains.
func SiteNameFromDir(dir string) string {
	abs, err := filepath.Abs(dir)
	if err == nil {
		dir = abs
	}
	return SanitizeSiteName(filepath.Base(dir))
}

// SanitizeSiteName turns an arbitrary string into a DNS-label site name, or
// "" when nothing usable remains.
func SanitizeSiteName(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 63 {
		out = strings.TrimRight(out[:63], "-")
	}
	return out
}
