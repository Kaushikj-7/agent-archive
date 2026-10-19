package collector

import (
	"os"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
)

func TestImportStageReadPreservesSelectedCaptureThroughBuildAndCompare(t *testing.T) {
	for _, child := range []bool{false, true} {
		t.Run(map[bool]string{false: "parent", true: "child"}[child], func(t *testing.T) {
			store, reg, opts, capturedAt := legacyCaptureStage(t, child)
			if capturedAt.Equal(reg.AdmittedAt) {
				t.Fatal("capture/admission clock control was not distinct")
			}
			if err := os.Remove(reg.TranscriptPath); err != nil {
				t.Fatal(err)
			}
			assertStageReadCapture(t, store, reg, opts, capturedAt)
		})
	}
}

func legacyCaptureStage(t *testing.T, child bool) (*state.Store, archive.SessionRegistration, Options, time.Time) {
	t.Helper()
	store := newTestStore(t)
	cfg, _, err := config.Load(store.Home())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Archive.Enabled, cfg.DurableStorageProtection, cfg.PublicationCompositionProtection, cfg.ImportStagingProtection = true, true, true, true
	if err := config.Save(store.Home(), cfg); err != nil {
		t.Fatal(err)
	}
	admittedAt := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	capturedAt := admittedAt.Add(-10 * time.Minute)
	path := writeTranscript(t, store.Home(), "parent.jsonl", `{"type":"assistant","sessionId":"parent-native","timestamp":"2026-01-01T00:01:00Z","message":{"role":"assistant","content":"parent"}}`+"\n")
	parent := archive.SessionRegistration{ArchiveSessionID: "synthetic-parent", NativeSessionID: "parent-native", ProjectID: "project", ProjectRoot: "/project", Harness: archive.Harness{Name: "claude"}, TranscriptPath: path, SessionStartedAt: admittedAt.Add(-time.Hour), RegisteredAt: admittedAt, AdmittedAt: admittedAt, Origin: archive.SessionOriginImport, ImportBatch: archive.NewImportBatch("synthetic-capture-batch"), DestinationID: cfg.DestinationID()}
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "synthetic", SkillEvidence: cfg.EffectiveSkillEvidence()}
	reg := parent
	var bundle archive.SourceBundle
	if child {
		path = writeTranscript(t, store.Home(), "agent-agent-1.jsonl", `{"type":"assistant","sessionId":"parent-native","agentId":"agent-1","timestamp":"2026-01-01T00:02:00Z","message":{"role":"assistant","content":"child"}}`+"\n")
		candidate := state.SubagentCandidate{ArchiveSessionID: "synthetic-child", NativeSessionID: "parent-native:subagent:agent-1", ParentArchiveSessionID: parent.ArchiveSessionID, ParentNativeSessionID: parent.NativeSessionID, ProjectID: parent.ProjectID, ProjectRoot: parent.ProjectRoot, Harness: parent.Harness, AgentID: "agent-1", TranscriptPath: path, ObservedAt: capturedAt, Origin: archive.SessionOriginImport}
		reg, bundle, err = ReadImportSubagentBundle(t.Context(), parent, candidate, opts)
	} else {
		bundle, _, _, err = ReadImportSourceSelection(t.Context(), parent, capturedAt, opts)
	}
	if err != nil {
		t.Fatal(err)
	}
	reg.StagedImportSHA256, err = store.SaveImportStage(t.Context(), reg, bundle, cfg.EffectiveSkillEvidence())
	if err != nil {
		t.Fatal(err)
	}
	stage, found, err := store.LoadImportStage(t.Context(), reg)
	if err != nil || !found || stage.TypedSelection {
		t.Fatal("ordinary selected stage control unavailable", found, err)
	}
	return store, reg, opts, capturedAt
}

func assertStageReadCapture(t *testing.T, store *state.Store, reg archive.SessionRegistration, opts Options, capturedAt time.Time) {
	t.Helper()
	readStore, closeRead := store.WithReadBudget(t.Context(), agentapi.NewNativeReadBudget(128<<20))
	defer closeRead()
	published, err := readStore.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	scan := newSessionScan(t.Context(), readStore, nil, reg, state.Request{}, published, reg.AdmittedAt.Add(24*time.Hour), opts)
	defer scan.releaseRetained()
	read, ok, err := scan.readImportStage()
	if err != nil || !ok || read.stagedCapturedAt != capturedAt {
		t.Fatal("actual validated retained read lost selected capture instant", ok, err)
	}
	candidate, _, err := scan.build(read)
	if err != nil {
		t.Fatal(err)
	}
	settled, err := scan.compare(read, &candidate)
	if err != nil || settled || candidate.Capture.CapturedAt != capturedAt {
		t.Fatal("first retained build/compare substituted another clock", settled, err)
	}
}
