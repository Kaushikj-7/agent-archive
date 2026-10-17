package muse

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

func FuzzMuseStartupFacts(f *testing.F) {
	for _, name := range []string{"synthetic-exec-empty.jsonl", "synthetic-msp-empty.jsonl", "synthetic-model-resolution-omissions.jsonl", "synthetic-route-startup.jsonl"} {
		raw, e := os.ReadFile(filepath.Join("testdata", name))
		if e != nil {
			f.Fatal(e)
		}
		f.Add(raw)
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 1<<20 {
			t.Skip()
		}
		path := filepath.Join(t.TempDir(), "startup.jsonl")
		if e := os.WriteFile(path, raw, 0600); e != nil {
			t.Fatal(e)
		}
		budget := agentapi.NewNativeReadBudget(8 << 20)
		pass, e := (SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{ReadBudget: budget})
		if e != nil {
			t.Fatal(e)
		}
		snap, e := pass.Read(t.Context(), agentapi.SourceRef{Path: path}, agentapi.ReadLimits{RawBytes: 1 << 20, RecordBytes: 1 << 20})
		if e == nil {
			facts, factErr := snap.(*sourceSnapshot).startupFacts(t.Context())
			if factErr == nil && (facts.NativeID == "" || facts.StreamID != facts.NativeID || facts.NativeCreatedAt.IsZero() || facts.RootRevision != "" || facts.Checkpoint != nil) {
				t.Fatal("startup fabricated or lost facts")
			}
			if e = snap.Close(); e != nil {
				t.Fatal(e)
			}
		}
		if e = pass.Close(); e != nil {
			t.Fatal(e)
		}
		if used, _ := budget.Charged(); used != 0 {
			t.Fatal("startup leaked private allocation", used)
		}
	})
}
