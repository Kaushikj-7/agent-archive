package collector

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type stagedChildFailureStore struct {
	*storagetest.MemoryStore
	childMetadata string
	failChild     bool
	sourceReads   map[string]int
}

func (s *stagedChildFailureStore) Put(ctx context.Context, key string, data []byte) error {
	if s.failChild && key == s.childMetadata {
		return errors.New("controlled child metadata failure")
	}
	return s.MemoryStore.Put(ctx, key, data)
}

func (s *stagedChildFailureStore) GetLimited(ctx context.Context, key string, limit int64) ([]byte, error) {
	if s.sourceReads == nil {
		s.sourceReads = make(map[string]int)
	}
	s.sourceReads[key]++
	return s.MemoryStore.GetLimited(ctx, key, limit)
}

// Real Run, imported candidate persistence and immutable staged transcripts
// exercise parent-first recovery. The batch record is the exact durable
// confirmation seam; no candidate alone substitutes for a child request.
func TestStagedParentFirstRestartContinuesChildAfterNativeDeletion(t *testing.T) {
	stagedParentFirstRestartControl(t, false)
}
func TestStagedGroupUsesOnePrecommitReadbackTriplet(t *testing.T) {
	stagedParentFirstRestartControl(t, true)
}
func stagedParentFirstRestartControl(t *testing.T, assertReadbacks bool) {
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
	// Real-shaped Claude child records name the parent session and child agent.
	// Use the dedicated child producer, as actual backfill staging does.
	childPath := writeTranscript(t, store.Home(), "agent-agent-1.jsonl", `{"type":"assistant","sessionId":"parent-native","agentId":"agent-1","timestamp":"2026-01-01T00:02:00Z","message":{"role":"assistant","content":"child"}}`+"\n")
	// Actual import planning reserves native-key mappings before preparation.
	parentID, _, err := store.EnsureArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "parent-native"})
	if err != nil {
		t.Fatal(err)
	}
	childID, _, err := store.EnsureArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "parent-native:subagent:agent-1"})
	if err != nil {
		t.Fatal(err)
	}
	parent := archive.SessionRegistration{ArchiveSessionID: parentID, NativeSessionID: "parent-native", ProjectID: "project", ProjectRoot: "/project", Harness: archive.Harness{Name: "claude"}, TranscriptPath: parentPath, SessionStartedAt: at.Add(-time.Hour), RegisteredAt: at, AdmittedAt: at, Origin: archive.SessionOriginImport, ImportBatch: archive.NewImportBatch("confirmed-group"), DestinationID: cfg.DestinationID()}
	child := parent
	child.ArchiveSessionID = childID
	child.NativeSessionID = "parent-native:subagent:agent-1"
	child.ParentSessionID = parent.ArchiveSessionID
	child.ParentNativeSessionID = parent.NativeSessionID
	child.SubagentID = "agent-1"
	child.TranscriptPath = childPath
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "synthetic", Now: func() time.Time { return at }, AcceptSession: cfg.AcceptSession, SkillEvidence: cfg.EffectiveSkillEvidence(), Retry: storage.RetryPolicy{MaxAttempts: 1}}
	candidate := state.SubagentCandidate{ArchiveSessionID: child.ArchiveSessionID, NativeSessionID: child.NativeSessionID, ParentArchiveSessionID: parent.ArchiveSessionID, ParentNativeSessionID: parent.NativeSessionID, ProjectID: parent.ProjectID, ProjectRoot: parent.ProjectRoot, Harness: parent.Harness, AgentID: child.SubagentID, TranscriptPath: childPath, ObservedAt: at, Origin: archive.SessionOriginImport}
	child, childBundle, err := ReadImportSubagentBundle(t.Context(), parent, candidate, opts)
	if err != nil {
		t.Fatalf("dedicated child selection: %v", err)
	}
	childSHA, err := store.SaveImportStage(t.Context(), child, childBundle, cfg.EffectiveSkillEvidence())
	if err != nil {
		t.Fatal(err)
	}
	candidate.StagedImportSHA256 = childSHA
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
	if err = os.Remove(parentPath); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(childPath); err != nil {
		t.Fatal(err)
	}
	guard, err := local.LockCollectorGuard(store.Home())
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Release()
	opts.CollectorGuard = guard
	key, err := archive.MetadataObjectKey("claude", child.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	remote := &stagedChildFailureStore{MemoryStore: storagetest.NewMemoryStore(), childMetadata: key, failChild: true}
	firstResult, runErr := Run(t.Context(), store, remote, opts)
	closeStagedControlResult(firstResult)
	if runErr != nil {
		t.Fatal(runErr)
	}
	request, found, err := store.LoadRequest(child.ArchiveSessionID)
	if err != nil || !found || request.Token == "" {
		t.Fatalf("child continuation request: %v %v", found, err)
	}
	candidates, issues, err := store.ScanSubagentCandidates()
	if err != nil || len(issues) != 0 || len(candidates) != 0 {
		t.Fatalf("acknowledged candidate: %v %v %v", candidates, issues, err)
	}
	// A failed child publication keeps exact selected evidence for restart.
	reg, found, err := store.LoadRegistration(child.ArchiveSessionID)
	if err != nil || !found {
		t.Fatalf("child admission: %v", err)
	}
	if _, found, err = store.LoadImportStage(t.Context(), reg); err != nil || !found {
		t.Fatalf("child stage lost: %v", err)
	}
	remote.failChild = false
	reopened, err := state.Open(store.Home())
	if err != nil {
		t.Fatal(err)
	}
	secondResult, runErr := Run(t.Context(), reopened, remote, opts)
	closeStagedControlResult(secondResult)
	if runErr != nil {
		t.Fatal(runErr)
	}
	published, err := store.LoadPublishedState(child.ArchiveSessionID)
	if err != nil || !published.TypedStagedPublished() {
		t.Fatalf("child not published: %v", err)
	}
	if _, found, err = store.LoadRequest(child.ArchiveSessionID); err != nil || found {
		t.Fatalf("child request after release: %v %v", found, err)
	}
	if _, found, err = store.LoadImportStage(t.Context(), reg); err != nil || found {
		t.Fatalf("child stage after release: %v %v", found, err)
	}
	if assertReadbacks {
		for _, bundle := range []archive.SourceBundle{parentBundle, childBundle} {
			compressed, err := archive.BuildCompressedSource(bundle)
			if err != nil {
				t.Fatal(err)
			}
			key, err := archive.SourceObjectKey(bundle, compressed.SHA256)
			if err != nil {
				t.Fatal(err)
			}
			// One upload verification plus the private release factory's mandatory
			// verification, each metadata/source/metadata. No duplicate outer triplet.
			if remote.sourceReads[key] != 2 {
				t.Fatalf("source readbacks: got %d want 2", remote.sourceReads[key])
			}
		}
	}
}
