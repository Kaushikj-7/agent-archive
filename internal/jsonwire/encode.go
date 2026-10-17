package jsonwire

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"math"
	"reflect"
	"slices"
	"unicode/utf8"
)

// MemoryBudget is the existing caller-owned byte ledger, not a storage capability.
type MemoryBudget interface {
	Reserve(int64) bool
	Release(int64)
	Available() int64
}

// ErrMemory refuses encoding before allocating unfunded scratch or metadata.
var ErrMemory = errors.New("JSON encoder exceeds shared memory budget")

// Encode streams the supported default JSON wire and one final newline. Bound
// must preflight the same value before the caller creates an atomic output.
func Encode(ctx context.Context, w io.Writer, value any, budget MemoryBudget) error {
	const scratch = 64 << 10
	if budget == nil || !budget.Reserve(scratch) {
		return ErrMemory
	}
	defer budget.Release(scratch)
	if _, err := Bound(ctx, value, math.MaxInt64); err != nil {
		return err
	}
	e := &streamEncoder{ctx: ctx, writer: w, budget: budget}
	t := traversal{ctx: ctx, stream: e}
	if err := t.value(reflect.ValueOf(value), 0, false); err != nil {
		return err
	}
	if err := e.text("\n"); err != nil {
		return err
	}
	return e.flush()
}

type streamEncoder struct {
	ctx    context.Context
	writer io.Writer
	budget MemoryBudget
	buffer [32 << 10]byte
	used   int
}

func (e *streamEncoder) flush() error {
	if err := e.ctx.Err(); err != nil {
		return err
	}
	if e.used == 0 {
		return nil
	}
	n, err := e.writer.Write(e.buffer[:e.used])
	if err == nil && n != e.used {
		err = io.ErrShortWrite
	}
	e.used = 0
	return err
}

