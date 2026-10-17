// Package jsonwire preflights a bounded JSON wire reservation without encoding
// a complete document. Allocator and map/slice overhead are measured separately.
package jsonwire

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"unicode/utf8"
)

// ErrLimit refuses an encoded value beyond its prospective reservation.
var ErrLimit = errors.New("JSON wire size exceeds reservation limit")

// ErrUnsupported refuses unknown encoding behavior or excessive nesting.
var ErrUnsupported = errors.New("unsupported JSON preflight value")

// Bound counts a conservative upper bound for supported default JSON output.
// It shares field inclusion and value validation with the streaming encoder.
// Custom marshalers are refused except the explicit bounded wire types below.
func Bound(ctx context.Context, value any, limit int64) (int64, error) {
	c := counter{ctx: ctx, left: limit}
	err := c.value(reflect.ValueOf(value), 0)
	return limit - c.left, err
}

type counter struct {
	ctx  context.Context
	left int64
}

func (c *counter) add(n int64) error {
	if n < 0 || n > c.left {
		return ErrLimit
	}
	c.left -= n
	return nil
}

func (c *counter) text(s string) error {
	if err := c.add(2); err != nil {
		return err
	}
	checkedAt := -256
	for i := 0; i < len(s); {
		if i-checkedAt >= 256 {
			checkedAt = i
			if err := c.ctx.Err(); err != nil {
				return err
			}
		}
		n := int64(1)
		b := s[i]
		if safeASCII(b) {
			start := i
			for i < len(s) && i-start < (32<<10) && safeASCII(s[i]) {
				i++
			}
			if err := c.add(int64(i - start)); err != nil {
				return err
			}
			continue
		}
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

func (c *counter) value(v reflect.Value, depth int) error {
	return (&traversal{ctx: c.ctx, count: c}).value(v, depth, false)
}

func (c *counter) rawMessage(x json.RawMessage) error {
	if x == nil {
		return c.add(4)
	}
	if err := c.add(int64(len(x))); err != nil {
		return err
	}
	// encoding/json compacts RawMessage and applies HTML escaping.
	for i := 0; i < len(x); i++ {
		if i%256 == 0 {
			if err := c.ctx.Err(); err != nil {
				return err
			}
		}
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
