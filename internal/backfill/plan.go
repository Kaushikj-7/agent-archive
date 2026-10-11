package backfill

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

// Plan is everything one backfill run found and decided. It is the input the
// import step registers from; its rendering never shows a transcript path,
// native ID, or content.
type Plan struct {
	GeneratedAt time.Time
	// Home is the user's home directory, for showing paths with ~.
	Home          string
	Filters       Filters
	Destination   Destination
	RetentionDays int
	// Harnesses are the apps with hooks installed (config.Config.Harnesses).
	Harnesses  []string
	Candidates []Candidate
	// CursorDatabaseChecked says whether Cursor's database was read, so the
	// chats found only there are among Candidates; CursorDatabaseUnchecked
	// says why it was not.
	CursorDatabaseChecked   bool
	CursorDatabaseUnchecked CursorUncheckedReason
	// CursorSubagentsNotImported counts the subagent chats, in Cursor's
	// database, of the Cursor chats the plan imports. They are not imported
	// yet.
	CursorSubagentsNotImported int
	// CursorDatabaseNewerFormat counts the database rows read with a newer
	// format version than this release knows.
	CursorDatabaseNewerFormat int
	// UnreadableFolders counts the folders in the apps' stores that could not
	// be listed; the sessions in them were not found.
	UnreadableFolders int
	// UnreadableStores are the apps, matching the filters, whose session
	// store could not be listed at all: none of their sessions were found
	// (for Codex, when codexArchivedOnly is set, only its archived ones).
	// Importing the rest is still allowed; a run after the permissions are
	// fixed imports what was missed.
	UnreadableStores []string
	// RecoveryEvidenceGaps are the apps whose recovery witness evidence was
	// incomplete, with the sessions each blocked or was recovered without.
	RecoveryEvidenceGaps []RecoveryEvidenceGap

	// resolvedHome is Home with symlinks resolved; roots are resolved paths.
	resolvedHome string
	// projectFilter holds the --project directories, resolved as the plan
	// compared them.
	projectFilter []string
	// codexArchivedOnly is set when only Codex's archived_sessions folder
	// could not be listed.
	codexArchivedOnly bool
	// cursorIncomplete is set when Cursor's transcript store could not be
	// fully listed; the plan then can't tell which of the database's chats
	// have transcripts, so it does not read the database.
	cursorIncomplete bool
	// nested holds, for each project the plan adds that would capture its
	// subfolders, the folders inside it the import keeps out (see nested.go).
	nested map[string]nestedFolders
	// policyConfig is the committed configuration the recovery policy was
	// bound against (bindRecoveryPolicy), so AdmitRecovery can bind it again
	// for the sessions it keeps.
	policyConfig *config.Config
}

// Destination names the bucket imports go to.
type Destination struct {
	Provider string `json:"provider"`
	Bucket   string `json:"bucket"`
	Prefix   string `json:"prefix"`
}

// workers is the spec's filter worker count: filtering is CPU-bound, and
// using every core would cost the person's machine more than it saves.
func defaultWorkers() int {
	return min(8, max(2, runtime.NumCPU()/2))
}

// maxBytesInFlight caps the transcript bytes being filtered at once, so two
// files near the 64 MiB limit are never read together. A file larger than
// the cap runs alone.
const maxBytesInFlight = 128 << 20

// work is one transcript and the facts the plan decides its reason from.
type work struct {
	t   *transcript
	c   Candidate
	res resolution
	// state is the archive's reason, from ArchiveState.
	state SkipReason
	// chat is a chat found only in Cursor's database, and messageFolders
	// the folders its messages name (see planCursorDatabase).
	chat           CursorDatabaseChat
	messageFolders []string
	// vanished is set when the file disappeared while planning; the session
	// is then not counted at all.
	vanished bool
	filtered bool
	// duplicated is set when another file carries the same harness and
	// native ID; duplicate when this file is not the one kept.
	duplicated bool
	duplicate  bool
	// Adapter outcomes.
	empty            bool
	unsafe           bool
	sourceChanged    bool
	tooLarge         bool
	sourceErr        error
	workspaceCurrent func(context.Context) bool
	proposedWitness  bool
	validated        bool
	// sourceRewritten is set when a change to the source was not a pure
	// append (see appendedOnly); appendWitness is an appended source's header
	// resolution, kept only as recovery evidence. evidenceOnly marks a witness
	// that can make a recovery ambiguous but cannot propose a destination.
	sourceRewritten bool
	appendWitness   *resolution
	evidenceOnly    bool
	// addedSource marks an evidence-only witness from a transcript added
	// while planning (addedWitnesses); its renewal accepts appends.
	addedSource bool
}

