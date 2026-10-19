package state

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/config"
)

func TestImportStageTraversalPreservesPagesAndCharges(t *testing.T) {
	t.Parallel()
	s, reg, _ := ownedImportReleaseFixture(t, "selected")
	for i := range 101 {
		if err := os.MkdirAll(filepath.Join(s.home, "sessions", fmt.Sprintf("000-%03d", i)), 0700); err != nil {
			t.Fatal(err)
		}
	}
	budget := agentapi.NewNativeReadBudget(128 << 20)
	read, closeRead := s.WithReadBudget(t.Context(), budget)
	defer closeRead()
	pages, found := 0, false
	err := read.VisitImportStagePages(t.Context(), "", func(stages []ImportStage, next string) error {
		pages++
		if pages < 3 && (next == "" || len(stages) != 0) {
			t.Fatal("sparse lexical page changed", pages, next, len(stages))
		}
		if pages == 3 {
			found = len(stages) == 1 && stages[0].Registration.ArchiveSessionID == reg.ArchiveSessionID && next == ""
		}
		if used, _ := budget.Charged(); used == 0 {
			t.Fatal("unfunded live page/index")
		}
		return nil
	})
	if err != nil || pages != 3 || !found {
		t.Fatal("late stage lost", pages, found, err)
	}
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("name/page owner leaked", used)
	}
	if reflect.TypeFor[ImportStage]().Size() > 4096 {
		t.Fatal("descriptor slot reservation too small")
	}
	canceled, cancel := context.WithCancel(t.Context())
	err = read.VisitImportStagePages(canceled, "", func([]ImportStage, string) error { cancel(); return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatal("canceled page completed", err)
	}
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("cancellation leaked owner", used)
	}
}

func TestImportStageTraversalRefusesChangedMembershipAndPressure(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if err := os.MkdirAll(filepath.Join(s.home, "sessions", "middle"), 0700); err != nil {
		t.Fatal(err)
	}
	budget := agentapi.NewNativeReadBudget(128 << 20)
	read, closeRead := s.WithReadBudget(t.Context(), budget)
	defer closeRead()
	err := read.VisitImportStagePages(t.Context(), "", func([]ImportStage, string) error {
		return os.MkdirAll(filepath.Join(s.home, "sessions", "before"), 0700)
	})
	if !errors.Is(err, errImportStageMembershipChanged) {
		t.Fatal("membership change certified EOF", err)
	}
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("changed membership leaked owner", used)
	}
	owner := budget.Reservation()
	pressure := budget.Available() - 1024
	if !owner.Reserve(pressure) {
		t.Fatal("pressure")
	}
	err = read.VisitImportStagePages(t.Context(), "", func([]ImportStage, string) error { t.Fatal("unfunded callback"); return nil })
	if !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatal("unfunded name index accepted", err)
	}
	owner.Release(pressure)
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("pressure failure leaked owner", used)
	}
	if err = read.VisitImportStagePages(t.Context(), "", func([]ImportStage, string) error { return nil }); err != nil {
		t.Fatal("fresh retry", err)
	}
}

type noProgressDurableDirectory struct{}

func (noProgressDurableDirectory) ReadDir(int) ([]os.DirEntry, error) { return nil, nil }

func TestDurableDirectoryRequiresEOFAndChecksOverflow(t *testing.T) {
	t.Parallel()
	if _, err := readDurableDirectory(t.Context(), noProgressDurableDirectory{}); !errors.Is(err, io.ErrNoProgress) {
		t.Fatal("no progress became EOF", err)
	}
	count := int(^uint(0) >> 1)
	if err := countDurableEntry(&count); !errors.Is(err, ErrDurableStorageRecovery) || count < 0 {
		t.Fatal("entry counter overflow", count, err)
	}
}

