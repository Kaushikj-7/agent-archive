package discovery

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/codexmeta"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"github.com/wangjohn/agent-archive/internal/state"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const maxCurrentThreads = 64

// maxCachedThreads bounds the thread sets one pass keeps for Check.
const maxCachedThreads = 256

// CodexRolloutLookup is one serial collector-pass view of validated native facts.
// Neither the SQLite current locator nor a cached identity grants admission.
// A thread's lineage is the chain its current rollout names through each
// rollout's history base (see Rollout); no observation proves that a thread
// has no other rollouts, and none needs to.
type CodexRolloutLookup struct {
	metadata   *metadataInventory
	homes      []string
	store      *state.Store
	catalog    *catalog
	readOnly   bool
	readBudget *agentapi.NativeReadBudget
	// dirty records a discovery checkpoint handed to Close (see
	// deferCheckpoint), which writes it without the pass deadline.
	dirty            bool
	roots            []string
	deadline         time.Time
	remaining        time.Duration
	observationBytes int64
	catalogCharge    int64
	restoredCharge   int64
	observations     map[string]rolloutObservation
	byThread         map[string]map[string]struct{}
	byPhysical       map[string]map[string]struct{}
	threads          map[string]agentapi.CodexRolloutSet
	indexes          map[string]*currentIndexView
	probes           int
	closed           bool
	threadOrder      []string
	queries          int
	listings         map[string]map[string][]string
	listingBytes     int64
}

type rolloutObservation struct {
	source   SourceDescriptor
	stamp    Fingerprint
	identity codexmeta.CodexIdentity
}

type currentIndexView struct {
	snapshot    *privateIndex
	db          *sql.DB
	refreshed   bool
	unavailable bool
	stamps      []os.FileInfo
}

// NewCodexRolloutLookup composes existing catalog and registrations lazily with
// targeted current-row evidence. It does no native enumeration or SQL at setup.
func NewCodexRolloutLookup(ctx context.Context, store *state.Store, homes []string) (*CodexRolloutLookup, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := sweepPrivateIndexes(ctx, indexSnapshotRoot(os.TempDir()), time.Now()); err != nil {
		return nil, agentapi.Wrap(agentapi.Cleanup, err)
	}
	roots := approvedRoots(homes)
	if len(roots) > 1024 {
		return nil, agentapi.Wrap(agentapi.Limit, errors.New("native home limit"))
	}
	lookup := &CodexRolloutLookup{store: store, homes: slices.Clone(homes), readBudget: agentapi.NewNativeReadBudget(agentapi.PassReadBudget), roots: roots, deadline: time.Now().Add(Budget), remaining: Budget, observations: map[string]rolloutObservation{}, byThread: map[string]map[string]struct{}{}, byPhysical: map[string]map[string]struct{}{}, threads: map[string]agentapi.CodexRolloutSet{}, indexes: map[string]*currentIndexView{}}
	var prior catalog
	if err := lookup.readCatalog(ctx, filepath.Join(store.Home(), "discovery-catalog.json"), &prior); err != nil && !errors.Is(err, os.ErrNotExist) {
		if errors.Is(err, agentapi.ErrReadBudget) || ctx.Err() != nil {
			return nil, errors.Join(err, lookup.closeWithContext(ctx))
		}
		return lookup, nil
	}
	if catalogNeedsReset(prior, roots) {
		lookup.releaseRestored()
		return lookup, nil
	}
	prior.reclassify()
	for path, entry := range prior.Cache {
		root := lookup.homeFor(path)
		if root == "" || entry.Observation.Identity == nil {
			continue
		}
		lookup.Observe(SourceDescriptor{Kind: archive.SourceKindFile, Root: root, Locator: path, StableKey: entry.Observation.Identity.RolloutID}, Fingerprint{Size: entry.Size, Mtime: entry.Mtime}, entry.Observation.Identity)
	}
	lookup.catalog = &prior
	return lookup, nil
}

