package docstore

import (
	"errors"
	"math"
	"testing"
)

func TestCleanString(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"plain", "plain"},
		{"é😀", "é😀"},
		{"a\xffb", "a�b"},
		{"\xff\xfe", "��"},      // every invalid byte is replaced
		{"\xe2\x82", "��"},      // truncated sequence
		{"x\xc3", "x�"},         // truncated at the end
		{"�", "�"},              // a real U+FFFD is kept
		{"\xed\xa0\x80", "���"}, // surrogate half
	} {
		got, err := CleanString(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("CleanString(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	for _, in := range []string{"\x00", "a\x00b", "\xff\x00"} {
		if _, err := CleanString(in); !errors.Is(err, ErrInvalid) {
			t.Errorf("CleanString(%q) = %v, want ErrInvalid", in, err)
		}
	}
}

func TestCleanNumber(t *testing.T) {
	for _, in := range []float64{0, 1, -1.5, 1e300, -1e-300, math.MaxFloat64, math.SmallestNonzeroFloat64} {
		if got, err := CleanNumber(in); err != nil || got != in {
			t.Errorf("CleanNumber(%v) = %v, %v", in, got, err)
		}
	}
	if got, err := CleanNumber(math.Copysign(0, -1)); err != nil || math.Signbit(got) {
		t.Errorf("CleanNumber(-0) = %v (signbit %v), %v", got, math.Signbit(got), err)
	}
	for _, in := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := CleanNumber(in); !errors.Is(err, ErrInvalid) {
			t.Errorf("CleanNumber(%v) = %v, want ErrInvalid", in, err)
		}
	}
}

func TestPrepareQueryCleansOperands(t *testing.T) {
	orig := Query{Where: []Filter{
		{"s", OpEq, "a\xff"},
		{"n", OpIn, []any{math.Copysign(0, -1), "x\xfe", 2}},
		{"z", OpGt, float32(math.Copysign(0, -1))},
	}}
	q, err := PrepareQuery(orig)
	if err != nil {
		t.Fatal(err)
	}
	if q.Where[0].Value != "a�" {
		t.Errorf("string operand = %q", q.Where[0].Value)
	}
	list := q.Where[1].Value.([]any)
	if f := list[0].(float64); f != 0 || math.Signbit(f) || list[1] != "x�" || list[2] != 2.0 {
		t.Errorf("list operand = %#v", list)
	}
	if f := q.Where[2].Value.(float64); math.Signbit(f) {
		t.Errorf("-0 operand kept its sign")
	}
	if orig.Where[0].Value != "a\xff" || orig.Where[1].Value.([]any)[1] != "x\xfe" {
		t.Errorf("caller's operands were modified: %+v", orig.Where)
	}
	for _, f := range []Filter{
		{"n", OpLt, math.NaN()},
		{"n", OpGte, math.Inf(-1)},
		{"n", OpNotIn, []any{1, math.Inf(1)}},
		{"s", OpContains, "\x00"},
		{"s", OpIn, []any{"ok", "a\x00"}},
	} {
		if _, err := PrepareQuery(Query{Where: []Filter{f}}); !errors.Is(err, ErrInvalid) {
			t.Errorf("PrepareQuery(%v %s %v) = %v, want ErrInvalid", f.Field, f.Op, f.Value, err)
		}
	}
}
