package reader

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/wangjohn/agent-archive/internal/storage"
)

// rangePlan contains only listed keys, never titles or content. It splits work
// across a complete key space and cannot establish existence or absence.
type rangePlan struct {
	Prefix string   `json:"prefix"`
	Keys   int      `json:"keys"`
	Bounds []string `json:"bounds"`
}

// planKeyRanges balances actual listed keys. Small prefixes stay sequential;
// larger ones use page-sized ranges with a shorter final range. All prefixes share the
// sixteen request slots, so extra small ranges add transfers and queueing.
// The policy bounds hints by 15 + ceil(keys/1000) boundaries per prefix.
func planKeyRanges(keys []string) []string {
	if len(keys) <= 1000 {
		return nil
	}
	ordered := slices.Clone(keys)
	sort.Strings(ordered)
	ordered = slices.Compact(ordered)
	if len(ordered) <= 1000 {
		return nil
	}
	ranges := (len(ordered) + 999) / 1000
	bounds := make([]string, 0, ranges-1)
	for i := 1000; i < len(ordered); i += 1000 {
		bounds = append(bounds, ordered[i-1])
	}
	return bounds
}

func (c *MetadataCache) rangePlanPath(prefix string) string {
	// No .json suffix: legacy cache inventory discards obsolete flat .json files.
	return filepath.Join(c.dir, ".ranges-"+sha256Hex([]byte(prefix)))
}

func (c *MetadataCache) rangeBounds(prefix string) ([]string, bool) {
	if c == nil {
		return nil, false
	}
	path := c.rangePlanPath(prefix)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() > 4<<20 {
		return nil, false
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, (4<<20)+1))
	var plan rangePlan
	if err != nil || len(data) > 4<<20 || json.Unmarshal(data, &plan) != nil || plan.Prefix != prefix || plan.Keys < 0 {
		return nil, false
	}
	pages := plan.Keys / 1000
	if plan.Keys%1000 != 0 {
		pages++
	}
	if len(plan.Bounds) > 15+pages {
		return nil, false
	}
	for i, bound := range plan.Bounds {
		if bound == "" || !strings.HasPrefix(bound, prefix) || (i > 0 && bound <= plan.Bounds[i-1]) {
			return nil, false
		}
	}
	return plan.Bounds, true
}

func (c *MetadataCache) saveRangeBounds(prefix string, objects []storage.Object) {
	if c == nil {
		return
	}
	keys := make([]string, len(objects))
	for i, object := range objects {
		keys[i] = object.Key
	}
	data, err := json.Marshal(rangePlan{Prefix: prefix, Keys: len(keys), Bounds: planKeyRanges(keys)})
	if err == nil && len(data) <= 4<<20 {
		_ = writeCacheFile(c.rangePlanPath(prefix), data)
	}
}
