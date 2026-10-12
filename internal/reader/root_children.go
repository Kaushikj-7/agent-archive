package reader

import (
	"errors"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// SelectRootChildren preserves date-matched items and their transitive
// descendants, in input order. Items must be a complete inventory filtered by
// all non-date predicates. Fields supplies identity, parent and capture time
// only; selected items retain their original authority. Parent links use global
// session IDs, as stats does. Missing ancestors are not added. A visited-ID
// queue bounds work to the inventory size even for deep chains and cycles.
func SelectRootChildren[T any](items []T, filter Filter, fields func(T) (sessionID, parentSessionID string, capturedAt time.Time)) []T {
	children := make(map[string][]int, len(items))
	ids := make([]string, len(items))
	selected := make([]bool, len(items))
	var queue []string
	for i, item := range items {
		id, parent, captured := fields(item)
		ids[i] = id
		if parent != "" {
			children[parent] = append(children[parent], i)
		}
		if (filter.From.IsZero() || !captured.Before(filter.From)) && (filter.To.IsZero() || !captured.After(filter.To)) {
			selected[i] = true
			queue = append(queue, id)
		}
	}
	visited := make(map[string]bool, len(items))
	for at := 0; at < len(queue); at++ {
		id := queue[at]
		if id == "" || visited[id] {
			continue
		}
		visited[id] = true
		for _, child := range children[id] {
			if !selected[child] {
				selected[child] = true
				queue = append(queue, ids[child])
			}
		}
	}
	var result []T
	for i, item := range items {
		if selected[i] {
			result = append(result, item)
		}
	}
	return result
}

func selectRootChildHeaders(objects []storage.Object, revisions map[RevisionID]listingindex.Revision, filter Filter) ([]storage.Object, error) {
	var candidates []listingindex.Revision
	identities := make(map[string]bool, len(objects))
	for _, obj := range objects {
		if !isMetadataKey(obj.Key) {
			continue
		}
		r, exists := revisions[RevisionID{Key: obj.Key, ETag: obj.ETag}]
		if obj.ETag == "" || !exists {
			return nil, errors.New("listing index does not cover current metadata; run list --rebuild-index")
		}
		if filter.Harness != "" && !strings.HasPrefix(r.MetadataKey, "sessions/"+filter.Harness+"/") ||
			filter.Replays == ReplaysHidden && r.Replay || filter.Replays == ReplaysOnly && !r.Replay {
			continue
		}
		id, _, _ := rootChildRevisionFields(r)
		if identities[id] {
			return nil, errors.New("root-child selection requires complete metadata for duplicate session IDs")
		}
		identities[id] = true
		candidates = append(candidates, r)
	}
	selected := SelectRootChildren(candidates, filter, rootChildRevisionFields)
	keys := make(map[string]bool, len(selected))
	for _, r := range selected {
		keys[r.MetadataKey] = true
	}
	var result []storage.Object
	for _, obj := range objects {
		if keys[obj.Key] {
			result = append(result, obj)
		}
	}
	return result, nil
}

func rootChildRevisionFields(r listingindex.Revision) (string, string, time.Time) {
	parts := strings.Split(r.MetadataKey, "/")
	return parts[len(parts)-2], r.Parent, r.CapturedAt
}
