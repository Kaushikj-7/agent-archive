package capture

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/wangjohn/agent-archive/internal/testutil/recoverytest"
)

func corruptQualifiedIndex(t *testing.T, home string, key agentmeta.SessionKey) {
	t.Helper()
	sum := sha256.Sum256(key.Encoding())
	if err := os.WriteFile(filepath.Join(home, "sessions-v1", hex.EncodeToString(sum[:])+".json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyChildHookBeforeRecoveryKeepsCandidateIdentity(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, project, at.Add(-time.Hour))
	if err := HandleEvent(home, "claude", claudeStart(project, "parent", "startup", "/synthetic/parent.jsonl"), at, WithDecoders(testDecoders)); err != nil {
		t.Fatal(err)
	}
	store := state.OpenReadOnly(home)
	parentID, _, err := store.ArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	child := agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "parent:subagent:child"}
	const childID = "legacy-child"
	sum := sha256.Sum256([]byte(child.NativeID))
	if err := local.Write(filepath.Join(home, "sessions", hex.EncodeToString(sum[:])+".json"), map[string]string{"archive_session_id": childID}); err != nil {
		t.Fatal(err)
	}
	candidate := state.SubagentCandidate{ArchiveSessionID: childID, NativeSessionID: child.NativeID, ParentArchiveSessionID: parentID, ParentNativeSessionID: "parent", ProjectID: archive.ProjectID(project), ProjectRoot: project, Harness: archive.Harness{Name: "claude-code"}, AgentID: "child", TranscriptPath: "/synthetic/child.jsonl", ObservedAt: at}
	if err := store.SaveSubagentCandidate(candidate); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{"hook_event_name": "SubagentStop", "session_id": "parent", "cwd": project, "agent_id": "child", "agent_transcript_path": candidate.TranscriptPath}
	if err := HandleEvent(home, "claude", payload, at.Add(time.Minute), WithDecoders(testDecoders)); !errors.Is(err, state.ErrSessionIndexRecoveryRequired) {
		t.Fatalf("legacy child hook: %v", err)
	}
	if err := recoverytest.Exhaust(context.Background(), store, state.SessionIndexRecoverySlice, true); err != nil {
		t.Fatal(err)
	}
	if err := HandleEvent(home, "claude", payload, at.Add(2*time.Minute), WithDecoders(testDecoders)); err != nil {
		t.Fatal(err)
	}
	candidates, err := store.LoadSubagentCandidates()
	if err != nil || len(candidates) != 1 || candidates[0].ArchiveSessionID != childID {
		t.Fatalf("legacy child duplicated: %#v %v", candidates, err)
	}
}

func TestCorruptIdentityPreservesDeferredProofAndGeneration(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		t.Run(map[bool]string{false: "recover original proof", true: "pause generation revoked"}[revoke], func(t *testing.T) {
			home, project := t.TempDir(), t.TempDir()
			at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
			setUpTestConfig(t, home, project, at.Add(-time.Hour))
			store, err := state.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			key := agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: " exact 界 / ID "}
			corruptQualifiedIndex(t, home, key)
			payload := claudeStart(project, key.NativeID, "startup", "/synthetic/original.jsonl")
			if err := HandleEvent(home, "CLAUDE-CODE", payload, at, WithDecoders(testDecoders)); !errors.Is(err, state.ErrSessionIndexRecoveryRequired) {
				t.Fatalf("bounded recovery error %v", err)
			}
			entries, err := os.ReadDir(admissionIntentDir(home))
			if err != nil || len(entries) != 1 {
				t.Fatalf("intent lost %#v %v", entries, err)
			}
			if regs, err := store.LoadRegistrations(); err != nil || len(regs) != 0 {
				t.Fatalf("corrupt lookup admitted %#v %v", regs, err)
			}
			if err := ReplayAdmissionIntents(home, at.Add(time.Minute), testDecoders); !errors.Is(err, state.ErrSessionIndexRecoveryRequired) {
				t.Fatalf("incomplete replay %v", err)
			}
			if revoke {
				if _, err := config.SetPaused(home, true); err != nil {
					t.Fatal(err)
				}
				if _, err := config.SetPaused(home, false); err != nil {
					t.Fatal(err)
				}
			}
			if err := recoverytest.Exhaust(context.Background(), store, state.SessionIndexRecoverySlice, true); err != nil {
				t.Fatal(err)
			}
			if err := ReplayAdmissionIntents(home, at.Add(2*time.Minute), testDecoders); err != nil {
				t.Fatal(err)
			}
			regs, err := store.LoadRegistrations()
			if err != nil {
				t.Fatal(err)
			}
			if revoke {
				if len(regs) != 0 {
					t.Fatal("old generation admitted")
				}
				return
			}
			if len(regs) != 1 || regs[0].NativeSessionID != key.NativeID || !regs[0].SessionStartedAt.Equal(at) || !regs[0].AdmittedAt.Equal(at) {
				t.Fatalf("proof/bytes lost %#v", regs)
			}
		})
	}
}

