package realtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/thasami/backyard/internal/pubsub"
)

// Gossip kinds published on "rt.<site>.room.<room>".
//
//   - join:    a member joined on instance I (sync=true asks every other
//     instance to answer with a "members" announcement);
//   - update:  a member's presence state changed;
//   - leave:   a member left;
//   - members: the authoritative list of instance I's local members of the
//     room (heartbeat, sync answer). Members of I missing from the list are
//     gone. Large lists are split into N parts (P = 1..N) sharing one
//     sequence number; members are applied per part, departures only once
//     every part arrived;
//   - msg:     a room message, delivered by every instance (including the
//     sender's) to its local members.
//
// Presence gossip carries a per-instance sequence number S, assigned under
// the site lock together with the state change it describes. Snapshots
// ("members") are published from other goroutines than the joins/leaves/
// updates they summarize and may overtake newer events on the wire, so
// receivers track the newest sequence applied per member (and, for a while,
// per departed member) and ignore older information about that member.
const (
	gossipJoin    = "join"
	gossipUpdate  = "update"
	gossipLeave   = "leave"
	gossipMembers = "members"
	gossipMsg     = "msg"
)

// gossip is the broker payload on room topics.
type gossip struct {
	K    string          `json:"k"`
	I    string          `json:"i"`
	S    uint64          `json:"s,omitempty"`
	M    *member         `json:"m,omitempty"`
	Ms   []member        `json:"ms,omitempty"`
	P    int             `json:"p,omitempty"` // part number of a split "members" snapshot
	N    int             `json:"n,omitempty"` // number of parts of a split snapshot
	Sync bool            `json:"sync,omitempty"`
	From *sender         `json:"from,omitempty"`
	To   string          `json:"to,omitempty"`
	Echo bool            `json:"echo,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

// room is one room of a site on this instance. It exists while at least one
// local connection is a member. Guarded by site.mu.
type room struct {
	name   string
	local  map[string]*localMember  // cid → member connected to this instance
	remote map[string]*remoteMember // cid → member connected elsewhere
	// gone remembers recently departed remote members (cid → sequence of
	// the departure) so that a stale snapshot cannot resurrect them.
	gone map[string]tombstone
	// partial collects the parts of split "members" snapshots, by instance.
	partial map[string]*partialSnapshot
}

// maxSnapshotParts bounds N in split snapshots.
const maxSnapshotParts = 4096

// partialSnapshot accumulates the parts of one split "members" snapshot.
type partialSnapshot struct {
	seq   uint64
	n     int
	parts map[int]struct{}
	cids  map[string]struct{}
	at    time.Time
}

type tombstone struct {
	seq uint64
	at  time.Time
}

type localMember struct {
	c *conn
	m member
}

type remoteMember struct {
	m        member
	instance string
	seen     time.Time
	seq      uint64 // newest gossip sequence applied to this member
}

// membership is a connection's view of a joined room (read goroutine only).
type membership struct {
	topic *topicEntry
}

func newRoom(name string) *room {
	return &room{
		name:    name,
		local:   make(map[string]*localMember),
		remote:  make(map[string]*remoteMember),
		gone:    make(map[string]tombstone),
		partial: make(map[string]*partialSnapshot),
	}
}

// collectPart records part g.P of instance g.I's split snapshot g.S, whose
// members are cids. It returns every member of the snapshot once all parts
// arrived, else nil (also for parts of a snapshot superseded by a newer one).
func (r *room) collectPart(g *gossip, cids map[string]struct{}, now time.Time) map[string]struct{} {
	if g.N > maxSnapshotParts || g.P < 1 || g.P > g.N {
		return nil
	}
	p := r.partial[g.I]
	switch {
	case p == nil || p.seq < g.S:
		p = &partialSnapshot{seq: g.S, n: g.N, parts: make(map[int]struct{}), cids: make(map[string]struct{}), at: now}
		r.partial[g.I] = p
	case p.seq > g.S || p.n != g.N:
		return nil
	}
	p.parts[g.P] = struct{}{}
	for cid := range cids {
		p.cids[cid] = struct{}{}
	}
	if len(p.parts) < p.n {
		return nil
	}
	delete(r.partial, g.I)
	return p.cids
}

// localMembers returns the members connected to this instance.
func (r *room) localMembers() []member {
	out := make([]member, 0, len(r.local))
	for _, lm := range r.local {
		out = append(out, lm.m)
	}
	sortMembers(out)
	return out
}

// others returns every known member except cid, oldest first.
func (r *room) others(cid string) []member {
	out := make([]member, 0, len(r.local)+len(r.remote))
	for id, lm := range r.local {
		if id != cid {
			out = append(out, lm.m)
		}
	}
	for id, rm := range r.remote {
		if id != cid {
			out = append(out, rm.m)
		}
	}
	sortMembers(out)
	return out
}

func sortMembers(ms []member) {
	slices.SortFunc(ms, func(a, b member) int {
		if c := a.JoinedAt.Compare(b.JoinedAt); c != 0 {
			return c
		}
		return strings.Compare(a.CID, b.CID)
	})
}

// broadcastPresence sends a presence event about m to every local member
// except m itself and exclude.
func (r *room) broadcastPresence(op string, m member, exclude string) {
	var frame []byte
	for cid, lm := range r.local {
		if cid == m.CID || cid == exclude {
			continue
		}
		if frame == nil {
			frame = mustJSON(presenceFrame{T: "presence", Room: r.name, Op: op, Member: m})
		}
		lm.c.enqueue(frame)
	}
}

// upsertRemote records a member announced by instance inst in gossip with
// sequence seq (0 = unsequenced). Information older than what is already
// known about the member is ignored.
func (r *room) upsertRemote(inst string, m member, seq uint64, now time.Time) {
	if !cidRe.MatchString(m.CID) {
		return
	}
	if _, ok := r.local[m.CID]; ok {
		return
	}
	rm := r.remote[m.CID]
	if seq != 0 {
		if tomb, ok := r.gone[m.CID]; ok {
			if tomb.seq >= seq {
				return // it left after this information was produced
			}
			delete(r.gone, m.CID)
		}
		if rm != nil && rm.seq > seq {
			return
		}
	}
	if len(m.State) == 0 || string(m.State) == "null" {
		m.State = emptyObject
	}
	if rm == nil {
		r.remote[m.CID] = &remoteMember{m: m, instance: inst, seen: now, seq: seq}
		r.broadcastPresence("join", m, "")
		return
	}
	rm.seen = now
	rm.instance = inst
	rm.seq = max(rm.seq, seq)
	if !bytes.Equal(rm.m.State, m.State) || rm.m.User != m.User {
		rm.m.State, rm.m.User = m.State, m.User
		r.broadcastPresence("update", rm.m, "")
	}
}

// removeRemote drops member cid of instance inst because of gossip with
// sequence seq (0 = unsequenced, e.g. expiry).
func (r *room) removeRemote(inst, cid string, seq uint64, now time.Time) {
	rm := r.remote[cid]
	if rm == nil {
		if seq != 0 && cidRe.MatchString(cid) {
			if tomb, ok := r.gone[cid]; !ok || tomb.seq < seq {
				r.gone[cid] = tombstone{seq: seq, at: now}
			}
		}
		return
	}
	if rm.instance != inst || (seq != 0 && rm.seq > seq) {
		return
	}
	delete(r.remote, cid)
	if seq != 0 {
		r.gone[cid] = tombstone{seq: seq, at: now}
	}
	r.broadcastPresence("leave", rm.m, "")
}

// ---------------------------------------------------------------------------
// Client operations (called from the connection's read goroutine)

func (c *conn) handleJoin(f *inFrame, id json.RawMessage) {
	h, s := c.h, c.site
	if !roomRe.MatchString(f.Room) {
		c.sendError(id, errf(codeBadRequest, "invalid room name (1-128 of A-Z a-z 0-9 _ - : .)"))
		return
	}
	state, werr := normalizeState(f.State, h.cfg.MaxPresenceBytes)
	if werr != nil {
		c.sendError(id, werr)
		return
	}
	topic := roomTopic(s.name, f.Room)

	if c.rooms[f.Room] != nil {
		// Re-join: replace the state and answer with a fresh member list.
		s.mu.Lock()
		r, lm := c.self(f.Room)
		if lm == nil {
			s.mu.Unlock()
			c.sendError(id, errf(codeInternal, "room membership lost; leave and join again"))
			return
		}
		changed := !bytes.Equal(lm.m.State, state)
		lm.m.State = state
		me := lm.m
		c.enqueue(mustJSON(joinedFrame{T: "joined", ID: id, Room: f.Room, Self: me, Members: r.others(c.cid)}))
		var seq uint64
		if changed {
			r.broadcastPresence("update", me, "")
			seq = h.nextSeq()
		}
		s.mu.Unlock()
		if changed {
			h.publish(topic, gossip{K: gossipUpdate, I: h.instance, S: seq, M: &me})
		}
		return
	}
	if len(c.rooms) >= h.cfg.MaxRoomsPerConn {
		c.sendError(id, errf(codeRateLimited, "too many rooms on this connection (max %d)", h.cfg.MaxRoomsPerConn))
		return
	}

	ctx, cancel := h.brokerCtx()
	siteName, roomName := s.name, f.Room
	entry, err := h.topics.acquire(ctx, topic, func(p []byte) { h.onRoomMessage(siteName, roomName, p) })
	cancel()
	if err != nil {
		c.backendError(id, "join", err)
		return
	}

	me := member{CID: c.cid, User: c.summary, State: state, JoinedAt: time.Now().UTC().Truncate(time.Millisecond)}
	s.mu.Lock()
	r := s.rooms[f.Room]
	firstLocal := r == nil
	if firstLocal {
		r = newRoom(f.Room)
		s.rooms[f.Room] = r
	}
	r.local[c.cid] = &localMember{c: c, m: me}
	// Enqueued under the lock: no presence event can overtake "joined".
	c.enqueue(mustJSON(joinedFrame{T: "joined", ID: id, Room: f.Room, Self: me, Members: r.others(c.cid)}))
	r.broadcastPresence("join", me, "")
	seq := h.nextSeq()
	s.mu.Unlock()
	c.rooms[f.Room] = &membership{topic: entry}

	// The first local member asks the other instances for their members.
	h.publish(topic, gossip{K: gossipJoin, I: h.instance, S: seq, M: &me, Sync: firstLocal})
}

// self returns the room and c's local entry in it (nil if not a member).
// The caller holds site.mu.
func (c *conn) self(name string) (*room, *localMember) {
	r := c.site.rooms[name]
	if r == nil {
		return nil, nil
	}
	return r, r.local[c.cid]
}

// leaveRoom removes c from a room (explicit leave or disconnect).
func (c *conn) leaveRoom(name string) {
	ms := c.rooms[name]
	if ms == nil {
		return
	}
	delete(c.rooms, name)
	h, s := c.h, c.site
	var (
		me    member
		found bool
		seq   uint64
	)
	s.mu.Lock()
	if r := s.rooms[name]; r != nil {
		if lm := r.local[c.cid]; lm != nil {
			me, found = lm.m, true
			delete(r.local, c.cid)
			r.broadcastPresence("leave", me, "")
			seq = h.nextSeq()
		}
		if len(r.local) == 0 {
			delete(s.rooms, name)
		}
	}
	s.mu.Unlock()
	if found {
		h.publish(roomTopic(s.name, name), gossip{K: gossipLeave, I: h.instance, S: seq, M: &me})
	}
	h.topics.release(ms.topic)
}

func (c *conn) handleLeave(f *inFrame, id json.RawMessage) {
	if !roomRe.MatchString(f.Room) {
		c.sendError(id, errf(codeBadRequest, "invalid room name"))
		return
	}
	c.leaveRoom(f.Room)
	if id != nil {
		c.enqueue(mustJSON(ackFrame{T: "ack", ID: id}))
	}
}

func (c *conn) handleState(f *inFrame, id json.RawMessage) {
	h, s := c.h, c.site
	if c.rooms[f.Room] == nil {
		c.sendError(id, errf(codeNotFound, "not a member of room %q", truncate(f.Room, 128)))
		return
	}
	state, werr := normalizeState(f.State, h.cfg.MaxPresenceBytes)
	if werr != nil {
		c.sendError(id, werr)
		return
	}
	s.mu.Lock()
	r, lm := c.self(f.Room)
	if lm == nil {
		s.mu.Unlock()
		c.sendError(id, errf(codeInternal, "room membership lost; leave and join again"))
		return
	}
	changed := !bytes.Equal(lm.m.State, state)
	lm.m.State = state
	me := lm.m
	var seq uint64
	if changed {
		r.broadcastPresence("update", me, "")
		seq = h.nextSeq()
	}
	s.mu.Unlock()
	if changed {
		h.publish(roomTopic(s.name, f.Room), gossip{K: gossipUpdate, I: h.instance, S: seq, M: &me})
	}
	if id != nil {
		c.enqueue(mustJSON(ackFrame{T: "ack", ID: id}))
	}
}

func (c *conn) handleSend(f *inFrame, id json.RawMessage) {
	h, s := c.h, c.site
	if c.rooms[f.Room] == nil {
		c.sendError(id, errf(codeNotFound, "not a member of room %q", truncate(f.Room, 128)))
		return
	}
	if f.To != "" {
		if !cidRe.MatchString(f.To) {
			c.sendError(id, errf(codeBadRequest, "invalid recipient connection id"))
			return
		}
		var isLocal, isRemote bool
		s.mu.Lock()
		if r := s.rooms[f.Room]; r != nil {
			_, isLocal = r.local[f.To]
			_, isRemote = r.remote[f.To]
		}
		s.mu.Unlock()
		if !isLocal && !isRemote {
			c.sendError(id, errf(codeNotFound, "no member %q in room %q", f.To, f.Room))
			return
		}
	}
	g := gossip{
		K: gossipMsg, I: h.instance,
		From: &sender{CID: c.cid, User: c.summary},
		To:   f.To, Echo: f.Echo,
		Data: normalizeData(f.Data),
	}
	payload, err := json.Marshal(g)
	if err != nil {
		c.sendError(id, errf(codeBadRequest, "invalid data"))
		return
	}
	ctx, cancel := h.brokerCtx()
	err = h.broker.Publish(ctx, roomTopic(s.name, f.Room), payload)
	cancel()
	if err != nil {
		if errors.Is(err, pubsub.ErrTooLarge) {
			c.sendError(id, errf(codeTooLarge, "message too large"))
			return
		}
		c.backendError(id, "send", err)
		return
	}
	if id != nil {
		c.enqueue(mustJSON(ackFrame{T: "ack", ID: id}))
	}
}

// ---------------------------------------------------------------------------
// Broker side

// onRoomMessage handles gossip for one room. Runs on the broker's per-topic
// goroutine, so messages of one topic are processed in order.
func (h *hub) onRoomMessage(siteName, roomName string, payload []byte) {
	var g gossip
	if err := json.Unmarshal(payload, &g); err != nil {
		h.log.Warn("ignoring malformed room gossip", "site", siteName, "room", roomName, "err", err)
		return
	}
	s := h.site(siteName)
	if s == nil {
		return
	}
	if g.K == gossipMsg {
		h.deliverMsg(s, roomName, &g)
		return
	}
	if g.I == "" || g.I == h.instance {
		return // our own presence changes were applied locally already
	}
	if reply, seq := h.applyPresence(s, roomName, &g); len(reply) > 0 {
		h.publishMembers(roomTopic(siteName, roomName), seq, reply)
	}
}

// applyPresence applies presence gossip from another instance to the local
// view of a room. It returns this instance's members (and the snapshot's
// sequence number) when g asks for them.
func (h *hub) applyPresence(s *site, roomName string, g *gossip) (reply []member, replySeq uint64) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock() // deferred: handler panics are recovered by the broker
	r := s.rooms[roomName]
	if r == nil {
		return nil, 0
	}
	switch g.K {
	case gossipJoin, gossipUpdate:
		if g.M != nil {
			r.upsertRemote(g.I, *g.M, g.S, now)
		}
		if g.K == gossipJoin && g.Sync {
			return r.localMembers(), h.nextSeq()
		}
	case gossipLeave:
		if g.M != nil {
			r.removeRemote(g.I, g.M.CID, g.S, now)
		}
	case gossipMembers:
		// The snapshot lists every local member of instance I at sequence
		// S: members of I it omits are gone, unless we know of them from
		// newer gossip (they joined after the snapshot was taken).
		present := make(map[string]struct{}, len(g.Ms))
		for _, m := range g.Ms {
			present[m.CID] = struct{}{}
			r.upsertRemote(g.I, m, g.S, now)
		}
		if g.N > 1 {
			// Departures are known only once every part arrived; an
			// incomplete snapshot is left to member expiry.
			present = r.collectPart(g, present, now)
		}
		if present != nil {
			for cid, rm := range r.remote {
				if _, ok := present[cid]; !ok && rm.instance == g.I {
					r.removeRemote(g.I, cid, g.S, now)
				}
			}
		}
	}
	return nil, 0
}

// deliverMsg delivers a room message to the local recipients.
func (h *hub) deliverMsg(s *site, roomName string, g *gossip) {
	if g.From == nil || g.From.CID == "" {
		return
	}
	data := g.Data
	if len(data) == 0 {
		data = json.RawMessage("null")
	}
	var frame []byte
	send := func(lm *localMember) {
		if frame == nil {
			frame = mustJSON(msgFrame{T: "msg", Room: roomName, From: *g.From, Data: data})
		}
		lm.c.enqueue(frame)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.rooms[roomName]
	if r == nil {
		return
	}
	if g.To != "" {
		if lm := r.local[g.To]; lm != nil {
			send(lm)
		}
		if g.Echo && g.From.CID != g.To {
			if lm := r.local[g.From.CID]; lm != nil {
				send(lm)
			}
		}
		return
	}
	for cid, lm := range r.local {
		if cid == g.From.CID && !g.Echo {
			continue
		}
		send(lm)
	}
}
