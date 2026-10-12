// Package discovery performs bounded local source observation and admission.
// Adapters supply source facts; current configuration authorizes capture.
package discovery

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

const (
	// Budget reserves collector time for queued publications and retention.
	Budget = 5 * time.Second
	// HeaderProbes caps metadata reads in one pass.
	HeaderProbes = 256
	// Reprobe older cached observations so they acquire explicit format profiles.
	catalogVersion = 6
	maxCatalog     = 8192
	// Keep disposable hints smaller than the accepted catalog limit. Coverage
	// and requested proofs retain their independent capacity and continuation.
	maxObservationCache = 2048
	maxDirectories      = 4096
	maxRetries          = 256
)

// Health separates scan coverage from upload and hook health. Codes never
// include native IDs, paths or native operating-system errors.
type Health struct {
	Enabled                      bool                `json:"enabled"`
	Supported                    bool                `json:"supported"`
	LastAttempt                  time.Time           `json:"last_attempt,omitzero"`
	LastReconciled               time.Time           `json:"last_reconciled,omitzero"`
	Pending                      bool                `json:"pending"`
	ProjectOperations            int                 `json:"project_metadata_operations"`
	GitBytes                     int                 `json:"git_metadata_bytes"`
	Probes                       int                 `json:"header_probes"`
	Entries                      int                 `json:"directory_entries"`
	IndexBytes                   int64               `json:"index_bytes_read,omitempty"`
	IndexQueries                 int                 `json:"index_queries,omitempty"`
	IndexLocators                int                 `json:"index_locators,omitempty"`
	Bytes                        int64               `json:"bytes_read"`
	NativeValidationBytes        int64               `json:"native_validation_bytes_read,omitempty"`
	NativeValidationReads        int64               `json:"native_validation_reads,omitempty"`
	NativeValidationAttempts     int64               `json:"native_validation_attempts,omitempty"`
	NativeValidationOpens        int64               `json:"native_validation_opens,omitempty"`
	Registered                   int                 `json:"registered"`
	Outcomes                     map[string]int      `json:"outcomes,omitempty"`
	Errors                       []string            `json:"errors,omitempty"`
	Formats                      []FormatObservation `json:"observed_formats,omitempty"`
	NativeReadBytes              int64               `json:"native_read_bytes,omitempty"`
	NativeReadOperations         int64               `json:"native_read_operations,omitempty"`
	RepositoryMetadataOperations int                 `json:"repository_metadata_operations,omitempty"`
	RepositoryLookups            int                 `json:"repository_lookups,omitempty"`
}

type directory struct {
	Root   string `json:"root"`
	Path   string `json:"path"`
	Offset int64  `json:"offset"`
}

type cached struct {
	SourceFingerprint string      `json:"source_fingerprint,omitempty"`
	Size              int64       `json:"size"`
	Mtime             int64       `json:"mtime"`
	Checked           time.Time   `json:"checked"`
	Observation       Observation `json:"observation"`
	ActiveHint        bool        `json:"active_hint,omitempty"`
}

type catalog struct {
	Recovery sourcefacts.RecoveryInventory `json:"project_recovery,omitempty"`
	Coverage *coverageInventory            `json:"native_coverage,omitempty"`
	Version  int                           `json:"version"`
	Roots    []string                      `json:"roots"`
	Queue    []directory                   `json:"queue"`
	Cache    map[string]cached             `json:"cache"`
	Health   Health                        `json:"health"`
	Priority map[string]int64              `json:"priority,omitempty"`
	Retries  []SourceDescriptor            `json:"retries,omitempty"`
	// Classifier is the header classification the cached observations'
	// refusals were made under (see reclassify).
	Classifier int `json:"classifier,omitempty"`
}

// headerClassifier revises which Codex headers capture understands without
// discarding the whole catalog: 1 accepts agent-created top-level threads.
const headerClassifier = 1

// reclassify drops cached observations that an older header classification
// refused as an unsupported execution without any identity, so each is read
// again, once, under the current one. A cached refusal is otherwise kept for
// as long as the file is unchanged, and an archived rollout never changes:
// its identity would stay unknown to coverage, which then proves the thread
// has no rollouts at all. A refusal that kept its identity (a guardian review
// child, say) already reaches coverage and is not read again: rereading many
// headers at once would hold coverage incomplete for several passes. Every
// other cached observation, the coverage inventory and its retained proofs
// stay.
func (c *catalog) reclassify() {
	if c.Classifier >= headerClassifier {
		return
	}
	for path, entry := range c.Cache {
		if entry.Observation.Outcome == outcomeUnsupportedExec && entry.Observation.Identity == nil {
			delete(c.Cache, path)
		}
	}
	c.Classifier = headerClassifier
}

// Options contains injectable clocks and stop signals; it cannot override
// source format support or authorize capture.
type Options struct {
	nativeOnly    bool
	inventoryOnly bool
	Sources       agentapi.SourcesLookup
	CodexRollouts agentapi.CodexRolloutLookup
	Now           func() time.Time
	Stop          func() bool
	// Rollouts receives the validated observations and bounded coverage state.
	RepositoryIdentity        sourcefacts.RepositoryLookup
	RepositoryIdentityCurrent func(sourcefacts.RepositoryIdentity) bool
	Rollouts                  *CodexRolloutLookup
}

// Run observes configured Codex homes under the caller's collector lock,
// before any storage initialization. Each admission revalidates permissions
// under hooks.lock, never holding it while reading source files.
func Run(ctx context.Context, store *state.Store, cfg config.Config, o Options) (Health, error) {
	// Historical ownership does not start future native inventory. Registered
	// capture owners still request their qualified coverage through PrepareRegistered.
	if cfg.HistoricalProjectProtection && (cfg.Discovery == nil || !cfg.Discovery.Enabled) {
		return Health{}, nil
	}
	return runWithAdapters(ctx, store, cfg, o, registeredAdapters())
}

