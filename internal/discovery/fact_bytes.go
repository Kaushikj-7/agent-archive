package discovery

import "github.com/wangjohn/agent-archive/internal/codexmeta"

// These bounds include worst-case JSON string escaping before accumulation or
// serialization. Structural allowances cover numeric fields and separators.
// They intentionally overestimate; they are charged fact sizes, not Go RSS.
func directoryByteBound(d directory) int64 { return 6*int64(len(d.Root)+len(d.Path)) + 512 }

func identityByteBound(id codexmeta.CodexIdentity) int64 {
	size := 6*int64(len(id.ThreadID)+len(id.RootID)+len(id.ParentID)+len(id.ForkID)+len(id.RolloutID)+len(id.HistoryMode)) + 768
	if id.HistoryBase != nil {
		size += 6*int64(len(id.HistoryBase.RolloutID)) + 192
	}
	return size
}

// rolloutFactByteBound is the charge of one observed rollout fact the lookup
// keeps for the pass.
func rolloutFactByteBound(source SourceDescriptor, id codexmeta.CodexIdentity) int64 {
	return 12*int64(len(source.Locator)) + 6*int64(len(source.Root)+len(source.StableKey)) + identityByteBound(id) + 1024
}