// Observe accepts already validated pass-owned metadata facts. It deep-copies
// them so caller mutations cannot silently alter a lookup revision token.
// Restored cache facts can name a locator that no longer exists; every
// consumer reopens and rechecks a locator before using it.
func (l *CodexRolloutLookup) Observe(source SourceDescriptor, stamp Fingerprint, identity *codexmeta.CodexIdentity) {
	if l.closed || identity == nil || identity.ThreadID == "" || identity.RolloutID == "" || source.Kind != archive.SourceKindFile || l.homeFor(source.Locator) != source.Root {
		return
	}
	if identityByteBound(*identity) > 4096 {
		return
	}
	copyID := cloneRolloutIdentity(*identity)

	prior, present := l.observations[source.Locator]
	if !present && len(l.observations) >= maxCatalog {
		return
	}
	size := rolloutFactByteBound(source, copyID)
	oldSize := int64(0)
	if present {
		oldSize = rolloutFactByteBound(prior.source, prior.identity)
	}
	if l.observationBytes+size-oldSize > maxObservationBytes {
		return
	}
	if size > oldSize && !l.readBudget.ReserveUnscoped(size-oldSize) {
		return
	}
	if oldSize > size {
		l.readBudget.ReleaseUnscoped(oldSize - size)
	}
	l.observationBytes += size - oldSize
	if present {
		delete(l.byThread[prior.identity.ThreadID], source.Locator)
		delete(l.byPhysical[prior.identity.RolloutID], source.Locator)
		if len(l.byThread[prior.identity.ThreadID]) == 0 {
			delete(l.byThread, prior.identity.ThreadID)
		}
		if len(l.byPhysical[prior.identity.RolloutID]) == 0 {
			delete(l.byPhysical, prior.identity.RolloutID)
		}
	}
	l.observations[source.Locator] = rolloutObservation{source: source, stamp: stamp, identity: copyID}
	if l.byThread[copyID.ThreadID] == nil {
		l.byThread[copyID.ThreadID] = map[string]struct{}{}
	}
	l.byThread[copyID.ThreadID][source.Locator] = struct{}{}
	if l.byPhysical[copyID.RolloutID] == nil {
		l.byPhysical[copyID.RolloutID] = map[string]struct{}{}
	}
	l.byPhysical[copyID.RolloutID][source.Locator] = struct{}{}
}

// Strings are immutable; copy the optional ordinal/base values so pass-owned
// observations cannot be changed by their caller. Avoid a JSON round trip for
// every restored catalog entry on every pass.
func cloneRolloutIdentity(id codexmeta.CodexIdentity) codexmeta.CodexIdentity {
	if id.ForkOrdinal != nil {
		value := *id.ForkOrdinal
		id.ForkOrdinal = &value
	}
	if id.SubagentOrdinal != nil {
		value := *id.SubagentOrdinal
		id.SubagentOrdinal = &value
	}
	if id.HistoryBase != nil {
		value := *id.HistoryBase
		id.HistoryBase = &value
	}
	return id
}

func (l *CodexRolloutLookup) homeFor(path string) string {
	if !filepath.IsAbs(path) || len(path) > 4096 || filepath.Clean(path) != path {
		return ""
	}
	for _, root := range l.roots {
		if local.PathWithin(path, filepath.Join(root, "sessions")) || local.PathWithin(path, filepath.Join(root, "archived_sessions")) {
			return root
		}
	}
	return ""
}

