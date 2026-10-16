package storagetest

import (
	"context"
	"maps"
	"slices"
	"strings"
)

// CloneMemoryStore captures a private writable snapshot without copying every
// body. Each snapshot owns its map and lock. Stored bodies are internally
// immutable: writes copy their input and replace entire objects, and reads copy
// their output, so sharing those bytes cannot expose mutations between stores.
// Object validators and timestamps are preserved exactly.
func CloneMemoryStore(ctx context.Context, source *MemoryStore) (*MemoryStore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	source.mu.RLock()
	defer source.mu.RUnlock()
	clone := &MemoryStore{objects: maps.Clone(source.objects)}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return clone, nil
}

// MemorySnapshot holds immutable object bodies and exact versions, without a
// writable store or map. Its only operation creates a private writable store.
type MemorySnapshot struct {
	entries []memorySnapshotEntry
}

type memorySnapshotEntry struct {
	key    string
	object memoryObject
}

// SnapshotMemoryStore captures an immutable seed. Later writes to the source
// replace its objects and cannot change the captured bodies or versions.
func SnapshotMemoryStore(ctx context.Context, source *MemoryStore) (MemorySnapshot, error) {
	if err := ctx.Err(); err != nil {
		return MemorySnapshot{}, err
	}
	source.mu.RLock()
	defer source.mu.RUnlock()
	snapshot := MemorySnapshot{entries: make([]memorySnapshotEntry, 0, len(source.objects))}
	for key, object := range source.objects {
		snapshot.entries = append(snapshot.entries, memorySnapshotEntry{key, object})
	}
	slices.SortFunc(snapshot.entries, func(a, b memorySnapshotEntry) int { return strings.Compare(a.key, b.key) })
	if err := ctx.Err(); err != nil {
		return MemorySnapshot{}, err
	}
	return snapshot, nil
}

// Clone creates a private writable store while reusing immutable seed bodies.
func (snapshot MemorySnapshot) Clone(ctx context.Context) (*MemoryStore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	clone := &MemoryStore{objects: make(map[string]memoryObject, len(snapshot.entries))}
	for _, entry := range snapshot.entries {
		clone.objects[entry.key] = entry.object
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return clone, nil
}
