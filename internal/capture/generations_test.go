package capture

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/state/statetest"
)

func TestGenerationChildrenKeepRecordedParent(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, project, at.Add(-time.Hour))
	if err := HandleEvent(home, "claude", claudeStart(project, "parent", "startup", "/synthetic/parent.jsonl"), at, WithDecoders(testDecoders)); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	parentID, _, err := store.ArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	parent, _, _ := store.LoadRegistration(parentID)
	stop := func(child string, at time.Time) {
		t.Helper()
		payload := map[string]any{"hook_event_name": "SubagentStop", "session_id": "parent", "cwd": project, "agent_id": child, "agent_transcript_path": "/synthetic/" + child + ".jsonl"}
		if err := HandleEvent(home, "claude", payload, at, WithDecoders(testDecoders)); err != nil {
			t.Fatal(err)
		}
	}
	stop("queued", at.Add(time.Minute))
	stop("registered", at.Add(time.Minute))
	candidates, err := store.LoadSubagentCandidates()
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range candidates {
		if candidate.AgentID == "registered" {
			child := parent
			child.ArchiveSessionID = candidate.ArchiveSessionID
			child.NativeSessionID = candidate.NativeSessionID
			child.ParentSessionID = parentID
			child.ParentNativeSessionID = parent.NativeSessionID
			child.SubagentID = candidate.AgentID
			child.TranscriptPath = candidate.TranscriptPath
			if err := store.SaveRegistration(child); err != nil {
				t.Fatal(err)
			}
			if err := store.RemoveSubagentCandidate(child.ArchiveSessionID); err != nil {
				t.Fatal(err)
			}
		}
	}
	bundle := archive.SourceBundle{SchemaVersion: archive.SourceSchemaVersion, ArchiveSessionID: parentID, NativeSessionID: parent.NativeSessionID, ProjectID: parent.ProjectID, Capture: archive.SourceCapture{Harness: parent.Harness, AdapterName: "test", CapturedAt: at}}
	source, err := archive.BuildCompressedSource(bundle)
	if err != nil {
		t.Fatal(err)
	}
	sourceKey, err := archive.SourceObjectKey(bundle, source.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	metadata := archive.Metadata{SchemaVersion: archive.MetadataSchemaVersion, SessionID: parentID, NativeSessionID: parent.NativeSessionID, ProjectID: parent.ProjectID, Harness: parent.Harness, CapturedAt: at, SourceBundle: archive.SourceReference{Key: sourceKey, SHA256: source.SHA256, CompressedBytes: len(source.Bytes)}}
	raw, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := statetest.SavePublished(store, parentID, bundle, at, state.CacheStatusPublished, raw); err != nil {
		t.Fatal(err)
	}
	if err := statetest.SaveBlocked(store, parentID, bundle, at, state.BlockedReasonTranscriptRewritten); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.GenerationProtection = true
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	recoveredAt := at.Add(time.Hour)
	next, err := store.BeginGenerationRecovery(parentID, recoveredAt, func(reg archive.SessionRegistration, id string) (archive.SessionRegistration, state.PendingPublication, error) {
		reg.ArchiveSessionID = id
		reg.PreviousGenerationID = parentID
		b := bundle
		b.ArchiveSessionID = id
		b.PreviousGenerationID = parentID
		b.Capture.CapturedAt = recoveredAt
		b.Capture.AdapterName = "test"
		source, err := archive.BuildCompressedSource(b)
		if err != nil {
			return reg, state.PendingPublication{}, err
		}
		sourceKey, err := archive.SourceObjectKey(b, source.SHA256)
		if err != nil {
			return reg, state.PendingPublication{}, err
		}
		metadataKey, err := archive.MetadataObjectKey(reg.Harness.Name, id)
		if err != nil {
			return reg, state.PendingPublication{}, err
		}
		pending := state.PendingPublication{Bundle: b, ReadyAt: recoveredAt, SourceKey: sourceKey, MetadataKey: metadataKey, SourceSHA256: source.SHA256, SourceBytes: source.Bytes}
		metadata := archive.Metadata{SchemaVersion: archive.MetadataSchemaVersion, SessionID: id, PreviousGenerationID: parentID, NativeSessionID: reg.NativeSessionID, ProjectID: reg.ProjectID, Harness: reg.Harness, CapturedAt: recoveredAt, SourceBundle: pending.SourceReference()}
		pending.MetadataBytes, err = json.Marshal(metadata)
		return reg, pending, err
	})
	if err != nil {
		t.Fatal(err)
	}
	stop("queued", recoveredAt.Add(time.Minute))
	stop("registered", recoveredAt.Add(time.Minute))
	stop("new", recoveredAt.Add(time.Minute))
	candidates, err = store.LoadSubagentCandidates()
	if err != nil || len(candidates) != 3 {
		t.Fatalf("candidates %#v %v", candidates, err)
	}
	for _, candidate := range candidates {
		want := parentID
		if candidate.AgentID == "new" {
			want = next
		}
		if candidate.ParentArchiveSessionID != want {
			t.Fatalf("child %s changed parent: %s want %s", candidate.AgentID, candidate.ParentArchiveSessionID, want)
		}
	}
}
