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
	"github.com/wangjohn/agent-archive/internal/jsonwire"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"github.com/wangjohn/agent-archive/internal/state"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const maxCurrentThreads = 64

// CodexRolloutLookup is one serial collector-pass view of validated native facts.
// Neither the SQLite current locator nor a cached identity grants admission.
// Observation-cache membership never implies complete filesystem coverage.
type CodexRolloutLookup struct {
	metadata         *metadataInventory
	homes            []string
	store            *state.Store
	catalog          *catalog
	readOnly         bool
	readBudget       *agentapi.NativeReadBudget
	coverage         *coverageInventory
	coverageDirty    bool
	roots            []string
	deadline         time.Time
	remaining        time.Duration
	observationBytes int64
	catalogCharge    int64
	observations     map[string]rolloutObservation
	byThread         map[string]map[string]struct{}
	byPhysical       map[string]map[string]struct{}
	threads          map[string]agentapi.CodexRolloutSet
	indexes          map[string]*currentIndexView
	probes           int
	closed           bool
	threadOrder      []string
	queries          int
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
	lookup := &CodexRolloutLookup{store: store, homes: slices.Clone(homes), readBudget: agentapi.NewNativeReadBudget(currentSnapshotLimit), roots: roots, deadline: time.Now().Add(Budget), remaining: Budget, coverage: newCoverage(roots), observations: map[string]rolloutObservation{}, byThread: map[string]map[string]struct{}{}, byPhysical: map[string]map[string]struct{}{}, threads: map[string]agentapi.CodexRolloutSet{}, indexes: map[string]*currentIndexView{}}
	lookup.coverage.reserveFacts = lookup.reserveCatalog
	var prior catalog
	if err := lookup.readCatalog(ctx, filepath.Join(store.Home(), "discovery-catalog.json"), &prior); err != nil && !errors.Is(err, os.ErrNotExist) {
		if errors.Is(err, agentapi.ErrReadBudget) || ctx.Err() != nil {
			return nil, errors.Join(err, lookup.closeWithContext(ctx))
		}
		return lookup, nil
	}
	if prior.Coverage != nil && prior.Coverage.Version != 1 {
		return nil, errors.Join(errors.New("native coverage requires a newer writer"), lookup.closeWithContext(ctx))
	}
	if catalogNeedsReset(prior, roots) {
		return lookup, nil
	}
	if prior.Coverage != nil {
		if err := prior.Coverage.validate(roots); err != nil {
			return nil, errors.Join(err, lookup.closeWithContext(ctx))
		}
		lookup.coverage = prior.Coverage
	} else {
		lookup.coverage = newCoverage(roots)
	}
	lookup.coverage.reserveFacts = lookup.reserveCatalog
	for path, entry := range prior.Cache {
		root := lookup.homeFor(path)
		if root == "" || entry.Observation.Identity == nil {
			continue
		}
		lookup.Observe(SourceDescriptor{Kind: archive.SourceKindFile, Root: root, Locator: path, StableKey: entry.Observation.Identity.RolloutID}, Fingerprint{Size: entry.Size, Mtime: entry.Mtime}, entry.Observation.Identity)
	}
	lookup.coverageDirty = false
	lookup.catalog = &prior
	return lookup, nil
}

