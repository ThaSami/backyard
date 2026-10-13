package docstore

import (
	"sync/atomic"
	"time"
)

// TimePrecision is the precision of document timestamps. Every backend
// stores and returns CreatedAt/UpdatedAt truncated to this precision
// (PostgreSQL's timestamptz resolution), so documents look identical whichever
// backend is configured.
const TimePrecision = time.Microsecond

var lastMicros atomic.Int64

// Now returns the current time in UTC, truncated to TimePrecision. Values
// returned within one process are strictly increasing, which keeps the default
// "creation order" (created_at, id) deterministic even when many documents are
// written within the same microsecond: if the wall clock has not advanced (or
// went backwards) the previous value plus one microsecond is returned.
func Now() time.Time {
	for {
		now := time.Now().UnixMicro()
		last := lastMicros.Load()
		if now <= last {
			now = last + 1
		}
		if lastMicros.CompareAndSwap(last, now) {
			return time.UnixMicro(now).UTC()
		}
	}
}

// TruncTime normalises t to the stored representation: UTC, truncated to
// TimePrecision.
func TruncTime(t time.Time) time.Time {
	return t.UTC().Truncate(TimePrecision)
}

// ParseTime parses a filter value for a timestamp system field
// (_meta.created_at / _meta.updated_at). It accepts RFC 3339 strings with
// optional fractional seconds and returns the time truncated to
// TimePrecision. ok is false for any other value; such a filter value has a
// different type than the field and therefore matches nothing.
func ParseTime(v any) (t time.Time, ok bool) {
	s, isStr := v.(string)
	if !isStr {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, false
	}
	return TruncTime(t), true
}

// IsTimeField reports whether field is a timestamp system field, whose values
// compare chronologically.
func IsTimeField(field string) bool {
	return field == "_meta.created_at" || field == "_meta.updated_at"
}