// subagentWork is one subagent transcript of an imported parent.
type subagentWork struct {
	parent    *work
	sub       Subagent
	skipped   bool
	vanished  bool
	sourceErr error
}

// importable reports whether nothing about the file itself stops it being
// imported: the checks a session's copies can differ on.
func (w *work) importable() bool {
	return !w.tooLarge && !w.unsafe && !w.empty && !w.t.identityMismatch && !w.t.capturePending && !w.sourceChanged
}

// markDuplicates keeps one file of a session found more than once and marks
// the rest. The kept file is one that can be imported, so a larger copy that
// is too large, empty, or refused never displaces a good one; then one whose
// own IDs agree, then a Codex file in sessions/ over archived_sessions/, then
// the larger file, then the lexically smallest path.
func markDuplicates(group []*work) {
	var live []*work
	for _, w := range group {
		if !w.vanished {
			live = append(live, w)
		}
	}
	if len(live) < 2 {
		return
	}

	sort.SliceStable(live, func(i, j int) bool {
		a, b := live[i], live[j]
		if a.importable() != b.importable() {
			return a.importable()
		}
		if a.t.identityMismatch != b.t.identityMismatch {
			return !a.t.identityMismatch
		}
		if a.t.sourcePriority != b.t.sourcePriority {
			return a.t.sourcePriority > b.t.sourcePriority
		}
		if a.t.size != b.t.size {
			return a.t.size > b.t.size
		}
		return a.t.path < b.t.path
	})
	for _, w := range live[1:] {
		w.duplicate = true
	}
}

// BuildPlan finds every session on this machine and decides, for each, whether it
// is imported or why not. It writes nothing.
func BuildPlan(ctx context.Context, env Environment, state ArchiveState, cfg config.Config, filters Filters) (Plan, error) {
	var err error
	filters, err = filters.Canonicalize(env.Discovery)
	if err != nil {
		return Plan{}, err
	}
	now := env.now()
	items, r, unread, workers, err := preparePlanWork(ctx, env, cfg, filters)
	if err != nil {
		return Plan{}, err
	}

	if env.PrepareCodexProof != nil {
		var ids []string
		for _, w := range items {
			if w.t.harness == harnessCodex && (w.c.NativeChild || w.c.RelatedHistory) && w.c.NativeSessionID != "" {
				ids = append(ids, w.c.NativeSessionID)
			}
		}
		if err := env.PrepareCodexProof(ctx, ids); err != nil {
			return Plan{}, err
		}
	}
	projectFilter := make([]string, 0, len(filters.Projects))
	for _, p := range filters.Projects {
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		projectFilter = append(projectFilter, env.resolved(p))
	}
	since, until := dateRange(filters, now.Location())

	if err := classifyPlanWork(ctx, env, state, filters, items, projectFilter, since, until, workers); err != nil {
		return Plan{}, err
	}

	for _, w := range items {
		if w.res.proof != nil && !w.res.included && !r.proposedRootEligible(w.res.root) {
			w.res = resolution{skip: SkipWorktreeUnresolved}
		}
	}
	if err := finalizePlanWork(ctx, env, items, &unread, since, until, now, workers); err != nil {
		return Plan{}, err
	}

	var candidates []Candidate
	for _, w := range items {
		if !w.vanished {
			candidates = append(candidates, w.c)
		}
	}
	var unreadableStores []string
	for _, h := range presentationAgents(sortedAgentKeys(unread.stores)) {
		// A store the filters leave out is not reported.
		if unread.stores[h] && harnessMatches(filters.Harnesses, h) {
			unreadableStores = append(unreadableStores, h)
		}
	}
	plan := Plan{
		GeneratedAt:       now,
		Home:              env.Home,
		Filters:           filters,
		Destination:       Destination{Provider: cfg.Storage.Provider, Bucket: cfg.Storage.Bucket, Prefix: cfg.Storage.Prefix},
		RetentionDays:     cfg.RetentionDays,
		Harnesses:         append([]string(nil), cfg.Harnesses...),
		Candidates:        candidates,
		UnreadableFolders: unread.folders,
		UnreadableStores:  unreadableStores,
		resolvedHome:      env.resolved(env.Home),
		projectFilter:     projectFilter,
		codexArchivedOnly: unread.codexArchivedOnly,
		cursorIncomplete:  unread.cursorIncomplete,
		// Decisions are final here; database-only chats never use recorded recovery.
		RecoveryEvidenceGaps: summarizeRecoveryGaps(r.recoveryGaps, items),
	}

	if err := planCursorDatabase(ctx, env, state, r, projectFilter, since, until, workers, &plan); err != nil {
		return Plan{}, err
	}
	if err := planNested(ctx, r, &plan); err != nil {
		return Plan{}, err
	}
	sort.SliceStable(plan.Candidates, func(i, j int) bool {
		a, b := plan.Candidates[i], plan.Candidates[j]
		if a.Harness != b.Harness {
			return a.Harness < b.Harness
		}
		if a.TranscriptPath != b.TranscriptPath {
			return a.TranscriptPath < b.TranscriptPath
		}
		return a.SourceKey < b.SourceKey
	})
	if err := bindRecoveryPolicy(cfg, &plan); err != nil {
		return Plan{}, err
	}
	return plan, nil
}