func (l *CodexRolloutLookup) checkBudget(ctx context.Context) error {
	if l.closed {
		return agentapi.Wrap(agentapi.Unavailable, agentapi.ErrClosed)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if l.remaining <= 0 {
		return allowanceSpent("native lookup pass deadline")
	}
	return nil
}

// allowanceSpent is the refusal of a lookup call past the pass's allowance:
// a limit of this pass, not of the source, which the next pass reads.
func allowanceSpent(what string) error {
	return agentapi.WithCause(agentapi.Wrap(agentapi.Limit, errors.New(what)), agentapi.CauseLookupAllowanceSpent)
}

// AllowanceSpent reports whether this pass's lookup time is used up, so that
// every further Rollout, Thread and Check call fails (see
// agentapi.CodexRolloutAllowance).
func (l *CodexRolloutLookup) AllowanceSpent() bool { return !l.closed && l.remaining <= 0 }

// BeginCollection gives a collector pass its own lookup allowance, a fresh
// Budget of lookup time and HeaderProbes header probes, separate from what
// discovery (or import registration) spent of the same lookup before it.
// Discovery takes as much of Budget as it can use, so without this a pass
// with a large discovery backlog left the collector none, and every Codex
// session failed. Observations and cached threads carry over: they are the
// same pass's validated facts, and every consumer rechecks a locator before
// using it.
func (l *CodexRolloutLookup) BeginCollection() {
	l.remaining = Budget
	l.probes = 0
}

func (l *CodexRolloutLookup) inspect(ctx context.Context, path, thread string) (*agentapi.SourceRef, error) {
	reservation := l.readBudget.Reservation()
	root := l.homeFor(path)
	if root == "" {
		return nil, agentapi.Wrap(agentapi.Unsafe, errors.New("current locator outside native home"))
	}
	if prior, present := l.observations[path]; present && prior.identity.ThreadID == thread {
		opened, err := sourcefacts.OpenRegular(root, path)
		if err == nil {
			info, statErr := opened.Stat()
			_ = opened.Close()
			if statErr == nil && prior.stamp.Size == info.Size() && prior.stamp.Mtime == info.ModTime().UnixNano() {
				return &agentapi.SourceRef{Kind: archive.SourceKindFile, Path: path, Key: thread}, nil
			}
		}
	}
	if l.probes >= HeaderProbes {
		return nil, allowanceSpent("native header probe limit")
	}
	const headerCharge = int64(sourcefacts.HeaderBytes + 128<<10)
	if !reservation.Reserve(headerCharge) {
		return nil, agentapi.Wrap(agentapi.Limit, errors.New("shared native header budget exhausted"))
	}
	defer reservation.Release(headerCharge)
	l.probes++
	header := sourcefacts.ReadHeader(ctx, root, path)
	if header.Identity == nil {
		return nil, agentapi.Wrap(agentapi.Unavailable, errors.New("current header unavailable"))
	}
	if header.Identity.ThreadID != thread {
		return nil, agentapi.Wrap(agentapi.Unsafe, errors.New("current thread mismatch"))
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, agentapi.Wrap(agentapi.Changed, err)
	}
	l.Observe(SourceDescriptor{Kind: archive.SourceKindFile, Root: root, Locator: path, StableKey: header.Identity.RolloutID}, Fingerprint{Size: info.Size(), Mtime: info.ModTime().UnixNano()}, header.Identity)
	return &agentapi.SourceRef{Kind: archive.SourceKindFile, Path: path, Key: thread}, nil
}

// Rollout returns the locators of one physical rollout, found by its ID: its
// observed locators that still exist, or else the rollout files named for it
// in the day directories its UUIDv7 creation time selects and in
// archived_sessions/. The source provider reopens each and checks its header
// identity. A Key is the observed thread, or empty for a locator found by
// name alone.
func (l *CodexRolloutLookup) Rollout(ctx context.Context, id string) (_ []agentapi.SourceRef, err error) {
	var done func(*error)
	ctx, done, err = l.beginOperation(ctx)
	if err != nil {
		return nil, err
	}
	defer done(&err)
	if err := l.checkBudget(ctx); err != nil {
		return nil, err
	}
	if id == "" || codexmeta.RolloutID(id+".jsonl") != id {
		return nil, agentapi.Wrap(agentapi.Unsafe, errors.New("invalid physical rollout"))
	}
	paths := map[string]string{}
	for path := range l.byPhysical[id] {
		if !rolloutPresent(path) {
			continue
		}
		if len(paths) >= archive.MaxHistorySpans {
			return nil, agentapi.Wrap(agentapi.Limit, errors.New("physical locator limit"))
		}
		paths[path] = l.observations[path].identity.ThreadID
	}
	if len(paths) == 0 {
		located, err := l.locate(ctx, id)
		if err != nil {
			return nil, err
		}
		for _, path := range located {
			if len(paths) >= archive.MaxHistorySpans {
				return nil, agentapi.Wrap(agentapi.Limit, errors.New("physical locator limit"))
			}
			paths[path] = ""
		}
	}
	refs := make([]agentapi.SourceRef, 0, len(paths))
	for path, thread := range paths {
		refs = append(refs, agentapi.SourceRef{Kind: archive.SourceKindFile, Path: path, Key: thread})
	}
	slices.SortFunc(refs, func(a, b agentapi.SourceRef) int { return strings.Compare(a.Path, b.Path) })
	return refs, nil
}

// Thread supplies the thread's current locator from Codex's own index and its
// bounded observed same-thread candidates.
func (l *CodexRolloutLookup) Thread(ctx context.Context, id string) (agentapi.CodexRolloutSet, error) {
	if codexmeta.RolloutID(id+".jsonl") != id || id == "" {
		return agentapi.CodexRolloutSet{}, agentapi.Wrap(agentapi.Unsafe, errors.New("invalid native thread"))
	}
	return l.thread(ctx, id)
}

// RegisteredThread retains current-row revalidation for the pre-native layout
// compatibility lane. Only actual unbound, non-discovery ownership qualifies.
func (l *CodexRolloutLookup) RegisteredThread(ctx context.Context, id string) (agentapi.CodexRolloutSet, error) {
	key, err := agentmeta.NewSessionKey("codex", id)
	if err != nil {
		return agentapi.CodexRolloutSet{}, agentapi.Wrap(agentapi.Unsafe, err)
	}
	archiveID, found, err := l.store.ArchiveSessionID(key)
	if err != nil || !found {
		return agentapi.CodexRolloutSet{}, agentapi.Wrap(agentapi.Unavailable, errors.New("legacy registration unavailable"))
	}
	reg, found, err := l.store.LoadRegistration(archiveID)
	if err != nil || !found || reg.CodexBinding != nil || reg.Origin == archive.SessionOriginDiscovery {
		return agentapi.CodexRolloutSet{}, agentapi.Wrap(agentapi.Unsafe, errors.New("legacy registration requires native selection"))
	}
	return l.thread(ctx, id)
}

func (l *CodexRolloutLookup) thread(ctx context.Context, id string) (_ agentapi.CodexRolloutSet, err error) {
	var done func(*error)
	ctx, done, err = l.beginOperation(ctx)
	if err != nil {
		return agentapi.CodexRolloutSet{}, err
	}
	defer done(&err)
	if err := l.checkBudget(ctx); err != nil {
		return agentapi.CodexRolloutSet{}, err
	}
	if cached, present := l.threads[id]; present {
		return l.withCandidates(ctx, id, cached)
	}
	l.registrationHint(ctx, id)
	var current *agentapi.SourceRef
	for _, root := range l.roots {
		view, err := l.index(ctx, root, false)
		if err != nil {
			if agentapi.Failure(err) == agentapi.Limit {
				return agentapi.CodexRolloutSet{}, err
			}
			continue
		}
		l.queries += 2 // bounded EXPLAIN and actual parameterized query
		path, found, err := currentLocator(ctx, view.db, id)
		if err != nil || !found {
			continue
		}
		if l.homeFor(path) != root {
			return agentapi.CodexRolloutSet{}, agentapi.Wrap(agentapi.Unsafe, errors.New("current locator belongs to another native home"))
		}
		if currentLocatorMissing(path) {
			continue
		}
		ref, err := l.inspect(ctx, path, id)
		if err != nil {
			return agentapi.CodexRolloutSet{}, err
		}
		if current != nil && current.Path != ref.Path {
			return agentapi.CodexRolloutSet{}, agentapi.Wrap(agentapi.Unavailable, errors.New("conflicting current locators"))
		}
		current = ref
	}
	set, err := l.withCandidates(ctx, id, agentapi.CodexRolloutSet{Current: current})
	if err != nil {
		return agentapi.CodexRolloutSet{}, err
	}
	if len(l.threads) >= maxCachedThreads {
		delete(l.threads, l.threadOrder[0])
		l.threadOrder = l.threadOrder[1:]
	}
	l.threads[id] = set
	l.threadOrder = append(l.threadOrder, id)
	return set, nil
}

func (l *CodexRolloutLookup) registrationHint(ctx context.Context, id string) {
	if ctx.Err() != nil {
		return
	}
	key, err := agentmeta.NewSessionKey("codex", id)
	if err != nil {
		return
	}
	archiveID, found, err := l.store.ArchiveSessionID(key)
	if err != nil || !found {
		return
	}
	reg, found, err := l.store.LoadRegistration(archiveID)
	if err != nil || !found {
		return
	}
	if l.homeFor(reg.TranscriptPath) != "" {
		_, _ = l.inspect(ctx, reg.TranscriptPath, id)
	}
}

// withCandidates adds the thread's observed same-thread rollouts to set and
// completes its revision, which covers them and their identity evidence, so
// a newly observed segment changes it. They are hints: without a current
// locator the source chooses a tip from them, and beside one it offers them
// as historical revisions; they never prove that no other rollout exists.
//
// Past archive.MaxHistorySpans observed rollouts, a thread without a current
// locator is refused: its tip is chosen from all of them. Beside a current
// locator the chain comes from walking history bases, not from these hints,
// so only the newest ones that still exist are kept, as best-effort
// historical revisions, and the rest are left out.
func (l *CodexRolloutLookup) withCandidates(ctx context.Context, id string, set agentapi.CodexRolloutSet) (agentapi.CodexRolloutSet, error) {
	paths := make([]string, 0, len(l.byThread[id]))
	for path := range l.byThread[id] {
		paths = append(paths, path)
	}
	if len(paths) > archive.MaxHistorySpans {
		if set.Current == nil {
			return agentapi.CodexRolloutSet{}, agentapi.Wrap(agentapi.Limit, errors.New("thread candidate limit"))
		}
		paths = slices.DeleteFunc(paths, func(path string) bool { return !rolloutPresent(path) })
		slices.SortFunc(paths, codexmeta.NewestRolloutFirst)
		paths = paths[:min(len(paths), archive.MaxHistorySpans)]
	}
	slices.Sort(paths)
	set.Candidates = nil
	for _, path := range paths {
		set.Candidates = append(set.Candidates, agentapi.SourceRef{Kind: archive.SourceKindFile, Path: path, Key: id})
	}
	value := struct {
		Current      *agentapi.SourceRef        `json:"Current"`
		Candidates   []agentapi.SourceRef       `json:"Candidates"`
		Observations []rolloutObservationDigest `json:"Observations"`
	}{Current: set.Current, Candidates: set.Candidates, Observations: l.candidateDigests(paths)}
	revision, err := l.revisionDigest(ctx, value)
	if err != nil {
		return agentapi.CodexRolloutSet{}, err
	}
	set.Revision = revision
	return set, nil
}

func (l *CodexRolloutLookup) revisionDigest(ctx context.Context, value any) (string, error) {
	reservation := l.readBudget.Reservation()
	const scratch = 32 << 10
	if !reservation.Reserve(scratch) {
		return "", errCatalogBudget
	}
	available := reservation.Available()
	n, err := agentmeta.JSONWireBound(ctx, value, available/2)
	if errors.Is(err, agentmeta.ErrJSONWireLimit) {
		reservation.Refuse(available + 2)
	}
	reservation.Release(scratch)
	if err != nil {
		return "", errors.Join(errCatalogBudget, err)
	}
	// The encoder buffer and returned immutable JSON coexist until hashing ends.
	if !reservation.Reserve(n + n) {
		return "", errCatalogBudget
	}
	defer reservation.Release(n + n)
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

type rolloutObservationDigest struct {
	Path     string                  `json:"Path"`
	Stamp    Fingerprint             `json:"Stamp"`
	Identity codexmeta.CodexIdentity `json:"Identity"`
}

// candidateDigests is the observed identity evidence of sorted paths.
func (l *CodexRolloutLookup) candidateDigests(paths []string) []rolloutObservationDigest {
	out := make([]rolloutObservationDigest, 0, len(paths))
	for _, path := range paths {
		o := l.observations[path]
		out = append(out, rolloutObservationDigest{path, o.stamp, o.identity})
	}
	return out
}

// Check revalidates selection within the pass. One private refresh per home is
// allowed; later same-generation appends replay into that existing private view.
func (l *CodexRolloutLookup) Check(ctx context.Context, id, revision string) (err error) {
	var done func(*error)
	ctx, done, err = l.beginOperation(ctx)
	if err != nil {
		return err
	}
	defer done(&err)
	if err := l.checkBudget(ctx); err != nil {
		return err
	}
	old, found := l.threads[id]
	if !found {
		return agentapi.Wrap(agentapi.Changed, errIndexChanged)
	}
	var current *agentapi.SourceRef
	for _, root := range l.roots {
		view, err := l.index(ctx, root, true)
		if err != nil {
			if old.Current != nil && l.homeFor(old.Current.Path) == root {
				return agentapi.Wrap(agentapi.Changed, err)
			}
			continue
		}
		l.queries += 2 // bounded EXPLAIN and actual parameterized query
		path, found, err := currentLocator(ctx, view.db, id)
		if err != nil {
			return agentapi.Wrap(agentapi.Changed, err)
		}
		if !found {
			continue
		}
		// The source provider checks opened header/content separately. Requerying
		// the same locator does not repeat its native header scan.
		ref := &agentapi.SourceRef{Kind: archive.SourceKindFile, Path: path, Key: id}
		if l.homeFor(path) != root {
			return agentapi.Wrap(agentapi.Unsafe, errIndexChanged)
		}
		if currentLocatorMissing(path) {
			continue
		}
		if current != nil && current.Path != ref.Path {
			return agentapi.Wrap(agentapi.Changed, errIndexChanged)
		}
		current = ref
	}
	now, err := l.withCandidates(ctx, id, agentapi.CodexRolloutSet{Current: current})
	if err != nil {
		return err
	}
	if now.Revision != revision {
		return agentapi.Wrap(agentapi.Changed, errIndexChanged)
	}
	return nil
}

func indexStamps(root string) []os.FileInfo {
	var out []os.FileInfo
	for _, suffix := range []string{"", "-wal"} {
		info, _ := os.Lstat(filepath.Join(root, "state_5.sqlite") + suffix)
		out = append(out, info)
	}
	return out
}

func equalIndexStamps(a, b []os.FileInfo) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] == nil || b[i] == nil {
			if a[i] != nil || b[i] != nil {
				return false
			}
			continue
		}
		if !os.SameFile(a[i], b[i]) || a[i].Size() != b[i].Size() || !a[i].ModTime().Equal(b[i].ModTime()) {
			return false
		}
	}
	return true
}

