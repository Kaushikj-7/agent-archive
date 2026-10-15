package muse

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"unicode/utf8"
)

const exactMax int64 = 1<<53 - 1
const maxDepth = 32
const maxString = 1 << 20
const maxNodes = 200000

var errFormat = errors.New("unsupported or incomplete Muse ordinary root")

// decodeObject bounds structure and rejects ambiguous keys before interpreting
// any authority. Numbers stay exact until their typed consumers validate them.
func decodeObject(raw []byte) (map[string]any, []string, error) {
	if !utf8.Valid(raw) || !validEscapes(raw) {
		return nil, nil, errFormat
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	r := jsonReader{decoder: d}
	v, k, e := r.value(0)
	if e != nil {
		return nil, nil, e
	}
	if _, e = d.Token(); !errors.Is(e, io.EOF) {
		return nil, nil, errFormat
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, nil, errFormat
	}
	return m, k, nil
}

type jsonReader struct {
	decoder *json.Decoder
	nodes   int
}

func (r *jsonReader) value(depth int) (any, []string, error) {
	r.nodes++
	if depth > maxDepth || r.nodes > maxNodes {
		return nil, nil, errFormat
	}
	t, e := r.decoder.Token()
	if e != nil {
		return nil, nil, errFormat
	}
	switch v := t.(type) {
	case json.Delim:
		if v == '{' {
			return r.object(depth)
		}
		if v == '[' {
			return r.array(depth)
		}
	case string:
		if len(v) > maxString {
			return nil, nil, errFormat
		}
		return v, nil, nil
	case json.Number:
		n, e := strconv.ParseInt(string(v), 10, 64)
		if e != nil || n > exactMax || n < -exactMax {
			return nil, nil, errFormat
		}
		return v, nil, nil
	case bool, nil:
		return v, nil, nil
	}
	return nil, nil, errFormat
}
func (r *jsonReader) object(depth int) (any, []string, error) {
	m := map[string]any{}
	var keys []string
	for r.decoder.More() {
		k, e := r.decoder.Token()
		if e != nil {
			return nil, nil, errFormat
		}
		key, ok := k.(string)
		if !ok || len(key) > maxString {
			return nil, nil, errFormat
		}
		if _, ok := m[key]; ok {
			return nil, nil, errFormat
		}
		value, _, e := r.value(depth + 1)
		if e != nil {
			return nil, nil, e
		}
		m[key] = value
		keys = append(keys, key)
	}
	if t, e := r.decoder.Token(); e != nil || t != json.Delim('}') {
		return nil, nil, errFormat
	}
	return m, keys, nil
}
func (r *jsonReader) array(depth int) (any, []string, error) {
	a := []any{}
	for r.decoder.More() {
		v, _, e := r.value(depth + 1)
		if e != nil {
			return nil, nil, e
		}
		a = append(a, v)
	}
	if t, e := r.decoder.Token(); e != nil || t != json.Delim(']') {
		return nil, nil, errFormat
	}
	return a, nil, nil
}

func object(v any) map[string]any { m, _ := v.(map[string]any); return m }
func str(v any) string            { s, _ := v.(string); return s }
func num(v any) int64 {
	n, ok := v.(json.Number)
	if !ok {
		return -1
	}
	i, e := strconv.ParseInt(string(n), 10, 64)
	if e != nil {
		return -1
	}
	return i
}
func keys(m map[string]any, names ...string) bool {
	if m == nil || len(m) != len(names) {
		return false
	}
	for _, k := range names {
		if _, ok := m[k]; !ok {
			return false
		}
	}
	return true
}
func stringFields(m map[string]any, names ...string) bool {
	for _, k := range names {
		if _, ok := m[k].(string); !ok {
			return false
		}
	}
	return true
}
func emptyArray(v any) bool { a, ok := v.([]any); return ok && len(a) == 0 }

// validEscapes rejects unpaired surrogate escapes which encoding/json would
// replace silently, changing the exact decoded child string before hashing.
func validEscapes(raw []byte) bool {
	inString := false
	for i := 0; i < len(raw); i++ {
		if raw[i] == '"' {
			inString = !inString
			continue
		}
		if !inString || raw[i] != '\\' {
			continue
		}
		i++
		if i >= len(raw) {
			return false
		}
		if raw[i] != 'u' {
			continue
		}
		if i+4 >= len(raw) {
			return false
		}
		n, e := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		if e != nil {
			return false
		}
		i += 4
		if n >= 0xdc00 && n <= 0xdfff {
			return false
		}
		if n >= 0xd800 && n <= 0xdbff {
			if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
				return false
			}
			low, e := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
			if e != nil || low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 6
		}
	}
	return !inString
}

// validRetainedStrings runs before JSON marshaling, which would otherwise
// replace invalid Go string bytes in an injected bundle map.
func validRetainedStrings(v any, depth int) bool {
	if depth > maxDepth {
		return false
	}
	switch x := v.(type) {
	case string:
		return utf8.ValidString(x) && len(x) <= maxString
	case map[string]any:
		for k, v := range x {
			if !utf8.ValidString(k) || len(k) > maxString || !validRetainedStrings(v, depth+1) {
				return false
			}
		}
	case []any:
		for _, v := range x {
			if !validRetainedStrings(v, depth+1) {
				return false
			}
		}
	}
	return true
}
