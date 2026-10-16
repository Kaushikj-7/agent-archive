package discovery

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
)

type nativeShapeKey string

const (
	nativeDirectoriesKey nativeShapeKey = "directories"
	nativeChildrenKey    nativeShapeKey = "children"
)

// Preflight bounds collection counts BEFORE encoding/json allocates typed
// backing arrays. Every field is mandatory, including zero-valued EOF fields;
// duplicate/unknown/missing fields cannot become default complete observations.
func nativeShape(raw []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	counts := map[nativeShapeKey]int{}
	if nativeShapeValue(decoder, reflect.TypeFor[nativeProgress](), "", counts) != nil {
		return false
	}
	_, err := decoder.Token()
	return errors.Is(err, io.EOF)
}

func nativeShapeValue(d *json.Decoder, t reflect.Type, key nativeShapeKey, counts map[nativeShapeKey]int) error {
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
		for i := range t.NumField() {
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
			if e = nativeShapeValue(d, fields[nameString], nativeShapeKey(nameString), counts); e != nil {
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
			if key == nativeDirectoriesKey || key == nativeChildrenKey {
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
	case reflect.Int, reflect.Int64, reflect.Uint32, reflect.Uint64:
		if _, ok := token.(json.Number); !ok {
			return bad
		}
	case reflect.Invalid, reflect.Bool, reflect.Int8, reflect.Int16, reflect.Int32,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uintptr, reflect.Float32,
		reflect.Float64, reflect.Complex64, reflect.Complex128, reflect.Array,
		reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer,
		reflect.UnsafePointer:
		return bad
	default:
		return bad
	}
	return nil
}
