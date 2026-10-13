package conv

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// values counts the values Size charges ValueOverhead for.
func values(v any) int {
	switch x := v.(type) {
	case []any:
		n := 1
		for _, e := range x {
			n += values(e)
		}
		return n
	case map[string]any:
		n := 1
		for _, e := range x {
			n += values(e)
		}
		return n
	}
	return 1
}

func TestSizeMatchesJSONEncoding(t *testing.T) {
	cases := []any{
		nil, true, false, "", "plain", `quote " backslash \ slash /`, "ctl \b\f\n\r\t \x00\x01\x1f",
		"<script>&amp;</script>", "h\u00e9llo w\u00f6rld \u2713 \U0001F600", "sep \u2028 \u2029", "bad \xff\xfe utf8",
		int64(0), int64(-1), int64(math.MaxInt64), int64(math.MinInt64), 7,
		0.0, 1.5, -2.25, 0.1, 1e20, 1e21, 123456789.123456789, 1e-6, 1e-7, 1.5e-300, -3e300, math.MaxFloat64, math.SmallestNonzeroFloat64,
		[]any{}, []any{int64(1), nil, "x", []any{true}},
		map[string]any{}, map[string]any{"a": int64(1), "b<": "c", "nested": map[string]any{"d": []any{1.5, "e"}}},
		struct{ X int }{3}, // not produced by the converters: measured by encoding
	}
	for _, v := range cases {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal %#v: %v", v, err)
		}
		if got, want := Size(v), len(b)+values(v)*ValueOverhead; got != want {
			t.Errorf("Size(%#v) = %d, want %d (JSON %s)", v, got, want, b)
		}
	}
	if got := Size(math.NaN()); got <= ValueOverhead {
		t.Errorf("Size(NaN) = %d", got)
	}
}

func TestStringSizeLarge(t *testing.T) {
	s := strings.Repeat("x", 1<<20) + strings.Repeat("\x01", 10)
	if got, want := StringSize(s), 2+(1<<20)+60; got != want {
		t.Errorf("StringSize = %d, want %d", got, want)
	}
}

func TestBudget(t *testing.T) {
	row := map[string]any{"pad": strings.Repeat("x", 100)}
	per := Size(row) + 1
	b := NewBudget(3*per + per/2)
	for i := range 3 {
		if !b.Add(row) {
			t.Fatalf("row %d does not fit (remaining %d, row %d)", i, b.Remaining(), per)
		}
	}
	if b.Used() != 3*per || b.Remaining() != per/2 {
		t.Errorf("used = %d remaining = %d", b.Used(), b.Remaining())
	}
	if b.Add(row) {
		t.Error("row beyond the budget accepted")
	}
	if b.Used() != 3*per {
		t.Errorf("rejected row was counted: used = %d", b.Used())
	}
	// A row larger than the whole budget never fits.
	if NewBudget(per - 1).Add(row) {
		t.Error("oversized row accepted")
	}
}
