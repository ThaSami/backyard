// Package postgres implements pubsub.Broker on PostgreSQL LISTEN/NOTIFY, so a
// horizontally scaled deployment that already uses Postgres for documents
// needs no extra infrastructure.
//
// All topics share one notification channel (Options.Channel). Each message is
// a JSON envelope {"topic": "...", "payload": "<base64>"}. NOTIFY payloads are
// limited to 8000 bytes, so larger messages are written to the table
// backyard_pubsub_spill (id bigserial, payload bytea, created_at timestamptz)
// and the notification carries {"topic": "...", "spill_id": N} instead; the
// insert and the notification commit atomically, and receivers read the row
// back. A janitor deletes spill rows older than Options.SpillTTL. The table is
// created on startup in the first schema of the connection's search_path.
//
// A dedicated connection runs LISTEN; when it fails, it is re-established with
// exponential backoff (notifications sent meanwhile are lost, matching the
// at-most-once contract). Idle listener connections are health-checked
// periodically. Publishing uses a connection pool.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thasami/backyard/internal/pubsub"
	"github.com/thasami/backyard/internal/pubsub/internal/fanout"
)

// SpillTable is the name of the table holding payloads too large for NOTIFY.
const SpillTable = "backyard_pubsub_spill"

// Defaults for Options.
const (
	DefaultChannel         = "backyard_events"
	DefaultSpillTTL        = 5 * time.Minute
	DefaultJanitorInterval = time.Minute
	DefaultHealthInterval  = 30 * time.Second
)

const (
	// maxNotifyBytes is the largest envelope sent inline. PostgreSQL rejects
	// NOTIFY payloads of 8000 bytes or more (default build).
	maxNotifyBytes = 7900
	opTimeout      = 10 * time.Second
	minBackoff     = 100 * time.Millisecond
	maxBackoff     = 10 * time.Second
	// schemaLockKey is the advisory lock serializing concurrent table creation.
	schemaLockKey int64 = 0x6279_7073_7062 // "byps"+"pb"
)

// Options configure a Broker.
type Options struct {
	// DSN is a PostgreSQL connection string (URL or key=value form).
	DSN string
	// Channel is the NOTIFY channel shared by all topics (default
	// "backyard_events"; at most 63 bytes).
	Channel string
	// Pool, if set, is used for publishing and spill reads instead of a pool
	// opened from DSN. It is not closed by Broker.Close. DSN is still required
	// for the dedicated LISTEN connection.
	Pool *pgxpool.Pool
	// SpillTTL is how long spilled payloads are kept (default 5m).
	SpillTTL time.Duration
	// JanitorInterval is how often expired spill rows are deleted (default 1m).
	JanitorInterval time.Duration
	// HealthInterval is how long the listener may sit idle before its
	// connection is pinged (default 30s).
	HealthInterval time.Duration
	// Logger receives operational logs (default slog.Default()).
	Logger *slog.Logger
	// QueueLen and QueueBytes bound the undelivered messages per local
	// subscription (defaults: 10000 messages, 64 MiB).
	QueueLen   int
	QueueBytes int
}

// Broker is a PostgreSQL LISTEN/NOTIFY pubsub.Broker.
type Broker struct {
	pool      *pgxpool.Pool
	ownsPool  bool
	listenCfg *pgx.ConnConfig
	channel   string
	listenSQL string
	opts      Options
	log       *slog.Logger
	reg       *fanout.Registry

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	closed    atomic.Bool
	closeOnce sync.Once
	// reconnects counts listener reconnections (observability and tests).
	reconnects atomic.Int64
}

var _ pubsub.Broker = (*Broker)(nil)

type envelope struct {
	Topic   string `json:"topic"`
	Payload []byte `json:"payload,omitempty"`
	SpillID *int64 `json:"spill_id,omitempty"`
}

