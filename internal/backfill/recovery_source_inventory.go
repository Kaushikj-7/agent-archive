package backfill

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
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
	// targets holds, for each listed path that is a symlink (a symlinked
	// store), what it names, taken before the listing. The link's own stamp
	// does not move when the directory gains entries.
	targets map[string]recoveryTarget
	// appendable names observed regular files whose content belongs to a work
	// item's own source check.
	appendable map[string]bool
	complete   bool
	// names holds the entry names of each directory listed at the baseline,
	// at most recoverySourceObservationLimit names in all. A directory whose
	// listing failed or did not fit has none and is listed again in full when
	// membership grew (addedListing).
	names     map[string]map[string]bool
	nameCount int
	// renewed and moved are what the last compare saw: the renewed
	// observation of each existing stamped path, and the directories whose
	// stamp moved and absent paths that now exist.
	renewed map[string]fs.FileInfo
	moved   []string
	// byBase indexes observed regular files by base name, built on first use
	// by known.
	byBase map[string][]string
}

const recoverySourceObservationLimit = 65536

func newRecoverySourceInventory(env Environment) *recoverySourceInventory {
	return &recoverySourceInventory{env: env, stamps: map[string]recoveryFileStamp{}, targets: map[string]recoveryTarget{}, appendable: map[string]bool{}, names: map[string]map[string]bool{}, complete: true}
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
		i.observeTarget(path)
		entries, err := i.env.readDir(path)
		if err == nil {
			i.recordNames(path, entries)
		}
		return entries, err
	}
	env.Lstat = func(path string) (fs.FileInfo, error) {
		i.observe(path)
		return i.env.lstat(path)
	}
	return env
}

// observeTarget stamps what a listed symlink names, before the listing, so
// entries added to a symlinked store move a stamp.
func (i *recoverySourceInventory) observeTarget(path string) {
	stamp, ok := i.stamps[path]
	if _, done := i.targets[path]; done || !ok || stamp.absent || stamp.info.Mode()&fs.ModeSymlink == 0 {
		return
	}
	if len(i.stamps)+len(i.targets) >= recoverySourceObservationLimit {
		i.complete = false
		return
	}
	i.targets[path] = observeRecoveryTarget(i.env.stat(path))
}

// recoveryTarget is what a listed symlink names. Only a directory is listed
// through the link; a missing target, a file and one that cannot be reached
// (a link loop, a folder without search permission) are listed as nothing,
// or as an unreadable store, an app's own gap rather than a strict failure.
type recoveryTarget struct {
	info fs.FileInfo
	kind recoveryTargetKind
}

type recoveryTargetKind int

const (
	recoveryTargetAbsent recoveryTargetKind = iota
	recoveryTargetUnreadable
	recoveryTargetOther
	recoveryTargetDirectory
)

func observeRecoveryTarget(info fs.FileInfo, err error) recoveryTarget {
	switch {
	case os.IsNotExist(err):
		return recoveryTarget{kind: recoveryTargetAbsent}
	case err != nil:
		return recoveryTarget{kind: recoveryTargetUnreadable}
	case info.IsDir():
		return recoveryTarget{info: info, kind: recoveryTargetDirectory}
	}
	return recoveryTarget{info: info, kind: recoveryTargetOther}
}

// recordNames keeps a baseline listing's entry names within the observation
// limit. Only the first listing of a directory counts: its stamp was taken
// before it, so a later addition moves the stamp.
func (i *recoverySourceInventory) recordNames(path string, entries []fs.DirEntry) {
	if _, ok := i.names[path]; ok || i.nameCount+len(entries) > recoverySourceObservationLimit {
		return
	}
	if _, ok := i.stamps[path]; !ok {
		return
	}
	names := make(map[string]bool, len(entries))
	for _, e := range entries {
		names[e.Name()] = true
	}
	i.names[path] = names
	i.nameCount += len(entries)
}

// compare renews every observation. ok is false for a change that can remove
// or replace evidence: a missing, replaced or rewritten observed path, a mode
// change, an incomplete baseline, or an ended context. grown reports the only
// other change, a directory whose own stamp moved or an absent path that now
// exists: membership that may have been added to. compare keeps those paths
// (moved) and the renewed observations (renewed) for rediscovery, which
// judges the additions (addedSources); a removed entry that was never
// observed was never evidence, and a removed observed entry fails its own
// stamp.
func (i *recoverySourceInventory) compare(ctx context.Context) (grown, ok bool) {
	if !i.complete || ctx.Err() != nil {
		return false, false
	}
	paths := make([]string, 0, len(i.stamps))
	for path := range i.stamps {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	i.renewed, i.moved = make(map[string]fs.FileInfo, len(paths)), nil
	for _, path := range paths {
		if ctx.Err() != nil {
			return false, false
		}
		before := i.stamps[path]
		info, err := i.env.lstat(path)
		switch {
		case before.absent && os.IsNotExist(err):
		case before.absent && err == nil:
			i.moved = append(i.moved, path)
		case err != nil || before.absent:
			return false, false
		case sameRecoveryDirectory(before.info, info):
			if !sameRecoveryMember(before.info, info, false) {
				i.moved = append(i.moved, path)
			}
		case !sameRecoveryMember(before.info, info, i.appendable[path]):
			return false, false
		}
		if err == nil {
			i.renewed[path] = info
		}
	}
	targets := make([]string, 0, len(i.targets))
	for path := range i.targets {
		targets = append(targets, path)
	}
	sort.Strings(targets)
	for _, path := range targets {
		if ctx.Err() != nil {
			return false, false
		}
		// The link itself is the same link (above). The directory it named
		// is listed again when its stamp moved, and must remain that
		// directory. Anything else it named was listed as nothing or as
		// unreadable; it is listed again when it became something else.
		before, after := i.targets[path], observeRecoveryTarget(i.env.stat(path))
		switch {
		case before.kind != recoveryTargetDirectory:
			if after.kind != before.kind {
				i.moved = append(i.moved, path)
			}
		case after.kind != recoveryTargetDirectory || !sameRecoveryDirectory(before.info, after.info):
			return false, false
		case !sameRecoveryMember(before.info, after.info, false):
			i.moved = append(i.moved, path)
		}
	}
	return len(i.moved) > 0, ctx.Err() == nil
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
	// still that file, not an added one. Such a spelling keeps the file's
	// name, so only observed files of the same name are compared, by file
	// identity: one Lstat per added path, no symlink resolution.
	if i.byBase == nil {
		i.byBase = map[string][]string{}
		for p, stamp := range i.stamps {
			if !stamp.absent && stamp.info.Mode().IsRegular() {
				i.byBase[filepath.Base(p)] = append(i.byBase[filepath.Base(p)], p)
			}
		}
	}
	same := i.byBase[filepath.Base(path)]
	if len(same) == 0 {
		return false
	}
	info, err := i.env.lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	for _, p := range same {
		if os.SameFile(i.stamps[p].info, info) {
			return true
		}
	}
	return false
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
