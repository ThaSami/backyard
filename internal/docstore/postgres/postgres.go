// Package postgres is the docstore.Store for PostgreSQL (Cloud SQL, RDS,
// Aurora, Azure Database for PostgreSQL Flexible Server, self-hosted…) using
// pgx/v5 and JSONB.
//
// Documents live in <schema>.backyard_docs:
//
//	site, collection, id  text COLLATE "C"   (primary key; byte-order sorting)
//	data                  jsonb
//	created_at, updated_at timestamptz       (microsecond precision)
//	created_by, updated_by text COLLATE "C"
//	version               bigint
//
// with an index on (site, collection, created_at, id) for the default listing
// order. The schema is created and migrated on start when auto_migrate is set;
// migrations are serialised across instances with pg_advisory_lock and
// recorded in <schema>.backyard_schema_version (component "docstore").
//
// Filters, ordering and pagination run in SQL (see query.go) with the
// type-strict semantics of SPEC §5.2; user input is always bound as
// parameters. Update is a single atomic statement:
// data = (data || patch_without_nulls) - keys_to_delete.
//
// Limitation: PostgreSQL cannot store the NUL character (\u0000) in jsonb
// strings; such documents are rejected with docstore.ErrInvalid.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/docstore"
	"github.com/thasami/backyard/internal/docstore/internal/dsutil"
)

const (
	// component is the row key in the schema_version table.
	component = "docstore"
	// connectTimeout bounds the initial connection check in Open.
	connectTimeout = 30 * time.Second
	tableName      = "backyard_docs"
	versionTable   = "backyard_schema_version"
)

// migrations are applied in order; each entry is one schema version. {{t}} is
// replaced by the qualified, quoted table name.
var migrations = [][]string{
	{
		`CREATE TABLE IF NOT EXISTS {{t}} (
			site       text COLLATE "C" NOT NULL,
			collection text COLLATE "C" NOT NULL,
			id         text COLLATE "C" NOT NULL,
			data       jsonb NOT NULL,
			created_at timestamptz NOT NULL,
			updated_at timestamptz NOT NULL,
			created_by text COLLATE "C" NOT NULL DEFAULT '',
			updated_by text COLLATE "C" NOT NULL DEFAULT '',
			version    bigint NOT NULL DEFAULT 1,
			PRIMARY KEY (site, collection, id)
		)`,
		`CREATE INDEX IF NOT EXISTS backyard_docs_created_idx ON {{t}} (site, collection, created_at, id)`,
	},
}

// Store is a PostgreSQL-backed document store.
type Store struct {
	pool     *pgxpool.Pool
	ownsPool bool
	schema   string
	table    string // quoted, schema-qualified table name
}

var _ docstore.Store = (*Store)(nil)

// Open connects to cfg.DSN (pool size cfg.MaxConns) and prepares the schema:
// with cfg.AutoMigrate (default true) the schema and tables are created or
// migrated, otherwise Open fails if they are missing or out of date.
func Open(ctx context.Context, cfg config.PostgresConfig) (*Store, error) {
	if cfg.DSN == "" {
		return nil, errors.New("docstore/postgres: database.postgres.dsn is required")
	}
	pc, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		// The error text may contain the DSN; do not wrap it.
		return nil, errors.New("docstore/postgres: invalid DSN")
	}
	if cfg.MaxConns > 0 {
		pc.MaxConns = int32(min(cfg.MaxConns, 1<<20))
	}
	cctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(cctx, pc)
	if err != nil {
		return nil, fmt.Errorf("docstore/postgres: connect: %w", err)
	}
	if err := pool.Ping(cctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("docstore/postgres: connect: %w", err)
	}
	autoMigrate := cfg.AutoMigrate == nil || *cfg.AutoMigrate
	s, err := New(ctx, pool, cfg.Schema, autoMigrate)
	if err != nil {
		pool.Close()
		return nil, err
	}
	s.ownsPool = true
	return s, nil
}

// New builds a store on an existing pool (which Close leaves open) in schema
// (default "public"), migrating it when autoMigrate is true.
func New(ctx context.Context, pool *pgxpool.Pool, schema string, autoMigrate bool) (*Store, error) {
	if schema == "" {
		schema = "public"
	}
	s := &Store{
		pool:   pool,
		schema: schema,
		table:  pgx.Identifier{schema, tableName}.Sanitize(),
	}
	if err := s.migrate(ctx, autoMigrate); err != nil {
		return nil, err
	}
	slog.Debug("docstore/postgres: ready", "schema", schema)
	return s, nil
}

// lockKey derives the advisory lock id for migrations of this schema.
func (s *Store) lockKey() int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("backyard/docstore:" + s.schema))
	return int64(h.Sum64())
}

