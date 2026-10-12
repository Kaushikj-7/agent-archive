package catalog

import (
	"context"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// updateChildCounters derives indexes from the candidate project tree in the
// same CAS as the mutation. It changes no immutable metadata or revision.
func (w *Writer) updateChildCounters(ctx context.Context, h *CatalogHead, key string, old, next *CatalogEntry) error {
	affected := map[string]bool{key: true}
	for _, entry := range []*CatalogEntry{old, next} {
		if entry == nil || entry.Summary.ParentSessionID == "" {
			continue
		}
		parent, err := archive.MetadataObjectKey(entry.Summary.Harness.Name, entry.Summary.ParentSessionID)
		if err != nil {
			return err
		}
		affected[parent] = true
	}
	snapshot := &Snapshot{writer: w, head: *h, started: time.Now(), cache: &NodeCache{}}
	for parent := range affected {
		r, err := w.find(ctx, h.Identity, parent)
		if err != nil {
			return err
		}
		if r.Entry == nil {
			continue
		}
		entry := *r.Entry
		counts := []*uint64{&entry.OrdinaryChildren, &entry.ReplayChildren}
		for i, count := range counts {
			prefix := ChildPrefix(entry.Summary.Harness.Name, entry.Summary.SessionID, i == 1)
			*count, err = snapshot.Count(ctx, Query{Index: ProjectIndex, Lower: prefix + "0", Upper: prefix + ":"})
			if err != nil {
				return err
			}
		}
		if entry.OrdinaryChildren == r.Entry.OrdinaryChildren && entry.ReplayChildren == r.Entry.ReplayChildren {
			continue
		}
		if err = w.updateOrders(ctx, h, parent, r.Entry, &entry); err != nil {
			return err
		}
		h.Identity, err = w.update(ctx, h.Identity, parent, record{r.Revision, &entry})
		if err != nil {
			return err
		}
	}
	return nil
}
