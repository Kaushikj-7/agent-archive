package archive

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"unicode/utf8"
)

// MaxNativeImportBytes bounds one private historical authority receipt.
const MaxNativeImportBytes = 16 * 1024

// ErrUnsupportedNativeImport preserves unsupported private import authority.
var ErrUnsupportedNativeImport = errors.New("unsupported native historical import authority")

// NativeImport binds one explicitly confirmed historical selection to its
// creating batch. It grants no future native reads or live capture permission.
type NativeImport struct {
	Contract     string                   `json:"contract"`
	Confirmation NativeImportConfirmation `json:"confirmation"`
	Membership   NativeImportMembership   `json:"membership"`
}

// NativeImportConfirmation identifies the durable batch instance and exact
// selected source. The enclosing registration binds owner and original birth.
type NativeImportConfirmation struct {
	BatchID               string           `json:"batch_id"`
	BatchInstance         string           `json:"batch_instance"`
	ScopeSHA256           string           `json:"scope_sha256"`
	DestinationID         string           `json:"destination_id"`
	ProjectID             string           `json:"project_id"`
	RootFingerprint       string           `json:"root_fingerprint"`
	SelectedBindingSHA256 string           `json:"selected_binding_sha256"`
	SelectedSource        NativeCheckpoint `json:"selected_source"`
}

// NativeImportMembershipState is the closed creating-batch attachment state.
type NativeImportMembershipState string

const (
	// NativeImportAttached retains the creating-batch registration effect.
	NativeImportAttached NativeImportMembershipState = "attached"
	// NativeImportDetached retains proof after the creating-batch effect is removed.
	NativeImportDetached NativeImportMembershipState = "detached"
)

// NativeImportMembership retains the exact creating-batch effects.
type NativeImportMembership struct {
	ReceiptID string                      `json:"receipt_id"`
	State     NativeImportMembershipState `json:"state"`
	Effects   []string                    `json:"effects"`
}

// Validate checks a present value for the bounded structural wire walkers.
func (n NativeImport) Validate() error { return validateNativeImport(&n) }

func validateNativeImport(n *NativeImport) error {
	if n == nil || n.Contract != "muse-historical-import-v1" || n.Confirmation.validate() != nil || n.Membership.validate() != nil {
		return ErrUnsupportedNativeImport
	}
	return nil
}

func (c NativeImportConfirmation) validate() error {
	if !nativeImportText(c.BatchID, 64) || !admissionHex(c.BatchInstance, 32) || !admissionHex(c.ScopeSHA256, 64) || !nativeImportText(c.DestinationID, 4096) || !nativeImportText(c.ProjectID, 4096) || !admissionHex(c.RootFingerprint, 64) || !admissionHex(c.SelectedBindingSHA256, 64) || c.SelectedSource.Validate() != nil {
		return ErrUnsupportedNativeImport
	}
	return nil
}

func (m NativeImportMembership) validate() error {
	if !admissionHex(m.ReceiptID, 32) || m.State != NativeImportAttached && m.State != NativeImportDetached || !slices.Equal(m.Effects, []string{"registration"}) {
		return ErrUnsupportedNativeImport
	}
	return nil
}

func nativeImportText(value string, limit int) bool {
	return len(value) <= limit && utf8.ValidString(value) && strings.TrimSpace(value) != "" && !strings.ContainsAny(value, "\x00\r\n")
}

// NativeImportIdentityDigest binds immutable receipt facts across attached and detached
// states. It is equality evidence, never a constructor for import permission.
func NativeImportIdentityDigest(n *NativeImport) (string, error) {
	if err := validateNativeImport(n); err != nil {
		return "", err
	}
	normalized := *n
	normalized.Membership.State = NativeImportAttached
	raw, err := json.Marshal(normalized)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte("agent-archive/muse-historical-import-receipt/v1\x00"), raw...))
	return hex.EncodeToString(sum[:]), nil
}

// MarshalJSON refuses invalid authority before a writer can emit it.
func (n NativeImport) MarshalJSON() ([]byte, error) {
	if err := n.Validate(); err != nil {
		return nil, err
	}
	type plain NativeImport
	raw, err := json.Marshal(plain(n))
	if err != nil || len(raw) > MaxNativeImportBytes {
		return nil, errors.Join(ErrUnsupportedNativeImport, err)
	}
	return raw, nil
}

// UnmarshalJSON validates every nested selector before retaining authority.
func (n *NativeImport) UnmarshalJSON(raw []byte) error {
	if err := nativeImportObject(raw, "contract", "confirmation", "membership"); err != nil {
		return err
	}
	type plain NativeImport
	var next plain
	if err := json.Unmarshal(raw, &next); err != nil {
		return errors.Join(ErrUnsupportedNativeImport, err)
	}
	value := NativeImport(next)
	if err := validateNativeImport(&value); err != nil {
		return err
	}
	*n = value
	return nil
}

// UnmarshalJSON refuses duplicate, aliased, null or future confirmation fields.
func (c *NativeImportConfirmation) UnmarshalJSON(raw []byte) error {
	if err := nativeImportObject(raw, "batch_id", "batch_instance", "scope_sha256", "destination_id", "project_id", "root_fingerprint", "selected_binding_sha256", "selected_source"); err != nil {
		return err
	}
	type plain NativeImportConfirmation
	var next plain
	if err := json.Unmarshal(raw, &next); err != nil {
		return errors.Join(ErrUnsupportedNativeImport, err)
	}
	value := NativeImportConfirmation(next)
	if err := value.validate(); err != nil {
		return err
	}
	*c = value
	return nil
}

// UnmarshalJSON recognizes only exact attached/detached registration ownership.
func (m *NativeImportMembership) UnmarshalJSON(raw []byte) error {
	if err := nativeImportObject(raw, "receipt_id", "state", "effects"); err != nil {
		return err
	}
	type plain NativeImportMembership
	var next plain
	if err := json.Unmarshal(raw, &next); err != nil {
		return errors.Join(ErrUnsupportedNativeImport, err)
	}
	value := NativeImportMembership(next)
	if err := value.validate(); err != nil {
		return err
	}
	*m = value
	return nil
}

func nativeImportObject(raw []byte, names ...string) error {
	return nativeImportClosedObject(raw, MaxNativeImportBytes, names, nil)
}

func nativeImportClosedObject(raw []byte, limit int, required, optional []string) error {
	return nativeImportClosedObjectNulls(raw, limit, required, optional, nil)
}

func nativeImportClosedObjectNulls(raw []byte, limit int, required, optional, nullable []string) error {
	if len(raw) > limit {
		return ErrUnsupportedNativeImport
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return ErrUnsupportedNativeImport
	}
	seen := make(map[string]bool, len(required)+len(optional))
	for d.More() {
		token, err := d.Token()
		key, ok := token.(string)
		if err != nil || !ok || seen[key] || !slices.Contains(required, key) && !slices.Contains(optional, key) {
			return ErrUnsupportedNativeImport
		}
		seen[key] = true
		var value json.RawMessage
		if err := d.Decode(&value); err != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) && !slices.Contains(nullable, key) {
			return ErrUnsupportedNativeImport
		}
	}
	for _, name := range required {
		if !seen[name] {
			return ErrUnsupportedNativeImport
		}
	}
	if _, err := d.Token(); err != nil {
		return ErrUnsupportedNativeImport
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return ErrUnsupportedNativeImport
	}
	return nil
}
