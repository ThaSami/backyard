package conv

import (
	"encoding/json"
	"math"
	"strconv"
	"unicode/utf8"
)

// ValueOverhead is the fixed cost Size adds for every value (including each
// row, array and object): roughly what holding a converted value in an
// interface, slice or map costs in memory beyond its JSON text.
const ValueOverhead = 16

// Size estimates the bytes a converted value occupies: the length of its
// encoding/json encoding plus ValueOverhead per value. It is exact for the
// values the converters produce (nil, bool, string, int64, float64, []any,
// map[string]any); anything else is measured by encoding it.
func Size(v any) int {
	n := ValueOverhead
	switch x := v.(type) {
	case nil:
		return n + len("null")
	case bool:
		if x {
			return n + len("true")
		}
		return n + len("false")
	case string:
		return n + StringSize(x)
	case int64:
		return n + intLen(x)
	case int:
		return n + intLen(int64(x))
	case float64:
		return n + floatLen(x)
	case []any:
		n += 2 + max(len(x)-1, 0) // brackets, commas
		for _, e := range x {
			n += Size(e)
		}
		return n
	case map[string]any:
		n += 2 + max(len(x)-1, 0) // braces, commas
		for k, e := range x {
			n += StringSize(k) + 1 + Size(e) // key, colon, value
		}
		return n
	}
	b, err := json.Marshal(v)
	if err != nil {
		return n
	}
	return n + len(b)
}

// StringSize returns the length of s encoded as a JSON string by
// encoding/json (quotes, escapes, HTML-safe escaping of <, > and &, and
// an escaped replacement character for each invalid UTF-8 byte).
func StringSize(s string) int {
	n := 2
	for i := 0; i < len(s); {
		if c := s[i]; c < utf8.RuneSelf {
			n += int(asciiSize[c])
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			n += 6 // the escaped replacement character
		case r == 0x2028 || r == 0x2029: // line/paragraph separator
			n += 6 // escaped
		default:
			n += size
		}
		i += size
	}
	return n
}

// asciiSize is the encoded length of each ASCII byte in a JSON string.
var asciiSize = func() (t [utf8.RuneSelf]uint8) {
	for c := range t {
		switch c {
		case '"', '\\', '\b', '\f', '\n', '\r', '\t':
			t[c] = 2
		case '<', '>', '&':
			t[c] = 6
		default:
			if c < 0x20 {
				t[c] = 6 // \u00XX
			} else {
				t[c] = 1
			}
		}
	}
	return t
}()

func intLen(i int64) int {
	var buf [24]byte
	return len(strconv.AppendInt(buf[:0], i, 10))
}

// floatLen is the length of f as encoding/json writes a float64 (finite
// values only; the converters turn NaN and infinities into strings).
func floatLen(f float64) int {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return len(`"-Infinity"`)
	}
	format := byte('f')
	if abs := math.Abs(f); abs != 0 && (abs < 1e-6 || abs >= 1e21) {
		format = 'e'
	}
	var buf [32]byte
	b := strconv.AppendFloat(buf[:0], f, format, -1, 64)
	n := len(b)
	if format == 'e' && n >= 4 && b[n-4] == 'e' && b[n-3] == '-' && b[n-2] == '0' {
		n-- // encoding/json writes e-7, not e-07
	}
	return n
}

// Budget tracks the estimated size (see Size) of the rows added to a result
// against a cap.
type Budget struct {
	limit, used int
}

// NewBudget returns a Budget of limit bytes.
func NewBudget(limit int) *Budget { return &Budget{limit: limit} }

// Add reports whether row fits in what is left of the budget and, if it
// does, counts it (plus the comma separating rows).
func (b *Budget) Add(row map[string]any) bool {
	n := Size(row) + 1
	if n > b.limit-b.used {
		return false
	}
	b.used += n
	return true
}

// Remaining returns the bytes left.
func (b *Budget) Remaining() int { return b.limit - b.used }

// Used returns the bytes counted so far.
func (b *Budget) Used() int { return b.used }
