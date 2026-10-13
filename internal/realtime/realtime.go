// Package realtime implements the per-site websocket hub described in
// docs/spec/SPEC.md §6: database change subscriptions, rooms with
// broadcast/direct messages and presence, fanned out across instances through
// a pubsub.Broker.
//
// # Protocol
//
// Each connection receives {"t":"hello","cid","user"} first, then JSON text
// frames as specified in SPEC §6 (sub/unsub → ack; join → joined; leave;
// state; send; ping → pong; server events db, presence, msg, error). Errors
// never close the connection: malformed or invalid frames are answered with
// {"t":"error","id"?,"error":{"code","message"}} using the SPEC §5 codes
// (bad_request, forbidden, not_found, too_large, rate_limited, internal).
// As a compatible extension, leave/state/send frames that carry an "id" are
// acknowledged with {"t":"ack","id"} on success.
//
// Limits (config.RealtimeConfig): frame size (larger frames get too_large; far
// larger ones close the connection with status 1009), a per-connection token
// bucket, rooms and subscriptions per connection, presence state size and
// connections per user (refused with HTTP 429 before the upgrade). The server
// pings every 25 s and drops connections that do not answer within 60 s;
// clients that cannot keep up with their outbound queue are disconnected
// (status 1008).
//
// # Multiple instances
//
// Every instance subscribes, through one reference-counted broker
// subscription per topic, to "rt.<site>.db" while a local client watches the
// site's collections and to "rt.<site>.room.<room>" while a local client is
// in the room. PublishDB publishes to the db topic and each instance delivers
// to its own subscribers. Room messages travel through the room topic and are
// delivered by every instance (including the sender's) to its local members.
// Presence is applied locally right away and gossiped: join (the first local
// member also asks the other instances to answer with their members), update,
// leave, and a full "members" snapshot per room every HeartbeatInterval
// (split into parts when it would not fit in one broker message). Members of
// other instances that are not refreshed within MemberTTL expire.
// Gossip carries per-instance sequence numbers so that a delayed snapshot
// can never resurrect a departed member or revert a newer state.
package realtime

import (
	"context"
	"log/slog"
	"net/http"
	"time"

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
	// (default: random). It is used as a readable label: a random suffix is
	// always appended, so instances given the same value (e.g. a hostname
	// that is not unique) still tell each other apart.
	InstanceID string

	// Logger receives operational logs (default slog.Default()).
	Logger *slog.Logger
	// PingInterval is how often the server pings every connection
	// (default 25s).
	PingInterval time.Duration
	// PongTimeout closes a connection that has not answered a ping for this
	// long (default 60s).
	PongTimeout time.Duration
	// HeartbeatInterval is how often this instance re-announces its local
	// room members to other instances (default 20s).
	HeartbeatInterval time.Duration
	// MemberTTL expires members of other instances that were not refreshed by
	// gossip for this long (default 60s; raised to three heartbeat intervals
	// when shorter than two).
	MemberTTL time.Duration
	// WriteTimeout bounds writing one frame to a client (default 10s).
	WriteTimeout time.Duration
	// SendQueue is the number of outbound frames buffered per connection; a
	// client that falls further behind is disconnected as a slow consumer
	// (default 4096). The queue is allocated on demand, so a generous bound
	// costs nothing for clients that keep up; it must absorb bursts such as
	// one presence event per member when a large room is first synced.
	SendQueue int
	// SendQueueBytes bounds the bytes buffered per connection in the same
	// way (default 4 MiB; a single larger frame is still accepted).
	SendQueueBytes int
	// BrokerTimeout bounds each broker Publish/Subscribe call made on behalf
	// of a client (default 5s).
	BrokerTimeout time.Duration
	// OnFrame, if set, is called once per websocket frame received from
	// (dir "in") or written to (dir "out") a client, e.g. to feed the
	// ws_messages_total metric. It must be fast and safe for concurrent use.
	OnFrame func(dir string)
}
