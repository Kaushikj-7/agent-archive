package builtin

import (
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/testutil/agenttest"
	"testing"
	"time"
)

func TestHistoricalImportConformance(t *testing.T) {
	t.Parallel()
	r := NewBuiltins()
	nativeStart := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	headerStart := nativeStart.Add(-time.Minute)
	for _, name := range r.DiscoveryAgents() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			provider, ok := r.LookupImport(name)
			if !ok {
				t.Fatal("missing inspector")
			}
			request := agentapi.ImportInspectionRequest{Session: agentapi.NativeSession{Agent: agentmeta.ID(name), NativeID: "qualified"}, Header: agentapi.NativeHeader{StartedAt: headerStart}, Filtered: archive.FilteredTranscript{Records: [][]byte{[]byte(`{"type":"user"}`)}, SessionIDs: []string{"copied", "qualified"}, NativeStartAt: nativeStart}}
			expected := agentapi.ImportInspection{Conversation: true}
			switch agentmeta.ID(name) {
			case agentmeta.Claude:
				expected.StartedAt = nativeStart
			case agentmeta.Cursor:
				// Native file creation supplies Cursor start evidence before filtering.
			case agentmeta.Codex:
				expected.StartedAt = headerStart
			case agentmeta.Muse:
				t.Fatal("runtime-only Muse cannot inspect historical imports")
			}
			cases := []agenttest.ImportCase{{Name: "copied identity remains valid", Request: request, Want: expected}}
			missing := request
			missing.Filtered.SessionIDs = []string{"another"}
			mismatch := expected
			if name == "claude" {
				mismatch.IdentityMismatch = true
			}
			cases = append(cases, agenttest.ImportCase{Name: "native identity policy", Request: missing, Want: mismatch})
			agenttest.HistoricalImport(t, provider, cases)
			policy := provider.ImportPolicy(agentapi.SourceRef{Path: "/synthetic/native"})
			if (policy.Start == agentapi.ImportFileCreatedStart) != (name == "cursor") {
				t.Fatalf("filter start policy: %+v", policy)
			}
		})
	}
}
