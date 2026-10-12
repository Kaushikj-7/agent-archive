package codex

import (
	"errors"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/codexmeta"
)

// TestPassCachesAreNoSessionsAndRelease: cached headers and ancestor
// prefixes outlive one session's read, so no session's scope counts them,
// and ReleaseCaches returns them before a session that needs the room.
func TestPassCachesAreNoSessionsAndRelease(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base, raw := historyFile(t, dir, threadA, threadA, 0, nil, strings.Repeat("safe text ", 2000))
	leaf, _ := historyFile(t, dir, threadB, threadB, 2, map[string]any{"forked_from_id": threadA, "forked_from_ordinal_exclusive": 2, "history_base": codexmeta.CodexHistoryPosition{RolloutID: threadA, EndOrdinal: 2, EndByteOffset: uint64(len(raw))}}, "own")
	lookup := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &leaf}, rollouts: map[string][]agentapi.SourceRef{threadA: {base}}}
	pass := historyPass(t, dir, lookup).(*relatedSourcePass)
	ledger := pass.env.ReadBudget
	end := ledger.BeginScope(1 << 20)
	snap, err := pass.Read(t.Context(), leaf, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if err := snap.Close(); err != nil {
		t.Fatal(err)
	}
	if pass.files[base.Path].prefix == nil {
		t.Fatal("prefix was not cached")
	}
	cached, _ := ledger.Charged()
	if cached < int64(len(raw))+2*sourceHeaderCharge || ledger.Available() != 1<<20 {
		t.Fatalf("pass caches counted toward the session: cached=%d available=%d", cached, ledger.Available())
	}
	end()
	if err := pass.ReleaseCaches(); err != nil {
		t.Fatal(err)
	}
	if used, _ := ledger.Charged(); used != 0 || len(pass.files) != 0 {
		t.Fatalf("caches kept %d bytes in %d files", used, len(pass.files))
	}
}

// TestRefusedHeaderIsSharedPressure: a rollout's header, though charged as
// a pass cache, is required for the read; refused because others hold the
// ledger, it marks the session's scope pressured, so the collector defers
// the session instead of failing it every pass.
func TestRefusedHeaderIsSharedPressure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ref, _ := historyFile(t, dir, threadA, threadA, 0, nil, "text")
	ledger := agentapi.NewNativeReadBudget(passLedger)
	held := ledger.Available() - (64 << 10)
	if !ledger.ReserveUnscoped(held) {
		t.Fatal("reserve")
	}
	end := ledger.BeginScope(sessionCeiling)
	defer end()
	pass, err := (SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{ReadBudget: ledger})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pass.Close() }()
	if _, err := pass.Read(t.Context(), ref, agentapi.ReadLimits{}); !errors.Is(err, agentapi.ErrReadBudget) || !ledger.ScopePressured() || ledger.ScopeExceeded() {
		t.Fatalf("refused header: err=%v pressured=%v", err, ledger.ScopePressured())
	}
	ledger.ReleaseUnscoped(held)
}

// TestChargedLineReaderFailureIsFinal: an oversized record ends the
// records; no later record is returned past it.
func TestChargedLineReaderFailureIsFinal(t *testing.T) {
	t.Parallel()
	pass := &relatedSourcePass{env: agentapi.SourceEnvironment{ReadBudget: agentapi.NewNativeReadBudget(1 << 20)}}
	input := strings.Repeat("x", 100) + "\nshort\n"
	lines, err := pass.newChargedLineReader(strings.NewReader(input), 50)
	if err != nil {
		t.Fatal(err)
	}
	defer lines.close()
	for range 2 {
		if _, more, err := lines.next(t.Context()); more || !errors.Is(err, archive.ErrRecordTooLarge) {
			t.Fatalf("after an oversized record: more=%v err=%v", more, err)
		}
	}
}