// New connects to PostgreSQL, creates the spill table if needed and starts
// listening. It fails if the first LISTEN connection cannot be established.
func New(ctx context.Context, opts Options) (*Broker, error) {
	if opts.DSN == "" {
		return nil, errors.New("pubsub/postgres: dsn is required")
	}
	if opts.Channel == "" {
		opts.Channel = DefaultChannel
	}
	if len(opts.Channel) > 63 {
		return nil, fmt.Errorf("pubsub/postgres: channel name %q longer than 63 bytes", opts.Channel)
	}
	if opts.SpillTTL <= 0 {
		opts.SpillTTL = DefaultSpillTTL
	}
	if opts.JanitorInterval <= 0 {
		opts.JanitorInterval = DefaultJanitorInterval
	}
	if opts.HealthInterval <= 0 {
		opts.HealthInterval = DefaultHealthInterval
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	cfg, err := pgx.ParseConfig(opts.DSN)
	if err != nil {
		// pgx parse errors do not include the password.
		return nil, fmt.Errorf("pubsub/postgres: parse dsn: %w", err)
	}
	if _, ok := cfg.RuntimeParams["application_name"]; !ok {
		cfg.RuntimeParams["application_name"] = "backyard-pubsub"
	}

	b := &Broker{
		pool:      opts.Pool,
		listenCfg: cfg,
		channel:   opts.Channel,
		listenSQL: "LISTEN " + pgx.Identifier{opts.Channel}.Sanitize(),
		opts:      opts,
		log:       opts.Logger.With("component", "pubsub", "backend", "postgres", "channel", opts.Channel),
		reg: fanout.New(fanout.Options{
			QueueLen: opts.QueueLen, QueueBytes: opts.QueueBytes, Logger: opts.Logger,
		}),
	}
	if b.pool == nil {
		b.pool, err = pgxpool.New(ctx, opts.DSN)
		if err != nil {
			return nil, fmt.Errorf("pubsub/postgres: open pool: %w", err)
		}
		b.ownsPool = true
	}
	fail := func(err error) (*Broker, error) {
		if b.ownsPool {
			b.pool.Close()
		}
		return nil, err
	}
	if err := b.ensureSchema(ctx); err != nil {
		return fail(err)
	}
	conn, err := b.connect(ctx)
	if err != nil {
		return fail(err)
	}

	b.ctx, b.cancel = context.WithCancel(context.Background())
	b.wg.Add(2)
	go b.listen(conn)
	go b.janitor()
	return b, nil
}

func (b *Broker) ensureSchema(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("pubsub/postgres: create spill table: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	// Concurrent CREATE TABLE IF NOT EXISTS can still collide on the catalog;
	// serialize instances starting at the same time.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, schemaLockKey); err != nil {
		return fmt.Errorf("pubsub/postgres: create spill table: %w", err)
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS `+SpillTable+` (
	id         bigserial PRIMARY KEY,
	payload    bytea NOT NULL,
	created_at timestamptz NOT NULL DEFAULT now()
)`); err != nil {
		return fmt.Errorf("pubsub/postgres: create spill table: %w", err)
	}
	if _, err := tx.Exec(ctx, `CREATE INDEX IF NOT EXISTS `+SpillTable+`_created_at_idx ON `+SpillTable+` (created_at)`); err != nil {
		return fmt.Errorf("pubsub/postgres: create spill index: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("pubsub/postgres: create spill table: %w", err)
	}
	return nil
}

// connect opens the dedicated listener connection and issues LISTEN.
func (b *Broker) connect(ctx context.Context) (*pgx.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, b.listenCfg)
	if err != nil {
		return nil, fmt.Errorf("pubsub/postgres: connect listener: %w", err)
	}
	if _, err := conn.Exec(ctx, b.listenSQL); err != nil {
		_ = conn.Close(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("pubsub/postgres: listen: %w", err)
	}
	return conn, nil
}

// listen owns the listener connection: it receives notifications and
// reconnects with exponential backoff when the connection fails.
func (b *Broker) listen(conn *pgx.Conn) {
	defer b.wg.Done()
	backoff := minBackoff
	for {
		if conn == nil {
			c, err := b.connect(b.ctx)
			if err != nil {
				if b.ctx.Err() != nil {
					return
				}
				b.log.Warn("listener reconnect failed", "err", err, "retry_in", backoff)
				if !b.sleep(jitter(backoff)) {
					return
				}
				backoff = min(2*backoff, maxBackoff)
				continue
			}
			conn = c
			backoff = minBackoff
			b.reconnects.Add(1)
			b.log.Info("listener reconnected")
		}
		err := b.receive(conn)
		closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = conn.Close(closeCtx)
		cancel()
		conn = nil
		if b.ctx.Err() != nil {
			return
		}
		b.log.Warn("listener connection lost; reconnecting", "err", err)
		if !b.sleep(jitter(minBackoff)) {
			return
		}
	}
}

// receive processes notifications until the connection fails or the broker
// closes.
func (b *Broker) receive(conn *pgx.Conn) error {
	for {
		wctx, cancel := context.WithTimeout(b.ctx, b.opts.HealthInterval)
		n, err := conn.WaitForNotification(wctx)
		cancel()
		if err != nil {
			if b.ctx.Err() != nil {
				return b.ctx.Err()
			}
			if errors.Is(err, context.DeadlineExceeded) && !conn.IsClosed() {
				// Idle: make sure the connection is still alive.
				pctx, cancel := context.WithTimeout(b.ctx, opTimeout)
				err := conn.Ping(pctx)
				cancel()
				if err != nil {
					return fmt.Errorf("health check: %w", err)
				}
				continue
			}
			return err
		}
		if n.Channel == b.channel {
			b.handle(n.Payload)
		}
	}
}

func (b *Broker) handle(raw string) {
	var env envelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		b.log.Warn("ignoring malformed notification", "err", err, "bytes", len(raw))
		return
	}
	if !b.reg.Has(env.Topic) {
		return
	}
	payload := env.Payload
	if env.SpillID != nil {
		ctx, cancel := context.WithTimeout(b.ctx, opTimeout)
		err := b.pool.QueryRow(ctx, `SELECT payload FROM `+SpillTable+` WHERE id = $1`, *env.SpillID).Scan(&payload)
		cancel()
		if err != nil {
			b.log.Warn("dropping spilled message", "topic", env.Topic, "spill_id", *env.SpillID, "err", err)
			return
		}
	}
	if payload == nil {
		payload = []byte{}
	}
	b.reg.Dispatch(env.Topic, payload)
}

// Publish sends payload to every subscriber of topic on all instances.
// Messages from one publisher are delivered in the order of their Publish
// calls (each NOTIFY commits before the next call starts).
func (b *Broker) Publish(ctx context.Context, topic string, payload []byte) error {
	if b.closed.Load() {
		return pubsub.ErrClosed
	}
	if err := pubsub.ValidatePublish(topic, payload); err != nil {
		return err
	}
	data, err := json.Marshal(envelope{Topic: topic, Payload: payload})
	if err != nil {
		return fmt.Errorf("pubsub/postgres: encode: %w", err)
	}
	if len(data) <= maxNotifyBytes {
		_, err = b.pool.Exec(ctx, `SELECT pg_notify($1, $2)`, b.channel, string(data))
	} else {
		// Insert and notify in one statement (one transaction): receivers are
		// notified only after the row is committed and visible.
		_, err = b.pool.Exec(ctx, `WITH s AS (
	INSERT INTO `+SpillTable+` (payload) VALUES ($2) RETURNING id
)
SELECT pg_notify($1, json_build_object('topic', $3::text, 'spill_id', s.id)::text) FROM s`,
			b.channel, payload, topic)
	}
	if err != nil {
		return fmt.Errorf("pubsub/postgres: publish %q: %w", topic, err)
	}
	return nil
}

// Subscribe registers h for topic until cancel is called. Delivery starts
// immediately: the broker listens on its channel for its whole lifetime.
func (b *Broker) Subscribe(ctx context.Context, topic string, h pubsub.Handler) (func(), error) {
	if b.closed.Load() {
		return nil, pubsub.ErrClosed
	}
	if h == nil {
		return nil, pubsub.ErrNilHandler
	}
	if err := pubsub.ValidateTopic(topic); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s, _, err := b.reg.Add(topic, h)
	if err != nil {
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(func() { b.reg.Remove(s) }) }, nil
}

func (b *Broker) janitor() {
	defer b.wg.Done()
	t := time.NewTicker(b.opts.JanitorInterval)
	defer t.Stop()
	for {
		b.purgeSpill()
		select {
		case <-b.ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (b *Broker) purgeSpill() {
	ctx, cancel := context.WithTimeout(b.ctx, opTimeout)
	defer cancel()
	tag, err := b.pool.Exec(ctx,
		`DELETE FROM `+SpillTable+` WHERE created_at < now() - make_interval(secs => $1)`,
		b.opts.SpillTTL.Seconds())
	if err != nil {
		if b.ctx.Err() == nil {
			b.log.Warn("spill janitor failed", "err", err)
		}
		return
	}
	if n := tag.RowsAffected(); n > 0 {
		b.log.Debug("spill janitor deleted rows", "rows", n)
	}
}

// Close stops listening, stops all subscriptions and closes the pool (when
// the broker opened it). It is idempotent.
func (b *Broker) Close() error {
	b.closeOnce.Do(func() {
		b.closed.Store(true)
		b.cancel()
		b.wg.Wait()
		b.reg.Close()
		if b.ownsPool {
			b.pool.Close()
		}
	})
	return nil
}

func (b *Broker) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-b.ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// jitter returns d randomized to [d/2, d).
func jitter(d time.Duration) time.Duration {
	half := d / 2
	if half <= 0 {
		return d
	}
	return half + rand.N(half)
}
