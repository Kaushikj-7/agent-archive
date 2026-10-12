package codex

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/codexmeta"
	"github.com/wangjohn/agent-archive/internal/sourceio"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

// passReadBudget is the shared ledger a pass opens when its caller supplies
// none. It charges bytes the pass holds in memory (headers, line buffers,
// cached prefixes), never the length of a file it reads through a
// descriptor.
const passReadBudget = agentapi.PassReadBudget

// prefixCacheLimit bounds the immutable ancestor prefixes a pass keeps for
// the threads that share them. The cache outlives one session's read, so it
// stays small beside a session's own ceiling.
const prefixCacheLimit int64 = 16 << 20

// PassCacheBytes is the most a pass keeps between reads: cached ancestor
// prefixes and the header of every open rollout, all charged unscoped
// within agentapi.PassCacheReserve.
const PassCacheBytes = prefixCacheLimit + archive.MaxHistorySpans*sourceHeaderCharge

// ordinaryScratch is what an ordinary snapshot holds while open: hashing and
// boundary scratch. The filter charges its own record buffer (see
// nativeScannerCharge).
const ordinaryScratch int64 = 64 << 10

const sourceHeaderCharge int64 = 128 << 10

// Describe retains ordinary append protection; revision publication is separately fenced.
func (SourceProvider) Describe(ref agentapi.SourceRef) (agentapi.SourceSemantics, error) {
	return (sourceio.FileProvider{}).Describe(ref)
}

// OpenPass owns a bounded shared dependency cache and every descriptor it opens.
func (SourceProvider) OpenPass(ctx context.Context, e agentapi.SourceEnvironment) (agentapi.SourcePass, error) {
	legacy, err := (sourceio.FileProvider{}).OpenPass(ctx, e)
	if err != nil {
		return nil, err
	}
	if shared, ok := e.CodexRollouts.(agentapi.CodexRolloutResourceBudget); ok && shared.NativeReadBudget() != nil && e.ReadBudget != nil && e.ReadBudget != shared.NativeReadBudget() {
		return nil, errors.Join(sourceFailure(agentapi.Unavailable, "source and lookup require the same charge ledger"), legacy.Close())
	}
	if e.ReadBudget == nil {
		if shared, ok := e.CodexRollouts.(agentapi.CodexRolloutResourceBudget); ok {
			e.ReadBudget = shared.NativeReadBudget()
		} else {
			e.ReadBudget = agentapi.NewNativeReadBudget(passReadBudget)
		}
	}
	if e.Files == nil {
		e.Files = transcriptio.OS{}
	}
	return &relatedSourcePass{env: e, legacy: legacy, files: map[string]*rolloutFile{}, live: map[*historySnapshot]bool{}, ordinaryLive: map[*ordinarySnapshot]bool{}}, nil
}

// Activities preserves cheap file ordering independently of history selection.
func (SourceProvider) Activities(ctx context.Context, e agentapi.SourceEnvironment, refs []agentapi.SourceRef) (map[agentapi.SourceRef]time.Time, error) {
	return (sourceio.FileProvider{}).Activities(ctx, e, refs)
}

type rolloutFile struct {
	ref          agentapi.SourceRef
	file         *transcriptio.Snapshot
	meta         codexmeta.CodexMeta
	identity     codexmeta.CodexIdentity
	header       []byte
	headerDigest [32]byte
	boundary     int64
	refs         int
	prefix       []byte
	validated    *prefixValidation
}

type relatedSourcePass struct {
	env          agentapi.SourceEnvironment
	legacy       agentapi.SourcePass
	files        map[string]*rolloutFile
	live         map[*historySnapshot]bool
	ordinaryLive map[*ordinarySnapshot]bool
	bytes        int64
	prefixBytes  int64
	cacheHits    int64
	closed       bool
	closeErr     error
}

func sourceFailure(kind agentapi.FailureKind, message string) error {
	return agentapi.Wrap(kind, errors.New(message))
}

