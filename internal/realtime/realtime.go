// Package realtime implements the per-site websocket hub described in
// docs/spec/SPEC.md §6: database change subscriptions, rooms with
// broadcast/direct messages and presence, fanned out across instances through
// a pubsub.Broker.
package realtime

import (
	"context"
	"net/http"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/docstore"
	"github.com/thasami/backyard/internal/identity"
)

// Op is a database change operation.
type Op string

// Database change operations.
const (
	OpCreate Op = "create"
	OpUpdate Op = "update"
	OpDelete Op = "delete"
)

// DBEvent describes a committed document change.
type DBEvent struct {
	Collection string
	Op         Op
	DocID      string
	Doc        *docstore.Doc // nil for deletes
}

// Hub accepts websocket connections and routes realtime traffic.
// The implementation lives in this package (hub.go etc.).
type Hub interface {
	// ServeWS upgrades the request and serves the connection until it closes.
	// The caller has already authenticated user and resolved site; ServeWS
	// performs the Origin check (allowedOrigins: exact origins such as
	// "https://foo.backyard.acme.com"; empty = same-origin only).
	ServeWS(w http.ResponseWriter, r *http.Request, site string, user *identity.User, allowedOrigins []string)
	// PublishDB fans out a database change to subscribers on all instances.
	PublishDB(ctx context.Context, site string, ev DBEvent) error
	// Stats returns current local connection counts (for metrics).
	Stats() Stats
	// Close disconnects all clients and unsubscribes from the broker.
	Close() error
}

// Stats are local (this instance) counters.
type Stats struct {
	Connections int
	Rooms       int
	Subs        int
}

// Options tune the hub; zero values come from config.RealtimeConfig.
type Options struct {
	Config config.RealtimeConfig
	// InstanceID distinguishes this server instance in presence gossip
	// (default: random).
	InstanceID string
}