func runWithAdapters(ctx context.Context, store *state.Store, cfg config.Config, o Options, adapters []SourceAdapter) (result Health, resultErr error) {
	adapter := findAdapter(adapters, "codex")
	if adapter == nil {
		return Health{}, nil
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	enabled := cfg.Discovery != nil && cfg.Discovery.Enabled
	if !enabled && (o.Rollouts == nil || len(o.Rollouts.roots) == 0) {
		return Health{}, nil
	}
	now := o.Now().UTC()
	h := Health{Enabled: enabled, LastAttempt: now, Outcomes: map[string]int{}}
	c, path, roots, err := prepareCatalog(ctx, store, cfg, o, adapter, &h)
	if err != nil {
		h.Pending = true
		return h, err
	}
	if c.Coverage != nil && c.Coverage.Phase == coverageValidate {
		h.Supported = c.Health.Supported
		h.Formats = slices.Clone(c.Health.Formats)
	}
	allowance := Budget
	checkpointAllowance := Budget
	if o.Rollouts != nil {
		checkpointAllowance = min(Budget, o.Rollouts.remaining)
		allowance = checkpointAllowance - time.Second
		if allowance <= 0 {
			return h, agentapi.Wrap(agentapi.Limit, errors.New("native observation budget exhausted"))
		}
	}
	started := time.Now()
	if o.Rollouts != nil {
		defer func() { o.Rollouts.remaining -= time.Since(started) }()
	}
	// Observation may exhaust its slice while the durable queue/proofs still
	// need saving. Keep the reserved checkpoint second inside the same pass
	// allowance and under the original caller's cancellation.
	checkpointCtx, cancelCheckpoint := context.WithDeadline(ctx, started.Add(checkpointAllowance))
	defer cancelCheckpoint()
	deadline := started.Add(allowance)
	ctx, cancel := context.WithDeadline(checkpointCtx, deadline)
	defer cancel()
	if o.CodexRollouts == nil && o.Rollouts != nil {
		o.CodexRollouts = o.Rollouts
	}
	proofs := &nativeProofPasses{sources: o.Sources, lookup: o.CodexRollouts, passes: map[string]agentapi.SourcePass{}}
	defer func() { resultErr = errors.Join(resultErr, proofs.Close()) }()
	resolver := sourcefacts.NewProjectResolver()
	recovery := sourcefacts.NewRecoveryResolver(cfg.Archive.Projects, nil, canonicalProjectPath, o.RepositoryIdentity, &c.Recovery)
	recovery.Validate = o.RepositoryIdentityCurrent
	refused := admissionRefusedStarts(store.Home(), cfg, adapter.Agent(), o)
	priority := scan{nativeOnly: o.nativeOnly, inventoryOnly: o.inventoryOnly, proofs: proofs, resolver: resolver, recovery: recovery, store: store, cfg: cfg, catalog: &c, health: &h, now: now, adapter: adapter, priority: true, ctx: ctx, rollouts: o.Rollouts, refused: refused}
	// The shared lookup owns the only native index projection. Production
	// observation uses the shared directory worker rather than opening native
	// SQLite via the legacy settled-only scheduling hint adapter.
	if o.Rollouts == nil {
		priority.observeIndexHints(ctx, o, roots, deadline)
	}
	if !o.nativeOnly && !o.inventoryOnly {
		priority.observeActiveHints(ctx, o, roots, deadline)
		priority.observeRetries(o)
	}

	priority.observeDirectories(o, deadline)
	// A just-validated inventory can prove deferred native ownership before the
	// next pass starts a new coverage epoch. The same deadline/ledger still applies.
	if !o.nativeOnly && !o.inventoryOnly && hasValidatedCoverage(c.Coverage) {
		priority.observeRetries(o)
	}
	h.NativeValidationBytes, h.NativeValidationReads, h.NativeValidationOpens = proofs.bytes, proofs.reads, proofs.opens
	h.NativeValidationAttempts = proofs.attempts
	h.ProjectOperations = resolver.Operations
	h.RepositoryMetadataOperations = recovery.MetadataOperations
	h.RepositoryLookups = recovery.Operations
	h.GitBytes = resolver.GitBytes
	h.Pending = catalogPending(c, o.nativeOnly)
	if !h.Pending && len(h.Errors) == 0 {
		h.LastReconciled = now
	}
	pruneCatalog(&c)
	c.Health = h
	if c.Coverage != nil {
		if err := c.Coverage.validate(roots); err != nil {
			return h, err
		}
	}
	if o.nativeOnly {
		if o.Rollouts != nil {
			o.Rollouts.catalog = &c
		}
		return h, nil
	}
	if err := checkpointCatalog(checkpointCtx, o.Rollouts, path, &c, &h); err != nil {
		return h, err
	}
	if o.Rollouts != nil {
		o.Rollouts.catalog = &c
		o.Rollouts.coverageDirty = false // checkpoint includes observation changes
	}
	if err := writeHealth(store.Home(), h); err != nil {
		return h, err
	}
	// The record only lets status point at backfill; failing to write it
	// leaves the previous one, and the next pass writes it again.
	_ = refused.save(store.Home(), c.Cache)
	return h, nil
}

func catalogNeedsReset(c catalog, roots []string) bool {
	return c.Version != catalogVersion || len(c.Cache) > maxCatalog || len(c.Queue) > maxDirectories || len(c.Priority) > 64 || len(c.Retries) > maxRetries || !slices.Equal(c.Roots, roots)
}

func validSource(source SourceDescriptor, root string) bool {
	return source.Root == root && filepath.IsAbs(source.Locator) && local.PathWithin(source.Locator, root) && source.Kind == archive.SourceKindFile && source.StableKey != "" && len(source.StableKey) <= 4096
}

func validCandidate(candidate Candidate, source SourceDescriptor, agent string, now time.Time) bool {
	return candidate.Agent == agent && candidate.Source == source && candidate.NativeSessionID != "" && len(candidate.NativeSessionID) <= 4096 && filepath.IsAbs(candidate.WorkingDirectory) && len(candidate.WorkingDirectory) <= 4096 && !candidate.StartedAt.IsZero() && !candidate.FirstTaskAt.IsZero() && !candidate.FirstTaskAt.Before(candidate.StartedAt.Add(-time.Second)) && !candidate.FirstTaskAt.After(now.Add(2*time.Minute)) && candidate.StartEvidence == "native_start" && candidate.Execution == "native" && (candidate.ParentNativeID == "" || candidate.NativeChild && sourcefacts.RolloutID(candidate.ParentNativeID+".jsonl") == candidate.ParentNativeID) && (candidate.ForkNativeID == "" || sourcefacts.RolloutID(candidate.ForkNativeID+".jsonl") == candidate.ForkNativeID)
}

func validCandidateMetadata(candidate Candidate, source SourceDescriptor, agent string) bool {
	metadata := candidate
	metadata.FirstTaskAt = metadata.StartedAt
	return validCandidate(metadata, source, agent, metadata.StartedAt)
}

func scanStopped(ctx context.Context, o Options) bool {
	return ctx.Err() != nil || (o.Stop != nil && o.Stop())
}

func approvedRoots(homes []string) []string {
	var roots []string
	for _, home := range homes {
		if !filepath.IsAbs(home) {
			continue
		}
		root, err := filepath.EvalSymlinks(home)
		if err != nil {
			root = filepath.Clean(home)
		}
		roots = appendUnique(roots, root)
	}
	return roots
}

func appendUnique(values []string, value string) []string {
	if !slices.Contains(values, value) {
		values = append(values, value)
	}
	return values
}

// Directory cookies continue bounded enumeration without rereading preceding
// names. They are hints: every completed round restarts reconciliation, so
// moves, directory replacement and invalidation cannot silently lose coverage.
func readBatch(d directory) ([]string, int64, bool, error) { return readBatchMeasured(d, nil) }

func readBatchMeasured(d directory, counts *metadataCounts) ([]string, int64, bool, error) {
	if counts != nil {
		counts.rootOpens++
	}
	root, err := os.OpenRoot(d.Root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, true, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	defer func() { _ = root.Close() }()
	if counts != nil {
		counts.stats++
	}
	info, err := root.Lstat(d.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, true, nil
	}
	if err != nil || !info.IsDir() {
		return nil, 0, false, errors.New("not a directory")
	}
	if counts != nil {
		counts.fileOpens++
	}
	f, err := root.Open(d.Path)
	if err != nil {
		return nil, 0, false, err
	}
	defer func() { _ = f.Close() }()
	if _, err = f.Seek(d.Offset, io.SeekStart); err != nil {
		return nil, 0, false, err
	}
	buffer := make([]byte, 4096)
	n, err := syscall.ReadDirent(int(f.Fd()), buffer)
	if err != nil {
		return nil, 0, false, err
	}
	_, _, names := syscall.ParseDirent(buffer[:n], -1, nil)
	offset, err := f.Seek(0, io.SeekCurrent)
	return names, offset, n == 0, err
}

func resolveProject(cfg config.Config, cwd string) (string, bool) {
	if cfg.EffectiveCodexCaptureScope() == config.CodexAllProjects {
		facts, ok := sourcefacts.PhysicalProject(cwd)
		if !ok || !cfg.CodexProjectAllowed(facts.Root, facts.Cwd, time.Now().Add(100*365*24*time.Hour)) {
			return "", false
		}
		return facts.Root, true
	}
	resolved := canonicalProjectPath(cwd)
	if !filepath.IsAbs(cwd) {
		return "", false
	}
	resolve := func(p string) string {
		r, e := filepath.EvalSymlinks(p)
		if e != nil {
			return filepath.Clean(p)
		}
		return r
	}
	if p, ok := sourcefacts.ConfiguredOwner(cfg.Archive.Projects, resolved, resolve); ok {
		info, err := os.Stat(p.Root)
		return p.Root, p.Included && err == nil && info.IsDir()
	}
	for d, depth := resolved, 0; depth < 64; depth++ {
		path := filepath.Join(d, ".git")
		info, e := os.Lstat(path)
		if e == nil {
			if info.IsDir() {
				if p, ok := sourcefacts.ConfiguredOwner(cfg.Archive.Projects, d, resolve); ok {
					return p.Root, p.Included
				}
				return "", false
			}
			read := func(path string) ([]byte, error) {
				f, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
				if e != nil {
					return nil, e
				}
				defer func() { _ = f.Close() }()
				info, e := f.Stat()
				if e != nil || !info.Mode().IsRegular() {
					return nil, errors.New("invalid Git metadata")
				}
				return io.ReadAll(io.LimitReader(f, 4097))
			}
			exists := func(path string) bool { info, e := os.Stat(path); return e == nil && info.IsDir() }
			main, ok := sourcefacts.WorktreeMain(d, read, exists, true)
			if !ok {
				return "", false
			}
			if p, ok := sourcefacts.ConfiguredOwner(cfg.Archive.Projects, resolve(main), resolve); ok {
				return p.Root, p.Included
			}
			return "", false
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	return "", false
}

func sessionKey(agent, native string) agentmeta.SessionKey {
	return agentmeta.SessionKey{Agent: agentmeta.ID(agent), NativeID: native}
}

func admitSession(store *state.Store, candidate Candidate, project, generation string, now time.Time, factSnapshots ...sourcefacts.ProjectFacts) (bool, error) {
	var cfgFacts *sourcefacts.ProjectFacts
	if len(factSnapshots) > 0 {
		cfgFacts = &factSnapshots[0]
	}
	var facts sourcefacts.ProjectFacts
	var factsOK bool
	if cfgFacts != nil {
		facts = *cfgFacts
		factsOK = true
	} else {
		facts, factsOK = sourcefacts.PhysicalProject(candidate.WorkingDirectory)
	}
	sourceRoot, locator := candidate.Source.Root, candidate.Source.Locator
	previous, replace := continuationLocator(store, candidate.Agent, candidate.NativeSessionID, candidate.Source)
	unlock, err := local.NamedLock(store.Home(), "hooks.lock")
	if err != nil {
		return false, err
	}
	defer unlock()
	if setupjournal.TransactionPending(store.Home()) {
		return false, errors.New("setup pending")
	}
	cfg, found, err := config.Load(store.Home())
	if err != nil || !found {
		return false, errors.New("configuration unavailable")
	}
	if cfg.Paused || !cfg.Archive.Enabled {
		return false, errors.New("capture authorization is inactive")
	}
	if _, removed, err := store.Removal(candidate.Agent, candidate.NativeSessionID); err != nil {
		return false, err
	} else if removed {
		return false, errors.New("session was removed")
	}
	id, exists, err := store.ArchiveSessionID(sessionKey(candidate.Agent, candidate.NativeSessionID))
	if errors.Is(err, state.ErrSessionIndexRecoveryRequired) {
		err = errors.Join(err, store.RequestSessionIndexRecovery(sessionKey(candidate.Agent, candidate.NativeSessionID)))
	}
	if err != nil {
		return false, err
	}
	if exists {
		_, found, err := mergeContinuation(store, cfg, candidate, project, continuationHint{ArchiveID: id, Previous: previous, Replace: replace, CanonicalCwd: facts.Cwd}, now)
		if found || err != nil {
			return false, err
		}
	}
	// A silent loss of both derived indexes is indistinguishable from a new
	// identity. Only the existing registration census can authorize that miss.
	absent, err := store.SessionIndexAbsent(sessionKey(candidate.Agent, candidate.NativeSessionID))
	if err != nil {
		return false, err
	}
	if !absent {
		err := store.RequestSessionIndexRecovery(sessionKey(candidate.Agent, candidate.NativeSessionID))
		return false, errors.Join(state.ErrSessionIndexRecoveryRequired, err)
	}
	if err := validateAdmissionGeneration(cfg, candidate, project, generation, now, facts, factsOK); err != nil {
		return false, err
	}
	if candidate.Binding != nil && !cfg.CodexHistoryProtection {
		cfg.CodexHistoryProtection = true
		if err := config.Save(store.Home(), cfg); err != nil {
			return false, err
		}
	}
	reg, err := store.RegisterOrMerge(sessionKey(candidate.Agent, candidate.NativeSessionID), func(id string) archive.SessionRegistration {
		var proof *archive.CodexAdmissionProof
		if cfg.EffectiveCodexCaptureScope() == config.CodexAllProjects {
			proof = &archive.CodexAdmissionProof{Generation: generation, Revision: cfg.CodexCapture.Revision, Cwd: facts.Cwd}
		}
		return archive.SessionRegistration{CodexAdmission: proof, ProjectResolution: candidate.ProjectResolution, RepoKey: candidate.RecordedRepoKey,
			CodexBinding: candidate.Binding, NativeChild: candidate.NativeChild, ParentNativeSessionID: candidate.ParentNativeID, NativeRootSessionID: candidate.RootNativeID, NativeSourceHome: sourceRoot,
			ArchiveSessionID: id, NativeSessionID: candidate.NativeSessionID, Harness: archive.Harness{Name: candidate.Agent, Version: candidate.HarnessVersion}, ProjectID: archive.ProjectID(project), ProjectRoot: project,
			SourceKind: candidate.Source.Kind, SourceKey: candidate.Source.StableKey, TranscriptPath: locator, DiscoveryRoot: sourceRoot, DiscoveryCwd: candidate.WorkingDirectory, DiscoveryProducerOriginator: candidate.ProducerOriginator, DiscoveryProducerSource: candidate.ProducerSource, DiscoveryGeneration: generation, DiscoverySourcePriority: candidate.Source.Priority, SessionStartedAt: candidate.StartedAt, RegisteredAt: now, AdmittedAt: now,
			Origin: archive.SessionOriginDiscovery, StartedAtSource: archive.StartedAtSourceTranscript, DestinationID: cfg.DestinationID(),
		}
	})
	// A registration without a request remains discoverable by collector's full
	// registration pass, so a crash here cannot lose the first publication.
	if err == nil {
		err = store.SaveRequest(reg.ArchiveSessionID, "discovery", now)
	}
	return true, err
}

type continuationHint struct {
	CanonicalCwd string
	ArchiveID    string
	Previous     string
	Replace      bool
}

// mergeContinuation runs under hooks.lock. Only the source locator can change;
// original origin, admission, start and destination attribution stay immutable.
func mergeContinuation(store *state.Store, cfg config.Config, candidate Candidate, project string, hint continuationHint, now time.Time) (archive.SessionRegistration, bool, error) {
	reg, found, err := store.LoadRegistration(hint.ArchiveID)
	if err != nil || !found {
		return reg, found, err
	}
	compatible := reg.ProjectRoot == project || (reg.CodexAdmission == nil && cfg.EffectiveCodexCaptureScope() == config.CodexAllProjects && local.PathWithin(candidate.WorkingDirectory, reg.ProjectRoot))
	permission := true
	if reg.CodexAdmission != nil {
		permission = hint.CanonicalCwd != "" && cfg.CodexContinuationAllowed(project, hint.CanonicalCwd)
	} else if cfg.EffectiveCodexCaptureScope() == config.CodexAllProjects {
		permission = cfg.CodexContinuationAllowed(project, hint.CanonicalCwd)
	}
	if !compatible || !cfg.AcceptSession(reg) || !permission {
		return reg, true, errors.New("identity conflict")
	}
	// A discovery observation cannot establish an unrestricted hook/import
	// locator. Those origins retain their own source authority and attribution.
	if reg.Origin != archive.SessionOriginDiscovery {
		return reg, true, nil
	}
	if reg.DiscoveryCwd != candidate.WorkingDirectory || !reg.SessionStartedAt.Equal(candidate.StartedAt) {
		return reg, true, errors.New("continuation identity conflict")
	}
	if reg.TranscriptPath == "" || (hint.Replace && reg.TranscriptPath == hint.Previous && reg.Origin == archive.SessionOriginDiscovery) {
		// Invalidate before moving the locator: a crash must never leave the
		// replacement trusted through the old file's same size/mtime signature.
		if err := store.RemoveScanSignature(hint.ArchiveID); err != nil {
			return reg, true, err
		}
		if err := store.SaveRequest(hint.ArchiveID, "discovery", now); err != nil {
			return reg, true, err
		}
		_, err = store.UpdateRegistration(hint.ArchiveID, func(r *archive.SessionRegistration) error {
			if r.TranscriptPath == reg.TranscriptPath {
				r.TranscriptPath = candidate.Source.Locator
				if r.Origin == archive.SessionOriginDiscovery {
					r.DiscoveryRoot = candidate.Source.Root
					r.DiscoverySourcePriority = candidate.Source.Priority
				}
			}
			reg = *r
			return nil
		})
	}
	return reg, true, err
}

// Source existence and active/archive preference are continuation hints only.
// Read them before hooks.lock; publication revalidates the chosen snapshot.
func continuationLocator(store *state.Store, agent, native string, source SourceDescriptor) (string, bool) {
	locator := source.Locator
	id, found, err := store.ArchiveSessionID(sessionKey(agent, native))
	if err != nil || !found {
		return "", false
	}
	r, found, err := store.LoadRegistration(id)
	if err != nil || !found || r.Origin != archive.SessionOriginDiscovery || r.TranscriptPath == locator {
		return "", false
	}
	_, err = os.Lstat(r.TranscriptPath)
	if errors.Is(err, os.ErrNotExist) {
		return r.TranscriptPath, true
	}
	return r.TranscriptPath, source.Priority < r.DiscoverySourcePriority
}

type scan struct {
	nativeOnly          bool
	inventoryOnly       bool
	proofs              *nativeProofPasses
	rollouts            *CodexRolloutLookup
	resolver            *sourcefacts.ProjectResolver
	recovery            *sourcefacts.RecoveryResolver
	store               *state.Store
	cfg                 config.Config
	catalog             *catalog
	health              *Health
	now                 time.Time
	adapter             SourceAdapter
	ctx                 context.Context
	priority            bool
	reservedDirectories int
	// refused records sessions refused for starting shortly before the
	// start floor, for status (nil when the pass admits nothing).
	refused *refusedStarts
}

func (s scan) visitEntry(d directory, source SourceEntry) (retry, stop bool) {
	c, h, now := s.catalog, s.health, s.now
	h.Entries++
	if source.Directory != "" {
		queued := s.enqueueDirectory(d, source.Directory)
		return !queued, !queued
	}
	if source.Source.Locator == "" {
		return false, false
	}
	loc := source.Source.Locator
	if !validSource(source.Source, d.Root) {
		h.Outcomes["invalid_source"]++
		return false, false
	}
	entry, hit := c.Cache[loc]
	retryDelay := time.Hour
	if entry.Observation.Outcome == outcomeIncomplete || entry.Observation.Outcome == outcomeUnavailable || entry.Observation.Outcome == outcomeChanged {
		retryDelay = time.Minute
	}
	if hit && cachedEntryNeedsProbe(entry, source) {
		delete(c.Cache, loc)
		hit = false
	}
	if !hit || cachedEntryNeedsRefresh(entry, source, now, retryDelay) {
		if !discoveryProbeAvailable(h, s.rollouts) {
			return true, true
		}
		headerCharge := int64(sourcefacts.HeaderBytes + 128<<10)
		if s.rollouts != nil && !s.rollouts.readBudget.Reserve(headerCharge) {
			return true, true
		}
		memberChanged := !hit || entry.Size != source.Fingerprint.Size || entry.Mtime != source.Fingerprint.Mtime || entry.SourceFingerprint != source.CoverageFingerprint
		observation := s.adapter.Inspect(s.ctx, source.Source)
		h.Probes++
		if s.rollouts != nil {
			s.rollouts.probes++
		}
		h.Bytes += observation.Bytes
		h.NativeReadBytes += observation.NativeReadBytes
		h.NativeReadOperations += observation.NativeReadOperations
		entry = cached{SourceFingerprint: source.CoverageFingerprint, Size: source.Fingerprint.Size, Mtime: source.Fingerprint.Mtime, Checked: now, Observation: observation}
		if !s.retainProbedObservation(entry, headerCharge) {
			return true, true
		}
		c.Cache[loc] = entry
		// A new or changed same-thread physical member can change the first
		// owned task or selected revision behind an earlier rejection.
		if id := entry.Observation.Identity; memberChanged && id != nil {
			for path, prior := range c.Cache {
				if path != loc && prior.Observation.Outcome == outcomeOwnTaskRejected && prior.Observation.Candidate.NativeSessionID == id.ThreadID {
					delete(c.Cache, path)
					s.retainRetry(prior.Observation.Candidate.Source)
				}
			}
		}
	}
	if s.priority && !entry.ActiveHint {
		entry.ActiveHint = true
		c.Cache[loc] = entry
	}
	if s.rollouts != nil {
		s.rollouts.Observe(source.Source, source.Fingerprint, entry.Observation.Identity)
	}
	if s.nativeOnly || s.inventoryOnly {
		return false, false
	}
	return s.admitObservation(source, entry)
}

func (s scan) admitObservation(source SourceEntry, entry cached) (bool, bool) {
	c, h, now := s.catalog, s.health, s.now
	loc := source.Source.Locator
	if observedSourceChanged(entry.Observation, source.Source) {
		delete(c.Cache, loc)
		h.Outcomes[string(outcomeChanged)]++
		s.retainRetry(source.Source)
		return false, false
	}
	h.Outcomes[string(entry.Observation.Outcome)]++
	if entry.Observation.Outcome != outcomeUsable {
		if entry.Observation.Outcome != outcomeIncomplete && entry.Observation.Outcome != outcomeRelatedHistory {
			return false, false
		}
		if entry.Observation.Candidate.NativeSessionID == "" {
			return false, false
		}
	}
	candidate := entry.Observation.Candidate
	// Shared structural checks do not trust an adapter to select another source
	// or authorize inherited/unknown evidence. Project/destination policy stays here.
	if !validCandidateMetadata(candidate, source.Source, s.adapter.Agent()) || (!candidate.FirstTaskAt.IsZero() && !validCandidate(candidate, source.Source, s.adapter.Agent(), now)) {
		h.Outcomes["invalid_candidate"]++
		return false, false
	}
	if !s.adapter.Supported(candidate) {
		h.Outcomes["unsupported_producer"]++
		return false, false
	}
	h.Supported = true
	h.observeFormat(candidate)
	if blocked, removed := s.removalBlocks(candidate); blocked {
		// Only a lookup that found the removal settles the record; one that
		// failed decides nothing about the session.
		if removed {
			s.refused.settle(candidate)
		}
		return false, false
	}
	// Lookup homes may include hooks/import locations for already admitted
	// sources. Their facts cannot expand discovery creation authority.
	if s.cfg.Discovery == nil || !s.cfg.Discovery.Enabled || !slices.Contains(approvedRoots(s.cfg.Discovery.CodexHomes), candidate.Source.Root) {
		h.Outcomes["source_not_authorized"]++
		return false, false
	}
	return s.admitCandidate(candidate, loc)
}

// retainProbedObservation transfers the probe scratch to a catalog fact owner
// before returning the scratch, including when retained ownership is refused.
func (s scan) retainProbedObservation(entry cached, scratch int64) bool {
	if s.rollouts == nil {
		return true
	}
	retained := s.rollouts.retainObservation(s.ctx, entry)
	s.rollouts.readBudget.Release(scratch)
	return retained
}

// Retained admission retries rotate separately from directory enumeration.
// One conflicting or contended source must not pin a directory's coverage.
func (s scan) retainRetry(source SourceDescriptor) {
	for _, prior := range s.catalog.Retries {
		if prior.Locator == source.Locator {
			return
		}
	}
	if len(s.catalog.Retries) == maxRetries {
		s.health.Errors = appendUnique(s.health.Errors, "retry_limit")
		return // Full filesystem reconciliation will revisit overflow sources.
	}
	s.catalog.Retries = append(s.catalog.Retries, source)
}

func (s scan) observeRetries(o Options) {
	worker := s
	worker.priority = false
	// Process only the initial queue once and reserve half the probes for
	// forward coverage, even when all retained retries remain conflicting.
	for remaining := min(len(s.catalog.Retries), 64); remaining > 0 && s.health.Probes < HeaderProbes/2 && !scanStopped(s.ctx, o); remaining-- {
		source := s.catalog.Retries[0]
		s.catalog.Retries = s.catalog.Retries[1:]
		if !slices.Contains(s.catalog.Roots, source.Root) || !validSource(source, source.Root) {
			continue
		}
		path, err := filepath.Rel(source.Root, filepath.Dir(source.Locator))
		if err != nil {
			continue
		}
		entry := s.adapter.Describe(source.Root, path, filepath.Base(source.Locator))
		worker.visitEntry(directory{Root: source.Root, Path: path}, entry)
	}
}

func (s scan) enqueueDirectory(d directory, path string) bool {
	if s.priority {
		return true
	}
	if filepath.IsAbs(path) || filepath.Clean(path) != path || !local.PathWithin(filepath.Join(d.Root, path), d.Root) {
		s.health.Outcomes["invalid_source"]++
		return true
	}
	if len(s.catalog.Queue)+s.reservedDirectories >= maxDirectories || strings.Count(d.Path, string(filepath.Separator)) >= 16 {
		s.health.Errors = appendUnique(s.health.Errors, "directory_limit")
		return true
	}
	next := directory{Root: d.Root, Path: path}
	if s.rollouts != nil && !s.rollouts.reserveCatalog(directoryByteBound(next)) {
		return false
	}
	s.catalog.Queue = append(s.catalog.Queue, next)
	return true
}

// removalBlocks avoids repeat census work for a known removed source. Retention
// and removal share collector.lock with the scanner's production caller.
// removed is true only when the lookup succeeded and found a removal.
func (s scan) removalBlocks(candidate Candidate) (blocked, removed bool) {
	if _, removed, err := s.store.Removal(candidate.Agent, candidate.NativeSessionID); err != nil {
		s.health.Outcomes["removal_unavailable"]++
		s.health.Errors = appendUnique(s.health.Errors, "removal_unavailable")
		return true, false
	} else if removed {
		s.health.Outcomes["removed_session"]++
		return true, true
	}
	return false, false
}

func pruneCatalog(c *catalog) {
	if len(c.Cache) > maxObservationCache {
		keys := make([]string, 0, len(c.Cache))
		for key := range c.Cache {
			keys = append(keys, key)
		}
		slices.SortFunc(keys, func(a, b string) int {
			if c.Cache[a].ActiveHint != c.Cache[b].ActiveHint {
				if c.Cache[a].ActiveHint {
					return 1
				}
				return -1
			}
			return c.Cache[a].Checked.Compare(c.Cache[b].Checked)
		})
		for _, key := range keys[:len(keys)-maxObservationCache] {
			delete(c.Cache, key)
		}
	}
}

func (s scan) admitCandidate(candidate Candidate, loc string) (bool, bool) {
	cfg, store, h, now := s.cfg, s.store, s.health, s.now
	var facts sourcefacts.ProjectFacts
	var root string
	var ok bool
	var ownedRoot string
	physical := cfg.EffectiveCodexCaptureScope() == config.CodexAllProjects
	if prior, blocked := s.retainedProject(candidate); blocked {
		h.Outcomes["project_not_authorized"]++
		s.refused.settle(candidate)
		return false, false
	} else if prior != nil {
		if prior.ProjectResolution != nil && prior.ProjectResolution.OriginalCwd == candidate.WorkingDirectory {
			if p, matched := sourcefacts.ConfiguredOwner(cfg.Archive.Projects, canonicalProjectPath(candidate.WorkingDirectory), canonicalProjectPath); matched && !p.Included {
				h.Outcomes["project_not_authorized"]++
				s.refused.settle(candidate)
				return false, false
			}
			ownedRoot = prior.ProjectRoot
			candidate.ProjectResolution = prior.ProjectResolution
		}
		if prior.CodexAdmission != nil {
			physical = true
		}
		if prior.CodexBinding != nil {
			candidate.SnapshotProven = true
			candidate.Binding = prior.CodexBinding
			candidate.StartedAt = prior.CodexBinding.NativeCreatedAt
		}
	}

	if ownedRoot != "" {
		root, ok = ownedRoot, true
		facts = sourcefacts.ProjectFacts{Root: root, Cwd: canonicalProjectPath(candidate.WorkingDirectory)}
	} else if physical {
		facts, ok = s.resolver.Resolve(candidate.WorkingDirectory)
		root = facts.Root
		if !ok && s.resolver.Exhausted {
			h.Outcomes["project_budget_exhausted"]++
			s.retainRetry(candidate.Source)
			return false, false
		}
	} else {
		root, ok = resolveProject(cfg, candidate.WorkingDirectory)
	}
	if !ok {
		if proof, outcome, attempted := s.recoverProject(candidate); attempted {
			if s.rejectChangedRecoverySource(candidate, loc) {
				return false, false
			}
			if outcome != "" {
				h.Outcomes[string(outcome)]++
				if outcome != sourcefacts.RecoveryExcluded {
					s.retainRetry(candidate.Source)
				} else {
					s.refused.settle(candidate)
				}
				return false, false
			}
			root, ok = proof.Root, true
			candidate.ProjectResolution = &proof
			facts = sourcefacts.ProjectFacts{Root: root, Cwd: canonicalProjectPath(candidate.WorkingDirectory)}
		}
	}

	if !ok {
		if physical {
			h.Outcomes["project_facts_unavailable"]++
			return false, false
		}
		h.Outcomes["project_not_authorized"]++
		s.refused.settle(candidate)
		return false, false
	}
	generation, authorized := cfg.DiscoveryGeneration(candidate.Agent, root, candidate.StartedAt, now)
	if cfg.EffectiveCodexCaptureScope() == config.CodexAllProjects {
		generation, authorized = cfg.CodexDiscoveryGeneration(root, facts.Cwd, candidate.StartedAt, now)
	}
	if !authorized {
		_, existing, err := store.ArchiveSessionID(sessionKey(candidate.Agent, candidate.NativeSessionID))
		if err != nil || !existing {
			h.Outcomes["start_not_authorized"]++
			if err == nil && s.startOnlyRefusal(candidate.Agent, root, facts.Cwd) {
				s.refuseProven(candidate, loc)
			} else if err == nil {
				s.refused.settle(candidate)
			}
			return false, false
		}
	}
	s.refused.settle(candidate)
	return s.finishCandidate(candidate, loc, root, generation, ownedRoot, physical, facts)
}

// startOnlyRefusal reports whether a session refused as start_not_authorized
// would be admitted had it started now: refused for its start alone, not
// also by a project rule (in all-projects scope an excluded project refuses
// through the same check), so backfill can import it.
func (s scan) startOnlyRefusal(agent, root, cwd string) bool {
	if s.cfg.EffectiveCodexCaptureScope() == config.CodexAllProjects {
		_, ok := s.cfg.CodexDiscoveryGeneration(root, cwd, s.now, s.now)
		return ok
	}
	_, ok := s.cfg.DiscoveryGeneration(agent, root, s.now, s.now)
	return ok
}

// refuseProven records a start-refused candidate only once it passes the
// native admission proof finishCandidate requires before admitting one, so a
// provisional observation (incomplete metadata, pending related history) or
// a session whose own task fails the proof is not named as one backfill
// imports. A recorded session is not proven again, and past
// maxRefusedProofs a pass leaves the rest for later passes.
func (s scan) refuseProven(candidate Candidate, loc string) {
	r := s.refused
	if r == nil {
		return
	}
	if !r.eligible(candidate, loc) {
		r.settle(candidate)
		return
	}
	if candidate.SnapshotProven {
		r.refuse(candidate, loc)
		return
	}
	if r.recorded(candidate, loc, s.catalog.Cache) || r.proofs >= maxRefusedProofs {
		return
	}
	r.proofs++
	proven, err := s.proofs.Prove(s.ctx, candidate)
	if err != nil {
		if nativeProofOutcome(err) == string(outcomeOwnTaskRejected) && s.ctx.Err() == nil {
			s.rejectOwnTask(loc)
		}
		r.settle(candidate)
		return
	}
	if !validCandidate(proven, proven.Source, s.adapter.Agent(), s.now) {
		r.settle(candidate)
		return
	}
	r.refuse(proven, loc)
}

// rejectOwnTask keeps a decisive own-task rejection in the catalog, so the
// unchanged source is not proven again, and drops its pending retry.
func (s scan) rejectOwnTask(loc string) {
	c := s.catalog
	entry := c.Cache[loc]
	entry.Observation.Outcome = outcomeOwnTaskRejected
	c.Cache[loc] = entry
	kept := c.Retries[:0]
	for _, pending := range c.Retries {
		if pending.Locator != loc {
			kept = append(kept, pending)
		}
	}
	c.Retries = kept
}

func (s scan) finishCandidate(candidate Candidate, loc, root, generation, ownedRoot string, physical bool, facts sourcefacts.ProjectFacts) (bool, bool) {
	store, h, c, now := s.store, s.health, s.catalog, s.now
	if !candidate.SnapshotProven {
		proven, err := s.proofs.Prove(s.ctx, candidate)
		if err != nil {
			outcome := nativeProofOutcome(err)
			h.Outcomes[outcome]++
			if outcome == string(outcomeOwnTaskRejected) && s.ctx.Err() == nil {
				s.rejectOwnTask(loc)
			} else {
				s.retainRetry(candidate.Source)
			}
			return false, false
		}
		if !validCandidate(proven, proven.Source, s.adapter.Agent(), now) {
			h.Outcomes["own_task_unavailable"]++
			s.retainRetry(candidate.Source)
			return false, false
		}
		return s.admitCandidate(proven, loc)
	}
	if candidate.ProjectResolution != nil && ownedRoot == "" {
		if outcome := s.recoveryEvidenceOutcome(candidate, loc); outcome != "" {
			h.Outcomes[outcome]++
			if outcome == string(outcomeChanged) {
				delete(c.Cache, loc)
			}
			s.retainRetry(candidate.Source)
			return false, false
		}
	}
	var created bool
	var e error
	if physical {
		created, e = admitSession(store, candidate, root, generation, now, facts)
	} else {
		created, e = admitSession(store, candidate, root, generation, now)
	}
	if e != nil {
		h.Outcomes["admission_retry"]++
		delete(c.Cache, loc)
		s.retainRetry(candidate.Source)
		return false, false
	}
	if created {
		h.Registered++
	}
	return false, false
}

func discoveryProbeAvailable(h *Health, rollouts *CodexRolloutLookup) bool {
	return h.Probes < HeaderProbes && (rollouts == nil || rollouts.probes < HeaderProbes-maxCurrentThreads)
}

func prepareCatalog(ctx context.Context, store *state.Store, cfg config.Config, o Options, adapter SourceAdapter, h *Health) (catalog, string, []string, error) {
	path := filepath.Join(store.Home(), "discovery-catalog.json")
	var c catalog
	if o.Rollouts != nil && o.Rollouts.catalog != nil {
		c = *o.Rollouts.catalog
	} else {
		read := func() error { return local.Read(path, &c) }
		if o.Rollouts != nil {
			read = func() error { return o.Rollouts.readCatalog(ctx, path, &c) }
		}
		if err := read(); err != nil && !errors.Is(err, os.ErrNotExist) {
			if errors.Is(err, agentapi.ErrReadBudget) {
				return c, path, nil, err
			}
			h.Errors = append(h.Errors, "catalog_rebuilt")
		}
	}
	var roots []string
	if cfg.Discovery != nil {
		roots = approvedRoots(cfg.Discovery.CodexHomes)
	}
	if o.Rollouts != nil {
		roots = slices.Clone(o.Rollouts.roots)
	}
	if c.Coverage != nil && c.Coverage.Version != 1 {
		return c, path, roots, errors.New("native coverage requires a newer writer")
	}
	if catalogNeedsReset(c, roots) {
		c = catalog{}
		if o.Rollouts != nil {
			// The discarded catalog's charge goes with it.
			o.Rollouts.releaseRestored()
		}
	}
	c.reclassify()
	if o.Rollouts != nil {
		if o.Rollouts.coverage != nil && o.Rollouts.coverage.validate(roots) == nil {
			c.Coverage = o.Rollouts.coverage
		}
		if c.Coverage == nil || c.Coverage.validate(roots) != nil {
			c.Coverage = newCoverage(roots)
		}
		o.Rollouts.coverage = c.Coverage
		c.Coverage.reserveFacts = o.Rollouts.reserveCatalog
		if c.Coverage.Phase == coverageComplete {
			c.Coverage.restart()
			c.Queue = nil
		}
	}
	c.Version = catalogVersion
	c.Roots = roots
	if c.Cache == nil {
		c.Cache = map[string]cached{}
	}
	h.LastReconciled = c.Health.LastReconciled
	if len(c.Queue) == 0 && (c.Coverage == nil || c.Coverage.Phase == coverageObserve) {
		for _, root := range roots {
			for _, path := range adapter.InitialDirectories() {
				next := directory{Root: root, Path: path}
				if o.Rollouts != nil && !o.Rollouts.reserveCatalog(directoryByteBound(next)) {
					return c, path, roots, errCatalogBudget
				}
				c.Queue = append(c.Queue, next)
			}
		}
	}
	return c, path, roots, nil
}

func (s scan) observeDirectories(o Options, deadline time.Time) {
	c, h := s.catalog, s.health
	// Retry unavailable directories before the remaining backlog next pass,
	// without revisiting them in this pass or consuming forward queue slots.
	var unavailable []directory
	for (c.Coverage == nil || c.Coverage.Phase == coverageObserve) && len(c.Queue) > 0 && discoveryProbeAvailable(h, o.Rollouts) && h.Entries < 2048 && time.Now().Before(deadline) {
		if scanStopped(s.ctx, o) {
			break
		}
		d := c.Queue[0]
		c.Queue = c.Queue[1:]
		batch, err := s.adapter.Enumerate(s.ctx, d.Root, d.Path, d.Offset)
		next, finished := batch.Continuation, batch.Complete
		if err != nil {
			if s.ctx.Err() != nil {
				c.Queue = append(c.Queue, d)
				break
			}
			h.Errors = appendUnique(h.Errors, "source_root_unavailable")
			unavailable = append(unavailable, d)
			continue
		}
		worker := scan{nativeOnly: s.nativeOnly, inventoryOnly: s.inventoryOnly, proofs: s.proofs, resolver: s.resolver, recovery: s.recovery, store: s.store, cfg: s.cfg, catalog: c, health: h, now: s.now, adapter: s.adapter, ctx: s.ctx, reservedDirectories: len(unavailable), rollouts: o.Rollouts, refused: s.refused}
		advanced := true
		for _, source := range batch.Entries {
			if scanStopped(s.ctx, o) || time.Now().After(deadline) {
				finished = false
				next = d.Offset
				advanced = false
				break
			}
			retry, stop := worker.visitEntry(d, source)
			if retry {
				finished = false
				next = d.Offset
				advanced = false
			}
			if stop {
				break
			}
		}

		if c.Coverage != nil {
			if batch.coverage == nil {
				c.Coverage.Failed = true
			} else {
				c.Coverage.recordBatch(d, *batch.coverage, next, finished, advanced)
			}
		}
		if !finished {
			d.Offset = next
			c.Queue = append(c.Queue, d)
		}
	}
	c.Queue = append(unavailable, c.Queue...)
	if c.Coverage != nil {
		if c.Coverage.Phase == coverageObserve && len(c.Queue) == 0 {
			c.Coverage.beginValidation()
		}
		advanceCoverageValidation(s.ctx, c, h, s.adapter, deadline, o)
	}
}

func (s scan) recoverProject(candidate Candidate) (archive.ProjectResolution, sourcefacts.RecoveryOutcome, bool) {
	if s.recovery == nil {
		return archive.ProjectResolution{}, "", false
	}
	if s.resolver.HasRepositoryEvidence(candidate.WorkingDirectory) {
		if s.resolver.Exhausted {
			return archive.ProjectResolution{}, sourcefacts.RecoveryBudgetExhausted, true
		}
		return archive.ProjectResolution{}, "", false
	}
	if _, err := os.Stat(candidate.WorkingDirectory); !errors.Is(err, os.ErrNotExist) {
		return archive.ProjectResolution{}, "", false
	}
	proof, outcome := s.recovery.Recover(s.ctx, candidate.WorkingDirectory, candidate.RecordedRepoKey)
	if outcome == "" {
		if info, err := os.Stat(proof.Root); err != nil || !info.IsDir() {
			outcome = sourcefacts.RecoveryInventoryUnavailable
		}
	}
	return proof, outcome, true
}

func (s scan) recoveryEvidenceOutcome(candidate Candidate, loc string) string {
	observed, cached := s.catalog.Cache[loc]
	if !cached || !sourceObservationCurrent(observed.Observation, loc) {
		return string(outcomeChanged)
	}
	_, cwdErr := os.Stat(candidate.WorkingDirectory)
	current := errors.Is(cwdErr, os.ErrNotExist) && s.recovery.Current(*candidate.ProjectResolution)
	// Repository revalidation may itself observe external changes. Keep the
	// accepted baseline until the last check before durable admission.
	if !sourceObservationCurrent(observed.Observation, loc) {
		return string(outcomeChanged)
	}
	if !current {
		if s.recovery.MetadataExhausted {
			return string(sourcefacts.RecoveryBudgetExhausted)
		}
		return string(sourcefacts.RecoveryInventoryUnavailable)
	}
	return ""
}

// Rejected-task fingerprints schedule revalidation; they never grant admission.
func cachedEntryNeedsProbe(entry cached, source SourceEntry) bool {
	return cachedObservationNeedsProbe(entry.Observation, source.Source) || entry.Observation.Outcome == outcomeOwnTaskRejected && (entry.SourceFingerprint == "" || entry.SourceFingerprint != source.CoverageFingerprint)
}

func cachedEntryNeedsRefresh(entry cached, source SourceEntry, now time.Time, retryDelay time.Duration) bool {
	return entry.Size != source.Fingerprint.Size || entry.Mtime != source.Fingerprint.Mtime || entry.Observation.Outcome != outcomeOwnTaskRejected && (now.Sub(entry.Checked) >= retryDelay || now.Before(entry.Checked))
}

// Persisted hints cannot recreate the identity behind recovered ownership.
// Live cached facts keep their baseline.
func cachedObservationNeedsProbe(observed Observation, source SourceDescriptor) bool {
	if (observed.Outcome != outcomeUsable && observed.Outcome != outcomeOwnTaskRejected) || source.Kind != archive.SourceKindFile {
		return false
	}
	if observed.SourceInfo == nil {
		return missingSourceCwd(observed.Candidate)
	}
	return !sourceObservationCurrent(observed, source.Locator)
}

func observedSourceChanged(observed Observation, source SourceDescriptor) bool {
	return observed.Outcome == outcomeUsable && source.Kind == archive.SourceKindFile && observed.SourceInfo != nil && !sourceObservationCurrent(observed, source.Locator)
}

func (s scan) rejectChangedRecoverySource(candidate Candidate, loc string) bool {
	observed, found := s.catalog.Cache[loc]
	if found && sourceObservationCurrent(observed.Observation, loc) {
		return false
	}
	delete(s.catalog.Cache, loc)
	s.health.Outcomes[string(outcomeChanged)]++
	s.retainRetry(candidate.Source)
	return true
}

func missingSourceCwd(candidate Candidate) bool {
	_, err := os.Stat(candidate.WorkingDirectory)
	return errors.Is(err, os.ErrNotExist)
}

func sourceObservationCurrent(observed Observation, loc string) bool {
	current, err := os.Lstat(loc)
	return err == nil && transcriptio.SameObservation(observed.SourceInfo, current)
}

func (s scan) retainedProject(candidate Candidate) (*archive.SessionRegistration, bool) {
	id, known, err := s.store.ArchiveSessionID(sessionKey(candidate.Agent, candidate.NativeSessionID))
	if err != nil || !known {
		return nil, false
	}
	prior, found, err := s.store.LoadRegistration(id)
	if err != nil || !found {
		return nil, false
	}
	if !s.cfg.AcceptSession(prior) {
		return nil, true
	}
	return &prior, false
}

func validateAdmissionGeneration(cfg config.Config, candidate Candidate, project, generation string, now time.Time, facts sourcefacts.ProjectFacts, factsOK bool) error {
	if candidate.ProjectResolution != nil && candidate.ProjectResolution.Method != "explicit_mapping" && candidate.ProjectResolution.Context != sourcefacts.RecoveryContext(cfg.Archive.Projects, nil, filepath.Clean) {
		return errors.New("project recovery policy changed")
	}
	current, allowed := cfg.DiscoveryGeneration(candidate.Agent, project, candidate.StartedAt, now)
	if cfg.EffectiveCodexCaptureScope() == config.CodexAllProjects {
		if !factsOK || facts.Root != project || cfg.Discovery == nil || !cfg.Discovery.Enabled {
			return errors.New("project facts changed")
		}
		current, allowed = cfg.CodexDiscoveryGeneration(facts.Root, facts.Cwd, candidate.StartedAt, now)
	}
	if !allowed || current != generation {
		return errors.New("authorization changed")
	}
	return nil
}

func hasValidatedCoverage(coverage *coverageInventory) bool {
	return coverage != nil && coverage.Phase == coverageComplete && coverage.proofEpoch == coverage.Epoch
}

func catalogPending(c catalog, nativeOnly bool) bool {
	return len(c.Queue) > 0 || !nativeOnly && len(c.Retries) > 0 || c.Coverage != nil && c.Coverage.Phase != coverageComplete
}

// deferCheckpoint hands a catalog whose checkpoint ran out of its slice (it
// can, just after coverage completed) to the lookup's close, which writes it
// without the pass deadline, so a completed epoch and the proof it becomes
// are not lost with the checkpoint. Other write failures stay this pass's.
func (l *CodexRolloutLookup) deferCheckpoint(c *catalog, err error) {
	if l != nil && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)) {
		l.catalog = c
		l.coverageDirty = true
	}
}

// checkpointCatalog persists the pass's catalog. One that, even without its
// observation cache, is past the size the next read accepts is not written:
// the previous file stays, the pass goes on, and health says so.
func checkpointCatalog(ctx context.Context, lookup *CodexRolloutLookup, path string, c *catalog, h *Health) error {
	err := persistCatalog(ctx, lookup, path, *c)
	if errors.Is(err, errCatalogTooLarge) {
		h.Errors = append(h.Errors, "catalog_over_cap")
		return nil
	}
	if err != nil {
		lookup.deferCheckpoint(c, err)
		return errors.Join(errors.New("discovery state write failed; retry next scan"), err)
	}
	return nil
}

// persistCatalog is persistNativeCatalog; tests replace it to fail a checkpoint.
var persistCatalog = persistNativeCatalog

func persistNativeCatalog(ctx context.Context, lookup *CodexRolloutLookup, path string, c catalog) error {
	if lookup != nil {
		return lookup.writeCatalog(ctx, path, c)
	}
	return local.WriteCompact(path, c)
}
