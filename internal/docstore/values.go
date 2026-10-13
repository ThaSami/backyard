package docstore

import (
	"fmt"
	"math"
	"strings"
	"unicode/utf8"
)

// MaxNestingDepth is the maximum nesting depth of a stored document: the
// document body is level 1 and every nested object or array adds a level.
// Deeper documents are rejected with ErrInvalid by every backend. The limit
// keeps documents well inside what every engine can evaluate (SQLite's JSON
// functions, including Cloudflare Durable Object SQLite, fail on JSON nested
// more than 1000 levels, which would make a single stored document break
// every query of its collection).
const MaxNestingDepth = 100

// SystemSite is the reserved site namespace for Backyard's own data
// ("_system/_users", "_system/_audit"; SPEC §10). It is exempt from the
// per-site document quota of WithLimits.
const SystemSite = "_system"

// CleanString returns s in the canonical form every backend stores: invalid
// UTF-8 is coerced the way encoding/json does (each invalid byte becomes
// U+FFFD), so a value reads back identically whichever backend stores it.
// Strings containing U+0000 are rejected with ErrInvalid because PostgreSQL
// cannot store them in jsonb or text; rejecting them everywhere keeps the
// backends identical.
func CleanString(s string) (string, error) {
	if strings.IndexByte(s, 0) >= 0 {
		return "", fmt.Errorf("%w: strings must not contain the NUL character (U+0000)", ErrInvalid)
	}
	if utf8.ValidString(s) {
		return s, nil
	}
	var sb strings.Builder
	sb.Grow(len(s) + 8)
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			sb.WriteRune(utf8.RuneError)
		} else {
			sb.WriteString(s[i : i+size])
		}
		i += size
	}
	return sb.String(), nil
}

// CleanNumber returns f in canonical form: negative zero becomes zero (as
// PostgreSQL's numeric type stores it) and NaN or infinities, which JSON
// cannot represent, are rejected with ErrInvalid.
func CleanNumber(f float64) (float64, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("%w: number %v is not valid JSON", ErrInvalid, f)
	}
	if f == 0 {
		return 0, nil // also turns -0 into +0
	}
	return f, nil
}

// cleanOperand canonicalises one (already NormalizeValue'd) scalar filter
// operand the same way stored data is canonicalised, so that every backend
// compares identical values (or rejects the query identically).
func cleanOperand(v any) (any, error) {
	switch x := v.(type) {
	case string:
		return CleanString(x)
	case float64:
		return CleanNumber(x)
	}
	return v, nil
}
