package state

import "github.com/wangjohn/agent-archive/internal/archive"

// CheckSessionUndo refuses legacy destructive undo for historical receipts.
// Purpose-specific receipt transitions must preserve acknowledged evidence and
// later independent work. Both the supplied snapshot and actual owner matter.
func (s *Store) CheckSessionUndo(reg archive.SessionRegistration) error {
	if !safeFileComponent(reg.ArchiveSessionID) {
		return ErrSessionIdentityConflict
	}
	if reg.NativeImport != nil {
		return archive.ErrUnsupportedNativeImport
	}
	unlock, err := s.lockRequest(reg.ArchiveSessionID)
	if err != nil {
		return err
	}
	defer unlock()
	return s.checkSessionUndoLocked(reg.ArchiveSessionID)
}

func (s *Store) checkSessionUndoLocked(id string) error {
	current, _, err := s.LoadRegistration(id)
	if err != nil {
		return err
	}
	if current.NativeImport != nil {
		return archive.ErrUnsupportedNativeImport
	}
	return nil
}