func (l *CodexRolloutLookup) index(ctx context.Context, root string, refresh bool) (*currentIndexView, error) {
	return l.indexDeadline(ctx, root, refresh, l.deadline)
}

func (l *CodexRolloutLookup) indexDeadline(ctx context.Context, root string, refresh bool, deadline time.Time) (*currentIndexView, error) {
	view := l.indexes[root]
	if view != nil {
		if !refresh || equalIndexStamps(view.stamps, indexStamps(root)) {
			if view.unavailable {
				return nil, errIndexChanged
			}
			return view, nil
		}
		if view.refreshed {
			if view.unavailable || view.db == nil || view.snapshot == nil {
				return nil, errIndexChanged
			}
			capturedStamps := indexStamps(root)
			if err := view.db.Close(); err != nil {
				return nil, agentapi.Wrap(agentapi.Cleanup, err)
			}
			view.db = nil
			if err := appendCurrentIndex(ctx, root, view.snapshot, l.readBudget); err != nil {
				view.unavailable = true
				return nil, err
			}
			db, err := openPrivateCurrent(view.snapshot.path)
			if err != nil {
				view.unavailable = true
				return nil, err
			}
			view.db = db
			view.stamps = capturedStamps
			return view, nil
		}
		if view.db != nil {
			if err := errors.Join(view.db.Close(), view.snapshot.close()); err != nil {
				return nil, agentapi.Wrap(agentapi.Cleanup, err)
			}
		}
		view = &currentIndexView{refreshed: true}
		l.indexes[root] = view
	} else {
		view = &currentIndexView{}
		l.indexes[root] = view
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	capturedStamps := indexStamps(root)
	snapshot, err := snapshotCurrentIndexBudget(ctx, root, nil, l.readBudget)
	if err != nil {
		// A read cut off by a deadline or cancellation says nothing about
		// the index: a later call, under a later allowance of the same pass
		// (the collector's after discovery's), reads it again.
		if agentapi.Failure(err) == agentapi.Limit || ctx.Err() != nil {
			delete(l.indexes, root)
			return nil, err
		}
		view.unavailable = true
		view.stamps = indexStamps(root)
		return nil, err
	}
	db, err := openPrivateCurrent(snapshot.path)
	if err != nil {
		cleanupErr := snapshot.close()
		view.unavailable = true
		return nil, errors.Join(err, cleanupErr)
	}
	db.SetMaxOpenConns(1)
	view.db = db
	view.snapshot = snapshot
	view.stamps = capturedStamps
	return view, nil
}

func openPrivateCurrent(path string) (*sql.DB, error) {
	params := url.Values{"mode": {"ro"}, "immutable": {"1"}}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: params.Encode()}).String())
	if err == nil {
		db.SetMaxOpenConns(1)
	}
	return db, err
}

