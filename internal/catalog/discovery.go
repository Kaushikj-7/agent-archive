package catalog

import (
	"context"
	"errors"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// DiscoveryBudget shares the existing bounded node accounting across a complete
// reconciliation pass, including cache hits and failed operations. The returned
// function reports consumed work even when a read fails.
func DiscoveryBudget(ctx context.Context, nodes int, bytes int64) (context.Context, func() PickerCost, error) {
	ctx, work, err := pickerWorkContext(ctx, PickerBudget{Nodes: nodes, Bytes: bytes})
	if err != nil {
		return ctx, nil, err
	}
	return context.WithValue(ctx, discoveryBudgetKey{}, true), func() PickerCost { return work.cost }, nil
}

// IdentityRecord includes tombstones so bounded discovery can checkpoint past
// removed identities. Entry is nil for a tombstone; neither omission nor a
// partial scan constitutes a current absence proof.
type IdentityRecord struct {
	Key   string
	Entry *CatalogEntry
}

// ScanIdentities returns at most limit physical identities strictly after after.
// Progress keys are reusable with a fresh snapshot. Done describes this pinned
// tree only; callers must separately check whether its root changed mid-sweep.
// On error no partial result or completeness proof is returned.
func (s *Snapshot) ScanIdentities(ctx context.Context, after string, limit int) ([]IdentityRecord, bool, error) {
	if limit < 1 || limit > 1000 {
		return nil, false, errors.New("identity discovery limit must be 1..1000")
	}
	if err := s.freshRequest(ctx); err != nil {
		return nil, false, err
	}
	records := make([]IdentityRecord, 0, limit+1)
	var walk func(ObjectRef, int) error
	walk = func(ref ObjectRef, depth int) error {
		if depth >= maxDepth {
			return errors.New("catalog tree depth exceeded")
		}
		n, err := s.readNode(ctx, ref)
		if err != nil {
			return err
		}
		for _, child := range n.Children {
			if child.Max <= after {
				continue
			}
			if err = walk(child.Ref, depth+1); err != nil {
				return err
			}
			if len(records) > limit {
				return nil
			}
		}
		for _, leaf := range n.Leaves {
			if leaf.Key <= after {
				continue
			}
			record, err := decodeIdentityRecord(leaf.Value, leaf.Key)
			if err != nil {
				return err
			}
			if err = s.head.validateLiveEntry(record.Entry); err != nil {
				return err
			}
			records = append(records, IdentityRecord{leaf.Key, record.Entry})
			if len(records) > limit {
				break
			}
		}
		return nil
	}
	if err := walk(s.head.Identity, 0); err != nil {
		return nil, false, err
	}
	if err := s.ValidateRead(ctx); err != nil {
		return nil, false, err
	}
	done := len(records) <= limit
	if !done {
		records = records[:limit]
	}
	return records, done, nil
}

type discoveryBudgetKey struct{}

type discoveryRecord string

const (
	discoveryHead        discoveryRecord = HeadKey
	discoveryCoordinator discoveryRecord = CoordinatorKey
)

func readDiscoveryVersion(ctx context.Context, getter storage.CatalogVersionedGetter, record discoveryRecord, limit int64) ([]byte, storage.CatalogObjectVersion, error) {
	key := string(record)
	work, _ := ctx.Value(pickerWorkKey{}).(*pickerWork)
	_, enabled := ctx.Value(discoveryBudgetKey{}).(bool)
	if work == nil || !enabled {
		return getter.GetCatalogVersion(ctx, key, limit)
	}
	fullLimit := limit
	limit = min(limit, work.budget.Bytes-work.cost.Bytes)
	if limit <= 0 {
		return nil, storage.CatalogObjectVersion{}, ErrPickerBudget
	}
	switch record {
	case discoveryHead:
		work.cost.HeadObservations++
	case discoveryCoordinator:
		work.cost.CoordinatorReads++
	}
	raw, version, err := getter.GetCatalogVersion(ctx, key, limit)
	if errors.Is(err, storage.ErrObjectTooLarge) && limit < fullLimit {
		return nil, version, ErrPickerBudget
	}
	if err == nil {
		err = work.bytes(len(raw))
	}
	return raw, version, err
}
