package jsonwire

import (
	"context"
	"encoding"
	"encoding/json"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// traversal is the shared supported wire semantics for sizing and encoding.
type traversal struct {
	ctx    context.Context
	count  *counter
	stream *streamEncoder
}

func (t *traversal) literal(s string, bound int64) error {
	if t.count != nil {
		return t.count.add(bound)
	}
	return t.stream.text(s)
}

func (t *traversal) quoted(s string) error {
	if t.count != nil {
		return t.count.text(s)
	}
	return t.stream.quote(s, false)
}

func (t *traversal) punctuation(s string) error { return t.literal(s, int64(len(s))) }

func (t *traversal) value(v reflect.Value, depth int, quoted bool) error {
	if err := t.ctx.Err(); err != nil {
		return err
	}
	if depth > 128 {
		return ErrUnsupported
	}
	if !v.IsValid() {
		return t.literal("null", 4)
	}
	if v.Kind() == reflect.Interface || v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return t.literal("null", 4)
		}
		return t.value(v.Elem(), depth+1, quoted)
	}
	if handled, err := t.special(v, quoted); handled {
		return err
	}
	switch v.Kind() {
	case reflect.Bool:
		return t.scalar(strconv.FormatBool(v.Bool()), 5, quoted)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return t.scalar(strconv.FormatInt(v.Int(), 10), 20, quoted)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return t.scalar(strconv.FormatUint(v.Uint(), 10), 20, quoted)
	case reflect.Float32, reflect.Float64:
		return t.floating(v, quoted)
	case reflect.String:
		if !quoted {
			return t.quoted(v.String())
		}
		if t.count != nil {
			before := t.count.left
			if err := t.count.text(v.String()); err != nil {
				return err
			}
			return t.count.add(before - t.count.left + 2)
		}
		return t.stream.quote(v.String(), true)
	case reflect.Slice, reflect.Array:
		return t.sequence(v, depth)
	case reflect.Map:
		return t.object(v, depth)
	case reflect.Struct:
		return t.structure(v, depth)
	case reflect.Invalid, reflect.Complex64, reflect.Complex128, reflect.Chan, reflect.Func, reflect.Interface, reflect.Pointer, reflect.UnsafePointer:
		return ErrUnsupported
	}
	return ErrUnsupported
}

func (t *traversal) scalar(s string, bound int64, quoted bool) error {
	if quoted {
		if t.count != nil {
			return t.count.add(bound + 2)
		}
		return t.quoted(s)
	}
	return t.literal(s, bound)
}

func (t *traversal) floating(v reflect.Value, quoted bool) error {
	f := v.Float()
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return ErrUnsupported
	}
	bits := v.Type().Bits()
	abs := math.Abs(f)
	format := byte('f')
	if abs != 0 && (bits == 64 && (abs < 1e-6 || abs >= 1e21) || bits == 32 && (float32(abs) < 1e-6 || float32(abs) >= 1e21)) {
		format = 'e'
	}
	s := strconv.FormatFloat(f, format, -1, bits)
	if format == 'e' {
		if i := strings.Index(s, "e-"); i >= 0 && i+3 < len(s) && s[i+2] == '0' {
			s = s[:i+2] + s[i+3:]
		}
	}
	return t.scalar(s, 24, quoted)
}

func (t *traversal) special(v reflect.Value, quoted bool) (bool, error) {
	typ := v.Type()
	if typ.PkgPath() == "github.com/wangjohn/agent-archive/internal/archive" && typ.Name() == "ImportBatch" {
		id := v.FieldByName("id")
		if !id.IsValid() || id.Kind() != reflect.String {
			return true, ErrUnsupported
		}
		return true, t.quoted(id.String())
	}
	if !v.CanInterface() {
		return false, nil
	}
	switch x := v.Interface().(type) {
	case time.Time:
		raw, e := x.MarshalJSON()
		if e != nil {
			return true, e
		}
		return true, t.literal(string(raw), 37)
	case json.Number:
		if x == "" {
			x = "0"
		}
		if !validNumber(t.ctx, string(x)) {
			if err := t.ctx.Err(); err != nil {
				return true, err
			}
			return true, ErrUnsupported
		}
		return true, t.scalar(string(x), int64(len(x)), quoted)
	case json.RawMessage:
		if x == nil {
			return true, t.literal("null", 4)
		}
		if e := validateRaw(t.ctx, x); e != nil {
			return true, e
		}
		if t.count != nil {
			return true, t.count.rawMessage(x)
		}
		return true, t.stream.raw(x)
	case json.Marshaler, encoding.TextMarshaler:
		return true, ErrUnsupported
	}
	if reflect.PointerTo(typ).Implements(reflect.TypeFor[json.Marshaler]()) || reflect.PointerTo(typ).Implements(reflect.TypeFor[encoding.TextMarshaler]()) {
		return true, ErrUnsupported
	}
	return false, nil
}

