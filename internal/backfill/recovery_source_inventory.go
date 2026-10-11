package backfill

import (
	"context"
	"io/fs"
	"os"
	"sort"
)

// recoverySourceInventory binds discovered membership to the native paths
// inspected during discovery. It never reopens transcripts. Directory stamps
// include absent stores, so new files/clones require a new plan.
//
// It checks membership, not content. A transcript that a work item owns (the
// item's header came from that same file) may grow in place: running agents
// append to their transcripts, and every header fact comes from a complete
// leading record that an append cannot change. A header that no complete record
// decided (no cwd yet, no session_meta) already leaves recovery unavailable for
// the plan. An owned file's content is only rechecked where its own source
// check (checkSource, sourceCurrent) runs: for an imported session's source and
// for the witness a recovery relies on. A same-inode rewrite that grows another
// owned transcript is therefore not caught here; agents append rather than
// rewrite, so that limit is accepted. Added, removed or replaced paths, mode changes,
// truncation and same-size rewrites still change the inventory, as does any
// change to a path no work item owns. These are practical observations, with
// the same timestamp-restoration limit as the existing native file checks, not
// an atomic filesystem snapshot.
type recoverySourceInventory struct {
	env    Environment
	stamps map[string]recoveryFileStamp
	// appendable names observed regular files whose content belongs to a work
	// item's own source check.
	appendable map[string]bool
	complete   bool
	// resolvedKnown holds the resolved paths of observed regular files, built
	// on first use by known.
	resolvedKnown map[string]bool
}

const recoverySourceObservationLimit = 65536

func newRecoverySourceInventory(env Environment) *recoverySourceInventory {
	return &recoverySourceInventory{env: env, stamps: map[string]recoveryFileStamp{}, appendable: map[string]bool{}, complete: true}
}

// ownContent hands content changes of an observed transcript to the work item
// whose header came from source. Only the same regular file qualifies; every
// other path keeps the full size and modification-time comparison.
func (i *recoverySourceInventory) ownContent(path string, source fs.FileInfo) {
	stamp, ok := i.stamps[path]
	if !ok || stamp.absent || source == nil || !stamp.info.Mode().IsRegular() || !source.Mode().IsRegular() || !os.SameFile(stamp.info, source) {
		return
	}
	i.appendable[path] = true
}

func (i *recoverySourceInventory) observe(path string) {
	if _, ok := i.stamps[path]; ok {
		return
	}
	if len(i.stamps) >= recoverySourceObservationLimit {
		i.complete = false
		return
	}
	info, err := i.env.lstat(path)
	if os.IsNotExist(err) {
		i.stamps[path] = recoveryFileStamp{absent: true}
	} else if err != nil {
		i.complete = false
	} else {
		i.stamps[path] = recoveryFileStamp{info: info}
	}
}

func (i *recoverySourceInventory) environment() Environment {
	env := i.env
	env.ReadDir = func(path string) ([]fs.DirEntry, error) {
		i.observe(path)
		return i.env.readDir(path)
	}
	env.Lstat = func(path string) (fs.FileInfo, error) {
		i.observe(path)
		return i.env.lstat(path)
	}
	return env
}

// compare renews every observation. ok is false for a change that can remove
// or replace evidence: a missing, replaced or rewritten observed path, a mode
// change, an incomplete baseline, or an ended context. grown reports the only
// other change, a directory whose own stamp moved or an absent path that now
// exists: membership that may have been added to. Additions are judged by
// rediscovery (addedSources); a removed entry that was never observed was
// never evidence, and a removed observed entry fails its own stamp.
func (i *recoverySourceInventory) compare(ctx context.Context) (grown, ok bool) {
	if !i.complete || ctx.Err() != nil {
		return false, false
	}
	paths := make([]string, 0, len(i.stamps))
	for path := range i.stamps {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if ctx.Err() != nil {
			return false, false
		}
		before := i.stamps[path]
		info, err := i.env.lstat(path)
		switch {
		case before.absent && os.IsNotExist(err):
		case before.absent && err == nil:
			grown = true
		case err != nil || before.absent:
			return false, false
		case sameRecoveryDirectory(before.info, info):
			grown = grown || !sameRecoveryMember(before.info, info, false)
		case !sameRecoveryMember(before.info, info, i.appendable[path]):
			return false, false
		}
	}
	return grown, ctx.Err() == nil
}

// sameRecoveryDirectory reports the same directory, whatever its entries.
func sameRecoveryDirectory(before, after fs.FileInfo) bool {
	return before.IsDir() && after.IsDir() && os.SameFile(before, after) && before.Mode() == after.Mode()
}

// known reports a regular file observed at the baseline; compare keeps it the
// same file.
func (i *recoverySourceInventory) known(path string) bool {
	if stamp, ok := i.stamps[path]; ok {
		return !stamp.absent && stamp.info.Mode().IsRegular()
	}
	// Another spelling of an observed file (a symlinked home or store) is
	// still that file, not an added one.
	if i.resolvedKnown == nil {
		i.resolvedKnown = map[string]bool{}
		for p, stamp := range i.stamps {
			if !stamp.absent && stamp.info.Mode().IsRegular() {
				i.resolvedKnown[i.env.resolved(p)] = true
			}
		}
	}
	return i.resolvedKnown[i.env.resolved(path)]
}

// sameRecoveryMember reports whether a path still names the observed member.
// An appendable transcript may grow; a shrink or a same-size content change is
// a rewrite, not an append.
func sameRecoveryMember(before, after fs.FileInfo, appendable bool) bool {
	if !os.SameFile(before, after) || before.Mode() != after.Mode() {
		return false
	}
	if appendable && after.Size() > before.Size() {
		return true
	}
	return before.Size() == after.Size() && before.ModTime().Equal(after.ModTime())
}

// recoveryOwnershipCurrent renews all discovered cwd classifications, including
// plain folders and absent paths that could acquire a live checkout. Git identity
// validation still belongs to the separate bounded repository inventory.
func recoveryOwnershipCurrent(r *resolver, items []*work) func(context.Context) bool {
	type ownershipFact struct {
		path             string
		res              resolution
		exists           bool
		workspaceCurrent func(context.Context) bool
	}
	facts := make([]ownershipFact, 0, min(len(items), recoverySourceObservationLimit))
	complete := len(items) <= recoverySourceObservationLimit
	for _, w := range items[:min(len(items), recoverySourceObservationLimit)] {
		path := w.t.cwd
		if path == "" {
			path = w.res.root
		}
		if path != "" {
			facts = append(facts, ownershipFact{path: path, res: w.res, exists: r.env.exists(path), workspaceCurrent: w.workspaceCurrent})
		}
	}
	return func(ctx context.Context) bool {
		if !complete || ctx.Err() != nil {
			return false
		}
		ownership := newResolver(r.env, r.cfg, r.filters)
		for _, fact := range facts {
			if ctx.Err() != nil || r.env.exists(fact.path) != fact.exists {
				return false
			}
			fresh := ownership.resolve(fact.path)
			if fresh.root != fact.res.root || fresh.kind != fact.res.kind || fresh.skip != fact.res.skip {
				return false
			}
			if fact.workspaceCurrent != nil && !fact.workspaceCurrent(ctx) {
				return false
			}
		}
		return ctx.Err() == nil
	}
}
