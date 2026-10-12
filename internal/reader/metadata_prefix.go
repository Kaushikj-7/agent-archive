package reader

import (
	"context"
	"sort"
	"strings"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// FindMetadataPrefix discovers current canonical headers once and reads only
// matching identity bodies. Additional matchers extend the result with text
// matches: because headers contain no titles, those require reading all bodies.
// Matchers run after validation, serially, and never suppress prefix matches.
func FindMetadataPrefix(ctx context.Context, store storage.ObjectStore, prefix, idPrefix string, filter Filter, opts ListOptions, matchers ...func(archive.Metadata) bool) ([]archive.Metadata, error) {
	opts.Cache.maintain(ctx)
	if CatalogAuthority(store) {
		return catalogPrefix(ctx, store, idPrefix, filter, opts, matchers...)
	}
	idPrefix = strings.ToLower(idPrefix)
	canonicalPrefix := listPrefixFor(prefix, filter.Harness)
	known := opts.Cache.keys(canonicalPrefix)
	objects, err := listObjects(ctx, store, canonicalPrefix, known)
	if err != nil {
		return nil, err
	}
	if opts.Cache != nil {
		opts.Cache.evictUnlisted(known, objects)
	}
	candidates := make([]storage.Object, 0, len(objects))
	for _, obj := range objects {
		if !isMetadataKey(obj.Key) {
			continue
		}
		parts := strings.Split(obj.Key, "/")
		if len(matchers) > 0 || strings.HasPrefix(strings.ToLower(parts[len(parts)-2]), idPrefix) {
			candidates = append(candidates, obj)
		}
	}
	loaded, skipped, err := readSidecars(ctx, store, candidates, opts.Cache, opts.Progress)
	if err != nil {
		return nil, err
	}
	if opts.Skipped != nil {
		for _, sidecar := range skipped {
			opts.Skipped(sidecar)
		}
	}
	var result []archive.Metadata
	for _, m := range loaded {
		if !matches(m, filter) {
			continue
		}
		include := strings.HasPrefix(strings.ToLower(m.SessionID), idPrefix)
		for _, match := range matchers {
			if match(m) {
				include = true
			}
		}
		if include {
			result = append(result, m)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		a, b := result[i], result[j]
		if !a.CapturedAt.Equal(b.CapturedAt) {
			return a.CapturedAt.After(b.CapturedAt)
		}
		if a.Harness.Name != b.Harness.Name {
			return a.Harness.Name < b.Harness.Name
		}
		return a.SessionID < b.SessionID
	})
	return result, nil
}

func catalogPrefix(ctx context.Context, store storage.ObjectStore, idPrefix string, filter Filter, opts ListOptions, matchers ...func(archive.Metadata) bool) ([]archive.Metadata, error) {
	snapshot, err := catalog.OpenSnapshot(ctx, store, nil)
	if err != nil {
		return nil, err
	}
	rows, err := catalogRows(ctx, snapshot)
	if err != nil {
		return nil, err
	}
	var selected []catalog.Row
	for _, row := range rows {
		m := row.Entry.Summary
		if !matches(m, filter) {
			continue
		}
		include := strings.HasPrefix(strings.ToLower(m.SessionID), strings.ToLower(idPrefix))
		for _, matcher := range matchers {
			if matcher(m) {
				include = true
			}
		}
		if include {
			selected = append(selected, row)
		}
	}
	sort.Slice(selected, func(i, j int) bool {
		a, b := selected[i].Entry.Summary, selected[j].Entry.Summary
		if !a.CapturedAt.Equal(b.CapturedAt) {
			return a.CapturedAt.After(b.CapturedAt)
		}
		return selected[i].Key < selected[j].Key
	})
	return hydrateCatalogRows(ctx, snapshot, selected, opts)
}
