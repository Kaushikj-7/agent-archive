package collector

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"github.com/wangjohn/agent-archive/internal/sourceio"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

// sourceState carries a provider observation and legacy equality facts.
type sourceState struct {
	binding *archive.CodexSourceBinding
	// relocated is where an unbound Codex registration's rollout was read
	// from when Codex had renamed it (see agentapi.SourceRelocation).
	relocated   string
	kind        archive.SourceKind
	file        transcriptFileInfo
	cursor      cursorstore.Signature
	observation agentapi.SourceObservation
}

func (s sourceState) empty() bool {
	if s.observation.Present {
		return s.observation.Empty
	}
	if s.kind == archive.SourceKindCursorSQLite {
		return s.cursor.HeaderCount == 0
	}
	return s.file.Size == 0
}

// size is raw observation data, independent of native locator framing.
func (s sourceState) size() int64 {
	if s.observation.Present {
		return s.observation.Size
	}
	return s.file.Size
}

func (s sourceState) matches(old state.ScanSignature) bool {
	if old.SourceKind != s.kind {
		return false
	}
	if s.observation.Present {
		if old.SourceSignature != nil {
			return *old.SourceSignature == s.observation.Signature
		}
		if s.kind == archive.SourceKindCursorSQLite {
			return sourceio.CursorSignature(old.CursorSignature()) == s.observation.Signature
		}
		return sourceio.FileSignature(old.TranscriptSize, old.TranscriptMtime) == s.observation.Signature
	}
	if s.kind == archive.SourceKindCursorSQLite {
		return old.CursorSignature() == s.cursor
	}
	return s.file == transcriptFileInfo{Size: old.TranscriptSize, Mtime: old.TranscriptMtime}
}

func observe(kind archive.SourceKind, o agentapi.SourceObservation) sourceState {
	var file transcriptFileInfo
	if kind == archive.SourceKindFile {
		file = transcriptFileInfo{Size: o.Size, Mtime: o.Activity.UnixNano()}
	}
	return sourceState{kind: kind, observation: o, file: file}
}

func sourceRef(reg archive.SessionRegistration) agentapi.SourceRef {
	if reg.CodexBinding != nil {
		return agentapi.SourceRef{Kind: archive.SourceKindFile, Path: reg.CodexBinding.Path, Key: reg.NativeSessionID}
	}
	key := reg.SourceKey
	if reg.Harness.Name == "codex" && reg.ReadsTranscriptFile() {
		key = reg.NativeSessionID
	}
	return agentapi.SourceRef{Kind: reg.SourceKind, Path: reg.TranscriptPath, Key: key}
}

// sourceReader remains a small compatibility seam while policy consumes provider values.
type sourceReader interface {
	Signature(context.Context) (sourceState, error)
	Filter(context.Context, archive.Adapter, int64) (archive.FilteredTranscript, sourceState, error)
}

func newSourceReader(reg archive.SessionRegistration, opts Options) (sourceReader, bool) {
	if reg.ReadsTranscriptFile() && reg.TranscriptPath == "" {
		return nil, false
	}
	var allowed func(string) bool
	if reg.HistoricalProject != nil {
		allowed = opts.HistoricalSourceAllowed
	}
	return providerReader{historicalSourceAllowed: allowed,
		ref: sourceRef(reg), harness: reg.Harness.Name, startedAt: reg.SessionStartedAt,
		subagentMetadata: reg.ParentSessionID != "", sources: opts.Sources,
		passes: opts.sourcePasses, resourceOwner: opts.retainedOwner, database: opts.CursorDatabase, rollouts: opts.CodexRollouts, pendingRollouts: opts.PendingCodexRollouts,
		admission: sourceAdmission(reg),
		discovery: confinedSourceRegistration(reg, opts.ConfiguredCodexHomes),
	}, true
}

