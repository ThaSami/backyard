// Package docstore defines Backyard's schemaless JSON document database
// (backyard.db). Implementations live in sub-packages (memory, sqlite,
// postgres, dynamodb); backends.Open builds one from configuration and the
// conformance package holds the shared test-suite every backend must pass.
//
// Semantics (filters, ordering, missing fields) are specified in
// docs/spec/SPEC.md §5.2 and must be identical across backends.
package docstore

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

var (
	// ErrNotFound: document does not exist.
	ErrNotFound = errors.New("docstore: not found")
	// ErrConflict: a document with this id already exists (Create), or an
	// optimistic version check failed.
	ErrConflict = errors.New("docstore: conflict")
	// ErrTooLarge: document exceeds the configured size limit.
	ErrTooLarge = errors.New("docstore: document too large")
	// ErrQuota: per-site document quota exceeded.
	ErrQuota = errors.New("docstore: quota exceeded")
	// ErrInvalid: invalid collection, id, field path, operator or value.
	ErrInvalid = errors.New("docstore: invalid request")
)

// Doc is a stored document. Data never contains the keys "id" or "_meta".
type Doc struct {
	Site       string
	Collection string
	ID         string
	Data       map[string]any
	CreatedAt  time.Time
	UpdatedAt  time.Time
	CreatedBy  string
	UpdatedBy  string
	Version    int64
}

// Meta is the "_meta" object of the wire representation.
type Meta struct {
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	CreatedBy string    `json:"created_by"`
	UpdatedBy string    `json:"updated_by"`
	Version   int64     `json:"version"`
}

// MarshalJSON renders the flattened wire shape: {"id", ...data, "_meta": {...}}.
func (d *Doc) MarshalJSON() ([]byte, error) {
	m := make(map[string]any, len(d.Data)+2)
	for k, v := range d.Data {
		m[k] = v
	}
	m["id"] = d.ID
	m["_meta"] = Meta{
		CreatedAt: d.CreatedAt.UTC(), UpdatedAt: d.UpdatedAt.UTC(),
		CreatedBy: d.CreatedBy, UpdatedBy: d.UpdatedBy, Version: d.Version,
	}
	return json.Marshal(m)
}

// Op is a filter operator.
type Op string

// Supported operators.
const (
	OpEq       Op = "=="
	OpNe       Op = "!="
	OpLt       Op = "<"
	OpLte      Op = "<="
	OpGt       Op = ">"
	OpGte      Op = ">="
	OpIn       Op = "in"
	OpNotIn    Op = "not-in"
	OpContains Op = "contains"
)

// Filter is one where-clause.
type Filter struct {
	Field string `json:"field"`
	Op    Op     `json:"op"`
	Value any    `json:"value"`
}

// Order is one sort key.
type Order struct {
	Field string `json:"field"`
	Desc  bool   `json:"-"`
	Dir   string `json:"dir,omitempty"` // "asc" | "desc" (wire form; Normalize sets Desc)
}

// Query selects documents of one collection.
type Query struct {
	Where   []Filter `json:"where,omitempty"`
	OrderBy []Order  `json:"order_by,omitempty"`
	Limit   int      `json:"limit,omitempty"`
	Offset  int      `json:"offset,omitempty"`
}

// Page is a query result.
type Page struct {
	Docs    []*Doc
	HasMore bool
}

// Store is a document database namespaced by site and collection.
// Implementations must be safe for concurrent use. Callers validate names and
// sizes (Validate* helpers) before calling; implementations may re-check.
type Store interface {
	// Create inserts a new document. If id is empty a ULID is generated.
	// Returns ErrConflict if the id already exists.
	Create(ctx context.Context, site, collection, id string, data map[string]any, actor string) (*Doc, error)
	// Get returns a document or ErrNotFound.
	Get(ctx context.Context, site, collection, id string) (*Doc, error)
	// Put creates or fully replaces a document (Version incremented; CreatedAt/By preserved).
	Put(ctx context.Context, site, collection, id string, data map[string]any, actor string) (*Doc, error)
	// Update shallow-merges patch into an existing document; nil values delete
	// keys. Returns ErrNotFound if missing. Must be atomic w.r.t. concurrent updates.
	Update(ctx context.Context, site, collection, id string, patch map[string]any, actor string) (*Doc, error)
	// Delete removes a document. Returns (false, nil) if it did not exist.
	Delete(ctx context.Context, site, collection, id string) (bool, error)
	// Query returns matching documents (q must be Normalize'd).
	Query(ctx context.Context, site, collection string, q Query) (*Page, error)
	// Collections lists non-empty collection names of a site, sorted
	// (including system collections starting with "_"; callers filter).
	Collections(ctx context.Context, site string) ([]string, error)
	// Count returns the number of documents of a site (all collections).
	Count(ctx context.Context, site string) (int64, error)
	// DeleteSite removes every document of a site.
	DeleteSite(ctx context.Context, site string) error
	// Ping checks connectivity.
	Ping(ctx context.Context) error
	// Close releases resources.
	Close() error
}

