package ai

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// JSONInstruction is appended to the system prompt when a request asks for a
// JSON response (ChatRequest.JSON). Providers with a native JSON mode use it
// as well: OpenAI requires the word "JSON" to appear in the prompt.
const JSONInstruction = "Respond with a single valid JSON object and nothing else: " +
	"no Markdown code fences, no commentary before or after it."

// Effort levels accepted in ChatRequest.Effort and the effort config fields.
var effortLevels = map[string]bool{"low": true, "medium": true, "high": true, "xhigh": true, "max": true}

// ValidEffort reports whether e is a known effort level (low, medium, high,
// xhigh, max). The empty string is not a valid level.
func ValidEffort(e string) bool { return effortLevels[e] }

// SystemWithJSON returns system with JSONInstruction appended.
func SystemWithJSON(system string) string {
	system = strings.TrimSpace(system)
	if system == "" {
		return JSONInstruction
	}
	return system + "\n\n" + JSONInstruction
}

// StripCodeFences extracts a JSON document from a model answer that was asked
// for JSON. It removes surrounding Markdown code fences (```json … ```) and,
// when the text still is not valid JSON, falls back to the outermost {…} or
// […] span if that span is valid JSON. Otherwise the trimmed input is returned
// unchanged, so callers never lose content.
func StripCodeFences(s string) string {
	t := strings.TrimSpace(s)
	if t == "" {
		return t
	}
	if json.Valid([]byte(t)) {
		return t
	}
	if i := strings.Index(t, "```"); i >= 0 {
		rest := t[i+3:]
		// Drop the info string (e.g. "json") up to the end of the line.
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			rest = rest[nl+1:]
		} else {
			rest = strings.TrimLeft(rest, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")
		}
		if j := strings.Index(rest, "```"); j >= 0 {
			rest = rest[:j]
		}
		inner := strings.TrimSpace(rest)
		if json.Valid([]byte(inner)) {
			return inner
		}
		if span, ok := jsonSpan(inner); ok {
			return span
		}
		return inner
	}
	if span, ok := jsonSpan(t); ok {
		return span
	}
	return t
}

// jsonSpan returns the outermost object or array in s if it is valid JSON.
func jsonSpan(s string) (string, bool) {
	for _, pair := range [][2]byte{{'{', '}'}, {'[', ']'}} {
		start := strings.IndexByte(s, pair[0])
		end := strings.LastIndexByte(s, pair[1])
		if start >= 0 && end > start {
			c := s[start : end+1]
			if json.Valid([]byte(c)) {
				return c, true
			}
		}
	}
	return "", false
}

// OpaqueUserID maps an end-user identifier (often an email address) to a
// stable opaque id suitable for provider abuse-monitoring fields such as
// Anthropic's metadata.user_id or OpenAI's user. Providers ask callers not to
// send personal data there. The empty string maps to the empty string.
func OpaqueUserID(user string) string {
	if user == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("backyard/ai-user\x00" + user))
	return "u_" + hex.EncodeToString(sum[:16])
}

// KeyedUserID is OpaqueUserID with a secret key (HMAC-SHA256): without the
// key, a provider cannot confirm a guessed email address by hashing it. An
// empty key falls back to OpaqueUserID. The empty user maps to "".
func KeyedUserID(key []byte, user string) string {
	if user == "" {
		return ""
	}
	if len(key) == 0 {
		return OpaqueUserID(user)
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte("backyard/ai-user\x00" + user))
	return "u_" + hex.EncodeToString(m.Sum(nil)[:16])
}

// TextOf concatenates the text parts of a message (images are skipped).
func TextOf(parts []Part) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// ParseDataURL decodes a base64 "data:" URL (data:image/png;base64,....). ok
// is false when s is not a base64 data URL or does not decode.
func ParseDataURL(s string) (data []byte, mediaType string, ok bool) {
	rest, found := strings.CutPrefix(s, "data:")
	if !found {
		return nil, "", false
	}
	meta, payload, found := strings.Cut(rest, ",")
	if !found {
		return nil, "", false
	}
	params := strings.Split(meta, ";")
	if len(params) < 2 || !strings.EqualFold(params[len(params)-1], "base64") {
		return nil, "", false
	}
	mediaType = strings.ToLower(strings.TrimSpace(params[0]))
	payload = strings.TrimSpace(payload)
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		if data, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(payload, "=")); err != nil {
			return nil, "", false
		}
	}
	return data, mediaType, true
}