func TestCorruptContinuationDoesNotCreateRegistrationOrStartIntent(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, project, at.Add(-time.Hour))
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	key := agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "resumed"}
	corruptQualifiedIndex(t, home, key)
	if err := HandleEvent(home, "claude", claudeStart(project, key.NativeID, "resume", "/synthetic/resumed.jsonl"), at, WithDecoders(testDecoders)); !errors.Is(err, state.ErrSessionIndexRecoveryRequired) {
		t.Fatalf("continuation %v", err)
	}
	if err := recoverytest.Exhaust(context.Background(), store, state.SessionIndexRecoverySlice, true); err != nil {
		t.Fatal(err)
	}
	if err := ReplayAdmissionIntents(home, at.Add(time.Minute), testDecoders); err != nil {
		t.Fatal(err)
	}
	regs, err := store.LoadRegistrations()
	if err != nil || len(regs) != 0 {
		t.Fatalf("continuation admitted %#v %v", regs, err)
	}
}

func TestQualifiedSubagentParentCollision(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, project, at.Add(-time.Hour))
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, agent := range []string{"claude", "codex"} {
		if err := HandleEvent(home, agent, claudeStart(project, "parent", "startup", "/synthetic/"+agent+".jsonl"), at, WithDecoders(testDecoders)); err != nil {
			t.Fatal(err)
		}
	}
	claudeID, _, _ := store.ArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "parent"})
	codexID, _, _ := store.ArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "parent"})
	if claudeID == "" || claudeID == codexID {
		t.Fatal("parents collided")
	}
	payload := map[string]any{"hook_event_name": "SubagentStop", "session_id": "parent", "cwd": project, "agent_id": "child", "agent_transcript_path": "/synthetic/child.jsonl"}
	if err := HandleEvent(home, "claude-code", payload, at.Add(time.Minute), WithDecoders(testDecoders)); err != nil {
		t.Fatal(err)
	}
	candidates, err := store.LoadSubagentCandidates()
	if err != nil || len(candidates) != 1 || candidates[0].ParentArchiveSessionID != claudeID || candidates[0].Harness.Name != "claude" {
		t.Fatalf("parent ownership %#v %v", candidates, err)
	}
	if _, found, err := store.LoadRequest(codexID); err != nil || !found { // starts produce evidence; child link must not.
		t.Fatalf("codex start evidence %#v %v", found, err)
	}
	request, _, err := store.LoadRequest(codexID)
	if err != nil {
		t.Fatal(err)
	}
	for _, evidence := range request.HookEvidence {
		if evidence.Kind == archive.EvidenceKindLinkedSession {
			t.Fatal("child linked wrong parent")
		}
	}
}

// A failed child lookup must request that child's recovery, rather than only
// the healthy parent named in the incoming hook. Regression: phase 3a P3A-R3.
func TestCorruptChildIdentityRequestsExactKeyRecovery(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, project, at.Add(-time.Hour))
	if err := HandleEvent(home, "claude", claudeStart(project, "parent", "startup", "/synthetic/parent.jsonl"), at, WithDecoders(testDecoders)); err != nil {
		t.Fatal(err)
	}
	store := state.OpenReadOnly(home)
	child := agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: "parent:subagent:child"}
	corruptQualifiedIndex(t, home, child)
	payload := map[string]any{"hook_event_name": "SubagentStop", "session_id": "parent", "cwd": project, "agent_id": "child", "agent_transcript_path": "/synthetic/child.jsonl"}
	for range 2 {
		if err := HandleEvent(home, "claude", payload, at.Add(time.Minute), WithDecoders(testDecoders)); !errors.Is(err, state.ErrSessionIndexRecoveryRequired) {
			t.Fatalf("lookup: %v", err)
		}
		// The child observation waits in the admission queue meanwhile.
		if err := ReplayAdmissionIntents(home, at.Add(time.Minute), testDecoders); !errors.Is(err, state.ErrSessionIndexRecoveryRequired) {
			t.Fatal(err)
		}
	}
	if regs, err := store.LoadRegistrations(); err != nil || len(regs) != 1 {
		t.Fatalf("unproved child admitted: %#v %v", regs, err)
	}
	if err := recoverytest.Exhaust(context.Background(), store, state.SessionIndexRecoverySlice, true); err != nil {
		t.Fatalf("child recovery stranded: %v", err)
	}
	if err := ReplayAdmissionIntents(home, at.Add(2*time.Minute), testDecoders); err != nil {
		t.Fatal(err)
	}
	if regs, err := store.LoadRegistrations(); err != nil || len(regs) != 1 {
		t.Fatalf("unproved child admitted: %#v %v", regs, err)
	}
	if err := HandleEvent(home, "claude", payload, at.Add(2*time.Minute), WithDecoders(testDecoders)); err != nil {
		t.Fatal(err)
	}
	candidates, err := store.LoadSubagentCandidates()
	if err != nil || len(candidates) != 1 || candidates[0].NativeSessionID != child.NativeID {
		t.Fatalf("child retry: %#v %v", candidates, err)
	}
}
