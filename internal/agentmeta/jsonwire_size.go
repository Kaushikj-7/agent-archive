// JSONWireBound preflights a bounded JSON wire reservation without encoding
// a complete document. Allocator and map/slice overhead are measured separately.
package agentmeta

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"
)

// ErrJSONWireLimit refuses an encoded value beyond its prospective reservation.
var ErrJSONWireLimit = errors.New("JSON wire size exceeds reservation limit")

// ErrJSONWireUnsupported refuses unknown encoding behavior or excessive nesting.
var ErrJSONWireUnsupported = errors.New("unsupported JSON preflight value")

// JSONWireBound counts an upper bound for encoding/json output. Omitted struct fields
// are included, making this conservative without guessing a heap multiplier.
// Custom marshalers are refused except the explicit bounded wire types below.
func JSONWireBound(ctx context.Context, value any, limit int64) (int64, error) {
	c := jsonWireCounter{ctx: ctx, left: limit}
	err := c.value(reflect.ValueOf(value), 0)
	return limit - c.left, err
}

type jsonWireCounter struct {
	ctx  context.Context
	left int64
}

func (c *jsonWireCounter) add(n int64) error {
	if n < 0 || n > c.left {
		return ErrJSONWireLimit
	}
	c.left -= n
	return nil
}

func (c *jsonWireCounter) text(s string) error {
	if err := c.add(2); err != nil {
		return err
	}
	for i := 0; i < len(s); {
		n := int64(1)
		b := s[i]
		switch {
		case b == '"' || b == '\\' || b == '\n' || b == '\r' || b == '\t' || b == '\b' || b == '\f':
			n = 2
			i++
		case b < 0x20 || b == '<' || b == '>' || b == '&':
			n = 6
			i++
		case b < utf8.RuneSelf:
			i++
		default:
			r, size := utf8.DecodeRuneInString(s[i:])
			i += size
			n = int64(size)
			if r == utf8.RuneError && size == 1 || r == '\u2028' || r == '\u2029' {
				n = 6
			}
		}
		if err := c.add(n); err != nil {
			return err
		}
	}
	return nil
}

func (c *jsonWireCounter) value(v reflect.Value, depth int) error {
	if err := c.ctx.Err(); err != nil {
		return err
	}
	if depth > 128 {
		return ErrJSONWireUnsupported
	}
	if !v.IsValid() {
		return c.add(4)
	}
	if handled, err := c.special(v); handled {
		return err
	}
	switch v.Kind() {
	case reflect.Interface, reflect.Pointer:
		if v.IsNil() {
			return c.add(4)
		}
		return c.value(v.Elem(), depth+1)
	case reflect.Bool:
		return c.add(5)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return c.add(20)
	case reflect.Float32, reflect.Float64:
		return c.add(24)
	case reflect.String:
		return c.text(v.String())
	case reflect.Slice, reflect.Array:
		return c.sequence(v, depth)
	case reflect.Map:
		return c.object(v, depth)
	case reflect.Struct:
		return c.structure(v, depth)
	case reflect.Invalid, reflect.Complex64, reflect.Complex128, reflect.Chan, reflect.Func, reflect.UnsafePointer:
		return ErrJSONWireUnsupported
	default:
		return ErrJSONWireUnsupported
	}
}

func (c *jsonWireCounter) special(v reflect.Value) (bool, error) {
	// ImportBatch is the archive's one bounded string wrapper. Keep this helper
	// standard-library-only and size its recorded field without invoking its
	// marshaler (or accepting arbitrary custom encoding behavior).
	batch := v
	if batch.Kind() == reflect.Pointer {
		batch = batch.Elem()
	}
	typ := v.Type()
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ.PkgPath() == "github.com/wangjohn/agent-archive/internal/archive" && typ.Name() == "ImportBatch" {
		if !batch.IsValid() {
			return true, c.add(4)
		}
		id := batch.FieldByName("id")
		if !id.IsValid() || id.Kind() != reflect.String {
			return true, ErrJSONWireUnsupported
		}
		return true, c.text(id.String())
	}

	if v.CanInterface() {
		switch x := v.Interface().(type) {
		case *time.Time:
			if x == nil {
				return true, c.add(4)
			}
			return true, c.add(37)
		case time.Time:
			return true, c.add(37) // quoted RFC3339Nano, including maximum precision and offset
		case json.Number:
			return true, c.add(int64(len(x)))
		case json.RawMessage:
			return true, c.rawMessage(x)
		case json.Marshaler:
			return true, ErrJSONWireUnsupported
		}
	}
	return false, nil
}

func (c *jsonWireCounter) sequence(v reflect.Value, depth int) error {
	if v.Kind() == reflect.Slice {
		if v.IsNil() {
			return c.add(4)
		}
		if v.Type().Elem().Kind() == reflect.Uint8 {
			groups := (int64(v.Len()) + 2) / 3
			if c.left < 2 || groups > (c.left-2)/4 {
				return ErrJSONWireLimit
			}
			return c.add(2 + groups*4)
		}
	}

	if err := c.add(2 + int64(v.Len())); err != nil {
		return err
	}
	for i := range v.Len() {
		if err := c.value(v.Index(i), depth+1); err != nil {
			return err
		}
	}
	return nil

}

func (c *jsonWireCounter) object(v reflect.Value, depth int) error {
	if v.IsNil() {
		return c.add(4)
	}
	if v.Type().Key().Kind() != reflect.String {
		return ErrJSONWireUnsupported
	}
	if int64(v.Len()) > (c.left-2)/2 {
		return ErrJSONWireLimit
	}
	if err := c.add(2 + int64(v.Len())*2); err != nil {
		return err
	}
	it := v.MapRange()
	for it.Next() {
		if err := c.text(it.Key().String()); err != nil {
			return err
		}
		if err := c.value(it.Value(), depth+1); err != nil {
			return err
		}
	}
	return nil

}

func (c *jsonWireCounter) structure(v reflect.Value, depth int) error {
	if err := c.add(2); err != nil {
		return err
	}
	typ := v.Type()
	for i := range v.NumField() {
		f := typ.Field(i)
		if f.PkgPath != "" {
			continue
		}
		tag := strings.Split(f.Tag.Get("json"), ",")
		if tag[0] == "-" {
			continue
		}
		name := tag[0]
		if name == "" {
			name = f.Name
		}
		if err := c.text(name); err != nil {
			return err
		}
		if err := c.add(2); err != nil {
			return err
		}
		if len(tag) > 1 && strings.Contains(f.Tag.Get("json"), ",string") {
			if err := c.add(2); err != nil {
				return err
			}
		}
		if err := c.value(v.Field(i), depth+1); err != nil {
			return err
		}
	}
	return nil
}

func (c *jsonWireCounter) rawMessage(x json.RawMessage) error {
	if x == nil {
		return c.add(4)
	}
	if err := c.add(int64(len(x))); err != nil {
		return err
	}
	// encoding/json compacts RawMessage and applies HTML escaping.
	for i := 0; i < len(x); i++ {
		if x[i] == '<' || x[i] == '>' || x[i] == '&' {
			if err := c.add(5); err != nil {
				return err
			}
		}
		if i+2 < len(x) && x[i] == 0xe2 && x[i+1] == 0x80 && (x[i+2] == 0xa8 || x[i+2] == 0xa9) {
			if err := c.add(3); err != nil {
				return err
			}
			i += 2
		}
	}
	return nil
}
