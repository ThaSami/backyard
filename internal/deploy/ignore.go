package deploy

import (
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

// IgnoreFile is the per-directory ignore file read by CollectFiles.
const IgnoreFile = ".backyardignore"

// Built-in ignore rules (SPEC §13) come in two tiers.
//
// ProtectedIgnore covers what a deploy must never publish, whatever the
// configuration: repository metadata (".git" at any depth: the directory,
// or the file a submodule or worktree has in its place) and Backyard's
// own project files (".backyard.json", ".backyardignore"). These patterns
// match in any letter case, as case-insensitive file systems (macOS,
// Windows) resolve them, and as the gateways reject any ".git" segment
// (ValidatePath). WithDefaultIgnore places them after every configured
// pattern, so no "!" line can re-include them: the last matching pattern
// wins, and a path inside an excluded directory cannot be re-included.
//
// The other defaults (overridableIgnore) are conveniences that a project
// may override explicitly with "!" lines in .backyardignore or the
// .backyard.json "ignore" list: node_modules/, agent instructions and
// settings (.claude/, .cursor/, CLAUDE.md, AGENTS.md), .github/, .DS_Store
// and every other path segment starting with "." except ".well-known/".
// For example "!.htaccess" publishes .htaccess files and "!.*" publishes
// every dotfile, while "!.git/" or "!.backyard.json" change nothing.

// overridableIgnore returns the default patterns "!" lines may override.
func overridableIgnore() []string {
	return []string{
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

// ProtectedIgnore returns the default patterns no configuration can
// override: ".git" (directory or file) and the project files, as
// case-insensitive globs (".[gG][iI][tT]" …).
func ProtectedIgnore() []string {
	return []string{
		anyCaseGlob(".git"),
		anyCaseGlob(".backyard.json"),
		anyCaseGlob(IgnoreFile),
	}
}

// DefaultIgnore returns the built-in ignore patterns alone: the overridable
// defaults followed by ProtectedIgnore. It is what a folder without any
// ignore configuration gets (WithDefaultIgnore(nil)).
func DefaultIgnore() []string {
	return WithDefaultIgnore(nil)
}

// WithDefaultIgnore returns the complete, ordered pattern list for the
// configured patterns: the overridable defaults, then patterns (which may
// re-include some of them with "!" lines), then ProtectedIgnore (which
// nothing can re-include). Every consumer — CollectFiles, the direct-sync
// exclude translation, the prune of ignored site files — evaluates this
// list as one gitignore sequence.
func WithDefaultIgnore(patterns []string) []string {
	out := overridableIgnore()
	out = append(out, patterns...)
	return append(out, ProtectedIgnore()...)
}

// anyCaseGlob returns a glob matching the literal name s in any ASCII letter
// case: each letter becomes a "[xX]" class; s must hold no glob
// metacharacters.
func anyCaseGlob(s string) string {
	var b strings.Builder
	for _, r := range s {
		lo, up := unicode.ToLower(r), unicode.ToUpper(r)
		if r < utf8.RuneSelf && lo != up {
			b.WriteString("[" + string(lo) + string(up) + "]")
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
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
