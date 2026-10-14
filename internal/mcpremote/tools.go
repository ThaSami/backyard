package mcpremote

// tools/call (SPEC §14.1) and the account tools.

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/thasami/backyard/internal/auth"
	"github.com/thasami/backyard/internal/deploy"
)

// toolFunc runs one tool on schema-valid arguments.
type toolFunc func(ctx context.Context, c *call, args json.RawMessage) toolResult

// toolResult is a tool's text and whether it is an error.
type toolResult struct {
	text  string
	isErr bool
}

func ok(text string) toolResult                        { return toolResult{text: text} }
func failure(text string) toolResult                   { return toolResult{text: text, isErr: true} }
func publishFailure(e *deploy.PublishError) toolResult { return failure(e.Message) }

// okJSON is a success whose text is compact JSON of v.
func okJSON(c *call, v any) toolResult {
	b, err := marshal(v)
	if err != nil {
		return failure("Internal error (request " + c.reqID + ")")
	}
	return ok(string(b))
}

// callToolResult is the result of tools/call.
type callToolResult struct {
	Content []textContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// maxDetail bounds the validation detail quoted in "Invalid arguments".
const maxDetail = 500

// callTool runs tools/call.
func (h *handler) callTool(ctx context.Context, c *call, params json.RawMessage) (any, *rpcError) {
	invalid := newRPCError(codeInvalidParams, "Invalid params")
	var p map[string]json.RawMessage
	if !isJSONObject(params) || json.Unmarshal(params, &p) != nil {
		return nil, invalid
	}
	var name string
	if raw, ok := p["name"]; !ok || !isJSONString(raw) || json.Unmarshal(raw, &name) != nil {
		return nil, invalid
	}
	args, present := p["arguments"]
	switch {
	case !present:
		args = json.RawMessage("{}")
	case !isJSONObject(args):
		return nil, invalid
	}
	tool, schema := h.tools[name], h.cat.schemas[name]
	if tool == nil || schema == nil {
		return nil, newRPCError(codeInvalidParams, "Unknown tool: "+h.scrub(truncate(name, 100)))
	}
	start := h.now()
	var res toolResult
	var inst any
	if err := json.Unmarshal(args, &inst); err != nil {
		res = failure("Invalid arguments: " + err.Error())
	} else if err := schema.Validate(inst); err != nil {
		res = failure("Invalid arguments: " + truncate(err.Error(), maxDetail))
	} else {
		inst = nil
		res = tool(ctx, c, args)
	}
	h.log.DebugContext(ctx, "mcp: tool call", "tool", name, "user", c.user.Actor(), "error", res.isErr,
		"ms", h.now().Sub(start).Milliseconds(), "request_id", c.reqID)
	return callToolResult{Content: []textContent{{Type: "text", Text: h.scrub(res.text)}}, IsError: res.isErr}, nil
}

// credentialRe matches the connector's tokens, codes and registered client
// ids (SPEC §8.4 token formats).
var credentialRe = regexp.MustCompile(`by(?:at|rt|ac|dc)_[A-Za-z0-9_.-]{16,}`)

// redacted replaces credentials in tool results.
const redacted = "[REDACTED]"

// scrub keeps credentials out of a tool result: connector tokens, codes and
// client ids, and the server secret (SPEC §14.1). File text
// and site metadata are written by other people and may quote anything.
func (h *handler) scrub(s string) string {
	s = credentialRe.ReplaceAllString(s, redacted)
	if secret := h.cfg.Server.Secret; len(secret) >= 8 {
		s = strings.ReplaceAll(s, secret, redacted)
	}
	return s
}

// ---------------------------------------------------------------------------
// backyard_docs and backyard_whoami

func (h *handler) toolDocs(_ context.Context, c *call, args json.RawMessage) toolResult {
	var a struct {
		Topic string `json:"topic"`
	}
	_ = json.Unmarshal(args, &a)
	if a.Topic == "" {
		a.Topic = "skill"
	}
	text, err := h.docsFor(a.Topic)
	if err != nil || strings.TrimSpace(text) == "" {
		h.log.Error("mcp: docs", "topic", a.Topic, "error", err, "request_id", c.reqID)
		return failure("Internal error (request " + c.reqID + ")")
	}
	return ok(text)
}

// maxReadBytes is the backyard_read_site_file cap.
const maxReadBytes = 128 << 10

// siteTemplate is a valid site name whose URL becomes site_url_template.
const siteTemplate = "zz0site0template0zz"

func (h *handler) toolWhoami(_ context.Context, c *call, _ json.RawMessage) toolResult {
	u, g := c.user, c.grant
	type user struct {
		ID        string `json:"id"`
		Email     string `json:"email,omitempty"`
		Name      string `json:"name"`
		Admin     bool   `json:"admin"`
		Anonymous bool   `json:"anonymous"`
	}
	type limits struct {
		MaxPublishBytes int64 `json:"max_publish_bytes"`
		MaxFiles        int   `json:"max_files"`
		MaxFileBytes    int64 `json:"max_file_bytes"`
		MaxSiteBytes    int64 `json:"max_site_bytes"`
		MaxReadBytes    int64 `json:"max_read_bytes"`
	}
	type connection struct {
		Client    string `json:"client"`
		ExpiresAt string `json:"expires_at"`
	}
	admin := !u.Anonymous && (u.Admin || h.cfg.Auth.IsAdmin(u.ID, u.Email, u.Groups))
	return okJSON(c, struct {
		User            user       `json:"user"`
		HubURL          string     `json:"hub_url"`
		SiteURLTemplate string     `json:"site_url_template"`
		CanPublish      bool       `json:"can_publish"`
		Limits          limits     `json:"limits"`
		Connection      connection `json:"connection"`
	}{
		User:            user{u.ID, u.Email, u.Name, admin, u.Anonymous},
		HubURL:          h.hubOrigin,
		SiteURLTemplate: strings.ReplaceAll(h.cfg.SiteURL(siteTemplate), siteTemplate, "{site}"),
		CanPublish:      h.canPublish(c),
		Limits: limits{deploy.MaxPublishBytes(h.cfg), h.cfg.Sites.MaxFiles, h.cfg.Sites.MaxFileBytes,
			h.cfg.Sites.MaxSiteBytes, maxReadBytes},
		Connection: connection{g.ClientName, rfc3339(g.ExpiresAt)},
	})
}

// canPublish reports whether backyard_publish can succeed for the caller
// at all (the gateway is on; a guest needs anonymous deploys).
func (h *handler) canPublish(c *call) bool {
	if h.gw == nil {
		return false
	}
	return !c.user.Anonymous || (h.cfg.Auth.Mode == auth.ModeNone && h.cfg.Deploy.AllowAnonymous)
}

// rfc3339 formats t in UTC, or "" for the zero time.
func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
