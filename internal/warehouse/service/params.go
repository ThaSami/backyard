package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"

	"github.com/thasami/backyard/internal/warehouse"
)

// maxSafeInt is the largest integer a float64 (a JSON number decoded by
// encoding/json) represents exactly.
const maxSafeInt = 1 << 53

// normalizeParam validates that v is a JSON scalar and converts it to the
// canonical Go type sources receive: nil, string, bool, int64 or float64.
// Integral JSON numbers become int64 so engines that infer parameter types
// (BigQuery, Athena) see an integer for 3 rather than 3.0.
func normalizeParam(v any) (any, error) {
	switch x := v.(type) {
	case nil, string, bool, int64:
		return x, nil
	case float64:
		return normalizeFloat(x)
	case float32:
		return normalizeFloat(float64(x))
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i, nil
		}
		f, err := x.Float64()
		if err != nil {
			return nil, fmt.Errorf("invalid number %q", x.String())
		}
		return normalizeFloat(f)
	case int:
		return int64(x), nil
	case int8:
		return int64(x), nil
	case int16:
		return int64(x), nil
	case int32:
		return int64(x), nil
	case uint8:
		return int64(x), nil
	case uint16:
		return int64(x), nil
	case uint32:
		return int64(x), nil
	case uint:
		if uint64(x) > math.MaxInt64 {
			return nil, fmt.Errorf("integer %d out of range", x)
		}
		return int64(x), nil
	case uint64:
		if x > math.MaxInt64 {
			return nil, fmt.Errorf("integer %d out of range", x)
		}
		return int64(x), nil
	}
	return nil, fmt.Errorf("must be a string, number, boolean or null (got %s)", jsonKind(v))
}

func normalizeFloat(f float64) (any, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, fmt.Errorf("number %v is not finite", f)
	}
	if f == math.Trunc(f) && math.Abs(f) <= maxSafeInt {
		return int64(f), nil
	}
	return f, nil
}

// jsonKind names the JSON type of a decoded value for error messages.
func jsonKind(v any) string {
	if v == nil {
		return "null"
	}
	switch reflect.TypeOf(v).Kind() {
	case reflect.Slice, reflect.Array:
		return "array"
	case reflect.Map, reflect.Struct:
		return "object"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// positionalParams converts the params of a raw SQL request (nil or a JSON
// array) into normalized positional arguments.
func positionalParams(p any) ([]any, error) {
	p, err := decodeRaw(p)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, nil
	}
	if list, ok := p.([]any); ok {
		return normalizeList(list)
	}
	rv := reflect.ValueOf(p)
	if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array || rv.Type().Elem().Kind() == reflect.Uint8 {
		return nil, fmt.Errorf("%w: params must be an array for sql queries (got %s)", warehouse.ErrBadRequest, jsonKind(p))
	}
	list := make([]any, rv.Len())
	for i := range list {
		list[i] = rv.Index(i).Interface()
	}
	return normalizeList(list)
}

func normalizeList(list []any) ([]any, error) {
	out := make([]any, len(list))
	for i, v := range list {
		nv, err := normalizeParam(v)
		if err != nil {
			return nil, fmt.Errorf("%w: params[%d] %v", warehouse.ErrBadRequest, i, err)
		}
		out[i] = nv
	}
	return out, nil
}

// namedParams converts the params of a named query request (nil or a JSON
// object) into a normalized map, checking it against the declared names:
// every declared parameter is required and unknown ones are rejected.
func namedParams(p any, declared []string) (map[string]any, error) {
	p, err := decodeRaw(p)
	if err != nil {
		return nil, err
	}
	in := map[string]any{}
	switch x := p.(type) {
	case nil:
	case map[string]any:
		in = x
	default:
		rv := reflect.ValueOf(p)
		switch {
		case rv.Kind() == reflect.Map && rv.Type().Key().Kind() == reflect.String:
			in = make(map[string]any, rv.Len())
			iter := rv.MapRange()
			for iter.Next() {
				in[iter.Key().String()] = iter.Value().Interface()
			}
		case (rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array) && rv.Len() == 0:
			// An empty array is accepted as "no parameters".
		default:
			return nil, fmt.Errorf("%w: params must be an object for named queries (got %s)", warehouse.ErrBadRequest, jsonKind(p))
		}
	}

	known := make(map[string]bool, len(declared))
	for _, name := range declared {
		known[name] = true
	}
	var unknown []string
	for name := range in {
		if !known[name] {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("%w: unknown parameter(s) %s", warehouse.ErrBadRequest, quoteList(unknown))
	}
	var missing []string
	out := make(map[string]any, len(declared))
	for _, name := range declared {
		v, ok := in[name]
		if !ok {
			missing = append(missing, name)
			continue
		}
		nv, err := normalizeParam(v)
		if err != nil {
			return nil, fmt.Errorf("%w: parameter %q %v", warehouse.ErrBadRequest, name, err)
		}
		out[name] = nv
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: missing parameter(s) %s", warehouse.ErrBadRequest, quoteList(missing))
	}
	return out, nil
}

// decodeRaw decodes params given as undecoded JSON (json.RawMessage), so
// callers may pass the request body's "params" member through untouched.
func decodeRaw(p any) (any, error) {
	raw, ok := p.(json.RawMessage)
	if !ok {
		return p, nil
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("%w: params: %v", warehouse.ErrBadRequest, err)
	}
	return v, nil
}

// quoteList renders up to 10 names for an error message.
func quoteList(names []string) string {
	const limit = 10
	q := make([]string, 0, min(len(names), limit)+1)
	for i, n := range names {
		if i == limit {
			q = append(q, fmt.Sprintf("… (%d more)", len(names)-limit))
			break
		}
		q = append(q, fmt.Sprintf("%q", truncate(n, 64)))
	}
	return strings.Join(q, ", ")
}
