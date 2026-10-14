package realtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/thasami/backyard/internal/identity"
)

// ErrClosed is returned by PublishDB after Close.
var ErrClosed = errors.New("realtime: hub closed")

// Error codes sent in {"t":"error"} frames (SPEC §5 codes).
const (
	codeBadRequest  = "bad_request"
	codeForbidden   = "forbidden"
	codeNotFound    = "not_found"
	codeTooLarge    = "too_large"
	codeRateLimited = "rate_limited"
	codeInternal    = "internal"
)

// Client → server frame types.
const (
	frameSub   = "sub"
	frameUnsub = "unsub"
	frameJoin  = "join"
	frameLeave = "leave"
	frameState = "state"
	frameSend  = "send"
	framePing  = "ping"
)

// maxIDBytes bounds the client correlation id.
const maxIDBytes = 128

var (
	roomRe = regexp.MustCompile(`^[A-Za-z0-9_\-:.]{1,128}$`)
	cidRe  = regexp.MustCompile(`^c_[0-9a-f]{1,64}$`)
)

// inFrame is any client → server frame. Unknown fields are ignored.
type inFrame struct {
	T          string          `json:"t"`
	ID         json.RawMessage `json:"id"`
	Collection string          `json:"collection"`
	Room       string          `json:"room"`
	State      json.RawMessage `json:"state"`
	Data       json.RawMessage `json:"data"`
	To         string          `json:"to"`
	Echo       bool            `json:"echo"`
}

// member is the wire Member: {"cid","user","state","joined_at"}.
type member struct {
	CID      string           `json:"cid"`
	User     identity.Summary `json:"user"`
	State    json.RawMessage  `json:"state"`
	JoinedAt time.Time        `json:"joined_at"`
}

// joinedAtLayout formats Member.joined_at: RFC 3339 in UTC with exactly three
// fractional digits ("2026-01-02T03:04:05.120Z"). A fixed width lets clients
// order members by comparing the strings, which time.RFC3339Nano (it drops
// trailing zeros) would break.
const joinedAtLayout = "2006-01-02T15:04:05.000Z07:00"

// MarshalJSON encodes m as the wire Member with a fixed-width joined_at and
// an object state.
func (m member) MarshalJSON() ([]byte, error) {
	state := m.State
	if len(state) == 0 {
		state = emptyObject
	}
	return json.Marshal(struct {
		CID      string           `json:"cid"`
		User     identity.Summary `json:"user"`
		State    json.RawMessage  `json:"state"`
		JoinedAt string           `json:"joined_at"`
	}{m.CID, m.User, state, m.JoinedAt.UTC().Format(joinedAtLayout)})
}

// sender is the "from" object of a msg frame.
type sender struct {
	CID  string           `json:"cid"`
	User identity.Summary `json:"user"`
}

type helloFrame struct {
	T    string         `json:"t"`
	CID  string         `json:"cid"`
	User *identity.User `json:"user"`
}

type ackFrame struct {
	T  string          `json:"t"`
	ID json.RawMessage `json:"id,omitempty"`
}

type joinedFrame struct {
	T       string          `json:"t"`
	ID      json.RawMessage `json:"id,omitempty"`
	Room    string          `json:"room"`
	Self    member          `json:"self"`
	Members []member        `json:"members"`
}

type presenceFrame struct {
	T      string `json:"t"`
	Room   string `json:"room"`
	Op     string `json:"op"`
	Member member `json:"member"`
}

type msgFrame struct {
	T    string          `json:"t"`
	Room string          `json:"room"`
	From sender          `json:"from"`
	Data json.RawMessage `json:"data"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type errorFrame struct {
	T     string          `json:"t"`
	ID    json.RawMessage `json:"id,omitempty"`
	Error errorBody       `json:"error"`
}

var pongFrame = []byte(`{"t":"pong"}`)

// wireError is a client-visible error.
type wireError struct {
	code string
	msg  string
}

func (e *wireError) Error() string { return e.code + ": " + e.msg }

func errf(code, format string, a ...any) *wireError {
	return &wireError{code: code, msg: fmt.Sprintf(format, a...)}
}

func encodeError(id json.RawMessage, e *wireError) []byte {
	return mustJSON(errorFrame{T: "error", ID: id, Error: errorBody{Code: e.code, Message: e.msg}})
}

// mustJSON marshals frames built from validated data; failure is a
// programming error, so it degrades to an internal error frame.
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"t":"error","error":{"code":"internal","message":"failed to encode frame"}}`)
	}
	return b
}

// marshalUnescaped encodes v as compact JSON without HTML escaping, so that
// '<', '>' and '&' in documents take one byte rather than six (json.Marshal
// would grow an HTML document up to sixfold on its way to subscribers).
func marshalUnescaped(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// normalizeID validates the optional client correlation id: a JSON string or
// number of at most maxIDBytes. null/absent → nil.
func normalizeID(raw json.RawMessage) (json.RawMessage, *wireError) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if len(raw) > maxIDBytes {
		return nil, errf(codeBadRequest, "id longer than %d bytes", maxIDBytes)
	}
	switch c := raw[0]; {
	case c == '"', c == '-', c >= '0' && c <= '9':
		return raw, nil
	default:
		return nil, errf(codeBadRequest, "id must be a string or a number")
	}
}

// peekID extracts a usable id from a frame that failed to decode, so that the
// error can still be correlated by the client.
func peekID(data []byte) json.RawMessage {
	var f struct {
		ID json.RawMessage `json:"id"`
	}
	if json.Unmarshal(data, &f) != nil {
		return nil
	}
	id, werr := normalizeID(f.ID)
	if werr != nil {
		return nil
	}
	return id
}

var emptyObject = json.RawMessage(`{}`)

// normalizeState validates a presence state: a JSON object (absent/null → {})
// of at most max bytes after compaction.
func normalizeState(raw json.RawMessage, max int) (json.RawMessage, *wireError) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return emptyObject, nil
	}
	if raw[0] != '{' {
		return nil, errf(codeBadRequest, "state must be a JSON object")
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil, errf(codeBadRequest, "invalid state")
	}
	if buf.Len() > max {
		return nil, errf(codeTooLarge, "presence state exceeds %d bytes", max)
	}
	return json.RawMessage(buf.Bytes()), nil
}

// normalizeData compacts message data; absent → null.
func normalizeData(raw json.RawMessage) json.RawMessage {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return json.RawMessage("null")
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return json.RawMessage("null")
	}
	return json.RawMessage(buf.Bytes())
}

// truncate shortens s to at most n bytes (on a rune boundary) for messages.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
