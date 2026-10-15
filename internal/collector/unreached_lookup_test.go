package collector

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// A Codex session left unopened because the lookup allowance is spent counts
// as ahead of the sessions the pass's stop leaves later, so status's "N ahead"
// numbers every unreached session of a pass in one sequence.
func TestSkippedCodexSessionsCountAheadOfLaterUnreachedOnes(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	dir := t.TempDir()
	c1 := registration(t, writeTranscript(t, dir, "codex.jsonl", codexTranscript))
	c2 := c1
	c2.ArchiveSessionID = "codex-2"
	c2.NativeSessionID = "codex-native-2"
	c2.SessionStartedAt = c1.SessionStartedAt.Add(time.Second)
	for _, r := range []archive.SessionRegistration{c1, c2} {
		if err := local.SaveRegistration(r); err != nil {
			t.Fatal(err)
		}
		if err := local.SaveRequest(r.ArchiveSessionID, "stop", r.RegisteredAt); err != nil {
			t.Fatal(err)
		}
	}
	claudePath := filepath.Join(dir, "claude.jsonl")
	if err := os.WriteFile(claudePath, []byte(`{"type":"assistant","sessionId":"claude-native","timestamp":"2026-01-01T00:00:02Z","message":{"role":"assistant","content":"visible"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cl := archive.SessionRegistration{ArchiveSessionID: "session-2", NativeSessionID: "claude-native", ProjectID: "project-1", ProjectRoot: "/p", Harness: archive.Harness{Name: "claude"}, TranscriptPath: claudePath, SessionStartedAt: c1.SessionStartedAt.Add(time.Minute), RegisteredAt: c1.RegisteredAt.Add(time.Minute)}
	if err := local.SaveRegistration(cl); err != nil {
		t.Fatal(err)
	}
	if err := local.SaveRequest(cl.ArchiveSessionID, "stop", cl.RegisteredAt); err != nil {
		t.Fatal(err)
	}
	stops := 0
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	result, err := Run(t.Context(), local, storagetest.NewMemoryStore(), Options{Sources: testSources, Parsers: testParsers, MachineID: "m", CodexRollouts: &spentAllowanceLookup{}, Now: func() time.Time { return now }, Stop: func() bool { stops++; return stops >= 3 }})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Unreached) != 2 || result.Unreached[c1.ArchiveSessionID] != 0 || result.Unreached[c2.ArchiveSessionID] != 1 {
		t.Fatalf("unreached=%v, want %s:0 %s:1", result.Unreached, c1.ArchiveSessionID, c2.ArchiveSessionID)
	}
}
