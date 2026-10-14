// Package sqlite is the embedded, single-node docstore.Store backed by SQLite
// (modernc.org/sqlite, pure Go, driver name "sqlite").
//
// Documents live in one table:
//
//	docs(site, collection, id, data JSON text, created_at, updated_at,
//	     created_by, updated_by, version, PRIMARY KEY (site, collection, id))
//
// with timestamps stored as sortable integers (unix nanoseconds, always whole
// microseconds) and an index on (site, collection, created_at, id) for the
// default listing order. Filters, ordering and pagination are pushed into SQL
// using json_extract/json_type so that the type-strict semantics of SPEC §5.2
// hold exactly (see query.go).
//
// The database runs in WAL mode. Writes go through a dedicated single
// connection (SQLite allows one writer at a time; serialising in-process avoids
// SQLITE_BUSY churn) using BEGIN IMMEDIATE transactions, while reads use a
// separate read-only pool and proceed concurrently. busy_timeout covers other
// processes sharing the file.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver

	"github.com/thasami/backyard/internal/docstore"
	"github.com/thasami/backyard/internal/docstore/internal/dsutil"
)

// busyTimeout is how long SQLite waits for a lock held by another connection
// or process before failing with SQLITE_BUSY.
const busyTimeout = 10 * time.Second

// schemaVersion is the current schema version, stored in PRAGMA user_version.
const schemaVersion = 1

var migrations = []string{
	// 1: documents table and creation-order index.
	`CREATE TABLE IF NOT EXISTS docs (
		site       TEXT    NOT NULL,
		collection TEXT    NOT NULL,
		id         TEXT    NOT NULL,
		data       TEXT    NOT NULL,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL,
		created_by TEXT    NOT NULL DEFAULT '',
		updated_by TEXT    NOT NULL DEFAULT '',
		version    INTEGER NOT NULL DEFAULT 1,
		PRIMARY KEY (site, collection, id)
	);
	CREATE INDEX IF NOT EXISTS docs_site_coll_created ON docs (site, collection, created_at, id);`,
}

// Store is a SQLite-backed document store.
type Store struct {
	w    *sql.DB // single writer connection
	r    *sql.DB // concurrent readers (same as w for in-memory databases)
	path string
}

var _ docstore.Store = (*Store)(nil)

// Open opens (creating if needed, with mode 0600) the database file at path
// and applies migrations. The parent directory must exist. path ":memory:"
// opens a private in-memory database (one connection; for tests).
func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("docstore/sqlite: empty database path")
	}
	memory := path == ":memory:"
	if strings.ContainsAny(path, "?#") && !memory {
		return nil, fmt.Errorf("docstore/sqlite: database path %q must not contain '?' or '#'", path)
	}
	if !memory && !strings.HasPrefix(path, "file:") {
		// Create a new database file readable by its owner only; SQLite gives
		// the -wal and -shm files the same permissions.
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		switch {
		case err == nil:
			_ = f.Close()
		case !errors.Is(err, fs.ErrExist):
			return nil, fmt.Errorf("docstore/sqlite: create %s: %w", path, err)
		}
	}
	params := url.Values{}
	params.Set("_busy_timeout", fmt.Sprint(busyTimeout.Milliseconds()))
	params.Set("_foreign_keys", "1")
	params.Set("_txlock", "immediate")
	if !memory {
		params.Set("_journal_mode", "WAL")
		params.Set("_synchronous", "NORMAL")
	}
	w, err := sql.Open("sqlite", path+"?"+params.Encode())
	if err != nil {
		return nil, fmt.Errorf("docstore/sqlite: open %s: %w", path, err)
	}
	w.SetMaxOpenConns(1)
	w.SetMaxIdleConns(1)
	w.SetConnMaxLifetime(0)
	w.SetConnMaxIdleTime(0)
	s := &Store{w: w, r: w, path: path}
	if err := s.migrate(ctx); err != nil {
		_ = w.Close()
		return nil, err
	}
	if !memory {
		rp := url.Values{}
		rp.Set("_busy_timeout", fmt.Sprint(busyTimeout.Milliseconds()))
		rp.Set("_query_only", "1")
		r, err := sql.Open("sqlite", path+"?"+rp.Encode())
		if err != nil {
			_ = w.Close()
			return nil, fmt.Errorf("docstore/sqlite: open %s for reading: %w", path, err)
		}
		n := max(4, runtime.GOMAXPROCS(0))
		r.SetMaxOpenConns(n)
		r.SetMaxIdleConns(n)
		r.SetConnMaxIdleTime(5 * time.Minute)
		s.r = r
	}
	slog.Debug("docstore/sqlite: opened", "path", path)
	return s, nil
}