// currentLocatorMissing reports a current row naming a rollout that is no
// longer there: Codex renamed it into archived_sessions/ (or back) without
// updating its thread index yet, or the row outlived the file. Such a row is
// no evidence of where the thread is now, so it is passed over like an absent
// row, and the thread's candidates, found by its ID, locate the rollout
// instead. Callers confine path to its native home first.
func currentLocatorMissing(path string) bool {
	_, err := os.Lstat(path)
	return errors.Is(err, os.ErrNotExist)
}

func currentLocator(ctx context.Context, db *sql.DB, id string) (string, bool, error) {
	// This query validates the selected locator in a private immutable view,
	// rather than collecting optional native scheduling hints. Allow the pass's
	// operation budget; its remaining allowance and caller cancellation still
	// cap this timeout through the inherited context.
	ctx, cancel := context.WithTimeout(ctx, Budget)
	defer cancel()
	conn, err := db.Conn(ctx)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = conn.Close() }()
	for _, limit := range []struct {
		id    int
		value int
	}{{sqlite3.SQLITE_LIMIT_LENGTH, 1 << 20}, {sqlite3.SQLITE_LIMIT_SQL_LENGTH, 16384}, {sqlite3.SQLITE_LIMIT_ATTACHED, 0}, {sqlite3.SQLITE_LIMIT_VDBE_OP, 20000}} {
		if _, err := sqlite.Limit(conn, limit.id, limit.value); err != nil {
			return "", false, err
		}
	}
	const query = "SELECT substr(rollout_path,1,4097) FROM threads WHERE id=? LIMIT 2"
	plan, err := conn.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query, id)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = plan.Close() }()
	valid := false
	count := 0
	for plan.Next() {
		var a, b, c int
		var detail string
		if err := plan.Scan(&a, &b, &c, &detail); err != nil {
			_ = plan.Close()
			return "", false, err
		}
		count++
		if count > 16 || len(detail) > 2048 || !strings.Contains(detail, "SEARCH threads USING INDEX sqlite_autoindex_threads_1 (id=?)") {
			_ = plan.Close()
			return "", false, errors.New("unsupported current primary key")
		}
		valid = true
	}
	err = plan.Err()
	_ = plan.Close()
	if err != nil || !valid {
		return "", false, errors.Join(errors.New("current primary key unavailable"), err)
	}
	rows, err := conn.QueryContext(ctx, query, id)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = rows.Close() }()
	var path string
	found := false
	for rows.Next() {
		if found {
			return "", false, errors.New("duplicate current thread")
		}
		if err := rows.Scan(&path); err != nil {
			return "", false, err
		}
		if path == "" || len(path) > 4096 {
			return "", false, errors.New("current locator length")
		}
		found = true
	}
	return path, found, rows.Err()
}

