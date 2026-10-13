package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/time/rate"

	"github.com/thasami/backyard/internal/docstore"
	"github.com/thasami/backyard/internal/identity"
)

// conn is one websocket client.
type conn struct {
	h       *hub
	ws      *websocket.Conn
	site    *site
	user    *identity.User
	summary identity.Summary
	cid     string
	log     *slog.Logger

	out     outQueue
	ctx     context.Context // canceled when the connection is finished
	cancel  context.CancelFunc
	closing atomic.Bool
	// closeOnce guards the single close attempt (graceful or abort).
	closeOnce sync.Once

	limiter     *rate.Limiter
	lastLimited time.Time

	// Read-goroutine-only state.
	subs  map[string]*dbSub
	rooms map[string]*membership
}

// ServeWS upgrades the request and serves the connection until it closes.
func (h *hub) ServeWS(w http.ResponseWriter, r *http.Request, siteName string, user *identity.User, allowedOrigins []string) {
	if h.cfg.Disabled {
		writeHTTPError(w, http.StatusNotFound, codeNotFound, "realtime is disabled")
		return
	}
	if user == nil || user.ID == "" {
		writeHTTPError(w, http.StatusUnauthorized, "unauthenticated", "authentication required")
		return
	}
	if err := docstore.ValidateSite(siteName); err != nil || strings.HasPrefix(siteName, "_") {
		writeHTTPError(w, http.StatusNotFound, codeNotFound, "unknown site")
		return
	}
	if !originAllowed(r, allowedOrigins) {
		h.log.Info("websocket origin rejected", "site", siteName, "origin", truncate(r.Header.Get("Origin"), 200))
		writeHTTPError(w, http.StatusForbidden, codeForbidden, "origin not allowed")
		return
	}
	s, refused := h.admit(siteName, user.ID)
	switch refused {
	case "":
	case "closed":
		writeHTTPError(w, http.StatusServiceUnavailable, codeInternal, "server is shutting down")
		return
	default:
		w.Header().Set("Retry-After", "10")
		writeHTTPError(w, http.StatusTooManyRequests, codeRateLimited,
			fmt.Sprintf("too many open connections for this user (max %d)", h.cfg.MaxConnsPerUser))
		return
	}

	// The Origin header was verified above; the library's own check (which
	// only knows the request host) is disabled so that allowedOrigins work.
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		h.leaveHub(nil, s, user.ID)
		h.log.Debug("websocket accept failed", "site", siteName, "err", err)
		return
	}
	c := h.newConn(ws, s, user)
	if !h.activate(c) {
		_ = ws.Close(websocket.StatusGoingAway, "server shutting down")
		h.leaveHub(nil, s, user.ID)
		return
	}
	defer h.leaveHub(c, s, user.ID)
	c.serve()
}

func (h *hub) newConn(ws *websocket.Conn, s *site, user *identity.User) *conn {
	cid := "c_" + randomHex(10)
	ctx, cancel := context.WithCancel(context.Background())
	return &conn{
		h:       h,
		ws:      ws,
		site:    s,
		user:    user,
		summary: user.Summary(),
		cid:     cid,
		log:     h.log.With("site", s.name, "cid", cid),
		out:     outQueue{maxFrames: h.opts.SendQueue, maxBytes: h.opts.SendQueueBytes, wake: make(chan struct{}, 1)},
		ctx:     ctx,
		cancel:  cancel,
		limiter: rate.NewLimiter(rate.Limit(h.cfg.MessagesPerSecond), h.cfg.MessageBurst),
		subs:    make(map[string]*dbSub),
		rooms:   make(map[string]*membership),
	}
}

// serve runs the connection: hello, writer, pinger and the read loop (in the
// calling goroutine), then cleanup.
func (c *conn) serve() {
	c.log.Debug("websocket connected", "user", c.user.ID)
	c.enqueue(mustJSON(helloFrame{T: "hello", CID: c.cid, User: c.user}))

	var (
		wg  sync.WaitGroup
		err error
	)
	wg.Go(c.writeLoop)
	wg.Go(c.pingLoop)
	defer func() {
		// Teardown also runs if a handler panics, so that the client's rooms,
		// subscriptions and goroutines are never leaked.
		if v := recover(); v != nil {
			c.log.Error("websocket handler panicked", "panic", v, "stack", string(debug.Stack()))
			err = fmt.Errorf("panic: %v", v)
		}
		c.closing.Store(true) // nothing more is sent to this client
		c.cleanup()
		c.cancel()
		c.abort()
		wg.Wait()
		c.log.Debug("websocket disconnected", "status", websocket.CloseStatus(err), "err", err)
	}()
	err = c.readLoop()
}

// cleanup leaves every room and drops every subscription.
func (c *conn) cleanup() {
	for name := range c.rooms {
		c.leaveRoom(name)
	}
	for _, sub := range c.subs {
		c.removeSub(sub)
	}
}

// outQueue buffers a connection's outbound frames. It is bounded by frame
// count and bytes (a frame counts until it has been written) and allocates
// nothing while empty, so idle connections stay cheap.
type outQueue struct {
	maxFrames int
	maxBytes  int
	wake      chan struct{} // capacity 1: frames are waiting

	mu     sync.Mutex
	frames [][]byte
	bytes  int
}

