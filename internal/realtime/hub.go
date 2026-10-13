package realtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/docstore"
	"github.com/thasami/backyard/internal/pubsub"
	"github.com/thasami/backyard/internal/pubsub/memory"
)

// Defaults for Options (SPEC §6).
const (
	DefaultPingInterval      = 25 * time.Second
	DefaultPongTimeout       = 60 * time.Second
	DefaultHeartbeatInterval = 20 * time.Second
	DefaultMemberTTL         = 60 * time.Second
	DefaultWriteTimeout      = 10 * time.Second
	DefaultSendQueue         = 4096
	DefaultSendQueueBytes    = 4 << 20
	DefaultBrokerTimeout     = 5 * time.Second
)

// closeWait bounds how long Close waits for connections to finish cleanup.
const closeWait = 10 * time.Second

const (
	// maxPresenceCap caps Config.MaxPresenceBytes: a member (state + user
	// summary) must always fit in one broker message (pubsub.MaxPayloadBytes).
	maxPresenceCap = 512 << 10
	// snapshotBudget bounds the encoded members carried by one "members"
	// gossip message; larger snapshots are split into parts.
	snapshotBudget = 256 << 10
	// maxInstanceLabel bounds the caller-chosen part of the instance id.
	maxInstanceLabel = 64
)

// hub implements Hub.
type hub struct {
	broker   pubsub.Broker
	owned    pubsub.Broker // closed by Close (nil when the caller owns broker)
	cfg      config.RealtimeConfig
	opts     Options
	instance string
	log      *slog.Logger
	topics   *topicSet

	// bg runs heartbeat and expiry loops.
	bgCtx    context.Context
	bgCancel context.CancelFunc
	bg       sync.WaitGroup
	// conns tracks ServeWS calls past the point of no return.
	conns sync.WaitGroup

	mu      sync.Mutex
	closed  bool
	active  map[*conn]struct{}
	perUser map[string]int
	sites   map[string]*site

	nsubs atomic.Int64
	// seq numbers presence gossip (see gossip). It starts at the wall clock
	// so that it keeps increasing across restarts of an instance that reuses
	// its InstanceID.
	seq atomic.Uint64
}

// site holds the realtime state of one site on this instance.
type site struct {
	name string

	// nconns is guarded by hub.mu.
	nconns int

	mu sync.Mutex
	// subs: collection → db subscriptions of local connections.
	subs map[string]map[*dbSub]struct{}
	// rooms with at least one local member.
	rooms map[string]*room
}

// dbSub is one {"t":"sub"} of a connection.
type dbSub struct {
	c          *conn
	id         json.RawMessage
	key        string
	collection string
	topic      *topicEntry
}