// Close releases all private databases and snapshots; it never changes natives.
func (l *CodexRolloutLookup) Close() error {
	// Normal pass cleanup must write a deferred discovery checkpoint even if
	// the pass deadline expired. Constructor failure uses its inherited context.
	return l.closeWithContext(context.Background())
}

// CloseReadOnly releases a preview's private resources without checkpointing
// observations into the capture catalog.
func (l *CodexRolloutLookup) CloseReadOnly() error {
	l.dirty = false
	return l.closeWithContext(context.Background())
}

func (l *CodexRolloutLookup) closeWithContext(ctx context.Context) error {
	if l.closed {
		return nil
	}
	l.closed = true
	defer func() {
		l.readBudget.ReleaseUnscoped(l.catalogCharge)
		l.catalogCharge = 0
		l.releaseRestored()
	}()
	l.readBudget.ReleaseUnscoped(l.observationBytes)
	l.observationBytes = 0
	l.readBudget.ReleaseUnscoped(l.listingBytes)
	l.listingBytes = 0
	l.listings = nil
	var errs []error
	if l.metadata != nil {
		errs = append(errs, l.metadata.close())
	}
	for _, view := range l.indexes {
		if view.db != nil {
			errs = append(errs, view.db.Close())
		}
		if view.snapshot != nil {
			errs = append(errs, view.snapshot.close())
		}
	}
	if l.dirty && !l.readOnly && l.catalog != nil {
		current := *l.catalog
		current.Version = catalogVersion
		current.Roots = slices.Clone(l.roots)
		errs = append(errs, l.writeCatalog(ctx, filepath.Join(l.store.Home(), "discovery-catalog.json"), current))
	}
	return errors.Join(errs...)
}

