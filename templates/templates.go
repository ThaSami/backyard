// Package templates renders the files written by `backyard init` (and the
// MCP tool backyard_init): the starter page(s), .backyard.json,
// .backyardignore, and the instructions for coding agents — CLAUDE.md plus
// the embedded Backyard skill for Claude Code, AGENTS.md for Codex and other
// AGENTS.md-aware agents, a Cursor project rule and GitHub Copilot
// repository instructions.
//
// Template sources live in templates/files and are embedded in the binary.
// Files ending in .tmpl are Go text/templates; the API cheat sheet, decision
// table and limits in the agent files are extracted from the skill's
// SKILL.md so they never drift from it.
package templates

// InitOptions configure Render.
type InitOptions struct {
	Site      string   // site name (validated by caller)
	Server    string   // hub URL, may be ""
	Agents    []string // any of: claude, codex, cursor, copilot (default: claude, codex)
	Template  string   // blank (default) | app
	SkipIndex bool     // do not write index.html (existing project)
	// BaseDomain and URLMode describe the server (its /info base_domain
	// and url_mode) when known: host-mode sites live at
	// <scheme>://<site>.<base_domain>[:port]/, and base_domain may differ
	// from the hub's host. Empty: guessed from Server.
	BaseDomain string
	URLMode    string // host | path | ""
}

// Render returns the files to create, keyed by "/"-separated relative path.
// It always includes .backyard.json; index.html unless SkipIndex; CLAUDE.md
// and .claude/skills/backyard/** for "claude"; AGENTS.md for "codex";
// .cursor/rules/backyard.mdc for "cursor"; .github/copilot-instructions.md for
// "copilot". Callers must not overwrite existing files without consent.
//
// It also always includes a default .backyardignore. With the "app"
// template the page is index.html, app.js and style.css; SkipIndex omits all
// three. Invalid options (an unknown agent or template, a malformed site
// name or server URL) are reported as errors.
func Render(opts InitOptions) (map[string][]byte, error) { return render(opts) }

// Templates returns the available template names.
func Templates() []string { return []string{TemplateBlank, TemplateApp} }