// Observe accepts already validated pass-owned metadata facts. It deep-copies
// them so caller mutations cannot silently alter a lookup revision token.
func (l *CodexRolloutLookup) Observe(source SourceDescriptor, stamp Fingerprint, identity *codexmeta.CodexIdentity) {
	if l.closed || identity == nil || identity.ThreadID == "" || identity.RolloutID == "" || source.Kind != archive.SourceKindFile || l.homeFor(source.Locator) != source.Root {
		return
	}
	if identityByteBound(*identity) > 4096 {
		return
	}
	copyID := cloneRolloutIdentity(*identity)
	if l.coverage != nil && l.coverage.observe(source, stamp, copyID) {
		l.coverageDirty = true
	}

	prior, present := l.observations[source.Locator]
	if !present && len(l.observations) >= maxCatalog {
		return
	}
	size := rolloutFactByteBound(source, stamp, copyID)
	oldSize := int64(0)
	if present {
		oldSize = rolloutFactByteBound(prior.source, prior.stamp, prior.identity)
	}
	inventoryBytes := int64(0)
	if l.coverage != nil {
		inventoryBytes = l.coverage.byteBound()
	}
	if l.observationBytes+size-oldSize > maxCoverageBytes/2 || inventoryBytes+l.observationBytes+size-oldSize > maxCoverageBytes {
		return
	}
	if size > oldSize && !l.readBudget.Reserve(size-oldSize) {
		return
	}
	if oldSize > size {
		l.readBudget.Release(oldSize - size)
	}
	l.observationBytes += size - oldSize
	if l.coverage != nil {
		l.coverage.hintBytes = l.observationBytes
	}
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

func rolloutFactByteBound(source SourceDescriptor, stamp Fingerprint, id codexmeta.CodexIdentity) int64 {
	return (coverageCandidate{Source: source, Stamp: stamp, Identity: id}).byteBound() + 6*int64(len(source.Locator)) + 512
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
		return agentapi.Wrap(agentapi.Limit, errors.New("native lookup pass deadline"))
	}
	return nil
}

func (l *CodexRolloutLookup) inspect(ctx context.Context, path, thread string) (*agentapi.SourceRef, error) {
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
		return nil, agentapi.Wrap(agentapi.Limit, errors.New("native header probe limit"))
	}
	const headerCharge = int64(sourcefacts.HeaderBytes + 128<<10)
	if !l.readBudget.Reserve(headerCharge) {
		return nil, agentapi.Wrap(agentapi.Limit, errors.New("shared native header budget exhausted"))
	}
	defer l.readBudget.Release(headerCharge)
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

// Rollout returns known physical locators, revalidated by the source provider.
func (l *CodexRolloutLookup) Rollout(ctx context.Context, id string) ([]agentapi.SourceRef, error) {
	var done func()
	var operationErr error
	ctx, done, operationErr = l.beginOperation(ctx)
	if operationErr != nil {
		return nil, operationErr
	}
	defer done()
	if err := l.checkBudget(ctx); err != nil {
		return nil, err
	}
	if id == "" || codexmeta.RolloutID(id+".jsonl") != id {
		return nil, agentapi.Wrap(agentapi.Unsafe, errors.New("invalid physical rollout"))
	}
	if l.coverage == nil {
		l.coverage = newCoverage(l.roots)
	}
	l.coverage.reserveFacts = l.reserveCatalog
	beforeSequence := l.coverage.Sequence
	if l.coverage.request(id) && l.coverage.Sequence != beforeSequence {
		l.coverageDirty = true
	}
	if request, found := l.coverage.Requests[id]; found && request.AttemptEpoch != 0 && request.Overflow && request.DeliveredEpoch != request.AttemptEpoch {
		request.DeliveredEpoch = request.AttemptEpoch
		l.coverage.Requests[id] = request
		l.coverageDirty = true
	}
	var refs []agentapi.SourceRef
	paths := map[string]string{}
	if request, found := l.coverage.Requests[id]; found {
		for path, candidate := range request.Candidates {
			if candidate.Identity.RolloutID == id {
				paths[path] = candidate.Identity.ThreadID
			}
		}
	}
	for path := range l.byPhysical[id] {
		observation := l.observations[path]
		if _, found := paths[path]; !found && len(paths) >= archive.MaxHistorySpans {
			return nil, agentapi.Wrap(agentapi.Limit, errors.New("physical locator limit"))
		}
		paths[path] = observation.identity.ThreadID
	}
	for path, thread := range paths {
		refs = append(refs, agentapi.SourceRef{Kind: archive.SourceKindFile, Path: path, Key: thread})
	}
	if len(refs) > archive.MaxHistorySpans {
		return nil, agentapi.Wrap(agentapi.Limit, errors.New("physical locator limit"))
	}
	if request, found := l.coverage.Requests[id]; found && request.CompleteEpoch == l.coverage.Epoch && l.coverage.proofEpoch == l.coverage.Epoch && !l.coverage.Failed {
		if request.DeliveredEpoch != request.AttemptEpoch {
			request.DeliveredEpoch = request.AttemptEpoch
			l.coverage.Requests[id] = request
			l.coverageDirty = true
		}
	}
	slices.SortFunc(refs, func(a, b agentapi.SourceRef) int { return strings.Compare(a.Path, b.Path) })
	return refs, nil
}

