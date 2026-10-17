package collector

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/wangjohn/agent-archive/internal/storage"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
)

type deniedHistoricalSources struct{ t *testing.T }

func (s deniedHistoricalSources) LookupSources(string) (agentapi.SourceProvider, agentapi.TranscriptFilter, bool) {
	s.t.Fatal("historical-only guard consulted raw native source")
	return nil, nil, false
}

func historicalNativeRegistration(t *testing.T) archive.SessionRegistration {
	t.Helper()
	at := time.Date(2026, 10, 15, 1, 0, 0, 0, time.UTC)
	cp := archive.NativeCheckpoint{Version: 1, Domain: "muse-ordinary", Algorithm: "sha256", Boundary: 1, SHA256: strings.Repeat("a", 64)}
	b := &archive.NativeSourceBinding{Version: 2, Agent: "muse", NativeID: "ordinary-id", NativeCreatedAt: at, Cwd: "/work", Path: "/native/session.jsonl", RootRevision: "one", Checkpoint: &cp}
	digest, err := archive.NativeImportBindingDigest(b)
	if err != nil {
		t.Fatal(err)
	}
	receipt := &archive.NativeImport{Contract: "muse-historical-import-v1", Confirmation: archive.NativeImportConfirmation{BatchID: "2026-10-17-1", BatchInstance: strings.Repeat("b", 32), ScopeSHA256: strings.Repeat("c", 64), DestinationID: "destination", ProjectID: "project", RootFingerprint: strings.Repeat("d", 64), SelectedBindingSHA256: digest, SelectedSource: cp}, Membership: archive.NativeImportMembership{ReceiptID: strings.Repeat("e", 32), State: "attached", Effects: []string{"registration"}}}
	reg := archive.SessionRegistration{NativeImport: receipt, NativeBinding: b, ArchiveSessionID: strings.Repeat("1", 32), NativeSessionID: b.NativeID, ProjectID: "project", ProjectRoot: "/work", DestinationID: "destination", Harness: archive.Harness{Name: "muse"}, TranscriptPath: b.Path, SessionStartedAt: at, AdmittedAt: at.Add(time.Hour), RegisteredAt: at.Add(time.Hour), Origin: archive.SessionOriginImport, ImportBatch: archive.NewImportBatch(receipt.Confirmation.BatchID)}
	if err := reg.Validate(); err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestNativeImportNeverFallsBackToRawSourceOrGenerationRecovery(t *testing.T) {
	t.Parallel()
	reg := historicalNativeRegistration(t)
	opts := Options{Sources: deniedHistoricalSources{t: t}}
	if _, available := newSourceReader(reg, opts); available {
		t.Fatal("historical owner acquired live source reader")
	}
	if _, _, err := PrepareGenerationRecovery(t.Context(), reg, time.Now(), opts); err == nil {
		t.Fatal("historical owner acquired live generation recovery")
	}
	activity, err := LastActivitiesWithError(t.Context(), []archive.SessionRegistration{reg}, "", opts.Sources)
	if err != nil || !activity[reg.ArchiveSessionID].Equal(reg.Admitted()) {
		t.Fatal("historical activity reopened native source", err)
	}
	if _, err := ReadLocalBundle(t.Context(), t.TempDir(), reg, time.Now(), "", opts.Sources); err == nil {
		t.Fatal("missing stage and publication fell back to native bytes")
	}
	reg.NativeImport.Contract = "future"
	if _, err := LastActivitiesWithError(context.Background(), []archive.SessionRegistration{reg}, "", opts.Sources); !errors.Is(err, archive.ErrUnsupportedNativeImport) {
		t.Fatal("future authority was treated as local activity", err)
	}
}

func TestNativeImportPublicationContextRetainsImmutableReceiptAcrossUndo(t *testing.T) {
	t.Parallel()
	reg := historicalNativeRegistration(t)
	scan := sessionScan{reg: reg}
	original := scan.publicationAdmission()
	scan.reg.NativeImport.Membership.State = "detached"
	scan.reg.StagedImportSHA256 = ""
	if scan.publicationAdmission() != original {
		t.Fatal("undo/stage cleanup changed selecting publication lineage")
	}
	scan.reg.NativeImport.Membership.ReceiptID = strings.Repeat("f", 32)
	if scan.publicationAdmission() == original {
		t.Fatal("another creating receipt reused selecting publication lineage")
	}
}

func TestNativeImportPublicationContextKeepsOrdinaryBytesAndRejectsProofTamper(t *testing.T) {
	t.Parallel()
	reg := historicalNativeRegistration(t)
	scan := sessionScan{reg: reg}
	expected := scan.publicationAdmission()
	scan.reg.NativeImport.Confirmation.SelectedBindingSHA256 = strings.Repeat("9", 64)
	if scan.publicationAdmission() != "" || scan.publicationAdmission() == expected {
		t.Fatal("tampered original proof retained selecting authority")
	}
	scan.reg.NativeImport = nil
	legacy, err := json.Marshal(struct {
		Session   string                `json:"Session"`
		Native    string                `json:"Native"`
		Project   string                `json:"Project"`
		Admission string                `json:"Admission"`
		Origin    archive.SessionOrigin `json:"Origin"`
		Batch     archive.ImportBatch   `json:"Batch"`
	}{Session: scan.reg.ArchiveSessionID, Native: scan.reg.NativeSessionID, Project: scan.reg.ProjectID, Admission: scan.reg.Admitted().UTC().Format("2006-01-02T15:04:05.999999999Z07:00"), Origin: scan.reg.Origin, Batch: scan.reg.ImportBatch})
	if err != nil || scan.publicationAdmission() != storage.SHA256Hex(legacy) {
		t.Fatal("ordinary admission preimage changed", err)
	}
}
