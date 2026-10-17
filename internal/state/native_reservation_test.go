package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/local"
)

func TestNativeReservationCrashRequiresExactFreshProof(t *testing.T) {
	t.Parallel()
	fixture := newNativeAdmissionFixture(t)
	prepared, err := fixture.prepare(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	fixture.store.onIndexStep = func(step string) error {
		if step == "reservation" {
			return errors.New("synthetic reservation crash")
		}
		return nil
	}
	if _, err := fixture.store.CommitNativeAdmission(t.Context(), prepared); err == nil {
		t.Fatal("crash not injected")
	}
	key := agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: fixture.candidate.Intent.NativeSessionID}
	path := qualifiedSessionIndexPath(fixture.store.home, key)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	reserved, found, err := fixture.store.readQualifiedIndex(key)
	if err != nil || !found || !supportedNativeReservation(reserved.NativeReservation) {
		t.Fatal(reserved, found, err)
	}
	if _, found, err := fixture.store.LoadRegistration(reserved.ArchiveSessionID); err != nil || found {
		t.Fatal("partial owner visible", found, err)
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	fixture.assertReleased(t)
	// A different original observation cannot inherit the unfinished ID even if
	// source bytes and current window are unchanged and a new full Filter passes.
	changed := fixture
	changed.candidate.Intent.ObservedAt = changed.candidate.Intent.ObservedAt.Add(1)
	if next, err := changed.prepare(t.Context()); err == nil {
		_ = next.Close()
		t.Fatal("reservation rebound to different observation")
	}
	fixture.assertReleased(t)
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("refused retry changed reservation", err)
	}
	fixture.store.onIndexStep = nil
	retried, err := fixture.prepare(t.Context())
	if err != nil {
		t.Fatal("exact fresh source proof refused", err)
	}
	if retried.reg.ArchiveSessionID != reserved.ArchiveSessionID {
		t.Fatal("retry minted replacement ID")
	}
	owner, err := fixture.store.CommitNativeAdmission(t.Context(), retried)
	if err != nil {
		t.Fatal(err)
	}
	if err := retried.Close(); err != nil {
		t.Fatal(err)
	}
	fixture.assertReleased(t)
	if owner.NativeAdmission == nil || owner.NativeBinding == nil || owner.NativeBinding.Version != 2 {
		t.Fatal("incomplete owner")
	}
	committed, found, err := fixture.store.readQualifiedIndex(key)
	if err != nil || !found || committed.Reservation != "" || committed.NativeReservation != "" {
		t.Fatal(committed, found, err)
	}
}

func TestNativeReservationMissingOrFutureAuthorityPreserved(t *testing.T) {
	t.Parallel()
	for _, marker := range []string{"", nativeReservationPrefix + strings.Repeat("0", 64), "muse-root-reservation-v2:" + strings.Repeat("0", 64)} {
		t.Run(marker, func(t *testing.T) {
			t.Parallel()
			fixture := newNativeAdmissionFixture(t)
			key := agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: fixture.candidate.Intent.NativeSessionID}
			entry := indexEntry(key, strings.Repeat("a", 32))
			entry.Reservation = strings.Repeat("b", 32)
			entry.NativeReservation = marker
			path := qualifiedSessionIndexPath(fixture.store.home, key)
			if err := local.Write(path, entry); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if prepared, err := fixture.prepare(t.Context()); err == nil {
				_ = prepared.Close()
				t.Fatal("unproven pending reservation reused")
			}
			fixture.assertReleased(t)
			if err := fixture.store.RequestSessionIndexRecovery(key); err == nil {
				t.Fatal("repair consumed native pending authority")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("pending authority mutated", err)
			}
		})
	}
}

