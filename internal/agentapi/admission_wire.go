package agentapi

import (
	"bytes"
	"encoding/json"
	"errors"
)

// UnmarshalJSON closes the native private protocol before any queue owner can
// interpret it. Future records return an error and remain untouched on disk.
// Existing supported producers retain their exact v0/v1 wire; unknown members
// cannot silently become authority merely because their integer is recognized.
func (i *AdmissionIntent) UnmarshalJSON(raw []byte) error {
	type plain AdmissionIntent
	if len(raw) > 16<<20 {
		return errors.New("admission intent exceeds existing read bound")
	}
	var peek struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(raw, &peek); err != nil {
		return err
	}
	if peek.Version == 2 && len(raw) > MaxNativeAdmissionIntentBytes {
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
		known := false
		for _, name := range []string{"version", "native_mode", "native_admission", "native_owner_id", "native_window_generation", "observed_cwd", "codex_policy_token", "harness", "event", "native_session_id", "project_root", "destination_id", "pause_generation", "transcript_path", "cursor_version", "composer_mode", "observed_at", "effects", "replay", "last_head"} {
			known = known || key == name
		}
		if !ok || !known || seen[key] {
			return errors.New("ambiguous admission intent")
		}
		seen[key] = true
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return err
		}
		if peek.Version == 2 && key == "effects" {
			if err := nativeIntentEffectsWire(value); err != nil {
				return err
			}
		}
	}
	var next AdmissionIntent
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode((*plain)(&next)); err != nil {
		return err
	}
	if next.Version == 2 && !ValidNativeAdmissionIntentShape(AdmissionIntent(next)) {
		return errors.New("unsupported native admission intent shape")
	}
	*i = AdmissionIntent(next)
	return nil
}

// nativeIntentEffectsWire checks nested selectors before encoding/json can merge
// duplicate objects or accept case aliases. Legacy effects keep their wire rules.
func nativeIntentEffectsWire(raw []byte) error {
	nested := map[string][]string{
		"Session": {"Agent", "NativeID", "Version", "Mode"},
		"Source":  {"Kind", "Path", "Key"},
		"Start":   {"Kind", "Reason", "Path"},
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil {
		return err
	}
	if token == nil {
		return nil
	}
	if token != json.Delim('[') {
		return errors.New("invalid native intent effects")
	}
	count := 0
	for d.More() {
		count++
		if count > 1 {
			return errors.New("unsupported native intent effects")
		}
		var effect json.RawMessage
		if err := d.Decode(&effect); err != nil {
			return err
		}

		if err := nativeIntentObjectWire(effect, []string{"Event"}, func(name string, value json.RawMessage) error {
			return nativeIntentObjectWire(value, []string{"NativeHookContract", "CodexProjectRoot", "CodexCwd", "CodexPolicyToken", "Kind", "Session", "ProjectRoot", "Source", "Locator", "Start", "NewOnly", "Deferred", "Reason", "NativeEvent", "Evidence", "Child"}, func(name string, value json.RawMessage) error {
				if names, known := nested[name]; known {
					return nativeIntentObjectWire(value, names, nil)
				}
				return nil
			})
		}); err != nil {
			return err
		}
	}
	_, err = d.Token()
	return err
}

func nativeIntentObjectWire(raw []byte, names []string, nested func(string, json.RawMessage) error) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return errors.New("invalid native intent object")
	}
	seen := make(map[string]bool, len(names))
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return err
		}
		name, ok := token.(string)
		known := false
		for _, allowed := range names {
			known = known || name == allowed
		}
		if !ok || !known || seen[name] {
			return errors.New("ambiguous native intent object")
		}
		seen[name] = true
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return err
		}
		if nested != nil {
			if err := nested(name, value); err != nil {
				return err
			}
		}
	}
	_, err = d.Token()
	return err
}
