package collector

import (
	"bytes"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func stagedUnadmittedGroupFixture(t *testing.T) (*state.Store, archive.SessionRegistration, archive.SessionRegistration, Options) {
	t.Helper()
	store := newTestStore(t)
	cfg, _, err := config.Load(store.Home())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Archive.Enabled = true
	cfg.DurableStorageProtection = true
	cfg.PublicationCompositionProtection = true
	cfg.ImportStagingProtection = true
	cfg.StagedSelectionProtection = true
	if err = config.Save(store.Home(), cfg); err != nil {
		t.Fatal(err)
	}
	cfg, _, err = config.Load(store.Home())
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	parentPath := writeTranscript(t, store.Home(), "parent.jsonl", `{"type":"assistant","sessionId":"parent-native","timestamp":"2026-01-01T00:01:00Z","message":{"role":"assistant","content":"parent"}}`+"\n")
	// Synthetic imported child has its own retained native ID, matching the
	// selected registration. Parent linkage remains separate explicit evidence.
	childPath := writeTranscript(t, store.Home(), "parent-native:subagent:agent-1.jsonl", `{"type":"assistant","sessionId":"parent-native:subagent:agent-1","agentId":"agent-1","timestamp":"2026-01-01T00:02:00Z","message":{"role":"assistant","content":"`+strings.Repeat("c", 96<<10)+`"}}`+"\n")
	parent := archive.SessionRegistration{ArchiveSessionID: "a-parent", NativeSessionID: "parent-native", ProjectID: "project", ProjectRoot: "/project", Harness: archive.Harness{Name: "claude"}, TranscriptPath: parentPath, SessionStartedAt: at.Add(-time.Hour), RegisteredAt: at, AdmittedAt: at, Origin: archive.SessionOriginImport, ImportBatch: archive.NewImportBatch("confirmed-group"), DestinationID: cfg.DestinationID()}
	child := parent
	child.ArchiveSessionID = "z-child"
	child.NativeSessionID = "parent-native:subagent:agent-1"
	child.ParentSessionID = parent.ArchiveSessionID
	child.ParentNativeSessionID = parent.NativeSessionID
	child.SubagentID = "agent-1"
	child.TranscriptPath = childPath
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "synthetic", Now: func() time.Time { return at }, AcceptSession: cfg.AcceptSession, SkillEvidence: cfg.EffectiveSkillEvidence(), Retry: storage.RetryPolicy{MaxAttempts: 1}}
	childBundle, preserved, _, err := ReadImportSourceSelection(t.Context(), child, at, opts)
	if err != nil || len(preserved) != 0 {
		t.Fatalf("child selection: %v", err)
	}
	childSHA, err := store.SaveImportStage(t.Context(), child, childBundle, cfg.EffectiveSkillEvidence())
	if err != nil {
		t.Fatal(err)
	}
	candidate := state.SubagentCandidate{ArchiveSessionID: child.ArchiveSessionID, NativeSessionID: child.NativeSessionID, ParentArchiveSessionID: parent.ArchiveSessionID, ParentNativeSessionID: parent.NativeSessionID, ProjectID: parent.ProjectID, ProjectRoot: parent.ProjectRoot, Harness: parent.Harness, AgentID: child.SubagentID, TranscriptPath: childPath, ObservedAt: at, Origin: archive.SessionOriginImport, StagedImportSHA256: childSHA}
	parentBundle, preserved, _, err := ReadImportSourceSelection(t.Context(), parent, at, opts)
	if err != nil || len(preserved) != 0 {
		t.Fatalf("parent selection: %v", err)
	}
	parentSHA, err := store.SaveImportStage(t.Context(), parent, parentBundle, cfg.EffectiveSkillEvidence(), candidate)
	if err != nil {
		t.Fatal(err)
	}
	parent.StagedImportSHA256 = parentSHA
	// Same durable minimal confirmation fields consumed by ResumeImportAdmissions.
	confirmation := struct {
		ID            string `json:"id"`
		DestinationID string `json:"destination_id"`
	}{"confirmed-group", cfg.DestinationID()}
	if err = local.Write(filepath.Join(store.Home(), "imports", "confirmed-group.json"), confirmation); err != nil {
		t.Fatal(err)
	}
	child.StagedImportSHA256 = childSHA
	return store, parent, child, opts
}

