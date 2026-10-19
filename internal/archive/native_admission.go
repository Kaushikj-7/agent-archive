package archive

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
)

// NativeAdmission preserves the original closed local capture window.
// Fingerprints establish equality; they do not authenticate native execution.
type NativeAdmission struct {
	Contract          string `json:"contract"`
	RootFingerprint   string `json:"root_fingerprint"`
	WindowGeneration  string `json:"window_generation"`
	PolicyFingerprint string `json:"policy_fingerprint"`
	HookContract      string `json:"hook_contract"`
}

// Validate refuses incomplete and future admission capabilities.
func (a *NativeAdmission) Validate() error {
	if a == nil || a.Contract != "muse-root-admission" || a.HookContract != "muse-root-hooks-v1" || !admissionHex(a.RootFingerprint, 64) || !admissionHex(a.PolicyFingerprint, 64) || !admissionHex(a.WindowGeneration, 32) {
		return errors.New("unsupported native admission capability")
	}
	return nil
}

func admissionHex(s string, n int) bool {
	if len(s) != n || strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// UnmarshalJSON preserves unknown authority by refusing it before mutation.
func (a *NativeAdmission) UnmarshalJSON(raw []byte) error {
	if len(raw) > 1024 {
		return errors.New("native admission exceeds bound")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return errors.New("invalid native admission")
	}
	seen := map[string]bool{}
	for d.More() {
		token, err = d.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok || seen[key] || !slices.Contains([]string{"contract", "root_fingerprint", "window_generation", "policy_fingerprint", "hook_contract"}, key) {
			return errors.New("unknown native admission member")
		}
		seen[key] = true
		var value string
		if err = d.Decode(&value); err != nil {
			return err
		}
	}
	if len(seen) != 5 {
		return errors.New("incomplete native admission")
	}
	type plain NativeAdmission
	var next plain
	if err := json.Unmarshal(raw, &next); err != nil {
		return err
	}
	value := NativeAdmission(next)
	if err := value.Validate(); err != nil {
		return err
	}
	*a = value
	return nil
}

// NativeRootFingerprint hashes the exact reviewed root with unambiguous fields.
func NativeRootFingerprint(root NativeRoot) (string, error) {
	if err := root.Validate(); err != nil {
		return "", err
	}
	h := sha256.New()
	for _, field := range []string{"agent-archive/native-root/v1", root.Agent, root.Configured, root.Canonical, root.Revision} {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(field)))
		_, _ = h.Write(size[:])
		_, _ = h.Write([]byte(field))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