// preparePlanWork performs local discovery, reads transcript heads, and maps
// working directories to projects before any archive-state classification.
func preparePlanWork(ctx context.Context, env Environment, cfg config.Config, filters Filters) ([]*work, *resolver, unreadable, int, error) {
	var unread unreadable
	if err := validateProjectMappings(env, cfg, filters.ProjectMappings); err != nil {
		return nil, nil, unread, 0, err
	}
	workers := env.Workers
	if workers <= 0 {
		workers = defaultWorkers()
	}
	var items []*work
	var err error
	inventory := newRecoverySourceInventory(env)
	unread, err = enumerateDiscovery(ctx, inventory.environment(), agentapi.DiscoveryImport, func(c agentapi.DiscoveryCandidate) error {
		t := &transcript{harness: harness(c.Session.Agent), path: c.Source.Path, size: c.Bytes, nativeID: c.Session.NativeID, cwd: c.Header.Directory, repoKey: c.Header.RepoKey, metaStart: c.Header.StartedAt, identityMismatch: c.Header.IdentityMismatch, capturePending: c.Header.CapturePending != "" && env.CodexRollouts == nil, cursorSlug: c.WorkspaceKey, sourcePriority: c.SourcePriority, sourceInfo: c.SourceInfo}
		w := &work{t: t, c: importNativeCandidate(c, t), unsafe: c.IdentityError != nil}
		w.checkSource(env)
		items = append(items, w)
		return nil
	})
	if err != nil {
		return nil, nil, unread, workers, err
	}

	// Projects. Cursor's come last: its slugs are matched against the roots
	// the other apps' sessions resolved to.
	if err := validateProjectMappings(env, cfg, filters.ProjectMappings); err != nil {
		return nil, nil, unread, workers, err
	}
	for _, w := range items {
		inventory.ownContent(w.t.path, w.t.sourceInfo)
	}
	r := newResolver(env, cfg, filters)
	r.inventoryAdded = inventory.added
	var cursorCandidates []string
	for _, p := range cfg.Archive.Projects {
		cursorCandidates = append(cursorCandidates, p.Root)
	}
	for _, w := range items {
		if w.t.cursorSlug != "" || w.vanished {
			continue
		}
		// Header facts and all later reads share the pre-header observation.
		// A settled rewrite during discovery or Git lookup cannot become a new
		// baseline for attribution from the earlier header.
		w.checkSource(env)
		if w.appendedOnly() {
			// The session itself stays source_changed; its unchanged header
			// remains clone evidence for other sessions (appendWitness).
			res := r.resolve(w.t.cwd)
			w.appendWitness = &res
			continue
		}
		if w.sourceChanged || w.vanished {
			continue
		}
		w.res = r.resolve(w.t.cwd)
		w.checkSource(env)
		if w.t.cwd != "" {
			cursorCandidates = append(cursorCandidates, w.t.cwd)
		}
		if w.res.root != "" {
			cursorCandidates = append(cursorCandidates, w.res.root)
		}
	}
	if err := resolveWorkspaceWitnesses(ctx, env, r, items, cursorCandidates); err != nil {
		return nil, nil, unread, workers, err
	}

	dbWitnesses, dbGaps, err := prepareCursorRecoveryWitnesses(ctx, env, r, items, unread)
	if err != nil {
		return nil, nil, unread, workers, err
	}
	prepareRecoveryInventory(ctx, r, append(slices.Clone(items), dbWitnesses...), unread, dbGaps)
	for _, w := range items {
		if w.t.cursorSlug != "" || w.vanished || w.sourceChanged || w.tooLarge {
			continue
		}
		w.checkSource(env)
		if w.sourceChanged || w.vanished {
			continue
		}
		w.res = r.resolveSessionEvidence(ctx, w.t.harness, w.t.cwd, w.t.repoKey)
		w.checkSource(env)
		if w.t.sourceInfo != nil && w.res.current != nil {
			base := w.res.current
			// The session's own source may only have grown: its header, which
			// recovery used, comes from leading records an append cannot change.
			w.res.current = &resolutionCheck{reset: base.reset, valid: func() bool {
				current, err := env.lstat(w.t.path)
				return (w.matchesSource(current, err) || appendedSource(w.t.sourceInfo, current, err)) && base.valid()
			}}
		}
	}
	return items, r, unread, workers, nil
}