func TestNativeReservationCompleteOwnerRecoveryAndFutureRefusal(t *testing.T) {
	t.Parallel()
	fixture := newNativeAdmissionFixture(t)
	prepared, err := fixture.prepare(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	fixture.store.onIndexStep = func(step string) error {
		if step == "registration" {
			return errors.New("synthetic post-owner crash")
		}
		return nil
	}
	if _, err := fixture.store.CommitNativeAdmission(t.Context(), prepared); err == nil {
		t.Fatal("crash not injected")
	}
	id := prepared.reg.ArchiveSessionID
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	fixture.assertReleased(t)
	owner, found, err := fixture.store.LoadRegistration(id)
	if err != nil || !found || owner.Validate() != nil || owner.NativeAdmission == nil || owner.NativeBinding == nil {
		t.Fatal("partial owner after crash", owner, found, err)
	}
	key := agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: fixture.candidate.Intent.NativeSessionID}
	entry, found, err := fixture.store.readQualifiedIndex(key)
	if err != nil || !found || !supportedNativeReservation(entry.NativeReservation) {
		t.Fatal(entry, found, err)
	}
	path := qualifiedSessionIndexPath(fixture.store.home, key)
	future := entry
	future.NativeReservation = "muse-root-reservation-v2:" + strings.Repeat("0", 64)
	if err := local.Write(path, future); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, repair := range []func() error{
		func() error { return fixture.store.finishSessionIndex(key, id) },
		func() error { return fixture.store.recoverRegistrationOwners(key, []string{id}) },
		func() error {
			return fixture.store.recoverCandidateIndex(t.Context(), SubagentCandidate{ArchiveSessionID: id, NativeSessionID: key.NativeID, Harness: owner.Harness})
		},
		func() error { return fixture.store.RequestSessionIndexRecovery(key) },
	} {
		if err := repair(); !errors.Is(err, errUnsupportedNativeReservation) {
			t.Fatalf("future authority not refused: %v", err)
		}
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("future authority repaired", err)
		}
	}
	if err := local.Write(path, entry); err != nil {
		t.Fatal(err)
	}
	fixture.store.onIndexStep = nil
	if err := fixture.store.recoverRegistrationOwners(key, []string{id}); err != nil {
		t.Fatal("complete matching owner cannot finish", err)
	}
	committed, found, err := fixture.store.readQualifiedIndex(key)
	if err != nil || !found || committed.Reservation != "" || committed.NativeReservation != "" {
		t.Fatal(committed, found, err)
	}
	ownerBytes, err := os.ReadFile(fixture.store.registrationPath(id))
	if err != nil {
		t.Fatal(err)
	}
	dropped := owner
	dropped.NativeAdmission = nil
	if err := fixture.store.SaveRegistration(dropped); err == nil {
		t.Fatal("generic save dropped immutable birth admission")
	}
	after, err := os.ReadFile(fixture.store.registrationPath(id))
	if err != nil || !bytes.Equal(ownerBytes, after) {
		t.Fatal("failed generic save changed proof", err)
	}
}

func TestNativeReservationClosedWireRefusesFutureAndAmbiguousAuthority(t *testing.T) {
	t.Parallel()
	key := agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: "opaque-root"}
	entry := indexEntry(key, strings.Repeat("a", 32))
	entry.Reservation = strings.Repeat("b", 32)
	entry.NativeReservation = nativeReservationPrefix + strings.Repeat("c", 64)
	raw, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	for _, changed := range [][]byte{
		bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":2`), 1),
		bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":1,"Version":1`), 1),
		bytes.Replace(raw, []byte(`"native_reservation":`), []byte(`"NATIVE_RESERVATION":`), 1),
		bytes.Replace(raw, []byte(`"native_reservation":"`+entry.NativeReservation+`"`), []byte(`"native_reservation":null`), 1),
		bytes.Replace(raw, []byte(`"native_reservation":"`+entry.NativeReservation+`"`), []byte(`"native_reservation":{"future":true}`), 1),
		bytes.Replace(raw, []byte(`"archive_session_id":"`+entry.ArchiveSessionID+`"`), []byte(`"archive_session_id":"future-owner"`), 1),
		append([]byte(`{"future_authority":true,`), raw[1:]...),
	} {
		if bytes.Equal(changed, raw) {
			t.Fatal("wire mutation did not change")
		}
		var decoded qualifiedSessionIndexEntry
		if err := json.Unmarshal(changed, &decoded); !errors.Is(err, errUnsupportedNativeReservation) {
			t.Fatalf("ambiguous authority accepted: %s %v", changed, err)
		}
	}
	var decoded qualifiedSessionIndexEntry
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded != entry {
		t.Fatal(decoded, err)
	}
}