// push appends frame; false when the queue is full. A frame larger than
// maxBytes is still accepted into an empty queue.
func (q *outQueue) push(frame []byte) bool {
	q.mu.Lock()
	if len(q.frames) >= q.maxFrames || (q.bytes > 0 && q.bytes+len(frame) > q.maxBytes) {
		q.mu.Unlock()
		return false
	}
	q.frames = append(q.frames, frame)
	q.bytes += len(frame)
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return true
}

// peek returns the oldest frame without removing it (nil if empty).
func (q *outQueue) peek() []byte {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.frames) == 0 {
		return nil
	}
	return q.frames[0]
}

// pop removes the oldest frame (after it was written).
func (q *outQueue) pop() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.frames) == 0 {
		return
	}
	q.bytes -= len(q.frames[0])
	q.frames[0] = nil
	q.frames = q.frames[1:]
	if len(q.frames) == 0 {
		q.frames = nil // release the backing array
	}
}

// enqueue queues a frame for the writer. A client whose queue is full is a
// slow consumer and gets disconnected. Safe for concurrent use; never blocks.
func (c *conn) enqueue(frame []byte) {
	if c.closing.Load() {
		return
	}
	if !c.out.push(frame) {
		c.closeWith(websocket.StatusPolicyViolation, "slow consumer")
	}
}

// closeWith starts a graceful close (close frame + handshake) in the
// background. Only the first close request has an effect.
func (c *conn) closeWith(code websocket.StatusCode, reason string) {
	c.closeOnce.Do(func() {
		c.closing.Store(true)
		if code != websocket.StatusGoingAway {
			c.log.Info("closing websocket", "status", code, "reason", reason)
		}
		go func() { _ = c.ws.Close(code, reason) }()
	})
}

func (c *conn) closeGoingAway() { c.closeWith(websocket.StatusGoingAway, "server shutting down") }

// abort closes the underlying connection immediately (idempotent).
func (c *conn) abort() {
	c.closing.Store(true)
	_ = c.ws.CloseNow()
}

func (c *conn) writeLoop() {
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-c.out.wake:
		}
		for frame := c.out.peek(); frame != nil; frame = c.out.peek() {
			ctx, cancel := context.WithTimeout(c.ctx, c.h.opts.WriteTimeout)
			err := c.ws.Write(ctx, websocket.MessageText, frame)
			cancel()
			c.out.pop()
			if err != nil {
				if c.closing.Load() {
					// A graceful close is in progress (its close frame
					// makes further writes fail); let it finish.
					return
				}
				if c.ctx.Err() == nil {
					c.log.Debug("websocket write failed", "err", err)
				}
				c.abort()
				return
			}
			c.h.frame("out")
		}
	}
}

// pingLoop pings the client every PingInterval and closes the connection when
// no pong arrived for PongTimeout.
func (c *conn) pingLoop() {
	t := time.NewTicker(c.h.opts.PingInterval)
	defer t.Stop()
	lastPong := time.Now()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-t.C:
		}
		deadline := lastPong.Add(c.h.opts.PongTimeout)
		if !time.Now().Before(deadline) {
			c.pongTimeout()
			return
		}
		ctx, cancel := context.WithDeadline(c.ctx, deadline)
		err := c.ws.Ping(ctx)
		cancel()
		if err != nil {
			// Anything but our deadline means the connection is already
			// going away; the read loop notices.
			if errors.Is(err, context.DeadlineExceeded) && c.ctx.Err() == nil && !c.closing.Load() {
				c.pongTimeout()
			}
			return
		}
		lastPong = time.Now()
	}
}

func (c *conn) pongTimeout() {
	c.log.Info("closing unresponsive websocket (no pong)", "timeout", c.h.opts.PongTimeout)
	// The peer is unresponsive: a close handshake would only wait.
	c.abort()
}

// readLoop reads frames until the connection fails or closes.
func (c *conn) readLoop() error {
	max := c.h.cfg.MaxMessageBytes
	// Frames up to max get processed; frames up to hardLimit are answered
	// with a too_large error; anything larger aborts the connection
	// (close status 1009) without buffering it.
	hardLimit := int64(4 * max)
	c.ws.SetReadLimit(hardLimit)
	for {
		typ, r, err := c.ws.Reader(c.ctx)
		if err != nil {
			return err
		}
		data, err := io.ReadAll(io.LimitReader(r, int64(max)+1))
		if err != nil {
			return err
		}
		oversized := len(data) > max
		if oversized {
			data = nil
			if _, err := io.Copy(io.Discard, r); err != nil {
				return err
			}
		}
		c.h.frame("in")
		if !c.limiter.Allow() {
			c.rateLimited()
			continue
		}
		switch {
		case oversized:
			c.sendError(nil, errf(codeTooLarge, "frame exceeds %d bytes", max))
		case typ != websocket.MessageText:
			c.sendError(nil, errf(codeBadRequest, "binary frames are not supported"))
		default:
			c.handle(data)
		}
	}
}