func (t *traversal) sequence(v reflect.Value, depth int) error {
	if v.Kind() == reflect.Slice {
		if v.IsNil() {
			return t.literal("null", 4)
		}
		if v.Type().Elem().Kind() == reflect.Uint8 {
			if t.count != nil {
				return t.count.add(2 + ((int64(v.Len())+2)/3)*4)
			}
			return t.stream.base64(v.Bytes())
		}
	}
	if e := t.punctuation("["); e != nil {
		return e
	}
	for i := range v.Len() {
		if i > 0 {
			if e := t.punctuation(","); e != nil {
				return e
			}
		}
		if e := t.value(v.Index(i), depth+1, false); e != nil {
			return e
		}
	}
	return t.punctuation("]")
}

func (t *traversal) object(v reflect.Value, depth int) error {
	if v.IsNil() {
		return t.literal("null", 4)
	}
	if v.Type().Key().Kind() != reflect.String {
		return ErrUnsupported
	}
	if e := t.punctuation("{"); e != nil {
		return e
	}
	if t.stream != nil {
		return t.stream.object(t, v, depth)
	}
	it := v.MapRange()
	first := true
	for it.Next() {
		if !first {
			if e := t.punctuation(","); e != nil {
				return e
			}
		}
		first = false
		if e := t.quoted(it.Key().String()); e != nil {
			return e
		}
		if e := t.punctuation(":"); e != nil {
			return e
		}
		if e := t.value(it.Value(), depth+1, false); e != nil {
			return e
		}
	}
	return t.punctuation("}")
}

type wireField struct {
	name   string
	value  reflect.Value
	quoted bool
	skip   bool
}

func field(v reflect.Value, i int) (wireField, error) {
	f := v.Type().Field(i)
	value := v.Field(i)
	if f.PkgPath != "" && !f.Anonymous {
		return wireField{skip: true}, nil
	}
	tag := f.Tag.Get("json")
	if tag == "-" {
		return wireField{skip: true}, nil
	}
	if f.Anonymous {
		return wireField{}, ErrUnsupported
	}
	name, opts, _ := strings.Cut(tag, ",")
	if name == "" {
		name = f.Name
	}
	has := func(option string) bool { return strings.Contains(","+opts+",", ","+option+",") }
	if has("omitempty") && empty(value) {
		return wireField{skip: true}, nil
	}
	if has("omitzero") {
		zero, e := wireZero(value)
		if e != nil {
			return wireField{}, e
		}
		if zero {
			return wireField{skip: true}, nil
		}
	}
	q := has("string")
	kind := value.Kind()
	if kind == reflect.Pointer {
		kind = value.Type().Elem().Kind()
	}
	q = q && (kind == reflect.Bool || kind >= reflect.Int && kind <= reflect.Float64 || kind == reflect.String)
	return wireField{name: name, value: value, quoted: q}, nil
}

func empty(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return v.Len() == 0
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr, reflect.Float32, reflect.Float64, reflect.Interface, reflect.Pointer:
		return v.IsZero()
	case reflect.Invalid, reflect.Complex64, reflect.Complex128, reflect.Chan, reflect.Func, reflect.Struct, reflect.UnsafePointer:
		return false
	}
	return false
}

func wireZero(v reflect.Value) (bool, error) {
	if v.CanInterface() {
		if x, ok := v.Interface().(time.Time); ok {
			return x.IsZero(), nil
		}
	}
	typ := v.Type()
	if typ.PkgPath() == "github.com/wangjohn/agent-archive/internal/archive" && typ.Name() == "ImportBatch" {
		return v.FieldByName("id").String() == "", nil
	}
	zeroer := reflect.TypeFor[interface{ IsZero() bool }]()
	if typ.Implements(zeroer) || reflect.PointerTo(typ).Implements(zeroer) {
		return false, ErrUnsupported
	}
	return v.IsZero(), nil
}

func (t *traversal) structure(v reflect.Value, depth int) error {
	if v.NumField() > 128 {
		return ErrUnsupported
	}
	if t.stream != nil {
		charge := int64(v.NumField()) * 128
		if !t.stream.budget.Reserve(charge) {
			return ErrMemory
		}
		defer t.stream.budget.Release(charge)
	}
	if e := t.punctuation("{"); e != nil {
		return e
	}
	first := true
	for i := range v.NumField() {
		f, e := field(v, i)
		if e != nil {
			return e
		}
		if f.skip {
			continue
		}
		if !first {
			if e = t.punctuation(","); e != nil {
				return e
			}
		}
		first = false
		if e = t.quoted(f.name); e != nil {
			return e
		}
		if e = t.punctuation(":"); e != nil {
			return e
		}
		if e = t.value(f.value, depth+1, f.quoted); e != nil {
			return e
		}
	}
	return t.punctuation("}")
}