func TestImportRecoveryPreservesUnchangedCursor(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		name := "scheduled"
		if explicit {
			name = "explicit"
		}
		t.Run(name, func(t *testing.T) {
			s := newTestStore(t)
			cfg, _, err := config.Load(s.home)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Archive.Enabled = true
			if err := config.Save(s.home, cfg); err != nil {
				t.Fatal(err)
			}
			if err := config.WithImportStaging(s.home, func(g config.DurableStorageGuard) error { return g.CheckHome(s.home) }); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(s.home, "sessions", "settled"), 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(s.home, "import-stage-cursor.json")
			if err := os.WriteFile(path, []byte(`{"after":""}`), 0600); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			budget := agentapi.NewNativeReadBudget(128 << 20)
			read, closeRead := s.WithReadBudget(t.Context(), budget)
			defer closeRead()
			if explicit {
				err = read.ResumeAllImportAdmissions(t.Context())
			} else {
				err = read.ResumeImportAdmissions(t.Context())
			}
			if err != nil {
				t.Fatal("settled recovery", err)
			}
			after, err := os.Stat(path)
			if err != nil || !sameDurableStamp(before, after) {
				t.Fatal("settled recovery rewrote unchanged scheduling cursor", err)
			}
			if used, peak := budget.Charged(); used != 0 || peak == 0 {
				t.Fatal("recovery skipped charged traversal or leaked its owner", used, peak)
			}
		})
	}
}

func TestImportStageTraversalPreservesBothCancellationOwners(t *testing.T) {
	t.Parallel()
	for _, captured := range []bool{false, true} {
		name := "caller"
		if captured {
			name = "captured-owner"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, reg, _ := ownedImportReleaseFixture(t, "canceled traversal")
			path := filepath.Join(s.home, importStageRelative(reg.ArchiveSessionID))
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
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
			read, closeRead := s.WithReadBudget(owner, budget)
			defer closeRead()
			positive := 0
			if err := read.VisitImportStagePages(caller, "", func(stages []ImportStage, _ string) error {
				positive += len(stages)
				return nil
			}); err != nil || positive != 1 {
				t.Fatal("funded live traversal positive control", positive, err)
			}
			if captured {
				cancelOwner()
			} else {
				cancelCaller()
			}
			called := false
			traverseErr := read.VisitImportStagePages(caller, "", func([]ImportStage, string) error {
				called = true
				return nil
			})
			if !errors.Is(traverseErr, context.Canceled) || called {
				t.Error("canceled traversal owner was replaced", called, traverseErr)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Error("canceled traversal changed selected evidence", err)
			}
			closeRead()
			closeRead()
			if used, _ := budget.Charged(); used != inherited {
				t.Error("canceled traversal changed inherited charge or leaked", used)
			}
		})
	}
}

func TestImportStageOwnedPagePreservesBothCancellationOwners(t *testing.T) {
	t.Parallel()
	for _, nested := range []bool{false, true} {
		name := "direct"
		if nested {
			name = "nested-result-scope"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, captured := range []bool{false, true} {
				name := "caller"
				if captured {
					name = "captured-owner"
				}
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					s, _, _ := ownedImportReleaseFixture(t, "canceled owned page")
					owner, cancelOwner := context.WithCancel(t.Context())
					defer cancelOwner()
					caller, cancelCaller := context.WithCancel(t.Context())
					defer cancelCaller()
					budget := agentapi.NewNativeReadBudget(2 << 20)
					read, closeRead := s.WithReadBudget(owner, budget)
					defer closeRead()
					closeNested := func() {}
					if nested {
						read, closeNested = read.WithImportResultScope(caller)
					}
					defer closeNested()
					stages, _, closePage, err := read.ImportStagesOwned(caller, "", 50)
					if err != nil || len(stages) != 1 {
						closePage()
						t.Fatal("funded live owned page positive control", len(stages), err)
					}
					closePage()
					if captured {
						cancelOwner()
					} else {
						cancelCaller()
					}
					stages, _, closePage, err = read.ImportStagesOwned(caller, "", 50)
					closePage()
					closePage()
					if !errors.Is(err, context.Canceled) || len(stages) != 0 {
						t.Error("canceled owned page revived a prior owner", len(stages), err)
					}
					closeNested()
					closeRead()
					if used, _ := budget.Charged(); used != 0 {
						t.Error("owned page or joined result scope leaked charge", used)
					}
				})
			}
		})
	}
}
