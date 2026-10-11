package discovery

import "github.com/wangjohn/agent-archive/internal/codexmeta"

// These bounds include worst-case JSON string escaping before accumulation or
// serialization. Structural allowances cover numeric fields and separators.
// They intentionally overestimate; they are charged fact sizes, not Go RSS.
func directoryByteBound(d directory) int64 { return 6*int64(len(d.Root)+len(d.Path)) + 512 }

func (candidate coverageCandidate) byteBound() int64 {
	return 6*int64(len(candidate.Source.Root)+len(candidate.Source.Locator)+len(candidate.Source.StableKey)) + identityByteBound(candidate.Identity) + 512
}

func (r coverageRequest) byteBound() int64 {
	size := int64(512)
	for _, candidates := range []map[string]coverageCandidate{r.Candidates, r.Proven} {
		for path, candidate := range candidates {
			size += 6*int64(len(path)) + candidate.byteBound() + 16
		}
	}
	return size
}

func (c *coverageInventory) byteBound() int64 {
	if c.factBound != 0 {
		return c.factBound
	}
	size := int64(1024)
	for _, root := range c.Roots {
		size += 6*int64(len(root)) + 16
	}
	for id, r := range c.Requests {
		size += 6*int64(len(id)) + r.byteBound() + 16
	}
	for key, d := range c.Directories {
		size += 6*int64(len(key)) + directoryByteBound(d.Directory) + 1024
	}
	// Validation can grow to one entry per observed directory. Reserve that
	// capacity even before beginValidation copies its bounded work queue.
	for _, d := range c.Directories {
		size += directoryByteBound(d.Directory)
	}
	c.factBound = size
	return size
}

func identityByteBound(id codexmeta.CodexIdentity) int64 {
	size := 6*int64(len(id.ThreadID)+len(id.RootID)+len(id.ParentID)+len(id.ForkID)+len(id.RolloutID)+len(id.HistoryMode)) + 768
	if id.HistoryBase != nil {
		size += 6*int64(len(id.HistoryBase.RolloutID)) + 192
	}
	return size
}

func (c *coverageInventory) totalByteBound() int64 { return c.byteBound() + c.hintBytes }
