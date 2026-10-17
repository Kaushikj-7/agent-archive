package agentapi

import (
	"bytes"
	"encoding/json"
	"errors"
)

// UnmarshalJSON closes the native private protocol before any queue owner can
// interpret it. Future records return an error and remain untouched on disk.
// Legacy versions retain their existing unknown-field compatibility.
func (i *AdmissionIntent) UnmarshalJSON(raw []byte) error {
	type plain AdmissionIntent
	var peek struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(raw, &peek); err != nil {
		return err
	}
	if peek.Version != 2 {
		var next plain
		if err := json.Unmarshal(raw, &next); err != nil {
			return err
		}
		*i = AdmissionIntent(next)
		return nil
	}
	if len(raw) > 16<<10 {
		return errors.New("native admission intent exceeds bound")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return errors.New("invalid admission intent")
	}
	seen := map[string]bool{}
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return errors.New("ambiguous admission intent")
		}
		seen[key] = true
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return err
		}
	}
	var next plain
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&next); err != nil {
		return err
	}
	*i = AdmissionIntent(next)
	return nil
}
