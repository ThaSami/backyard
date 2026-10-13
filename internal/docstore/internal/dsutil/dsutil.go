// Package dsutil holds helpers shared by the docstore backends: argument
// validation, canonicalisation of document bodies and JSON encoding.
package dsutil

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/thasami/backyard/internal/docstore"
)

// CheckNamespace validates a site and collection (user or system collection).
func CheckNamespace(site, collection string) error {
	if err := docstore.ValidateSite(site); err != nil {
		return err
	}
	return docstore.ValidateCollection(collection, strings.HasPrefix(collection, "_"))
}

// MaxSystemIDBytes bounds document ids in system collections.
const MaxSystemIDBytes = 1024

// CheckKey validates site, collection and document id. Ids in user
// collections must satisfy docstore.ValidateID. System collections (names
// starting with "_") are written by Backyard itself with ids such as e-mail
// addresses (the "_system/_users" cache), so there any valid UTF-8 id of 1 to
// MaxSystemIDBytes bytes without control characters is accepted.
func CheckKey(site, collection, id string) error {
	if err := CheckNamespace(site, collection); err != nil {
		return err
	}
	if !strings.HasPrefix(collection, "_") {
		return docstore.ValidateID(id)
	}
	if id == "" || len(id) > MaxSystemIDBytes || !utf8.ValidString(id) ||
		strings.ContainsFunc(id, unicode.IsControl) {
		return fmt.Errorf("%w: document id %q", docstore.ErrInvalid, id)
	}
	return nil
}

// CheckWrite validates the key of a write like CheckKey and returns the
// actor in canonical form (see CleanActor).
func CheckWrite(site, collection, id, actor string) (string, error) {
	if err := CheckKey(site, collection, id); err != nil {
		return "", err
	}
	return CleanActor(actor)
}

// PrepareWrite turns a Create/Put body into the stored form: reserved keys
// ("id", "_meta") are dropped, the body is deep-copied into canonical JSON
// types (map[string]any, []any, float64, string, bool, nil) and encoded.
func PrepareWrite(data map[string]any) (map[string]any, []byte, error) {
	out, err := Canonical(data)
	if err != nil {
		return nil, nil, err
	}
	b, err := Marshal(out)
	if err != nil {
		return nil, nil, err
	}
	return out, b, nil
}

// PreparePatch canonicalises an Update patch (reserved keys dropped, nil
// values kept as deletion markers).
func PreparePatch(patch map[string]any) (map[string]any, error) {
	return Canonical(patch)
}

// Canonical returns a deep copy of a document body with reserved keys removed
// and every value converted to canonical JSON types (map[string]any, []any,
// float64, string, bool, nil). Values of other Go types are converted through
// encoding/json. Strings and keys are cleaned with docstore.CleanString and
// numbers with docstore.CleanNumber, and bodies nested deeper than
// docstore.MaxNestingDepth are rejected, so that every backend stores (and
// returns) exactly the same values; violations yield ErrInvalid. Nil values
// are kept (null data, or deletion markers in a patch).
func Canonical(data map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(data))
	for k, v := range data {
		if k == "id" || k == "_meta" {
			continue
		}
		ck, err := docstore.CleanString(k)
		if err != nil {
			return nil, fmt.Errorf("field name %q: %w", k, err)
		}
		cv, err := canonicalValue(v, 2)
		if err != nil {
			return nil, fmt.Errorf("%w: field %q: %v", docstore.ErrInvalid, ck, err)
		}
		out[ck] = cv
	}
	return out, nil
}

// errTooDeep reports a body nested deeper than docstore.MaxNestingDepth.
var errTooDeep = fmt.Errorf("nested deeper than %d levels", docstore.MaxNestingDepth)