// resolveWorkspaceWitnesses retains one renewed matcher per agent and slice.
func resolveWorkspaceWitnesses(ctx context.Context, env Environment, r *resolver, items []*work, cursorCandidates []string) error {
	matchers := map[string]*workspaceMatcher{}
	freshMatchers := map[string]*workspaceMatcher{}
	r.workspaceReset = func() { freshMatchers = map[string]*workspaceMatcher{} }
	for _, w := range items {
		if w.t.cursorSlug == "" {
			continue
		}
		name := string(w.t.harness)
		matcher := matchers[name]
		if matcher == nil {
			matcher = &workspaceMatcher{env: env, agent: name, candidates: cursorCandidates}
			matchers[name] = matcher
		}
		folder, ok, err := matcher.match(ctx, w.t.cursorSlug)
		if err != nil {
			return err
		}
		if ok {
			w.res = r.resolve(folder)
			w.workspaceCurrent = func(ctx context.Context) bool {
				fresh := freshMatchers[name]
				if fresh == nil {
					fresh = &workspaceMatcher{env: env, agent: name, candidates: cursorCandidates}
					freshMatchers[name] = fresh
				}
				current, matched, err := fresh.match(ctx, w.t.cursorSlug)
				return err == nil && matched && env.resolved(current) == env.resolved(folder)
			}
		} else {
			w.res = resolution{skip: SkipProjectUnknown}
		}
	}

	return nil
}