// providerReader keeps only read dependencies; boxing whole registrations and
// collector options would allocate their unrelated policy fields per source.
type providerReader struct {
	historicalSourceAllowed func(string) bool
	pendingRollouts         func() agentapi.CodexRolloutLookup
	resourceOwner           *sessionScan
	admission               agentapi.SourceAdmission
	rollouts                agentapi.CodexRolloutLookup
	ref                     agentapi.SourceRef
	harness                 string
	startedAt               time.Time
	subagentMetadata        bool
	sources                 agentapi.SourcesLookup
	passes                  *sourcePassSet
	database                string
	discovery               *archive.SessionRegistration
}

// discoveryRegistration retains only discovery authority for strict native reads.
func discoveryRegistration(reg archive.SessionRegistration) *archive.SessionRegistration {
	if reg.Origin != archive.SessionOriginDiscovery && (reg.CodexBinding == nil || reg.CodexBinding.Home == "") {
		return nil
	}
	return &reg
}

// confinedSourceRegistration recognizes only an admitted native locator below
// an actual configured home. Lookup hints never supply containment authority.
func confinedSourceRegistration(reg archive.SessionRegistration, homes []string) *archive.SessionRegistration {
	if confined := discoveryRegistration(reg); confined != nil {
		return confined
	}
	ref := sourceRef(reg)
	if reg.Harness.Name != archive.HarnessCodex || sourcefacts.RolloutID(ref.Path) == "" {
		return nil
	}
	root := ""
	for _, home := range homes {
		canonical, err := filepath.EvalSymlinks(home)
		if err != nil || !filepath.IsAbs(canonical) {
			continue
		}
		info, err := os.Stat(canonical)
		if err != nil || !info.IsDir() {
			continue
		}
		// The configured root itself may use a platform alias (for example
		// /var on macOS). OpenRoot still confines every descendant and rejects
		// native symlinks; the alias does not authorize another home.
		for _, scope := range []string{filepath.Clean(home), canonical} {
			if !filepath.IsAbs(scope) || (!local.PathWithin(ref.Path, filepath.Join(scope, "sessions")) && !local.PathWithin(ref.Path, filepath.Join(scope, "archived_sessions"))) {
				continue
			}
			if root == "" || len(scope) > len(root) {
				root = scope
			}
		}
	}
	if root == "" {
		return nil
	}
	reg.DiscoveryRoot = root // transient read policy only; persisted provenance is unchanged.
	return &reg
}

// sourceEnvironment keeps discovered files confined to their admitted source home.
// Hooks and backfill retain the provider's existing default filesystem semantics.
func sourceEnvironment(reg *archive.SessionRegistration, database string) agentapi.SourceEnvironment {
	db := (Options{CursorDatabase: database}).cursorDatabase()
	if reg != nil {
		root := reg.DiscoveryRoot
		if reg.CodexBinding != nil && reg.CodexBinding.Home != "" {
			root = reg.CodexBinding.Home
		}
		return agentapi.SourceEnvironment{
			Database: db, RequireConfinedHistory: true,
			Files:  sourcefacts.RootOpener{Root: root},
			Policy: transcriptio.OpenPolicy{Root: root, RejectSymlinks: true},
		}
	}
	return agentapi.SourceEnvironment{Database: db, RequireConfinedHistory: true}
}

func (r providerReader) binding() (agentapi.SourceProvider, agentapi.TranscriptFilter, error) {
	if r.sources == nil {
		return nil, nil, errors.New("source integrations are required")
	}
	p, f, ok := r.sources.LookupSources(r.harness)
	if !ok {
		return nil, nil, fmt.Errorf("source integration unavailable for %s", r.harness)
	}
	if _, err := p.Describe(r.ref); err != nil {
		return nil, nil, err
	}
	return p, f, nil
}

