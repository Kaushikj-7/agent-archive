package storage

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
)

// ProbeConditionalSemantics performs destructive operations ONLY on a fresh
// synthetic .catalog-qualification key. Operators must invoke it explicitly on
// an isolated provider destination. A pass is one observation, not a release
// qualification: record exact endpoint, SDK, credentials policy and repetitions
// in reviewed provider evidence before enabling catalog-v4.
func ProbeConditionalSemantics(ctx context.Context, store ObjectStore) (err error) {
	conditional, ok := store.(ConditionalPutter)
	if !ok {
		return ErrAtomicCatalogUnqualified
	}
	getter, ok := store.(LimitedVersionedGetter)
	if !ok {
		return errors.New("qualification requires bounded versioned reads")
	}
	key := ".catalog-qualification/" + rand.Text() + ".json"
	created := false
	defer func() {
		if created {
			err = errors.Join(err, store.Delete(ctx, key))
		}
	}()
	first := []byte(`{"revision":"initial"}`)
	etag, err := conditional.PutConditional(ctx, key, first, PutCondition{CreateOnly: true})
	if err != nil {
		return err
	}
	created = true
	if etag == "" {
		return errors.New("conditional write returned no validator")
	}
	if _, err = conditional.PutConditional(ctx, key, []byte(`{"revision":"duplicate"}`), PutCondition{CreateOnly: true}); !errors.Is(err, ErrPreconditionFailed) {
		return fmt.Errorf("duplicate create must conflict: %w", err)
	}
	var group sync.WaitGroup
	results := make(chan error, 2)
	for _, body := range [][]byte{[]byte(`{"revision":"left"}`), []byte(`{"revision":"right"}`)} {
		group.Go(func() {
			_, e := conditional.PutConditional(ctx, key, body, PutCondition{MatchETag: etag})
			results <- e
		})
	}
	group.Wait()
	close(results)
	wins, conflicts := 0, 0
	for e := range results {
		if e == nil {
			wins++
		} else if errors.Is(e, ErrPreconditionFailed) {
			conflicts++
		} else {
			return e
		}
	}
	if wins != 1 || conflicts != 1 {
		return fmt.Errorf("same-validator race produced %d successes and %d conflicts", wins, conflicts)
	}
	raw, newTag, err := getter.GetLimitedVersioned(ctx, key, 1024)
	if err != nil {
		return err
	}
	if newTag == "" || newTag == etag || string(raw) != `{"revision":"left"}` && string(raw) != `{"revision":"right"}` {
		return errors.New("conditional winner readback differs")
	}
	if _, err = conditional.PutConditional(ctx, key, first, PutCondition{MatchETag: etag}); !errors.Is(err, ErrPreconditionFailed) {
		return fmt.Errorf("stale validator must conflict: %w", err)
	}
	return nil
}