// classifyPlanWork asks the archive about native IDs, then reads full
// transcripts only where the decision requires their contents or start time.
func classifyPlanWork(ctx context.Context, env Environment, state ArchiveState, filters Filters, items []*work, projectFilter []string, since, until time.Time, workers int) error {
	sessions := map[agentmeta.SessionKey][]*work{}
	for _, w := range items {
		if w.vanished {
			continue
		}
		if strings.TrimSpace(w.c.NativeSessionID) != "" {
			key, keyErr := agentmeta.NewSessionKey(string(w.t.harness), w.c.NativeSessionID)
			if keyErr != nil {
				w.unsafe = true
				continue
			}
			reason, err := state.Classify(string(w.t.harness), w.c.NativeSessionID)
			if err != nil {
				return fmt.Errorf("check the archive: %w", err)
			}
			if filters.IncludeRemoved && (reason == SkipRemovedByUndo || reason == SkipRemovedByRetention) {
				reason = ""
			}
			w.state = reason
			sessions[key] = append(sessions[key], w)
		}
		w.filtered = !harnessMatches(filters.Harnesses, string(w.t.harness)) || !projectMatches(env, projectFilter, w.res.root)
		w.tooLarge = w.tooLarge || w.t.size > collector.DefaultMaxRawTranscriptBytes
	}
	for _, group := range sessions {
		markPendingHistory(group)
		if len(group) > 1 {
			for _, w := range group {
				w.duplicated = true
			}
		}
	}

	// The adapter runs over every transcript that may be imported, as the
	// collector will, so the plan's counts are what gets imported. A session
	// already decided by an earlier reason is not read, unless a date filter
	// needs its start or a duplicate needs its identity checked.
	toFilter := selectAdapterWork(items, since, until)
	budget := newByteBudget(maxBytesInFlight)
	if err := forEach(ctx, workers, toFilter, func(w *work) {
		n := budget.acquire(w.t.size)
		defer budget.release(n)
		runAdapter(ctx, env, w)
	}); err != nil {
		return err
	}
	for _, w := range toFilter {
		if w.sourceErr != nil {
			return w.sourceErr
		}
	}
	for _, group := range sessions {
		markPendingHistory(group)
		markDuplicates(group)
	}

	return nil
}

// Metadata found beyond the bounded header can also establish related history.
// Every observed sibling must stay pending before duplicate selection.
func markPendingHistory(group []*work) {
	for _, w := range group {
		if w.t.capturePending {
			for _, sibling := range group {
				sibling.t.capturePending = true
			}
			return
		}
	}
}

// selectAdapterWork decides which whole transcripts need filtering. It does
// not open files, which keeps the expensive adapter pass bounded to work
// whose start, identity, or importability still needs evidence.
func selectAdapterWork(items []*work, since, until time.Time) []*work {
	dated := !since.IsZero() || !until.IsZero()
	var selected []*work
	for _, w := range items {
		if w.vanished || (w.state == SkipAlreadyArchived && !w.proposedWitness) || w.tooLarge || w.unsafe || w.sourceChanged || w.t.capturePending {
			continue
		}
		if w.proposedWitness || w.duplicated || (w.state == "" && !w.filtered && (dated || (w.res.skip == "" && !w.t.identityMismatch))) {
			selected = append(selected, w)
		}
	}
	return selected
}

// finalizePlanWork decides skip reasons and checks subagents of imported
// Claude sessions. Its I/O is confined to project existence and subagents.
func finalizePlanWork(ctx context.Context, env Environment, items []*work, unread *unreadable, since, until, now time.Time, workers int) error {
	budget := newByteBudget(maxBytesInFlight)
	for _, w := range items {
		if !w.vanished && w.res.root != "" {
			w.c.ProjectExists = env.exists(w.res.root)
		}
	}
	parents := decidePlanCandidates(items, since, until, now)

	// Subagent transcripts of imported parents pass the same checks as their
	// parents; one that fails is left out and counted.
	var subagents []*subagentWork
	for _, w := range parents {
		children, err := discoverChildren(ctx, env, w.t, unread)
		if err != nil {
			return err
		}
		for _, sub := range children {
			subagents = append(subagents, &subagentWork{parent: w, sub: sub})
		}
	}
	if err := forEach(ctx, workers, subagents, func(s *subagentWork) {
		if s.sub.Bytes > collector.DefaultMaxRawTranscriptBytes {
			s.skipped = true
			return
		}
		n := budget.acquire(s.sub.Bytes)
		defer budget.release(n)
		filtered, _, err := collector.FilterSource(ctx, string(s.parent.t.harness), agentapi.SourceRef{Path: s.sub.Path}, time.Time{}, env.Sources)
		if err != nil {
			if fatalSourceFailure(err) {
				s.sourceErr = err
				return
			}
			s.vanished = isNotExist(err)
			s.skipped = !s.vanished
			return
		}
		// The collector registers the subagent only if it passes these
		// checks; observed now, it is observed no later at the import.
		s.skipped = collector.CheckImportedSubagent(filtered, s.parent.c.NativeSessionID, s.sub.AgentID, s.parent.c.StartedAt, now) != nil
	}); err != nil {
		return err
	}
	for _, s := range subagents {
		if s.sourceErr != nil {
			return s.sourceErr
		}
		switch {
		case s.vanished:
		case s.skipped:
			s.parent.c.SubagentsSkipped++
		default:
			s.parent.c.Subagents = append(s.parent.c.Subagents, s.sub)
		}
	}
	return nil
}

