package state

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
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
