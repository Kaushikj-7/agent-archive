package capture

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/testutil/recoverytest"
)

// SubagentStop's agent_type is kept on the candidate when it is a short
// plain name, dropped when it is too long, holds anything else, or is not a
// string, and its absence changes nothing else.
func TestSubagentStopRecordsASanitizedAgentType(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		agentType any
		want      string
	}{
		{"builtin", "Explore", "Explore"},
		{"plugin", "my-plugin:reviewer", "my-plugin:reviewer"},
		{"absent", nil, ""},
		{"oversized", strings.Repeat("x", 65), ""},
		{"odd characters", "Explore; rm -rf /", ""},
		{"not a string", 42.0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			home, project := t.TempDir(), t.TempDir()
			at := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
			setUpTestConfig(t, home, project, at.Add(-time.Hour))
			parentPath := writeTestTranscript(t, "parent.jsonl", "")
			if err := HandleEvent(home, "claude", map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-parent", "cwd": project, "transcript_path": parentPath}, at, WithDecoders(testDecoders)); err != nil {
				t.Fatal(err)
			}
			stop := map[string]any{"hook_event_name": "SubagentStop", "session_id": "native-parent", "agent_id": "agent-1", "agent_transcript_path": "/never/written.jsonl"}
			if tc.agentType != nil {
				stop["agent_type"] = tc.agentType
			}
			if err := HandleEvent(home, "claude", stop, at.Add(time.Minute), WithDecoders(testDecoders)); err != nil {
				t.Fatal(err)
			}
			store, err := state.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			candidates, err := store.LoadSubagentCandidates()
			if err != nil || len(candidates) != 1 {
				t.Fatalf("candidates=%+v err=%v", candidates, err)
			}
			if got := candidates[0]; got.AgentType != tc.want || got.AgentID != "agent-1" || got.TranscriptPath != "/never/written.jsonl" {
				t.Fatalf("candidate=%+v, want type %q", got, tc.want)
			}
		})
	}
}

func TestCodexSubagentPathDoesNotEnableTranscriptCapture(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), "/synthetic/project"
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, project, at.Add(-time.Hour))
	if err := HandleEvent(home, "codex", map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "parent-native", "cwd": project, "transcript_path": "/synthetic/codex/parent.jsonl"}, at, WithDecoders(testDecoders)); err != nil {
		t.Fatal(err)
	}
	if err := HandleEvent(home, "codex", map[string]any{"hook_event_name": "SubagentStop", "session_id": "parent-native", "agent_id": "agent-1", "agent_transcript_path": "/synthetic/codex/child.jsonl"}, at.Add(time.Minute), WithDecoders(testDecoders)); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := store.LoadSubagentCandidates()
	if err != nil || len(candidates) != 0 {
		t.Fatalf("unsupported child candidates=%+v err=%v", candidates, err)
	}
	parentID, _, err := store.ArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "parent-native"})
	if err != nil {
		t.Fatal(err)
	}
	request, found, err := store.LoadRequest(parentID)
	if err != nil || !found {
		t.Fatalf("parent request found=%t err=%v", found, err)
	}
	for _, evidence := range request.HookEvidence {
		if evidence.Kind == archive.EvidenceKindLinkedSession {
			t.Fatal("unverified Codex stop synthesized a child archive identity")
		}
	}
	if _, found, err := store.ArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "parent-native:subagent:agent-1"}); err != nil || found {
		t.Fatalf("composite native identity allocated: found=%v err=%v", found, err)
	}
}

// While discovery waits for a Codex session's census proof, a Claude Code
// subagent finishing and a new Claude Code session are both still captured;
// neither is refused for collector recovery.
func TestClaudeHooksCaptureWhileDiscoveryAwaitsAbsenceProof(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	at := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, project, at.Add(-time.Hour))
	parentPath := writeTestTranscript(t, "parent.jsonl", "")
	if err := HandleEvent(home, "claude", map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-parent", "cwd": project, "transcript_path": parentPath}, at, WithDecoders(testDecoders)); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := recoverytest.Exhaust(context.Background(), store, state.SessionIndexRecoverySlice, false); err != nil {
		t.Fatal(err)
	}
	if err := store.RequestSessionIndexAbsence(agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "discovered-codex"}); err != nil {
		t.Fatal(err)
	}
	stop := map[string]any{"hook_event_name": "SubagentStop", "session_id": "native-parent", "agent_id": "agent-1", "agent_transcript_path": "/never/written.jsonl"}
	if err := HandleEvent(home, "claude", stop, at.Add(time.Minute), WithDecoders(testDecoders)); err != nil {
		t.Fatalf("subagent refused while discovery waits: %v", err)
	}
	if candidates, err := store.LoadSubagentCandidates(); err != nil || len(candidates) != 1 {
		t.Fatalf("candidates=%+v err=%v", candidates, err)
	}
	newPath := writeTestTranscript(t, "new.jsonl", "")
	if err := HandleEvent(home, "claude", map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-new", "cwd": project, "transcript_path": newPath}, at.Add(2*time.Minute), WithDecoders(testDecoders)); err != nil {
		t.Fatalf("new session refused while discovery waits: %v", err)
	}
	if _, found, err := store.ArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "native-new"}); err != nil || !found {
		t.Fatalf("new session not registered: %v %v", found, err)
	}
	if diagnostics, err := ReadDiagnostics(home); err != nil || len(diagnostics) != 0 {
		t.Fatalf("diagnostics=%+v err=%v", diagnostics, err)
	}
}

// A resumed subagent stops again while its child identity is still held for
// the next slice's absence proof. Its parent is already owned, so the refusal
// must leave the complete census alone: only the child waits, and every other
// key's miss is still admitted.
func TestResumedSubagentStopKeepsCompleteCensusForOwnedParent(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	at := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, project, at.Add(-time.Hour))
	parentPath := writeTestTranscript(t, "parent.jsonl", "")
	if err := HandleEvent(home, "claude", map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-parent", "cwd": project, "transcript_path": parentPath}, at, WithDecoders(testDecoders)); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := recoverytest.Exhaust(context.Background(), store, state.SessionIndexRecoverySlice, false); err != nil {
		t.Fatal(err)
	}
	child := agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "native-parent:subagent:agent-1"}
	if err := store.RequestSessionIndexAbsence(child); err != nil {
		t.Fatal(err)
	}
	stop := map[string]any{"hook_event_name": "SubagentStop", "session_id": "native-parent", "agent_id": "agent-1", "agent_transcript_path": "/never/written.jsonl"}
	if err := HandleEvent(home, "claude", stop, at.Add(time.Minute), WithDecoders(testDecoders)); err != nil && !errors.Is(err, state.ErrSessionIndexRecoveryRequired) {
		t.Fatal(err)
	}
	if status, err := store.SessionIndexRecoveryStatus(); err != nil || !status.Complete || status.Pending {
		t.Fatalf("census after resumed subagent stop: %+v err=%v", status, err)
	}
	if _, created, err := store.EnsureArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "unrelated"}); err != nil || !created {
		t.Fatalf("unrelated miss: created=%v err=%v", created, err)
	}
}
