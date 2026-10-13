// Package wirepolicy checks JSON authority field spelling and multiplicity.
package wirepolicy

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/wangjohn/agent-archive/internal/stateerrors"
)

// RejectDuplicateAuthorityKeys refuses invalid JSON and repeated authority keys,
// matching the supplied names without regard to case.
func RejectDuplicateAuthorityKeys(data []byte, names []string) error {
	if !json.Valid(data) {
		return stateerrors.ErrDurableStorageRecovery
	}
	seen := map[string]bool{}
	maxKey := 0
	for _, name := range names {
		maxKey = max(maxKey, len(name))
	}
	depth, expectKey := 0, false
	for i := 0; i < len(data); i++ {
		switch data[i] {
		case '{', '[':
			depth++
			if depth == 1 {
				expectKey = true
			}
		case '}', ']':
			depth--
		case ',':
			if depth == 1 {
				expectKey = true
			}
		case ':':
			if depth == 1 {
				expectKey = false
			}
		case '"':
			start := i
			for i++; i < len(data); i++ {
				if data[i] == '\\' {
					i++
					continue
				}
				if data[i] == '"' {
					break
				}
			}
			if depth != 1 || !expectKey || i-start > 6*maxKey+2 {
				continue
			}
			var key string
			if json.Unmarshal(data[start:i+1], &key) != nil {
				return stateerrors.ErrDurableStorageRecovery
			}
			canonical := ""
			for _, name := range names {
				if strings.EqualFold(key, name) {
					canonical = name
					break
				}
			}
			if canonical == "" {
				continue
			}
			key = canonical
			if seen[key] {
				return stateerrors.ErrDurableStorageRecovery
			}
			seen[key] = true
		}
	}
	return nil
}

// StrictStagedKeys requires exactly the supplied keys with nonnull values and
// rejects duplicate authority keys.
func StrictStagedKeys(raw []byte, keys []string) error {
	if err := RejectDuplicateAuthorityKeys(raw, keys); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return stateerrors.ErrDurableStorageRecovery
	}
	for _, key := range keys {
		value, present := fields[key]
		if !present || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return stateerrors.ErrDurableStorageRecovery
		}
	}
	if len(fields) != len(keys) {
		return stateerrors.ErrDurableStorageRecovery
	}
	return nil
}

// RejectStagedNullAliases refuses case aliases and null values for the supplied
// authority names, as well as duplicate authority keys.
func RejectStagedNullAliases(raw []byte, names []string) error {
	if err := RejectDuplicateAuthorityKeys(raw, names); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return stateerrors.ErrDurableStorageRecovery
	}
	for key, value := range fields {
		for _, name := range names {
			if strings.EqualFold(key, name) && (key != name || bytes.Equal(bytes.TrimSpace(value), []byte("null"))) {
				return stateerrors.ErrDurableStorageRecovery
			}
		}
	}
	return nil
}
