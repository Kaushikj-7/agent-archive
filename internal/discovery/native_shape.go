package discovery

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
)

// Preflight bounds collection counts BEFORE encoding/json allocates typed
// backing arrays. Every field is mandatory, including zero-valued EOF fields;
// duplicate/unknown/missing fields cannot become default complete observations.
func nativeShape(raw []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	counts := map[string]int{}
	if nativeShapeValue(decoder, reflect.TypeFor[nativeProgress](), "", counts) != nil {
		return false
	}
	_, err := decoder.Token()
	return errors.Is(err, io.EOF)
}
func nativeShapeValue(d *json.Decoder, t reflect.Type, key string, counts map[string]int) error {
	bad := errors.New("unsupported native observation shape")
	token, err := d.Token()
	if err != nil {
		return err
	}
	switch t.Kind() {
	case reflect.Struct:
		if token != json.Delim('{') {
			return bad
		}
		fields := map[string]reflect.Type{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			fields[strings.Split(f.Tag.Get("json"), ",")[0]] = f.Type
		}
		seen := map[string]bool{}
		for d.More() {
			name, e := d.Token()
			if e != nil {
				return e
			}
			nameString, ok := name.(string)
			if !ok || seen[nameString] || fields[nameString] == nil {
				return bad
			}
			seen[nameString] = true
			if e = nativeShapeValue(d, fields[nameString], nameString, counts); e != nil {
				return e
			}
		}
		if len(seen) != len(fields) {
			return bad
		}
		token, err = d.Token()
		if err != nil || token != json.Delim('}') {
			return bad
		}
	case reflect.Slice:
		if token == nil {
			return nil
		} // nil is only an empty collection, never coverage.
		if token != json.Delim('[') {
			return bad
		}
		for d.More() {
			counts[key]++
			limit := maxCatalog
			if key == "directories" || key == "children" {
				limit = maxDirectories
			}
			if counts[key] > limit {
				return bad
			}
			if err = nativeShapeValue(d, t.Elem(), key, counts); err != nil {
				return err
			}
		}
		token, err = d.Token()
		if err != nil || token != json.Delim(']') {
			return bad
		}
	case reflect.String:
		value, ok := token.(string)
		if !ok || len(value) > 4096 {
			return bad
		}
	default:
		if _, ok := token.(json.Number); !ok {
			return bad
		}
	}
	return nil
}
