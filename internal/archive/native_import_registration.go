package archive

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

// NativeImportBindingDigest identifies the complete original proven binding.
// It retains no locator text and grants no source permission by itself.
func NativeImportBindingDigest(binding *NativeSourceBinding) (string, error) {
	if binding == nil || binding.Version != 2 || binding.Agent != "muse" || binding.Validate() != nil {
		return "", ErrUnsupportedNativeImport
	}
	raw, err := json.Marshal(binding)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte("agent-archive/muse-historical-import-binding/v1\x00"), raw...))
	return hex.EncodeToString(sum[:]), nil
}

// ValidateNativeImport binds a closed historical receipt to its original owner.
// Ephemeral stage files, current batch membership and current native files are
// deliberately irrelevant to the authority of an already retained publication.
func (r SessionRegistration) ValidateNativeImport() error {
	n, b := r.NativeImport, r.NativeBinding
	if n == nil || n.Validate() != nil || !admissionHex(r.ArchiveSessionID, 32) || !nativePath(r.ProjectRoot) || r.validateNativeBinding() != nil || b == nil || b.Version != 2 || b.Validate() != nil || !r.Imported() || r.Harness.Name != "muse" || r.IsChild() || r.ParentNativeSessionID != "" || b.ParentID != "" || b.RootID != "" && b.RootID != b.NativeID || r.HistoricalProject != nil || r.CodexBinding != nil || r.CodexAdmission != nil || r.PreviousGenerationID != "" || !r.ReadsTranscriptFile() {
		return ErrUnsupportedNativeImport
	}
	c := n.Confirmation
	digest, err := NativeImportBindingDigest(b)
	if err != nil || digest != c.SelectedBindingSHA256 || c.SelectedSource != *b.Checkpoint || c.BatchID != r.ImportBatch.Recorded() || c.ProjectID != r.ProjectID || c.DestinationID != r.DestinationID || b.Agent != r.Harness.Name || b.NativeID != r.NativeSessionID || b.Path != r.TranscriptPath || !b.NativeCreatedAt.Equal(r.SessionStartedAt) {
		return ErrUnsupportedNativeImport
	}
	return nil
}

// DecodeSessionRegistration preserves private authority classification even
// when malformed JSON would prevent encoding/json from invoking UnmarshalJSON.
// State readers use this before deciding whether damaged bytes are repairable.
func DecodeSessionRegistration(raw []byte, r *SessionRegistration) error {
	err := r.UnmarshalJSON(raw)
	if err != nil && nativeImportKeyPossible(raw) {
		return errors.Join(ErrUnsupportedNativeImport, err)
	}
	return err
}

// UnmarshalJSON protects the private receipt selector before ordinary field
// decoding can normalize an alias, discard null, or replace a duplicate proof.
// Registrations without this selector retain their existing decoder behavior.
func (r *SessionRegistration) UnmarshalJSON(raw []byte) error {
	present, err := nativeImportRegistrationSelector(raw)
	if err != nil {
		return err
	}
	type plain SessionRegistration
	if !present {
		return json.Unmarshal(raw, (*plain)(r))
	}
	var next plain
	if err := json.Unmarshal(raw, &next); err != nil {
		if present {
			return errors.Join(ErrUnsupportedNativeImport, err)
		}
		return err
	}
	value := SessionRegistration(next)
	if present {
		if err := value.ValidateNativeImport(); err != nil {
			return err
		}
	}
	*r = value
	return nil
}

func nativeImportRegistrationSelector(raw []byte) (present bool, decodeErr error) {
	defer func() {
		if present && decodeErr != nil {
			decodeErr = errors.Join(ErrUnsupportedNativeImport, decodeErr)
		}
	}()
	// Normal legacy registrations have neither this key nor escaped keys. Avoid
	// allocating a second set of their field values; escaped names still take
	// the structural path so a JSON escape cannot hide private authority.
	if bytes.IndexByte(raw, '\\') < 0 && !nativeImportKeyPossible(raw) {
		return false, nil
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return false, nil
	}
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return present, err
		}
		key, ok := token.(string)
		if !ok {
			return present, ErrUnsupportedNativeImport
		}
		isImport := strings.EqualFold(key, "native_import")
		if isImport {
			if present || key != "native_import" {
				return true, ErrUnsupportedNativeImport
			}
			present = true
		}
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return present, err
		}
		if isImport && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return true, ErrUnsupportedNativeImport
		}
	}
	return present, nil
}

func nativeImportKeyPossible(raw []byte) bool {
	key := []byte(`"native_import"`)
	for i := 0; i+len(key) <= len(raw); i++ {
		if raw[i] == '"' && bytes.EqualFold(raw[i:i+len(key)], key) {
			return true
		}
	}
	return nativeImportEscapedKeyPossible(raw)
}

// Malformed values before an escaped selector must not hide its presence from
// state preservation. Inspect only bounded JSON string tokens followed by a
// colon; ordinary valid string values retain their existing decoding behavior.
func nativeImportEscapedKeyPossible(raw []byte) bool {
	if bytes.IndexByte(raw, '\\') < 0 {
		return false
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] != '"' {
			continue
		}
		start := i
		i++
		for i < len(raw) && raw[i] != '"' {
			if raw[i] == '\\' {
				i++
			}
			i++
		}
		end := i + 1
		if end > len(raw) {
			return false
		}
		next := end
		for next < len(raw) && (raw[next] == ' ' || raw[next] == '\n' || raw[next] == '\r' || raw[next] == '\t') {
			next++
		}
		if end-start > 80 || next == len(raw) || raw[next] != ':' {
			continue
		}
		var key string
		if json.Unmarshal(raw[start:end], &key) == nil && strings.EqualFold(key, "native_import") {
			return true
		}
	}
	return false
}
