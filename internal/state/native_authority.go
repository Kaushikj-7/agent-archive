package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

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
		if err := s.checkNativeRegistrationWrite(w.path, reg); err != nil {
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
			return s.checkNativeRegistrationWrite(w.path, *next)
		}
		return nil
	}
	return w
}

func (s *Store) checkNativeRegistrationWrite(path string, next archive.SessionRegistration) error {
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
	if previous.NativeBinding != nil || before.found && next.NativeBinding != nil {
		if !next.NativeBinding.PreservesFacts(previous.NativeBinding) || !nativeRegistrationOwnerMatches(next, previous) {
			return errors.New("registration write cannot change native binding authority")
		}
	}
	// Ordinary existing integrations keep their established compatibility paths.
	if next.NativeBinding == nil && (next.Harness.Name == "codex" || next.Harness.Name == "claude" || next.Harness.Name == "cursor") {
		return nil
	}
	cfg, found, err := config.Load(s.home)
	if err != nil {
		return err
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
