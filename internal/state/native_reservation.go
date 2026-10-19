package state

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/wangjohn/agent-archive/internal/config"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
)

const nativeReservationPrefix = "muse-root-reservation-v1:"

var errUnsupportedNativeReservation = errors.New("unsupported native reservation authority")

func supportedNativeReservation(value string) bool {
	if !strings.HasPrefix(value, nativeReservationPrefix) {
		return false
	}
	digest := strings.TrimPrefix(value, nativeReservationPrefix)
	if len(digest) != sha256.Size*2 || strings.ToLower(digest) != digest {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

func (p *PreparedNativeAdmission) reservationAuthority() (string, error) {
	raw, err := json.Marshal(struct {
		Domain    string                       `json:"domain"`
		Candidate NativeAdmissionCandidate     `json:"candidate"`
		Binding   *archive.NativeSourceBinding `json:"binding"`
	}{Domain: "agent-archive/muse-root-reservation/v1", Candidate: p.candidate, Binding: p.reg.NativeBinding})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return nativeReservationPrefix + hex.EncodeToString(sum[:]), nil
}

// The optional marker is authority, not repairable derived index damage. Legacy
// entries retain their existing decoder behavior when the selector is absent.
func (e *qualifiedSessionIndexEntry) UnmarshalJSON(raw []byte) error {
	type plain qualifiedSessionIndexEntry
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return json.Unmarshal(raw, (*plain)(e))
	}
	present, err := nativeReservationSelectors(raw)
	if err != nil {
		return err
	}
	var next plain
	decoder = json.NewDecoder(bytes.NewReader(raw))
	if present {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(&next); err != nil {
		if present {
			return errors.Join(errUnsupportedNativeReservation, err)
		}
		return err
	}
	if present && !supportedNativeReservationEntry(qualifiedSessionIndexEntry(next)) {
		return errUnsupportedNativeReservation
	}
	*e = qualifiedSessionIndexEntry(next)
	return nil
}

// Generic index repair may consume a supported pending marker only after a
// complete matching durable owner proves that prepared first admission finished.
func (s *Store) nativeReservationOwner(entry qualifiedSessionIndexEntry) error {
	if !supportedNativeReservation(entry.NativeReservation) {
		return errUnsupportedNativeReservation
	}
	owner, found, err := s.LoadRegistration(entry.ArchiveSessionID)
	if err != nil {
		return err
	}
	if !found || owner.ArchiveSessionID != entry.ArchiveSessionID || owner.Harness.Name != "muse" || owner.NativeSessionID != entry.NativeID || owner.NativeAdmission == nil || owner.NativeAdmission.Validate() != nil || owner.NativeBinding == nil || owner.NativeBinding.Version != 2 || owner.Validate() != nil {
		return ErrSessionIndexRecoveryRequired
	}
	return nil
}

func (s *Store) guardNativeReservationWrite(w lockedWrite) lockedWrite {
	if filepath.Dir(w.path) != filepath.Join(s.home, "sessions-v1") || filepath.Ext(w.path) != ".json" {
		return w
	}
	change := w.change
	w.blind = false
	w.change = func(current fileSnapshot) (any, bool, error) {
		var prior qualifiedSessionIndexEntry
		if current.found {
			if err := json.Unmarshal(current.data, &prior); errors.Is(err, errUnsupportedNativeReservation) {
				return nil, false, err
			}
		}
		value, write, err := change(current)
		if err != nil || !write {
			return value, write, err
		}
		next, ok := value.(qualifiedSessionIndexEntry)
		if !ok {
			return nil, false, errUnsupportedNativeReservation
		}
		if next.NativeReservation != "" {
			if !supportedNativeReservationEntry(next) {
				return nil, false, errUnsupportedNativeReservation
			}
			if prior.NativeReservation != "" && next != prior {
				return nil, false, errUnsupportedNativeReservation
			}
			if next.NativeReservation != prior.NativeReservation {
				if w.nativeReservation == nil {
					return nil, false, errUnsupportedNativeReservation
				}
				expected, err := w.nativeReservation.reservationAuthority()
				if err != nil || next.NativeReservation != expected {
					return nil, false, errors.Join(errUnsupportedNativeReservation, err)
				}
			}
		}
		if prior.NativeReservation != "" && next.NativeReservation == "" {
			key := agentmeta.SessionKey{Agent: prior.Agent, NativeID: prior.NativeID}
			if next != indexEntry(key, prior.ArchiveSessionID) {
				return nil, false, errUnsupportedNativeReservation
			}
			if err := s.nativeReservationOwner(prior); err != nil {
				return nil, false, err
			}
		}
		return next, true, nil
	}
	return w
}

// Recovery requests must not replace a pending native reservation, including a
// protected old reservation which lacks the new exact-authority marker.
func (s *Store) guardNativeReservationRequest(key agentmeta.SessionKey) error {
	if key.Agent != agentmeta.Muse {
		return nil
	}
	var entry qualifiedSessionIndexEntry
	err := readRecoveryJSONLimit(qualifiedSessionIndexPath(s.home, key), &entry, 32<<10)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if entry.NativeReservation != "" {
		return errUnsupportedNativeReservation
	}
	if entry.Reservation != "" {
		cfg, found, err := config.Load(s.home)
		if err != nil {
			return err
		}
		if found && cfg.NativeAdmissionProtection {
			return errUnsupportedNativeReservation
		}
	}
	return nil
}

func nativeReservationSelectors(raw []byte) (present bool, readErr error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if _, err := decoder.Token(); err != nil {
		return false, err
	}
	exact := true
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return present, err
		}
		key, ok := token.(string)
		if !ok {
			return present, errUnsupportedNativeReservation
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return present, err
		}
		canonical := ""
		for _, name := range []string{"version", "agent", "native_id", "archive_session_id", "reservation", "native_reservation", "conflict", "recovery", "absent"} {
			if strings.EqualFold(key, name) {
				canonical = name
				break
			}
		}
		if slices.Contains([]string{"conflict", "recovery", "absent"}, canonical) {
			exact = exact && (bytes.Equal(value, []byte("true")) || bytes.Equal(value, []byte("false")))
		}
		exact = exact && canonical != "" && key == canonical && !seen[canonical]
		seen[canonical] = true
		if canonical == "native_reservation" {
			present = true
			var marker string
			if json.Unmarshal(value, &marker) != nil || !supportedNativeReservation(marker) {
				return true, errUnsupportedNativeReservation
			}
		}
	}
	if present && !exact {
		return true, errUnsupportedNativeReservation
	}
	return present, nil
}

func nativeReservationID(value string) bool {
	if len(value) != 32 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func supportedNativeReservationEntry(entry qualifiedSessionIndexEntry) bool {
	return entry.Version == 1 && entry.Agent == agentmeta.Muse && supportedNativeReservation(entry.NativeReservation) && nativeReservationID(entry.ArchiveSessionID) && nativeReservationID(entry.Reservation) && !entry.Absent && !entry.Conflict && !entry.Recovery && nativeReservationNativeID(entry.NativeID)
}

func nativeReservationNativeID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for i := range len(id) {
		ch := id[i]
		alnum := ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9'
		if !alnum && (i == 0 || ch != '_' && ch != '-') {
			return false
		}
	}
	return true
}

func nativeReservationRepairError(entry qualifiedSessionIndexEntry, readErr error) error {
	if errors.Is(readErr, errUnsupportedNativeReservation) || entry.NativeReservation != "" {
		return errors.Join(errUnsupportedNativeReservation, readErr)
	}
	return nil
}
