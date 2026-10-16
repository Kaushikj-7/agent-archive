package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/destination"
	"github.com/wangjohn/agent-archive/internal/local"
)

type nativeValidationCommit struct {
	owner archive.SessionRegistration
	next  *archive.NativeSourceBinding
}

func (p *nativeValidationCommit) check(previous, next archive.SessionRegistration, found bool) error {
	if !found || !nativeValidationOwnerMatches(previous, p.owner) || previous.TranscriptPath != p.owner.TranscriptPath || !sameNativeImportBinding(previous.NativeBinding, p.owner.NativeBinding) || !sameNativeImportBinding(next.NativeBinding, p.next) || !p.next.AdvancesCheckpoint(previous.NativeBinding) {
		return errors.New("native validation owner or expected checkpoint changed")
	}
	if previous.NativeBinding == nil && previous.TranscriptPath != "" {
		return errors.New("native locator lost original binding")
	}
	return nil
}

// Only actual later lifecycle observations may overtake preparation.
func nativeValidationOwnerMatches(current, expected archive.SessionRegistration) bool {
	current.HookObservedAt = expected.HookObservedAt
	current.LastHead = expected.LastHead
	current.LastHeadSeenAt = expected.LastHeadSeenAt
	a, errA := json.Marshal(current)
	b, errB := json.Marshal(expected)
	return errA == nil && errB == nil && bytes.Equal(a, b)
}

// CommitNativeValidation stages a complete validated binding outside hooks.lock,
// verifies its live source after every stage, then fences current policy and owner
// under hooks→request locks for the atomic rename. It never creates an owner.
// verify must retain its snapshot until this method returns; it must independently
// verify both the expected old raw prefix and the new complete captured boundary.
func (s *Store) CommitNativeValidation(ctx context.Context, owner archive.SessionRegistration, root archive.NativeRoot, next *archive.NativeSourceBinding, verify func(context.Context) error) (updated archive.SessionRegistration, err error) {
	if owner.CaptureFrozen || owner.NativeChild || owner.Imported() || owner.HistoricalProject != nil || s.hook || verify == nil || !safeFileComponent(owner.ArchiveSessionID) || root.Validate() != nil || next == nil || !next.AdvancesCheckpoint(owner.NativeBinding) || root.Agent != next.Agent || root.Revision != next.RootRevision || !local.PathWithin(next.Path, root.Canonical) {
		return updated, errors.New("native validation requires prepared source authority")
	}
	// Freeze caller-owned pointer members before invoking any external verifier.
	ownerBytes, err := json.Marshal(owner)
	if err != nil {
		return updated, err
	}
	if err = json.Unmarshal(ownerBytes, &owner); err != nil {
		return updated, err
	}
	nextCopy := *next
	checkpointCopy := *next.Checkpoint
	nextCopy.Checkpoint = &checkpointCopy
	next = &nextCopy
	prepared := &nativeValidationCommit{owner: owner, next: next}
	err = s.writeUnderLock(lockedWrite{
		path: s.registrationPath(owner.ArchiveSessionID), nativeValidation: prepared,
		beforeLock: func() error { return verify(ctx) },
		lock: func() (func(), error) {
			unlockHooks, err := s.namedLockWait("hooks.lock", time.Second)
			if err != nil {
				return nil, err
			}
			unlockRequest, err := s.lockRequest(owner.ArchiveSessionID)
			if err != nil {
				unlockHooks()
				return nil, err
			}
			return func() { unlockRequest(); unlockHooks() }, nil
		},
		check: func() error { return s.nativeValidationFence(ctx, owner, root, next) },
		change: func(current fileSnapshot) (any, bool, error) {
			if err := ctx.Err(); err != nil {
				return nil, false, err
			}
			if !current.found {
				return nil, false, errors.New("native validation owner disappeared")
			}
			var reg archive.SessionRegistration
			if err := json.Unmarshal(current.data, &reg); err != nil {
				return nil, false, err
			}
			reg.NativeBinding = next
			reg.TranscriptPath = next.Path
			if err := reg.Validate(); err != nil {
				return nil, false, err
			}
			updated = reg
			return reg, true, nil
		},
	})
	return updated, err
}

func (s *Store) nativeValidationFence(ctx context.Context, owner archive.SessionRegistration, root archive.NativeRoot, next *archive.NativeSourceBinding) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := os.Lstat(filepath.Join(s.home, "setup-transaction.json")); !errors.Is(err, os.ErrNotExist) {
		return errors.New("setup pending before native validation commit")
	}
	cfg, found, err := config.Load(s.home)
	if err != nil {
		return err
	}
	matched := false
	for _, current := range cfg.NativeRoots {
		matched = matched || current == root
	}
	if !found || !matched || !cfg.NativeAuthorityProtection || cfg.Paused || !nativeCapturePolicyAllows(cfg, owner, next) {
		return errors.New("native validation requires current capture permission and root")
	}
	if _, removed, err := s.Removal(owner.Harness.Name, owner.NativeSessionID); err != nil || removed {
		return errors.New("native validation owner was removed")
	}
	return nil
}

// Native capture never inherits legacy all-harness/empty-project/time-only
// destination fallbacks. Both actual native birth and owner admission must fit
// the current explicit project window; physical mapping already ran outside locks.
func nativeCapturePolicyAllows(cfg config.Config, owner archive.SessionRegistration, binding *archive.NativeSourceBinding) bool {
	if owner.Imported() || owner.NativeChild || owner.CaptureFrozen || !slices.Contains(cfg.Harnesses, binding.Agent) || !cfg.Archive.Eligible(owner.ProjectRoot, binding.NativeCreatedAt) || !cfg.AcceptSession(owner) || !cfg.NativeValidationInCurrentDestination(owner, binding) || !nativeDestinationConfigured(cfg.Storage) {
		return false
	}
	var selected archive.ProjectActivation
	found := false
	for _, project := range cfg.Archive.Projects {
		if local.PathWithin(binding.Cwd, project.Root) && (!found || len(project.Root) > len(selected.Root) || len(project.Root) == len(selected.Root) && !project.Included) {
			selected = project
			found = true
		}
	}
	return found && selected.Included && selected.Root == owner.ProjectRoot && selected.ProjectID == owner.ProjectID && !selected.ActivatedAt.IsZero() && !binding.NativeCreatedAt.Before(selected.ActivatedAt) && !owner.Admitted().Before(selected.ActivatedAt)
}

func nativeDestinationConfigured(storage destination.Config) bool {
	if strings.TrimSpace(storage.Bucket) == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(storage.Provider)) {
	case destination.ProviderS3:
		return true
	case destination.ProviderR2:
		_, err := destination.R2Endpoint(storage.R2Endpoint, storage.R2AccountID)
		return err == nil
	default:
		return false
	}
}
