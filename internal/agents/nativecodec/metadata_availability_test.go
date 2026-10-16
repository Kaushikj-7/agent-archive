package nativecodec

import (
	"encoding/json"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestMetadataToolAvailabilityPreservesNativeEvidence(t *testing.T) {
	t.Parallel()
	schema := evalExportSchema(t)
	for _, harness := range []string{"claude", "codex", "cursor"} {
		t.Run(harness, func(t *testing.T) {
			t.Parallel()
			b, m := evalExportFixture(t, harness)
			a, err := Parse(t.Context(), b)
			if err != nil {
				t.Fatal(err)
			}
			if m.Counts.ToolCalls == nil || *m.Counts.ToolCalls != len(a.View.ToolCalls) || m.Counts.FilesTouched == nil || m.Counts.Commits == nil {
				t.Fatalf("lost observable calls/files/Git: %+v", m.Counts)
			}
			if a.Observability.ToolResults.Available() {
				if m.Counts.ToolResults == nil || *m.Counts.ToolResults != len(a.View.ToolResults) {
					t.Fatalf("observable results: %v", m.Counts.ToolResults)
				}
			} else if m.Counts.ToolResults != nil {
				t.Fatalf("unavailable results measured: %v", m.Counts.ToolResults)
			}
			e, err := BuildEvalExport(b, m, EvalExportSourceArchive, EvalExportDetailFull)
			if err != nil {
				t.Fatal(err)
			}
			if e.FilesEdited == nil || len(*e.FilesEdited) != *m.Counts.FilesTouched {
				t.Fatal("observable file presence changed")
			}
			raw, err := json.Marshal(e)
			if err != nil {
				t.Fatal(err)
			}
			validateAgainst(t, schema, harness, raw)
			if harness == "cursor" {
				old := m
				old.Parser.Version = "0.20.0"
				old.Counts.ToolResults = new(0)
				refreshed, err := archive.BuildEvalExportWithAnalysis(b, a, old, EvalExportSourceArchive, EvalExportDetailFull, ParserInfo{Name: "cursor", Version: "0.21.0"})
				if err != nil {
					t.Fatal(err)
				}
				if refreshed.Counts.ToolResults != nil || refreshed.Counts.ToolCalls == nil || *refreshed.Counts.ToolCalls != len(a.View.ToolCalls) || refreshed.Parser.Version != "0.21.0" {
					t.Fatal("Cursor stale sidecar did not refresh result availability")
				}
			}
			// Empty structured sources still establish measured zeros, including Cursor.
			b.NativeRecords = nil
			a, err = Parse(t.Context(), b)
			if err != nil {
				t.Fatal(err)
			}
			e, err = archive.BuildEvalExportWithAnalysis(b, a, m, EvalExportSourceArchive, EvalExportDetailFull, ParserInfo{Version: "synthetic-refresh"})
			if err != nil {
				t.Fatal(err)
			}
			for name, count := range map[string]*int{"calls": e.Counts.ToolCalls, "results": e.Counts.ToolResults, "files": e.Counts.FilesTouched, "commits": e.Counts.Commits} {
				if count == nil || *count != 0 {
					t.Errorf("empty structured %s count: %v", name, count)
				}
			}
			if e.FilesEdited == nil || len(*e.FilesEdited) != 0 {
				t.Fatal("measured empty files lost")
			}
		})
	}
}