func (s *Store) migrate(ctx context.Context) error {
	conn, err := s.w.Conn(ctx)
	if err != nil {
		return fmt.Errorf("docstore/sqlite: connect: %w", err)
	}
	defer conn.Close()
	var v int
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return fmt.Errorf("docstore/sqlite: read schema version: %w", err)
	}
	if v > schemaVersion {
		slog.Warn("docstore/sqlite: database schema is newer than this binary", "path", s.path, "version", v, "supported", schemaVersion)
		return nil
	}
	for i := v; i < len(migrations); i++ {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("docstore/sqlite: migrate: %w", err)
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("docstore/sqlite: migration %d: %w", i+1, err)
		}
		// PRAGMA does not take bound parameters; i+1 is a trusted integer.
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("docstore/sqlite: migration %d: %w", i+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("docstore/sqlite: migration %d: %w", i+1, err)
		}
	}
	return nil
}

const docColumns = `id, data, created_at, updated_at, created_by, updated_by, version`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanDoc(row rowScanner, site, collection string) (*docstore.Doc, error) {
	var (
		d              = &docstore.Doc{Site: site, Collection: collection}
		data           []byte
		created, updat int64
	)
	if err := row.Scan(&d.ID, &data, &created, &updat, &d.CreatedBy, &d.UpdatedBy, &d.Version); err != nil {
		return nil, err
	}
	m, err := dsutil.DecodeData(data)
	if err != nil {
		return nil, err
	}
	d.Data = m
	d.CreatedAt = fromNanos(created)
	d.UpdatedAt = fromNanos(updat)
	return d, nil
}

func toNanos(t time.Time) int64 { return t.UnixNano() }

func fromNanos(n int64) time.Time { return time.Unix(0, n).UTC() }

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
	res, err := s.w.ExecContext(ctx,
		`INSERT INTO docs (site, collection, id, data, created_at, updated_at, created_by, updated_by, version)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1)
		 ON CONFLICT (site, collection, id) DO NOTHING`,
		site, collection, id, string(raw), toNanos(now), toNanos(now), actor, actor)
	if err != nil {
		return nil, fmt.Errorf("docstore/sqlite: create: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, fmt.Errorf("docstore/sqlite: create: %w", err)
	} else if n == 0 {
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
	row := s.r.QueryRowContext(ctx,
		`SELECT `+docColumns+` FROM docs WHERE site = ? AND collection = ? AND id = ?`,
		site, collection, id)
	d, err := scanDoc(row, site, collection)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, docstore.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("docstore/sqlite: get: %w", err)
	}
	return d, nil
}

// Put creates or replaces a document; see docstore.Store.
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
	var created int64
	err = s.w.QueryRowContext(ctx,
		`INSERT INTO docs (site, collection, id, data, created_at, updated_at, created_by, updated_by, version)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1)
		 ON CONFLICT (site, collection, id) DO UPDATE SET
		   data = excluded.data,
		   updated_at = excluded.updated_at,
		   updated_by = excluded.updated_by,
		   version = docs.version + 1
		 RETURNING created_at, created_by, version`,
		site, collection, id, string(raw), toNanos(now), toNanos(now), actor, actor,
	).Scan(&created, &d.CreatedBy, &d.Version)
	if err != nil {
		return nil, fmt.Errorf("docstore/sqlite: put: %w", err)
	}
	d.CreatedAt = fromNanos(created)
	return d, nil
}

// Update shallow-merges patch into an existing document inside one
// BEGIN IMMEDIATE transaction; see docstore.Store.
func (s *Store) Update(ctx context.Context, site, collection, id string, patch map[string]any, actor string) (*docstore.Doc, error) {
	return s.update(ctx, site, collection, id, nil, patch, actor)
}

