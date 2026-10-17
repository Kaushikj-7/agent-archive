package state

import (
	"bytes"
	"errors"
	"os"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

func replayConsumerFixture(t *testing.T, marker *archive.Replay) nativeAdmissionFixture {
	t.Helper()
	f := newNativeAdmissionFixture(t)
	cfg, found, err := config.Load(f.store.home)
	if err != nil || !found {
		t.Fatal(err)
	}
	cfg.NativeReplayProtection = true
	if err := config.Save(f.store.home, cfg); err != nil {
		t.Fatal(err)
	}
	f.candidate.Intent.Replay = marker
	return f
}

func TestNativeReplayConsumerFreezesFirstOwnerMarker(t *testing.T) {
	t.Parallel()
	for _, marker := range []*archive.Replay{nil, {}, {RunID: "run-first"}} {
		f := replayConsumerFixture(t, marker)
		var expected *archive.Replay
		if marker != nil {
			value := *marker
			expected = &value
		}
		prepared, err := f.prepare(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		authority, err := prepared.reservationAuthority()
		if err != nil {
			t.Fatal(err)
		}
		if marker != nil {
			marker.RunID = "caller-mutated-after-prepare"
		}
		frozen, err := prepared.reservationAuthority()
		if err != nil || authority != frozen {
			t.Fatal("caller pointer changed reservation authority", err)
		}
		owner, err := f.store.CommitNativeAdmission(t.Context(), prepared)
		if err != nil {
			t.Fatal(err)
		}
		if !sameNativeReplay(owner.Replay, expected) {
			t.Fatal("caller pointer changed first owner marker")
		}
		if err := prepared.Close(); err != nil {
			t.Fatal(err)
		}
		f.assertReleased(t)
		loaded, found, err := f.store.LoadRegistration(owner.ArchiveSessionID)
		if err != nil || !found || !sameNativeReplay(loaded.Replay, expected) {
			t.Fatal("persisted first owner lost marker", err)
		}
	}
}

func TestNativeReplayConsumerReservationRejectsMarkerOnlyRetry(t *testing.T) {
	t.Parallel()
	f := replayConsumerFixture(t, &archive.Replay{RunID: "original"})
	prepared, err := f.prepare(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	f.store.onIndexStep = func(step string) error {
		if step == "reservation" {
			return errors.New("synthetic reservation crash")
		}
		return nil
	}
	if _, err := f.store.CommitNativeAdmission(t.Context(), prepared); err == nil {
		t.Fatal("missing reservation crash")
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	f.assertReleased(t)
	key := agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: f.candidate.Intent.NativeSessionID}
	path := qualifiedSessionIndexPath(f.store.home, key)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := f
	changed.candidate.Intent.Replay = &archive.Replay{RunID: "different"}
	if other, err := changed.prepare(t.Context()); err == nil {
		other.Close()
		t.Fatal("marker-only retry acquired original reservation")
	}
	f.assertReleased(t)
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("refused marker retry changed reservation", err)
	}
	f.store.onIndexStep = nil
	retried, err := f.prepare(t.Context())
	if err != nil {
		t.Fatal("exact replay retry refused", err)
	}
	owner, err := f.store.CommitNativeAdmission(t.Context(), retried)
	if err != nil {
		t.Fatal(err)
	}
	if !sameNativeReplay(owner.Replay, f.candidate.Intent.Replay) {
		t.Fatal("retry changed first owner marker")
	}
	if err := retried.Close(); err != nil {
		t.Fatal(err)
	}
	f.assertReleased(t)
}

func TestNativeReplayConsumerMarkerOnlyReservationCAS(t *testing.T) {
	t.Parallel()
	f := replayConsumerFixture(t, &archive.Replay{RunID: "original"})
	prepared, err := f.prepare(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	key := agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: f.candidate.Intent.NativeSessionID}
	replacement := *prepared
	replacement.candidate = prepared.candidate
	replacement.candidate.Intent.Replay = &archive.Replay{RunID: "different"}
	otherAuthority, err := replacement.reservationAuthority()
	if err != nil {
		t.Fatal(err)
	}
	originalAuthority, err := prepared.reservationAuthority()
	if err != nil || originalAuthority == otherAuthority {
		t.Fatal("marker not bound in reservation digest", err)
	}
	changed := false
	f.store.onIndexStep = func(step string) error {
		if step != "reservation" {
			return nil
		}
		entry, found, err := f.store.readQualifiedIndex(key)
		if err != nil || !found {
			return errors.New("reserved index missing")
		}
		entry.NativeReservation = otherAuthority
		if err := local.Write(qualifiedSessionIndexPath(f.store.home, key), entry); err != nil {
			return err
		}
		changed = true
		return nil
	}
	if _, err := f.store.CommitNativeAdmission(t.Context(), prepared); err == nil || !changed {
		t.Fatal("marker-only index replacement bypassed owner CAS", err)
	}
	if _, found, err := f.store.LoadRegistration(prepared.reg.ArchiveSessionID); err != nil || found {
		t.Fatal("conflicting marker created owner", err)
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	f.assertReleased(t)
}
