package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

// guardNativeRegistrationWrite applies to every registration write, including
// blind replacements and generation recovery. It runs before staging and again
// in the existing request-lock commit check. It introduces no authority store.
func (s *Store) guardNativeRegistrationWrite(w lockedWrite) lockedWrite {
	if filepath.Dir(w.path) != filepath.Join(s.home, "registrations") {
		return w
	}
	change, check := w.change, w.check
	var next *archive.SessionRegistration
	w.change = func(current fileSnapshot) (any, bool, error) {
		next = nil
		value, write, err := change(current)
		if err != nil || !write {
			return value, write, err
		}
		reg, ok := value.(archive.SessionRegistration)
		if !ok {
			return nil, false, errors.New("registration write requires typed authority")
		}
		next = &reg
		if err := s.checkNativeRegistrationTransition(w.path, reg, w.nativeValidation, w.nativeAdmission); err != nil {
			return nil, false, err
		}
		return value, true, nil
	}
	w.check = func() error {
		if check != nil {
			if err := check(); err != nil {
				return err
			}
		}
		if next != nil {
			return s.checkNativeRegistrationTransition(w.path, *next, w.nativeValidation, w.nativeAdmission)
		}
		return nil
	}
	return w
}

func (s *Store) checkNativeRegistrationWrite(path string, next archive.SessionRegistration) error {
	return s.checkNativeRegistrationTransition(path, next, nil, nil)
}

func (s *Store) checkNativeRegistrationTransition(path string, next archive.SessionRegistration, prepared *nativeValidationCommit, admission *nativeAdmissionCommit) error {
	before, err := readSnapshot(path)
	if err != nil {
		return err
	}
	var previous archive.SessionRegistration
	if before.found {
		if err := json.Unmarshal(before.data, &previous); err != nil {
			return err
		}
	}
	if !before.found || deletionOwner(previous) != deletionOwner(next) {
		if err := s.checkDeletionRegistrationTransition(next); err != nil {
			return err
		}
	}
	if err := preserveNativeRegistrationAuthority(previous, next, before.found, prepared, admission); err != nil {
		return err
	}
	// Ordinary existing integrations keep their established compatibility paths.
	if next.NativeBinding == nil && (agentmeta.ID(next.Harness.Name) == agentmeta.Codex || agentmeta.ID(next.Harness.Name) == agentmeta.Claude || agentmeta.ID(next.Harness.Name) == agentmeta.Cursor) {
		return nil
	}
	cfg, found, err := config.Load(s.home)
	if err != nil {
		return err
	}
	if cfg.NativeAdmissionProtection && next.Harness.Name == "muse" && (next.NativeAdmission == nil || !before.found && admission == nil) {
		return errors.New("muse ownership requires prepared native admission")
	}
	for _, root := range cfg.NativeRoots {
		if before.found && previous.NativeBinding == nil && previous.TranscriptPath != "" && previous.Harness.Name == root.Agent {
			return errors.New("native locator lost original authority; recover original binding before mutation")
		}
	}
	if next.NativeBinding != nil {
		if !sameNativeImportBinding(previous.NativeBinding, next.NativeBinding) {
			if _, err := os.Lstat(filepath.Join(s.home, "setup-transaction.json")); !errors.Is(err, os.ErrNotExist) {
				return errors.New("setup pending before native source binding change")
			}
		}
		return validateNativeRegistrationFence(cfg, found, previous, next)
	}

	for _, root := range cfg.NativeRoots {
		if root.Agent == next.Harness.Name && next.TranscriptPath != "" {
			return errors.New("native locator requires its original source binding")
		}
	}
	return nil
}

func preserveNativeRegistrationAuthority(previous, next archive.SessionRegistration, found bool, prepared *nativeValidationCommit, admission *nativeAdmissionCommit) error {
	if previous.NativeAdmission != nil || next.NativeAdmission != nil {
		a, _ := json.Marshal(previous.NativeAdmission)
		b, _ := json.Marshal(next.NativeAdmission)
		if admission == nil && string(a) != string(b) {
			return errors.New("native admission proof is immutable")
		}
	}
	if admission != nil {
		return admission.check(next, found)
	}
	if prepared != nil {
		if err := prepared.check(previous, next, found); err != nil {
			return err
		}
	} else if !next.NativeBinding.SameCheckpoint(previous.NativeBinding) {
		return errors.New("native checkpoint requires prepared validation commit")
	}
	if next.NativeBinding != nil && next.NativeBinding.Agent == "muse" && next.NativeBinding.Version == 1 && previous.NativeBinding == nil {
		return errors.New("new Muse binding requires successful native checkpoint")
	}
	if previous.NativeBinding != nil {
		if err := previous.NativeBinding.Validate(); err != nil {
			return err
		}
	}
	if previous.NativeBinding != nil || found && next.NativeBinding != nil {
		if !next.NativeBinding.PreservesFacts(previous.NativeBinding) || !nativeRegistrationOwnerMatches(next, previous) {
			return errors.New("registration write cannot change native binding authority")
		}
	}
	return nil
}

func nativeRegistrationOwnerMatches(a, b archive.SessionRegistration) bool {
	return a.ArchiveSessionID == b.ArchiveSessionID && a.NativeSessionID == b.NativeSessionID && a.Harness.Name == b.Harness.Name && a.ProjectID == b.ProjectID && a.ProjectRoot == b.ProjectRoot && a.DestinationID == b.DestinationID && a.Origin == b.Origin && a.AdmittedAt.Equal(b.AdmittedAt) && a.SessionStartedAt.Equal(b.SessionStartedAt) && a.ParentSessionID == b.ParentSessionID && a.ParentNativeSessionID == b.ParentNativeSessionID && a.NativeRootSessionID == b.NativeRootSessionID && a.NativeChild == b.NativeChild && a.SourceKind == b.SourceKind && a.SourceKey == b.SourceKey && a.NativeSourceHome == b.NativeSourceHome
}

func validateNativeRegistrationFence(cfg config.Config, found bool, previous, next archive.SessionRegistration) error {
	if !found || !cfg.NativeAuthorityProtection {
		return errors.New("native binding requires committed writer protection")
	}
	if previous.NativeBinding == nil {
		matched := false
		for _, root := range cfg.NativeRoots {
			matched = matched || root.Agent == next.NativeBinding.Agent && root.Revision == next.NativeBinding.RootRevision && local.PathWithin(next.NativeBinding.Path, root.Canonical)
		}
		if !matched {
			return errors.New("new native binding requires matching committed root revision")
		}
	}
	return next.Validate()
}

// genericNativeReservationAllowed keeps strong Muse admission on its prepared
// first-owner path. Existing-owner continuation never needs a new reservation.
func (s *Store) genericNativeReservationAllowed(key agentmeta.SessionKey) error {
	if key.Agent != agentmeta.Muse {
		return nil
	}
	cfg, _, err := config.Load(s.home)
	if err != nil {
		return err
	}
	if cfg.NativeAdmissionProtection {
		return errors.New("muse reservation requires prepared native admission")
	}
	return nil
}