// New creates a Hub that uses b for cross-instance fan-out. The hub does not
// own b: Close leaves it open. A nil b selects a private in-process broker
// (single instance), which Close releases.
func New(b pubsub.Broker, opts Options) Hub {
	var owned pubsub.Broker
	if b == nil {
		b = memory.New()
		owned = b
	}
	cfg := opts.Config
	setDefault(&cfg.MaxRoomsPerConn, 50)
	setDefault(&cfg.MaxSubsPerConn, 100)
	setDefault(&cfg.MessagesPerSecond, 60)
	setDefault(&cfg.MessageBurst, 120)
	setDefault(&cfg.MaxMessageBytes, 64<<10)
	setDefault(&cfg.MaxConnsPerUser, 50)
	setDefault(&cfg.MaxPresenceBytes, 8<<10)
	cfg.MaxPresenceBytes = min(cfg.MaxPresenceBytes, maxPresenceCap)
	opts.Config = cfg
	setDefaultDur(&opts.PingInterval, DefaultPingInterval)
	setDefaultDur(&opts.PongTimeout, DefaultPongTimeout)
	setDefaultDur(&opts.HeartbeatInterval, DefaultHeartbeatInterval)
	setDefaultDur(&opts.MemberTTL, DefaultMemberTTL)
	setDefaultDur(&opts.WriteTimeout, DefaultWriteTimeout)
	setDefaultDur(&opts.BrokerTimeout, DefaultBrokerTimeout)
	if opts.SendQueue <= 0 {
		opts.SendQueue = DefaultSendQueue
	}
	if opts.SendQueueBytes <= 0 {
		opts.SendQueueBytes = DefaultSendQueueBytes
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.MemberTTL < 2*opts.HeartbeatInterval {
		// Members would expire between two heartbeats and flap.
		ttl := 3 * opts.HeartbeatInterval
		opts.Logger.Warn("realtime: MemberTTL shorter than two heartbeat intervals; raising it",
			"member_ttl", opts.MemberTTL, "heartbeat_interval", opts.HeartbeatInterval, "new_member_ttl", ttl)
		opts.MemberTTL = ttl
	}
	// The gossip identity always carries a random part: two instances that
	// were given the same InstanceID (e.g. a hostname that is not unique on
	// some container platforms) would otherwise take each other's gossip for
	// their own and never see each other's members.
	instance := "i"
	if label := truncate(opts.InstanceID, maxInstanceLabel); label != "" {
		instance = label
	}
	instance += "~" + randomHex(6)

	h := &hub{
		broker:   b,
		owned:    owned,
		cfg:      cfg,
		opts:     opts,
		instance: instance,
		log:      opts.Logger.With("component", "realtime", "instance", instance),
		topics:   newTopicSet(b),
		active:   make(map[*conn]struct{}),
		perUser:  make(map[string]int),
		sites:    make(map[string]*site),
	}
	h.seq.Store(uint64(time.Now().UnixNano()))
	h.bgCtx, h.bgCancel = context.WithCancel(context.Background())
	h.bg.Add(2)
	go h.heartbeatLoop()
	go h.expiryLoop()
	return h
}

func setDefault(p *int, v int) {
	if *p <= 0 {
		*p = v
	}
}

func setDefaultDur(p *time.Duration, v time.Duration) {
	if *p <= 0 {
		*p = v
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // crypto/rand.Read never fails (Go ≥ 1.24)
	return hex.EncodeToString(b)
}

// nextSeq returns the next presence sequence number. Callers hold the lock of
// the site whose state change the number describes.
func (h *hub) nextSeq() uint64 { return h.seq.Add(1) }

func dbTopic(siteName string) string { return "rt." + siteName + ".db" }

func roomTopic(siteName, room string) string { return "rt." + siteName + ".room." + room }

// site returns the state of siteName or nil if no local connection uses it.
func (h *hub) site(siteName string) *site {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sites[siteName]
}

func (h *hub) frame(dir string) {
	if h.opts.OnFrame != nil {
		h.opts.OnFrame(dir)
	}
}

// brokerCtx returns a context for one broker call made on behalf of a client.
func (h *hub) brokerCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), h.opts.BrokerTimeout)
}

// publish marshals v and publishes it on topic (best effort; errors logged).
func (h *hub) publish(topic string, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		h.log.Error("encode gossip", "err", err)
		return
	}
	ctx, cancel := h.brokerCtx()
	defer cancel()
	if err := h.broker.Publish(ctx, topic, data); err != nil && !errors.Is(err, pubsub.ErrClosed) {
		h.log.Warn("publish failed", "topic", topic, "err", err)
	}
}

// ---------------------------------------------------------------------------
// Database events

// dbWire is the broker payload of a database change.
type dbWire struct {
	Collection string          `json:"c"`
	Op         Op              `json:"o"`
	DocID      string          `json:"id"`
	Doc        json.RawMessage `json:"d"`
}