func (s *Store) migrate(ctx context.Context, autoMigrate bool) (err error) {
	vt := pgx.Identifier{s.schema, versionTable}.Sanitize()
	if !autoMigrate {
		var v int
		err := s.pool.QueryRow(ctx, `SELECT version FROM `+vt+` WHERE component = $1`, component).Scan(&v)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) && !isUndefinedTable(err) {
			return fmt.Errorf("docstore/postgres: reading schema version: %w", err)
		}
		if v < len(migrations) {
			return fmt.Errorf("docstore/postgres: schema %q is at version %d, version %d required; "+
				"enable database.postgres.auto_migrate or run backyard once with it enabled", s.schema, v, len(migrations))
		}
		return nil
	}

	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("docstore/postgres: migrate: %w", err)
	}
	defer conn.Release()
	key := s.lockKey()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, key); err != nil {
		return fmt.Errorf("docstore/postgres: migrate: acquiring advisory lock: %w", err)
	}
	defer func() {
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if _, uerr := conn.Exec(uctx, `SELECT pg_advisory_unlock($1)`, key); uerr != nil {
			// Closing the session releases the lock; never return a
			// connection that still holds it to the pool.
			_ = conn.Conn().Close(uctx)
			slog.Warn("docstore/postgres: releasing migration lock failed; connection closed", "err", uerr)
		}
	}()

	var exists bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)`, s.schema).Scan(&exists); err != nil {
		return fmt.Errorf("docstore/postgres: migrate: %w", err)
	}
	if !exists {
		if _, err := conn.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS `+pgx.Identifier{s.schema}.Sanitize()); err != nil {
			return fmt.Errorf("docstore/postgres: creating schema %q: %w", s.schema, err)
		}
	}
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS `+vt+` (
		component  text PRIMARY KEY,
		version    integer NOT NULL,
		updated_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return fmt.Errorf("docstore/postgres: creating %s: %w", versionTable, err)
	}
	var v int
	err = conn.QueryRow(ctx, `SELECT version FROM `+vt+` WHERE component = $1`, component).Scan(&v)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("docstore/postgres: reading schema version: %w", err)
	}
	if v > len(migrations) {
		slog.Warn("docstore/postgres: database schema is newer than this binary", "schema", s.schema, "version", v, "supported", len(migrations))
		return nil
	}
	for i := v; i < len(migrations); i++ {
		if err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			for _, stmt := range migrations[i] {
				if _, err := tx.Exec(ctx, strings.ReplaceAll(stmt, "{{t}}", s.table)); err != nil {
					return err
				}
			}
			_, err := tx.Exec(ctx, `INSERT INTO `+vt+` (component, version) VALUES ($1, $2)
				ON CONFLICT (component) DO UPDATE SET version = EXCLUDED.version, updated_at = now()`, component, i+1)
			return err
		}); err != nil {
			return fmt.Errorf("docstore/postgres: migration %d: %w", i+1, err)
		}
		slog.Info("docstore/postgres: applied migration", "schema", s.schema, "version", i+1)
	}
	return nil
}

func isUndefinedTable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42P01"
}

// mapErr wraps a database error, translating data errors into docstore errors.
func mapErr(op string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "22P05", "22021", "22P02", "22025":
			// untranslatable character (e.g. \u0000 in jsonb), bad encoding,
			// invalid text representation, invalid escape sequence.
			return fmt.Errorf("%w: %s (%s)", docstore.ErrInvalid, pgErr.Message, op)
		case "54000":
			return fmt.Errorf("%w: %s (%s)", docstore.ErrTooLarge, pgErr.Message, op)
		}
	}
	return fmt.Errorf("docstore/postgres: %s: %w", op, err)
}

const docColumns = `id, data, created_at, updated_at, created_by, updated_by, version`

func scanDoc(row pgx.Row, site, collection string) (*docstore.Doc, error) {
	d := &docstore.Doc{Site: site, Collection: collection}
	var data []byte
	if err := row.Scan(&d.ID, &data, &d.CreatedAt, &d.UpdatedAt, &d.CreatedBy, &d.UpdatedBy, &d.Version); err != nil {
		return nil, err
	}
	m, err := dsutil.DecodeData(data)
	if err != nil {
		return nil, err
	}
	d.Data = m
	d.CreatedAt = d.CreatedAt.UTC()
	d.UpdatedAt = d.UpdatedAt.UTC()
	return d, nil
}

