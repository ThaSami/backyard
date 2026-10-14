package ai

import (
	"strings"
	"testing"
)

func TestStripCodeFences(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"plain object", `{"a":1}`, `{"a":1}`},
		{"whitespace", "  \n{\"a\":1}\n ", `{"a":1}`},
		{"json fence", "```json\n{\"a\":1}\n```", `{"a":1}`},
		{"bare fence", "```\n[1,2]\n```", `[1,2]`},
		{"fence with prose", "Here you go:\n```json\n{\"a\":1}\n```\nThanks!", `{"a":1}`},
		{"inline fence", "```json {\"a\":1}```", `{"a":1}`},
		{"prose around object", "Sure! {\"a\": {\"b\": 2}} hope that helps", `{"a": {"b": 2}}`},
		{"array in prose", "result: [1, 2, 3].", `[1, 2, 3]`},
		{"not json", "no json here", "no json here"},
		{"empty", "   ", ""},
		{"invalid fenced", "```json\n{oops\n```", "{oops"},
		{"unterminated fence", "```json\n{\"a\":1}", `{"a":1}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := StripCodeFences(tt.in); got != tt.want {
				t.Errorf("StripCodeFences(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSystemWithJSON(t *testing.T) {
	if got := SystemWithJSON(""); got != JSONInstruction {
		t.Errorf("empty system: %q", got)
	}
	got := SystemWithJSON("Be brief.")
	if !strings.HasPrefix(got, "Be brief.\n\n") || !strings.HasSuffix(got, JSONInstruction) {
		t.Errorf("got %q", got)
	}
	if !strings.Contains(strings.ToLower(JSONInstruction), "json") {
		t.Error("instruction must mention JSON (OpenAI json_object requirement)")
	}
}

func TestOpaqueUserID(t *testing.T) {
	if OpaqueUserID("") != "" {
		t.Error("empty user must map to empty")
	}
	a, b := OpaqueUserID("alice@acme.com"), OpaqueUserID("bob@acme.com")
	if a == b || a != OpaqueUserID("alice@acme.com") {
		t.Error("ids must be stable and distinct")
	}
	if strings.Contains(a, "alice") || len(a) != 34 {
		t.Errorf("unexpected id %q", a)
	}
}

func TestValidEffort(t *testing.T) {
	for _, e := range []string{"low", "medium", "high", "xhigh", "max"} {
		if !ValidEffort(e) {
			t.Errorf("%s should be valid", e)
		}
	}
	for _, e := range []string{"", "HIGH", "extreme"} {
		if ValidEffort(e) {
			t.Errorf("%q should be invalid", e)
		}
	}
}

func TestTextOf(t *testing.T) {
	got := TextOf([]Part{{Type: "text", Text: "a"}, {Type: "image", URL: "x"}, {Type: "text", Text: "b"}})
	if got != "ab" {
		t.Errorf("got %q", got)
	}
}

func TestParseDataURL(t *testing.T) {
	tests := []struct {
		in   string
		ok   bool
		mt   string
		data string
	}{
		{"data:image/png;base64,aGVsbG8=", true, "image/png", "hello"},
		{"data:IMAGE/PNG;base64,aGVsbG8", true, "image/png", "hello"},
		{"data:image/jpeg;charset=x;base64,aGk=", true, "image/jpeg", "hi"},
		{"data:text/plain,hello", false, "", ""},
		{"data:image/png;base64,!!!", false, "", ""},
		{"https://example.com/a.png", false, "", ""},
		{"data:image/png;base64", false, "", ""},
	}
	for _, tt := range tests {
		data, mt, ok := ParseDataURL(tt.in)
		if ok != tt.ok || mt != tt.mt || string(data) != tt.data {
			t.Errorf("ParseDataURL(%q) = %q, %q, %v", tt.in, data, mt, ok)
		}
	}
}

func TestKeyedUserID(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	a := KeyedUserID(key, "alice@example.com")
	if a == "" || !strings.HasPrefix(a, "u_") || len(a) != 34 {
		t.Fatalf("KeyedUserID = %q", a)
	}
	if a != KeyedUserID(key, "alice@example.com") {
		t.Error("not deterministic")
	}
	if a == KeyedUserID(key, "bob@example.com") || a == KeyedUserID([]byte("other-key"), "alice@example.com") {
		t.Error("ids must depend on user and key")
	}
	if a == OpaqueUserID("alice@example.com") {
		t.Error("keyed id must differ from the unkeyed hash")
	}
	if KeyedUserID(nil, "alice@example.com") != OpaqueUserID("alice@example.com") {
		t.Error("empty key falls back to OpaqueUserID")
	}
	if KeyedUserID(key, "") != "" {
		t.Error("empty user maps to empty id")
	}
}