func TestStagedRunFreshGroupAdmissionRespectsCapturedPressure(t *testing.T) {
	store, parent, child, opts := stagedUnadmittedGroupFixture(t)
	// Existing unrelated authorities prove that refusal does not damage files,
	// while all fresh group's registration/request/candidate files stay absent.
	unrelated := parent
	unrelated.ArchiveSessionID = "unrelated"
	unrelated.NativeSessionID = "native-unrelated"
	unrelated.StagedImportSHA256 = ""
	unrelated.ImportBatch = archive.NewImportBatch("unrelated-batch")
	if err := store.SaveRegistration(unrelated); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRequest(unrelated.ArchiveSessionID, "backfill", parent.AdmittedAt); err != nil {
		t.Fatal(err)
	}
	priorCandidate := state.SubagentCandidate{ArchiveSessionID: "unrelated-child", NativeSessionID: "native-unrelated:subagent:aux", ParentArchiveSessionID: unrelated.ArchiveSessionID, ParentNativeSessionID: unrelated.NativeSessionID, ProjectID: unrelated.ProjectID, ProjectRoot: unrelated.ProjectRoot, Harness: unrelated.Harness, AgentID: "aux", TranscriptPath: child.TranscriptPath, ObservedAt: parent.AdmittedAt}
	if err := store.SaveSubagentCandidate(priorCandidate); err != nil {
		t.Fatal(err)
	}
	before := stagedGroupAuthorityFiles(t, store, parent, child)
	budget := agentapi.NewNativeReadBudget(128 << 20)
	pressure := budget.Reservation()
	if !pressure.Reserve((128 << 20) - 512) {
		t.Fatal("captured independent pressure")
	}
	defer pressure.Release((128 << 20) - 512)
	opts.CodexRollouts = recoveryBudgetLookup{budget: budget}
	opts.SkipSessionIndexRecovery = true
	guard, err := local.LockCollectorGuard(store.Home())
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Release()
	opts.CollectorGuard = guard
	result, err := Run(t.Context(), store, storagetest.NewMemoryStore(), opts)
	defer closeStagedControlResult(result)
	assertStagedGroupAuthorityFiles(t, store, parent, child, before)
	if !errors.Is(err, agentapi.ErrReadBudget) || !errors.Is(result.Errors["staged-imports"], agentapi.ErrReadBudget) {
		t.Fatalf("actual fresh admission escaped captured owner: %v %v", result.Errors, err)
	}
}

func TestStagedRunFreshGroupAdmissionWithSufficientCapturedOwner(t *testing.T) {
	t.Helper()
	store, parent, child, opts := stagedUnadmittedGroupFixture(t)
	opts.CodexRollouts = recoveryBudgetLookup{budget: agentapi.NewNativeReadBudget(128 << 20)}
	opts.SkipSessionIndexRecovery = true
	guard, err := local.LockCollectorGuard(store.Home())
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Release()
	opts.CollectorGuard = guard
	result, err := Run(t.Context(), store, storagetest.NewMemoryStore(), opts)
	defer closeStagedControlResult(result)
	if err != nil {
		t.Fatalf("actual fresh group sufficient owner: %v %v", result.Errors, err)
	}
	for _, want := range []archive.SessionRegistration{parent, child} {
		got, found, err := store.LoadRegistration(want.ArchiveSessionID)
		if err != nil || !found || got.StagedImportSHA256 != want.StagedImportSHA256 {
			t.Fatalf("actual group admission %s: %v %v", want.ArchiveSessionID, found, err)
		}
	}
}

