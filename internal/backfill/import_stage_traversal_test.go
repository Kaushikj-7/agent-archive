package backfill

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
		t.Run(strconv.Itoa(count), func(t *testing.T) {
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

func TestExplicitImportRecoveryPreservesBothCancellationOwners(t *testing.T) {
	for _, captured := range []bool{false, true} {
		name := "caller"
		if captured {
			name = "captured-owner"
		}
		t.Run(name, func(t *testing.T) {
			r, candidate, _ := stagingFixture(t)
			r.AfterStage = func(string) error { return errors.New("synthetic interruption") }
			_, err := r.Run([]Candidate{candidate})
			if err == nil {
				t.Fatal("missing interrupted preparation")
			}
			stages, _, closeStages, err := r.Store.ImportStagesOwned(t.Context(), "", 50)
			if err != nil || len(stages) != 1 {
				closeStages()
				t.Fatal("actual selected stage positive control", len(stages), err)
			}
			id := strings.Clone(stages[0].Registration.ArchiveSessionID)
			closeStages()
			paths := []string{filepath.Join(r.Home, "config.json"), filepath.Join(r.Home, "sessions", id, "import-stage", "stage.json")}
			before := make([][]byte, len(paths))
			for i, path := range paths {
				before[i], err = os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
			}
			owner, cancelOwner := context.WithCancel(t.Context())
			defer cancelOwner()
			caller, cancelCaller := context.WithCancel(t.Context())
			defer cancelCaller()
			budget := agentapi.NewNativeReadBudget(2 << 20)
			pressure := budget.Reservation()
			const inherited = 4096
			if !pressure.Reserve(inherited) {
				t.Fatal("inherited pressure")
			}
			defer pressure.Release(inherited)
			read, closeRead := r.Store.WithReadBudget(owner, budget)
			defer closeRead()
			if captured {
				cancelOwner()
			} else {
				cancelCaller()
			}
			if err := read.ResumeAllImportAdmissions(caller); !errors.Is(err, context.Canceled) {
				t.Error("canceled recovery owner was replaced", err)
			}
			for i, path := range paths {
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(before[i], after) {
					t.Error("canceled recovery changed durable evidence", path, err)
				}
			}
			if regs, err := r.Store.LoadRegistrations(); err != nil || len(regs) != 0 {
				t.Error("canceled recovery admitted a stage", len(regs), err)
			}
			if _, err := os.Stat(filepath.Join(r.Home, "import-stage-cursor.json")); !errors.Is(err, os.ErrNotExist) {
				t.Error("canceled recovery changed scheduling progress", err)
			}
			closeRead()
			closeRead()
			if used, _ := budget.Charged(); used != inherited {
				t.Error("canceled recovery changed inherited charge or leaked", used)
			}
		})
	}
}

func TestExplicitImportRecoveryVisitsBeforeScheduledCursor(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		name := "scheduled"
		if explicit {
			name = "explicit"
		}
		t.Run(name, func(t *testing.T) {
			r, candidate, _ := stagingFixture(t)
			r.AfterStage = func(string) error { return errors.New("synthetic interruption") }
			_, err := r.Run([]Candidate{candidate})
			if err == nil {
				t.Fatal("missing interrupted preparation")
			}
			stages, _, closeStages, err := r.Store.ImportStagesOwned(t.Context(), "", 50)
			if err != nil || len(stages) != 1 {
				closeStages()
				t.Fatal("actual selected stage positive control", len(stages), err)
			}
			selectedID := strings.Clone(stages[0].Registration.ArchiveSessionID)
			digest := strings.Clone(stages[0].SHA256)
			closeStages()
			if err := os.Remove(candidate.TranscriptPath); err != nil {
				t.Fatal(err)
			}
			cursorPath := filepath.Join(r.Home, "import-stage-cursor.json")
			if err := os.WriteFile(cursorPath, []byte(fmt.Sprintf(`{"after":%q}`, selectedID+"z")), 0600); err != nil {
				t.Fatal(err)
			}
			budget := agentapi.NewNativeReadBudget(2 << 20)
			read, closeRead := r.Store.WithReadBudget(t.Context(), budget)
			defer closeRead()
			if explicit {
				err = read.ResumeAllImportAdmissions(t.Context())
			} else {
				err = read.ResumeImportAdmissions(t.Context())
			}
			if err != nil {
				t.Fatal("recovery", err)
			}
			regs, err := r.Store.LoadRegistrations()
			if err != nil {
				t.Fatal(err)
			}
			if explicit && (len(regs) != 1 || regs[0].ArchiveSessionID != selectedID || regs[0].StagedImportSHA256 != digest || !regs[0].InBatch(r.Batch)) {
				t.Error("first explicit recovery skipped selected stage before scheduled cursor", len(regs))
			} else if !explicit && len(regs) != 0 {
				t.Error("scheduled recovery ignored its lexical cursor", len(regs))
			}
			if after, err := os.ReadFile(cursorPath); err != nil || !bytes.Contains(after, []byte(`"after": ""`)) {
				t.Error("completed suffix/full pass did not clear progress", string(after), err)
			}
			closeRead()
			closeRead()
			if used, _ := budget.Charged(); used != 0 {
				t.Error("recovery leaked result or index owner", used)
			}
		})
	}
}
