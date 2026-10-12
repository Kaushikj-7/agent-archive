package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

// Saving a publication larger than the session's share of the pass ledger
// is refused at the session's own ceiling, so the collector records it as
// the session's gap rather than shared pressure retried every pass.
func TestSaveOverSessionCeilingMarksTheScope(t *testing.T) {
	t.Parallel()
	base, err := openTestStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	budget := agentapi.NewNativeReadBudget(agentapi.PassReadBudget)
	store, closeStore := base.WithReadBudget(t.Context(), budget)
	defer closeStore()
	end := budget.BeginScope(1 << 20)
	defer end()
	source := bytes.Repeat([]byte("s"), 2<<20)
	err = store.SavePending("session-1", PendingPublication{ReadyAt: time.Unix(1, 0), SourceKey: "source", MetadataKey: "metadata", SourceSHA256: "sha", SourceBytes: source, MetadataBytes: []byte(`{}`)})
	if !errors.Is(err, agentapi.ErrReadBudget) || !budget.ScopeExceeded() || budget.ScopePressured() {
		t.Fatalf("save over the ceiling: err=%v exceeded=%v", err, budget.ScopeExceeded())
	}
}

// The size-limit kinds and the signature fields are written to disk and read
// by later versions: their spellings are pinned.
func TestSizeLimitGapSpellings(t *testing.T) {
	t.Parallel()
	for kind, want := range map[SizeLimitKind]string{SizeLimitRaw: "raw", SizeLimitFiltered: "filtered", SizeLimitRecord: "record", SizeLimitReadBudget: "read_budget"} {
		if string(kind) != want {
			t.Errorf("%q, want %q", kind, want)
		}
	}
	encoded, err := json.Marshal(ScanSignature{FailedRawLimit: 7, SizeLimit: &SizeLimitGap{Kind: SizeLimitRaw, LimitBytes: 5, SizeBytes: 7}})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	limit, _ := fields["size_limit"].(map[string]any)
	if fields["failed_raw_limit"] != float64(7) || limit["kind"] != "raw" || limit["limit_bytes"] != float64(5) || limit["size_bytes"] != float64(7) {
		t.Fatalf("%s", encoded)
	}
}
