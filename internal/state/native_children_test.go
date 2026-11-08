package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// syntheticMuseChild is the child session the Muse adapter's synthetic
// child-link fixture links.
const syntheticMuseChild = "00000000-0000-7000-8000-0000000000c1"

// withChildLinkSource replaces a native fixture's session with the Muse
// adapter's synthetic session that links one child.
func withChildLinkSource(t *testing.T, f nativeAdmissionFixture) nativeAdmissionFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "agents", "muse", "testdata", "synthetic-child-link.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.ReplaceAll(raw, []byte("/work/synthetic-muse"), []byte(f.candidate.ProjectRoot))
	if err := os.WriteFile(f.path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

// assertChildReserved checks the child key holds a plain reservation of id,
// with no native authority marker.
func assertChildReserved(t *testing.T, s *Store, id string) {
	t.Helper()
	entry, found, err := s.readQualifiedIndex(agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: syntheticMuseChild})
	if err != nil || !found || entry.ArchiveSessionID != id || entry.Reservation == "" || entry.NativeReservation != "" {
		t.Fatalf("child reservation = %+v, %v", entry, err)
	}
	if _, found, err := s.LoadRegistration(id); err != nil || found {
		t.Fatal("child link registered a child session", found, err)
	}
}

func TestNativeImportRecordsVerifiedChildLinks(t *testing.T) {
	t.Parallel()
	f := nativeImportPrepareFixtureFromNative(t, withChildLinkSource(t, newNativeAdmissionFixture(t)), "", 8<<20)
	p, err := f.prepare(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := p.Close(); err != nil {
			t.Error(err)
		}
	}()
	owner, err := f.native.store.CommitNativeImport(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	stage, found, err := f.native.store.LoadImportStage(t.Context(), owner)
	if err != nil || !found {
		t.Fatal("import stage", found, err)
	}
	links := stage.Bundle.LinkedSessions
	if len(links) != 1 || links[0].Relationship != "subagent" || links[0].Status != archive.LinkedSessionUnavailable || !links[0].ObservedAt.Equal(owner.SessionStartedAt) {
		t.Fatalf("stage links = %+v", links)
	}
	assertChildReserved(t, f.native.store, links[0].SessionID)
	// A second pass over the same parent links the same reservation.
	again, err := f.native.store.nativeChildLinks(owner, archive.FilteredTranscript{SessionIDs: []string{owner.NativeSessionID}, NativeChildIDs: []string{syntheticMuseChild}})
	if err != nil || len(again) != 1 || again[0].Payload["archive_session_id"] != links[0].SessionID {
		t.Fatal("repeated link changed the child reservation", err)
	}
}

func TestNativeAdmissionLinksVerifiedChildrenOnlyAfterCommit(t *testing.T) {
	t.Parallel()
	f := withChildLinkSource(t, newNativeAdmissionFixture(t))
	p, err := f.prepare(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := p.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := f.store.LinkNativeChildren(p); !errors.Is(err, errNativeChildLinks) {
		t.Fatalf("uncommitted admission linked children: %v", err)
	}
	if err := f.store.LinkNativeChildren(nil); !errors.Is(err, errNativeChildLinks) {
		t.Fatalf("nil admission linked children: %v", err)
	}
	owner, err := f.store.CommitNativeAdmission(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.LinkNativeChildren(p); err != nil {
		t.Fatal(err)
	}
	req, found, err := f.store.LoadRequest(owner.ArchiveSessionID)
	if err != nil || !found {
		t.Fatal("no request carries the links", found, err)
	}
	var linked []string
	for _, e := range req.HookEvidence {
		if e.Kind == archive.EvidenceKindLinkedSession {
			linked = append(linked, firstStringPayload(e, "archive_session_id"))
		}
	}
	if len(linked) != 1 {
		t.Fatalf("request links = %v", linked)
	}
	assertChildReserved(t, f.store, linked[0])
	// Linking again saves identical evidence, which the request keeps once.
	if err := f.store.LinkNativeChildren(p); err != nil {
		t.Fatal(err)
	}
	if again, _, err := f.store.LoadRequest(owner.ArchiveSessionID); err != nil || len(again.HookEvidence) != len(req.HookEvidence) {
		t.Fatal("repeated link grew the request", err)
	}
}

func firstStringPayload(e archive.SupplementalEvidence, key string) string {
	s, _ := e.Payload[key].(string)
	return s
}

func TestForgedChildLinksGetNoReservation(t *testing.T) {
	t.Parallel()
	f := withChildLinkSource(t, newNativeAdmissionFixture(t))
	p, err := f.prepare(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := p.Close(); err != nil {
			t.Error(err)
		}
	}()
	owner, err := f.store.CommitNativeAdmission(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	s := f.store
	pass := func(children ...string) archive.FilteredTranscript {
		return archive.FilteredTranscript{SessionIDs: []string{owner.NativeSessionID}, NativeChildIDs: children}
	}
	many := make([]string, maxNativeChildLinks+1)
	for i := range many {
		many[i] = "00000000-0000-7000-8000-" + strings.Repeat("0", 8) + string(rune('a'+i%26)) + string(rune('a'+i/26%26)) + string(rune('a'+i/676%26)) + "0"
	}
	other := owner
	other.Harness.Name = "codex"
	unbound := owner
	unbound.NativeBinding = nil
	child := owner
	child.NativeChild = true
	for name, c := range map[string]struct {
		owner    archive.SessionRegistration
		filtered archive.FilteredTranscript
	}{
		"another session's pass": {owner, archive.FilteredTranscript{SessionIDs: []string{"00000000-0000-7000-8000-0000000000ff"}, NativeChildIDs: []string{syntheticMuseChild}}},
		"no session identity":    {owner, archive.FilteredTranscript{NativeChildIDs: []string{syntheticMuseChild}}},
		"the parent itself":      {owner, pass(owner.NativeSessionID)},
		"linked twice":           {owner, pass(syntheticMuseChild, syntheticMuseChild)},
		"unsafe ID":              {owner, pass("../" + syntheticMuseChild)},
		"too many":               {owner, pass(many...)},
		"not Muse":               {other, pass(syntheticMuseChild)},
		"no native binding":      {unbound, pass(syntheticMuseChild)},
		"child parent":           {child, pass(syntheticMuseChild)},
	} {
		if _, err := s.nativeChildLinks(c.owner, c.filtered); !errors.Is(err, errNativeChildLinks) {
			t.Errorf("%s: forged child link accepted: %v", name, err)
		}
	}
	if _, found, err := s.readQualifiedIndex(agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: syntheticMuseChild}); err != nil || found {
		t.Fatal("a refused link left a reservation", found, err)
	}
	// The generic reservation path stays closed to Muse child keys.
	if _, _, err := s.EnsureArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: syntheticMuseChild}); err == nil {
		t.Fatal("generic reservation accepted a Muse child")
	}
	// A child ID that is a root session, or reserved as one, is never linked.
	rootKey := agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: "00000000-0000-7000-8000-0000000000c3"}
	writeIndexForTest(t, s, rootKey, qualifiedSessionIndexEntry{Version: 1, Agent: rootKey.Agent, NativeID: rootKey.NativeID, ArchiveSessionID: owner.ArchiveSessionID})
	reservedKey := agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: "00000000-0000-7000-8000-0000000000c4"}
	writeIndexForTest(t, s, reservedKey, qualifiedSessionIndexEntry{Version: 1, Agent: reservedKey.Agent, NativeID: reservedKey.NativeID, ArchiveSessionID: "00000000-0000-4000-8000-0000000000c4", Reservation: "token", NativeReservation: "muse-root-reservation-v1:" + strings.Repeat("a", 64)})
	for _, id := range []string{rootKey.NativeID, reservedKey.NativeID} {
		// The index reader itself may refuse a root reservation it cannot qualify.
		if links, err := s.nativeChildLinks(owner, pass(id)); err == nil || len(links) != 0 {
			t.Errorf("root %s linked as a child: %v", id, err)
		}
	}
}

