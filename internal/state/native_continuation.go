package state

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

type nativeContinuation struct {
	intent agentapi.AdmissionIntent
	owner  archive.SessionRegistration
	ctx    context.Context
}

// ForNativeContinuation freezes an exact already validated owner's observation.
// Every resulting write stages with hooks.lock free, then reacquires hooks before
// the existing request lock for the final current-policy and owner comparison.
func (s *Store) ForNativeContinuation(ctx context.Context, intent agentapi.AdmissionIntent) (*Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	proof, err := s.nativeContinuationProof(intent)
	if err != nil {
		return nil, err
	}
	proof.ctx = ctx
	if err := s.checkNativeContinuation(proof); err != nil {
		return nil, err
	}
	copied := *s
	copied.hook = false
	copied.nativeContinuation = proof
	return &copied, nil
}

func (s *Store) checkNativeContinuation(p *nativeContinuation) error {
	if p.ctx != nil {
		if err := p.ctx.Err(); err != nil {
			return err
		}
	}
	i, owner := p.intent, p.owner
	current, found, err := s.LoadRegistration(i.NativeOwnerID)
	if err != nil {
		return err
	}
	if !nativeContinuationOwnerMatches(current, owner, i, found) {
		return errors.New("native continuation owner changed")
	}
	if _, err := os.Lstat(filepath.Join(s.home, "setup-transaction.json")); !errors.Is(err, os.ErrNotExist) {
		return errors.New("setup pending before native continuation")
	}
	cfg, found, err := config.Load(s.home)
	if err != nil {
		return err
	}
	if !nativeContinuationConfigMatches(cfg, found, owner, i) {
		return errors.New("native continuation capture policy changed")
	}
	if i.ObservedAt.Before(cfg.MuseCapture.Since) || time.Since(i.ObservedAt) > 24*time.Hour || i.ObservedAt.After(time.Now().Add(time.Minute)) || !local.PathWithin(i.ObservedCwd, owner.ProjectRoot) {
		return errors.New("native continuation observation is outside captured window or project")
	}
	matched := false
	for _, root := range cfg.NativeRoots {
		if root.Agent == "muse" {
			fingerprint, err := archive.NativeRootFingerprint(root)
			matched = err == nil && fingerprint == i.NativeAdmission.RootFingerprint && root.Revision == owner.NativeBinding.RootRevision
		}
	}
	selected := archive.ProjectActivation{}
	selectedFound := false
	for _, project := range cfg.Archive.Projects {
		if local.PathWithin(owner.NativeBinding.Cwd, project.Root) && (!selectedFound || len(project.Root) > len(selected.Root) || len(project.Root) == len(selected.Root) && !project.Included) {
			selected = project
			selectedFound = true
		}
	}
	if !selectedFound || !selected.Included || selected.Root != owner.ProjectRoot || selected.ProjectID != owner.ProjectID {
		return errors.New("native continuation physical project policy changed")
	}
	if !matched {
		return errors.New("native continuation root changed")
	}
	if _, removed, err := s.Removal("muse", i.NativeSessionID); err != nil || removed {
		return errors.New("native continuation was removed")
	}
	return nil
}

func (s *Store) guardNativeContinuationWrite(w lockedWrite) lockedWrite {
	if s.nativeContinuation == nil {
		return w
	}
	ownerID := s.nativeContinuation.owner.ArchiveSessionID
	if w.path != s.registrationPath(ownerID) && w.path != s.requestPath(ownerID) {
		w.change = func(fileSnapshot) (any, bool, error) {
			return nil, false, errors.New("native continuation cannot write another owner or state path")
		}
		return w
	}
	lock, check := w.lock, w.check
	w.lock = func() (func(), error) {
		hooks, err := s.namedLockWait("hooks.lock", time.Second)
		if err != nil {
			return nil, err
		}
		unlock, err := lock()
		if err != nil {
			hooks()
			return nil, err
		}
		return func() { unlock(); hooks() }, nil
	}
	w.check = func() error {
		if err := s.checkNativeContinuation(s.nativeContinuation); err != nil {
			return err
		}
		if check != nil {
			return check()
		}
		return nil
	}
	return w
}

func nativeContinuationOwnerMatches(current, owner archive.SessionRegistration, i agentapi.AdmissionIntent, found bool) bool {
	return found && nativeValidationOwnerMatches(current, owner) && owner.NativeAdmission != nil && *owner.NativeAdmission == *i.NativeAdmission && owner.NativeBinding != nil && owner.NativeBinding.Version == 2 && owner.NativeBinding.Validate() == nil && owner.NativeSessionID == i.NativeSessionID && owner.ProjectRoot == i.ProjectRoot && owner.DestinationID == i.DestinationID && !owner.CaptureFrozen && !owner.Imported() && !owner.NativeChild
}

func nativeContinuationConfigMatches(cfg config.Config, found bool, owner archive.SessionRegistration, i agentapi.AdmissionIntent) bool {
	return found && cfg.NativeAdmissionProtection && cfg.NativeAuthorityProtection && !cfg.Paused && cfg.Archive.Enabled && slices.Contains(cfg.Harnesses, "muse") && cfg.PauseGeneration == i.PauseGeneration && cfg.MuseCapture != nil && cfg.MuseCapture.Generation == i.NativeWindowGeneration && cfg.DestinationID() == i.DestinationID && cfg.AcceptSession(owner)
}

// CheckNativeContinuation checks current observation eligibility without returning
// a writable scope. Actual continuation writes require ForNativeContinuation.
func (s *Store) CheckNativeContinuation(intent agentapi.AdmissionIntent) error {
	proof, err := s.nativeContinuationProof(intent)
	if err != nil {
		return err
	}
	return s.checkNativeContinuation(proof)
}

func (s *Store) nativeContinuationProof(intent agentapi.AdmissionIntent) (*nativeContinuation, error) {
	if !agentapi.ValidNativeAdmissionIntentShape(intent) || intent.NativeMode != agentapi.NativeOwnerContinuation {
		return nil, errors.New("native continuation requires closed exact owner observation")
	}

	raw, err := json.Marshal(intent)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &intent); err != nil {
		return nil, err
	}
	owner, found, err := s.LoadRegistration(intent.NativeOwnerID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrSessionNotRegistered
	}
	proof := &nativeContinuation{intent: intent, owner: owner, ctx: nil}
	return proof, nil
}
