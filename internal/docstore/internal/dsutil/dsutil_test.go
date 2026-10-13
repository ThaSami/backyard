package dsutil

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/thasami/backyard/internal/docstore"
)

func TestCanonical(t *testing.T) {
	type point struct {
		X int `json:"x"`
	}
	in := map[string]any{
		"id": "dropped", "_meta": "dropped",
		"i": 1, "u": uint64(2), "f32": float32(1.5), "num": json.Number("2.5"),
		"strs": []string{"a"}, "raw": json.RawMessage(`{"k":[1,true]}`),
		"struct": point{X: 3}, "typed": map[string]int{"a": 1}, "ptr": &point{X: 4},
		"nil": nil, "nested": map[string]any{"deep": []any{int8(1), map[string]any{"z": int16(2)}}},
	}
	got, err := Canonical(in)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"i": 1.0, "u": 2.0, "f32": 1.5, "num": 2.5,
		"strs": []any{"a"}, "raw": map[string]any{"k": []any{1.0, true}},
		"struct": map[string]any{"x": 3.0}, "typed": map[string]any{"a": 1.0}, "ptr": map[string]any{"x": 4.0},
		"nil": nil, "nested": map[string]any{"deep": []any{1.0, map[string]any{"z": 2.0}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Canonical =\n%#v\nwant\n%#v", got, want)
	}
	// The result shares nothing with the input.
	in["nested"].(map[string]any)["deep"].([]any)[0] = "changed"
	if got["nested"].(map[string]any)["deep"].([]any)[0] != 1.0 {
		t.Error("Canonical aliased the input")
	}
	for name, bad := range map[string]any{
		"nan": math.NaN(), "inf": math.Inf(1), "func": func() {}, "badnum": json.Number("1x"),
		"nested nan": []any{math.NaN()}, "chan": make(chan int),
	} {
		if _, err := Canonical(map[string]any{"v": bad}); !errors.Is(err, docstore.ErrInvalid) {
			t.Errorf("Canonical(%s) = %v, want ErrInvalid", name, err)
		}
	}
	if m, err := Canonical(nil); err != nil || m == nil || len(m) != 0 {
		t.Errorf("Canonical(nil) = %#v, %v", m, err)
	}
}

func TestMarshalAndDecode(t *testing.T) {
	b, err := Marshal(map[string]any{"html": "<a>&</a>"})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"html":"<a>&</a>"}` {
		t.Errorf("Marshal = %s", b)
	}
	if b, _ := Marshal(nil); string(b) != "{}" {
		t.Errorf("Marshal(nil) = %s", b)
	}
	for _, in := range []string{"", "null", "  ", "{}"} {
		m, err := DecodeData([]byte(in))
		if err != nil || m == nil || len(m) != 0 {
			t.Errorf("DecodeData(%q) = %#v, %v", in, m, err)
		}
	}
	if _, err := DecodeData([]byte("[1]")); err == nil {
		t.Error("DecodeData(array) succeeded")
	}
	merged, raw, err := MergePatch(map[string]any{"a": 1.0, "b": 2.0}, map[string]any{"a": nil, "c": "x"})
	if err != nil || !reflect.DeepEqual(merged, map[string]any{"b": 2.0, "c": "x"}) || string(raw) != `{"b":2,"c":"x"}` {
		t.Errorf("MergePatch = %v %s %v", merged, raw, err)
	}
}

func TestCheckKey(t *testing.T) {
	ok := [][3]string{
		{"site", "posts", "01J"}, {"_system", "_users", "alice.smith+tag@acme.com"}, {"a-b", "_files", "x:y.z"},
		{"site", "_files", "名前 with spaces/and#hash"},
	}
	for _, k := range ok {
		if err := CheckKey(k[0], k[1], k[2]); err != nil {
			t.Errorf("CheckKey(%v) = %v", k, err)
		}
	}
	bad := [][3]string{
		{"Site", "c", "x"}, {"s", "_", "x"}, {"s", "c d", "x"}, {"s", "c", ""}, {"s", "c", "a/b"}, {"", "c", "x"},
		{"s#x", "c", "x"}, {"s", "c", "a@b.c"}, {"s", "_users", ""}, {"s", "_users", "a\nb"},
		{"s", "_users", "\xff"}, {"s", "_users", strings.Repeat("x", MaxSystemIDBytes+1)},
	}
	for _, k := range bad {
		if err := CheckKey(k[0], k[1], k[2]); !errors.Is(err, docstore.ErrInvalid) {
			t.Errorf("CheckKey(%v) = %v, want ErrInvalid", k, err)
		}
	}
}

func TestCanonicalCleansValues(t *testing.T) {
	negZero := math.Copysign(0, -1)
	got, err := Canonical(map[string]any{
		"z": negZero, "z32": float32(negZero), "arr": []any{negZero}, "jn": json.Number("-0"),
		"bad\xff": "x\xffy\xc3", "strs": []string{"\xfe"}, "nested": map[string]any{"\xff": "ok"},
		"struct": struct {
			S string `json:"s"`
		}{"plain"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"z": 0.0, "z32": 0.0, "arr": []any{0.0}, "jn": 0.0,
		"bad�": "x�y�", "strs": []any{"�"}, "nested": map[string]any{"�": "ok"},
		"struct": map[string]any{"s": "plain"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Canonical =\n%#v\nwant\n%#v", got, want)
	}
	for _, k := range []string{"z", "z32", "jn"} {
		if math.Signbit(got[k].(float64)) {
			t.Errorf("%s: -0 kept", k)
		}
	}
	if math.Signbit(got["arr"].([]any)[0].(float64)) {
		t.Error("arr: -0 kept")
	}
	// The canonical form survives a JSON round trip unchanged, as stored by
	// the SQL and DynamoDB backends.
	raw, err := Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	back, err := DecodeData(raw)
	if err != nil || !reflect.DeepEqual(back, got) {
		t.Errorf("round trip = %#v, %v", back, err)
	}
}

func TestCanonicalRejectsNUL(t *testing.T) {
	for name, data := range map[string]map[string]any{
		"value":         {"s": "a\x00"},
		"key":           {"\x00": 1},
		"nested key":    {"o": map[string]any{"k\x00": 1}},
		"array":         {"a": []any{"\x00"}},
		"string slice":  {"a": []string{"ok", "\x00"}},
		"raw message":   {"r": json.RawMessage(`{"k":"\u0000"}`)},
		"struct string": {"p": struct{ S string }{"\x00"}},
	} {
		if _, err := Canonical(data); !errors.Is(err, docstore.ErrInvalid) {
			t.Errorf("%s: Canonical = %v, want ErrInvalid", name, err)
		}
	}
	if _, err := CleanActor("a\x00"); !errors.Is(err, docstore.ErrInvalid) {
		t.Errorf("CleanActor(NUL) = %v", err)
	}
	if a, err := CleanActor("ok\xff"); err != nil || a != "ok�" {
		t.Errorf("CleanActor = %q, %v", a, err)
	}
	if a, err := CheckWrite("site", "c", "id", "alice@acme.com"); err != nil || a != "alice@acme.com" {
		t.Errorf("CheckWrite = %q, %v", a, err)
	}
	if _, err := CheckWrite("site", "c", "a/b", "x"); !errors.Is(err, docstore.ErrInvalid) {
		t.Errorf("CheckWrite(bad id) = %v", err)
	}
}

func TestCanonicalDepth(t *testing.T) {
	nest := func(levels int, mk func(any) any) any {
		var v any = 1
		for range levels {
			v = mk(v)
		}
		return v
	}
	arr := func(v any) any { return []any{v} }
	obj := func(v any) any { return map[string]any{"k": v} }
	strs := func(levels int) any { // innermost container is a []string
		var v any = []string{"x"}
		for range levels - 1 {
			v = []any{v}
		}
		return v
	}
	for name, mk := range map[string]func(any) any{"arrays": arr, "objects": obj} {
		// The body is level 1, so MaxNestingDepth-1 containers fit inside it.
		if _, err := Canonical(map[string]any{"d": nest(docstore.MaxNestingDepth-1, mk)}); err != nil {
			t.Errorf("%s at the limit: %v", name, err)
		}
		if _, err := Canonical(map[string]any{"d": nest(docstore.MaxNestingDepth, mk)}); !errors.Is(err, docstore.ErrInvalid) {
			t.Errorf("%s past the limit: %v, want ErrInvalid", name, err)
		}
	}
	if _, err := Canonical(map[string]any{"d": strs(docstore.MaxNestingDepth - 1)}); err != nil {
		t.Errorf("[]string at the limit: %v", err)
	}
	if _, err := Canonical(map[string]any{"d": strs(docstore.MaxNestingDepth)}); !errors.Is(err, docstore.ErrInvalid) {
		t.Errorf("[]string past the limit: %v", err)
	}
	deepRaw := json.RawMessage(strings.Repeat("[", docstore.MaxNestingDepth) + strings.Repeat("]", docstore.MaxNestingDepth))
	if _, err := Canonical(map[string]any{"d": deepRaw}); !errors.Is(err, docstore.ErrInvalid) {
		t.Errorf("deep RawMessage: %v, want ErrInvalid", err)
	}
}