// Create inserts a new document; see docstore.Store.
func (s *Store) Create(ctx context.Context, site, collection, id string, data map[string]any, actor string) (*docstore.Doc, error) {
	if id == "" {
		id = docstore.NewID()
	}
	actor, err := dsutil.CheckWrite(site, collection, id, actor)
	if err != nil {
		return nil, err
	}
	body, raw, err := dsutil.PrepareWrite(data)
	if err != nil {
		return nil, err
	}
	now := docstore.Now()
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO `+s.table+` (site, collection, id, data, created_at, updated_at, created_by, updated_by, version)
		 VALUES ($1, $2, $3, $4::jsonb, $5, $5, $6, $6, 1)
		 ON CONFLICT (site, collection, id) DO NOTHING`,
		site, collection, id, string(raw), now, actor)
	if err != nil {
		return nil, mapErr("create", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, fmt.Errorf("%w: document %q already exists in %s", docstore.ErrConflict, id, collection)
	}
	return &docstore.Doc{
		Site: site, Collection: collection, ID: id, Data: body,
		CreatedAt: now, UpdatedAt: now, CreatedBy: actor, UpdatedBy: actor, Version: 1,
	}, nil
}

// Get returns a document; see docstore.Store.
func (s *Store) Get(ctx context.Context, site, collection, id string) (*docstore.Doc, error) {
	if err := dsutil.CheckKey(site, collection, id); err != nil {
		return nil, err
	}
	row := s.pool.QueryRow(ctx,
		`SELECT `+docColumns+` FROM `+s.table+` WHERE site = $1 AND collection = $2 AND id = $3`,
		site, collection, id)
	d, err := scanDoc(row, site, collection)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, docstore.ErrNotFound
	}
	if err != nil {
		return nil, mapErr("get", err)
	}
	return d, nil
}

// Put creates or replaces a document in one statement; see docstore.Store.
func (s *Store) Put(ctx context.Context, site, collection, id string, data map[string]any, actor string) (*docstore.Doc, error) {
	actor, err := dsutil.CheckWrite(site, collection, id, actor)
	if err != nil {
		return nil, err
	}
	body, raw, err := dsutil.PrepareWrite(data)
	if err != nil {
		return nil, err
	}
	now := docstore.Now()
	d := &docstore.Doc{Site: site, Collection: collection, ID: id, Data: body, UpdatedAt: now, UpdatedBy: actor}
	err = s.pool.QueryRow(ctx,
		`INSERT INTO `+s.table+` AS d (site, collection, id, data, created_at, updated_at, created_by, updated_by, version)
		 VALUES ($1, $2, $3, $4::jsonb, $5, $5, $6, $6, 1)
		 ON CONFLICT (site, collection, id) DO UPDATE SET
		   data = EXCLUDED.data,
		   updated_at = EXCLUDED.updated_at,
		   updated_by = EXCLUDED.updated_by,
		   version = d.version + 1
		 RETURNING created_at, created_by, version`,
		site, collection, id, string(raw), now, actor,
	).Scan(&d.CreatedAt, &d.CreatedBy, &d.Version)
	if err != nil {
		return nil, mapErr("put", err)
	}
	d.CreatedAt = d.CreatedAt.UTC()
	return d, nil
}

// Update shallow-merges patch into an existing document in a single atomic
// statement; see docstore.Store.
func (s *Store) Update(ctx context.Context, site, collection, id string, patch map[string]any, actor string) (*docstore.Doc, error) {
	return s.update(ctx, site, collection, id, nil, patch, actor)
}

// UpdateIf is Update while the document is at rev, checked by the same
// statement; see docstore.Store.
func (s *Store) UpdateIf(ctx context.Context, site, collection, id string, rev docstore.Rev, patch map[string]any, actor string) (*docstore.Doc, error) {
	return s.update(ctx, site, collection, id, &rev, patch, actor)
}

// update merges patch, if rev is set only into the document at rev.
func (s *Store) update(ctx context.Context, site, collection, id string, rev *docstore.Rev, patch map[string]any, actor string) (*docstore.Doc, error) {
	actor, err := dsutil.CheckWrite(site, collection, id, actor)
	if err != nil {
		return nil, err
	}
	p, err := dsutil.PreparePatch(patch)
	if err != nil {
		return nil, err
	}
	set := make(map[string]any, len(p))
	del := []string{}
	for k, v := range p {
		if v == nil {
			del = append(del, k)
		} else {
			set[k] = v
		}
	}
	raw, err := dsutil.Marshal(set)
	if err != nil {
		return nil, err
	}
	now := docstore.Now()
	stmt := `UPDATE ` + s.table + ` AS d SET
		   data = (d.data || $4::jsonb) - $5::text[],
		   updated_at = $6,
		   updated_by = $7,
		   version = d.version + 1
		 WHERE site = $1 AND collection = $2 AND id = $3`
	args := []any{site, collection, id, string(raw), del, now, actor}
	if rev != nil {
		stmt += ` AND d.version = $8 AND d.created_at = $9`
		args = append(args, rev.Version, docstore.TruncTime(rev.CreatedAt))
	}
	row := s.pool.QueryRow(ctx, stmt+` RETURNING `+docColumns, args...)
	d, err := scanDoc(row, site, collection)
	if errors.Is(err, pgx.ErrNoRows) {
		if rev != nil {
			// Missing, or at another revision: tell them apart.
			var one int
			err := s.pool.QueryRow(ctx, `SELECT 1 FROM `+s.table+` WHERE site = $1 AND collection = $2 AND id = $3`,
				site, collection, id).Scan(&one)
			switch {
			case err == nil:
				return nil, fmt.Errorf("%w: document %q has changed", docstore.ErrConflict, id)
			case !errors.Is(err, pgx.ErrNoRows):
				return nil, mapErr("update", err)
			}
		}
		return nil, docstore.ErrNotFound
	}
	if err != nil {
		return nil, mapErr("update", err)
	}
	return d, nil
}

// Delete removes a document; see docstore.Store.
func (s *Store) Delete(ctx context.Context, site, collection, id string) (bool, error) {
	if err := dsutil.CheckKey(site, collection, id); err != nil {
		return false, err
	}
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM `+s.table+` WHERE site = $1 AND collection = $2 AND id = $3`, site, collection, id)
	if err != nil {
		return false, mapErr("delete", err)
	}
	return tag.RowsAffected() > 0, nil
}