func (r providerReader) pass(ctx context.Context, p agentapi.SourceProvider, key string) (agentapi.SourcePass, func() error, error) {
	if r.passes != nil && r.historicalSourceAllowed == nil {
		pass, err := r.passes.get(ctx, sourcePassKey{name: key, root: r.discoveryRoot(), discovery: r.discovery != nil, legacy: r.harness == archive.HarnessCodex && r.admission.Binding == nil && (r.discovery == nil || r.discovery.Origin != archive.SessionOriginDiscovery)}, p)
		if err != nil {
			return nil, nil, err
		}
		r.passes.active++
		return pass, func() error { r.passes.active--; return nil }, nil
	}
	env := sourceEnvironment(r.discovery, r.database)
	env.HistorySourceAllowed = r.historicalSourceAllowed
	env.CodexRollouts = r.rollouts
	if r.resourceOwner != nil {
		env.ReadBudget = r.resourceOwner.readBudget()
	}
	env.LegacyUnboundRegistration = r.harness == archive.HarnessCodex && r.admission.Binding == nil && (r.discovery == nil || r.discovery.Origin != archive.SessionOriginDiscovery)
	pass, err := p.OpenPass(ctx, env)
	if err != nil {
		return nil, nil, err
	}
	return pass, pass.Close, nil
}

func (r providerReader) discoveryRoot() string {
	if r.discovery != nil {
		if r.discovery.CodexBinding != nil && r.discovery.CodexBinding.Home != "" {
			return r.discovery.CodexBinding.Home
		}
		return r.discovery.DiscoveryRoot
	}
	return ""
}

// validationContext bounds explicit historical source reads as well as their sweep.
// Ordinary captures retain their caller's existing context and filesystem costs.
func (r providerReader) validationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	lookup := r.rollouts
	if r.passes != nil {
		lookup = r.passes.env.CodexRollouts
	}
	if _, ok := lookup.(agentapi.CodexRolloutSliceProvider); ok {
		if _, bounded := ctx.Deadline(); !bounded {
			return context.WithTimeout(ctx, 30*time.Second)
		}
	}
	return ctx, func() {}
}

func (r providerReader) Signature(ctx context.Context) (out sourceState, err error) {
	ctx, cancel := r.validationContext(ctx)
	defer cancel()
	provider, filter, err := r.binding()
	if err != nil {
		return out, err
	}
	p, closePass, err := r.pass(ctx, provider, filter.Name())
	if err != nil {
		return out, err
	}
	defer func() { err = errors.Join(err, closePass()) }()
	if validator, ok := p.(agentapi.SourceAdmissionSignature); ok && r.admission.NativeID != "" {
		if err := validator.ValidateSourceAdmission(ctx, r.ref, r.admission); err != nil {
			legacy := r.discovery == nil && r.admission.Binding == nil && r.rollouts == nil && (agentapi.Failure(err) == agentapi.FormatMismatch || agentapi.Failure(err) == agentapi.Unavailable)
			if !legacy {
				return out, r.observePendingHistory(ctx, err)
			}
		}
	} else if r.discovery != nil {
		return out, errors.New("confined source admission validator required")
	}

	o, err := p.Signature(ctx, r.ref)
	if err == nil {
		err = r.validateObservation(provider, o)
	}
	return observe(r.ref.Kind, o), r.observePendingHistory(ctx, err)
}

