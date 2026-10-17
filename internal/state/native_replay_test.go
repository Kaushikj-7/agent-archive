package state

import (
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
	defer prepared.Close()
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
