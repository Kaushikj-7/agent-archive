package collector

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
)

// persistCodexBinding runs after native validation and keeps admission and
// provenance untouched. The caller already holds collector.lock; hooks.lock
// fences stale hooks and setup before installing irreversible writer protection.
func (s *sessionScan) persistCodexBinding(binding *archive.CodexSourceBinding) error {
	if binding == nil {
		return nil
	}
	changed, err := s.nativeBindingChanged(binding)
	if err != nil || !changed {
		return err
	}

	canonicalCwd := ""
	if s.reg.CodexAdmission != nil {
		selectedCwd := binding.SelectedCwd
		if selectedCwd == "" {
			selectedCwd = binding.Cwd
		}
		canonicalCwd, err = canonicalBindingCwd(selectedCwd)
		if err != nil {
			return err
		}
	}
	home := s.local.Home()
	unlock, err := local.NamedLockWait(home, "hooks.lock", time.Second)
	if err != nil {
		return err
	}
	defer unlock()
	if setupjournal.TransactionPending(home) {
		return errors.New("setup pending before history writer protection")
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		return err
	}
	if !found || cfg.Paused || !cfg.AcceptSession(s.reg) {
		return errors.New("codex binding requires current capture permission")
	}
	if err := s.validateMigrationHome(cfg, binding); err != nil {
		return err
	}
	if s.reg.CodexAdmission != nil {
		if !cfg.CodexContinuationAllowed(s.reg.ProjectRoot, canonicalCwd) {
			return errors.New("native cwd is outside current Codex continuation permission")
		}
	}
	if !cfg.CodexHistoryProtection {
		cfg.CodexHistoryProtection = true
		if err := config.Save(home, cfg); err != nil {
			return err
		}
	}
	var updated archive.SessionRegistration
	found, err = s.local.UpdateRegistration(s.id(), func(current *archive.SessionRegistration) error {
		if current.NativeSessionID != s.reg.NativeSessionID || !cfg.AcceptSession(*current) {
			return errors.New("codex registration changed before binding")
		}
		if !binding.PreservesFacts(current.CodexBinding) {
			return errors.New("codex binding facts changed before update")
		}
		if err := applyNativeChildBinding(current, binding); err != nil {
			return err
		}
		current.CodexBinding = binding
		current.TranscriptPath = binding.Path
		updated = *current
		return nil
	})
	if err != nil {
		return err
	}
	if !found {
		return errors.New("codex registration disappeared before binding")
	}
	s.reg = updated
	return nil
}

// canonicalBindingCwd preserves the spelling below an existing ancestor when a
// native worktree has vanished. It is used only during migration, outside locks.
func canonicalBindingCwd(path string) (string, error) {
	path = filepath.Clean(path)
	var missing []string
	for range 64 {
		canonical, err := filepath.EvalSymlinks(path)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				canonical = filepath.Join(canonical, missing[i])
			}
			return canonical, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", err
		}
		missing = append(missing, filepath.Base(path))
		path = parent
	}
	return "", errors.New("native cwd ancestor limit exceeded")
}

func nativeBindingRelationshipsMatch(reg archive.SessionRegistration, binding *archive.CodexSourceBinding) bool {
	if !binding.Child {
		return true
	}
	return reg.NativeChild && (binding.ParentID == "" || reg.ParentNativeSessionID == binding.ParentID) && (binding.RootID == "" || reg.NativeRootSessionID == binding.RootID) && (binding.Home == "" || reg.NativeSourceHome == binding.Home)
}

func applyNativeChildBinding(reg *archive.SessionRegistration, binding *archive.CodexSourceBinding) error {
	if !binding.Child {
		return nil
	}
	if reg.ParentNativeSessionID != "" && binding.ParentID != "" && reg.ParentNativeSessionID != binding.ParentID {
		return errors.New("native child parent facts conflict")
	}
	if reg.NativeRootSessionID != "" && binding.RootID != "" && reg.NativeRootSessionID != binding.RootID {
		return errors.New("native child root facts conflict")
	}
	if reg.NativeSourceHome != "" && binding.Home != "" && reg.NativeSourceHome != binding.Home {
		return errors.New("native child source home facts conflict")
	}
	reg.NativeChild = true
	if binding.ParentID != "" {
		reg.ParentNativeSessionID = binding.ParentID
	}
	if binding.RootID != "" {
		reg.NativeRootSessionID = binding.RootID
	}
	if binding.Home != "" {
		reg.NativeSourceHome = binding.Home
	}
	return nil
}

