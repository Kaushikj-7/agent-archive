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
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

func TestStagedFactoryRejectsRedigestedMetadataIdentity(t *testing.T) {
	for _, field := range []string{"native", "project", "harness", "harness-version", "harness-mode", "parent", "capture", "filter", "adapter-name", "adapter-version"} {
		t.Run(field, func(t *testing.T) {
			s, p, _ := stagedPreparedFixture(t, true)
			p.StagedSelection = nil
			p.JournalVersion = 0
			var m archive.Metadata
			if err := json.Unmarshal(p.MetadataBytes, &m); err != nil {
				t.Fatal(err)
			}
			switch field {
			case "native":
				m.NativeSessionID += "-other"
			case "project":
				m.ProjectID += "-other"
			case "harness":
				m.Harness.Name = "claude"
			case "harness-version":
				m.Harness.Version = "other"
			case "harness-mode":
				m.Harness.Mode = "other"
			case "parent":
				m.ParentSessionID = "other"
			case "capture":
				m.CapturedAt = m.CapturedAt.AddDate(0, 0, 1)
			case "filter":
				m.FilterVersion = "other"
			case "adapter-name":
				m.Adapter.Name = "other"
			case "adapter-version":
				m.Adapter.Version = "other"
			}
			var err error
			p.MetadataBytes, err = json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			before := stagedAuthorityBytes(t, s, p.Bundle.ArchiveSessionID)
			if _, err := s.FreezeStagedSelection(t.Context(), p.Bundle.ArchiveSessionID, p, ReadyPredecessorAbsent, nil); err == nil {
				t.Fatal("altered metadata minted staged authority")
			}
			assertStagedAuthorityBytes(t, s, p.Bundle.ArchiveSessionID, before)
		})
	}
}

func TestStagedFactoryRejectsSameOwnerReplacementBundle(t *testing.T) {
	s, p, _ := stagedPreparedFixture(t, true)
	p.StagedSelection = nil
	p.JournalVersion = 0
	p.Bundle.NativeRecords = []map[string]any{{"content": "different retained evidence"}}
	source, err := archive.BuildCompressedSource(p.Bundle)
	if err != nil {
		t.Fatal(err)
	}
	p.SourceBytes, p.SourceSHA256 = source.Bytes, source.SHA256
	p.SourceKey, err = archive.SourceObjectKey(p.Bundle, source.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	var m archive.Metadata
	if err = json.Unmarshal(p.MetadataBytes, &m); err != nil {
		t.Fatal(err)
	}
	m.SourceBundle = p.SourceReference()
	p.MetadataBytes, err = json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	before := stagedAuthorityBytes(t, s, p.Bundle.ArchiveSessionID)
	if _, err = s.FreezeStagedSelection(t.Context(), p.Bundle.ArchiveSessionID, p, ReadyPredecessorAbsent, nil); err == nil {
		t.Fatal("replacement bundle minted authority")
	}
	assertStagedAuthorityBytes(t, s, p.Bundle.ArchiveSessionID, before)
}

func TestStagedFactoryRejectsRehashedNoncanonicalSource(t *testing.T) {
	s, p, _ := stagedPreparedFixture(t, true)
	p.StagedSelection = nil
	p.JournalVersion = 0
	p.SourceBytes = []byte("arbitrary same-owner source")
	p.SourceSHA256 = readySHA(p.SourceBytes)
	var err error
	p.SourceKey, err = archive.SourceObjectKey(p.Bundle, p.SourceSHA256)
	if err != nil {
		t.Fatal(err)
	}
	var m archive.Metadata
	if err = json.Unmarshal(p.MetadataBytes, &m); err != nil {
		t.Fatal(err)
	}
	m.SourceBundle = p.SourceReference()
	p.MetadataBytes, err = json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	before := stagedAuthorityBytes(t, s, p.Bundle.ArchiveSessionID)
	if _, err = s.FreezeStagedSelection(t.Context(), p.Bundle.ArchiveSessionID, p, ReadyPredecessorAbsent, nil); err == nil {
		t.Fatal("noncanonical source minted authority")
	}
	assertStagedAuthorityBytes(t, s, p.Bundle.ArchiveSessionID, before)
}

func TestStagedProofRejectsOversizeBeforeDecode(t *testing.T) {
	_, p, _ := stagedPreparedFixture(t, true)
	raw, err := json.Marshal(p.StagedSelection)
	if err != nil {
		t.Fatal(err)
	}
	// JSON whitespace is otherwise semantically identical. The selecting codec
	// bounds the entire incoming wire, before RawMessage or self encoding.
	raw = append(append([]byte{'{'}, []byte(strings.Repeat(" ", 16<<10))...), raw[1:]...)
	var proof StagedSelection
	if err = json.Unmarshal(raw, &proof); err == nil {
		t.Fatal("oversize selecting wire accepted")
	}
}

func TestStagedReleaseRechecksPolicyAfterReadback(t *testing.T) {
	s, p, guard := stagedDetachedFixture(t)
	id := p.Bundle.ArchiveSessionID
	before := stagedAuthorityBytes(t, s, id)
	remote := stagedRemote(p)
	remote.afterRead = func(n int) error {
		if n != 3 {
			return nil
		}
		cfg, _, err := config.Load(s.Home())
		if err != nil {
			return err
		}
		cfg.Paused = true
		return config.Save(s.Home(), cfg)
	}
	if err := s.VerifyAndBeginStagedRelease(t.Context(), id, remote, guard); !errors.Is(err, ErrReadySelectionFrozen) {
		t.Fatalf("changed policy: %v", err)
	}
	if remote.reads != 3 {
		t.Fatalf("readback seam not reached: %d", remote.reads)
	}
	assertStagedAuthorityBytes(t, s, id, before)
}

func TestStagedCancelAndForgetRefuseBeforeEffects(t *testing.T) {
	for _, operation := range []string{"cancel", "forget", "idle", "orphan"} {
		t.Run(operation, func(t *testing.T) {
			s, p, _ := stagedDetachedFixture(t)
			id := p.Bundle.ArchiveSessionID
			reg, _, err := s.LoadRegistration(id)
			if err != nil {
				t.Fatal(err)
			}
			if operation == "cancel" {
				if err := os.Remove(s.registrationPath(id)); err != nil {
					t.Fatal(err)
				}
			}
			before := stagedAuthorityBytes(t, s, id)
			switch operation {
			case "cancel":
				err = s.CancelImportStage(t.Context(), reg)
			case "forget":
				err = s.ForgetSession(id, agentmeta.SessionKey{})
			case "idle":
				_, err = s.ForgetIdleSession(id, agentmeta.SessionKey{}, false, nil)
			case "orphan":
				_, err = s.ForgetOrphan(id)
			}
			if !errors.Is(err, ErrReadySelectionFrozen) {
				t.Fatalf("typed retirement: %v", err)
			}
			assertStagedAuthorityBytes(t, s, id, before)
			if _, err := os.Stat(filepath.Join(s.Home(), importCancelRelative(id))); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("cancel mutated refusal: %v", err)
			}
		})
	}
}

