package backfill

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// A source below the ordinary capture ceiling could previously be admitted
// with a journal too large for the scheduled recovery reader's decode budget.
func TestImportStagingSizeBoundPrecedesAdmission(t *testing.T) {
	// Sequential: these fixtures intentionally approach the shared memory limit.
	for _, tc := range []struct {
		name  string
		rows  int
		admit bool
	}{
		{name: "near limit survives native deletion", rows: 520, admit: true},
		{name: "oversized remains unadmitted", rows: 800},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, c, cfg := stagingFixture(t)
			file, err := os.OpenFile(c.TranscriptPath, os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			encoder := json.NewEncoder(file)
			for range tc.rows {
				if err := encoder.Encode(map[string]any{"type": "user", "sessionId": c.NativeSessionID, "message": map[string]any{"role": "user", "content": strings.Repeat("synthetic transcript content ", 2200)}}); err != nil {
					t.Fatal(err)
				}
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			result, err := r.Run([]Candidate{c})
			if !tc.admit {
				if err == nil || len(result.Sessions) != 0 {
					t.Fatalf("oversized source admitted: %+v %v", result, err)
				}
				regs, err := r.Store.LoadRegistrations()
				if err != nil || len(regs) != 0 {
					t.Fatalf("oversized source acquired durable ownership: %+v %v", regs, err)
				}
				stages, _, err := r.Store.ImportStages(t.Context(), "", 50)
				if err != nil || len(stages) != 0 {
					t.Fatalf("oversized preparation survived: %+v %v", stages, err)
				}
				if _, err := os.Stat(filepath.Join(r.Home, "import-stage-cursor.json")); !os.IsNotExist(err) {
					t.Fatalf("rejected source changed recovery progress: %v", err)
				}
				return
			}
			if err != nil || len(result.Sessions) != 1 {
				t.Fatalf("near-bound source was not admitted: %+v %v", result, err)
			}
			info, err := os.Stat(filepath.Join(r.Home, "sessions", result.Sessions[0], "import-stage", "stage.json"))
			if err != nil || info.Size() < 31<<20 || info.Size() > state.MaxImportStageBytes {
				t.Fatalf("fixture did not exercise the journal boundary: %v %v", info, err)
			}
			if err := os.Remove(c.TranscriptPath); err != nil {
				t.Fatal(err)
			}
			remote := storagetest.NewMemoryStore()
			got, err := collector.Run(t.Context(), r.Store, remote, collector.Options{Sources: testSources, Parsers: testSources, MachineID: "synthetic", Now: func() time.Time { return fixedNow.Add(time.Minute) }, AcceptSession: cfg.AcceptSession})
			if err != nil || len(got.Errors) > 0 || len(got.Published) != 1 {
				t.Fatalf("admitted evidence cannot recover: %+v %v", got, err)
			}
		})
	}
}

func TestImportStagingAccountsForEncodedTextAndRefilterScratch(t *testing.T) {
	// Sequential: these fixtures intentionally approach the shared memory limit.
	for _, tc := range []struct {
		name    string
		content string
		rows    int
	}{
		{name: "encoded expansion", content: strings.Repeat("<", 12000), rows: 500},
		{name: "refilter scratch", content: strings.Repeat("synthetic ", 6000), rows: 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, c, _ := stagingFixture(t)
			c.Harness = "cursor"
			c.TranscriptPath = filepath.Join(c.ProjectRoot, "selected.txt")
			if err := os.WriteFile(c.TranscriptPath, []byte(strings.Repeat("user: "+tc.content+"\n", tc.rows)), 0600); err != nil {
				t.Fatal(err)
			}
			result, err := r.Run([]Candidate{c})
			if !errors.Is(err, state.ErrDurableStorageCapacity) || len(result.Sessions) != 0 {
				t.Fatalf("unreadable text acquired ownership: %+v %v", result, err)
			}
			regs, err := r.Store.LoadRegistrations()
			if err != nil || len(regs) != 0 {
				t.Fatalf("unreadable text admitted: %+v %v", regs, err)
			}
			stages, _, err := r.Store.ImportStages(t.Context(), "", 50)
			if err != nil || len(stages) != 0 {
				t.Fatalf("unreadable text left preparation: %+v %v", stages, err)
			}
		})
	}
}