// decidePlanCandidates applies final date and skip decisions after the
// transcript and project existence evidence has been gathered.
func decidePlanCandidates(items []*work, since, until, now time.Time) []*work {
	dated := !since.IsZero() || !until.IsZero()
	var parents []*work
	for _, w := range items {
		if w.vanished {
			continue
		}
		// A session without a start is not judged by the date filters: it
		// keeps the reason that left it without one.
		if dated && w.state == "" && !w.c.StartedAt.IsZero() && !inRange(w.c.StartedAt, since, until) {
			w.filtered = true
		}
		w.c.ProjectRoot, w.c.ProjectKind, w.c.ProjectIncluded = w.res.root, w.res.kind, w.res.included
		w.c.ProjectResolution = w.res.proof
		if w.res.current != nil {
			w.c.projectResolutionCurrent = w.res.current.valid
			w.c.projectResolutionReset = w.res.current.reset
		}
		w.c.Skip = w.reason(now)
		w.c.Diagnostic = candidateDiagnostic(w.c.Skip, w.res.outcome, w.res.cause)
		if w.c.Skip == "" {
			parents = append(parents, w)
		}
	}
	return parents
}

// reason picks the first applicable skip reason in the spec's order.
func (w *work) reason(now time.Time) SkipReason {
	applies := map[SkipReason]bool{
		w.state:    w.state != "",
		w.res.skip: w.res.skip != "",
	}
	for reason, set := range map[SkipReason]bool{
		SkipDuplicateSession: w.duplicate,
		SkipFilteredOut:      w.filtered,
		SkipIdentityMismatch: w.t.identityMismatch && !w.sourceChanged,
		SkipRelatedHistory:   w.t.capturePending && !w.sourceChanged,
		SkipEmpty:            w.empty,
		SkipUnsafeFormat:     w.unsafe,
		SkipSourceChanged:    w.sourceChanged && !w.tooLarge,
		SkipTooLarge:         w.tooLarge,
		// A session that would register without a start time cannot be
		// imported: the registration requires one. Only Cursor's start
		// comes from the file; a Claude Code or Codex session gets its
		// start from its records or not at all.
		SkipStartUnknown:  w.state == "" && w.c.StartedAt.IsZero(),
		SkipStartInFuture: w.c.StartedAt.After(now),
	} {
		applies[reason] = applies[reason] || set
	}
	for _, reason := range skipOrder {
		if applies[reason] {
			return reason
		}
	}
	return ""
}

// checkSource preserves disappearance and size-limit outcomes while treating
// other changes to the header observation as a retryable source change.
func (w *work) checkSource(env Environment) {
	if w.t.sourceInfo == nil {
		return
	}
	current, err := env.lstat(w.t.path)
	if isNotExist(err) {
		w.vanished = true
		return
	}
	if err == nil && current.Size() > collector.DefaultMaxRawTranscriptBytes {
		w.tooLarge = true
	}
	if !w.matchesSource(current, err) {
		w.sourceChanged = true
		w.sourceRewritten = w.sourceRewritten || !appendedSource(w.t.sourceInfo, current, err)
	}
}

// sourceCurrent compares with the observation that produced the native header.
// Providers without file headers (such as Cursor's catalog) keep their own bounds.
func (w *work) sourceCurrent(env Environment) bool {
	if w.t.sourceInfo == nil {
		return true
	}
	current, err := env.lstat(w.t.path)
	return w.matchesSource(current, err)
}

func (w *work) matchesSource(current os.FileInfo, err error) bool {
	original := w.t.sourceInfo
	return err == nil && transcriptio.SameObservation(original, current)
}