func TestNativeReservationActivePackedReadAndPhysicalCensus(t *testing.T) {
	t.Parallel()
	store, _, marker := packedOwnerFixture(t)
	key := agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: "opaque-root"}
	hash := packedIndexHash(key)
	entry := indexEntry(key, strings.Repeat("a", 32))
	entry.Reservation = strings.Repeat("b", 32)
	entry.NativeReservation = "muse-root-reservation-v2:" + strings.Repeat("c", 64)
	packed := packedSessionIndex{Version: 1, Epoch: marker.PackedEpoch, Revision: marker.PackedRevision, Inventory: marker.PackedInventory, Entries: map[string]qualifiedSessionIndexEntry{hash: entry}}
	raw, err := encodePackedIndex(packed)
	if err != nil {
		t.Fatal(err)
	}
	path := packedIndexPath(store.home, hash[:2])
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	for _, complete := range []bool{false, true} {
		marker.Complete = complete
		if err := local.Write(filepath.Join(store.home, sessionIndexMarkerFile), marker); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.readPackedEntry(hash, marker); !errors.Is(err, errUnsupportedNativeReservation) {
			t.Fatalf("active future marker became cache miss: %v", err)
		}
		if _, err := store.readQualifiedIndexSnapshot(qualifiedSessionIndexPath(store.home, key), false); !errors.Is(err, errUnsupportedNativeReservation) {
			t.Fatalf("future packed marker fell through incomplete fallback: %v", err)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, after) {
		t.Fatal("active packed authority mutated", err)
	}
	// Current native reservations stay in the physical authoritative overlay.
	fixture := newNativeAdmissionFixture(t)
	prepared, err := fixture.prepare(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	fixture.store.onIndexStep = func(step string) error {
		if step == "reservation" {
			return errors.New("synthetic crash")
		}
		return nil
	}
	if _, err := fixture.store.CommitNativeAdmission(t.Context(), prepared); err == nil {
		t.Fatal("crash not injected")
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	fixture.assertReleased(t)
	key = agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: fixture.candidate.Intent.NativeSessionID}
	physicalPath := qualifiedSessionIndexPath(fixture.store.home, key)
	physical, err := os.ReadFile(physicalPath)
	if err != nil {
		t.Fatal(err)
	}
	fixture.store.onIndexStep = nil
	if err := fixture.store.MarkSessionIndexRecoveryNeeded(); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.RecoverSessionIndexScheduled(t.Context(), SessionIndexRecoverySlice); err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(physicalPath)
	if err != nil || !bytes.Equal(physical, current) {
		t.Fatal("census stripped pending native reservation", err)
	}
}

func FuzzNativeReservationWire(f *testing.F) {
	entry := indexEntry(agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: "opaque-root"}, strings.Repeat("a", 32))
	entry.Reservation = strings.Repeat("b", 32)
	entry.NativeReservation = nativeReservationPrefix + strings.Repeat("c", 64)
	raw, err := json.Marshal(entry)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(raw)
	f.Add([]byte(`{"native_reservation":{"future":true}}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 32<<10 {
			return
		}
		var decoded qualifiedSessionIndexEntry
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return
		}
		if decoded.NativeReservation != "" && !supportedNativeReservationEntry(decoded) {
			t.Fatal("decoder manufactured supported reservation authority")
		}
	})
}
