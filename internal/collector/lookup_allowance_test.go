package collector

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// spentAllowanceLookup is a Codex lookup whose collector allowance is used up
// as soon as it begins: every call is refused as the real lookup refuses
// calls past its allowance, and each one is counted.
type spentAllowanceLookup struct {
	begun int
	calls int
}

func (l *spentAllowanceLookup) Rollout(context.Context, string) ([]agentapi.SourceRef, error) {
	l.calls++
	return nil, lookupAllowanceSpent()
}

func (l *spentAllowanceLookup) Thread(context.Context, string) (agentapi.CodexRolloutSet, error) {
	l.calls++
	return agentapi.CodexRolloutSet{}, lookupAllowanceSpent()
}

func (l *spentAllowanceLookup) Check(context.Context, string, string) error {
	l.calls++
	return lookupAllowanceSpent()
}

func (l *spentAllowanceLookup) BeginCollection() { l.begun++ }

func (l *spentAllowanceLookup) AllowanceSpent() bool { return true }

// A pass whose Codex lookup allowance is spent leaves its Codex sessions for
// the next pass without opening them: none fails, and the pass's other
// sessions are captured as usual.
func TestSpentCodexLookupAllowanceLeavesCodexSessionsWithoutFailing(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	dir := t.TempDir()
	codexReg := registration(t, writeTranscript(t, dir, "codex.jsonl", codexTranscript))
	if err := local.SaveRegistration(codexReg); err != nil {
		t.Fatal(err)
	}
	if err := local.SaveRequest(codexReg.ArchiveSessionID, "stop", codexReg.RegisteredAt); err != nil {
		t.Fatal(err)
	}
	claudePath := filepath.Join(dir, "claude.jsonl")
	if err := os.WriteFile(claudePath, []byte(`{"type":"assistant","sessionId":"claude-native","timestamp":"2026-01-01T00:00:02Z","message":{"role":"assistant","content":"visible"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Started after the Codex session, so the pass reaches it second.
	claudeReg := archive.SessionRegistration{ArchiveSessionID: "session-2", NativeSessionID: "claude-native", ProjectID: "project-1", ProjectRoot: "/p", Harness: archive.Harness{Name: "claude"}, TranscriptPath: claudePath, SessionStartedAt: codexReg.SessionStartedAt.Add(time.Minute), RegisteredAt: codexReg.RegisteredAt.Add(time.Minute)}
	if err := local.SaveRegistration(claudeReg); err != nil {
		t.Fatal(err)
	}
	lookup := &spentAllowanceLookup{}
	store := storagetest.NewMemoryStore()
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	result, err := Run(t.Context(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", CodexRollouts: lookup, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if lookup.begun != 1 {
		t.Fatalf("collector began %d lookup allowances, want 1", lookup.begun)
	}
	if lookup.calls != 0 {
		t.Fatalf("the pass made %d lookup calls past a spent allowance", lookup.calls)
	}
	if len(result.Errors) != 0 || len(result.Deferred) != 0 {
		t.Fatalf("a spent lookup allowance failed or deferred sessions: errors=%v deferred=%v", result.Errors, result.Deferred)
	}
	if len(result.Published) != 1 || result.Published[0] != claudeReg.ArchiveSessionID {
		t.Fatalf("published=%v, want only the Claude Code session", result.Published)
	}
	status, err := local.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.PendingCount != 1 || len(status.LastErrors) != 0 || len(status.SessionIssues) != 0 {
		t.Fatalf("status=%+v, want the Codex session pending with no problem", status)
	}

	// The next pass, with an allowance, captures the Codex session.
	result, err = Run(t.Context(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return now.Add(time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Published) != 1 || result.Published[0] != codexReg.ArchiveSessionID || len(result.Errors) != 0 {
		t.Fatalf("next pass result=%+v, want the Codex session published", result)
	}
}
