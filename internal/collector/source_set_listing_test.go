package collector

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type budgetedListingRepairStore struct {
	*noChecksumHistoryStore
	beforeSourceStat func()
	unboundedReads   int
}

func (s *budgetedListingRepairStore) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	if strings.Contains(key, "/source.") && s.beforeSourceStat != nil {
		callback := s.beforeSourceStat
		s.beforeSourceStat = nil
		callback()
	}
	return s.noChecksumHistoryStore.Stat(ctx, key)
}

func (s *budgetedListingRepairStore) GetVersioned(context.Context, string) ([]byte, string, error) {
	s.unboundedReads++
	return nil, "", errors.New("unbounded metadata revision read")
}

func TestListingRepairSharesReadBudgetAndUsesBoundedRevisionResponses(t *testing.T) {
	scan, pending := privacyJournal(t)
	cloud := scan.remote.(*storagetest.MemoryStore)
	for _, stage := range pending.History.Sources {
		data, err := scan.local.ReadPendingSource(scan.id(), stage)
		if err != nil {
			t.Fatal(err)
		}
		if err := cloud.Put(t.Context(), stage.Reference.Key, data); err != nil {
			t.Fatal(err)
		}
	}
	if err := cloud.Put(t.Context(), pending.MetadataKey, pending.MetadataBytes); err != nil {
		t.Fatal(err)
	}
	scan.releaseRetained()
	budget := agentapi.NewNativeReadBudget(64 << 20)
	owner := budget.Reservation()
	var pressure int64
	remote := &budgetedListingRepairStore{noChecksumHistoryStore: &noChecksumHistoryStore{MemoryStore: cloud}, beforeSourceStat: func() {
		pressure = budget.Available()
		if !owner.Reserve(pressure) {
			t.Fatal("reserve competing source-read pressure")
		}
	}}
	scan.remote, scan.retainedBudget = remote, budget
	if err := scan.repairListingRevision(pending.MetadataKey); !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatalf("repair bypassed shared source-read budget: %v", err)
	}
	if remote.sourceGets != 0 || remote.sourceLimitedGets != 0 || remote.unboundedReads != 0 {
		t.Fatal("repair read source before reserving or used unbounded metadata")
	}
	if used, _ := budget.Charged(); used != pressure {
		t.Fatalf("failed repair leaked owned bytes: got%d want%d", used, pressure)
	}
	owner.Release(pressure)
	if err := scan.repairListingRevision(pending.MetadataKey); err != nil {
		t.Fatal("repair did not progress after pressure release", err)
	}
	if remote.sourceGets != 0 || remote.sourceLimitedGets == 0 || remote.unboundedReads != 0 {
		t.Fatal("repair did not verify sources with bounded responses")
	}
	if used, _ := budget.Charged(); used != 0 {
		t.Fatalf("successful repair leaked%d", used)
	}
}
