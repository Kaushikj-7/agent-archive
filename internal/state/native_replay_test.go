package state

import (
	"bytes"
	"context"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/local"
	"os"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

func TestNativeReplayClassificationImmutableAcrossOrdinaryWriters(t *testing.T) {
	fixture := newNativeAdmissionFixture(t)
	prepared, err := fixture.prepare(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	owner, err := fixture.store.CommitNativeAdmission(t.Context(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	for _, before := range []*archive.Replay{nil, {}, {RunID: "run-one"}} {
		for _, after := range []*archive.Replay{nil, {}, {RunID: "run-one"}, {RunID: "run-two"}} {
			previous := owner
			previous.Replay = before
			next := previous
			next.Replay = after
			err := preserveNativeRegistrationAuthority(previous, next, true, nil, nil)
			if sameNativeReplay(before, after) && err != nil {
				t.Fatal("unchanged replay classification refused", before, err)
			}
			if !sameNativeReplay(before, after) && err == nil {
				t.Fatal("generic native write relabeled owner", before, after)
			}
		}
	}
	next := owner
	next.Replay = &archive.Replay{RunID: "run-one"}
	if err := fixture.store.SaveRegistration(next); err == nil {
		t.Fatal("blind save relabeled ordinary native owner")
	}
	if _, err := fixture.store.UpdateRegistration(owner.ArchiveSessionID, func(r *archive.SessionRegistration) error { r.Replay = &archive.Replay{}; return nil }); err == nil {
		t.Fatal("update relabeled ordinary native owner")
	}
	_, _ = fixture.store.RegisterOrMerge(agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: owner.NativeSessionID}, func(string) archive.SessionRegistration { return next })
	got, found, err := fixture.store.LoadRegistration(owner.ArchiveSessionID)
	if err != nil || !found || got.Replay != nil {
		t.Fatal("merge changed original classification", err)
	}
	fixture.assertReleased(t)
}

func TestNativeReplayPreparedPolicyRequiresStrongerCurrentFence(t *testing.T) {
	fixture := newNativeAdmissionFixture(t)
	prepared, err := fixture.prepare(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := prepared.Close(); err != nil {
			t.Error(err)
		}
	}()
	// Current codec deliberately refuses replay candidates. Exercise the shared
	// prepared policy directly so the future consumer cannot bypass this fence.
	prepared.candidate.Intent.Replay = &archive.Replay{}
	if err := prepared.checkPolicy(t.Context(), time.Now().UTC()); err == nil {
		t.Fatal("prepared policy admitted replay without stronger writer")
	}
	cfg, found, err := config.Load(fixture.store.home)
	if err != nil || !found {
		t.Fatal(err)
	}
	cfg.NativeReplayProtection = true
	if err := config.Save(fixture.store.home, cfg); err != nil {
		t.Fatal(err)
	}
	if err := prepared.checkPolicy(t.Context(), time.Now().UTC()); err != nil {
		t.Fatal("stronger fence changed original qualified policy", err)
	}
	// Removing the capability from the in-memory current view must independently
	// fail the shared predicate used by every pre-CAS policy check.
	cfg.NativeReplayProtection = false
	if nativeAdmissionWindowMatches(cfg, true, prepared.reg, prepared.candidate.Intent, time.Now().UTC()) {
		t.Fatal("current policy predicate ignored replay fence")
	}
}

func TestNativeReplayExistingOwnerRequiresStrongFence(t *testing.T) {
	fixture := newNativeAdmissionFixture(t)
	prepared, err := fixture.prepare(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	owner, err := fixture.store.CommitNativeAdmission(t.Context(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	// Arrange a valid persisted replay owner whose stronger config capability
	// is absent (for example, an incompatible restored settings snapshot).
	owner.Replay = &archive.Replay{RunID: "existing-run"}
	if err := owner.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := local.Write(fixture.store.registrationPath(owner.ArchiveSessionID), owner); err != nil {
		t.Fatal(err)
	}
	cfg, found, err := config.Load(fixture.store.home)
	if err != nil || !found {
		t.Fatal(err)
	}
	intent := fixture.candidate.Intent
	intent.NativeMode = agentapi.NativeOwnerContinuation
	intent.NativeOwnerID = owner.ArchiveSessionID
	intent.NativeWindowGeneration = cfg.MuseCapture.Generation
	intent.Event = "Stop"
	intent.Effects = []agentapi.ReplayEffect{{Event: agentapi.LifecycleEvent{Kind: agentapi.EventStop, Session: agentapi.NativeSession{Agent: agentmeta.Muse, NativeID: owner.NativeSessionID}, ProjectRoot: owner.ProjectRoot, NativeHookContract: "muse-root-hooks-v1", NativeEvent: "Stop", Reason: "native_root_observation"}}}
	if !agentapi.ValidNativeAdmissionIntentShape(intent) {
		t.Fatal("invalid continuation test fixture")
	}
	original, err := os.ReadFile(fixture.store.registrationPath(owner.ArchiveSessionID))
	if err != nil {
		t.Fatal(err)
	}
	t.Run("continuation", func(t *testing.T) {
		if _, err := fixture.store.ForNativeContinuation(t.Context(), intent); err == nil {
			t.Fatal("replay owner continued without stronger current fence")
		}
	})
	t.Run("validation", func(t *testing.T) {
		if _, err := fixture.store.CommitNativeValidation(t.Context(), owner, fixture.candidate.Root, owner.NativeBinding, func(context.Context) error { return nil }); err == nil {
			t.Fatal("replay checkpoint validated without stronger fence")
		}
	})
	t.Run("save", func(t *testing.T) {
		if err := fixture.store.SaveRegistration(owner); err == nil {
			t.Fatal("generic save accepted unfenced replay owner")
		}
	})
	t.Run("update", func(t *testing.T) {
		if _, err := fixture.store.UpdateRegistration(owner.ArchiveSessionID, func(r *archive.SessionRegistration) error {
			r.HookObservedAt = r.HookObservedAt.Add(time.Second)
			return nil
		}); err == nil {
			t.Fatal("generic update accepted unfenced replay owner")
		}
	})
	after, err := os.ReadFile(fixture.store.registrationPath(owner.ArchiveSessionID))
	if err != nil || !bytes.Equal(original, after) {
		t.Fatal("refusal changed original owner", err)
	}
	if cfg.NativeValidationInCurrentDestination(owner, owner.NativeBinding) {
		t.Fatal("config native validation accepted unfenced replay owner")
	}
	cfg.NativeReplayProtection = true
	if err := config.Save(fixture.store.home, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.ForNativeContinuation(t.Context(), intent); err != nil {
		t.Fatal("strongly fenced unchanged replay owner refused", err)
	}
	if _, err := fixture.store.CommitNativeValidation(t.Context(), owner, fixture.candidate.Root, owner.NativeBinding, func(context.Context) error { return nil }); err != nil {
		t.Fatal("strongly fenced checkpoint refused", err)
	}
	if err := fixture.store.SaveRegistration(owner); err != nil {
		t.Fatal("strongly fenced unchanged save refused", err)
	}
	fixture.assertReleased(t)
}
