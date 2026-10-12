package catalog

import (
	"context"
	"errors"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// Delta is verified reconciliation between two complete identity roots. It
// carries changed/deleted entries, never partial canonical LIST authority.
// Changed overflow entries are lazy projections; complete-summary consumers must
// call ResolveRow before using their summaries.
type Delta struct {
	Prior   ObjectRef
	Next    ObjectRef
	Changed []Row
	Removed []string
	Rebuild bool
}

// Delta validates prior and new immutable paths. Equal hashed subtrees are
// skipped. A structural split may read the changed subtree in full; this is
// reconciliation work, not an advertised bounded user query. An unavailable
// prior root requires a complete summary rebuild, preserving all results.
func (s *Snapshot) Delta(ctx context.Context, prior ObjectRef) (Delta, error) {
	d := Delta{Prior: prior, Next: s.head.Identity}
	if err := s.check(ctx); err != nil {
		return d, err
	}
	if prior == d.Next && prior.Key != "" {
		return d, nil
	}
	old := map[string]CatalogEntry{}
	next := map[string]CatalogEntry{}
	if prior.Key == "" {
		d.Rebuild = true
		if err := s.collectDelta(ctx, d.Next, next, 0); err != nil {
			return d, err
		}
	} else if err := s.diffDelta(ctx, prior, d.Next, old, next, 0); err != nil {
		// Unknown previous roots can be collected after the lifetime. Integrity,
		// cancellation and network errors fail closed rather than hiding corruption.
		if !errors.Is(err, storage.ErrNotFound) {
			return d, err
		}
		d.Rebuild = true
		old = map[string]CatalogEntry{}
		next = map[string]CatalogEntry{}
		if err = s.collectDelta(ctx, d.Next, next, 0); err != nil {
			return d, err
		}
	}
	for key, entry := range next {
		if previous, ok := old[key]; !ok || previous.Revision != entry.Revision || previous.Metadata != entry.Metadata || previous.OrdinaryChildren != entry.OrdinaryChildren || previous.ReplayChildren != entry.ReplayChildren {
			d.Changed = append(d.Changed, Row{Key: key, Entry: entry})
		}
	}
	for key := range old {
		if _, ok := next[key]; !ok {
			d.Removed = append(d.Removed, key)
		}
	}
	return d, s.check(ctx)
}

func (s *Snapshot) collectDelta(ctx context.Context, ref ObjectRef, target map[string]CatalogEntry, depth int) error {
	if depth >= maxDepth {
		return errors.New("catalog delta depth exceeded")
	}
	n, err := s.readNode(ctx, ref)
	if err != nil {
		return err
	}
	for _, child := range n.Children {
		if err = s.collectDelta(ctx, child.Ref, target, depth+1); err != nil {
			return err
		}
	}
	for _, leaf := range n.Leaves {
		r, e := decodeIdentityRecord(leaf.Value, leaf.Key)
		if e != nil {
			return e
		}
		if r.Entry != nil {
			if leafSessionKey(r.Entry) != leaf.Key || r.Entry.Revision == "" {
				return errors.New("catalog delta identity mismatch")
			}
			target[leaf.Key] = *r.Entry
		}
	}
	return nil
}

func (s *Snapshot) diffDelta(ctx context.Context, a, b ObjectRef, old, next map[string]CatalogEntry, depth int) error {
	if a == b {
		return nil
	}
	if depth >= maxDepth {
		return errors.New("catalog delta depth exceeded")
	}
	na, err := s.readNode(ctx, a)
	if err != nil {
		return err
	}
	nb, err := s.readNode(ctx, b)
	if err != nil {
		return err
	}
	aligned := len(na.Children) > 0 && len(na.Children) == len(nb.Children)
	if aligned {
		for i := range na.Children {
			if na.Children[i].Max != nb.Children[i].Max {
				aligned = false
				break
			}
		}
	}
	if aligned {
		for i := range na.Children {
			if err = s.diffDelta(ctx, na.Children[i].Ref, nb.Children[i].Ref, old, next, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err = s.collectDelta(ctx, a, old, depth); err != nil {
		return err
	}
	return s.collectDelta(ctx, b, next, depth)
}