// canonicalValue converts v, found at nesting level depth (the document body
// is level 1), into canonical form.
func canonicalValue(v any, depth int) (any, error) {
	switch x := v.(type) {
	case nil, bool:
		return x, nil
	case string:
		return cleanString(x)
	case float64:
		return cleanNumber(x)
	case float32:
		return cleanNumber(float64(x))
	case int:
		return float64(x), nil
	case int8:
		return float64(x), nil
	case int16:
		return float64(x), nil
	case int32:
		return float64(x), nil
	case int64:
		return float64(x), nil
	case uint:
		return float64(x), nil
	case uint8:
		return float64(x), nil
	case uint16:
		return float64(x), nil
	case uint32:
		return float64(x), nil
	case uint64:
		return float64(x), nil
	case json.Number:
		f, err := x.Float64()
		if err != nil {
			return nil, fmt.Errorf("invalid number %q", x.String())
		}
		return cleanNumber(f)
	case map[string]any:
		if depth > docstore.MaxNestingDepth {
			return nil, errTooDeep
		}
		out := make(map[string]any, len(x))
		for k, e := range x {
			ck, err := cleanString(k)
			if err != nil {
				return nil, err
			}
			ce, err := canonicalValue(e, depth+1)
			if err != nil {
				return nil, err
			}
			out[ck] = ce
		}
		return out, nil
	case []any:
		if depth > docstore.MaxNestingDepth {
			return nil, errTooDeep
		}
		out := make([]any, len(x))
		for i, e := range x {
			ce, err := canonicalValue(e, depth+1)
			if err != nil {
				return nil, err
			}
			out[i] = ce
		}
		return out, nil
	case []string:
		if depth > docstore.MaxNestingDepth {
			return nil, errTooDeep
		}
		out := make([]any, len(x))
		for i, e := range x {
			ce, err := cleanString(e)
			if err != nil {
				return nil, err
			}
			out[i] = ce
		}
		return out, nil
	case json.RawMessage:
		var out any
		if err := Unmarshal(x, &out); err != nil {
			return nil, err
		}
		return canonicalValue(out, depth)
	}
	// Anything else (structs, typed maps/slices, pointers…) goes through JSON
	// and is then canonicalised like any other value.
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return canonicalValue(out, depth)
}

// cleanString and cleanNumber unwrap the docstore helpers' ErrInvalid so that
// Canonical can wrap the message with the offending field name once.
func cleanString(s string) (string, error) {
	c, err := docstore.CleanString(s)
	if err != nil {
		return "", errors.New("strings must not contain the NUL character (U+0000)")
	}
	return c, nil
}

func cleanNumber(f float64) (float64, error) {
	c, err := docstore.CleanNumber(f)
	if err != nil {
		return 0, fmt.Errorf("number %v is not valid JSON", f)
	}
	return c, nil
}

// CleanActor canonicalises the actor recorded in created_by/updated_by like
// any other stored string (see docstore.CleanString).
func CleanActor(actor string) (string, error) {
	c, err := docstore.CleanString(actor)
	if err != nil {
		return "", fmt.Errorf("actor: %w", err)
	}
	return c, nil
}

// Marshal encodes a canonical document body as compact JSON (HTML characters
// are not escaped).
func Marshal(data map[string]any) ([]byte, error) {
	if data == nil {
		return []byte("{}"), nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(data); err != nil {
		return nil, fmt.Errorf("%w: encoding document: %v", docstore.ErrInvalid, err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// Unmarshal decodes JSON into v using canonical types (numbers as float64).
func Unmarshal(b []byte, v any) error {
	return json.Unmarshal(b, v)
}

// DecodeData decodes a stored document body. Empty input yields an empty map.
func DecodeData(b []byte) (map[string]any, error) {
	out := map[string]any{}
	if len(bytes.TrimSpace(b)) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("docstore: decoding stored document: %w", err)
	}
	if out == nil { // stored JSON "null"
		out = map[string]any{}
	}
	return out, nil
}

// MergePatch applies a canonical patch to stored data (shallow merge; nil
// deletes) and returns the merged body and its encoding.
func MergePatch(data, patch map[string]any) (map[string]any, []byte, error) {
	merged := docstore.ApplyPatch(data, patch)
	b, err := Marshal(merged)
	if err != nil {
		return nil, nil, err
	}
	return merged, b, nil
}