// DeleteIf removes a document while it is at rev, in one statement; see
// docstore.Store.
func (s *Store) DeleteIf(ctx context.Context, site, collection, id string, rev docstore.Rev) (bool, error) {
	if err := dsutil.CheckKey(site, collection, id); err != nil {
		return false, err
	}
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM `+s.table+` WHERE site = $1 AND collection = $2 AND id = $3 AND version = $4 AND created_at = $5`,
		site, collection, id, rev.Version, docstore.TruncTime(rev.CreatedAt))
	if err != nil {
		return false, mapErr("delete", err)
	}
	return tag.RowsAffected() > 0, nil
}

// Query returns matching documents; see docstore.Store.
func (s *Store) Query(ctx context.Context, site, collection string, q docstore.Query) (*docstore.Page, error) {
	if err := dsutil.CheckNamespace(site, collection); err != nil {
		return nil, err
	}
	q, err := docstore.PrepareQuery(q)
	if err != nil {
		return nil, err
	}
	stmt, args, err := buildQuery(s.table, site, collection, q)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, stmt, args...)
	if err != nil {
		return nil, mapErr("query", err)
	}
	defer rows.Close()
	page := &docstore.Page{Docs: make([]*docstore.Doc, 0, min(q.Limit, 128))}
	for rows.Next() {
		if len(page.Docs) == q.Limit {
			// buildQuery fetches one row past the limit to detect more.
			page.HasMore = true
			break
		}
		d, err := scanDoc(rows, site, collection)
		if err != nil {
			return nil, mapErr("query", err)
		}
		page.Docs = append(page.Docs, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, mapErr("query", err)
	}
	return page, nil
}

// Collections lists the non-empty collections of a site in byte order, using
// a loose index scan over the primary key.
func (s *Store) Collections(ctx context.Context, site string) ([]string, error) {
	if err := docstore.ValidateSite(site); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		WITH RECURSIVE c AS (
			(SELECT collection FROM `+s.table+` WHERE site = $1 ORDER BY collection LIMIT 1)
			UNION ALL
			SELECT (SELECT t.collection FROM `+s.table+` t
			        WHERE t.site = $1 AND t.collection > c.collection
			        ORDER BY t.collection LIMIT 1)
			FROM c WHERE c.collection IS NOT NULL
		)
		SELECT collection FROM c WHERE collection IS NOT NULL`, site)
	if err != nil {
		return nil, mapErr("collections", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, mapErr("collections", err)
	}
	if out == nil {
		out = []string{}
	}
	return out, nil
}

// Count returns the number of documents of a site.
func (s *Store) Count(ctx context.Context, site string) (int64, error) {
	if err := docstore.ValidateSite(site); err != nil {
		return 0, err
	}
	var n int64
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM `+s.table+` WHERE site = $1`, site).Scan(&n); err != nil {
		return 0, mapErr("count", err)
	}
	return n, nil
}

// DeleteSite removes every document of a site.
func (s *Store) DeleteSite(ctx context.Context, site string) error {
	if err := docstore.ValidateSite(site); err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM `+s.table+` WHERE site = $1`, site); err != nil {
		return mapErr("delete site", err)
	}
	return nil
}

// Ping checks connectivity.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.pool.Ping(ctx); err != nil {
		return fmt.Errorf("docstore/postgres: ping: %w", err)
	}
	return nil
}

// Close closes the pool if Open created it.
func (s *Store) Close() error {
	if s.ownsPool {
		s.pool.Close()
	}
	return nil
}