// PublishDB fans out a database change to subscribers on all instances.
// Changes to system collections (names starting with "_") are never
// published.
func (h *hub) PublishDB(ctx context.Context, siteName string, ev DBEvent) error {
	h.mu.Lock()
	closed := h.closed
	h.mu.Unlock()
	if closed {
		return ErrClosed
	}
	if err := docstore.ValidateSite(siteName); err != nil {
		return fmt.Errorf("realtime: publish db event: %w", err)
	}
	if strings.HasPrefix(siteName, "_") || strings.HasPrefix(ev.Collection, "_") {
		return nil // system data is never visible to websocket clients
	}
	if err := docstore.ValidateCollection(ev.Collection, false); err != nil {
		return fmt.Errorf("realtime: publish db event: %w", err)
	}
	switch ev.Op {
	case OpCreate, OpUpdate, OpDelete:
	default:
		return fmt.Errorf("realtime: publish db event: unknown op %q", ev.Op)
	}
	w := dbWire{Collection: ev.Collection, Op: ev.Op, DocID: ev.DocID, Doc: json.RawMessage("null")}
	if ev.Doc != nil {
		doc, err := marshalUnescaped(ev.Doc.Wire())
		if err != nil {
			return fmt.Errorf("realtime: encode document: %w", err)
		}
		w.Doc = doc
		if w.DocID == "" {
			w.DocID = ev.Doc.ID
		}
	}
	if w.DocID == "" {
		return errors.New("realtime: publish db event: missing document id")
	}
	payload, err := marshalUnescaped(w)
	if err != nil {
		return fmt.Errorf("realtime: encode db event: %w", err)
	}
	if len(payload) > pubsub.MaxPayloadBytes && ev.Doc != nil {
		// The document does not fit in one broker message (database.max_doc_bytes
		// is not bounded by it): publish the change without the document
		// (doc: null) rather than not at all, so that subscribers still learn
		// about it and can read the document.
		h.log.Info("db event published without its document: too large for one broker message",
			"site", siteName, "collection", ev.Collection, "doc_id", w.DocID,
			"bytes", len(payload), "max_bytes", pubsub.MaxPayloadBytes)
		w.Doc = json.RawMessage("null")
		if payload, err = marshalUnescaped(w); err != nil {
			return fmt.Errorf("realtime: encode db event: %w", err)
		}
	}
	if err := h.broker.Publish(ctx, dbTopic(siteName), payload); err != nil {
		return fmt.Errorf("realtime: publish db event: %w", err)
	}
	return nil
}

// onDBMessage delivers a database change to local subscribers of its
// collection. Runs on the broker's per-topic goroutine.
func (h *hub) onDBMessage(siteName string, payload []byte) {
	var w dbWire
	if err := json.Unmarshal(payload, &w); err != nil || w.Collection == "" {
		h.log.Warn("ignoring malformed db event", "site", siteName, "err", err)
		return
	}
	s := h.site(siteName)
	if s == nil {
		return
	}
	if len(w.Doc) == 0 {
		w.Doc = json.RawMessage("null")
	}
	// Everything after "sub" is identical for all subscribers: encode once.
	tail, err := marshalUnescaped(struct {
		Collection string          `json:"collection"`
		Op         Op              `json:"op"`
		Doc        json.RawMessage `json:"doc"`
		DocID      string          `json:"doc_id"`
	}{w.Collection, w.Op, w.Doc, w.DocID})
	if err != nil {
		h.log.Warn("ignoring unencodable db event", "site", siteName, "err", err)
		return
	}
	const head = `{"t":"db","sub":`
	s.mu.Lock()
	defer s.mu.Unlock()
	for sub := range s.subs[w.Collection] {
		frame := make([]byte, 0, len(head)+len(sub.id)+len(tail))
		frame = append(frame, head...)
		frame = append(frame, sub.id...)
		frame = append(frame, ',')
		frame = append(frame, tail[1:]...)
		sub.c.enqueue(frame)
	}
}

// ---------------------------------------------------------------------------
// Connection registry

// admit reserves a connection slot for user on siteName. It returns a
// non-empty error code when the connection must be refused.
func (h *hub) admit(siteName, userID string) (*site, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, "closed"
	}
	if h.perUser[userID] >= h.cfg.MaxConnsPerUser {
		return nil, codeRateLimited
	}
	h.perUser[userID]++
	s := h.sites[siteName]
	if s == nil {
		s = &site{
			name:  siteName,
			subs:  make(map[string]map[*dbSub]struct{}),
			rooms: make(map[string]*room),
		}
		h.sites[siteName] = s
	}
	s.nconns++
	h.conns.Add(1)
	return s, ""
}

// activate makes c visible to Close and Stats; false if the hub is closing.
func (h *hub) activate(c *conn) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false
	}
	h.active[c] = struct{}{}
	return true
}

// leaveHub releases everything admit and activate took.
func (h *hub) leaveHub(c *conn, s *site, userID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c != nil {
		delete(h.active, c)
	}
	if n := h.perUser[userID] - 1; n > 0 {
		h.perUser[userID] = n
	} else {
		delete(h.perUser, userID)
	}
	s.nconns--
	if s.nconns <= 0 && h.sites[s.name] == s {
		delete(h.sites, s.name)
	}
	h.conns.Done()
}

// Stats returns local counters.
func (h *hub) Stats() Stats {
	h.mu.Lock()
	st := Stats{Connections: len(h.active), Subs: int(h.nsubs.Load())}
	sites := make([]*site, 0, len(h.sites))
	for _, s := range h.sites {
		sites = append(sites, s)
	}
	h.mu.Unlock()
	for _, s := range sites {
		s.mu.Lock()
		st.Rooms += len(s.rooms)
		s.mu.Unlock()
	}
	return st
}

