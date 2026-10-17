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
	if intent.Version != 2 || intent.NativeMode != agentapi.NativeOwnerContinuation || intent.Harness != "muse" || intent.NativeOwnerID == "" || intent.NativeAdmission == nil || intent.NativeAdmission.Validate() != nil {
		return nil, errors.New("native continuation requires exact owner authority")
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
	proof := &nativeContinuation{intent: intent, owner: owner, ctx: ctx}
	if err := s.checkNativeContinuation(ctx, proof); err != nil {
		return nil, err
	}
	copied := *s
	copied.hook = false
	copied.nativeContinuation = proof
	return &copied, nil
}

func (s *Store) checkNativeContinuation(ctx context.Context, p *nativeContinuation) error {
	if err := p.ctx.Err(); err != nil {
		return err
	}
	i, owner := p.intent, p.owner
	current, found, err := s.LoadRegistration(i.NativeOwnerID)
	if err != nil {
		return err
	}
	if !found || !nativeValidationOwnerMatches(current, owner) || owner.NativeAdmission == nil || *owner.NativeAdmission != *i.NativeAdmission || owner.NativeBinding == nil || owner.NativeBinding.Version != 2 || owner.NativeBinding.Validate() != nil || owner.NativeSessionID != i.NativeSessionID || owner.ProjectRoot != i.ProjectRoot || owner.DestinationID != i.DestinationID || owner.CaptureFrozen || owner.Imported() || owner.NativeChild {
		return errors.New("native continuation owner changed")
	}
	if _, err := os.Lstat(filepath.Join(s.home, "setup-transaction.json")); !errors.Is(err, os.ErrNotExist) {
		return errors.New("setup pending before native continuation")
	}
	cfg, found, err := config.Load(s.home)
	if err != nil {
		return err
	}
	if !found || !cfg.NativeAdmissionProtection || !cfg.NativeAuthorityProtection || cfg.Paused || !cfg.Archive.Enabled || !slices.Contains(cfg.Harnesses, "muse") || cfg.PauseGeneration != i.PauseGeneration || cfg.DestinationID() != i.DestinationID || !cfg.AcceptSession(owner) {
		return errors.New("native continuation capture policy changed")
	}
	matched := false
	for _, root := range cfg.NativeRoots {
		if root.Agent == "muse" {
			fingerprint, err := archive.NativeRootFingerprint(root)
			matched = err == nil && fingerprint == i.NativeAdmission.RootFingerprint && root.Revision == owner.NativeBinding.RootRevision
		}
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
		if err := s.checkNativeContinuation(context.Background(), s.nativeContinuation); err != nil {
			return err
		}
		if check != nil {
			return check()
		}
		return nil
	}
	return w
}