// rateLimited reports dropped frames, at most once per second.
func (c *conn) rateLimited() {
	now := time.Now()
	if now.Sub(c.lastLimited) < time.Second {
		return
	}
	c.lastLimited = now
	c.sendError(nil, errf(codeRateLimited, "too many messages (limit %d/s, burst %d); message dropped",
		c.h.cfg.MessagesPerSecond, c.h.cfg.MessageBurst))
}

func (c *conn) sendError(id json.RawMessage, e *wireError) {
	c.enqueue(encodeError(id, e))
}

// backendError reports a broker failure to the client.
func (c *conn) backendError(id json.RawMessage, op string, err error) {
	if errors.Is(err, ErrClosed) {
		c.sendError(id, errf(codeInternal, "server is shutting down"))
		return
	}
	c.log.Warn("realtime backend error", "op", op, "err", err)
	c.sendError(id, errf(codeInternal, "realtime backend unavailable"))
}

// handle processes one text frame. Malformed input only ever produces an
// error frame.
func (c *conn) handle(data []byte) {
	if c.closing.Load() {
		return
	}
	var f inFrame
	if err := json.Unmarshal(data, &f); err != nil {
		c.sendError(peekID(data), errf(codeBadRequest, "malformed frame: %s", truncate(err.Error(), 200)))
		return
	}
	id, werr := normalizeID(f.ID)
	if werr != nil {
		c.sendError(nil, werr)
		return
	}
	switch f.T {
	case framePing:
		c.enqueue(pongFrame)
	case frameSub:
		c.handleSub(&f, id)
	case frameUnsub:
		c.handleUnsub(id)
	case frameJoin:
		c.handleJoin(&f, id)
	case frameLeave:
		c.handleLeave(&f, id)
	case frameState:
		c.handleState(&f, id)
	case frameSend:
		c.handleSend(&f, id)
	case "":
		c.sendError(id, errf(codeBadRequest, `missing frame type "t"`))
	default:
		c.sendError(id, errf(codeBadRequest, "unknown frame type %q", truncate(f.T, 64)))
	}
}

// ---------------------------------------------------------------------------
// Database subscriptions

func (c *conn) handleSub(f *inFrame, id json.RawMessage) {
	h, s := c.h, c.site
	if id == nil {
		c.sendError(nil, errf(codeBadRequest, "sub requires an id"))
		return
	}
	if strings.HasPrefix(f.Collection, "_") {
		c.sendError(id, errf(codeForbidden, "system collections cannot be subscribed to"))
		return
	}
	if err := docstore.ValidateCollection(f.Collection, false); err != nil {
		c.sendError(id, errf(codeBadRequest, "invalid collection name %q", truncate(f.Collection, 64)))
		return
	}
	key := string(id)
	if old := c.subs[key]; old != nil {
		if old.collection == f.Collection {
			c.enqueue(mustJSON(ackFrame{T: "ack", ID: id}))
			return
		}
		c.removeSub(old) // same id, different collection: replace
	}
	if len(c.subs) >= h.cfg.MaxSubsPerConn {
		c.sendError(id, errf(codeRateLimited, "too many subscriptions on this connection (max %d)", h.cfg.MaxSubsPerConn))
		return
	}
	ctx, cancel := h.brokerCtx()
	siteName := s.name
	entry, err := h.topics.acquire(ctx, dbTopic(siteName), func(p []byte) { h.onDBMessage(siteName, p) })
	cancel()
	if err != nil {
		c.backendError(id, "sub", err)
		return
	}
	sub := &dbSub{c: c, id: id, key: key, collection: f.Collection, topic: entry}
	s.mu.Lock()
	set := s.subs[sub.collection]
	if set == nil {
		set = make(map[*dbSub]struct{})
		s.subs[sub.collection] = set
	}
	set[sub] = struct{}{}
	// Enqueued under the lock: no event of this subscription precedes the ack.
	c.enqueue(mustJSON(ackFrame{T: "ack", ID: id}))
	s.mu.Unlock()
	c.subs[key] = sub
	h.nsubs.Add(1)
}

func (c *conn) handleUnsub(id json.RawMessage) {
	if id == nil {
		c.sendError(nil, errf(codeBadRequest, "unsub requires the id of the subscription"))
		return
	}
	if sub := c.subs[string(id)]; sub != nil {
		c.removeSub(sub)
	}
	c.enqueue(mustJSON(ackFrame{T: "ack", ID: id}))
}

func (c *conn) removeSub(sub *dbSub) {
	s := c.site
	s.mu.Lock()
	if set := s.subs[sub.collection]; set != nil {
		delete(set, sub)
		if len(set) == 0 {
			delete(s.subs, sub.collection)
		}
	}
	s.mu.Unlock()
	delete(c.subs, sub.key)
	c.h.nsubs.Add(-1)
	c.h.topics.release(sub.topic)
}

// ---------------------------------------------------------------------------

func writeHTTPError(w http.ResponseWriter, status int, code, msg string) {
	body, _ := json.Marshal(map[string]any{"error": errorBody{Code: code, Message: msg}})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
