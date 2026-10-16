package state

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/destination"
	"github.com/wangjohn/agent-archive/internal/local"
)

func nativeValidationFixture(t *testing.T) (*Store, archive.SessionRegistration, archive.NativeRoot, *archive.NativeSourceBinding) {
	t.Helper()
	s := newTestStore(t)
	reg := registration(t)
	reg.Harness.Name = "muse"
	reg.TranscriptPath = ""
	root := archive.NativeRoot{Agent: "muse", Configured: "/native", Canonical: "/native", Revision: "one"}
	cfg := config.Config{RetentionDays: 90, Harnesses: []string{"muse"}, NativeRoots: []archive.NativeRoot{root}, Storage: destination.Config{Provider: destination.ProviderS3, Bucket: "synthetic-validation", Prefix: "synthetic", AWSProfile: "synthetic"}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: reg.ProjectRoot, ProjectID: reg.ProjectID, Included: true, ActivatedAt: reg.SessionStartedAt.Add(-time.Hour)}}}}
	reg.DestinationID = cfg.DestinationID()
	if e := s.SaveRegistration(reg); e != nil {
		t.Fatal(e)
	}
	if e := config.Save(s.home, cfg); e != nil {
		t.Fatal(e)
	}
	hash := sha256.Sum256([]byte("synthetic accepted native boundary\n"))
	binding := &archive.NativeSourceBinding{Agent: "muse", Version: 2, NativeID: reg.NativeSessionID, NativeCreatedAt: reg.SessionStartedAt, Cwd: reg.ProjectRoot, Path: "/native/session.jsonl", RootRevision: "one", Checkpoint: &archive.NativeCheckpoint{Version: 1, Domain: "muse-ordinary", Algorithm: "sha256", Boundary: 33, SHA256: hex.EncodeToString(hash[:])}}
	return s, reg, root, binding
}

