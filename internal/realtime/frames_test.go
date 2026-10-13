package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/pubsub/memory"
)

func TestNormalizeID(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want string
		ok   bool
	}{
		{``, ``, true},
		{`null`, ``, true},
		{` "s1" `, `"s1"`, true},
		{`42`, `42`, true},
		{`-1.5`, `-1.5`, true},
		{`true`, ``, false},
		{`{"a":1}`, ``, false},
		{`[1]`, ``, false},
		{`"` + strings.Repeat("x", 200) + `"`, ``, false},
	} {
		got, werr := normalizeID(json.RawMessage(tc.raw))
		if tc.ok != (werr == nil) || string(got) != tc.want {
			t.Errorf("normalizeID(%s) = %s, %v", tc.raw, got, werr)
		}
	}
}

func TestNormalizeState(t *testing.T) {
	for _, tc := range []struct {
		raw, want, code string
	}{
		{``, `{}`, ""},
		{`null`, `{}`, ""},
		{` { "a" : [1, 2] } `, `{"a":[1,2]}`, ""},
		{`[1]`, ``, codeBadRequest},
		{`"x"`, ``, codeBadRequest},
		{`3`, ``, codeBadRequest},
		{`{"k":"` + strings.Repeat("v", 40) + `"}`, ``, codeTooLarge},
	} {
		got, werr := normalizeState(json.RawMessage(tc.raw), 32)
		code := ""
		if werr != nil {
			code = werr.code
		}
		if code != tc.code || string(got) != tc.want {
			t.Errorf("normalizeState(%s) = %s, %v", tc.raw, got, werr)
		}
	}
}

func TestNormalizeData(t *testing.T) {
	for raw, want := range map[string]string{
		``:               `null`,
		`  `:             `null`,
		`null`:           `null`,
		` { "a" : 1 } `:  `{"a":1}`,
		`"text"`:         `"text"`,
		`[1, "two", {}]`: `[1,"two",{}]`,
		`12.5`:           `12.5`,
	} {
		if got := normalizeData(json.RawMessage(raw)); string(got) != want {
			t.Errorf("normalizeData(%q) = %s, want %s", raw, got, want)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Errorf("got %q", got)
	}
	if got := truncate("abcdef", 3); got != "abc…" {
		t.Errorf("got %q", got)
	}
	// Never splits a multi-byte rune.
	if got := truncate("ééé", 3); got != "é…" {
		t.Errorf("got %q", got)
	}
}

func TestEncodeError(t *testing.T) {
	var f map[string]any
	if err := json.Unmarshal(encodeError(json.RawMessage(`"x"`), errf(codeNotFound, "no %s", "room")), &f); err != nil {
		t.Fatal(err)
	}
	if f["t"] != "error" || f["id"] != "x" || f["error"].(map[string]any)["code"] != "not_found" ||
		f["error"].(map[string]any)["message"] != "no room" {
		t.Fatalf("frame = %v", f)
	}
	f = nil
	if err := json.Unmarshal(encodeError(nil, errf(codeBadRequest, "bad")), &f); err != nil {
		t.Fatal(err)
	}
	if _, ok := f["id"]; ok {
		t.Fatalf("id must be omitted: %v", f)
	}
}

func TestTopicSetRefcounting(t *testing.T) {
	b := memory.New()
	defer b.Close()
	ts := newTopicSet(b)
	ctx := context.Background()
	var got atomic.Int64
	handler := func([]byte) { got.Add(1) }

	// Many concurrent users of one topic share a single broker subscription.
	var wg sync.WaitGroup
	entries := make(chan *topicEntry, 50)
	for range 50 {
		wg.Go(func() {
			e, err := ts.acquire(ctx, "t", handler)
			if err != nil {
				t.Error(err)
				return
			}
			entries <- e
		})
	}
	wg.Wait()
	close(entries)
	if n := ts.len(); n != 1 {
		t.Fatalf("topics = %d", n)
	}
	if err := b.Publish(ctx, "t", []byte("x")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "one delivery", func() bool { return got.Load() == 1 })
	time.Sleep(20 * time.Millisecond)
	if got.Load() != 1 {
		t.Fatalf("delivered %d times through a shared subscription", got.Load())
	}
	var all []*topicEntry
	for e := range entries {
		all = append(all, e)
	}
	for _, e := range all[1:] {
		ts.release(e)
	}
	if n := ts.len(); n != 1 {
		t.Fatalf("released too early: %d topics", n)
	}
	ts.release(all[0])
	if n := ts.len(); n != 0 {
		t.Fatalf("topics after last release = %d", n)
	}
	if err := b.Publish(ctx, "t", []byte("y")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if got.Load() != 1 {
		t.Fatal("handler called after the last release")
	}

	// Resubscribe after full release, then close.
	e, err := ts.acquire(ctx, "t", handler)
	if err != nil {
		t.Fatal(err)
	}
	ts.close()
	ts.release(e) // harmless after close
	if _, err := ts.acquire(ctx, "t", handler); !errors.Is(err, ErrClosed) {
		t.Fatalf("acquire after close = %v", err)
	}
}

func TestTopicSetSubscribeFailure(t *testing.T) {
	ts := newTopicSet(failingBroker{})
	if _, err := ts.acquire(context.Background(), "t", func([]byte) {}); !errors.Is(err, errBrokerDown) {
		t.Fatalf("acquire = %v", err)
	}
	if n := ts.len(); n != 0 {
		t.Fatalf("failed entry kept: %d", n)
	}
}