func (r providerReader) Filter(ctx context.Context, adapter archive.Adapter, maxBytes int64) (out archive.FilteredTranscript, observed sourceState, err error) {
	ctx, cancel := r.validationContext(ctx)
	defer cancel()
	provider, _, err := r.binding()
	if err != nil {
		return out, observed, err
	}
	f, ok := adapter.(agentapi.TranscriptFilter)
	if !ok {
		return out, observed, errors.New("native filter port required")
	}
	p, closePass, err := r.pass(ctx, provider, f.Name())
	if err != nil {
		return out, observed, err
	}
	defer func() { err = errors.Join(err, closePass()) }()
	limits := agentapi.ReadLimits{RawBytes: maxRawBytes(r.harness, maxBytes), RecordBytes: recordLimit, SubagentMetadata: r.subagentMetadata}
	snap, err := p.Read(ctx, r.ref, limits)
	if err != nil {
		if o, ok := agentapi.ErrorObservation(err); ok {
			if observationErr := r.validateObservation(provider, o); observationErr != nil {
				return out, observed, errors.Join(err, agentapi.Wrap(agentapi.Unavailable, observationErr))
			}
			observed = observe(r.ref.Kind, o)
		}
		return out, observed, translateSourceError(r.observePendingHistory(ctx, err))
	}
	defer func() { err = errors.Join(err, snap.Close()) }()
	observed = observe(r.ref.Kind, snap.Observation())
	if err = r.validateObservation(provider, observed.observation); err != nil {
		return out, sourceState{}, err
	}
	if r.ref.Kind == archive.SourceKindFile {
		transcriptFilters.Add(1)
	}
	in := snap.Input()
	observed.binding, err = r.snapshotBinding(ctx, snap)
	if err != nil {
		return out, observed, err
	}
	if relocation, ok := snap.(agentapi.SourceRelocation); ok && r.admission.Binding == nil {
		if moved, relocated := relocation.RelocatedFrom(r.ref); relocated {
			observed.relocated = moved.Path
		}
	}

	out, err = r.filterOwned(ctx, f, in, agentapi.FilterContext{Filename: filepath.Base(r.ref.Path), StartedAt: r.startedAt, Limits: limits})
	if err != nil {
		return out, observed, translateSourceError(r.observePendingHistory(ctx, err))
	}
	return out, observed, checkFilteredSize(out, maxBytes)
}

// observePendingHistory is a diagnostic-only route behind the existing fence.
// It never supplies the catalog to the active capture pass or filters ancestors.
func (r providerReader) observePendingHistory(ctx context.Context, original error) (out error) {
	if r.harness != "codex" || r.pendingRollouts == nil || !errors.Is(original, archive.ErrRelatedHistory) {
		return original
	}
	lookup := r.pendingRollouts()
	if lookup == nil {
		return original
	}
	bounded, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if provider, ok := lookup.(agentapi.CodexRolloutSliceProvider); ok {
		slice, err := provider.BeginValidationSlice(bounded, agentapi.CodexValidationLimits{Steps: 8, Duration: 100 * time.Millisecond})
		if err != nil {
			return errors.Join(original, errors.New("pending Codex history: locator evidence unavailable"))
		}
		defer func() {
			if err := slice.Close(); err != nil {
				out = errors.Join(out, errors.New("pending Codex history: locator evidence unavailable"))
			}
		}()
		lookup = slice
	}
	refs, err := lookup.Rollout(bounded, sourcefacts.RolloutID(r.ref.Path))
	if err != nil {
		return errors.Join(original, errors.New("pending Codex history: locator evidence unavailable"))
	}
	detail := fmt.Sprintf("%d candidate rollout locators", len(refs))
	return errors.Join(original, fmt.Errorf("pending Codex history: %s; this operation remains pending", detail))
}

func (r providerReader) validateObservation(provider agentapi.SourceProvider, o agentapi.SourceObservation) error {
	if !o.Present || o.Size < 0 {
		return errors.New("invalid present source observation")
	}
	if err := sourceio.ValidateSignature(o.Signature); err != nil {
		return err
	}
	semantics, err := provider.Describe(r.ref)
	if err != nil {
		return err
	}
	if o.Signature.Provider != semantics.Provider {
		return errors.New("source signature provider does not match declared semantics")
	}
	return nil
}

func translateSourceError(err error) error {
	switch {
	case errors.Is(err, agentapi.ErrRawLimit):
		return errors.Join(errTranscriptTooLarge, err)
	case errors.Is(err, archive.ErrRecordTooLarge), errors.Is(err, cursorstore.ErrRecordLimit):
		return errors.Join(errRecordTooLarge, err)
	}
	return err
}

// transcriptFilters counts full changed-file filtering, never previews.
var transcriptFilters atomic.Int64

// CursorChatSize counts existing native value bytes, excluding framing.
func CursorChatSize(c cursorstore.Composer) int64 {
	n := int64(len(c.Composer))
	for _, b := range c.Bubbles {
		n += int64(len(b.Value))
	}
	return n
}

