// Package conv holds JSON-friendly value conversions shared by the warehouse
// sources (sqlsource, bigquery, athena), so every engine renders numbers,
// decimals, bytes and timestamps the same way (SPEC §5.5).
package conv

import (
	"encoding/base64"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"
)

// UniqueNames returns names with duplicates (and empty names) disambiguated so
// they can be used as keys of a row map: the first occurrence keeps its name,
// later ones get a "_2", "_3", … suffix (skipping suffixes already taken).
// Empty names become "column_<n>" (1-based position).
func UniqueNames(names []string) []string {
	out := make([]string, len(names))
	seen := make(map[string]bool, len(names))
	for _, n := range names {
		seen[n] = true
	}
	used := make(map[string]bool, len(names))
	for i, n := range names {
		if n == "" {
			n = "column_" + strconv.Itoa(i+1)
		}
		if !used[n] {
			used[n] = true
			out[i] = n
			continue
		}
		for k := 2; ; k++ {
			cand := n + "_" + strconv.Itoa(k)
			if !used[cand] && !seen[cand] {
				used[cand] = true
				out[i] = cand
				break
			}
		}
	}
	return out
}

// Float renders a float64 for JSON: finite values stay numbers; NaN and ±Inf
// (which encoding/json rejects) become the strings "NaN", "Infinity" and
// "-Infinity".
func Float(f float64) any {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	return f
}

// Float32 renders a float32 using its shortest decimal representation (so
// float32(0.1) becomes 0.1, not 0.10000000149011612).
func Float32(f float32) any {
	f64, err := strconv.ParseFloat(strconv.FormatFloat(float64(f), 'g', -1, 32), 64)
	if err != nil {
		return Float(float64(f))
	}
	return Float(f64)
}

// Rat renders an exact decimal value: a float64 when the float's shortest
// decimal representation equals r exactly (no precision is lost), otherwise
// the exact decimal string. exact is the engine's textual rendering of r; when
// empty, r.FloatString with a scale large enough for r is used.
func Rat(r *big.Rat, exact string) any {
	if r == nil {
		return nil
	}
	f, _ := r.Float64()
	if !math.IsInf(f, 0) && !math.IsNaN(f) {
		if back, ok := new(big.Rat).SetString(strconv.FormatFloat(f, 'g', -1, 64)); ok && back.Cmp(r) == 0 {
			return f
		}
	}
	if exact == "" {
		exact = RatString(r)
	}
	return exact
}

// RatString renders r as a plain decimal string without exponent and without
// superfluous trailing zeros. Non-terminating fractions are cut at 38 digits.
func RatString(r *big.Rat) string {
	if r.IsInt() {
		return r.Num().String()
	}
	s := r.FloatString(scaleOf(r))
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimSuffix(s, ".")
	}
	return s
}

// scaleOf returns the number of fractional digits needed to render r exactly
// if its denominator is of the form 2^a·5^b, else 38.
func scaleOf(r *big.Rat) int {
	d := new(big.Int).Set(r.Denom())
	two, five := big.NewInt(2), big.NewInt(5)
	var a, b int
	mod := new(big.Int)
	for {
		q, m := new(big.Int).QuoRem(d, two, mod)
		if m.Sign() != 0 {
			break
		}
		d, a = q, a+1
	}
	for {
		q, m := new(big.Int).QuoRem(d, five, mod)
		if m.Sign() != 0 {
			break
		}
		d, b = q, b+1
	}
	if d.Cmp(big.NewInt(1)) != 0 {
		return 38
	}
	return max(a, b)
}

// Decimal parses a decimal string (as returned by an engine for DECIMAL /
// NUMERIC columns) and renders it with Rat. Unparseable input (e.g. "NaN") is
// returned unchanged.
func Decimal(s string) any {
	t := strings.TrimSpace(s)
	if t == "" {
		return s
	}
	r, ok := new(big.Rat).SetString(t)
	if !ok {
		return s
	}
	return Rat(r, t)
}

// Bytes renders binary data as standard base64.
func Bytes(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// Timestamp renders an instant as RFC 3339 with nanoseconds, in UTC.
func Timestamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// Date renders the calendar date of t as YYYY-MM-DD (t's own location).
func Date(t time.Time) string { return t.Format(time.DateOnly) }