func TestStagedGenericPendingRemovalRefusesCompletedProgress(t *testing.T) {
	s, p, _ := stagedDetachedFixture(t)
	id := p.Bundle.ArchiveSessionID
	receipt := stagedReleaseReceipt{Version: 3, Selection: *p.StagedSelection}
	var err error
	receipt.SHA256, err = s.readyDigest(t.Context(), receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.writeStagedControl(t.Context(), id, "release.json", receipt); err != nil {
		t.Fatal(err)
	}
	progress := stagedReleaseProgress{Version: 3, ReceiptSHA256: receipt.SHA256, Phase: "local-complete"}
	if err = s.writeStagedControl(t.Context(), id, "release-progress.json", progress); err != nil {
		t.Fatal(err)
	}
	before := stagedAuthorityBytes(t, s, id)
	if err = s.RemovePending(id); !errors.Is(err, ErrReadySelectionFrozen) {
		t.Fatalf("generic removal: %v", err)
	}
	assertStagedAuthorityBytes(t, s, id, before)
}

// Controlled handoff seam isolates the required child receipt from parent
// preparation. The whole-group Run test covers production parent ownership.
func TestStagedChildPublishedAloneCannotReleaseRoster(t *testing.T) {
	s, p, _ := stagedDetachedFixture(t)
	id := p.Bundle.ArchiveSessionID
	reg, _, err := s.LoadRegistration(id)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(s.requestPath(id)); err != nil {
		t.Fatal(err)
	}
	parent := ImportStage{Registration: archive.SessionRegistration{ImportBatch: reg.ImportBatch}, Children: []SubagentCandidate{{ArchiveSessionID: id, NativeSessionID: reg.NativeSessionID, ParentNativeSessionID: reg.ParentNativeSessionID, Harness: reg.Harness, ProjectID: reg.ProjectID, StagedImportSHA256: reg.StagedImportSHA256}}}
	proof := &StagedSelection{Core: ReadySelection{DestinationID: reg.DestinationID}}
	if err = s.checkStagedChildHandoffs(t.Context(), parent, proof); err == nil {
		t.Fatal("published-only child authorized roster release")
	}
	receipt := stagedReleaseReceipt{Version: 3, Selection: *p.StagedSelection}
	receipt.SHA256, err = s.readyDigest(t.Context(), receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.writeStagedControl(t.Context(), id, "release.json", receipt); err != nil {
		t.Fatal(err)
	}
	progress := stagedReleaseProgress{Version: 3, ReceiptSHA256: receipt.SHA256, Phase: "local-complete"}
	if err = s.writeStagedControl(t.Context(), id, "release-progress.json", progress); err != nil {
		t.Fatal(err)
	}
	if err = s.checkStagedChildHandoffs(t.Context(), parent, proof); err != nil {
		t.Fatalf("exact completed child handoff control: %v", err)
	}
}

func stagedAuthorityBytes(t *testing.T, s *Store, id string) map[string][]byte {
	t.Helper()
	paths := []string{s.registrationPath(id), s.requestPath(id), s.pendingPath(id), s.publishedPath(id), filepath.Join(s.Home(), importStageRelative(id))}
	out := make(map[string][]byte)
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		out[path] = raw
	}
	return out
}
func assertStagedAuthorityBytes(t *testing.T, s *Store, id string, before map[string][]byte) {
	t.Helper()
	after := stagedAuthorityBytes(t, s, id)
	if len(after) != len(before) {
		t.Fatal("authority files removed or added")
	}
	for path, raw := range before {
		if !bytes.Equal(raw, after[path]) {
			t.Fatalf("authority changed: %s", path)
		}
	}
}
