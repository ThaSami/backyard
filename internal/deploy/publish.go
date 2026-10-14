package deploy

// Publishing from memory for the remote MCP connector (SPEC §14.1,
// mcp-design.md §6.3): the files arrive inside a tool call instead of through
// the create/upload/commit protocol of §7.
//
// PINNED API (mcp-design.md §11): the declarations below are the contract
// with internal/mcpremote. The body of Publish is a placeholder until the
// publish core lands.

import (
	"context"
	"time"

	"github.com/thasami/backyard/internal/identity"
)

// PublishFile is one file of a publish call, already decoded.
type PublishFile struct {
	Path        string
	Data        []byte
	ContentType string
}

// PublishRequest is one backyard_publish call.
type PublishRequest struct {
	User      *identity.User
	Site      string
	Files     []PublishFile
	Delete    []string
	Replace   bool
	Overwrite bool
	// ClientIP, ClientName and GrantID go into the deploy.commit audit record.
	ClientIP, ClientName, GrantID string
}

// PublishResult describes the site after a publish.
type PublishResult struct {
	Site, URL                           string
	Created                             bool
	Files, Uploaded, Unchanged, Deleted int
	Bytes                               int64
	Warnings                            []string
}

// PublishError is a refusal or failure with its catalog id ("E2" … "E27",
// mcp-design.md §6.4) and the tool error text.
type PublishError struct {
	ID, Message string
	RetryAfter  time.Duration
}

func (e *PublishError) Error() string { return e.Message }

// Publish writes req to the site: by default the listed files are added or
// replaced and the others kept; Delete removes paths; Replace makes the site
// exactly Files.
func (g *Gateway) Publish(ctx context.Context, req PublishRequest) (*PublishResult, *PublishError) {
	return nil, &PublishError{ID: "E21", Message: "Publishing failed: not implemented."}
}
