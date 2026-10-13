package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

// Protected staged and scoped authorities use exact selecting fields; older
// configuration decoding remains unchanged.
func checkStagedConfigAuthority(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return err
	}
	staged := false
	schemaExact := true
	seen := map[string]int{}
	var schema json.RawMessage
	for d.More() {
		token, err = d.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok {
			return errors.New("invalid configuration key")
		}
		var value json.RawMessage
		if err = d.Decode(&value); err != nil {
			return err
		}
		for _, name := range []stagedConfigField{stagedConfigSchemaField, stagedConfigSelectionField, stagedConfigCursorField, stagedConfigPickerField} {
			if strings.EqualFold(key, string(name)) {
				seen[string(name)]++
				if name == stagedConfigSchemaField {
					schema = value
					schemaExact = schemaExact && key == string(name)
				}
				if name == stagedConfigSelectionField || name == stagedConfigCursorField || name == stagedConfigPickerField {
					staged = true
					if key != string(name) || seen[string(name)] != 1 || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
						return errors.New("invalid staged configuration authority")
					}
				}
			}
		}
	}
	if !staged {
		return nil
	}
	if !schemaExact || seen["schema_version"] != 1 {
		return errors.New("ambiguous staged writer")
	}
	return checkStagedWriterObject(schema)
}

func checkStagedWriterObject(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return errors.New("staged writer must be a protected object")
	}
	seen := map[string]bool{}
	for d.More() {
		token, err = d.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok || (stagedWriterField(key) != stagedWriterVersionField && stagedWriterField(key) != stagedWriterNameField) || seen[key] {
			return errors.New("ambiguous staged writer field")
		}
		seen[key] = true
		var value json.RawMessage
		if err = d.Decode(&value); err != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return errors.New("invalid staged writer field")
		}
	}
	if len(seen) != 2 {
		return errors.New("incomplete staged writer")
	}
	return nil
}

type stagedWriterField string

const (
	stagedWriterVersionField stagedWriterField = "version"
	stagedWriterNameField    stagedWriterField = "writer"
)

type stagedConfigField string

const (
	stagedConfigSchemaField    stagedConfigField = "schema_version"
	stagedConfigSelectionField stagedConfigField = "staged_selection_protection"
	stagedConfigCursorField    stagedConfigField = "settled_cursor_selection_protection"
	stagedConfigPickerField    stagedConfigField = "scoped_picker_protection"
)
