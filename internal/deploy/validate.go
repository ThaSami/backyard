package deploy

import (
	"strings"
	"unicode/utf8"

	"github.com/thasami/backyard/internal/blob"
	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/sites"
)

const (
	// MaxPathBytes is the longest site-relative path accepted (SPEC §7.3).
	MaxPathBytes = 1024
	// ReservedPathPrefix can never be deployed: it is the platform namespace.
	ReservedPathPrefix = "__backyard/"
	maxContentTypeLen  = 255
	maxMessageBytes    = 4096
)

// ValidatePath reports why a site-relative path is unacceptable for a
// deploy or a gateway read, or "" when it is valid: ValidatePathSyntax, and
// no segment may be ".git" in any letter case (HasGitSegment).
//
// The ".git" rule is defense in depth: deploys never publish repository
// metadata (the CLI's ignore rules cannot re-include .git/, see
// ProtectedIgnore), so a client sending such a path is broken or hostile,
// and a gateway never serves one back to a "backyard pull" (a crafted
// .git/config runs commands on the next git command in the pulled folder).
// Case-insensitive file systems (macOS, Windows) resolve ".GIT" to ".git".
// The Cloudflare edition's validatePath applies the same rule.
func ValidatePath(p string) string {
	if why := ValidatePathSyntax(p); why != "" {
		return why
	}
	if HasGitSegment(p) {
		return `path may not contain a ".git" segment`
	}
	return ""
}

// ValidatePathSyntax reports why p is not a well-formed site-relative path,
// or "" when it is: relative, "/"-separated, no "." / ".." / empty
// segments, at most MaxPathBytes, valid UTF-8 without control characters or
// backslashes, and not under "__backyard/". It does not apply ValidatePath's
// ".git" rule, so clients use it for paths a site may already hold (stored
// before the rule, or by a direct sync) that they only need to name safely,
// e.g. to skip or delete them.
func ValidatePathSyntax(p string) string {
	switch {
	case p == "":
		return "empty path"
	case len(p) > MaxPathBytes:
		return "path longer than 1024 bytes"
	case !utf8.ValidString(p):
		return "path is not valid UTF-8"
	case p[0] == '/':
		return "path must be relative (no leading /)"
	case strings.ContainsRune(p, '\\'):
		return `path may not contain "\"`
	case p == strings.TrimSuffix(ReservedPathPrefix, "/") || strings.HasPrefix(p, ReservedPathPrefix):
		return `path may not start with "__backyard/"`
	}
	for i := 0; i < len(p); i++ {
		if c := p[i]; c < 0x20 || c == 0x7f {
			return "path contains a control character"
		}
	}
	for seg := range strings.SplitSeq(p, "/") {
		switch seg {
		case "":
			return "path contains an empty segment"
		case ".", "..":
			return `path may not contain "." or ".." segments`
		}
	}
	return ""
}

// HasGitSegment reports whether a "/"-separated segment of p is ".git" in
// any letter case.
func HasGitSegment(p string) bool {
	for seg := range strings.SplitSeq(p, "/") {
		if strings.EqualFold(seg, ".git") {
			return true
		}
	}
	return false
}

// validSitePath validates p and that the resulting storage key is usable.
func validSitePath(site, p string) *apiError {
	if why := ValidatePath(p); why != "" {
		return badRequest("invalid path %q: %s", truncate(p, 200), why)
	}
	if !blob.ValidKey(sites.SiteKey(site, p)) {
		return badRequest("invalid path %q: too long for storage", truncate(p, 200))
	}
	return nil
}

// isSHA256Hex reports whether s is 64 hex digits.
func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
			return false
		}
	}
	return true
}

// validContentType accepts short header-safe values.
func validContentType(ct string) bool {
	if len(ct) > maxContentTypeLen {
		return false
	}
	for i := 0; i < len(ct); i++ {
		if c := ct[i]; c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

// validateFiles checks a deploy file list against the path rules and the
// configured limits. It returns a normalized copy (lowercase sha256, content
// type filled in) and the total byte count.
func validateFiles(site string, files []FileEntry, lim config.SitesConfig) ([]FileEntry, int64, *apiError) {
	if len(files) == 0 {
		return nil, 0, badRequest("no files to deploy (to remove a site, delete it)")
	}
	if lim.MaxFiles > 0 && len(files) > lim.MaxFiles {
		return nil, 0, tooLarge("%d files exceed the limit of %d files per site", len(files), lim.MaxFiles)
	}
	out := make([]FileEntry, len(files))
	seen := make(map[string]bool, len(files))
	var total int64
	for i, f := range files {
		if err := validSitePath(site, f.Path); err != nil {
			return nil, 0, err
		}
		if seen[f.Path] {
			return nil, 0, badRequest("duplicate path %q", truncate(f.Path, 200))
		}
		seen[f.Path] = true
		if f.Size < 0 {
			return nil, 0, badRequest("invalid size for %q", truncate(f.Path, 200))
		}
		if lim.MaxFileBytes > 0 && f.Size > lim.MaxFileBytes {
			return nil, 0, tooLarge("%q is %d bytes; the limit is %d bytes per file", truncate(f.Path, 200), f.Size, lim.MaxFileBytes)
		}
		if !isSHA256Hex(f.SHA256) {
			return nil, 0, badRequest("invalid sha256 for %q (want 64 hex digits)", truncate(f.Path, 200))
		}
		if !validContentType(f.ContentType) {
			return nil, 0, badRequest("invalid content_type for %q", truncate(f.Path, 200))
		}
		total += f.Size
		if lim.MaxSiteBytes > 0 && total > lim.MaxSiteBytes {
			return nil, 0, tooLarge("site exceeds the limit of %d bytes", lim.MaxSiteBytes)
		}
		f.SHA256 = strings.ToLower(f.SHA256)
		f.ContentType = strings.TrimSpace(f.ContentType)
		if f.ContentType == "" {
			f.ContentType = blob.DetectContentType(f.Path, nil)
		}
		out[i] = f
	}
	// A path cannot be both a file and a directory (fs storage, rsync).
	for _, f := range out {
		for i := strings.LastIndexByte(f.Path, '/'); i > 0; i = strings.LastIndexByte(f.Path[:i], '/') {
			if seen[f.Path[:i]] {
				return nil, 0, badRequest("path %q conflicts with file %q", truncate(f.Path, 200), truncate(f.Path[:i], 200))
			}
		}
	}
	return out, total, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
