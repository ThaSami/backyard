package mcpremote

// JSON-RPC 2.0 framing of the MCP endpoint (SPEC §14.1, gate steps 10
// and 11): one message per POST, no batches.

import (
	"bytes"
	"encoding/json"
	"regexp"
	"unicode/utf8"
)

// JSON-RPC error codes.
const (
	codeParseError         = -32700
	codeInvalidRequest     = -32600
	codeMethodNotFound     = -32601
	codeInvalidParams      = -32602
	codeInternalError      = -32603
	codeUnsupportedVersion = -32022
)

// rpcError is the error member of a JSON-RPC response.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func newRPCError(code int, msg string) *rpcError { return &rpcError{Code: code, Message: msg} }

// rpcResponse is a JSON-RPC response; exactly one of Result and Error is set.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// nullID is the id of answers to messages whose id is unknown.
var nullID = json.RawMessage("null")

// kind classifies a parsed message.
type kind int

const (
	kindRequest      kind = iota // method and id: answered with 200
	kindNotification             // method, no id: 202
	kindResponse                 // no method, result or error: a client's answer, 202
)

// message is one parsed JSON-RPC message.
type message struct {
	kind   kind
	id     json.RawMessage // compact; nil when absent
	method string
	params json.RawMessage // nil when absent
}

// integerID matches a JSON number without fraction or exponent.
var integerID = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)

// parseMessage parses a request body. A non-nil error is answered with HTTP
// 400 and id null. A body that is not UTF-8 is no JSON text (RFC 8259 §8.1):
// encoding/json would replace each invalid byte with U+FFFD, and the Worker
// each maximal invalid sequence, so both refuse it instead.
func parseMessage(body []byte) (*message, *rpcError) {
	body = bytes.TrimSpace(body)
	if !utf8.Valid(body) || !json.Valid(body) {
		return nil, newRPCError(codeParseError, "Parse error")
	}
	switch body[0] {
	case '[':
		return nil, newRPCError(codeInvalidRequest, "Invalid Request: JSON-RPC batches are not supported")
	case '{':
	default:
		return nil, newRPCError(codeInvalidRequest, "Invalid Request")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, newRPCError(codeInvalidRequest, "Invalid Request")
	}
	invalid := newRPCError(codeInvalidRequest, "Invalid Request")
	var version string
	if raw, ok := obj["jsonrpc"]; !ok || json.Unmarshal(raw, &version) != nil || version != "2.0" {
		return nil, invalid
	}
	m := &message{}
	rawMethod, hasMethod := obj["method"]
	if hasMethod {
		if !isJSONString(rawMethod) || json.Unmarshal(rawMethod, &m.method) != nil {
			return nil, invalid
		}
	}
	if raw, ok := obj["id"]; ok {
		if !isJSONString(raw) && !integerID.Match(raw) {
			return nil, invalid // null, fractional, object, array, boolean
		}
		m.id = raw
	}
	m.params = obj["params"]
	_, hasResult := obj["result"]
	_, hasError := obj["error"]
	switch {
	case hasMethod && m.id != nil:
		m.kind = kindRequest
	case hasMethod:
		m.kind = kindNotification
	case hasResult || hasError:
		m.kind = kindResponse
	default:
		return nil, invalid
	}
	return m, nil
}

// isJSONString reports whether raw (valid JSON) is a string.
func isJSONString(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == '"'
}

// isJSONObject reports whether raw (valid JSON) is an object.
func isJSONObject(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == '{'
}