// ---------------------------------------------------------------------------
// Validation

var (
	collectionRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	systemCollRe = regexp.MustCompile(`^_[A-Za-z0-9][A-Za-z0-9_-]{0,62}$`)
	idRe         = regexp.MustCompile(`^[A-Za-z0-9_\-:.]{1,128}$`)
	segmentRe    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]{0,63}$`)
	siteRe       = regexp.MustCompile(`^(_system|[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)$`)
)

// SystemFields are the addressable non-data fields.
var SystemFields = map[string]bool{
	"id": true, "_meta.created_at": true, "_meta.updated_at": true,
	"_meta.created_by": true, "_meta.updated_by": true, "_meta.version": true,
}

// ValidateSite checks a site namespace (a site name or "_system").
func ValidateSite(site string) error {
	if !siteRe.MatchString(site) {
		return fmt.Errorf("%w: site %q", ErrInvalid, site)
	}
	return nil
}

// ValidateCollection checks a user collection name (system=false) or a system
// collection name (system=true, must start with "_").
func ValidateCollection(name string, system bool) error {
	if system {
		if !systemCollRe.MatchString(name) {
			return fmt.Errorf("%w: system collection %q", ErrInvalid, name)
		}
		return nil
	}
	if !collectionRe.MatchString(name) {
		return fmt.Errorf("%w: collection name %q (letters, digits, '_' and '-', max 64, must not start with '_')", ErrInvalid, name)
	}
	return nil
}

// ValidateID checks a document id.
func ValidateID(id string) error {
	if !idRe.MatchString(id) {
		return fmt.Errorf("%w: document id %q", ErrInvalid, id)
	}
	return nil
}

// ValidateField checks a dot-path field reference (user data path or system field).
func ValidateField(field string) error {
	if SystemFields[field] {
		return nil
	}
	segs := strings.Split(field, ".")
	if len(segs) > 8 {
		return fmt.Errorf("%w: field path %q too deep", ErrInvalid, field)
	}
	for _, s := range segs {
		if !segmentRe.MatchString(s) {
			return fmt.Errorf("%w: field path %q", ErrInvalid, field)
		}
	}
	if segs[0] == "_meta" || segs[0] == "id" {
		return fmt.Errorf("%w: field path %q", ErrInvalid, field)
	}
	return nil
}

func isScalar(v any) bool {
	switch v.(type) {
	case nil, string, bool, float64, json.Number:
		return true
	case int, int64, int32, float32, uint, uint64, uint32:
		return true
	}
	return false
}

// Normalize validates q, applies defaults (limit, default order) and appends
// the id tiebreaker. maxList caps the limit.
func (q *Query) Normalize(maxList int) error {
	if maxList <= 0 {
		maxList = 1000
	}
	for i := range q.Where {
		f := &q.Where[i]
		if err := ValidateField(f.Field); err != nil {
			return err
		}
		f.Value = NormalizeValue(f.Value)
		switch f.Op {
		case OpEq, OpNe, OpLt, OpLte, OpGt, OpGte, OpContains:
			if !isScalar(f.Value) {
				return fmt.Errorf("%w: value for %q must be a scalar", ErrInvalid, f.Op)
			}
		case OpIn, OpNotIn:
			arr, ok := f.Value.([]any)
			if !ok || len(arr) == 0 || len(arr) > 30 {
				return fmt.Errorf("%w: value for %q must be an array of 1-30 scalars", ErrInvalid, f.Op)
			}
			for _, v := range arr {
				if !isScalar(v) {
					return fmt.Errorf("%w: value for %q must be an array of scalars", ErrInvalid, f.Op)
				}
			}
		default:
			return fmt.Errorf("%w: unknown operator %q", ErrInvalid, f.Op)
		}
	}
	if len(q.Where) > 20 {
		return fmt.Errorf("%w: at most 20 filters", ErrInvalid)
	}
	for i := range q.OrderBy {
		o := &q.OrderBy[i]
		if err := ValidateField(o.Field); err != nil {
			return err
		}
		switch strings.ToLower(o.Dir) {
		case "", "asc":
			if o.Dir != "" {
				o.Desc = false
			}
		case "desc":
			o.Desc = true
		default:
			return fmt.Errorf("%w: order dir must be asc or desc", ErrInvalid)
		}
		o.Dir = map[bool]string{false: "asc", true: "desc"}[o.Desc]
	}
	if len(q.OrderBy) > 5 {
		return fmt.Errorf("%w: at most 5 order_by fields", ErrInvalid)
	}
	if len(q.OrderBy) == 0 {
		q.OrderBy = []Order{{Field: "_meta.created_at", Dir: "asc"}}
	}
	if last := q.OrderBy[len(q.OrderBy)-1]; last.Field != "id" {
		q.OrderBy = append(q.OrderBy, Order{Field: "id", Dir: "asc"})
	}
	if q.Limit <= 0 {
		q.Limit = 100
	}
	if q.Limit > maxList {
		q.Limit = maxList
	}
	if q.Offset < 0 {
		return fmt.Errorf("%w: offset must be >= 0", ErrInvalid)
	}
	if q.Offset > 100000 {
		return fmt.Errorf("%w: offset too large", ErrInvalid)
	}
	return nil
}

// NormalizeValue converts Go numeric types and json.Number to float64 and
// []string etc. to []any so that backends compare uniformly.
func NormalizeValue(v any) any {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		if err != nil {
			return x.String()
		}
		return f
	case int:
		return float64(x)
	case int32:
		return float64(x)
	case int64:
		return float64(x)
	case uint:
		return float64(x)
	case uint32:
		return float64(x)
	case uint64:
		return float64(x)
	case float32:
		return float64(x)
	case []string:
		out := make([]any, len(x))
		for i, s := range x {
			out[i] = s
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = NormalizeValue(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = NormalizeValue(e)
		}
		return out
	}
	return v
}

// PrepareData strips reserved keys ("id", "_meta") from a write body,
// normalises numbers and enforces maxBytes on the JSON encoding.
func PrepareData(data map[string]any, maxBytes int) (map[string]any, error) {
	out := make(map[string]any, len(data))
	for k, v := range data {
		if k == "id" || k == "_meta" {
			continue
		}
		out[k] = NormalizeValue(v)
	}
	if maxBytes > 0 {
		b, err := json.Marshal(out)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		if len(b) > maxBytes {
			return nil, ErrTooLarge
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// IDs

var (
	ulidMu   sync.Mutex
	ulidLast [16]byte
	ulidMs   uint64
)

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewID returns a 26-character, lexicographically time-ordered ULID. IDs
// generated within the same millisecond are monotonically increasing.
func NewID() string {
	ulidMu.Lock()
	defer ulidMu.Unlock()
	ms := uint64(time.Now().UnixMilli())
	var b [16]byte
	if ms == ulidMs {
		b = ulidLast
		// increment the 80-bit random part
		for i := 15; i >= 6; i-- {
			b[i]++
			if b[i] != 0 {
				break
			}
		}
	} else {
		binary.BigEndian.PutUint16(b[0:2], uint16(ms>>32))
		binary.BigEndian.PutUint32(b[2:6], uint32(ms))
		_, _ = rand.Read(b[6:])
		ulidMs = ms
	}
	ulidLast = b
	return encodeULID(b)
}

func encodeULID(b [16]byte) string {
	// 128 bits → 26 base32 chars (first char encodes 3 bits).
	var out [26]byte
	var hi = uint64(b[0])<<56 | uint64(b[1])<<48 | uint64(b[2])<<40 | uint64(b[3])<<32 |
		uint64(b[4])<<24 | uint64(b[5])<<16 | uint64(b[6])<<8 | uint64(b[7])
	var lo = uint64(b[8])<<56 | uint64(b[9])<<48 | uint64(b[10])<<40 | uint64(b[11])<<32 |
		uint64(b[12])<<24 | uint64(b[13])<<16 | uint64(b[14])<<8 | uint64(b[15])
	for i := 25; i >= 0; i-- {
		out[i] = crockford[lo&31]
		lo = lo>>5 | hi<<59
		hi >>= 5
	}
	return string(out[:])
}