func checkCursorChatSize(c cursorstore.Composer, maxBytes int64) error {
	if int64(len(c.Composer)) > recordLimit {
		return errRecordTooLarge
	}
	for _, b := range c.Bubbles {
		if int64(len(b.Value)) > recordLimit {
			return errRecordTooLarge
		}
	}
	if CursorChatSize(c) > maxRawBytes(archive.HarnessCursor, maxBytes) {
		return errTranscriptTooLarge
	}
	return nil
}

// FilterCursorChat filters already materialized import data through an injected native filter.
func FilterCursorChat(c cursorstore.Composer, sources agentapi.SourcesLookup) (archive.FilteredTranscript, error) {
	if sources == nil {
		return archive.FilteredTranscript{}, errors.New("source integrations are required")
	}
	_, f, ok := sources.LookupSources(archive.HarnessCursor)
	if !ok {
		return archive.FilteredTranscript{}, errors.New("cursor filter unavailable")
	}
	if err := checkCursorChatSize(c, DefaultMaxTranscriptBytes); err != nil {
		return archive.FilteredTranscript{}, errors.Join(archive.ErrRecordTooLarge, err)
	}
	out, err := f.Filter(context.Background(), agentapi.NativeInput{Records: &composerRecords{c: c}}, agentapi.FilterContext{})
	if err != nil {
		return out, err
	}
	return out, checkFilteredSize(out, DefaultMaxTranscriptBytes)
}

type composerRecords struct {
	c cursorstore.Composer
	i int
}

func (r *composerRecords) Next(ctx context.Context) (agentapi.NativeRecord, bool, error) {
	if err := ctx.Err(); err != nil {
		return agentapi.NativeRecord{}, false, err
	}
	if r.i == 0 {
		r.i++
		return agentapi.NativeRecord{Kind: agentapi.ComposerRecord, Raw: r.c.Composer}, true, nil
	}
	i := r.i - 1
	if i >= len(r.c.Bubbles) {
		return agentapi.NativeRecord{}, false, nil
	}
	b := r.c.Bubbles[i]
	r.i++
	return agentapi.NativeRecord{Kind: agentapi.BubbleRecord, Key: b.ID, Raw: b.Value, Missing: b.Value == nil}, true, nil
}

// cursorDatabase supplies an explicit caller override; native providers own defaults.
func (o Options) cursorDatabase() string { return o.CursorDatabase }

// openCursorPass gives the pass one Reader for Cursor's database when any
// session is read from it (Run has already swept snapshots a killed pass
// left behind).
// The returned function removes the pass's snapshot, and says if it could
// not: a copy of every Cursor chat left in the temporary directory is worth
// a failed pass (the next sweep removes it once it is stale).
type sourcePassKey struct {
	name      string
	root      string
	discovery bool
	legacy    bool
}

type sourcePassSet struct {
	sliceFailure error
	slice        agentapi.CodexRolloutSlice
	active       int
	env          agentapi.SourceEnvironment
	passes       map[sourcePassKey]agentapi.SourcePass
}

// releaseCaches drops the open source passes' idle caches when budget has
// less than need free (see pass.scan).
func (s *sourcePassSet) releaseCaches(budget *agentapi.NativeReadBudget, need int64) error {
	if s == nil || budget.Available() >= need {
		return nil
	}
	var err error
	for _, pass := range s.passes {
		if caches, ok := pass.(interface{ ReleaseCaches() error }); ok {
			err = errors.Join(err, caches.ReleaseCaches())
		}
	}
	return err
}