// runAdapter filters the whole transcript with the collector's own code and
// keeps only what the plan needs: whether anything is left, the IDs the
// records carry, and the earliest record's time.
func runAdapter(ctx context.Context, env Environment, w *work) {
	w.checkSource(env)
	if w.sourceChanged || w.vanished || w.tooLarge {
		return
	}
	defer func() {
		w.checkSource(env)
	}()
	if env.Imports == nil {
		w.unsafe = true
		return
	}
	inspector, ok := env.Imports.LookupImport(string(w.t.harness))
	if !ok {
		w.unsafe = true
		return
	}
	var freshStart time.Time
	if inspector.ImportPolicy(agentapi.SourceRef{Kind: w.c.SourceKind, Path: w.t.path, Key: w.c.SourceKey}).Start == agentapi.ImportFileCreatedStart {
		// Cursor records carry no timestamps; the file's creation is the
		// start, and the text filter needs it as its fresh-start proof.
		created, err := env.fileCreated(w.t.path)
		if err != nil {
			if isNotExist(err) {
				w.vanished = true
			} else {
				w.unsafe = true
			}
			return
		}
		freshStart = created.UTC()
		w.c.StartedAt, w.c.StartedAtSource = freshStart, archive.StartedAtSourceFileCreated
	}
	filtered, release, err := filterImportSource(ctx, env, w, freshStart)
	defer release()
	if err != nil {
		if fatalSourceFailure(err) {
			w.sourceErr = err
			return
		}
		info, statErr := env.lstat(w.t.path)
		switch {
		case isNotExist(err) || isNotExist(statErr):
			w.vanished = true
		case agentapi.HasFailure(err, agentapi.Changed):
			w.sourceChanged = true
		case errors.Is(err, archive.ErrRelatedHistory) || agentapi.HasFailure(err, agentapi.Unavailable) || agentapi.HasFailure(err, agentapi.Limit):
			w.t.capturePending = true
		case errors.Is(err, archive.ErrRecordTooLarge) || (statErr == nil && info.Size() > collector.DefaultMaxRawTranscriptBytes):
			// One record over the limit, or a file that grew past it since
			// discovery: the collector blocks it as too large.
			w.tooLarge = true
		default:
			// The collector would refuse it too, and block the registration
			// for good.
			w.unsafe = true
		}
		return
	}
	applyImportInspection(ctx, inspector, w, filtered)
}

func applyImportInspection(ctx context.Context, inspector agentapi.ImportInspector, w *work, filtered archive.FilteredTranscript) {
	observed, err := inspector.InspectImport(ctx, agentapi.ImportInspectionRequest{Session: agentapi.NativeSession{Agent: agentmeta.ID(w.t.harness), NativeID: w.t.nativeID}, Source: agentapi.SourceRef{Kind: w.c.SourceKind, Path: w.t.path, Key: w.c.SourceKey}, Header: agentapi.NativeHeader{NativeID: w.t.nativeID, Directory: w.t.cwd, StartedAt: w.t.metaStart}, Filtered: filtered})
	if err != nil {
		w.unsafe = true
		return
	}
	w.validated = true
	w.empty = !observed.Conversation
	w.t.identityMismatch = w.t.identityMismatch || observed.IdentityMismatch
	if !observed.StartedAt.IsZero() {
		w.c.StartedAt, w.c.StartedAtSource = observed.StartedAt.UTC(), archive.StartedAtSourceTranscript
	}
}

