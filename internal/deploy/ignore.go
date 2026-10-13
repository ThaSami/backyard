package deploy

import (
	"path"
	"strings"
)

// IgnoreFile is the per-directory ignore file read by CollectFiles.
const IgnoreFile = ".backyardignore"

// DefaultIgnore returns the built-in ignore patterns (SPEC §13): VCS and
// tooling directories, agent instruction files, and every path segment
// starting with "." except ".well-known/".
func DefaultIgnore() []string {
	return []string{
		".git/",
		".backyard.json",
		IgnoreFile,
		"node_modules/",
		".claude/",
		".cursor/",
		".github/",
		"CLAUDE.md",
		"AGENTS.md",
		".DS_Store",
		".*",
		"!.well-known/",
	}
}

// Ignored reports whether a site-relative path is excluded by patterns (used
// by direct-sync methods to build tool-specific exclude flags).
func Ignored(rel string, isDir bool, patterns []string) bool {
	return NewMatcher(patterns).Ignored(rel, isDir)
}

// Matcher evaluates gitignore-style patterns against site-relative paths.
//
// Supported syntax: blank lines and "#" comments, "!" negation (the last
// matching pattern wins), a trailing "/" for directory-only patterns, a
// leading or inner "/" to anchor a pattern at the root (otherwise it matches
// a name at any depth), "**" for any number of directories ("**/x", "x/**",
// "a/**/b"), and path.Match globs ("*", "?", "[…]", "\" escapes) within a
// segment. As in git, a path inside an excluded directory cannot be
// re-included. Invalid globs are ignored.
type Matcher struct {
	rules []ignoreRule
}

type ignoreRule struct {
	negate  bool
	dirOnly bool
	segs    []string
}

// NewMatcher compiles patterns (one gitignore line each).
func NewMatcher(patterns []string) *Matcher {
	m := &Matcher{}
	for _, p := range patterns {
		if r, ok := parseIgnoreLine(p); ok {
			m.rules = append(m.rules, r)
		}
	}
	return m
}

// parseIgnoreLine compiles one gitignore line.
func parseIgnoreLine(line string) (ignoreRule, bool) {
	line = strings.TrimSuffix(line, "\r")
	// Trailing spaces are dropped unless escaped with a backslash.
	for strings.HasSuffix(line, " ") && !strings.HasSuffix(line, `\ `) {
		line = line[:len(line)-1]
	}
	if line == "" || line[0] == '#' {
		return ignoreRule{}, false
	}
	var r ignoreRule
	if line[0] == '!' {
		r.negate = true
		line = line[1:]
	}
	if strings.HasSuffix(line, "/") {
		r.dirOnly = true
		line = strings.TrimRight(line, "/")
	}
	if line == "" {
		return ignoreRule{}, false
	}
	anchored := strings.Contains(line, "/")
	for _, s := range strings.Split(strings.TrimPrefix(line, "/"), "/") {
		if s == "" {
			continue // "a//b" == "a/b"
		}
		if s != "**" && strings.Contains(s, "**") {
			s = collapseStars(s) // "a**b" behaves like "a*b"
		}
		s = translateClassNegation(s)
		if _, err := path.Match(s, ""); err != nil {
			return ignoreRule{}, false
		}
		r.segs = append(r.segs, s)
	}
	if len(r.segs) == 0 {
		return ignoreRule{}, false
	}
	if !anchored {
		r.segs = append([]string{"**"}, r.segs...)
	}
	return r, true
}

// translateClassNegation rewrites gitignore/fnmatch "[!…]" classes to the
// "[^…]" form path.Match understands.
func translateClassNegation(s string) string {
	if !strings.Contains(s, "[!") {
		return s
	}
	var b strings.Builder
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case escaped:
			escaped = false
		case c == '\\':
			escaped = true
		case c == '[' && i+1 < len(s) && s[i+1] == '!':
			b.WriteString("[^")
			i++
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func collapseStars(s string) string {
	for strings.Contains(s, "**") {
		s = strings.ReplaceAll(s, "**", "*")
	}
	return s
}

// Ignored reports whether rel ("/"-separated, relative to the site root) is
// excluded, either itself or through an excluded parent directory.
func (m *Matcher) Ignored(rel string, isDir bool) bool {
	rel = strings.Trim(rel, "/")
	if rel == "" || len(m.rules) == 0 {
		return false
	}
	segs := strings.Split(rel, "/")
	for i := 1; i < len(segs); i++ {
		if m.match(segs[:i], true) {
			return true
		}
	}
	return m.match(segs, isDir)
}

// match evaluates the rules against one path (parents not considered).
func (m *Matcher) match(segs []string, isDir bool) bool {
	ignored := false
	for _, r := range m.rules {
		if r.dirOnly && !isDir {
			continue
		}
		if r.negate != ignored {
			continue // cannot change the outcome
		}
		if matchSegments(r.segs, segs) {
			ignored = !r.negate
		}
	}
	return ignored
}

// matchSegments matches glob segments against path segments. "**" matches
// zero or more segments, except as the final pattern segment where it
// matches one or more ("dir/**" matches what is inside dir, not dir).
func matchSegments(pat, segs []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			if len(pat) == 1 {
				return len(segs) > 0
			}
			for i := 0; i <= len(segs); i++ {
				if matchSegments(pat[1:], segs[i:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 {
			return false
		}
		if ok, _ := path.Match(pat[0], segs[0]); !ok {
			return false
		}
		pat, segs = pat[1:], segs[1:]
	}
	return len(segs) == 0
}