func (s *sourcePassSet) get(ctx context.Context, key sourcePassKey, p agentapi.SourceProvider) (agentapi.SourcePass, error) {
	if key.name == "codex" {
		if provider, ok := s.env.CodexRollouts.(agentapi.CodexRolloutSliceProvider); ok {
			var validation error
			if s.slice != nil {
				validation = s.slice.Valid(ctx)
				// Preserve the failed sweep for its bounded slice. Expiry returns
				// a distinct error and permits renewal after all readers close.
				if validation != nil && errors.Is(validation, s.sliceFailure) {
					return nil, validation
				}
			}
			if s.slice == nil || validation != nil {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				if s.active != 0 {
					return nil, agentapi.Wrap(agentapi.Limit, errors.New("close active source snapshots before renewing catalog validation"))
				}
				for k, pass := range s.passes {
					if k.name == "codex" {
						if err := pass.Close(); err != nil {
							return nil, err
						}
						delete(s.passes, k)
					}
				}
				if s.slice != nil {
					if err := s.slice.Close(); err != nil {
						return nil, err
					}
				}
				slice, err := provider.BeginValidationSlice(ctx, agentapi.CodexValidationLimits{})
				if err != nil {
					return nil, err
				}
				s.slice = slice
				s.sliceFailure = slice.Valid(ctx)
				if s.sliceFailure != nil {
					return nil, s.sliceFailure
				}
			}
		}
	}
	if pass := s.passes[key]; pass != nil {
		return pass, nil
	}
	e := s.env
	if key.name == "codex" && s.slice != nil {
		e.CodexRollouts = s.slice
	}
	e.LegacyUnboundRegistration = key.legacy
	if key.discovery {
		e.Files = sourcefacts.RootOpener{Root: key.root}
		e.Policy = transcriptio.OpenPolicy{Root: key.root, RejectSymlinks: true}
	}
	pass, err := p.OpenPass(ctx, e)
	if err == nil {
		s.passes[key] = pass
	}
	return pass, err
}

func openCursorPass(_ []archive.SessionRegistration, opts *Options) func() error {
	budget := agentapi.NewNativeReadBudget(agentapi.PassReadBudget)
	if shared, ok := opts.CodexRollouts.(agentapi.CodexRolloutResourceBudget); ok && shared.NativeReadBudget() != nil {
		budget = shared.NativeReadBudget()
	}
	opts.sourcePasses = &sourcePassSet{env: agentapi.SourceEnvironment{ReadBudget: budget, RequireConfinedHistory: true, Database: opts.cursorDatabase(), CodexRollouts: opts.CodexRollouts}, passes: map[sourcePassKey]agentapi.SourcePass{}}
	return func() error {
		var err error
		for _, p := range opts.sourcePasses.passes {
			if counter, ok := p.(interface{ Snapshots() int }); ok && opts.afterCursorPass != nil {
				opts.afterCursorPass(counter.Snapshots())
			}
			err = errors.Join(err, p.Close())
		}
		if opts.sourcePasses.slice != nil {
			err = errors.Join(err, opts.sourcePasses.slice.Close())
		}
		return err
	}
}

// rememberFailedRead records, for a cursor-sqlite session whose chat was
// read but could not be captured for a reason that reading it again cannot
// change (a filter error, or a size limit), the state it failed at, with
// the error's text ("" for a recorded gap). unchangedSinceLastScan then
// skips the chat until its signature, or a derivation version, changes, so
// the failure costs one in-place signature read per pass rather than a copy
// of the database. A failure to read at all (a lock, a changed file) is
// left to be retried. The size limits in force are recorded too, so raising
// one reads the chat again, and so is the gap (blocked) a size limit
// recorded, for status.
func rememberFailedRead(local *state.Store, reg archive.SessionRegistration, adapter archive.Adapter, observed sourceState, opts Options, failure error, blocked state.BlockedReason, limit *state.SizeLimitGap, publishedLastHead string) error {
	if !observed.observation.Present {
		return nil
	}
	message := ""
	if failure != nil && (!agentapi.Deterministic(failure) || (agentapi.Failure(failure) != agentapi.Unsafe && agentapi.Failure(failure) != agentapi.FormatMismatch && agentapi.Failure(failure) != agentapi.Limit)) {
		return nil
	}
	if failure != nil {
		message = failure.Error()
	}
	return local.SaveScanSignature(reg.ArchiveSessionID, state.ScanSignature{
		SourceSetVersion: sourceSetVersion(reg),
		SkillEvidence:    string(opts.skillEvidence()),
		ParserVersion:    opts.parserVersionFor(reg.Harness.Name), FilterVersion: archive.FilterVersion, AdapterVersion: adapter.Version(),
		SourceSignature: signaturePointer(observed), SourceKind: observed.kind, CursorLastUpdatedAt: observed.cursor.LastUpdatedAt,
		CursorHeaderCount: observed.cursor.HeaderCount, CursorLastBubbleID: observed.cursor.LastBubbleID,
		CursorMessageRows: observed.cursor.MessageRows, CursorLastMessageHash: observed.cursor.LastMessageHash,
		Failed: true, FailedError: message, FailedMaxBytes: opts.maxTranscriptBytes(), FailedRecordLimit: recordLimit, FailedRawLimit: opts.maxRawBytes(reg.Harness.Name), SizeLimit: limit,
		Blocked: blocked, PublishedLastHead: publishedLastHead,
	})
}

