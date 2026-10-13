package catalog

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// The reservation must not be activated until construction, mutation and reads
// maintain the complete scoped index universe. Protocol 10 remains writable.
const (
	scopedPickerProtocol   = 11
	scopedPickerSchema     = 5
	scopedPickerCapability = "scoped-picker-v1"
)

var errScopedPickerUnavailable = errors.New("scoped catalog requires complete scoped picker support; upgrade before reading or modifying this destination")

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
	next := plainHead{}
	wire := struct {
		*plainHead
		Picker json.RawMessage `json:"Picker"`
	}{plainHead: &next}
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
		return errScopedPickerUnavailable
	}
	if next.Protocol != 10 || next.Schema != 4 {
		return errors.New("unsupported catalog head protocol or schema; upgrade before accessing this destination")
	}
	*h = CatalogHead(next)
	return nil
}
