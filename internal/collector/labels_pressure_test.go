package collector

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/codex"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// Integrity verification must refuse pressure before encoding retained bytes.
// Valid deferred debt publishes after relief; uncommitted bytes remain refused.
func TestExternalRenameIntegrityCompressionRespectsSharedPressure(t *testing.T) {
	t.Parallel()
	for _, corrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "acknowledged source", true: "uncommitted retained bytes"}[corrupt], func(t *testing.T) {
			t.Parallel()
			local := newTestStore(t)
			reg := registration(t, writeTranscript(t, t.TempDir(), "session.jsonl", codexTranscript))
			reg.NativeSessionID = "01900000-0000-7000-8000-000000000001"
			if err := local.SaveRegistration(reg); err != nil {
				t.Fatal(err)
			}
			remote := &countedPublications{MemoryStore: storagetest.NewMemoryStore()}
			now := reg.RegisteredAt.Add(time.Hour)
			provider := &mutableLabels{}
			opts := Options{Sources: testSources, Parsers: testParsers, Labels: mutableLabelLookup{provider}, MachineID: "machine", Now: func() time.Time { return now }}
			if result, err := Run(t.Context(), local, remote, opts); err != nil || len(result.Errors) != 0 {
				t.Fatal(result, err)
			}
			before := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
			if corrupt {
				file := readPublishedStateFile(t, local, reg.ArchiveSessionID)
				file.Bundle.NativeRecords = append(file.Bundle.NativeRecords, map[string]any{"type": "event_msg", "payload": map[string]any{"type": "user_message", "message": "Uncommitted retained bytes"}})
				writePublishedStateFile(t, local, reg.ArchiveSessionID, file)
			}
			if err := os.Remove(reg.TranscriptPath); err != nil {
				t.Fatal(err)
			}
			provider.label = archive.SessionLabel{State: archive.SessionLabelPresent, Name: "Deferred rename", Source: archive.SessionLabelDatabase, Contract: codex.LabelContract}
			budget := agentapi.NewNativeReadBudget(64 << 10)
			// Complete coverage proves the transcript deleted, not renamed.
			opts.CodexRollouts = recoveryBudgetLookup{CodexRolloutLookup: &reconciliationLookup{set: agentapi.CodexRolloutSet{Complete: true}}, budget: budget}
			now = now.Add(time.Hour)
			remote.keys = nil
			result, err := Run(t.Context(), local, remote, opts)
			// Pressure from a pass ledger smaller than the session's own share
			// defers the session to a later pass; it neither publishes nor fails.
			if err != nil || result.Deferred[reg.ArchiveSessionID] != DeferredReadBudget || len(result.Errors) != 0 || len(result.Published) != 0 || len(remote.keys) != 0 {
				t.Fatalf("name verification did not refuse shared compression pressure: result=%+v err=%v writes=%v", result, err, remote.keys)
			}
			if budget.Available() != 64<<10 {
				t.Fatal("pressure refusal leaked ownership", budget.Available())
			}
			cache, err := local.LoadLabels()
			if err != nil {
				t.Fatal(err)
			}
			if cache.Entries[reg.ArchiveSessionID].Label.Name != provider.label.Name {
				t.Fatal("safe deferred rename debt lost")
			}
			after := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
			if after.SourceBundle != before.SourceBundle || after.Name != before.Name || !after.CapturedAt.Equal(before.CapturedAt) {
				t.Fatal("pressure changed acknowledged source, name or capture")
			}
			if _, found, err := local.LoadPending(reg.ArchiveSessionID); err != nil || found {
				t.Fatal("pressure created pending publication", found, err)
			}
			opts.CodexRollouts = nil
			now = now.Add(30 * time.Second)
			calls := provider.calls
			result, err = Run(t.Context(), local, remote, opts)
			if corrupt {
				failure := result.Errors[reg.ArchiveSessionID]
				if err != nil || failure == nil || !strings.Contains(failure.Error(), "retained source cannot be verified for session name refresh") || len(result.Published) != 0 || len(remote.keys) != 0 {
					t.Fatal("relief certified uncommitted bytes", result, err)
				}
				after = fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
				if after.SourceBundle != before.SourceBundle || after.Name != before.Name || !after.CapturedAt.Equal(before.CapturedAt) {
					t.Fatal("integrity refusal changed acknowledged source, name or capture")
				}
			} else {
				if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
					t.Fatal("relief did not publish deferred rename", result, err)
				}
				after = fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
				if after.Name != provider.label.Name || after.SourceBundle == before.SourceBundle || !after.CapturedAt.Equal(before.CapturedAt) {
					t.Fatal("relief changed capture or lost name")
				}
			}
			if provider.calls != calls {
				t.Fatal("cached debt reread native label during backoff")
			}
		})
	}
}
