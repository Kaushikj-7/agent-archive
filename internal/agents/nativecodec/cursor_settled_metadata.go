package nativecodec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/wangjohn/agent-archive/internal/cursorformat"
)

func rejectCursorMetadataDuplicates(ctx context.Context, raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	var readValue func(int) error
	readValue = func(depth int) error {
		if depth > 128 {
			return &cursorformat.NotCheckedError{Reason: cursorformat.UnknownFormat}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		token, err := d.Token()
		if err != nil {
			return err
		}
		delimiter, compound := token.(json.Delim)
		if !compound {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok {
					return &cursorformat.NotCheckedError{Reason: cursorformat.UnknownFormat}
				}
				normalized := strings.ToLower(name)
				if seen[normalized] {
					return &cursorformat.NotCheckedError{Reason: cursorformat.UnknownFormat}
				}
				seen[normalized] = true
				if err := readValue(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := readValue(depth + 1); err != nil {
					return err
				}
			}
		default:
			return &cursorformat.NotCheckedError{Reason: cursorformat.UnknownFormat}
		}
		_, err = d.Token()
		return err
	}
	if err := readValue(0); err != nil {
		return err
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return &cursorformat.NotCheckedError{Reason: cursorformat.UnknownFormat}
	}
	return nil
}

func ValidateSettledCursorMetadata(ctx context.Context, key string, raw []byte) error {
	if err := rejectCursorMetadataDuplicates(ctx, raw); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return &cursorformat.NotCheckedError{Reason: cursorformat.UnknownFormat}
	}
	names := []string{"_v", "composerId", "workspaceIdentifier", "subagentComposerIds", "isDraft", "fullConversationHeadersOnly", "conversation", "createdAt"}
	for name := range fields {
		for _, canonical := range names {
			if strings.EqualFold(name, canonical) && name != canonical {
				return &cursorformat.NotCheckedError{Reason: cursorformat.UnknownFormat}
			}
		}
	}
	if value, present := fields["composerId"]; present {
		var id string
		if json.Unmarshal(value, &id) != nil || id == "" || id != strings.TrimPrefix(key, "composerData:") {
			return &cursorformat.NotCheckedError{Reason: cursorformat.UnknownFormat}
		}
	}
	if value, present := fields["workspaceIdentifier"]; present {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return &cursorformat.NotCheckedError{Reason: cursorformat.UnknownFormat}
		}
		var workspace map[string]json.RawMessage
		if json.Unmarshal(value, &workspace) != nil || workspace == nil {
			return &cursorformat.NotCheckedError{Reason: cursorformat.UnknownFormat}
		}
		for name := range workspace {
			if strings.EqualFold(name, "id") && name != "id" || strings.EqualFold(name, "uri") && name != "uri" {
				return &cursorformat.NotCheckedError{Reason: cursorformat.UnknownFormat}
			}
		}
		if raw, present := workspace["id"]; present {
			var id string
			if json.Unmarshal(raw, &id) != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				return &cursorformat.NotCheckedError{Reason: cursorformat.UnknownFormat}
			}
		}
		if raw, present := workspace["uri"]; present {
			if err := validateSettledURI(raw); err != nil {
				return err
			}
		}
	}
	if raw, present := fields["subagentComposerIds"]; present {
		if err := ValidateSettledCursorRoster(ctx, raw); err != nil {
			return err
		}
	}
	return nil
}

func validateSettledURI(raw []byte) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return &cursorformat.NotCheckedError{Reason: cursorformat.UnknownFormat}
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return &cursorformat.NotCheckedError{Reason: cursorformat.UnknownFormat}
	}
	names := []string{"scheme", "authority", "fsPath", "path", "external"}
	for name, value := range fields {
		for _, canonical := range names {
			if strings.EqualFold(name, canonical) {
				if name != canonical || bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, &text) != nil {
					return &cursorformat.NotCheckedError{Reason: cursorformat.UnknownFormat}
				}
			}
		}
	}
	return nil
}

// ValidateSettledCursorRoster bounds every prospective ID before the lenient catalog
// decoder allocates its roster; that decoder still owns canonical sorting/hash.
func ValidateSettledCursorRoster(ctx context.Context, raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('[') {
		return &cursorformat.NotCheckedError{Reason: cursorformat.UnknownFormat}
	}
	seen := map[string]bool{}
	count := 0
	for decoder.More() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if count == 64 {
			return &cursorformat.NotCheckedError{Reason: cursorformat.UnknownFormat}
		}
		var encoded json.RawMessage
		if decoder.Decode(&encoded) != nil || len(encoded) > 6*255+2 || bytes.Equal(bytes.TrimSpace(encoded), []byte("null")) {
			return &cursorformat.NotCheckedError{Reason: cursorformat.UnknownFormat}
		}
		var id string
		if json.Unmarshal(encoded, &id) != nil || id == "" || len(id) > 255 || seen[id] {
			return &cursorformat.NotCheckedError{Reason: cursorformat.UnknownFormat}
		}
		seen[id] = true
		count++
	}
	token, err = decoder.Token()
	if err != nil || token != json.Delim(']') {
		return &cursorformat.NotCheckedError{Reason: cursorformat.UnknownFormat}
	}
	var extra json.RawMessage
	if decoder.Decode(&extra) != io.EOF {
		return &cursorformat.NotCheckedError{Reason: cursorformat.UnknownFormat}
	}
	return nil
}