func TestNativeValidationCommitsAfterStageOutsideHooksAndPreservesLateHook(t *testing.T) {
	t.Parallel()
	s, owner, root, binding := nativeValidationFixture(t)
	syncs, verifies := 0, 0
	s.onWriteSync = func() {
		syncs++
		unlock, e := local.NamedLockWait(s.home, "hooks.lock", 0)
		if e != nil {
			t.Error("sync under hooks.lock", e)
		} else {
			unlock()
		}
	}
	updated, e := s.CommitNativeValidation(t.Context(), owner, root, binding, func(context.Context) error {
		verifies++
		if syncs < verifies {
			t.Fatal("verified before staging")
		}
		unlock, e := local.NamedLockWait(s.home, "hooks.lock", 0)
		if e != nil {
			return e
		}
		unlock()
		if verifies == 1 {
			return s.RecordHookObservation(owner.ArchiveSessionID, owner.RegisteredAt.Add(time.Hour))
		}
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	if verifies != 2 || !updated.HookObservedAt.Equal(owner.RegisteredAt.Add(time.Hour)) || !updated.NativeBinding.SameCheckpoint(binding) || updated.TranscriptPath != binding.Path {
		t.Fatal(updated, verifies)
	}
	if e := s.SaveRegistration(owner); e == nil {
		t.Fatal("blind old owner dropped checkpoint")
	}
	if _, e := s.UpdateRegistration(owner.ArchiveSessionID, func(r *archive.SessionRegistration) error { r.NativeBinding.Checkpoint.Boundary++; return nil }); e == nil {
		t.Fatal("generic update advanced checkpoint")
	}
	if _, e := s.UpdateRegistration(owner.ArchiveSessionID, func(r *archive.SessionRegistration) error {
		r.HookObservedAt = r.HookObservedAt.Add(time.Second)
		return nil
	}); e != nil {
		t.Fatal("same proof hook refused", e)
	}
}

type nativeCommitFailure string

const (
	commitFailureVerification  nativeCommitFailure = "verification"
	commitFailurePause         nativeCommitFailure = "pause"
	commitFailureRoot          nativeCommitFailure = "root"
	commitFailureSetup         nativeCommitFailure = "setup"
	commitFailureRemoval       nativeCommitFailure = "removal"
	commitFailureDestination   nativeCommitFailure = "destination"
	commitFailureFreeze        nativeCommitFailure = "freeze"
	commitFailureExpectedProof nativeCommitFailure = "expected-proof"
)

func TestNativeValidationFailureKeepsExactProtectedCheckpoint(t *testing.T) {
	t.Parallel()
	for _, mode := range []nativeCommitFailure{commitFailureVerification, commitFailurePause, commitFailureRoot, commitFailureSetup, commitFailureRemoval, commitFailureDestination, commitFailureFreeze, commitFailureExpectedProof} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			s, owner, root, binding := nativeValidationFixture(t)
			bound, e := s.CommitNativeValidation(t.Context(), owner, root, binding, func(context.Context) error { return nil })
			if e != nil {
				t.Fatal(e)
			}
			before, e := os.ReadFile(s.registrationPath(owner.ArchiveSessionID))
			if e != nil {
				t.Fatal(e)
			}
			next := *binding
			cp := *binding.Checkpoint
			cp.Boundary++
			next.Checkpoint = &cp
			_, e = s.CommitNativeValidation(t.Context(), bound, root, &next, func(verifyCtx context.Context) error {
				switch mode {
				case commitFailureVerification:
					return errors.New("synthetic invalid suffix")
				case commitFailureSetup:
					return os.WriteFile(filepath.Join(s.home, "setup-transaction.json"), []byte("{}"), 0600)
				case commitFailureFreeze:
					_, err := s.UpdateRegistration(owner.ArchiveSessionID, func(r *archive.SessionRegistration) error { r.CaptureFrozen = true; return nil })
					return err
				case commitFailureExpectedProof:
					// Another independently prepared validation wins; stale expected proof refuses.
					other := next
					otherCP := cp
					otherCP.Boundary++
					other.Checkpoint = &otherCP
					_, err := s.CommitNativeValidation(verifyCtx, bound, root, &other, func(context.Context) error { return nil })
					return err
				case commitFailureRemoval:
					return os.Remove(s.registrationPath(owner.ArchiveSessionID))
				case commitFailurePause, commitFailureRoot, commitFailureDestination:
					cfg, _, err := config.Load(s.home)
					if err != nil {
						return err
					}
					if mode == commitFailurePause {
						cfg.Paused = true
					}
					if mode == commitFailureRoot {
						cfg.NativeRoots[0].Revision = "new"
					}
					if mode == commitFailureDestination {
						cfg.DestinationSince = owner.Admitted().Add(time.Hour)
					}
					return config.Save(s.home, cfg)
				default:
					return errors.New("unknown synthetic commit failure")
				}
			})
			if e == nil {
				t.Fatal("racing authority accepted")
			}
			after, err := os.ReadFile(s.registrationPath(owner.ArchiveSessionID))
			if mode == commitFailureRemoval {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatal("owner resurrected", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode == commitFailureFreeze || mode == commitFailureExpectedProof {
				return
			}
			if !bytes.Equal(before, after) {
				t.Fatal("failed commit changed checkpoint")
			}
		})
	}
}

func TestNativeValidationDoesNotReconstructV1OrLoseProofOnCacheRemoval(t *testing.T) {
	t.Parallel()
	s, owner, root, binding := nativeValidationFixture(t)
	bound, e := s.CommitNativeValidation(t.Context(), owner, root, binding, func(context.Context) error { return nil })
	if e != nil {
		t.Fatal(e)
	}
	// Last durably accepted validation survives a disposable failed scan cache.
	if e = s.SaveScanSignature(owner.ArchiveSessionID, ScanSignature{Failed: true}); e != nil {
		t.Fatal(e)
	}
	if e = s.RemoveScanSignature(owner.ArchiveSessionID); e != nil {
		t.Fatal(e)
	}
	loaded, found, e := s.LoadRegistration(owner.ArchiveSessionID)
	if e != nil || !found || !sameNativeImportBinding(loaded.NativeBinding, bound.NativeBinding) {
		t.Fatal(e)
	}
	legacy := *binding
	legacy.Version = 1
	legacy.Checkpoint = nil
	if binding.AdvancesCheckpoint(&legacy) {
		t.Fatal("v1 silently upgraded")
	}
	for _, mutate := range []func(*archive.NativeSourceBinding){func(b *archive.NativeSourceBinding) { b.Checkpoint = nil }, func(b *archive.NativeSourceBinding) { b.Version = 1; b.Checkpoint = nil }, func(b *archive.NativeSourceBinding) {
		c := *b.Checkpoint
		c.SHA256 = hex.EncodeToString(make([]byte, 32))
		b.Checkpoint = &c
	}} {
		copyBinding := *binding
		mutate(&copyBinding)
		replacement := bound
		replacement.NativeBinding = &copyBinding
		if e = s.SaveRegistration(replacement); e == nil {
			t.Fatal("proof dropped/downgraded/replaced")
		}
	}
	// The current-v1 writer's validator is pinned separately by the old-source
	// compatibility invocation; these bytes retain version 2 for that refusal.
	raw, e := json.Marshal(bound)
	if e != nil || !bytes.Contains(raw, []byte(`"version":2`)) {
		t.Fatal(e)
	}
}

type nativePolicyFailure string

const (
	policyFailureDisabled                nativePolicyFailure = "disabled"
	policyFailureNoProject               nativePolicyFailure = "no-project"
	policyFailureUnknownActivation       nativePolicyFailure = "unknown-activation"
	policyFailureEmptyHarnesses          nativePolicyFailure = "empty-harnesses"
	policyFailureOtherHarness            nativePolicyFailure = "other-harness"
	policyFailureNativeBeforeProject     nativePolicyFailure = "native-before-project"
	policyFailureNativeBeforeDestination nativePolicyFailure = "native-before-destination"
	policyFailureEmptyDestination        nativePolicyFailure = "empty-destination"
	policyFailureUnconfiguredDestination nativePolicyFailure = "unconfigured-destination"
	policyFailureNestedExclusion         nativePolicyFailure = "nested-exclusion"
	policyFailureProjectId               nativePolicyFailure = "project-id"
)

func TestNativeValidationRequiresExplicitCurrentCaptureWindows(t *testing.T) {
	t.Parallel()
	for _, mode := range []nativePolicyFailure{policyFailureDisabled, policyFailureNoProject, policyFailureUnknownActivation, policyFailureEmptyHarnesses, policyFailureOtherHarness, policyFailureNativeBeforeProject, policyFailureNativeBeforeDestination, policyFailureEmptyDestination, policyFailureUnconfiguredDestination, policyFailureNestedExclusion, policyFailureProjectId} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			s, owner, root, binding := nativeValidationFixture(t)
			cfg, _, e := config.Load(s.home)
			if e != nil {
				t.Fatal(e)
			}
			switch mode {
			case policyFailureDisabled:
				cfg.Archive.Enabled = false
			case policyFailureNoProject:
				cfg.Archive.Projects = nil
			case policyFailureUnknownActivation:
				cfg.Archive.Projects[0].ActivatedAt = time.Time{}
			case policyFailureEmptyHarnesses:
				cfg.Harnesses = nil
			case policyFailureOtherHarness:
				cfg.Harnesses = []string{"codex"}
			case policyFailureNativeBeforeProject:
				cfg.Archive.Projects[0].ActivatedAt = binding.NativeCreatedAt.Add(time.Hour)
				owner.AdmittedAt = binding.NativeCreatedAt.Add(2 * time.Hour)
			case policyFailureNativeBeforeDestination:
				cfg.DestinationSince = binding.NativeCreatedAt.Add(time.Hour)
				owner.AdmittedAt = binding.NativeCreatedAt.Add(2 * time.Hour)
			case policyFailureEmptyDestination:
				owner.DestinationID = ""
			case policyFailureUnconfiguredDestination:
				cfg.Storage = destination.Config{}
				owner.DestinationID = cfg.DestinationID()
			case policyFailureNestedExclusion:
				binding.Cwd = filepath.Join(owner.ProjectRoot, "excluded", "sub")
				cfg.Archive.Projects = append(cfg.Archive.Projects, archive.ProjectActivation{Root: filepath.Join(owner.ProjectRoot, "excluded"), ProjectID: "excluded", Included: false, ActivatedAt: owner.SessionStartedAt})
			case policyFailureProjectId:
				cfg.Archive.Projects[0].ProjectID = "another-owner"
			}
			if e = s.SaveRegistration(owner); e != nil {
				t.Fatal(e)
			}
			if e = config.Save(s.home, cfg); e != nil {
				t.Fatal(e)
			}
			before, e := os.ReadFile(s.registrationPath(owner.ArchiveSessionID))
			if e != nil {
				t.Fatal(e)
			}
			if _, e = s.CommitNativeValidation(t.Context(), owner, root, binding, func(context.Context) error { return nil }); e == nil {
				t.Fatal("legacy fallback supplied native capture authority")
			}
			after, e := os.ReadFile(s.registrationPath(owner.ArchiveSessionID))
			if e != nil || !bytes.Equal(before, after) {
				t.Fatal("failed current window mutated owner", e)
			}
		})
	}
}