// Close disconnects every client with StatusGoingAway (members leave their
// rooms, which is gossiped to other instances), cancels broker subscriptions
// and stops background work. The broker itself is not closed.
func (h *hub) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	conns := make([]*conn, 0, len(h.active))
	for c := range h.active {
		conns = append(conns, c)
	}
	h.mu.Unlock()

	for _, c := range conns {
		c.closeGoingAway()
	}
	done := make(chan struct{})
	go func() {
		h.conns.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(closeWait):
		h.log.Warn("timed out waiting for websocket connections to close")
		for _, c := range conns {
			c.abort()
		}
	}
	h.bgCancel()
	h.bg.Wait()
	h.topics.close()
	if h.owned != nil {
		return h.owned.Close()
	}
	return nil
}

// ---------------------------------------------------------------------------
// Background gossip

func (h *hub) heartbeatLoop() {
	defer h.bg.Done()
	t := time.NewTicker(h.opts.HeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-h.bgCtx.Done():
			return
		case <-t.C:
			h.heartbeat()
		}
	}
}

// heartbeat re-announces the full list of local members of every room.
func (h *hub) heartbeat() {
	type announce struct {
		topic string
		g     gossip
	}
	var out []announce
	h.mu.Lock()
	sites := make([]*site, 0, len(h.sites))
	for _, s := range h.sites {
		sites = append(sites, s)
	}
	h.mu.Unlock()
	for _, s := range sites {
		s.mu.Lock()
		for _, r := range s.rooms {
			out = append(out, announce{roomTopic(s.name, r.name),
				gossip{K: gossipMembers, I: h.instance, S: h.nextSeq(), Ms: r.localMembers()}})
		}
		s.mu.Unlock()
	}
	for _, a := range out {
		if h.bgCtx.Err() != nil {
			return
		}
		h.publishMembers(a.topic, a.g.S, a.g.Ms)
	}
}

// publishMembers announces this instance's local members of a room (snapshot
// with sequence seq), split into parts when it would not fit comfortably in
// one broker message.
func (h *hub) publishMembers(topic string, seq uint64, ms []member) {
	parts := splitMembers(ms, snapshotBudget)
	if len(parts) == 1 {
		h.publish(topic, gossip{K: gossipMembers, I: h.instance, S: seq, Ms: parts[0]})
		return
	}
	for i, p := range parts {
		h.publish(topic, gossip{K: gossipMembers, I: h.instance, S: seq, Ms: p, P: i + 1, N: len(parts)})
	}
}

// splitMembers groups ms into parts of at most budget encoded bytes (a single
// larger member gets a part of its own). It always returns at least one part.
func splitMembers(ms []member, budget int) [][]member {
	var (
		parts [][]member
		cur   []member
		size  int
	)
	for _, m := range ms {
		n := len(mustJSON(m)) + 1
		if len(cur) > 0 && size+n > budget {
			parts = append(parts, cur)
			cur, size = nil, 0
		}
		cur = append(cur, m)
		size += n
	}
	return append(parts, cur)
}

func (h *hub) expiryLoop() {
	defer h.bg.Done()
	every := max(h.opts.MemberTTL/4, 10*time.Millisecond)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-h.bgCtx.Done():
			return
		case now := <-t.C:
			h.expire(now)
		}
	}
}

// expire removes remote members not refreshed within MemberTTL and forgets
// old departures.
func (h *hub) expire(now time.Time) {
	h.mu.Lock()
	sites := make([]*site, 0, len(h.sites))
	for _, s := range h.sites {
		sites = append(sites, s)
	}
	h.mu.Unlock()
	cutoff := now.Add(-h.opts.MemberTTL)
	for _, s := range sites {
		s.mu.Lock()
		for _, r := range s.rooms {
			for cid, rm := range r.remote {
				if rm.seen.Before(cutoff) {
					r.removeRemote(rm.instance, cid, 0, now)
				}
			}
			for cid, tomb := range r.gone {
				if tomb.at.Before(cutoff) {
					delete(r.gone, cid)
				}
			}
			for inst, p := range r.partial {
				if p.at.Before(cutoff) {
					delete(r.partial, inst)
				}
			}
		}
		s.mu.Unlock()
	}
}
