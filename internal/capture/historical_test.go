package capture

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
)

func TestHistoricalImportNeverAcceptsHookContinuationOrChildren(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	at := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, "/work/widget", at.Add(-time.Hour))
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	const projectID = "11111111111111111111111111111111"
	cfg.HistoricalProjects = []config.HistoricalProject{{ID: projectID, Label: "Retired", Permissions: []config.HistoricalProjectPermission{{DestinationID: cfg.DestinationID(), Enabled: true}}}}
	cfg.HistoricalProjectProtection = true
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := store.EnsureArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "native-1"})
	if err != nil {
		t.Fatal(err)
	}
	reg := archive.SessionRegistration{ArchiveSessionID: id, NativeSessionID: "native-1", ProjectID: projectID, Harness: archive.Harness{Name: "claude"}, TranscriptPath: "/old/transcript.jsonl", SessionStartedAt: at.Add(-time.Hour), RegisteredAt: at, AdmittedAt: at, Origin: archive.SessionOriginImport, ImportBatch: archive.NewImportBatch("2026-10-12-1"), DestinationID: cfg.DestinationID(), StagedImportSHA256: strings.Repeat("a", 64), HistoricalProject: &archive.HistoricalProjectProof{Version: 1, ProjectID: projectID, DestinationID: cfg.DestinationID(), OriginalCwd: "/gone", CanonicalCwd: "/gone", Label: "Retired"}}
	if err := store.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "Stop", "SubagentStop"} {
		payload := map[string]any{"hook_event_name": event, "source": "resume", "session_id": "native-1", "cwd": "/work/widget", "transcript_path": "/current/native.jsonl", "agent_id": "new-child", "agent_transcript_path": "/current/child.jsonl"}
		if err := HandleEvent(home, "claude", payload, at.Add(time.Hour), WithDecoders(testDecoders), WithRepoKey(func(string) string { t.Fatal("historical hook queried Git"); return "" }), WithGitHead(func(string, bool) (string, *bool) { t.Fatal("historical hook queried HEAD"); return "", nil })); err != nil {
			t.Fatal(err)
		}
	}
	current, found, err := store.LoadRegistration(id)
	if err != nil || !found || !reflect.DeepEqual(current, reg) {
		t.Fatal("hook modified historical admission", current, err)
	}
	if _, found, err := store.LoadRequest(id); err != nil || found {
		t.Fatal("hook queued historical native capture", err)
	}
	candidates, err := store.LoadSubagentCandidates()
	if err != nil || len(candidates) > 0 {
		t.Fatal("historical parent granted new child capture", err)
	}
}
