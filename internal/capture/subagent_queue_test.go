package capture

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/testutil/recoverytest"
)

func subagentStopPayload(project string) map[string]any {
	return map[string]any{"hook_event_name": "SubagentStop", "session_id": "native-parent", "cwd": project, "agent_id": "agent-1", "agent_type": "Explore", "agent_transcript_path": "/never/written.jsonl"}
}

// A subagent finishing while collector identity recovery is genuinely
// pending (damage, a packed census restart) cannot reserve its child
// identity yet. Its content-free observation waits in the admission queue
// and is applied once recovery completes, instead of being lost.
func TestSubagentStopDuringPendingRecoveryIsReplayed(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	at := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, project, at.Add(-time.Hour))
	parentPath := writeTestTranscript(t, "parent.jsonl", "")
	if err := HandleEvent(home, "claude", claudeStart(project, "native-parent", "startup", parentPath), at, WithDecoders(testDecoders)); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkSessionIndexRecoveryNeeded(); err != nil {
		t.Fatal(err)
	}
	stop := subagentStopPayload(project)
	if err := HandleEvent(home, "claude", stop, at.Add(time.Minute), WithDecoders(testDecoders)); !errors.Is(err, state.ErrSessionIndexRecoveryRequired) {
		t.Fatalf("pending recovery: %v", err)
	}
	if got := len(queueFiles(t, home)); got != 1 {
		t.Fatalf("queued %d intents", got)
	}
	// Replay before recovery completes keeps the observation queued.
	if err := ReplayAdmissionIntents(home, at.Add(2*time.Minute), testDecoders); !errors.Is(err, state.ErrSessionIndexRecoveryRequired) {
		t.Fatalf("early replay: %v", err)
	}
	if got := len(queueFiles(t, home)); got != 1 {
		t.Fatalf("early replay left %d intents", got)
	}
	if err := recoverytest.Exhaust(context.Background(), store, state.SessionIndexRecoverySlice, false); err != nil {
		t.Fatal(err)
	}
	if err := ReplayAdmissionIntents(home, at.Add(3*time.Minute), testDecoders); err != nil {
		t.Fatal(err)
	}
	candidates, err := store.LoadSubagentCandidates()
	if err != nil || len(candidates) != 1 || candidates[0].NativeSessionID != "native-parent:subagent:agent-1" || candidates[0].TranscriptPath != "/never/written.jsonl" || candidates[0].AgentType != "Explore" {
		t.Fatalf("candidates=%+v err=%v", candidates, err)
	}
	if got := len(queueFiles(t, home)); got != 0 {
		t.Fatalf("replayed intent kept: %d", got)
	}
	// A repeated native SubagentStop is idempotent with the replayed one.
	if err := HandleEvent(home, "claude", stop, at.Add(4*time.Minute), WithDecoders(testDecoders)); err != nil {
		t.Fatal(err)
	}
	if candidates, err := store.LoadSubagentCandidates(); err != nil || len(candidates) != 1 {
		t.Fatalf("duplicate child: %+v %v", candidates, err)
	}
}

// When the parent's own start was also queued during recovery, the child
// observation waits for that start rather than being dropped as orphaned.
func TestQueuedSubagentWaitsForQueuedParentStart(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	at := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, project, at.Add(-time.Hour))
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkSessionIndexRecoveryNeeded(); err != nil {
		t.Fatal(err)
	}
	parentPath := writeTestTranscript(t, "parent.jsonl", "")
	if err := HandleEvent(home, "claude", claudeStart(project, "native-parent", "startup", parentPath), at, WithDecoders(testDecoders)); !errors.Is(err, state.ErrSessionIndexRecoveryRequired) {
		t.Fatalf("parent start: %v", err)
	}
	if err := HandleEvent(home, "claude", subagentStopPayload(project), at.Add(time.Minute), WithDecoders(testDecoders)); !errors.Is(err, state.ErrSessionIndexRecoveryRequired) {
		t.Fatalf("child: %v", err)
	}
	queued := queueFiles(t, home)
	if len(queued) != 2 {
		t.Fatalf("queued %d intents", len(queued))
	}
	if err := recoverytest.Exhaust(context.Background(), store, state.SessionIndexRecoverySlice, false); err != nil {
		t.Fatal(err)
	}
	// While the parent's start has not replayed (here: set aside), the child
	// observation stays queued instead of being acknowledged as orphaned.
	start := filepath.Join(admissionIntentDir(home), queued[0])
	aside := filepath.Join(t.TempDir(), queued[0])
	if err := os.Rename(start, aside); err != nil {
		t.Fatal(err)
	}
	if err := ReplayAdmissionIntents(home, at.Add(2*time.Minute), testDecoders); err != nil {
		t.Fatal(err)
	}
	if got := queueFiles(t, home); len(got) != 1 || got[0] != queued[1] {
		t.Fatalf("child observation dropped before its parent: %v", got)
	}
	if err := os.Rename(aside, start); err != nil {
		t.Fatal(err)
	}
	if err := ReplayAdmissionIntents(home, at.Add(2*time.Minute), testDecoders); err != nil {
		t.Fatal(err)
	}
	parentID, found, err := store.ArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "native-parent"})
	if err != nil || !found {
		t.Fatalf("parent: %v %v", found, err)
	}
	candidates, err := store.LoadSubagentCandidates()
	if err != nil || len(candidates) != 1 || candidates[0].ParentArchiveSessionID != parentID {
		t.Fatalf("candidates=%+v err=%v", candidates, err)
	}
	if got := len(queueFiles(t, home)); got != 0 {
		t.Fatalf("intents left: %d", got)
	}
}

// A SubagentStop that finds hooks.lock busy is queued like a start, rather
// than reported as not recorded.
func TestBusySubagentStopIsQueued(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	at := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, project, at.Add(-time.Hour))
	parentPath := writeTestTranscript(t, "parent.jsonl", "")
	if err := HandleEvent(home, "claude", claudeStart(project, "native-parent", "startup", parentPath), at, WithDecoders(testDecoders)); err != nil {
		t.Fatal(err)
	}
	busy := func(string, time.Duration) (func(), error) { return nil, local.ErrBusy }
	if err := handleEvent(home, "claude", subagentStopPayload(project), at.Add(time.Minute), busy, nil); err != nil {
		t.Fatalf("busy child: %v", err)
	}
	if err := ReplayAdmissionIntents(home, at.Add(2*time.Minute), testDecoders); err != nil {
		t.Fatal(err)
	}
	store := state.OpenReadOnly(home)
	if candidates, err := store.LoadSubagentCandidates(); err != nil || len(candidates) != 1 {
		t.Fatalf("candidates=%+v err=%v", candidates, err)
	}
}