// Thread supplies current facts, plus bounded known same-thread candidates.
func (l *CodexRolloutLookup) Thread(ctx context.Context, id string) (agentapi.CodexRolloutSet, error) {
	if codexmeta.RolloutID(id+".jsonl") != id || id == "" {
		return agentapi.CodexRolloutSet{}, agentapi.Wrap(agentapi.Unsafe, errors.New("invalid native thread"))
	}
	return l.thread(ctx, id, true)
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
	return l.thread(ctx, id, false)
}

func (l *CodexRolloutLookup) thread(ctx context.Context, id string, native bool) (agentapi.CodexRolloutSet, error) {
	var done func()
	var operationErr error
	ctx, done, operationErr = l.beginOperation(ctx)
	if operationErr != nil {
		return agentapi.CodexRolloutSet{}, operationErr
	}
	defer done()
	if err := l.checkBudget(ctx); err != nil {
		return agentapi.CodexRolloutSet{}, err
	}

	if l.coverage == nil {
		l.coverage = newCoverage(l.roots)
	}
	l.coverage.reserveFacts = l.reserveCatalog
	beforeSequence := l.coverage.Sequence
	if native && l.coverage.request(id) && l.coverage.Sequence != beforeSequence {
		l.coverageDirty = true
	}
	if request, found := l.coverage.Requests[id]; found && request.AttemptEpoch != 0 && request.Overflow {
		if request.DeliveredEpoch != request.AttemptEpoch {
			request.DeliveredEpoch = request.AttemptEpoch
			l.coverage.Requests[id] = request
			l.coverageDirty = true
		}
	}
	if l.candidateCount(id) > archive.MaxHistorySpans {
		return agentapi.CodexRolloutSet{}, agentapi.Wrap(agentapi.Limit, errors.New("thread candidate limit"))
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
		ref, err := l.inspect(ctx, path, id)
		if err != nil {
			return agentapi.CodexRolloutSet{}, err
		}
		if current != nil && current.Path != ref.Path {
			return agentapi.CodexRolloutSet{}, agentapi.Wrap(agentapi.Unavailable, errors.New("conflicting current locators"))
		}
		current = ref
	}
	if l.candidateCount(id) > archive.MaxHistorySpans {
		return agentapi.CodexRolloutSet{}, agentapi.Wrap(agentapi.Limit, errors.New("thread candidate limit"))
	}
	set, err := l.withCandidates(ctx, id, agentapi.CodexRolloutSet{Current: current})
	if err != nil {
		return agentapi.CodexRolloutSet{}, err
	}
	if len(l.threads) >= maxCoverageRequests {
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

func (l *CodexRolloutLookup) withCandidates(ctx context.Context, id string, set agentapi.CodexRolloutSet) (agentapi.CodexRolloutSet, error) {
	set.Candidates = nil
	for path := range l.byThread[id] {
		set.Candidates = append(set.Candidates, agentapi.SourceRef{Kind: archive.SourceKindFile, Path: path, Key: id})
	}
	slices.SortFunc(set.Candidates, func(a, b agentapi.SourceRef) int { return strings.Compare(a.Path, b.Path) })
	set.Complete = false
	if l.coverage != nil {
		if request, present := l.coverage.Requests[id]; present && request.CompleteEpoch == l.coverage.Epoch && l.coverage.proofEpoch == l.coverage.Epoch && !request.Overflow && l.coverage.Phase == coverageComplete && !l.coverage.Failed {
			set.Candidates = nil
			for _, candidate := range request.Candidates {
				set.Candidates = append(set.Candidates, agentapi.SourceRef{Kind: archive.SourceKindFile, Path: candidate.Source.Locator, Key: id})
			}
			slices.SortFunc(set.Candidates, func(a, b agentapi.SourceRef) int { return strings.Compare(a.Path, b.Path) })
			set.Complete = true
		}
	}
	value := struct {
		Current      *agentapi.SourceRef        `json:"Current"`
		Complete     bool                       `json:"Complete"`
		Candidates   []agentapi.SourceRef       `json:"Candidates"`
		Observations []rolloutObservationDigest `json:"Observations"`
	}{Current: set.Current, Complete: set.Complete, Candidates: set.Candidates, Observations: l.candidateDigests(id)}
	revision, err := l.revisionDigest(ctx, value)
	if err != nil {
		return agentapi.CodexRolloutSet{}, err
	}
	set.Revision = revision
	// A refused digest has delivered no complete result. Keep that requested
	// attempt enrolled until the caller actually receives its revision proof.
	if set.Complete {
		request := l.coverage.Requests[id]
		if request.DeliveredEpoch != request.AttemptEpoch {
			request.DeliveredEpoch = request.AttemptEpoch
			l.coverage.Requests[id] = request
			l.coverageDirty = true
		}
	}
	return set, nil
}

func (l *CodexRolloutLookup) revisionDigest(ctx context.Context, value any) (string, error) {
	const scratch = 32 << 10
	if !l.readBudget.Reserve(scratch) {
		return "", errCatalogBudget
	}
	n, err := jsonwire.Bound(ctx, value, l.readBudget.Available()/2)
	l.readBudget.Release(scratch)
	if err != nil {
		return "", errors.Join(errCatalogBudget, err)
	}
	// The encoder buffer and returned immutable JSON coexist until hashing ends.
	if !l.readBudget.Reserve(n + n) {
		return "", errCatalogBudget
	}
	defer l.readBudget.Release(n + n)
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func (l *CodexRolloutLookup) candidateCount(id string) int {
	if l.coverage != nil && l.coverage.Phase == coverageComplete && l.coverage.proofEpoch == l.coverage.Epoch && !l.coverage.Failed {
		if request, found := l.coverage.Requests[id]; found && request.CompleteEpoch == l.coverage.Epoch && !request.Overflow {
			return len(request.Candidates)
		}
	}
	return len(l.byThread[id])
}

type rolloutObservationDigest struct {
	Path     string                  `json:"Path"`
	Stamp    Fingerprint             `json:"Stamp"`
	Identity codexmeta.CodexIdentity `json:"Identity"`
}

func (l *CodexRolloutLookup) candidateDigests(id string) []rolloutObservationDigest {
	var out []rolloutObservationDigest
	if l.coverage != nil && l.coverage.Phase == coverageComplete && l.coverage.proofEpoch == l.coverage.Epoch && !l.coverage.Failed {
		if request, present := l.coverage.Requests[id]; present && request.CompleteEpoch == l.coverage.Epoch && !request.Overflow {
			for path, candidate := range request.Candidates {
				out = append(out, rolloutObservationDigest{path, candidate.Stamp, candidate.Identity})
			}
			slices.SortFunc(out, func(a, b rolloutObservationDigest) int { return strings.Compare(a.Path, b.Path) })
			return out
		}
	}
	for path := range l.byThread[id] {
		o := l.observations[path]
		out = append(out, rolloutObservationDigest{path, o.stamp, o.identity})
	}
	slices.SortFunc(out, func(a, b rolloutObservationDigest) int { return strings.Compare(a.Path, b.Path) })
	return out
}

// Check revalidates selection within the pass. One private refresh per home is
// allowed; later same-generation appends replay into that existing private view.
func (l *CodexRolloutLookup) Check(ctx context.Context, id, revision string) error {
	var done func()
	var operationErr error
	ctx, done, operationErr = l.beginOperation(ctx)
	if operationErr != nil {
		return operationErr
	}
	defer done()
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
		if current != nil && current.Path != ref.Path {
			return agentapi.Wrap(agentapi.Changed, errIndexChanged)
		}
		current = ref
	}
	if l.candidateCount(id) > archive.MaxHistorySpans {
		return agentapi.Wrap(agentapi.Limit, errors.New("thread candidate limit"))
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
		if agentapi.Failure(err) == agentapi.Limit {
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
	// Normal pass cleanup must checkpoint bounded durable coverage even if the
	// pass deadline expired. Constructor failure uses its inherited context.
	return l.closeWithContext(context.Background())
}

// CloseReadOnly releases a preview's private resources without checkpointing
// newly requested coverage or observations into the capture catalog.
func (l *CodexRolloutLookup) CloseReadOnly() error {
	l.coverageDirty = false
	return l.closeWithContext(context.Background())
}

func (l *CodexRolloutLookup) closeWithContext(ctx context.Context) error {
	if l.closed {
		return nil
	}
	l.closed = true
	defer func() {
		l.readBudget.Release(l.catalogCharge)
		l.catalogCharge = 0
	}()
	l.readBudget.Release(l.observationBytes)
	l.observationBytes = 0
	if l.coverage != nil {
		l.coverage.hintBytes = 0
	}
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
	if l.coverageDirty && !l.readOnly {
		if err := l.coverage.validate(l.roots); err != nil {
			return errors.Join(append(errs, err)...)
		}
		var current catalog
		var err error
		if l.catalog != nil {
			current = *l.catalog
		} else {
			err = l.readCatalog(ctx, filepath.Join(l.store.Home(), "discovery-catalog.json"), &current)
		}
		if err == nil || errors.Is(err, os.ErrNotExist) {
			if current.Coverage != nil && current.Coverage.Version != 1 {
				errs = append(errs, errors.New("native coverage requires a newer writer"))
			} else {
				current.Version = catalogVersion
				current.Roots = slices.Clone(l.roots)
				current.Coverage = l.coverage
				errs = append(errs, l.writeCatalog(ctx, filepath.Join(l.store.Home(), "discovery-catalog.json"), current))
			}
		} else {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

var _ agentapi.CodexRolloutLookup = (*CodexRolloutLookup)(nil)

func (l *CodexRolloutLookup) beginOperation(ctx context.Context) (context.Context, func(), error) {
	if err := l.checkBudget(ctx); err != nil {
		return ctx, nil, err
	}
	started := time.Now()
	deadline := started.Add(l.remaining)
	l.deadline = deadline
	operation, cancel := context.WithDeadline(ctx, deadline)
	return operation, func() { cancel(); l.remaining -= time.Since(started) }, nil
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

// PrepareRegistered requests qualified history coverage only for already admitted
// owners, then advances one shared observation slice before source snapshots open.
// Requests and incomplete work survive refusal; observations grant no admission.
func (l *CodexRolloutLookup) PrepareRegistered(ctx context.Context, cfg config.Config, regs []archive.SessionRegistration, o Options) error {
	if l.closed {
		return agentapi.ErrClosed
	}
	needed := false
	for _, reg := range regs {
		if reg.Harness.Name != "codex" || !cfg.AcceptSession(reg) || l.homeFor(reg.TranscriptPath) == "" {
			continue
		}
		id := reg.NativeSessionID
		if reg.CodexBinding != nil {
			id = reg.CodexBinding.NativeThreadID
		}
		if codexmeta.RolloutID(id+".jsonl") != id || id == "" {
			continue
		}
		if l.coverage == nil {
			l.coverage = newCoverage(l.roots)
		}
		l.coverage.reserveFacts = l.reserveCatalog
		before := l.coverage.Sequence
		if !l.coverage.request(id) {
			continue
		}
		if before != l.coverage.Sequence {
			l.coverageDirty = true
		}
		request := l.coverage.Requests[id]
		if request.CompleteEpoch != l.coverage.Epoch || l.coverage.proofEpoch != l.coverage.Epoch || l.coverage.Failed {
			needed = true
		}
	}
	if !needed {
		return nil
	}
	o.Rollouts = l
	o.inventoryOnly = true
	_, err := runWithAdapters(ctx, l.store, cfg, o, registeredAdapters())
	return err

}