// writeIndexForTest writes a session index entry directly, as an earlier
// writer would have left it.
func writeIndexForTest(t *testing.T, s *Store, key agentmeta.SessionKey, entry qualifiedSessionIndexEntry) {
	t.Helper()
	raw, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	path := qualifiedSessionIndexPath(s.home, key)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestNativeValidationRecordsTheSamePassChildLinks(t *testing.T) {
	t.Parallel()
	s, owner, root, binding := nativeValidationFixture(t)
	linked := archive.FilteredTranscript{SessionIDs: []string{owner.NativeSessionID}, NativeChildIDs: []string{syntheticMuseChild}}
	bound, err := s.CommitNativeValidation(t.Context(), owner, root, binding, func(context.Context) error { return nil }, linked)
	if err != nil {
		t.Fatal(err)
	}
	req, found, err := s.LoadRequest(bound.ArchiveSessionID)
	if err != nil || !found || len(req.HookEvidence) != 1 || req.HookEvidence[0].Kind != archive.EvidenceKindLinkedSession {
		t.Fatal("validated pass recorded no child link", found, err)
	}
	assertChildReserved(t, s, firstStringPayload(req.HookEvidence[0], "archive_session_id"))
	// Another session's pass links nothing, and the binding still commits.
	next := *binding
	checkpoint := *binding.Checkpoint
	checkpoint.Boundary++
	next.Checkpoint = &checkpoint
	forged := archive.FilteredTranscript{SessionIDs: []string{"00000000-0000-7000-8000-0000000000ff"}, NativeChildIDs: []string{"00000000-0000-7000-8000-0000000000c5"}}
	updated, err := s.CommitNativeValidation(t.Context(), bound, root, &next, func(context.Context) error { return nil }, forged)
	if !errors.Is(err, errNativeChildLinks) || updated.NativeBinding == nil || updated.NativeBinding.Checkpoint.Boundary != checkpoint.Boundary {
		t.Fatalf("forged pass: %v", err)
	}
	if _, found, err := s.readQualifiedIndex(agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: "00000000-0000-7000-8000-0000000000c5"}); err != nil || found {
		t.Fatal("forged pass reserved a child", found, err)
	}
}
