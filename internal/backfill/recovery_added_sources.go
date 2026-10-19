package backfill

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/discoveryio"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
)

// Native transcripts added while a plan is made or confirmed. Running agents
// create new transcripts all the time: a review agent in a temporary clone
// starts a rollout every minute. A new transcript can only add evidence; it
// cannot take away a clone another transcript named. So an inventory whose
// only change is added membership (recoverySourceInventory.compare) does not
// fail every recorded recovery. The added transcripts are found again with
// the apps' own discovery and header readers, within a bounded count and
// byte budget, without listing the stores again in full (addedListing), and
// each one is judged by the clone root it names
// (dev/specs/backfill.md, "Recorded project recovery"):
//
//   - A transcript whose folder is not an existing, unconfigured repository
//     (a configured project, a plain folder without a Git marker, a deleted folder)
//     names no clone that could compete with a recorded key, and is ignored.
//   - One in a root the witness inventory already holds adds nothing.
//   - One naming a new root makes that root evidence: while planning, it joins
//     the witness inventory, so the recovery resolver judges the root's key;
//     at confirmation and admission, the root's repository key is read and
//     every recovery of that key fails.
//   - One whose folder cannot be told (no folder, a header that is not
//     understood, a Cursor project folder no planned chat had, a repository
//     key that cannot be read), and a store no longer listed in full, is a gap
//     in its app's evidence and blocks that app's recoveries, as any other
//     per-app gap does.
//
// Removed, replaced or rewritten observed paths, and more added transcripts
// than the budget allows, stay strict: every recovery fails.

// recoveryAddedSourceLimit bounds how many added transcripts one renewal
// reads headers of; recoveryAddedHeaderBytes bounds the bytes those reads
// take together. Past either, the renewal fails every recovery. Tests lower
// them.
var (
	recoveryAddedSourceLimit       = 64
	recoveryAddedHeaderBytes int64 = 16 << 20
)

// errAddedSourceBudget ends a header read past the added-transcript budget.
var errAddedSourceBudget = errors.New("added native transcript budget exhausted")

// addedSources are the transcripts found since the inventory baseline.
type addedSources struct {
	candidates []agentapi.DiscoveryCandidate
	// incomplete names the apps whose stores could not be listed again in
	// full; what they hold now is unknown.
	incomplete map[string]bool
}

// added reports what was added to the inventory's membership. ok is false
// for any other change (see compare) and when the added transcripts could
// not all be read within the budget.
func (i *recoverySourceInventory) added(ctx context.Context) (addedSources, bool) {
	grown, ok := i.compare(ctx)
	if !ok {
		return addedSources{}, false
	}
	if !grown {
		return addedSources{}, true
	}
	return i.rediscover(ctx)
}

// rediscover runs every app's own discovery again over a view of its stores
// that holds only what may have been added (addedListing), and reads headers
// only of transcripts the baseline did not observe.
func (i *recoverySourceInventory) rediscover(ctx context.Context) (addedSources, bool) {
	env := i.env
	if env.Discovery == nil {
		return addedSources{}, false
	}
	out := addedSources{incomplete: map[string]bool{}}
	budget := &addedHeaderBudget{files: recoveryAddedSourceLimit, bytes: recoveryAddedHeaderBytes, seen: map[string]bool{}}
	files := discoveryFiles{env}
	listing := i.addedListing(ctx)
	for _, name := range env.Discovery.DiscoveryAgents() {
		provider, _ := env.Discovery.LookupDiscovery(name)
		if provider == nil {
			return addedSources{}, false
		}
		scan := func(path string, visit func([]byte) bool) error {
			if i.known(path) {
				return nil // its header is the baseline's; compare kept the file
			}
			return budget.scan(ctx, files, path, visit)
		}
		report, err := provider.Discover(ctx, agentapi.DiscoveryRequest{Purpose: agentapi.DiscoveryImport, Stage: agentapi.DiscoveryIdentities, Locations: agentapi.NativeLocations{UserHome: env.Home, Directories: env.nativeDirectories(name)}, Files: listing, HeaderBytes: headScanLimit, RecordBytes: headLineLimit, Scan: scan}, func(c agentapi.DiscoveryCandidate) error {
			if !i.known(c.Source.Path) {
				if err := budget.admit(c.Source.Path); err != nil {
					return err
				}
				out.candidates = append(out.candidates, c)
			}
			return nil
		})
		if err != nil || budget.exhausted || listing.exhausted || ctx.Err() != nil {
			return addedSources{}, false
		}
		if report.StoreUnreadable || report.UnreadableFolders > 0 || report.Incomplete {
			out.incomplete[name] = true
		}
	}
	return out, true
}

