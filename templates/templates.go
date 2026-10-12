// Package templates renders the files written by `backyard init`.
package templates

// STUBS: replaced by the templates implementation.

// InitOptions configure Render.
type InitOptions struct {
	Site      string   // site name (validated by caller)
	Server    string   // hub URL, may be ""
	Agents    []string // any of: claude, codex, cursor, copilot (default: claude, codex)
	Template  string   // blank (default) | app
	SkipIndex bool     // do not write index.html (existing project)
}

// Render returns the files to create, keyed by "/"-separated relative path.
// It always includes .backyard.json; index.html unless SkipIndex; CLAUDE.md
// and .claude/skills/backyard/** for "claude"; AGENTS.md for "codex";
// .cursor/rules/backyard.mdc for "cursor"; .github/copilot-instructions.md for
// "copilot". Callers must not overwrite existing files without consent.
func Render(opts InitOptions) (map[string][]byte, error) { panic("templates: not implemented") }

// Templates returns the available template names.
func Templates() []string { panic("templates: not implemented") }
