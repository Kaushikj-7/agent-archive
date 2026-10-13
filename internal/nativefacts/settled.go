// Package nativefacts owns closed native capture comparison values, never proof authority.
package nativefacts

import (
	"bytes"
	"encoding/json"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/cursorformat"
)

// SettledImportFacts records verified same-transaction native selection facts.
// Public facts alone confer no admission or publication authority.
type SettledImportFacts struct {
	Chat         agentapi.DatabaseChat `json:"chat"`
	GlobalSHA256 string                `json:"global_sha256"`
}

// UnmarshalJSON keeps persisted settled facts closed and unambiguous.
func (f *SettledImportFacts) UnmarshalJSON(raw []byte) error {
	if len(raw) > 8<<10 {
		return &cursorformat.NotCheckedError{Reason: cursorformat.UnknownFormat}
	}
	if err := exactSettledFields(raw, []string{"chat", "global_sha256"}); err != nil {
		return err
	}
	var outer map[string]json.RawMessage
	if json.Unmarshal(raw, &outer) != nil {
		return &cursorformat.NotCheckedError{Reason: cursorformat.UnknownFormat}
	}
	if err := exactSettledFields(outer["chat"], []string{"CursorFacts", "ID", "KeyID", "CreatedAt", "Folder", "WorkspaceID", "WorkspaceIdentifier", "Malformed"}); err != nil {
		return err
	}
	var chat map[string]json.RawMessage
	if json.Unmarshal(outer["chat"], &chat) != nil {
		return &cursorformat.NotCheckedError{Reason: cursorformat.UnknownFormat}
	}
	if err := exactSettledFields(chat["CursorFacts"], []string{"VersionPresent", "Version", "Relationships", "ChildIDsSHA256"}); err != nil {
		return err
	}
	type plain SettledImportFacts
	var next plain
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&next); err != nil {
		return err
	}
	*f = SettledImportFacts(next)
	return nil
}

func exactSettledFields(raw []byte, keys []string) error {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) != len(keys) {
		return &cursorformat.NotCheckedError{Reason: cursorformat.UnknownFormat}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return &cursorformat.NotCheckedError{Reason: cursorformat.UnknownFormat}
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err = decoder.Token()
		if err != nil {
			return err
		}
		name, ok := token.(string)
		if !ok || seen[name] {
			return &cursorformat.NotCheckedError{Reason: cursorformat.UnknownFormat}
		}
		seen[name] = true
		var value json.RawMessage
		if err = decoder.Decode(&value); err != nil {
			return err
		}
	}
	for _, key := range keys {
		if !seen[key] || bytes.Equal(bytes.TrimSpace(fields[key]), []byte("null")) {
			return &cursorformat.NotCheckedError{Reason: cursorformat.UnknownFormat}
		}
	}
	return nil
}
