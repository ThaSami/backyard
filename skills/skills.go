// Package skills embeds the Backyard agent skill (skills/backyard/**), the
// single source of truth for agent-facing documentation. It is installed into
// projects by `backyard init` / `backyard skill install`, served as /llms.txt,
// and returned by the MCP tool backyard_docs.
//
// The tree is a Claude Code Agent Skill: backyard/SKILL.md (YAML front
// matter + overview) and backyard/references/*.md (API reference, recipes,
// deploy guide) loaded on demand. The documents use HubURLPlaceholder
// wherever they mean "the hub URL"; LLMSFullTxt replaces it with the
// server's public URL.
package skills

import (
	"embed"
	"io/fs"
	"strings"
	"sync"
)

// content holds skills/backyard/** ("all:" keeps dotfiles, if any are added).
//
//go:embed all:backyard
var content embed.FS

// FS returns the skill tree rooted so that "backyard/SKILL.md" exists.
func FS() fs.FS { return content }

// SkillMarkdown returns backyard/SKILL.md.
func SkillMarkdown() string { return readDoc(skillPath) }

// Reference returns backyard/references/<name>.md ("" if missing). The name
// is matched case-insensitively and may carry a ".md" suffix; anything that
// is not a plain document name (paths, "..") yields "".
func Reference(name string) string {
	name, ok := normalizeRefName(name)
	if !ok {
		return ""
	}
	return readDoc(referencesDir + "/" + name + ".md")
}

// LLMSTxt returns the /llms.txt document (concise; links to /llms-full.txt).
// publicURL is the hub URL (e.g. "https://backyard.acme.com"); when empty,
// links are server-relative.
func LLMSTxt(publicURL string) string { return buildLLMSTxt(publicURL) }

// LLMSFullTxt returns SKILL.md followed by every reference document.
// HubURLPlaceholder is replaced with publicURL (when non-empty) and links
// between the documents point at <publicURL>/docs/<name>.md. Results are
// cached per public URL (the documents never change at run time).
func LLMSFullTxt(publicURL string) string {
	base := baseURL(publicURL)
	fullCache.Lock()
	doc, ok := fullCache.m[base]
	fullCache.Unlock()
	if ok {
		return doc
	}
	doc = buildLLMSFullTxt(base)
	fullCache.Lock()
	defer fullCache.Unlock()
	if len(fullCache.m) >= maxFullCache {
		clear(fullCache.m) // a server has one public URL; this only bounds misuse
	}
	fullCache.m[base] = doc
	return doc
}

// maxFullCache bounds the number of cached /llms-full.txt renderings.
const maxFullCache = 8

// fullCache memoizes LLMSFullTxt by normalized public URL.
var fullCache = struct {
	sync.Mutex
	m map[string]string
}{m: map[string]string{}}

// readDoc reads an embedded document, returning "" when it does not exist.
func readDoc(p string) string {
	b, err := fs.ReadFile(content, p)
	if err != nil {
		return ""
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}