func (s *sessionScan) nativeBindingChanged(binding *archive.CodexSourceBinding) (bool, error) {
	if err := binding.Validate(); err != nil {
		return false, err
	}
	if s.reg.CodexBinding == nil && s.reg.CodexAdmission != nil {
		cwd, err := canonicalBindingCwd(binding.Cwd)
		if err != nil {
			return false, err
		}
		if cwd != s.reg.CodexAdmission.Cwd {
			facts, known := sourcefacts.PhysicalProject(binding.Cwd)
			if !known || facts.Root != s.reg.ProjectRoot {
				return false, errors.New("native cwd contradicts admitted physical project evidence")
			}
		}
	}
	if !binding.PreservesFacts(s.reg.CodexBinding) {
		return false, errors.New("codex native binding facts changed")
	}
	before, err := json.Marshal(s.reg.CodexBinding)
	if err != nil {
		return false, err
	}
	after, err := json.Marshal(binding)
	if err != nil {
		return false, err
	}
	if string(before) == string(after) && nativeBindingRelationshipsMatch(s.reg, binding) {
		return false, nil
	}
	return true, nil
}

// errRelocationRaced reports that the registration changed between the read
// and the relocation, so following the rollout is left to a later pass.
var errRelocationRaced = errors.New("codex registration changed before following its rollout")

// followRelocatedRollout moves an unbound legacy Codex registration's
// locator to where its rollout was just read from, after Codex renamed it
// into archived_sessions/ (or back). Nothing else about the registration
// changes, and only the same rollout file name inside the same trusted Codex
// home qualifies: the source already matched its thread and rollout IDs.
// Without this the registration would keep naming a locator that no longer
// exists, so every later pass would find the rollout again by its ID.
//
// The read has already succeeded, so following is best effort: whatever
// stops it (a busy hooks.lock, a pending setup, capture paused or no longer
// accepted, a registration changed or removed meanwhile) leaves the
// registration as it is for a later pass, as the same checks stop
// persistCodexBinding. Only a failure to read the configuration or write the
// registration is an error.
func (s *sessionScan) followRelocatedRollout(path string) error {
	if path == "" || path == s.reg.TranscriptPath || s.reg.CodexBinding != nil {
		return nil
	}
	old := s.reg.TranscriptPath
	confined := confinedSourceRegistration(s.reg, s.opts.ConfiguredCodexHomes)
	if confined == nil || confined.DiscoveryRoot == "" || filepath.Base(path) != filepath.Base(old) || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil
	}
	root := confined.DiscoveryRoot
	if !local.PathWithin(path, filepath.Join(root, "sessions")) && !local.PathWithin(path, filepath.Join(root, "archived_sessions")) {
		return nil
	}
	home := s.local.Home()
	unlock, err := local.NamedLockWait(home, "hooks.lock", time.Second)
	if errors.Is(err, local.ErrBusy) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unlock()
	if setupjournal.TransactionPending(home) {
		return nil
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		return err
	}
	if !found || cfg.Paused || !cfg.AcceptSession(s.reg) {
		return nil
	}
	var updated archive.SessionRegistration
	found, err = s.local.UpdateRegistration(s.id(), func(current *archive.SessionRegistration) error {
		if current.NativeSessionID != s.reg.NativeSessionID || current.CodexBinding != nil || current.TranscriptPath != old || !cfg.AcceptSession(*current) {
			return errRelocationRaced
		}
		current.TranscriptPath = path
		updated = *current
		return nil
	})
	if errors.Is(err, errRelocationRaced) || err == nil && !found {
		return nil
	}
	if err != nil {
		return err
	}
	s.reg = updated
	return nil
}

func (s *sessionScan) validateMigrationHome(cfg config.Config, binding *archive.CodexSourceBinding) error {
	if binding.Home == "" || s.reg.CodexBinding != nil && s.reg.CodexBinding.Home != "" || s.reg.Origin == archive.SessionOriginDiscovery {
		return nil
	}
	homes := []string{}
	if cfg.Discovery != nil {
		homes = append(homes, cfg.Discovery.CodexHomes...)
	}
	if path := cfg.HookFiles["codex"]; path != "" {
		homes = append(homes, filepath.Dir(path))
	}
	if s.opts.ResolveCodexReadHomes != nil {
		var err error
		homes, err = s.opts.ResolveCodexReadHomes(cfg)
		if err != nil {
			return err
		}
	}
	confined := confinedSourceRegistration(s.reg, homes)
	if confined == nil || confined.DiscoveryRoot != binding.Home {
		return errors.New("trusted confined source home changed before binding migration")
	}
	return nil
}

// Earlier writers persisted child facts before the public marker existed.
// Upgrade only the same acknowledged owner using its admitted binding;
// current native files cannot license this retained maintenance.
func (s *sessionScan) prepareRetainedOwnership() error {
	if err := s.recoverReferenceAuthority(); err != nil {
		return err
	}
	binding := s.reg.CodexBinding
	if binding == nil || !binding.Child || s.reg.NativeChild {
		return nil
	}
	bundle, _, found := s.published.LastPublished()
	if !found {
		return nil
	}
	owner := s.reg
	if err := applyNativeChildBinding(&owner, binding); err != nil {
		return err
	}
	if !nativeChildOwned(owner, bundle) {
		return errors.New("legacy child source does not match its admitted binding")
	}
	return s.persistCodexBinding(binding)
}