// recoveryAddedEntryLimit bounds the directory entries one renewal lists
// that the baseline did not: new entries of a directory whose stamp moved,
// and every entry of a directory the baseline did not list (a new day
// folder, a new store). Past it, the renewal fails every recovery. Tests
// lower it.
var recoveryAddedEntryLimit = 4096

// addedListing is the stores' view for rediscover. The apps' discovery walks
// it exactly as it walks the stores, except:
//
//   - A directory listed at the baseline whose stamp has not moved lists only
//     its entries on the way to a path that changed: a directory whose stamp
//     moved, an absent path that now exists, or a directory the baseline did
//     not list in full. Its own entries are the baseline's (adding or
//     removing one moves its stamp), and its unchanged subdirectories hold
//     nothing new, so the rest of the store is not walked again.
//   - Every other directory is listed in full, as discovery lists it, so an
//     app's choices within a folder (Cursor's JSONL over text) stay the same
//     and a folder that cannot be listed leaves the app's store incomplete.
//   - A path compare renewed is not looked up again.
//
// The work is one listing per changed directory and per directory on the way
// to one, plus the entries the baseline did not list, at most
// recoveryAddedEntryLimit. One difference from a full rediscovery remains: a
// transcript added under another of an app's roots with the name of one it
// already holds (a Codex rollout copied to archived_sessions) is judged by
// its own header, where discovery would have kept only the first. It can
// only add evidence, never hide it.
func (i *recoverySourceInventory) addedListing(ctx context.Context) *addedListingFiles {
	relist := make(map[string]bool, len(i.moved))
	for _, path := range i.moved {
		relist[path] = true
	}
	for path, stamp := range i.stamps {
		if _, listed := i.names[path]; !listed && !stamp.absent && stamp.info.IsDir() {
			relist[path] = true
		}
	}
	branches := map[string]map[string]bool{}
	for path := range relist {
		for child, parent := path, filepath.Dir(path); parent != child; child, parent = parent, filepath.Dir(parent) {
			name := filepath.Base(child)
			if branches[parent][name] {
				break
			}
			if branches[parent] == nil {
				branches[parent] = map[string]bool{}
			}
			branches[parent][name] = true
		}
	}
	return &addedListingFiles{ctx: ctx, i: i, files: discoveryFiles{i.env}, relist: relist, branches: branches, entries: recoveryAddedEntryLimit}
}

// addedListingFiles is the view addedListing describes.
type addedListingFiles struct {
	ctx      context.Context
	i        *recoverySourceInventory
	files    discoveryFiles
	relist   map[string]bool
	branches map[string]map[string]bool
	// entries is what is left of recoveryAddedEntryLimit.
	entries   int
	exhausted bool
}

func (f *addedListingFiles) ReadDir(path string) ([]fs.DirEntry, error) {
	if err := f.ctx.Err(); err != nil {
		return nil, err
	}
	names, listed := f.i.names[path]
	if listed && !f.relist[path] {
		return f.branchEntries(path)
	}
	entries, err := f.files.ReadDir(path)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if names[e.Name()] {
			continue
		}
		if f.entries--; f.entries < 0 {
			f.exhausted = true
			return nil, errAddedSourceBudget
		}
	}
	return entries, nil
}