// CaptureGapCursorChatRewritten marks a Cursor database chat whose messages
// changed after an earlier snapshot of it was taken, so a later snapshot
// replaced one that did not lead to it (see sessionScan.guard). A chat has at
// most one: its detail counts the rewrites, and its observation time is the
// last one's.
const CaptureGapCursorChatRewritten = "cursor_chat_rewritten"

// cursorRewriteProvenance is the rewrite gap's provenance: the collector's
// own, never a hook's, so it is not taken for a resume.
const cursorRewriteProvenance = "collector:cursor-rewrite"

// cursorRewriteDetail is the rewrite gap's detail for n rewrites. The
// earlier snapshot may have been published, or only held back (rate
// limited or declined), so the wording claims neither.
const cursorRewriteDetail = "Cursor changed messages of this chat after an earlier snapshot of it %d time(s); the snapshot is the chat as it was last read"

// withCursorRewriteGap returns evidence with the chat's one rewrite gap
// counting one more rewrite, observed at: an earlier rewrite gap is replaced,
// not added to, so a chat Cursor rewrites often (late token counts are
// routine) does not grow a gap per rewrite.
func withCursorRewriteGap(evidence []archive.SupplementalEvidence, at time.Time) []archive.SupplementalEvidence {
	rewrites := 0
	out := make([]archive.SupplementalEvidence, 0, len(evidence)+1)
	for _, e := range evidence {
		if e.Kind == archive.EvidenceKindCaptureGap && e.Provenance == cursorRewriteProvenance {
			n := 0
			detail, _ := e.Payload["detail"].(string)
			if _, err := fmt.Sscanf(detail, cursorRewriteDetail, &n); err != nil || n < 1 {
				n = 1
			}
			rewrites += n
			continue
		}
		out = append(out, e)
	}
	return append(out, archive.SupplementalEvidence{
		Kind: archive.EvidenceKindCaptureGap, ObservedAt: at.UTC(), Provenance: cursorRewriteProvenance,
		Payload: map[string]any{
			"code":   CaptureGapCursorChatRewritten,
			"detail": fmt.Sprintf(cursorRewriteDetail, rewrites+1),
		},
	})
}

// unchangedSinceFailureError reports a remembered failure again on a pass that
// skipped the chat because it has not changed since.
type unchangedSinceFailureError struct {
	message string
	cursor  bool
}

func (e unchangedSinceFailureError) Error() string {
	if e.cursor {
		return e.message + " (the Cursor chat has not changed since)"
	}
	return e.message + " (the source has not changed since)"
}

func signaturePointer(s sourceState) *agentapi.SourceSignature {
	if !s.observation.Present {
		return nil
	}
	v := s.observation.Signature
	return &v
}

func sourceSemantics(sources agentapi.SourcesLookup, reg archive.SessionRegistration) (agentapi.SourceSemantics, error) {
	if sources == nil {
		return agentapi.SourceSemantics{}, errors.New("source integrations are required")
	}
	p, _, ok := sources.LookupSources(reg.Harness.Name)
	if !ok {
		return agentapi.SourceSemantics{}, errors.New("source integration unavailable")
	}
	return p.Describe(sourceRef(reg))
}