// UpdateIf is Update while the document is at rev, checked in the same
// transaction; see docstore.Store.
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
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("docstore/sqlite: update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	row := tx.QueryRowContext(ctx,
		`SELECT `+docColumns+` FROM docs WHERE site = ? AND collection = ? AND id = ?`,
		site, collection, id)
	d, err := scanDoc(row, site, collection)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, docstore.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("docstore/sqlite: update: %w", err)
	}
	if rev != nil && !d.At(*rev) {
		return nil, fmt.Errorf("%w: document %q has changed", docstore.ErrConflict, id)
	}
	merged, raw, err := dsutil.MergePatch(d.Data, p)
	if err != nil {
		return nil, err
	}
	now := docstore.Now()
	if _, err := tx.ExecContext(ctx,
		`UPDATE docs SET data = ?, updated_at = ?, updated_by = ?, version = version + 1
		 WHERE site = ? AND collection = ? AND id = ?`,
		string(raw), toNanos(now), actor, site, collection, id); err != nil {
		return nil, fmt.Errorf("docstore/sqlite: update: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("docstore/sqlite: update: %w", err)
	}
	d.Data = merged
	d.UpdatedAt = now
	d.UpdatedBy = actor
	d.Version++
	return d, nil
}

// Delete removes a document; see docstore.Store.
func (s *Store) Delete(ctx context.Context, site, collection, id string) (bool, error) {
	if err := dsutil.CheckKey(site, collection, id); err != nil {
		return false, err
	}
	res, err := s.w.ExecContext(ctx, `DELETE FROM docs WHERE site = ? AND collection = ? AND id = ?`, site, collection, id)
	if err != nil {
		return false, fmt.Errorf("docstore/sqlite: delete: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("docstore/sqlite: delete: %w", err)
	}
	return n > 0, nil
}

// DeleteIf removes a document while it is at rev, in one statement; see
// docstore.Store.
func (s *Store) DeleteIf(ctx context.Context, site, collection, id string, rev docstore.Rev) (bool, error) {
	if err := dsutil.CheckKey(site, collection, id); err != nil {
		return false, err
	}
	res, err := s.w.ExecContext(ctx,
		`DELETE FROM docs WHERE site = ? AND collection = ? AND id = ? AND version = ? AND created_at = ?`,
		site, collection, id, rev.Version, toNanos(docstore.TruncTime(rev.CreatedAt)))
	if err != nil {
		return false, fmt.Errorf("docstore/sqlite: delete: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("docstore/sqlite: delete: %w", err)
	}
	return n > 0, nil
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
	stmt, args := buildQuery(site, collection, q)
	rows, err := s.r.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, fmt.Errorf("docstore/sqlite: query: %w", err)
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
			return nil, fmt.Errorf("docstore/sqlite: query: %w", err)
		}
		page.Docs = append(page.Docs, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("docstore/sqlite: query: %w", err)
	}
	return page, nil
}

// Collections lists the non-empty collections of a site, sorted bytewise.
func (s *Store) Collections(ctx context.Context, site string) ([]string, error) {
	if err := docstore.ValidateSite(site); err != nil {
		return nil, err
	}
	rows, err := s.r.QueryContext(ctx, `SELECT DISTINCT collection FROM docs WHERE site = ? ORDER BY collection`, site)
	if err != nil {
		return nil, fmt.Errorf("docstore/sqlite: collections: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, fmt.Errorf("docstore/sqlite: collections: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("docstore/sqlite: collections: %w", err)
	}
	return out, nil
}

// Count returns the number of documents of a site.
func (s *Store) Count(ctx context.Context, site string) (int64, error) {
	if err := docstore.ValidateSite(site); err != nil {
		return 0, err
	}
	var n int64
	if err := s.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM docs WHERE site = ?`, site).Scan(&n); err != nil {
		return 0, fmt.Errorf("docstore/sqlite: count: %w", err)
	}
	return n, nil
}

// DeleteSite removes every document of a site.
func (s *Store) DeleteSite(ctx context.Context, site string) error {
	if err := docstore.ValidateSite(site); err != nil {
		return err
	}
	if _, err := s.w.ExecContext(ctx, `DELETE FROM docs WHERE site = ?`, site); err != nil {
		return fmt.Errorf("docstore/sqlite: delete site: %w", err)
	}
	return nil
}

// Ping checks that the database answers.
func (s *Store) Ping(ctx context.Context) error {
	var one int
	if err := s.r.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
		return fmt.Errorf("docstore/sqlite: ping: %w", err)
	}
	return nil
}

// Close closes both connection pools.
func (s *Store) Close() error {
	var errs []error
	if s.r != s.w {
		errs = append(errs, s.r.Close())
	}
	errs = append(errs, s.w.Close())
	return errors.Join(errs...)
}