func (e *streamEncoder) text(s string) error {
	for len(s) > 0 {
		if err := e.ctx.Err(); err != nil {
			return err
		}
		n := copy(e.buffer[e.used:], s)
		e.used += n
		s = s[n:]
		if e.used == len(e.buffer) {
			if err := e.flush(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *streamEncoder) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		if err := e.ctx.Err(); err != nil {
			return total - len(p), err
		}
		n := copy(e.buffer[e.used:], p)
		e.used += n
		p = p[n:]
		if e.used == len(e.buffer) {
			if err := e.flush(); err != nil {
				return total - len(p), err
			}
		}
	}
	return total, nil
}

func (e *streamEncoder) quote(s string, double bool) error {
	if err := e.text("\""); err != nil {
		return err
	}
	emit := e.text
	if double {
		emit = func(part string) error { return e.escaped(part, e.text) }
		if err := emit("\""); err != nil {
			return err
		}
	}
	if err := e.escaped(s, emit); err != nil {
		return err
	}
	if double {
		if err := emit("\""); err != nil {
			return err
		}
	}
	return e.text("\"")
}

func safeASCII(b byte) bool {
	return b >= 0x20 && b < utf8.RuneSelf && b != '"' && b != '\\' && b != '<' && b != '>' && b != '&'
}

func (e *streamEncoder) escaped(s string, emit func(string) error) error {
	const hex = "0123456789abcdef"
	for i := 0; i < len(s); {
		if err := e.ctx.Err(); err != nil {
			return err
		}
		b := s[i]
		if safeASCII(b) {
			start := i
			for i < len(s) && i-start < (32<<10) {
				b = s[i]
				if !safeASCII(b) {
					break
				}
				i++
			}
			if err := emit(s[start:i]); err != nil {
				return err
			}
			continue
		}
		size := 1
		part := s[i : i+1]
		switch b {
		case '"':
			part = "\\\""
		case '\\':
			part = "\\\\"
		case '\n':
			part = "\\n"
		case '\r':
			part = "\\r"
		case '\t':
			part = "\\t"
		case '\b':
			part = "\\b"
		case '\f':
			part = "\\f"
		default:
			if b < 0x20 || b == '<' || b == '>' || b == '&' {
				raw := [6]byte{'\\', 'u', '0', '0', hex[b>>4], hex[b&15]}
				if err := emit(string(raw[:])); err != nil {
					return err
				}
				i++
				continue
			}
			if b >= utf8.RuneSelf {
				r, n := utf8.DecodeRuneInString(s[i:])
				size = n
				part = s[i : i+n]
				if r == utf8.RuneError && n == 1 {
					part = "�"
				}
				if r == '\u2028' {
					part = "\\u2028"
				}
				if r == '\u2029' {
					part = "\\u2029"
				}
			}
		}
		if err := emit(part); err != nil {
			return err
		}
		i += size
	}
	return nil
}

func (e *streamEncoder) base64(data []byte) error {
	if err := e.text("\""); err != nil {
		return err
	}
	encoder := base64.NewEncoder(base64.StdEncoding, e)
	_, err := encoder.Write(data)
	err = errors.Join(err, encoder.Close())
	if err != nil {
		return err
	}
	return e.text("\"")
}

func (e *streamEncoder) object(t *traversal, v reflect.Value, depth int) error {
	if int64(v.Len()) > math.MaxInt64/64 {
		return ErrMemory
	}
	charge := int64(v.Len()) * 64
	if !e.budget.Reserve(charge) {
		return ErrMemory
	}
	defer e.budget.Release(charge)
	keys := make([]reflect.Value, 0, v.Len())
	it := v.MapRange()
	for it.Next() {
		if err := e.ctx.Err(); err != nil {
			return err
		}
		keys = append(keys, it.Key())
	}
	var sortErr error
	slices.SortFunc(keys, func(a, b reflect.Value) int {
		if sortErr != nil {
			return 0
		}
		left, right := a.String(), b.String()
		for i := range min(len(left), len(right)) {
			if i%256 == 0 {
				if sortErr = e.ctx.Err(); sortErr != nil {
					return 0
				}
			}
			if left[i] < right[i] {
				return -1
			}
			if left[i] > right[i] {
				return 1
			}
		}
		if len(left) < len(right) {
			return -1
		}
		if len(left) > len(right) {
			return 1
		}
		return 0
	})
	if sortErr != nil {
		return sortErr
	}
	for i, key := range keys {
		if err := e.ctx.Err(); err != nil {
			return err
		}
		if i > 0 {
			if err := t.punctuation(","); err != nil {
				return err
			}
		}
		if err := t.quoted(key.String()); err != nil {
			return err
		}
		if err := t.punctuation(":"); err != nil {
			return err
		}
		if err := t.value(v.MapIndex(key), depth+1, false); err != nil {
			return err
		}
	}
	return t.punctuation("}")
}

func (e *streamEncoder) raw(data []byte) error {
	inString, escaped := false, false
	for i := 0; i < len(data); i++ {
		if err := e.ctx.Err(); err != nil {
			return err
		}
		b := data[i]
		if !inString && (b == ' ' || b == '\n' || b == '\r' || b == '\t') {
			continue
		}
		if b == '<' || b == '>' || b == '&' {
			const hex = "0123456789abcdef"
			part := [6]byte{'\\', 'u', '0', '0', hex[b>>4], hex[b&15]}
			if err := e.text(string(part[:])); err != nil {
				return err
			}
		} else if b == 0xe2 && i+2 < len(data) && data[i+1] == 0x80 && (data[i+2] == 0xa8 || data[i+2] == 0xa9) {
			part := "\\u2028"
			if data[i+2] == 0xa9 {
				part = "\\u2029"
			}
			if err := e.text(part); err != nil {
				return err
			}
			i += 2
		} else {
			one := [1]byte{b}
			if _, err := e.Write(one[:]); err != nil {
				return err
			}
		}
		if inString {
			if escaped {
				escaped = false
			} else if b == '\\' {
				escaped = true
			} else if b == '"' {
				inString = false
			}
		} else if b == '"' {
			inString = true
		}
	}
	return nil
}
