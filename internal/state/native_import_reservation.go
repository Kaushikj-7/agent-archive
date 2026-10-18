package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
)

const nativeImportReservationPrefix = "muse-import-reservation-v1:"

// The import marker is independent of the root live-admission scheme. Its
// identity omits allocation time, mutable progress and membership receipt ID.
func nativeImportReservationAuthority(reg archive.SessionRegistration) (string, error) {
	if err := reg.ValidateNativeImport(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(struct {
		Domain       string                           `json:"domain"`
		Confirmation archive.NativeImportConfirmation `json:"confirmation"`
		NativeID     string                           `json:"native_id"`
		ProjectRoot  string                           `json:"project_root"`
		Binding      *archive.NativeSourceBinding     `json:"binding"`
	}{"agent-archive/muse-historical-import-reservation/v1", reg.NativeImport.Confirmation, reg.NativeSessionID, reg.ProjectRoot, reg.NativeBinding})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return nativeImportReservationPrefix + hex.EncodeToString(digest[:]), nil
}

func (p *PreparedNativeImport) reservationAuthority() (string, error) {
	if p == nil || p.self != p || p.closed || p.store == nil {
		return "", errUnsupportedNativeReservation
	}
	return nativeImportReservationAuthority(p.reg)
}

// Generic repair can finish an import index only after the exact complete owner
// and original durable stage plus still-current confirmation prove completion.
// Missing/future/mismatched source or controls preserve the marker unchanged.
func (s *Store) nativeImportReservationOwner(entry qualifiedSessionIndexEntry) error {
	read, closeRead := s.WithImportResultScope(s.durableContext())
	defer closeRead()
	s = read
	if !supportedReservationPrefix(entry.NativeReservation, nativeImportReservationPrefix) {
		return errUnsupportedNativeReservation
	}
	reg, found, err := s.LoadRegistration(entry.ArchiveSessionID)
	if err != nil || !found || reg.NativeSessionID != entry.NativeID || entry.Agent != agentmeta.Muse || reg.ValidateNativeImport() != nil {
		return errors.Join(ErrSessionIndexRecoveryRequired, err)
	}
	expected, err := nativeImportReservationAuthority(reg)
	if err != nil || expected != entry.NativeReservation {
		return errors.Join(errUnsupportedNativeReservation, err)
	}
	stage, found, err := s.LoadImportStage(s.durableContext(), reg)
	if err != nil || !found || !stage.TypedSelection || stage.Version != 2 || stage.Validate(s.durableContext(), reg) != nil {
		return errors.Join(ErrSessionIndexRecoveryRequired, err)
	}
	return s.nativeImportCurrentBatch(s.durableContext(), reg)
}

// Pending historical identity is not generic deletion work. Refuse before any
// remote deletion or durable deletion phase; committed ordinary index rows keep
// the established retention behavior.
func (s *Store) checkNativeImportReservationDeletion(reg archive.SessionRegistration) error {
	if reg.Harness.Name != "muse" {
		return nil
	}
	entry, found, err := s.readQualifiedIndex(agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: reg.NativeSessionID})
	if err != nil {
		return err
	}
	if found && supportedReservationPrefix(entry.NativeReservation, nativeImportReservationPrefix) {
		return errUnsupportedNativeReservation
	}
	return nil
}