// branchEntries lists an unchanged directory's entries on the way to a
// changed path.
func (f *addedListingFiles) branchEntries(path string) ([]fs.DirEntry, error) {
	names := make([]string, 0, len(f.branches[path]))
	for name := range f.branches[path] {
		names = append(names, name)
	}
	sort.Strings(names)
	entries := make([]fs.DirEntry, 0, len(names))
	for _, name := range names {
		info, err := f.Lstat(filepath.Join(path, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		entries = append(entries, fs.FileInfoToDirEntry(info))
	}
	return entries, nil
}

func (f *addedListingFiles) Lstat(path string) (fs.FileInfo, error) {
	if stamp, ok := f.i.stamps[path]; ok && !stamp.absent {
		if info, ok := f.i.renewed[path]; ok {
			return info, nil
		}
	}
	return f.files.Lstat(path)
}

func (f *addedListingFiles) Open(path string) (io.ReadCloser, error) { return f.files.Open(path) }

// addedHeaderBudget bounds the header reads of added transcripts.
type addedHeaderBudget struct {
	files     int
	bytes     int64
	exhausted bool
	seen      map[string]bool
}

// admit charges each added transcript path once, whether discovery scans its
// header or emits its identity directly. Cursor file discovery selects one
// physical transcript per chat; SQLite identities belong to the separate epoch.
func (b *addedHeaderBudget) admit(path string) error {
	if b.seen[path] {
		return nil
	}
	if b.files <= 0 {
		b.exhausted = true
		return errAddedSourceBudget
	}
	b.files--
	if b.seen == nil {
		b.seen = map[string]bool{}
	}
	b.seen[path] = true
	return nil
}

// scan reads one added transcript's header with discovery's own bounds, cut
// to what is left of the budget. A read the budget cut short exhausts it:
// a shortened header could leave out the folder it would have named.
func (b *addedHeaderBudget) scan(ctx context.Context, files agentapi.DiscoveryFiles, path string, visit func([]byte) bool) error {
	if err := b.admit(path); err != nil {
		return err
	}
	limit := min(int64(headScanLimit), b.bytes)
	counted := &countingFiles{DiscoveryFiles: files}
	err := discoveryio.ScanRecords(ctx, counted, path, limit, headLineLimit, visit)
	b.bytes -= counted.n
	if limit < headScanLimit && counted.n >= limit {
		b.exhausted = true
	}
	return err
}

// countingFiles counts the bytes read through Open.
type countingFiles struct {
	agentapi.DiscoveryFiles
	n int64
}

func (f *countingFiles) Open(path string) (io.ReadCloser, error) {
	rc, err := f.DiscoveryFiles.Open(path)
	if err != nil {
		return nil, err
	}
	return &countingReader{ReadCloser: rc, n: &f.n}, nil
}

type countingReader struct {
	io.ReadCloser
	n *int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	*r.n += int64(n)
	return n, err
}

// addedSourceRoot is the clone root an added transcript names. unknown is
// set when its folder cannot be told; relevant when it names an existing,
// unconfigured repository root, the only kind the witness inventory holds.
type addedSourceRoot struct {
	agent    string
	res      resolution
	unknown  bool
	relevant bool
}

// addedSourceSlugs are the Cursor project folders planned chats were found
// in, by app: a new chat in one of them names the folder they already name.
func addedSourceSlugs(items []*work) map[string]bool {
	slugs := map[string]bool{}
	for _, w := range items {
		if w.t.cursorSlug != "" {
			slugs[string(w.t.harness)+"\x00"+w.t.cursorSlug] = true
		}
	}
	return slugs
}

// classifyAddedSource resolves the root an added transcript names with
// ownership (committed configuration only), as discovery's own items are.
func classifyAddedSource(ownership *resolver, slugs map[string]bool, c agentapi.DiscoveryCandidate) addedSourceRoot {
	out := addedSourceRoot{agent: string(c.Session.Agent)}
	if c.WorkspaceKey != "" {
		out.unknown = !slugs[out.agent+"\x00"+c.WorkspaceKey]
		return out
	}
	if c.IdentityError != nil || c.Header.IdentityMismatch {
		out.unknown = true
		return out
	}
	out.res = ownership.resolve(c.Header.Directory)
	if out.res.skip == SkipProjectUnknown {
		out.unknown = true
		return out
	}
	configured := false
	for _, p := range ownership.cfg.Archive.Projects {
		configured = configured || ownership.env.resolved(p.Root) == ownership.env.resolved(out.res.root)
	}
	out.relevant = ownership.recoveryRepositoryWitness(out.res) && ownership.env.exists(out.res.root) && !configured
	return out
}

// addedWitnesses turns transcripts added during planning into evidence-only
// witnesses for the inventory, and their unknown folders into per-app gaps.
func addedWitnesses(r *resolver, items []*work, added addedSources) ([]*work, recoveryGaps) {
	var gaps recoveryGaps
	for agent := range added.incomplete {
		gaps.add(agent, CauseNativeInventoryChanged)
	}
	slugs := addedSourceSlugs(items)
	var out []*work
	for _, c := range added.candidates {
		root := classifyAddedSource(r, slugs, c)
		switch {
		case root.unknown:
			gaps.add(root.agent, CauseNativeInventoryChanged)
		case root.relevant:
			t := &transcript{harness: harness(root.agent), path: c.Source.Path, size: c.Bytes, nativeID: c.Session.NativeID, cwd: c.Header.Directory, repoKey: c.Header.RepoKey, capturePending: c.Header.CapturePending != "", sourceInfo: c.SourceInfo}
			out = append(out, &work{t: t, res: root.res, evidenceOnly: true, addedSource: true})
		}
	}
	return out, gaps
}

// addedRecoveryBlocks judges transcripts added after planning against the
// planned witness roots. keys are the repository keys of new clone roots:
// a recovery of one of them is no longer unique. gaps are the apps whose
// added evidence could not be told.
func addedRecoveryBlocks(ctx context.Context, r *resolver, slugs map[string]bool, witnesses map[string][]*work, added addedSources) (keys map[string]bool, gaps recoveryGaps) {
	keys = map[string]bool{}
	for agent := range added.incomplete {
		gaps.add(agent, CauseNativeInventoryChanged)
	}
	ownership := newResolver(r.env, r.cfg, r.filters)
	looked := map[string]bool{}
	for _, c := range added.candidates {
		root := classifyAddedSource(ownership, slugs, c)
		if root.unknown {
			gaps.add(root.agent, CauseNativeInventoryChanged)
			continue
		}
		if !root.relevant || len(witnesses[root.res.root]) > 0 || looked[root.res.root] {
			continue
		}
		looked[root.res.root] = true
		if r.recovery != nil {
			// Added roots share the recovery's bounded Git lookups.
			if r.recovery.Operations >= r.recovery.MaxOperations {
				gaps.add(root.agent, CauseRecoveryBudget)
				continue
			}
			r.recovery.Operations++
		}
		var id sourcefacts.RepositoryIdentity
		if r.env.RepositoryIdentity != nil {
			id = r.env.RepositoryIdentity(ctx, root.res.root)
		}
		switch {
		case id.Known && id.Key != "":
			keys[id.Key] = true
		case id.Known:
			// A repository without a key matches no recorded key.
		default:
			gaps.add(root.agent, CauseNativeInventoryChanged)
		}
	}
	return keys, gaps
}

// addedAllows reports whether transcripts added after planning leave a
// recovery of a session of h current. Only a recorded-key recovery depends
// on the clone roots transcripts name.
func addedAllows(keys map[string]bool, gaps recoveryGaps, h harness, proof archive.ProjectResolution) bool {
	if proof.Method != "recorded_repository" {
		return true
	}
	if _, blocked := gaps.blocking(h); blocked {
		return false
	}
	return !keys[proof.RecordedRepoKey]
}

// addedSourceGrown renews a witness from an added transcript that its own
// agent kept appending to: the header it was judged by cannot change.
func (w *work) addedSourceGrown(env Environment) bool {
	if !w.addedSource || w.t.sourceInfo == nil {
		return false
	}
	current, err := env.lstat(w.t.path)
	return appendedSource(w.t.sourceInfo, current, err)
}
