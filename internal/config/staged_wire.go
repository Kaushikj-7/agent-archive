package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

// Only the new authority is tightened; old configuration decoding is unchanged.
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
		for _, name := range []string{"schema_version", "staged_selection_protection", "settled_cursor_selection_protection"} {
			if strings.EqualFold(key, name) {
				seen[name]++
				if name == "schema_version" {
					schema = value
					schemaExact = schemaExact && key == name
				}
				if name == "staged_selection_protection" || name == "settled_cursor_selection_protection" {
					staged = true
					if key != name || seen[name] != 1 || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
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
