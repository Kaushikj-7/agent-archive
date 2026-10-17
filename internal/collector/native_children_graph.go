package collector

import "github.com/wangjohn/agent-archive/internal/archive"

// nativeChildGraph evaluates only the admitted registration inventory. Logical
// agent names and transcript paths supply no graph authority. Its temporary
// working maps borrow the linker's shared-ledger reservation and do not survive
// reconciliation. Each node is visited at most once, including ancestor chains.
type nativeChildGraph struct {
	invalid map[string]bool
}

type nativeChildGraphIndex struct {
	byNative  map[nativeLinkKey]string
	ambiguous map[nativeLinkKey]bool
	byArchive map[string]archive.SessionRegistration
	parents   map[string]string
	invalid   map[string]bool
}

func newNativeChildGraph(registrations []archive.SessionRegistration) nativeChildGraph {
	index := nativeChildGraphIndex{
		byNative: make(map[nativeLinkKey]string, len(registrations)), ambiguous: make(map[nativeLinkKey]bool),
		byArchive: make(map[string]archive.SessionRegistration, len(registrations)),
		parents:   make(map[string]string, len(registrations)), invalid: make(map[string]bool),
	}
	for _, reg := range registrations {
		index.observe(reg)
	}
	for _, reg := range registrations {
		if reg.NativeChild {
			index.connect(reg)
		}
	}
	index.validateAncestry()
	return nativeChildGraph{invalid: index.invalid}
}

func (index *nativeChildGraphIndex) observe(reg archive.SessionRegistration) {
	key := nativeParentKey(reg, reg.NativeSessionID)
	if previous, ok := index.byNative[key]; ok && previous != reg.ArchiveSessionID {
		index.ambiguous[key] = true
	}
	index.byNative[key] = reg.ArchiveSessionID
	if previous, ok := index.byArchive[reg.ArchiveSessionID]; ok && (nativeParentKey(previous, previous.NativeSessionID) != key || previous.ParentNativeSessionID != reg.ParentNativeSessionID || previous.ParentSessionID != reg.ParentSessionID || previous.NativeRootSessionID != reg.NativeRootSessionID || previous.NativeChild != reg.NativeChild) {
		index.invalid[reg.ArchiveSessionID] = true
	}
	index.byArchive[reg.ArchiveSessionID] = reg
}

func (index *nativeChildGraphIndex) connect(reg archive.SessionRegistration) {
	if index.ambiguous[nativeParentKey(reg, reg.NativeSessionID)] {
		index.invalid[reg.ArchiveSessionID] = true
		return
	}
	key := nativeParentKey(reg, reg.ParentNativeSessionID)
	if linked, found := index.byArchive[reg.ParentSessionID]; reg.ParentSessionID != "" && found && nativeParentKey(linked, linked.NativeSessionID) != key {
		index.invalid[reg.ArchiveSessionID] = true
		return
	}
	parent, found := index.byNative[key]
	if index.ambiguous[key] || reg.ParentNativeSessionID == reg.NativeSessionID {
		index.invalid[reg.ArchiveSessionID] = true
		return
	}
	if !found {
		return // Absence is pending, never a fabricated root.
	}
	if reg.ParentSessionID != "" && reg.ParentSessionID != parent {
		index.invalid[reg.ArchiveSessionID] = true
		return
	}
	index.parents[reg.ArchiveSessionID] = parent
}

func (index *nativeChildGraphIndex) validateAncestry() {
	// Functional graph traversal, using one path per unvisited component. A
	// cycle or conflicting ancestor invalidates every descendant in that path.
	done := make(map[string]bool, len(index.byArchive))
	observedRoots := make(map[string]string, len(index.byArchive))
	for id := range index.byArchive {
		path := make([]string, 0)
		onPath := make(map[string]bool)
		current := id
		for current != "" && !done[current] && !index.invalid[current] && !onPath[current] {
			onPath[current] = true
			path = append(path, current)
			current = index.parents[current]
		}
		invalid := index.invalid[current] || onPath[current]
		root := observedRoots[current]
		for i := len(path) - 1; i >= 0; i-- {
			node := path[i]
			reg := index.byArchive[node]
			// A present independently admitted ordinary owner identifies the
			// terminal root. Missing parents never enter this branch and no
			// inferred value is persisted as native evidence.
			if root == "" && reg.NativeRootSessionID == "" && !reg.NativeChild && reg.ParentNativeSessionID == "" && reg.ParentSessionID == "" {
				root = reg.NativeSessionID
			}
			if observed := nativeAncestryRoot(reg); observed != "" {
				if root != "" && root != observed {
					invalid = true
				}
				root = observed
			}
			if invalid {
				index.invalid[node] = true
			}
			observedRoots[node] = root
			done[node] = true
		}
	}
}

func nativeAncestryRoot(reg archive.SessionRegistration) string {
	// Codex session_id may identify a child's immediate-parent history scope.
	// Preserve that source fact without interpreting it as the terminal tree
	// root. Distinct explicit roots, including fork provenance, still compare.
	if reg.Harness.Name == archive.HarnessCodex && reg.NativeChild && reg.NativeRootSessionID == reg.ParentNativeSessionID {
		return ""
	}
	return reg.NativeRootSessionID
}

func (g nativeChildGraph) accepts(reg archive.SessionRegistration) bool {
	return !g.invalid[reg.ArchiveSessionID]
}