var (
	_ agentapi.CodexRolloutLookup    = (*CodexRolloutLookup)(nil)
	_ agentapi.CodexRolloutAllowance = (*CodexRolloutLookup)(nil)
)

// beginOperation starts one lookup call within the allowance left, and
// returns its end, which charges the call's time to the allowance. A call
// that fails because the allowance ran out while it ran (its own deadline
// passed, its caller's did not) fails as one past the allowance, whatever the
// interrupted step reported (a context error, an index unavailable), so the
// caller can put the source off rather than fail it.
func (l *CodexRolloutLookup) beginOperation(ctx context.Context) (context.Context, func(*error), error) {
	if err := l.checkBudget(ctx); err != nil {
		return ctx, nil, err
	}
	started := time.Now()
	deadline := started.Add(l.remaining)
	l.deadline = deadline
	operation, cancel := context.WithDeadline(ctx, deadline)
	return operation, func(err *error) {
		if *err != nil && ctx.Err() == nil && errors.Is(operation.Err(), context.DeadlineExceeded) {
			*err = allowanceSpent("native lookup pass deadline")
		}
		cancel()
		l.remaining -= time.Since(started)
	}, nil
}

// NativeReadBudget is the pass-owned shared native/source/cache charge ledger.
func (l *CodexRolloutLookup) NativeReadBudget() *agentapi.NativeReadBudget { return l.readBudget }

// ObserveReadOnly advances the existing bounded inventory without admission or
// durable state writes. It shares this lookup's pass deadline and charge ledger.
func (l *CodexRolloutLookup) ObserveReadOnly(ctx context.Context) (Health, error) {
	l.readOnly = true
	// Planning slices share one overall deadline and retained-data ledger. Reset
	// only this slice's I/O allowance so large inventories can advance fairly.
	l.probes = 0
	return runWithAdapters(ctx, l.store, config.Config{}, Options{Rollouts: l, nativeOnly: true}, registeredAdapters())
}
