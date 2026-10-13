package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
)

// Scoped format activation requires exhaustive construction under durable ownership.
const scopedPickerProtocol = 11
const scopedPickerSchema = 5
const scopedPickerCapability = "scoped-picker-v1"

var errScopedPickerUnavailable = errors.New("scoped catalog is incomplete or unavailable; complete the scoped catalog upgrade before modifying this destination")

type scopedPickerFormat struct {
	Version      uint64   `json:"Version"`
	RowSchema    uint64   `json:"RowSchema"`
	Capabilities []string `json:"Capabilities"`
}

// UnmarshalJSON applies the same protocol boundary to live, retained and
// recovery heads. In particular, GC cannot silently traverse a future head
// using only the old roots and then reclaim its new indexed objects.
func (h *CatalogHead) UnmarshalJSON(raw []byte) error {
	type plainHead CatalogHead
	var wire struct {
		*plainHead
		Picker json.RawMessage `json:"Picker"`
	}
	next := plainHead{}
	wire.plainHead = &next
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("invalid trailing catalog head data")
	}
	if next.Protocol == scopedPickerProtocol || next.Schema == scopedPickerSchema || len(wire.Picker) != 0 {
		var format scopedPickerFormat
		formatDecoder := json.NewDecoder(bytes.NewReader(wire.Picker))
		formatDecoder.DisallowUnknownFields()
		if formatDecoder.Decode(&format) != nil {
			return errors.New("invalid scoped catalog descriptor")
		}
		if next.Protocol != scopedPickerProtocol || next.Schema != scopedPickerSchema || len(wire.Picker) == 0 || format.Version != 1 || format.RowSchema != 1 || len(format.Capabilities) != 1 || format.Capabilities[0] != scopedPickerCapability {
			return errors.New("invalid scoped catalog protocol, schema or capabilities")
		}
		next.Picker = &format
	}
	if next.Picker == nil && (next.Protocol != 10 || next.Schema != 4) {
		return errors.New("unsupported catalog head protocol or schema; upgrade before accessing this destination")
	}
	*h = CatalogHead(next)
	return nil
}

func pickerFormat() *scopedPickerFormat {
	return &scopedPickerFormat{Version: 1, RowSchema: 1, Capabilities: []string{scopedPickerCapability}}
}

func (f *scopedPickerFormat) valid() bool {
	return f != nil && f.Version == 1 && f.RowSchema == 1 && len(f.Capabilities) == 1 && f.Capabilities[0] == scopedPickerCapability
}

func (h CatalogHead) validFormat() bool {
	return (h.Protocol == 10 && h.Schema == 4 && h.Picker == nil) || (h.Protocol == 11 && h.Schema == 5 && h.Picker.valid())
}

// Protected writes require exact head/coordinator composition, including during
// activation recovery. A seal alone is never permission to cross a format fence.
func (w *Writer) writeFormat(ctx context.Context, state admissions) error {
	h, _, err := w.Head(ctx)
	if err != nil {
		return err
	}
	if state.Upgrade != nil || h.Protocol != state.Protocol || (h.Picker != nil) != (state.Picker != nil) {
		return ErrAdmissionClosed
	}
	return nil
}