func fatalSourceFailure(err error) bool {
	return agentapi.HasFailure(err, agentapi.Cleanup) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// discoverChildren consumes native association evidence while retaining shared
// byte budgets, identity checks and admission for the selected imported parent.
func discoverChildren(ctx context.Context, env Environment, t *transcript, u *unreadable) ([]Subagent, error) {
	if env.Children == nil {
		return nil, nil
	}
	provider, ok := env.Children.LookupChildren(string(t.harness))
	if !ok {
		return nil, nil
	}
	var children []Subagent
	report, err := provider.DiscoverChildren(ctx, agentapi.ChildDiscoveryRequest{Parent: agentapi.NativeSession{Agent: agentmeta.ID(t.harness), NativeID: t.nativeID}, Source: agentapi.SourceRef{Path: t.path}, Files: discoveryFiles{env}}, func(c agentapi.ChildCandidate) error {
		children = append(children, Subagent{Path: c.Source.Path, AgentID: c.NativeID, Bytes: c.Bytes})
		return nil
	})
	u.folders += report.UnreadableFolders
	return children, err
}

func harnessMatches(harnesses []string, harness string) bool {
	if len(harnesses) == 0 {
		return true
	}
	for _, h := range harnesses {
		if archive.CanonicalHarness(h) == harness {
			return true
		}
	}
	return false
}

// projectMatches compares a session's project root with the --project
// directories, both resolved. A session without a project matches none.
func projectMatches(env Environment, projects []string, root string) bool {
	if len(projects) == 0 {
		return true
	}
	if root == "" {
		return false
	}
	return slices.Contains(projects, env.resolved(root))
}

// dateRange turns --since and --until into [since, until) in loc.
func dateRange(f Filters, loc *time.Location) (since, until time.Time) {
	if f.Since != "" {
		since, _ = time.ParseInLocation(dateLayout, f.Since, loc)
	}
	if f.Until != "" {
		if day, err := time.ParseInLocation(dateLayout, f.Until, loc); err == nil {
			until = day.AddDate(0, 0, 1)
		}
	}
	return since, until
}

func inRange(t, since, until time.Time) bool {
	if t.IsZero() {
		return false
	}
	return (since.IsZero() || !t.Before(since)) && (until.IsZero() || t.Before(until))
}

// forEach runs fn over items with a fixed number of workers.
func forEach[T any](ctx context.Context, workers int, items []T, fn func(T)) error {
	jobs := make(chan T)
	var wg sync.WaitGroup
	for range min(workers, max(1, len(items))) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for w := range jobs {
				fn(w)
			}
		}()
	}
	var err error
	for _, w := range items {
		if err = ctx.Err(); err != nil {
			break
		}
		jobs <- w
	}
	close(jobs)
	wg.Wait()
	if err != nil {
		return err
	}
	return ctx.Err()
}

// byteBudget is a counting semaphore over bytes.
type byteBudget struct {
	mu        sync.Mutex
	available *sync.Cond
	limit     int64
	used      int64
}

func newByteBudget(limit int64) *byteBudget {
	b := &byteBudget{limit: limit}
	b.available = sync.NewCond(&b.mu)
	return b
}

// acquire waits until n bytes fit and returns what it took; a request larger
// than the whole budget takes all of it, so that file runs alone.
func (b *byteBudget) acquire(n int64) int64 {
	n = min(max(n, 0), b.limit)
	b.mu.Lock()
	defer b.mu.Unlock()
	for b.used+n > b.limit {
		b.available.Wait()
	}
	b.used += n
	return n
}

func (b *byteBudget) release(n int64) {
	b.mu.Lock()
	b.used -= n
	b.mu.Unlock()
	b.available.Broadcast()
}

type codexStoreDirectory string

const (
	codexSessionsDirectory codexStoreDirectory = "sessions"
	codexArchivedDirectory codexStoreDirectory = "archived_sessions"
)

// importNativeCandidate retains scheduling identity without granting admission.
func importNativeCandidate(source agentapi.DiscoveryCandidate, t *transcript) Candidate {
	home, key := "", source.Source.Key
	var child, related bool
	var parent, root string
	if id := source.Header.CodexIdentity; id != nil {
		child, related = id.Child, id.ForkID != "" || id.HistoryBase != nil || id.RolloutID != id.ThreadID
		parent, root = id.ParentID, id.RootID
		home, key = source.Root, id.ThreadID
		if base := codexStoreDirectory(filepath.Base(home)); base == codexSessionsDirectory || base == codexArchivedDirectory {
			home = filepath.Dir(home)
		}
	}
	return Candidate{Harness: string(source.Session.Agent), TranscriptPath: t.path, SourceKind: source.Source.Kind, SourceKey: key, Bytes: t.size, NativeSessionID: t.nativeID, NativeChild: child, RelatedHistory: related, ParentNativeID: parent, RootNativeID: root, NativeHome: home}
}
