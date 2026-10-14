package conv

import (
	"math"
	"math/big"
	"reflect"
	"testing"
	"time"
)

func TestUniqueNames(t *testing.T) {
	cases := []struct{ in, want []string }{
		{nil, []string{}},
		{[]string{"a", "b"}, []string{"a", "b"}},
		{[]string{"a", "a", "a"}, []string{"a", "a_2", "a_3"}},
		{[]string{"a", "a", "a_2"}, []string{"a", "a_3", "a_2"}},
		{[]string{"", "x", ""}, []string{"column_1", "x", "column_3"}},
		{[]string{"?column?", "?column?"}, []string{"?column?", "?column?_2"}},
	}
	for _, c := range cases {
		if got := UniqueNames(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("UniqueNames(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFloat(t *testing.T) {
	cases := []struct {
		in   float64
		want any
	}{
		{1.5, 1.5},
		{0, 0.0},
		{math.NaN(), "NaN"},
		{math.Inf(1), "Infinity"},
		{math.Inf(-1), "-Infinity"},
	}
	for _, c := range cases {
		if got := Float(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Float(%v) = %#v", c.in, got)
		}
	}
	if got := Float32(0.1); got != 0.1 {
		t.Errorf("Float32(0.1) = %#v", got)
	}
	if got := Float32(float32(math.Inf(1))); got != "Infinity" {
		t.Errorf("Float32(+Inf) = %#v", got)
	}
}

func TestRatAndDecimal(t *testing.T) {
	r := func(s string) *big.Rat {
		v, ok := new(big.Rat).SetString(s)
		if !ok {
			t.Fatalf("bad rat %q", s)
		}
		return v
	}
	cases := []struct {
		in   string
		want any
	}{
		{"12.25", 12.25},
		{"0.1", 0.1},
		{"1.10", 1.1},
		{"-0.007", -0.007},
		{"100", 100.0},
		{"9007199254740993", "9007199254740993"},
		{"12345678901234567890.123456789", "12345678901234567890.123456789"},
		{"0.1234567890123456789", "0.1234567890123456789"},
	}
	for _, c := range cases {
		if got := Rat(r(c.in), ""); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Rat(%s) = %#v, want %#v", c.in, got, c.want)
		}
		if got := Decimal(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Decimal(%s) = %#v, want %#v", c.in, got, c.want)
		}
	}
	// The engine's exact rendering is preserved when lossy.
	if got := Rat(r("9007199254740993"), "9007199254740993.000"); got != "9007199254740993.000" {
		t.Errorf("exact rendering lost: %#v", got)
	}
	if got := Rat(nil, ""); got != nil {
		t.Errorf("Rat(nil) = %#v", got)
	}
	if got := RatString(r("1/3")); got != "0.33333333333333333333333333333333333333" {
		t.Errorf("RatString(1/3) = %s", got)
	}
	if got := RatString(r("-5/4")); got != "-1.25" {
		t.Errorf("RatString(-5/4) = %s", got)
	}
	for _, s := range []string{"NaN", "", "abc"} {
		if got := Decimal(s); got != s {
			t.Errorf("Decimal(%q) = %#v", s, got)
		}
	}
	// Huge exponents stay strings.
	if got := Decimal("1e400"); got != "1e400" {
		t.Errorf("Decimal(1e400) = %#v", got)
	}
}

func TestBytesAndTimes(t *testing.T) {
	if got := Bytes([]byte{0xde, 0xad, 0xbe, 0xef}); got != "3q2+7w==" {
		t.Errorf("Bytes = %s", got)
	}
	loc := time.FixedZone("x", 2*3600)
	ts := time.Date(2026, 1, 2, 3, 4, 5, 123456789, loc)
	if got := Timestamp(ts); got != "2026-01-02T01:04:05.123456789Z" {
		t.Errorf("Timestamp = %s", got)
	}
	if got := Date(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)); got != "2026-01-02" {
		t.Errorf("Date = %s", got)
	}
}