func (p *relatedSourcePass) open(ctx context.Context, ref agentapi.SourceRef) (*rolloutFile, error) {
	if p.closed {
		return nil, agentapi.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(ref.Path) > 8192 || len(ref.Key) > 4096 {
		return nil, sourceFailure(agentapi.Limit, "rollout locator limit")
	}
	if ref.Kind != archive.SourceKindFile {
		return nil, sourceFailure(agentapi.Unsafe, "invalid rollout kind")
	}
	if old := p.files[ref.Path]; old != nil {
		if err := old.file.Check(); err == nil && old.checkHeader(ctx) == nil {
			return old, nil
		}
		if old.refs > 0 {
			return nil, sourceFailure(agentapi.Changed, "rollout dependency changed")
		}
		p.dropFile(ref.Path, old)
		if err := old.file.Close(); err != nil {
			return nil, agentapi.Wrap(agentapi.Cleanup, err)
		}
	}
	if len(p.files) >= archive.MaxHistorySpans {
		return nil, sourceFailure(agentapi.Limit, "shared rollout file limit")
	}
	f, err := transcriptio.Open(p.env.Files, ref.Path, p.env.Policy)
	if err != nil {
		return nil, sourceio.Classify(err)
	}
	ownedCharge := int64(0)
	fail := func(e error) (*rolloutFile, error) {
		if ownedCharge > 0 {
			p.releaseCache(ownedCharge)
			ownedCharge = 0
		}
		return nil, errors.Join(e, agentapi.Wrap(agentapi.Cleanup, f.Close()))
	}
	if f.Length() > archive.MaxCodexRawTranscriptBytes {
		stamp := f.Stamp()
		o := agentapi.SourceObservation{Present: true, Size: stamp.Size, Activity: stamp.ModifiedAt, Signature: sourceio.FileSignature(stamp.Size, stamp.ModifiedAt.UnixNano())}
		return fail(agentapi.RawLimit(f.Length(), archive.MaxCodexRawTranscriptBytes, &o))
	}
	// The file is read through its descriptor, a bounded record at a time;
	// only the header and proof state stay in memory with it.
	if !p.reserveHeader() {
		return fail(agentapi.ReadBudgetLimit(errors.New("shared source and index budget exhausted")))
	}
	ownedCharge = sourceHeaderCharge
	reader := bufio.NewReaderSize(io.NewSectionReader(f, 0, min(f.Length(), int64(64<<10)+1)), 4096)
	line, err := reader.ReadBytes('\n')
	if err != nil || len(line) > 64<<10 {
		return fail(sourceFailure(agentapi.Unavailable, "rollout metadata incomplete"))
	}
	meta, _, found, err := codexmeta.ParseCodexMeta(line)
	if err != nil || !found {
		return fail(sourceFailure(agentapi.FormatMismatch, "invalid rollout metadata"))
	}
	if meta.HistoryMode != "" && meta.HistoryMode != codexmeta.CodexHistoryLegacy && meta.HistoryMode != codexmeta.CodexHistoryPaginated {
		return fail(sourceFailure(agentapi.Unsafe, "unsupported history mode"))
	}
	identity, outcome := meta.Identity(ref.Path)
	if outcome != "" {
		return fail(sourceFailure(agentapi.FormatMismatch, string(outcome)))
	}
	boundary, err := p.chargedBoundary(f)
	if err != nil {
		return fail(sourceio.Classify(err))
	}
	out := &rolloutFile{ref: ref, file: f, meta: meta, identity: identity, header: line, headerDigest: sha256.Sum256(line), boundary: boundary}
	p.files[ref.Path] = out
	return out, nil
}

func (p *relatedSourcePass) evict() error {
	var err error
	for path, f := range p.files {
		if f.refs == 0 {
			err = errors.Join(err, agentapi.Wrap(agentapi.Cleanup, f.file.Close()))
			p.dropFile(path, f)
		}
	}
	return err
}

// dropFile forgets a cached file and returns everything it held.
func (p *relatedSourcePass) dropFile(path string, f *rolloutFile) {
	p.releaseCache(f.charge())
	p.prefixBytes -= int64(len(f.prefix))
	delete(p.files, path)
}

func (p *relatedSourcePass) Close() error {
	if p.closed {
		return p.closeErr
	}
	p.closed = true
	err := p.legacy.Close()
	for s := range p.live {
		err = errors.Join(err, s.Close())
	}
	for s := range p.ordinaryLive {
		err = errors.Join(err, s.Close())
	}
	for _, f := range p.files {
		p.releaseCache(f.charge())
		err = errors.Join(err, agentapi.Wrap(agentapi.Cleanup, f.file.Close()))
		f.prefix = nil
		f.validated = nil
	}
	p.files = nil
	p.bytes = 0
	p.prefixBytes = 0
	p.closeErr = err
	return err
}

// checkHeader binds every parsed header fact to the bytes observed at open,
// independently of whether this rollout contributes a nonempty history span.
func (f *rolloutFile) checkHeader(ctx context.Context) error {
	return sourceio.Classify(f.file.CheckPrefix(ctx, int64(len(f.header)), f.headerDigest))
}

type sourceSelection struct {
	legacyOrdinary bool
	leaf           *rolloutFile
	set            agentapi.CodexRolloutSet
	thread         string
	headers        []*rolloutFile

	// lineage reports that selectLineage chose leaf from set's candidates
	// alone, without a current locator.
	lineage bool
}

func (p *relatedSourcePass) selectSource(ctx context.Context, ref agentapi.SourceRef) (sourceSelection, error) {
	if len(p.files) >= 32 || p.prefixBytes > prefixCacheLimit/2 {
		if err := p.evict(); err != nil {
			return sourceSelection{}, err
		}
	}
	var err error
	ref, err = p.initialLookupRef(ctx, ref)
	if err != nil {
		return sourceSelection{}, err
	}

	seed, err := p.openSelectedSeed(ctx, ref)
	if err != nil {
		return sourceSelection{}, err
	}
	if ref.Key != "" && seed.identity.ThreadID != ref.Key {
		return sourceSelection{}, sourceFailure(agentapi.Unsafe, "registered native thread identity mismatch")
	}
	selected := sourceSelection{leaf: seed, thread: seed.identity.ThreadID, headers: []*rolloutFile{seed}}
	if p.env.CodexRollouts == nil {
		if seed.identity.RolloutID != seed.identity.ThreadID || seed.identity.HistoryBase != nil {
			return selected, errors.Join(sourceFailure(agentapi.Unavailable, "current rollout evidence unavailable"), archive.ErrRelatedHistory)
		}
		return selected, nil
	}
	set, err := p.env.CodexRollouts.Thread(ctx, selected.thread)
	if err != nil {
		return selected, err
	}
	if len(set.Revision) > 4096 || len(set.Candidates) > archive.MaxHistorySpans {
		return selected, sourceFailure(agentapi.Limit, "rollout candidate limit")
	}
	for _, candidate := range set.Candidates {
		if len(candidate.Path) > 8192 || len(candidate.Key) > 4096 || candidate.Kind != archive.SourceKindFile {
			return selected, sourceFailure(agentapi.Unsafe, "invalid rollout candidate locator")
		}
	}
	selected.set = set
	if set.Current != nil {
		current, e := p.open(ctx, *set.Current)
		if e != nil {
			return selected, e
		}
		if current.identity.ThreadID != selected.thread {
			return selected, sourceFailure(agentapi.Unsafe, "current rollout identity mismatch")
		}
		selected.headers = append(selected.headers, current)
		selected.leaf = current
		return selected, nil
	}
	if p.legacyOrdinarySeed(seed) {
		// An existing ordinary legacy registration retains its anchored source
		// semantics. The real lookup token still rejects a later current row.
		selected.legacyOrdinary = true
		return selected, nil
	}
	return p.selectLineage(ctx, selected)
}

func (p *relatedSourcePass) openSelectedSeed(ctx context.Context, ref agentapi.SourceRef) (*rolloutFile, error) {
	seed, err := p.open(ctx, ref)
	if errors.Is(err, os.ErrNotExist) && p.env.CodexRollouts != nil && codexmeta.RolloutID(ref.Key+".jsonl") == ref.Key && ref.Key != "" {
		set, lookupErr := p.env.CodexRollouts.Thread(ctx, ref.Key)
		if lookupErr != nil {
			return nil, lookupErr
		}
		if set.Current != nil {
			return p.open(ctx, *set.Current)
		}
		if set.Complete && len(set.Candidates) > 0 && len(set.Candidates) <= archive.MaxHistorySpans {
			return p.open(ctx, set.Candidates[0])
		}
		if p.coverageUnproven(set) {
			// Codex may have renamed it (into archived_sessions/ or back): it
			// is missing only once coverage finds no rollout by its ID.
			return nil, coverageIncomplete("registered rollout not found; coverage incomplete")
		}
	}
	return seed, err
}

func (p *relatedSourcePass) legacyOrdinarySeed(seed *rolloutFile) bool {
	id := seed.identity
	return p.env.LegacyUnboundRegistration && seed.meta.HistoryMode == "" && seed.meta.LocalExecutionSource() && !hasRelated(seed) && id.ParentID == "" && id.ForkOrdinal == nil && id.SubagentOrdinal == nil && (id.RootID == "" || id.RootID == id.ThreadID)
}

func (p *relatedSourcePass) selectLineage(ctx context.Context, selected sourceSelection) (sourceSelection, error) {
	set := selected.set
	if !set.Complete {
		return selected, coverageIncomplete("rollout lineage incomplete")
	}
	candidates := map[string]*rolloutFile{}
	bases := map[string]bool{}
	for _, candidate := range set.Candidates {
		f, e := p.open(ctx, candidate)
		if e != nil {
			return selected, e
		}
		selected.headers = append(selected.headers, f)
		id := f.identity
		if id.ThreadID != selected.thread {
			return selected, sourceFailure(agentapi.Unsafe, "rollout candidate identity mismatch")
		}
		if prior := candidates[id.RolloutID]; prior != nil && prior.ref.Path != f.ref.Path {
			return selected, p.coverageRefusal(set, "ambiguous physical rollout")
		}
		candidates[id.RolloutID] = f
		if id.HistoryBase != nil {
			bases[id.HistoryBase.RolloutID] = true
		}
	}
	var tip *rolloutFile
	for id, f := range candidates {
		if !bases[id] {
			if tip != nil {
				return selected, p.coverageRefusal(set, "ambiguous rollout tips")
			}
			tip = f
		}
	}
	if tip == nil {
		return selected, p.coverageRefusal(set, "no complete rollout tip")
	}
	spans, err := p.graph(ctx, tip, set)
	if err != nil {
		return selected, err
	}
	for _, span := range spans {
		delete(candidates, span.file.identity.RolloutID)
	}
	if len(candidates) != 0 {
		return selected, p.coverageRefusal(set, "disconnected rollout lineage")
	}
	selected.leaf = tip
	selected.lineage = true
	return selected, nil
}

type physicalSpan struct {
	file         *rolloutFile
	end          int64
	startOrdinal uint64
	endOrdinal   uint64
	digest       [32]byte
}

func (p *relatedSourcePass) graph(ctx context.Context, leaf *rolloutFile, set agentapi.CodexRolloutSet) ([]physicalSpan, error) {
	var reversed []physicalSpan
	seen := map[string]bool{}
	current := leaf
	end, err := p.chargedNewlineBoundary(leaf.file)
	if err != nil {
		return nil, sourceio.Classify(err)
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		id := current.identity
		if seen[id.RolloutID] {
			return nil, sourceFailure(agentapi.Unsafe, "rollout history cycle")
		}
		seen[id.RolloutID] = true
		if len(reversed) >= archive.MaxHistorySpans {
			return nil, sourceFailure(agentapi.Limit, "rollout history depth limit")
		}
		start := uint64(0)
		if id.HistoryBase != nil {
			start = id.HistoryBase.EndOrdinal
		}
		reversed = append(reversed, physicalSpan{file: current, end: end, startOrdinal: start})
		if id.HistoryBase == nil {
			break
		}
		if p.env.CodexRollouts == nil {
			return nil, errors.Join(sourceFailure(agentapi.Unavailable, "history dependency unavailable"), archive.ErrRelatedHistory)
		}
		refs, err := p.env.CodexRollouts.Rollout(ctx, id.HistoryBase.RolloutID)
		if err != nil {
			return nil, err
		}
		if len(refs) != 1 {
			return nil, p.coverageRefusal(set, "history dependency missing or ambiguous")
		}
		base, err := p.open(ctx, refs[0])
		if err != nil {
			return nil, err
		}
		if base.identity.RolloutID != id.HistoryBase.RolloutID {
			return nil, sourceFailure(agentapi.Unsafe, "history dependency identity mismatch")
		}
		if id.HistoryBase.EndByteOffset > math.MaxInt64 {
			return nil, sourceFailure(agentapi.Unsafe, "invalid history byte boundary")
		}
		end = int64(id.HistoryBase.EndByteOffset)
		if end > base.boundary {
			return nil, sourceFailure(agentapi.Unsafe, "invalid history byte boundary")
		}
		var last [1]byte
		if end > 0 {
			if _, err := base.file.ReadAt(last[:], end-1); err != nil || last[0] != '\n' {
				return nil, sourceFailure(agentapi.Unsafe, "split history record boundary")
			}
		}
		current = base
	}
	slices.Reverse(reversed)
	return reversed, nil
}

func hasRelated(f *rolloutFile) bool {
	id := f.identity
	return id.Child || id.ForkID != "" || id.HistoryBase != nil || id.RolloutID != id.ThreadID
}

func (p *relatedSourcePass) Signature(ctx context.Context, ref agentapi.SourceRef) (agentapi.SourceObservation, error) {
	if p.env.CodexRollouts != nil && p.genericLegacy(ref) {
		return p.genericLegacySignature(ctx, ref)
	}
	if p.env.CodexRollouts == nil && codexmeta.RolloutID(ref.Path) == "" {
		return p.legacy.Signature(ctx, ref)
	}
	selection, err := p.selectSource(ctx, ref)
	if err != nil {
		if selection.leaf == nil && p.env.CodexRollouts == nil && !agentapi.HasFailure(err, agentapi.Cleanup) && (agentapi.Failure(err) == agentapi.FormatMismatch || agentapi.Failure(err) == agentapi.Unavailable) {
			return p.legacy.Signature(ctx, ref)
		}
		return agentapi.SourceObservation{}, err
	}
	if !hasRelated(selection.leaf) {
		if p.env.CodexRollouts == nil {
			observed, err := p.legacy.Signature(ctx, ref)
			if err != nil {
				return observed, err
			}
			return observed, checkHeaders(ctx, selection.headerFiles(nil))
		}
		return p.observation(ctx, selection, []physicalSpan{{file: selection.leaf, end: selection.leaf.boundary}})
	}
	if p.env.RequireConfinedHistory && p.env.Policy.Root == "" {
		return agentapi.SourceObservation{}, sourceFailure(agentapi.Unavailable, "related history requires admitted confined source home")
	}
	spans, err := p.graph(ctx, selection.leaf, selection.set)
	if err != nil {
		return agentapi.SourceObservation{}, err
	}
	return p.observation(ctx, selection, spans)
}

// headerFiles retains selection evidence as well as contributing graph nodes.
func (selection sourceSelection) headerFiles(spans []physicalSpan) []*rolloutFile {
	files := slices.Clone(selection.headers)
	for _, span := range spans {
		files = append(files, span.file)
	}
	out := files[:0]
	for _, f := range files {
		if !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	return out
}

func checkHeaders(ctx context.Context, files []*rolloutFile) error {
	for _, f := range files {
		if err := f.checkHeader(ctx); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (p *relatedSourcePass) observation(ctx context.Context, selection sourceSelection, spans []physicalSpan) (agentapi.SourceObservation, error) {
	h := sha256.New()
	_, _ = fmt.Fprint(h, "codex-history-v1\x00", selection.set.LineageToken(), "\x00", selection.leaf.ref.Path)
	candidatePaths := make([]string, 0, len(selection.set.Candidates))
	for _, ref := range selection.set.Candidates {
		candidatePaths = append(candidatePaths, string(ref.Kind)+"\x00"+ref.Path+"\x00"+ref.Key)
	}
	slices.Sort(candidatePaths)
	for _, candidate := range candidatePaths {
		_, _ = fmt.Fprint(h, "\x00candidate:", candidate)
	}
	out := agentapi.SourceObservation{Present: true}
	for _, span := range spans {
		stamp := span.file.file.Stamp()
		out.Size += span.end
		if stamp.ModifiedAt.After(out.Activity) {
			out.Activity = stamp.ModifiedAt
		}
		_, _ = fmt.Fprintf(h, "\x00%s:%d:%d:%d:%d:%x", span.file.ref.Path, stamp.Size, stamp.ModifiedAt.UnixNano(), span.end, span.startOrdinal, span.file.headerDigest)
	}
	if p.env.CodexRollouts != nil {
		if err := p.env.CodexRollouts.Check(ctx, selection.thread, selection.set.Revision); err != nil {
			return out, sourceio.Classify(err)
		}
	}
	if err := checkHeaders(ctx, selection.headerFiles(spans)); err != nil {
		return out, err
	}
	out.Empty = out.Size == 0
	out.Signature = agentapi.SourceSignature{Version: 1, Provider: "file", Token: hex.EncodeToString(h.Sum(nil))}
	return out, nil
}

func (p *relatedSourcePass) Read(ctx context.Context, ref agentapi.SourceRef, limits agentapi.ReadLimits) (agentapi.SourceSnapshot, error) {
	if p.env.CodexRollouts != nil && p.genericLegacy(ref) {
		return p.genericLegacyRead(ctx, ref, limits)
	}
	if p.env.CodexRollouts == nil && codexmeta.RolloutID(ref.Path) == "" {
		return p.legacy.Read(ctx, ref, limits)
	}
	fail := p.readFailure
	selection, err := p.selectSource(ctx, ref)
	if err != nil {
		if selection.leaf == nil && p.env.CodexRollouts == nil && !agentapi.HasFailure(err, agentapi.Cleanup) && (agentapi.Failure(err) == agentapi.FormatMismatch || agentapi.Failure(err) == agentapi.Unavailable) {
			return p.legacy.Read(ctx, ref, limits)
		}
		return fail(err)
	}
	if selection.lineage && selection.set.Retained {
		// A retained proof can lack a segment created since it, so the tip it
		// yields may be stale. It still lets Signature recognize an unchanged
		// source; content waits for this pass's own coverage proof.
		return fail(coverageIncomplete("rollout lineage incomplete"))
	}
	return p.readSelection(ctx, limits, selection)
}

func (p *relatedSourcePass) readSelection(ctx context.Context, limits agentapi.ReadLimits, selection sourceSelection) (agentapi.SourceSnapshot, error) {
	fail := p.readFailure
	if !hasRelated(selection.leaf) {
		snapshot, err := p.ordinary(ctx, limits, selection)
		if err != nil {
			return fail(err)
		}
		if selection.legacyOrdinary {
			return legacyOrdinarySnapshot{SourceSnapshot: snapshot}, nil
		}
		return snapshot, nil
	}
	return p.readHistorySelection(ctx, limits, selection)
}

func (p *relatedSourcePass) readHistorySelection(ctx context.Context, limits agentapi.ReadLimits, selection sourceSelection) (agentapi.SourceSnapshot, error) {
	if p.env.RequireConfinedHistory && p.env.Policy.Root == "" {
		return nil, sourceFailure(agentapi.Unavailable, "related history requires admitted confined source home")
	}
	fail := p.readFailure
	spans, err := p.graph(ctx, selection.leaf, selection.set)
	if err != nil {
		return fail(err)
	}
	var size int64
	for _, span := range spans {
		size += span.end
	}
	if rawLimit := codexRawLimit(limits); size > rawLimit {
		// The observation lets the caller record the gap at this state, so
		// an unchanged lineage is not read again to reach it.
		observed, err := p.observation(ctx, selection, spans)
		if err != nil {
			return fail(errors.Join(agentapi.RawLimit(size, rawLimit, nil), err))
		}
		return fail(agentapi.RawLimit(size, rawLimit, &observed))
	}
	limit := limits.RecordBytes
	if limit <= 0 || limit > archive.MaxRecordBytes {
		limit = archive.MaxRecordBytes
	}
	s := &historySnapshot{owner: p, selection: selection, spans: spans, headers: selection.headerFiles(spans), recordLimit: limit}
	for _, f := range s.headers {
		f.refs++
	}
	p.live[s] = true
	if err = p.cachePrefixes(ctx, spans); err != nil {
		return fail(errors.Join(err, s.Close()))
	}
	if err = s.validate(ctx); err != nil {
		return fail(errors.Join(err, s.Close()))
	}
	s.observed, err = p.observation(ctx, selection, spans)
	if err != nil {
		return fail(errors.Join(err, s.Close()))
	}
	return s, nil
}

type historySnapshot struct {
	admission    *archive.CodexSourceBinding
	firstOwnTask *agentapi.OwnTaskFacts
	owner        *relatedSourcePass
	selection    sourceSelection
	spans        []physicalSpan
	headers      []*rolloutFile
	history      archive.SourceHistory
	observed     agentapi.SourceObservation
	recordLimit  int64
	span         int
	lines        *chargedLineReader
	nextOrdinal  uint64
	headerSent   bool
	closed       bool
}

func (s *historySnapshot) validate(ctx context.Context) error {
	id := s.selection.leaf.identity
	s.history = archive.SourceHistory{ThreadID: id.ThreadID, ActiveRolloutID: id.RolloutID, OwnStart: id.SubagentOrdinal}
	for _, span := range s.spans {
		facts := span.file.identity
		if facts.ThreadID != id.ThreadID {
			continue
		}
		if facts.ForkID != "" && facts.ForkOrdinal == nil {
			return sourceFailure(agentapi.Unavailable, "fork ownership boundary unavailable")
		}
		for _, boundary := range []*uint64{facts.ForkOrdinal, facts.SubagentOrdinal} {
			if boundary != nil && (s.history.OwnStart == nil || *boundary > *s.history.OwnStart) {
				value := *boundary
				s.history.OwnStart = &value
			}
		}
	}
	count := 0
	for i := range s.spans {
		span := &s.spans[i]
		first := count
		facts, err := s.validateSpan(ctx, span)
		if err != nil {
			return err
		}
		if s.firstOwnTask == nil && span.file.identity.ThreadID == s.selection.thread && facts.firstTask.Seen {
			task := facts.firstTask
			s.firstOwnTask = &task
		}
		count += facts.records
		if count > archive.MaxHistoryRecords {
			return sourceFailure(agentapi.Limit, "history record limit")
		}
		span.digest, span.endOrdinal = facts.digest, facts.endOrdinal
		next := facts.endOrdinal
		if i+1 < len(s.spans) && next != s.spans[i+1].startOrdinal {
			return sourceFailure(agentapi.Unsafe, "history prefix ordinal mismatch")
		}
		s.history.Spans = append(s.history.Spans, archive.HistorySpan{RolloutID: span.file.identity.RolloutID, ThreadID: span.file.identity.ThreadID, FirstRecord: first, EndRecord: count, StartOrdinal: span.startOrdinal, EndOrdinal: next})
	}
	return s.history.Validate(id.ThreadID, count)
}

func (s *historySnapshot) Observation() agentapi.SourceObservation { return s.observed }

func (s *historySnapshot) Input() agentapi.NativeInput { return agentapi.NativeInput{Records: s} }

func (s *historySnapshot) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	delete(s.owner.live, s)
	s.lines.close()
	s.lines = nil
	for _, f := range s.headers {
		f.refs--
	}
	s.headers = nil
	s.spans = nil
	s.selection = sourceSelection{}
	return nil
}

func (s *historySnapshot) ValidateAdmission(ctx context.Context, a agentapi.SourceAdmission) error {
	s.admission = a.Binding
	if s.history.OwnStart == nil && a.Binding != nil && a.Binding.OwnStart != nil {
		s.history.OwnStart = a.Binding.OwnStart
		// Validation may have cached a task before retained admission supplied
		// its owned boundary. Historical reads validate admission before their
		// consumer gathers evidence, so that earlier result must be recomputed.
		s.firstOwnTask = nil
	}
	facts, err := s.AdmissionFacts(ctx)
	if err != nil {
		return err
	}
	if !validAdmissionFacts(s.selection.leaf, facts, a) {
		return sourceFailure(agentapi.Unsafe, "source admission facts changed")
	}
	if a.NativeID != s.selection.thread || a.Cwd != "" && a.Cwd != s.selection.leaf.meta.Cwd {
		return sourceFailure(agentapi.Unsafe, "source admission identity changed")
	}
	return s.check(ctx)
}

func (s *historySnapshot) check(ctx context.Context) error {
	for _, span := range s.spans {
		if err := span.file.file.CheckPrefix(ctx, span.end, span.digest); err != nil {
			return sourceio.Classify(err)
		}
	}
	if s.owner.env.CodexRollouts != nil {
		if err := s.owner.env.CodexRollouts.Check(ctx, s.selection.thread, s.selection.set.Revision); err != nil {
			return sourceio.Classify(err)
		}
	}
	return checkHeaders(ctx, s.headers)
}

func (s *historySnapshot) Next(ctx context.Context) (agentapi.NativeRecord, bool, error) {
	if s.closed || s.owner.closed {
		return agentapi.NativeRecord{}, false, agentapi.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return agentapi.NativeRecord{}, false, err
	}
	if !s.headerSent {
		s.headerSent = true
		return agentapi.NativeRecord{Kind: agentapi.CodexHistoryHeader, History: &s.history, Raw: s.selection.leaf.header}, true, nil
	}
	for s.span < len(s.spans) {
		span := s.spans[s.span]
		if s.lines == nil {
			lines, err := s.owner.newChargedLineReader(io.NewSectionReader(span.reader(), 0, span.end), s.recordLimit)
			if err != nil {
				return agentapi.NativeRecord{}, false, err
			}
			s.lines = lines
			s.nextOrdinal = span.startOrdinal
		}
		raw, more, err := s.lines.next(ctx)
		if err != nil {
			return agentapi.NativeRecord{}, false, sourceio.Classify(err)
		}
		if more {
			if len(bytes.TrimSpace(raw)) == 0 {
				continue
			}
			ordinal := s.nextOrdinal
			s.nextOrdinal++
			return agentapi.NativeRecord{Kind: agentapi.CodexHistoryRecord, Key: span.file.identity.RolloutID, Raw: raw, Ordinal: ordinal}, true, nil
		}
		s.lines.close()
		s.lines = nil
		s.span++
	}
	return agentapi.NativeRecord{}, false, s.check(ctx)
}

// ordinarySnapshot retains file framing while accepting verified later appends.
type ordinarySnapshot struct {
	scratchCharge int64
	owner         *relatedSourcePass
	source        *rolloutFile
	selection     sourceSelection
	ctx           context.Context
	length        int64
	digest        [32]byte
	closed        bool
	observed      agentapi.SourceObservation
	headers       []*rolloutFile
}

func (p *relatedSourcePass) ordinary(ctx context.Context, limits agentapi.ReadLimits, selection sourceSelection) (agentapi.SourceSnapshot, error) {
	f := selection.leaf
	if rawLimit := codexRawLimit(limits); f.file.Length() > rawLimit {
		stamp := f.file.Stamp()
		o := agentapi.SourceObservation{Present: true, Size: stamp.Size, Activity: stamp.ModifiedAt, Signature: sourceio.FileSignature(stamp.Size, stamp.ModifiedAt.UnixNano())}
		if p.env.CodexRollouts != nil {
			observed, err := p.observation(ctx, selection, []physicalSpan{{file: f, end: f.boundary}})
			if err != nil {
				return nil, errors.Join(agentapi.RawLimit(f.file.Length(), rawLimit, nil), err)
			}
			o = observed
		}
		return nil, agentapi.RawLimit(f.file.Length(), rawLimit, &o)
	}
	if !p.reserve(ordinaryScratch) {
		return nil, agentapi.ReadBudgetLimit(errors.New("ordinary native scratch budget exhausted"))
	}
	transferred := false
	defer func() {
		if !transferred {
			p.release(ordinaryScratch)
		}
	}()
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f.file.Reader(ctx), f.boundary)); err != nil {
		return nil, err
	}
	stamp := f.file.Stamp()
	out := &ordinarySnapshot{scratchCharge: ordinaryScratch, owner: p, source: f, selection: selection, headers: selection.headerFiles(nil), ctx: ctx, length: f.boundary, observed: agentapi.SourceObservation{Signature: sourceio.FileSignature(stamp.Size, stamp.ModifiedAt.UnixNano()), Present: true, Empty: f.boundary == 0, Activity: stamp.ModifiedAt, Size: stamp.Size}}
	copy(out.digest[:], h.Sum(nil))
	if p.env.CodexRollouts != nil {
		var err error
		out.observed, err = p.observation(ctx, selection, []physicalSpan{{file: f, end: f.boundary}})
		if err != nil {
			return nil, err
		}
	}
	if err := checkHeaders(ctx, out.headers); err != nil {
		return nil, err
	}
	for _, header := range out.headers {
		header.refs++
	}
	p.ordinaryLive[out] = true
	transferred = true
	return out, nil
}

func (s *ordinarySnapshot) Observation() agentapi.SourceObservation { return s.observed }

func (s *ordinarySnapshot) Input() agentapi.NativeInput {
	return agentapi.NativeInput{File: ordinaryFile{FileInput: s.source.file, s: s}}
}

func (s *ordinarySnapshot) Close() error {
	if !s.closed {
		s.closed = true
		delete(s.owner.ordinaryLive, s)
		s.owner.release(s.scratchCharge)
		s.scratchCharge = 0
		for _, header := range s.headers {
			header.refs--
		}
		s.headers = nil
	}
	return nil
}

type ordinaryFile struct {
	agentapi.FileInput
	s *ordinarySnapshot
}

func (f ordinaryFile) Length() int64 { return f.s.length }

func (f ordinaryFile) ReadAt(b []byte, off int64) (int, error) {
	if f.s.closed || f.s.owner.closed {
		return 0, agentapi.ErrClosed
	}
	return f.FileInput.ReadAt(b, off)
}

func (f ordinaryFile) Check() error {
	if f.s.closed || f.s.owner.closed {
		return agentapi.ErrClosed
	}
	return f.s.check(f.s.ctx)
}

// newlineBoundary excludes even valid JSON until its producer commits the newline.
func newlineBoundary(file io.ReaderAt, size int64) (int64, error) {
	if size == 0 {
		return 0, nil
	}
	var last [1]byte
	if _, err := file.ReadAt(last[:], size-1); err != nil {
		return 0, err
	}
	if last[0] == '\n' {
		return size, nil
	}
	buf := make([]byte, 64<<10)
	floor := max(int64(0), size-archive.MaxRecordBytes-1)
	for end := size; end > floor; {
		start := max(floor, end-int64(len(buf)))
		part := buf[:end-start]
		if _, err := file.ReadAt(part, start); err != nil {
			return 0, err
		}
		if i := bytes.LastIndexByte(part, '\n'); i >= 0 {
			return start + int64(i) + 1, nil
		}
		end = start
	}
	return 0, agentapi.Wrap(agentapi.Limit, archive.ErrRecordTooLarge)
}

// chargedNewlineBoundary preserves the history producer's newline commit
// boundary. Ordinary JSONL may accept a valid unterminated record instead.
func (p *relatedSourcePass) chargedNewlineBoundary(file *transcriptio.Snapshot) (int64, error) {
	size := file.Length()
	if size == 0 {
		return 0, nil
	}
	var last [1]byte
	if _, err := file.ReadAt(last[:], size-1); err != nil {
		return 0, err
	}
	if last[0] == '\n' {
		return size, nil
	}
	const scratch = 64 << 10
	if !p.reserve(scratch) {
		return 0, agentapi.ReadBudgetLimit(errors.New("native newline scratch budget exhausted"))
	}
	defer p.release(scratch)
	return newlineBoundary(file, size)
}

func (f ordinaryFile) Records(ctx context.Context, tail bool, windowBytes, recordBytes int64, visit func([]byte) bool) (transcriptio.RecordWindow, error) {
	if f.s.closed || f.s.owner.closed {
		return transcriptio.RecordWindow{}, agentapi.ErrClosed
	}
	charge := max(int64(4096), min(f.s.length, max(recordBytes, windowBytes))) + max(windowBytes, 0) + 64<<10
	if !f.s.owner.reserve(charge) {
		return transcriptio.RecordWindow{}, sourceFailure(agentapi.Unavailable, "native record window budget exhausted")
	}
	defer f.s.owner.release(charge)
	return f.FileInput.Records(ctx, tail, windowBytes, recordBytes, visit)
}

func (s *ordinarySnapshot) ValidateAdmission(ctx context.Context, a agentapi.SourceAdmission) error {
	facts, err := s.AdmissionFacts(ctx)
	if err != nil {
		return err
	}
	if a.Binding != nil {
		facts.FirstNativeTaskAt = a.Binding.FirstNativeTaskAt
		facts.FirstNativeTaskID = a.Binding.FirstNativeTaskID
	}
	if !validAdmissionFacts(s.source, facts, a) {
		return sourceFailure(agentapi.Unsafe, "source admission facts changed")
	}
	if s.closed || s.owner.closed {
		return agentapi.ErrClosed
	}
	if a.NativeID != s.source.identity.ThreadID || a.Cwd != "" && a.Cwd != s.source.meta.Cwd {
		return sourceFailure(agentapi.Unsafe, "source admission identity changed")
	}
	return s.check(ctx)
}

func (s *ordinarySnapshot) check(ctx context.Context) error {
	if err := s.source.file.CheckPrefix(ctx, s.length, s.digest); err != nil {
		return sourceio.Classify(err)
	}
	if s.owner.env.CodexRollouts != nil {
		if err := s.owner.env.CodexRollouts.Check(ctx, s.selection.thread, s.selection.set.Revision); err != nil {
			return sourceio.Classify(err)
		}
	}
	return checkHeaders(ctx, s.headers)
}

func bindingFacts(f *rolloutFile, home string, own *uint64) (archive.CodexSourceBinding, error) {
	_, created, found, err := codexmeta.ParseCodexMeta(f.header)
	if err != nil || !found || created.IsZero() {
		return archive.CodexSourceBinding{}, sourceFailure(agentapi.Unavailable, "native creation evidence unavailable")
	}
	var source string
	rawSource := f.meta.Source
	if projected, known := f.meta.ExecutionSourceFacts(); known {
		rawSource = projected
	}
	if json.Unmarshal(rawSource, &source) != nil && len(rawSource) > 0 {
		var value any
		if err := json.Unmarshal(rawSource, &value); err != nil {
			return archive.CodexSourceBinding{}, err
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return archive.CodexSourceBinding{}, err
		}
		source = string(encoded)
	}
	root := f.identity.RootID
	if root == "" && !f.identity.Child && f.identity.ForkID == "" {
		root = f.identity.ThreadID
	}
	facts := archive.CodexSourceBinding{Version: 1, Child: f.identity.Child, NativeThreadID: f.identity.ThreadID, NativeCreatedAt: created, Cwd: f.meta.Cwd, SelectedCwd: f.meta.Cwd, ProducerSource: source, PhysicalProducerVersion: f.meta.Version, PhysicalProducerOriginator: f.meta.Originator, RootID: root, ParentID: f.identity.ParentID, OwnStart: own, PhysicalRolloutID: f.identity.RolloutID, Path: f.ref.Path, Home: home}
	err = facts.Validate()
	return facts, err
}

func (s *ordinarySnapshot) AdmissionFacts(ctx context.Context) (archive.CodexSourceBinding, error) {
	if s.closed || s.owner.closed {
		return archive.CodexSourceBinding{}, agentapi.ErrClosed
	}
	if err := s.source.file.CheckPrefix(ctx, s.length, s.digest); err != nil {
		return archive.CodexSourceBinding{}, sourceio.Classify(err)
	}
	return bindingFacts(s.source, s.owner.env.Policy.Root, nil)
}

func (s *historySnapshot) AdmissionFacts(ctx context.Context) (archive.CodexSourceBinding, error) {
	if s.closed || s.owner.closed {
		return archive.CodexSourceBinding{}, agentapi.ErrClosed
	}
	if err := s.check(ctx); err != nil {
		return archive.CodexSourceBinding{}, err
	}
	return s.owner.historyBindingFacts(ctx, s.selection, s.spans, s.history.OwnStart, s.admission)

}

func (p *relatedSourcePass) ValidateSourceAdmission(ctx context.Context, ref agentapi.SourceRef, admission agentapi.SourceAdmission) error {
	if p.env.CodexRollouts != nil && p.genericLegacy(ref) && admission.Binding == nil && admission.NativeCreatedAt.IsZero() {
		if admission.NativeID != ref.Key {
			return sourceFailure(agentapi.Unsafe, "legacy admission identity mismatch")
		}
		set, err := p.registeredLegacySet(ctx, ref)
		if err != nil {
			return err
		}
		return p.env.CodexRollouts.Check(ctx, ref.Key, set.Revision)
	}
	selection, err := p.selectSource(ctx, ref)
	if err != nil {
		return err
	}
	if selection.legacyOrdinary && (admission.Binding != nil || !admission.NativeCreatedAt.IsZero()) {
		return sourceFailure(agentapi.Unavailable, "legacy compatibility requires unbound admission")
	}
	if p.env.RequireConfinedHistory && p.env.Policy.Root == "" && hasRelated(selection.leaf) {
		return sourceFailure(agentapi.Unavailable, "related admission requires confined source home")
	}
	spans, err := p.graph(ctx, selection.leaf, selection.set)
	if err != nil {
		return err
	}
	var own *uint64
	for _, span := range spans {
		if span.file.identity.ThreadID != selection.thread {
			continue
		}
		for _, boundary := range []*uint64{span.file.identity.ForkOrdinal, span.file.identity.SubagentOrdinal} {
			if boundary != nil && (own == nil || *boundary > *own) {
				v := *boundary
				own = &v
			}
		}
		digest := sha256.Sum256(span.file.header)
		if err := span.file.file.CheckPrefix(ctx, int64(len(span.file.header)), digest); err != nil {
			return sourceio.Classify(err)
		}
	}
	facts, err := p.historyBindingFacts(ctx, selection, spans, own, admission.Binding)
	if err != nil {
		return err
	}
	if selection.thread != admission.NativeID || admission.Cwd != "" && facts.Cwd != admission.Cwd || !validAdmissionFacts(selection.leaf, facts, admission) {
		return sourceFailure(agentapi.Unsafe, "source admission facts changed")
	}
	if p.env.CodexRollouts != nil {
		return p.env.CodexRollouts.Check(ctx, selection.thread, selection.set.Revision)
	}
	return ctx.Err()
}

func validAdmissionFacts(f *rolloutFile, facts archive.CodexSourceBinding, a agentapi.SourceAdmission) bool {
	if a.Binding != nil && a.Binding.PhysicalRolloutID == f.identity.RolloutID {
		if a.Binding.PhysicalProducerVersion != "" && a.Binding.PhysicalProducerVersion != f.meta.Version || a.Binding.PhysicalProducerOriginator != "" && a.Binding.PhysicalProducerOriginator != f.meta.Originator {
			return false
		}
	}
	return facts.PreservesFacts(a.Binding) && (a.NativeCreatedAt.IsZero() || facts.NativeCreatedAt.Equal(a.NativeCreatedAt)) && (a.InitialProducerVersion == "" || f.meta.Version == a.InitialProducerVersion) && (a.InitialProducerOriginator == "" || f.meta.Originator == a.InitialProducerOriginator) && (a.InitialProducerSource == "" || facts.ProducerSource == a.InitialProducerSource)
}

// prefixValidation is cached only for one immutable, fully captured ancestor prefix.
type prefixValidation struct {
	taskOwned      bool
	taskBoundary   uint64
	firstTask      agentapi.OwnTaskFacts
	startOrdinal   uint64
	endOrdinal     uint64
	records        int
	maxRecordBytes int64
	digest         [32]byte
}

const prefixSummaryCharge int64 = 128

func (f *rolloutFile) charge() int64 {
	charge := sourceHeaderCharge
	if f.prefix != nil {
		charge += int64(len(f.prefix)) + prefixSummaryCharge
	}
	return charge
}

func (span physicalSpan) reader() io.ReaderAt {
	if int64(len(span.file.prefix)) >= span.end {
		return bytes.NewReader(span.file.prefix[:span.end])
	}
	return span.file.file
}

func (p *relatedSourcePass) cachePrefixes(ctx context.Context, spans []physicalSpan) error {
	for _, span := range spans[:len(spans)-1] {
		f := span.file
		if f.prefix != nil || p.prefixBytes+span.end > prefixCacheLimit || span.end+prefixSummaryCharge > p.env.ReadBudget.Available() {
			continue
		}
		// The one bounded immutable copy is shared by descendants; never replace it
		// while a snapshot might hold a borrowed scanner over its bytes.
		charge := span.end + prefixSummaryCharge
		if !p.reserveCache(charge) {
			continue
		}
		raw := make([]byte, span.end)
		reader := sourceio.Reader(ctx, f.file, span.end)
		if _, err := io.ReadFull(reader, raw); err != nil {
			p.releaseCache(charge)
			return sourceio.Classify(err)
		}
		f.prefix = raw
		p.prefixBytes += span.end
	}
	return nil
}

// codexRawLimit is the raw size a read may cover: the caller's limit, never
// more than archive.MaxCodexRawTranscriptBytes.
func codexRawLimit(limits agentapi.ReadLimits) int64 {
	if limits.RawBytes > 0 {
		return min(limits.RawBytes, archive.MaxCodexRawTranscriptBytes)
	}
	return archive.MaxCodexRawTranscriptBytes
}

func (s *historySnapshot) validateSpan(ctx context.Context, span *physicalSpan) (prefixValidation, error) {
	cached := span.file.validated
	if cached != nil && int64(len(span.file.prefix)) == span.end && cached.startOrdinal == span.startOrdinal {
		if cached.maxRecordBytes > s.recordLimit {
			return prefixValidation{}, agentapi.Wrap(agentapi.Limit, archive.ErrRecordTooLarge)
		}
		s.owner.cacheHits++
		result := *cached
		boundary := uint64(0)
		if s.history.OwnStart != nil {
			boundary = *s.history.OwnStart
		}
		if span.file.identity.ThreadID != s.selection.thread || !cached.taskOwned || cached.taskBoundary != boundary {
			result.firstTask = agentapi.OwnTaskFacts{}
		}
		return result, ctx.Err()
	}
	boundary := uint64(0)
	if s.history.OwnStart != nil {
		boundary = *s.history.OwnStart
	}
	facts := prefixValidation{startOrdinal: span.startOrdinal, endOrdinal: span.startOrdinal, taskOwned: span.file.identity.ThreadID == s.selection.thread, taskBoundary: boundary}

	h := sha256.New()
	lines, err := s.owner.newChargedLineReader(io.TeeReader(io.NewSectionReader(span.reader(), 0, span.end), h), s.recordLimit)
	if err != nil {
		return facts, err
	}
	defer lines.close()
	for {
		raw, more, err := lines.next(ctx)
		if err != nil {
			return facts, sourceio.Classify(err)
		}
		if !more {
			break
		}
		facts.maxRecordBytes = max(facts.maxRecordBytes, int64(len(raw)))
		line := bytes.TrimSpace(raw)
		if len(line) == 0 {
			continue
		}
		var record struct {
			Ordinal *uint64 `json:"ordinal"`
		}
		if json.Unmarshal(line, &record) != nil {
			return facts, sourceFailure(agentapi.FormatMismatch, "invalid history record")
		}
		if span.file.identity.HistoryMode == codexmeta.CodexHistoryPaginated && record.Ordinal == nil {
			return facts, sourceFailure(agentapi.Unsafe, "missing history ordinal")
		}
		if record.Ordinal != nil && *record.Ordinal != facts.endOrdinal {
			return facts, sourceFailure(agentapi.Unsafe, "noncontiguous history ordinal")
		}
		if facts.endOrdinal == math.MaxUint64 {
			return facts, sourceFailure(agentapi.Limit, "history ordinal overflow")
		}
		if facts.taskOwned && !facts.firstTask.Seen && facts.endOrdinal >= facts.taskBoundary {
			facts.firstTask = ownTaskFacts(line, localExecutionShape(span.file))
		}
		facts.endOrdinal++
		facts.records++
		if facts.records > archive.MaxHistoryRecords {
			return facts, sourceFailure(agentapi.Limit, "history record limit")
		}
	}
	copy(facts.digest[:], h.Sum(nil))
	if int64(len(span.file.prefix)) == span.end {
		span.file.validated = &facts
	}
	return facts, nil
}

func (p *relatedSourcePass) ReadRevision(ctx context.Context, ref agentapi.SourceRef, admission agentapi.SourceAdmission, limits agentapi.ReadLimits) (agentapi.SourceSnapshot, error) {
	leaf, err := p.open(ctx, ref)
	if err != nil {
		return nil, err
	}
	if admission.NativeID == "" || leaf.identity.ThreadID != admission.NativeID {
		return nil, sourceFailure(agentapi.Unsafe, "historical revision belongs to another thread")
	}
	var set agentapi.CodexRolloutSet
	if p.env.CodexRollouts != nil {
		set, err = p.env.CodexRollouts.Thread(ctx, admission.NativeID)
		if err != nil {
			return nil, err
		}
	}
	selection := sourceSelection{leaf: leaf, thread: admission.NativeID, set: set}
	snapshot, err := p.readHistorySelection(ctx, limits, selection)
	if err != nil {
		return nil, err
	}
	validator, ok := snapshot.(agentapi.SourceAdmissionValidator)
	if !ok {
		return nil, errors.Join(sourceFailure(agentapi.Unsafe, "historical admission validator unavailable"), snapshot.Close())
	}
	if err := validator.ValidateAdmission(ctx, admission); err != nil {
		return nil, errors.Join(err, snapshot.Close())
	}
	return snapshot, nil
}

func (s *historySnapshot) RevisionCandidates(ctx context.Context) ([]agentapi.SourceRef, error) {
	if s.closed || s.owner.closed {
		return nil, agentapi.ErrClosed
	}
	if err := s.check(ctx); err != nil {
		return nil, err
	}
	if s.owner.env.CodexRollouts != nil && !s.selection.set.Complete {
		return nil, coverageIncomplete("historical candidate coverage incomplete")
	}
	out := make([]agentapi.SourceRef, 0, archive.MaxHistorySpans)
	seen := map[string]bool{}
	add := func(ref agentapi.SourceRef) error {
		if seen[ref.Path] {
			return nil
		}
		if len(out) >= archive.MaxHistorySpans {
			return sourceFailure(agentapi.Limit, "historical candidate limit")
		}
		seen[ref.Path] = true
		out = append(out, ref)
		return nil
	}
	for _, span := range s.spans {
		if span.file.identity.ThreadID == s.selection.thread {
			if err := add(span.file.ref); err != nil {
				return nil, err
			}
		}
	}
	for _, ref := range s.selection.set.Candidates {
		if err := add(ref); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *ordinarySnapshot) RevisionCandidates(ctx context.Context) ([]agentapi.SourceRef, error) {
	if _, err := s.AdmissionFacts(ctx); err != nil {
		return nil, err
	}
	if s.owner.env.CodexRollouts != nil && !s.selection.set.Complete {
		return nil, coverageIncomplete("historical candidate coverage incomplete")
	}
	out := []agentapi.SourceRef{s.source.ref}
	seen := map[string]bool{s.source.ref.Path: true}
	for _, ref := range s.selection.set.Candidates {
		if seen[ref.Path] {
			continue
		}
		if len(out) >= archive.MaxHistorySpans {
			return nil, sourceFailure(agentapi.Limit, "historical candidate limit")
		}
		seen[ref.Path] = true
		out = append(out, ref)
	}
	return out, nil
}

func (p *relatedSourcePass) historyBindingFacts(ctx context.Context, selection sourceSelection, spans []physicalSpan, own *uint64, known *archive.CodexSourceBinding) (archive.CodexSourceBinding, error) {
	var original *rolloutFile
	for _, span := range spans {
		if span.file.identity.ThreadID == selection.thread && span.file.identity.RolloutID == selection.thread {
			original = span.file
			break
		}
	}
	if original == nil && known == nil && p.env.CodexRollouts != nil {
		refs, err := p.env.CodexRollouts.Rollout(ctx, selection.thread)
		if err != nil {
			return archive.CodexSourceBinding{}, err
		}
		if len(refs) > archive.MaxHistorySpans {
			return archive.CodexSourceBinding{}, sourceFailure(agentapi.Limit, "original fact candidate limit")
		}
		for _, ref := range refs {
			candidate, err := p.open(ctx, ref)
			if err != nil {
				return archive.CodexSourceBinding{}, err
			}
			if candidate.identity.ThreadID == selection.thread && candidate.identity.RolloutID == selection.thread {
				if original != nil && original.ref.Path != candidate.ref.Path {
					return archive.CodexSourceBinding{}, p.coverageRefusal(selection.set, "original native identity ambiguous")
				}
				original = candidate
			}
		}
	}
	var facts archive.CodexSourceBinding
	if original != nil {
		var err error
		facts, err = bindingFacts(original, p.env.Policy.Root, own)
		if err != nil {
			return archive.CodexSourceBinding{}, err
		}
	} else if known != nil {
		facts = *known
		if own != nil {
			facts.OwnStart = own
		}
	} else {
		return archive.CodexSourceBinding{}, p.coverageRefusal(selection.set, "original native creation facts unavailable")
	}
	for _, span := range spans {
		id := span.file.identity
		if id.ThreadID != selection.thread {
			continue
		}
		if id.RootID != "" {
			if facts.RootID != "" && facts.RootID != id.RootID {
				return archive.CodexSourceBinding{}, sourceFailure(agentapi.Unsafe, "native root identity conflict")
			}
			facts.RootID = id.RootID
		}
		if id.ParentID != "" {
			if facts.ParentID != "" && facts.ParentID != id.ParentID {
				return archive.CodexSourceBinding{}, sourceFailure(agentapi.Unsafe, "native parent identity conflict")
			}
			facts.ParentID = id.ParentID
		}
		if id.Child && !facts.Child {
			return archive.CodexSourceBinding{}, sourceFailure(agentapi.Unsafe, "native child identity conflict")
		}
	}
	if known != nil {
		facts.FirstNativeTaskAt = known.FirstNativeTaskAt
		facts.FirstNativeTaskID = known.FirstNativeTaskID
	}
	facts.PhysicalRolloutID = selection.leaf.identity.RolloutID
	facts.Path = selection.leaf.ref.Path
	facts.SelectedCwd = selection.leaf.meta.Cwd
	facts.PhysicalProducerVersion = selection.leaf.meta.Version
	facts.PhysicalProducerOriginator = selection.leaf.meta.Originator
	err := facts.Validate()
	return facts, err
}

func ownTaskFacts(line []byte, local bool) agentapi.OwnTaskFacts {
	seen, native := codexmeta.NativeFirstTask(line)
	if !seen {
		return agentapi.OwnTaskFacts{}
	}
	var event struct {
		Payload struct {
			TurnID string `json:"turn_id"`
		} `json:"payload"`
	}
	_ = json.Unmarshal(line, &event)
	return agentapi.OwnTaskFacts{Seen: true, Native: native, LocalExecution: local, StartedAt: codexmeta.FirstTaskAt(line), TurnID: event.Payload.TurnID}
}

func localExecutionShape(f *rolloutFile) bool {
	return f.meta.LocalExecutionSource()
}

func (s *historySnapshot) FirstOwnTask(ctx context.Context) (agentapi.OwnTaskFacts, error) {
	facts, err := s.gatherOwnTask(ctx)
	if err != nil {
		return facts, err
	}
	return facts, s.check(ctx)
}

func (s *historySnapshot) gatherOwnTask(ctx context.Context) (agentapi.OwnTaskFacts, error) {
	if s.closed || s.owner.closed {
		return agentapi.OwnTaskFacts{}, agentapi.ErrClosed
	}
	if s.firstOwnTask != nil {
		return *s.firstOwnTask, ctx.Err()
	}
	for _, span := range s.spans {
		if span.file.identity.ThreadID != s.selection.thread {
			continue
		}
		ordinal := span.startOrdinal
		facts := agentapi.OwnTaskFacts{}
		err := s.owner.scanChargedLines(ctx, io.NewSectionReader(span.file.file, 0, span.end), s.recordLimit, func(line []byte) bool {
			line = bytes.TrimSpace(line)
			if len(line) == 0 {
				return false
			}
			current := ordinal
			ordinal++
			if s.history.OwnStart != nil && current < *s.history.OwnStart {
				return false
			}
			facts = ownTaskFacts(line, localExecutionShape(span.file))
			return facts.Seen
		})
		if err != nil {
			return agentapi.OwnTaskFacts{}, err
		}
		if facts.Seen {
			s.firstOwnTask = &facts
			return facts, nil
		}

	}
	facts := agentapi.OwnTaskFacts{}
	s.firstOwnTask = &facts
	return facts, ctx.Err()
}

func (s *ordinarySnapshot) FirstOwnTask(ctx context.Context) (agentapi.OwnTaskFacts, error) {
	if s.closed || s.owner.closed {
		return agentapi.OwnTaskFacts{}, agentapi.ErrClosed
	}
	facts := agentapi.OwnTaskFacts{}
	err := s.owner.scanChargedLines(ctx, io.NewSectionReader(s.source.file, 0, s.length), archive.MaxRecordBytes, func(line []byte) bool {
		facts = ownTaskFacts(bytes.TrimSpace(line), localExecutionShape(s.source))
		return facts.Seen
	})
	if err != nil {
		return agentapi.OwnTaskFacts{}, err
	}
	return facts, (ordinaryFile{FileInput: s.source.file, s: s}).Check()

}

func (s *historySnapshot) AdmissionEvidence(ctx context.Context, admission agentapi.SourceAdmission) (agentapi.AdmissionEvidence, error) {
	if s.closed || s.owner.closed {
		return agentapi.AdmissionEvidence{}, agentapi.ErrClosed
	}
	s.admission = admission.Binding
	if s.history.OwnStart == nil && admission.Binding != nil && admission.Binding.OwnStart != nil {
		s.history.OwnStart = admission.Binding.OwnStart
		s.firstOwnTask = nil
	}
	binding, err := s.owner.historyBindingFacts(ctx, s.selection, s.spans, s.history.OwnStart, s.admission)
	if err != nil {
		return agentapi.AdmissionEvidence{}, err
	}
	if admission.NativeID != s.selection.thread || admission.Cwd != "" && admission.Cwd != s.selection.leaf.meta.Cwd || !validAdmissionFacts(s.selection.leaf, binding, admission) {
		return agentapi.AdmissionEvidence{}, sourceFailure(agentapi.Unsafe, "source admission facts changed")
	}
	task, err := s.gatherOwnTask(ctx)
	if err != nil {
		return agentapi.AdmissionEvidence{}, err
	}
	evidence := agentapi.AdmissionEvidence{Binding: binding, Task: task}
	return evidence, s.check(ctx)
}

func (s *ordinarySnapshot) AdmissionEvidence(ctx context.Context, admission agentapi.SourceAdmission) (agentapi.AdmissionEvidence, error) {
	if s.closed || s.owner.closed {
		return agentapi.AdmissionEvidence{}, agentapi.ErrClosed
	}
	binding, err := bindingFacts(s.source, s.owner.env.Policy.Root, nil)
	if err != nil {
		return agentapi.AdmissionEvidence{}, err
	}
	if admission.Binding != nil {
		binding.FirstNativeTaskAt = admission.Binding.FirstNativeTaskAt
		binding.FirstNativeTaskID = admission.Binding.FirstNativeTaskID
	}
	if admission.NativeID != s.source.identity.ThreadID || admission.Cwd != "" && admission.Cwd != s.source.meta.Cwd || !validAdmissionFacts(s.source, binding, admission) {
		return agentapi.AdmissionEvidence{}, sourceFailure(agentapi.Unsafe, "source admission facts changed")
	}
	task, err := s.FirstOwnTask(ctx)
	if err != nil {
		return agentapi.AdmissionEvidence{}, err
	}
	return agentapi.AdmissionEvidence{Binding: binding, Task: task}, nil
}

func (p *relatedSourcePass) reserve(bytes int64) bool {
	if bytes < 0 || !p.env.ReadBudget.Reserve(bytes) {
		return false
	}
	p.bytes += bytes
	return true
}

func (p *relatedSourcePass) release(bytes int64) { p.env.ReadBudget.Release(bytes); p.bytes -= bytes }

// reserveCache charges what the pass keeps between reads (cached headers and
// ancestor prefixes): no session's own, so no session's scope counts it.
func (p *relatedSourcePass) reserveCache(bytes int64) bool {
	if bytes < 0 || !p.env.ReadBudget.ReserveUnscoped(bytes) {
		return false
	}
	p.bytes += bytes
	return true
}

// reserveHeader charges a cached rollout's header like reserveCache, but
// the read cannot go on without it: a refusal marks the session's scope
// pressured, so the session is deferred rather than failed.
func (p *relatedSourcePass) reserveHeader() bool {
	if !p.env.ReadBudget.ReserveUnscopedRequired(sourceHeaderCharge) {
		return false
	}
	p.bytes += sourceHeaderCharge
	return true
}

func (p *relatedSourcePass) releaseCache(bytes int64) {
	p.env.ReadBudget.ReleaseUnscoped(bytes)
	p.bytes -= bytes
}

// ReleaseCaches drops every cached file no snapshot holds, with its header
// and any ancestor prefix, so the next session's scan starts with the pass
// ledger as free as the pass can make it.
func (p *relatedSourcePass) ReleaseCaches() error {
	if p.closed {
		return nil
	}
	return p.evict()
}

func (p *relatedSourcePass) lookupThread(ctx context.Context, id string) (agentapi.CodexRolloutSet, error) {
	set, err := p.env.CodexRollouts.Thread(ctx, id)
	if agentapi.Failure(err) == agentapi.Limit {
		if evictionErr := p.evict(); evictionErr != nil {
			return set, errors.Join(err, evictionErr)
		}
		return p.env.CodexRollouts.Thread(ctx, id)
	}
	return set, err
}

func (p *relatedSourcePass) initialLookupRef(ctx context.Context, ref agentapi.SourceRef) (agentapi.SourceRef, error) {
	if p.env.CodexRollouts != nil && ref.Key != "" && codexmeta.RolloutID(ref.Key+".jsonl") == ref.Key {
		set, lookupErr := p.lookupThread(ctx, ref.Key)
		if lookupErr != nil {
			return ref, lookupErr
		}
		if set.Current != nil {
			ref = *set.Current
		} else if set.Complete && len(set.Candidates) > 0 {
			ref = set.Candidates[0]
		}
	}
	return ref, nil
}

func (p *relatedSourcePass) chargedBoundary(f *transcriptio.Snapshot) (int64, error) {
	if f.Length() == 0 {
		return 0, nil
	}
	var last [1]byte
	if _, err := f.ReadAt(last[:], f.Length()-1); err != nil {
		return 0, err
	}
	if last[0] == '\n' {
		return f.Length(), nil
	}
	charge := min(f.Length(), int64(archive.MaxRecordBytes)) + 64<<10
	if !p.reserve(charge) {
		return 0, sourceFailure(agentapi.Unavailable, "native boundary scratch budget exhausted")
	}
	defer p.release(charge)
	return transcriptio.CompleteJSONLBoundary(f, f.Length(), archive.MaxRecordBytes)
}

// A transient shared reservation refusal leaves idle header/proof owners intact.
// Other failed reads retain the existing cleanup of unusable private snapshots.
func (p *relatedSourcePass) readFailure(err error) (agentapi.SourceSnapshot, error) {
	if errors.Is(err, agentapi.ErrReadBudget) {
		return nil, err
	}
	return nil, errors.Join(err, p.evict())
}
