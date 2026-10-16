package state

import (
	"bufio"
	"context"
	"encoding"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
)

// streamJSONBuffer is the write buffer a streamed encoding holds.
const streamJSONBuffer = 32 << 10

// streamJSON writes value exactly as json.NewEncoder(w).Encode(value) does,
// without holding the whole encoding: a struct is written field by field, a
// slice of composite values element by element and a byte slice as base64
// in chunks, each other value (a leaf) encoded on its own with json.Marshal,
// whose bytes are the ones the whole encoding carries for it. Only one
// leaf's encoding is held at a time, charged to reservation for as long as
// it is held (bounded first by agentmeta.JSONWireBound), so a pending
// publication that carries a long thread's whole source and records is
// written with about one record's memory rather than the whole file's.
func streamJSON(ctx context.Context, w io.Writer, value any, reservation stateReservation) error {
	if !reservation.Reserve(streamJSONBuffer) {
		return errStateBudget
	}
	defer reservation.Release(streamJSONBuffer)
	s := jsonStreamer{ctx: ctx, w: bufio.NewWriterSize(w, streamJSONBuffer), reservation: reservation}
	if err := s.value(reflect.ValueOf(value)); err != nil {
		return err
	}
	if err := s.w.WriteByte('\n'); err != nil {
		return err
	}
	return s.w.Flush()
}

type jsonStreamer struct {
	ctx         context.Context
	w           *bufio.Writer
	reservation stateReservation
}

var (
	jsonMarshalerType = reflect.TypeFor[json.Marshaler]()
	textMarshalerType = reflect.TypeFor[encoding.TextMarshaler]()
)

// streamable reports whether v's encoding is written in parts: a plain
// struct (no marshaler of its own) or a slice. Anything else is a leaf.
func streamable(t reflect.Type) bool {
	if t.Implements(jsonMarshalerType) || t.Implements(textMarshalerType) || reflect.PointerTo(t).Implements(jsonMarshalerType) || reflect.PointerTo(t).Implements(textMarshalerType) {
		return false
	}
	switch t.Kind() {
	case reflect.Struct:
		return true
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			return true // base64, unless the element type has a marshaler (below)
		}
		switch t.Elem().Kind() {
		case reflect.Map, reflect.Struct, reflect.Slice, reflect.Interface, reflect.Pointer, reflect.String:
			return true
		}
	}
	return false
}

func (s *jsonStreamer) value(v reflect.Value) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	if !v.IsValid() || !streamable(v.Type()) {
		return s.leaf(v)
	}
	switch v.Kind() {
	case reflect.Struct:
		return s.structValue(v)
	default:
		return s.slice(v)
	}
}

// leaf writes json.Marshal's encoding of v, charged while it is held.
func (s *jsonStreamer) leaf(v reflect.Value) error {
	var value any
	if v.IsValid() {
		value = v.Interface()
	}
	limit := s.reservation.Available() - 1
	n, err := agentmeta.JSONWireBound(s.ctx, value, limit)
	if errors.Is(err, agentmeta.ErrJSONWireUnsupported) {
		// A custom marshaler JSONWireBound does not know: charged after.
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if !s.reservation.Reserve(int64(len(data))) {
			return errStateBudget
		}
		defer s.reservation.Release(int64(len(data)))
		_, err = s.w.Write(data)
		return err
	}
	if err != nil {
		if errors.Is(err, agentmeta.ErrJSONWireLimit) {
			// Past all that is left, as the whole encoding's refusal was.
			s.reservation.Refuse(limit + 2)
		}
		return errors.Join(errStateBudget, err)
	}
	if !s.reservation.Reserve(n) {
		return errStateBudget
	}
	defer s.reservation.Release(n)
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = s.w.Write(data)
	return err
}

func (s *jsonStreamer) slice(v reflect.Value) error {
	if v.IsNil() {
		_, err := s.w.WriteString("null")
		return err
	}
	if v.Type().Elem().Kind() == reflect.Uint8 {
		return s.bytes(v.Bytes())
	}
	if err := s.w.WriteByte('['); err != nil {
		return err
	}
	for i := range v.Len() {
		if i > 0 {
			if err := s.w.WriteByte(','); err != nil {
				return err
			}
		}
		if err := s.leaf(v.Index(i)); err != nil {
			return err
		}
	}
	return s.w.WriteByte(']')
}

// bytes writes data as encoding/json writes a []byte: a base64 string.
func (s *jsonStreamer) bytes(data []byte) error {
	if err := s.w.WriteByte('"'); err != nil {
		return err
	}
	enc := base64.NewEncoder(base64.StdEncoding, s.w)
	for len(data) > 0 {
		if err := s.ctx.Err(); err != nil {
			return err
		}
		n := min(len(data), 48<<10)
		if _, err := enc.Write(data[:n]); err != nil {
			return err
		}
		data = data[n:]
	}
	if err := enc.Close(); err != nil {
		return err
	}
	return s.w.WriteByte('"')
}

// structValue writes v's fields as encoding/json does: exported fields in
// order, named by their json tag, omitempty honoured. A struct whose fields
// this does not follow exactly (an embedded field, a ",string" or
// "omitzero" option) is written as a leaf.
func (s *jsonStreamer) structValue(v reflect.Value) error {
	t := v.Type()
	type field struct {
		index     int
		name      string
		omitEmpty bool
	}
	fields := make([]field, 0, t.NumField())
	for i := range t.NumField() {
		f := t.Field(i)
		if f.Anonymous {
			return s.leaf(v)
		}
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, options, _ := strings.Cut(tag, ",")
		omitEmpty := false
		for option := range strings.SplitSeq(options, ",") {
			switch option {
			case "omitempty":
				omitEmpty = true
			case "":
			default:
				return s.leaf(v)
			}
		}
		if name == "" {
			name = f.Name
		}
		fields = append(fields, field{index: i, name: name, omitEmpty: omitEmpty})
	}
	if err := s.w.WriteByte('{'); err != nil {
		return err
	}
	first := true
	for _, f := range fields {
		fv := v.Field(f.index)
		if f.omitEmpty && emptyJSONValue(fv) {
			continue
		}
		if !first {
			if err := s.w.WriteByte(','); err != nil {
				return err
			}
		}
		first = false
		key, err := json.Marshal(f.name)
		if err != nil {
			return err
		}
		if _, err := s.w.Write(append(key, ':')); err != nil {
			return err
		}
		if err := s.value(fv); err != nil {
			return err
		}
	}
	return s.w.WriteByte('}')
}

// emptyJSONValue is encoding/json's omitempty test.
func emptyJSONValue(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return v.Len() == 0
	case reflect.Bool:
		return !v.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return v.Uint() == 0
	case reflect.Float32, reflect.Float64:
		return v.Float() == 0
	case reflect.Interface, reflect.Pointer:
		return v.IsNil()
	}
	return false
}
