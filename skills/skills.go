// Package skills embeds the Backyard agent skill (skills/backyard/**), the
// single source of truth for agent-facing documentation. It is installed into
// projects by `backyard init` / `backyard skill install`, served as /llms.txt,
// and returned by the MCP tool backyard_docs.
package skills

import "io/fs"

// STUBS: replaced by the skills implementation.

// FS returns the skill tree rooted so that "backyard/SKILL.md" exists.
func FS() fs.FS { panic("skills: not implemented") }

// SkillMarkdown returns backyard/SKILL.md.
func SkillMarkdown() string { panic("skills: not implemented") }

// Reference returns backyard/references/<name>.md ("" if missing).
func Reference(name string) string { panic("skills: not implemented") }

// LLMSTxt returns the /llms.txt document (concise; links to /llms-full.txt).
func LLMSTxt(publicURL string) string { panic("skills: not implemented") }

// LLMSFullTxt returns SKILL.md followed by every reference document.
func LLMSFullTxt(publicURL string) string { panic("skills: not implemented") }