func TestStagedMaterializationFitsOneValidatedStageView(t *testing.T) {
	for _, mode := range []string{"refuses-before-first-view", "persists-single-view-continuation"} {
		t.Run(mode, func(t *testing.T) {
			store, parent, child, opts := stagedUnadmittedGroupFixture(t)
			if err := store.SaveRegistration(parent); err != nil {
				t.Fatal(err)
			}
			stage, found, err := store.LoadImportStage(t.Context(), parent)
			if err != nil || !found || len(stage.Children) != 1 {
				t.Fatalf("actual prepared roster: %v", err)
			}
			candidate := stage.Children[0]
			if err = store.SaveSubagentCandidate(candidate); err != nil {
				t.Fatal(err)
			}
			before := stagedGroupAuthorityFiles(t, store, parent, child)
			info, err := os.Stat(filepath.Join(store.Home(), "sessions", child.ArchiveSessionID, "import-stage", "stage.json"))
			if err != nil {
				t.Fatal(err)
			}
			// The three charged stage wire views and typed digest scratch coexist.
			// A child payload larger than the scratch distinguishes a second full read.
			limit := 3*info.Size() + int64(32<<10)
			if mode == "refuses-before-first-view" {
				limit--
			}
			opts.CodexRollouts = recoveryBudgetLookup{budget: agentapi.NewNativeReadBudget(limit)}
			err = materializeSubagentCandidate(t.Context(), store, candidate, opts, parent.AdmittedAt)
			if mode == "refuses-before-first-view" {
				if !errors.Is(err, agentapi.ErrReadBudget) {
					t.Fatalf("first view pressure: %v", err)
				}
				assertStagedGroupAuthorityFiles(t, store, parent, child, before)
				return
			}
			if err != nil {
				t.Fatalf("single validated view did not persist continuation: %v", err)
			}
			admitted, found, err := store.LoadRegistration(child.ArchiveSessionID)
			if err != nil || !found || admitted.StagedImportSHA256 != child.StagedImportSHA256 {
				t.Fatalf("child registration: %v %v", found, err)
			}
			request, found, err := store.LoadRequest(child.ArchiveSessionID)
			if err != nil || !found || request.Token == "" {
				t.Fatalf("child continuation request: %v %v", found, err)
			}
			if _, found, err = store.LoadSubagentCandidate(child.ArchiveSessionID); err != nil || found {
				t.Fatalf("candidate acknowledgement: %v %v", found, err)
			}
			after := stagedGroupAuthorityFiles(t, store, parent, child)
			for _, path := range []string{filepath.Join(store.Home(), "config.json"), filepath.Join(store.Home(), "imports", "confirmed-group.json"), filepath.Join(store.Home(), "registrations", parent.ArchiveSessionID+".json"), filepath.Join(store.Home(), "sessions", parent.ArchiveSessionID, "import-stage", "stage.json"), filepath.Join(store.Home(), "sessions", child.ArchiveSessionID, "import-stage", "stage.json")} {
				if !bytes.Equal(before[path], after[path]) {
					t.Fatalf("materialization changed retained parent/source authority: %s", path)
				}
			}
		})
	}
}

func stagedGroupAuthorityFiles(t *testing.T, store *state.Store, parent, child archive.SessionRegistration) map[string][]byte {
	t.Helper()
	paths := []string{filepath.Join(store.Home(), "config.json"), filepath.Join(store.Home(), "imports", "confirmed-group.json"), filepath.Join(store.Home(), "import-stage-cursor.json")}
	for _, id := range []string{parent.ArchiveSessionID, child.ArchiveSessionID, "unrelated", "unrelated-child"} {
		for _, directory := range []string{"registrations", "requests", "subagent-candidates", "pending", "published"} {
			paths = append(paths, filepath.Join(store.Home(), directory, id+".json"))
		}
		paths = append(paths, filepath.Join(store.Home(), "sessions", id, "import-stage", "stage.json"))
	}
	out := make(map[string][]byte)
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			out[path] = nil
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		out[path] = raw
	}
	return out
}
func assertStagedGroupAuthorityFiles(t *testing.T, store *state.Store, parent, child archive.SessionRegistration, before map[string][]byte) {
	t.Helper()
	after := stagedGroupAuthorityFiles(t, store, parent, child)
	for path, raw := range before {
		if (raw == nil) != (after[path] == nil) || !bytes.Equal(raw, after[path]) {
			t.Fatalf("authority changed under pressure: %s", path)
		}
	}
}
