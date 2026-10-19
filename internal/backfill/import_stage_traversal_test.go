package backfill

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/trace"
)

// The trace recorder is process-wide, so these synthetic consumers run sequentially.
// Each reported count belongs to the actual individual name acquisition.
func TestImportFullConsumersAcquireStageNamesOnce(t *testing.T) {
	for _, count := range []int{5000, 50000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			r, candidate, _ := stagingFixture(t)
			r.AfterStage = func(string) error { return errors.New("synthetic interruption") }
			if _, err := r.Run([]Candidate{candidate}); err == nil {
				t.Fatal("missing interrupted stage")
			}
			initial, err := os.ReadDir(filepath.Join(r.Home, "sessions"))
			if err != nil {
				t.Fatal(err)
			}
			expectedEntries := count + len(initial)
			for i := range count {
				if err := os.MkdirAll(filepath.Join(r.Home, "sessions", fmt.Sprintf("000-%05d", i)), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Remove(candidate.TranscriptPath); err != nil {
				t.Fatal(err)
			}
			budget := agentapi.NewNativeReadBudget(128 << 20)
			store, closeStore := r.Store.WithReadBudget(t.Context(), budget)
			defer closeStore()
			check := func(name string, run func() error) {
				t.Helper()
				disableTrace := trace.Enable()
				defer disableTrace()
				started := time.Now()
				runErr := run()
				elapsed := time.Since(started)
				var recording strings.Builder
				trace.Write(&recording)
				disableTrace()
				if runErr != nil {
					t.Fatal(name, runErr, recording.String())
				}
				var acquisitions, reads, entries int
				for line := range strings.SplitSeq(recording.String(), "\n") {
					if !strings.Contains(line, "import stage names") {
						continue
					}
					acquisitions++
					_, detail, found := strings.Cut(line, "  · ")
					if !found || strings.Contains(line, "×") || strings.Contains(line, "unfinished") {
						t.Fatal("repeated or unfinished name acquisition", line)
					}
					if _, err := fmt.Sscanf(detail, "directory reads %d, acquired entries %d", &reads, &entries); err != nil {
						t.Fatal("missing acquisition work counts", line, err)
					}
				}
				used, peak := budget.Charged()
				t.Logf("traced physical sparse sessions=%d consumer=%s elapsed=%s directory_reads=%d acquired_entries=%d charged_current=%d charged_peak=%d", count, name, elapsed, reads, entries, used, peak)
				if acquisitions != 1 || entries != expectedEntries || reads <= 0 || reads > (expectedEntries+127)/128+1 {
					t.Fatalf("%s replayed stage-name acquisition: acquisitions=%d reads=%d entries=%d expected=%d", name, acquisitions, reads, entries, expectedEntries)
				}
			}
			check("ResumeAll", func() error { return store.ResumeAllImportAdmissions(t.Context()) })
			regs, err := store.LoadRegistrations()
			if err != nil || len(regs) != 1 {
				t.Fatal("late staged registration lost", len(regs), err)
			}
			batch := Batch{ID: r.Batch, DestinationID: r.DestinationID}
			check("Reconcile", func() error { return batch.Reconcile(store) })
			if len(batch.StagedSessions) != 1 || batch.StagedSessions[0] != regs[0].ArchiveSessionID {
				t.Fatal("late batch stage lost", batch.StagedSessions)
			}
			batch.CloseStagedResults()
			check("undo", func() error {
				selected, err := importUndoRegistrations(store, nil, r.Batch)
				if err == nil && (len(selected) != 1 || selected[0].ArchiveSessionID != regs[0].ArchiveSessionID) {
					t.Fatal("late undo stage lost", len(selected))
				}
				return err
			})
			// Each invocation closes its name index and every page descriptor.
			closeStore()
			if used, _ := budget.Charged(); used != 0 {
				t.Fatal("stage traversal leaked shared charge", used)
			}
			canceled, cancel := context.WithCancel(t.Context())
			cancel()
			if err := store.ResumeAllImportAdmissions(canceled); !errors.Is(err, context.Canceled) {
				t.Fatal("canceled traversal completed", err)
			}
		})
	}
}