func sourceAdmission(reg archive.SessionRegistration) agentapi.SourceAdmission {
	if reg.Harness.Name != "codex" {
		return agentapi.SourceAdmission{}
	}
	cwd := ""
	if reg.Origin == archive.SessionOriginDiscovery {
		cwd = reg.DiscoveryCwd
	}
	if reg.HistoricalProject != nil {
		cwd = reg.HistoricalProject.OriginalCwd
	}
	if reg.CodexBinding != nil && reg.HistoricalProject == nil {
		cwd = ""
	}
	createdAt := time.Time{}
	producerVersion, producerOriginator, producerSource := "", "", ""
	if reg.Origin == archive.SessionOriginDiscovery && reg.CodexBinding == nil {
		createdAt = reg.SessionStartedAt
		producerVersion = reg.Harness.Version
		producerOriginator = reg.DiscoveryProducerOriginator
		producerSource = reg.DiscoveryProducerSource
	}
	return agentapi.SourceAdmission{NativeID: reg.NativeSessionID, Cwd: cwd, Binding: reg.CodexBinding, NativeCreatedAt: createdAt, InitialProducerVersion: producerVersion, InitialProducerOriginator: producerOriginator, InitialProducerSource: producerSource}
}

func (r providerReader) snapshotBinding(ctx context.Context, snap agentapi.SourceSnapshot) (*archive.CodexSourceBinding, error) {
	var binding *archive.CodexSourceBinding
	if evidenceReader, ok := snap.(agentapi.SourceAdmissionEvidence); ok && r.admission.NativeID != "" {
		evidence, evidenceErr := evidenceReader.AdmissionEvidence(ctx, r.admission)
		if evidenceErr != nil {
			return nil, evidenceErr
		}
		if evidence.Binding.RequiresOwnTask() && !evidence.Task.ValidNativeCreation(evidence.Binding.NativeCreatedAt) {
			return nil, agentapi.Wrap(agentapi.Unavailable, errors.New("first own task does not establish native execution"))
		}
		if r.admission.Binding != nil && !r.admission.Binding.FirstNativeTaskAt.IsZero() {
			evidence.Binding.FirstNativeTaskAt = r.admission.Binding.FirstNativeTaskAt
			evidence.Binding.FirstNativeTaskID = r.admission.Binding.FirstNativeTaskID
		} else if evidence.Task.Native && evidence.Task.LocalExecution {
			evidence.Binding.FirstNativeTaskAt = evidence.Task.StartedAt
			evidence.Binding.FirstNativeTaskID = evidence.Task.TurnID
		}
		binding = &evidence.Binding
	} else {
		if validator, ok := snap.(agentapi.SourceAdmissionValidator); ok && r.admission.NativeID != "" {
			if err := validator.ValidateAdmission(ctx, r.admission); err != nil {
				return nil, err
			}
		} else if r.discovery != nil {
			return nil, errors.New("confined source admission validator required")
		}
		if facts, ok := snap.(agentapi.SourceAdmissionFacts); ok && r.admission.NativeID != "" {
			factsBinding, err := facts.AdmissionFacts(ctx)
			if err != nil {
				return nil, err
			}
			binding = &factsBinding
		}
	}
	if binding != nil && r.discovery != nil && r.discovery.Origin == archive.SessionOriginDiscovery && !binding.NativeCreatedAt.Equal(r.discovery.SessionStartedAt) {
		return nil, errors.New("native original creation evidence changed")
	}

	return binding, nil
}

func (r providerReader) filterOwned(ctx context.Context, filter agentapi.TranscriptFilter, in agentapi.NativeInput, c agentapi.FilterContext) (archive.FilteredTranscript, error) {
	if leased, ok := filter.(agentapi.LeasedTranscriptFilter); ok && r.resourceOwner != nil && leased.LeasedFilterFor(filter) {
		out, release, err := leased.FilterLeased(ctx, in, c, r.resourceOwner.readBudget())
		if err == nil {
			r.resourceOwner.retainedReleases = append(r.resourceOwner.retainedReleases, release)
		}
		return out, err
	}
	return filter.Filter(ctx, in, c)
}
