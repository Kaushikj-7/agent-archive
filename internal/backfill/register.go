package backfill

import (
	"context"
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"github.com/wangjohn/agent-archive/internal/state"
)

// ErrStopped is returned when Stop asked registration to end early.
var ErrStopped = errors.New("registration stopped")

// Hooks wait at most one second for hooks.lock and then drop their event, so
// registration holds it only briefly: at most maxHoldSteps steps or
// maxHold, whichever comes first, then leaves it free for holdGap, longer
// than a waiting hook's 10 ms retry interval, so a waiting hook gets it.
const (
	maxHoldSteps  = 50
	maxHold       = 100 * time.Millisecond
	holdGap       = 20 * time.Millisecond
	hooksLockWait = 10 * time.Second
)

// Registration is step 5 of an import: it registers the confirmed plan's
// sessions, in short holds of hooks.lock. The caller holds setup.lock and
// collector.lock throughout, so no collector pass runs between a session's
// subagent candidates and its registration. Holding collector.lock also
// keeps `pause` out (it takes that lock), so collection cannot be paused
// while registration runs; each hold still rereads the configuration.
type Registration struct {
	// HistoricalSourcePolicy binds reviewed source-location restrictions to current config.
	HistoricalSourcePolicy func(config.Config) func(string) bool
	// Context bounds outside-lock repository validation; nil gets 30 seconds per slice.
	Context context.Context
	Sources agentapi.SourcesLookup
	// DurableSources is mandatory for production confirmed imports.
	DurableSources       bool
	CodexRollouts        agentapi.CodexRolloutLookup
	ConfiguredCodexHomes []string
	// AfterStage observes a durable preparation boundary before admission.
	AfterStage func(string) error
	// OnCapacity may publish a completed group outside hooks.lock before retry.
	OnCapacity func(context.Context) error
	Home       string
	Store      *state.Store
	Batch      string
	AdmittedAt time.Time
	// DestinationID is the destination the import was confirmed for, the
	// batch's. Each registration records it, so a session is not admitted if
	// the configured destination is somehow a different one by then.
	DestinationID string
	// MaxHoldSteps overrides maxHoldSteps; tests lower it to force several
	// holds.
	MaxHoldSteps int
	// AfterHold runs after each hold of hooks.lock is released, with the
	// sessions and subagents registered during it. The CLI appends them to
	// the batch file. An error stops registration.
	AfterHold func(sessions, subagents []string) error
	// Stop, when set, is checked between holds; once it reports true, Run
	// returns ErrStopped. Ctrl-C ends registration this way.
	Stop func() bool
	// CursorDatabase is Cursor's state.vscdb, which a chat found only there
	// is checked against before it is registered. Empty skips the check.
	CursorDatabase string
	// RepoKey, when set, returns archive.RepoKey of the git repository at a
	// project root, or "" when it has none or the root is gone. Registration
	// asks once per root, before a hold, never under hooks.lock, and records
	// the answer on each imported session's registration.
	RepoKey func(root string) string
}

// RegistrationResult counts what registration did with the plan's sessions.
type RegistrationResult struct {
	// Sessions and Subagents are the archive session IDs registered, and
	// given to subagent candidates.
	Sessions  []string
	Subagents []string
	// AlreadyArchived counts sessions a hook, or another run, registered
	// after the plan was made.
	AlreadyArchived int
	// Gone counts sessions whose transcript disappeared after the plan.
	Gone int
	// NotAdmitted counts sessions the configuration stopped accepting after
	// the plan, for example because their project was excluded.
	NotAdmitted int
	// StartInFuture counts sessions that start after the admission, so no
	// registration ever starts after it was admitted.
	StartInFuture int
	// Invalid counts sessions whose registration would not be valid, and
	// SubagentsInvalid subagents whose candidate conflicts with an earlier
	// one or is incomplete. Neither stops the import.
	Invalid          int
	SubagentsInvalid int
}

// parentWork is one imported session in progress. Its subagent candidates
// are written before the session is registered, so a crash at any point
// leaves the session unregistered, and a rerun plans and registers it again;
// the candidates it left are reused, or discarded by the collector.
type parentWork struct {
	c        Candidate
	id       string
	next     int
	children []string
	links    []archive.SupplementalEvidence
	// chatChecked and chatGone are the result of checking, before the hold,
	// whether a chat found only in Cursor's database is still there (see
	// checkChats).
	chatChecked bool
	chatGone    bool
	// repoKey is the project's repository key, and repoKeyChecked whether it
	// was asked for (see resolveRepoKeys).
	repoKey              string
	repoKeyChecked       bool
	projectEvidenceStale bool
	stagedSHA256         string
	stagedChildren       map[string]state.SubagentCandidate
	stageObsolete        bool
}

// Run registers every candidate, in order.
func (r Registration) Run(candidates []Candidate) (RegistrationResult, error) {
	var result RegistrationResult
	ctx := r.Context
	if ctx == nil {
		ctx = context.Background()
	}
	works := make([]*parentWork, len(candidates))
	for i, c := range candidates {
		works[i] = &parentWork{c: c}
	}
	flushed := [2]int{}
	repoKeys := map[string]string{}
	flush := func() error {
		sessions, subagents := result.Sessions[flushed[0]:], result.Subagents[flushed[1]:]
		flushed = [2]int{len(result.Sessions), len(result.Subagents)}
		if r.AfterHold == nil || (len(sessions) == 0 && len(subagents) == 0) {
			return nil
		}
		return r.AfterHold(sessions, subagents)
	}
	for i := 0; i < len(works); {
		if ctx.Err() != nil || (r.Stop != nil && r.Stop()) {
			return result, errors.Join(ErrStopped, r.cancelPrepared(works))
		}
		if err := r.checkChats(works[i:]); err != nil {
			return result, err
		}
		r.resolveRepoKeys(works[i:], repoKeys)
		// Filesystem/Git proof validation belongs before the short lock hold.
		limit := maxHoldSteps
		if r.MaxHoldSteps > 0 {
			limit = r.MaxHoldSteps
		}
		validationCtx := ctx
		cancelValidation := func() {}
		if r.Context == nil {
			validationCtx, cancelValidation = context.WithTimeout(ctx, 30*time.Second)
		}
		for _, w := range works[i : i+min(limit, len(works)-i)] {
			if w.c.projectResolutionReset != nil {
				w.c.projectResolutionReset(validationCtx)
			}
		}
		for _, w := range works[i : i+min(limit, len(works)-i)] {
			if w.c.projectResolutionCurrent != nil {
				w.projectEvidenceStale = !w.c.projectResolutionCurrent()
			}
		}
		cancelValidation()
		if err := r.stageGroup(ctx, works[i:i+min(limit, len(works)-i)]); err != nil {
			if ctx.Err() != nil || (r.Stop != nil && r.Stop()) {
				return result, errors.Join(ErrStopped, err, r.cancelPrepared(works))
			}
			return result, err
		}
		if ctx.Err() != nil || (r.Stop != nil && r.Stop()) {
			return result, errors.Join(ErrStopped, r.cancelPrepared(works))
		}
		groupStart := i
		err := r.hold(works, &i, &result)
		if cleanupErr := r.cleanObsoleteStages(ctx, works[groupStart:groupStart+min(limit, len(works)-groupStart)]); err == nil {
			err = cleanupErr
		}
		if flushErr := flush(); err == nil {
			err = flushErr
		}
		if err != nil {
			return result, err
		}
		if i < len(works) {
			time.Sleep(holdGap)
		}
	}
	return result, nil
}

// checkChats checks, before hooks.lock is taken, whether each Cursor
// database chat the next hold can reach is still in the database. Reading
// Cursor's database can wait on Cursor's own lock, and a hook waits at most
// a second for hooks.lock before dropping its event, so the read is kept out
// of the hold. The answer can be a hold old by the time it is used; it only
// ever was a check against the plan, and the collector handles a chat
// deleted later.
func (r Registration) checkChats(works []*parentWork) error {
	limit := maxHoldSteps
	if r.MaxHoldSteps > 0 {
		limit = r.MaxHoldSteps
	}
	// A hold finishes at most one session per step.
	for _, w := range works[:min(limit, len(works))] {
		if w.c.SourceKind == archive.SourceKindCursorSQLite && !w.chatChecked {
			gone, err := r.chatGone(w.c)
			if err != nil {
				return err
			}
			w.chatChecked, w.chatGone = true, gone
		}
	}
	return nil
}

// resolveRepoKeys asks, before hooks.lock is taken, for the repository key of
// each project the next hold can reach, at most once per project root
// (repoKeys remembers the answers across holds). Asking runs git, which a
// hold must not wait for. A session the configuration does not admit (an
// excluded project, one not yet activated) is not asked about at all; it will
// not be registered.
func (r Registration) resolveRepoKeys(works []*parentWork, repoKeys map[string]string) {
	if r.RepoKey == nil {
		return
	}
	cfg, found, err := config.Load(r.Home)
	if err != nil || !found {
		// The hold reports it; nothing is asked about meanwhile.
		return
	}
	limit := maxHoldSteps
	if r.MaxHoldSteps > 0 {
		limit = r.MaxHoldSteps
	}
	for _, w := range works[:min(limit, len(works))] {
		if w.c.HistoricalProject != nil {
			w.repoKeyChecked = true
			continue
		}
		if w.repoKeyChecked {
			continue
		}
		if !cfg.AcceptSession(r.registration(w.c, "", "")) {
			w.repoKeyChecked = true
			continue
		}
		key, asked := repoKeys[w.c.ProjectRoot]
		if !asked {
			if key = r.RepoKey(w.c.ProjectRoot); !archive.IsRepoKey(key) {
				key = ""
			}
			repoKeys[w.c.ProjectRoot] = key
		}
		w.repoKey, w.repoKeyChecked = key, true
	}
}

// hold takes hooks.lock, rereads the configuration, and works through the
// candidates from *i until the hold's budget is spent.
func (r Registration) hold(works []*parentWork, i *int, result *RegistrationResult) error {
	unlock, err := local.NamedLockWait(r.Home, "hooks.lock", hooksLockWait)
	if errors.Is(err, local.ErrBusy) {
		return fmt.Errorf("capture hooks held hooks.lock for %s", hooksLockWait)
	}
	if err != nil {
		return fmt.Errorf("lock capture hooks: %w", err)
	}
	defer unlock()
	cfg, found, err := config.Load(r.Home)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if !found {
		return errors.New("the configuration disappeared during the import")
	}
	limit := maxHoldSteps
	if r.MaxHoldSteps > 0 {
		limit = r.MaxHoldSteps
	}
	start := time.Now()
	for steps := 0; *i < len(works) && steps < limit && time.Since(start) < maxHold; steps++ {
		done, err := r.step(cfg, works[*i], result)
		if err != nil {
			return err
		}
		if done {
			*i++
		}
	}
	return nil
}

// step does one small piece of a session: check it and find its archive ID,
// write one subagent candidate, or register it. done is true when the
// session is finished, registered or skipped.
func (r Registration) step(cfg config.Config, w *parentWork, result *RegistrationResult) (done bool, err error) {
	c := w.c
	if w.id == "" {
		skip, err := r.skip(cfg, w, result)
		if err != nil || skip {
			return true, err
		}
		if !r.valid(c) {
			result.Invalid++
			return true, nil
		}
		if (c.NativeChild || c.CodexBinding != nil) && !cfg.CodexHistoryProtection {
			cfg.CodexHistoryProtection = true
			if err := config.Save(r.Home, cfg); err != nil {
				return true, err
			}
		}
		w.id, _, err = r.Store.EnsureArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness(c.Harness)), NativeID: c.NativeSessionID})
		return false, err
	}
	if r.DurableSources && w.stagedSHA256 == "" {
		// Reservation can outlive its project proof or authorization. Finish
		// rejected work even when it no longer qualifies for staging.
		return r.skip(cfg, w, result)
	}
	if w.next < len(c.Subagents) {
		sub := c.Subagents[w.next]
		w.next++
		err := r.subagent(w, sub)
		if errors.Is(err, state.ErrSubagentCandidateConflict) || errors.Is(err, state.ErrSubagentCandidateIncomplete) {
			result.SubagentsInvalid++
			err = nil
		}
		return false, err
	}
	// The configuration may have changed since the session was checked, in
	// an earlier hold.
	skip, err := r.skip(cfg, w, result)
	if err != nil || skip {
		return true, err
	}
	reg, err := r.Store.RegisterReservedSession(agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness(c.Harness)), NativeID: c.NativeSessionID}, w.id, func(id string) archive.SessionRegistration {
		reg := r.registration(c, id, w.repoKey)
		reg.StagedImportSHA256 = w.stagedSHA256
		return reg
	})
	if err != nil {
		return true, fmt.Errorf("register an imported session: %w", err)
	}
	if err := r.Store.SaveRequest(reg.ArchiveSessionID, "backfill", r.AdmittedAt, w.links...); err != nil {
		return true, fmt.Errorf("queue an imported session: %w", err)
	}
	if reg.NativeChild {
		result.Subagents = append(result.Subagents, reg.ArchiveSessionID)
	} else {
		result.Sessions = append(result.Sessions, reg.ArchiveSessionID)
	}
	result.Subagents = append(result.Subagents, w.children...)
	return true, nil
}

// skip reports whether a session is no longer imported, counting why: the
// configuration no longer accepts it, its transcript is gone, a hook or
// another run registered it since the plan was made, or it starts after the
// admission. CheckClock already refuses an admission before the plan, so the
// last is a backstop: no registration ever starts after its admission.
func (r Registration) skip(cfg config.Config, w *parentWork, result *RegistrationResult) (bool, error) {
	c := w.c
	if w.projectEvidenceStale {
		result.NotAdmitted++
		return true, nil
	}
	if c.ProjectResolution != nil && c.ProjectResolution.PolicyContext != sourcefacts.RecoveryContext(cfg.Archive.Projects, nil, filepath.Clean) {
		result.NotAdmitted++
		return true, nil
	}
	if !cfg.AcceptSession(r.registration(c, "", "")) {
		result.NotAdmitted++
		return true, nil
	}
	if c.SourceKind == archive.SourceKindCursorSQLite {
		// Checked before the hold (checkChats), never under hooks.lock.
		if w.stagedSHA256 == "" && w.chatGone {
			result.Gone++
			return true, nil
		}
	} else if w.stagedSHA256 == "" && !regularFile(c.TranscriptPath) {
		result.Gone++
		return true, nil
	}
	id, found, err := r.Store.ArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness(c.Harness)), NativeID: c.NativeSessionID})
	if err != nil {
		return false, err
	}
	if found {
		_, registered, err := r.Store.LoadRegistration(id)
		if err != nil {
			return false, err
		}
		if registered {
			result.AlreadyArchived++
			if w.stagedSHA256 != "" {
				w.stageObsolete = true
			}
			return true, nil
		}
	}
	if c.StartedAt.After(r.AdmittedAt) {
		result.StartInFuture++
		return true, nil
	}
	return false, nil
}

// subagent writes one subagent candidate, as a SubagentStop hook does, and
// keeps the link evidence for the parent's request. The collector validates
// and registers the child.
func (r Registration) subagent(w *parentWork, sub Subagent) error {
	if !r.DurableSources && !regularFile(sub.Path) {
		return nil
	}
	c := w.c
	childNativeID := c.NativeSessionID + ":subagent:" + sub.AgentID
	childID, _, err := r.Store.EnsureArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness(c.Harness)), NativeID: childNativeID})
	if err != nil {
		return fmt.Errorf("assign a subagent archive session ID: %w", err)
	}
	if _, registered, err := r.Store.LoadRegistration(childID); err != nil || registered {
		// A hook registered this subagent already.
		return err
	}
	candidate := state.SubagentCandidate{HistoricalProject: c.HistoricalProject,
		ArchiveSessionID: childID, NativeSessionID: childNativeID, ParentArchiveSessionID: w.id, ParentNativeSessionID: c.NativeSessionID,
		ProjectID: c.ProjectIdentity(), ProjectRoot: c.ProjectRoot, Harness: archive.Harness{Name: c.Harness}, AgentID: sub.AgentID, TranscriptPath: sub.Path, ObservedAt: r.AdmittedAt, Origin: archive.SessionOriginImport,
	}
	if r.DurableSources {
		staged, ok := w.stagedChildren[sub.AgentID]
		if !ok {
			return errors.New("missing staged imported child")
		}
		candidate = staged
	}
	if err := r.Store.SaveSubagentCandidate(candidate); err != nil {
		return fmt.Errorf("record a subagent: %w", err)
	}
	link, err := archive.NewLinkedSessionEvidence(childID, archive.LinkedSessionPending, r.AdmittedAt)
	if err != nil {
		return fmt.Errorf("filter subagent link: %w", err)
	}
	w.links = append(w.links, link)
	w.children = append(w.children, childID)
	return nil
}

// valid reports whether c's registration would be valid. An invalid one is
// counted, not returned as an error: it does not stop the import.
func (r Registration) valid(c Candidate) bool {
	return r.registration(c, "check", "").Validate() == nil
}

// registration is the imported session's registration: its true start, the
// import's admission, and the batch.
func (r Registration) registration(c Candidate, archiveID, repoKey string) archive.SessionRegistration {
	if repoKey == "" && c.ProjectResolution != nil && archive.IsRepoKey(c.ProjectResolution.RecordedRepoKey) {
		repoKey = c.ProjectResolution.RecordedRepoKey
	}
	return archive.SessionRegistration{
		HistoricalProject: c.HistoricalProject, CodexBinding: c.CodexBinding, ProjectResolution: c.ProjectResolution, NativeChild: c.NativeChild, NativeRootSessionID: c.RootNativeID, ParentNativeSessionID: c.ParentNativeID, NativeSourceHome: c.NativeHome,
		ArchiveSessionID: archiveID,
		NativeSessionID:  c.NativeSessionID,
		ProjectID:        c.ProjectIdentity(),
		ProjectRoot:      c.ProjectRoot,
		RepoKey:          repoKey,
		Harness:          archive.Harness{Name: c.Harness},
		TranscriptPath:   c.TranscriptPath,
		SourceKind:       c.SourceKind,
		SourceKey:        c.SourceKey,
		SessionStartedAt: c.StartedAt,
		StartedAtSource:  c.StartedAtSource,
		RegisteredAt:     r.AdmittedAt,
		AdmittedAt:       r.AdmittedAt,
		Origin:           archive.SessionOriginImport,
		ImportBatch:      archive.NewImportBatch(r.Batch),
		DestinationID:    r.DestinationID,
	}
}

// chatGone reports whether a chat found only in Cursor's database is no
// longer there. It reads a few indexed rows in place, never a copy. A
// database that can't be read now (Cursor holds a lock) is not "gone": the
// chat is registered, and the collector reads it when it can.
func (r Registration) chatGone(c Candidate) (bool, error) {
	if r.CursorDatabase == "" {
		return false, nil
	}
	_, err := observeSource(context.Background(), r.Sources, agentapi.SourceEnvironment{Database: r.CursorDatabase}, c.Harness, agentapi.SourceRef{Kind: c.SourceKind, Path: c.TranscriptPath, Key: c.SourceKey})
	if fatalSourceFailure(err) {
		return false, err
	}
	return isNotExist(err), nil
}

// observeSource asks the selected provider and closes its serial owner before
// registration takes hooks.lock. Cleanup failures remain observable.
func observeSource(ctx context.Context, sources agentapi.SourcesLookup, environment agentapi.SourceEnvironment, name string, ref agentapi.SourceRef) (observation agentapi.SourceObservation, err error) {
	if sources == nil {
		return observation, errors.New("source lookup required")
	}
	provider, _, ok := sources.LookupSources(name)
	if !ok {
		return observation, errors.New("source capability unavailable")
	}
	pass, err := provider.OpenPass(ctx, environment)
	if err != nil {
		return observation, err
	}
	defer func() { err = errors.Join(err, pass.Close()) }()
	return pass.Signature(ctx, ref)
}

// regularFile reports whether path is still a regular file, without
// following a symlink.
func regularFile(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}

// stage reads selected native evidence outside hooks.lock and freezes it before
// the atomic registration can reference it. Capacity or filtering failure leaves
// the reservation unadmitted and safe to retry.
func (r Registration) stage(ctx context.Context, w *parentWork) error {
	reg := r.registration(w.c, w.id, w.repoKey)
	if existing, found, err := r.Store.LoadImportStage(ctx, reg); err != nil {
		return err
	} else if found {
		w.stagedSHA256 = existing.SHA256
		w.c.CodexBinding = existing.Registration.CodexBinding
		w.c.TranscriptPath = existing.Registration.TranscriptPath
		w.stagedChildren = map[string]state.SubagentCandidate{}
		for _, child := range existing.Children {
			w.stagedChildren[child.AgentID] = child
		}
		return nil
	}
	cfg, found, err := config.Load(r.Home)
	if err != nil || !found {
		return errors.Join(errors.New("load import policy"), err)
	}
	if !cfg.AcceptSession(reg) {
		w.projectEvidenceStale = true
		return nil
	}
	var sourceAllowed func(string) bool
	if reg.HistoricalProject != nil {
		if r.HistoricalSourcePolicy == nil {
			return errors.New("historical import requires reviewed original source restrictions")
		}
		sourceAllowed = r.HistoricalSourcePolicy(cfg)
	}
	bundle, preserved, selectedBinding, err := collector.ReadImportSourceSelection(ctx, reg, r.AdmittedAt, collector.Options{HistoricalSourceAllowed: func(cwd string) bool {
		canonical := local.CanonicalPath(cwd)
		if sourceAllowed != nil && !sourceAllowed(cwd) {
			return false
		}
		if reg.HistoricalProject != nil {
			for _, path := range uniquePaths(filepath.Clean(cwd), canonical) {
				if !slices.Contains(reg.HistoricalProject.SourceCwds, path) {
					reg.HistoricalProject.SourceCwds = append(reg.HistoricalProject.SourceCwds, path)
				}
			}
			if len(reg.HistoricalProject.SourceCwds) > 2*archive.MaxHistorySpans {
				return false
			}
		}
		return true
	}, Sources: r.Sources, CursorDatabase: r.CursorDatabase, CodexRollouts: r.CodexRollouts, ConfiguredCodexHomes: r.ConfiguredCodexHomes})
	if err != nil {
		return fmt.Errorf("stage imported evidence before admission: %w", err)
	}
	if selectedBinding != nil {
		reg.CodexBinding = selectedBinding
		reg.TranscriptPath = selectedBinding.Path
		w.c.CodexBinding = selectedBinding
		w.c.TranscriptPath = selectedBinding.Path
	}
	children := []state.SubagentCandidate{}
	w.stagedChildren = map[string]state.SubagentCandidate{}
	for _, sub := range w.c.Subagents {
		childNative := w.c.NativeSessionID + ":subagent:" + sub.AgentID
		childID, _, err := r.Store.EnsureArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness(w.c.Harness)), NativeID: childNative})
		if err != nil {
			return err
		}
		candidate := state.SubagentCandidate{HistoricalProject: reg.HistoricalProject, ArchiveSessionID: childID, NativeSessionID: childNative, ParentArchiveSessionID: w.id, ParentNativeSessionID: w.c.NativeSessionID, ProjectID: reg.ProjectID, ProjectRoot: reg.ProjectRoot, Harness: reg.Harness, AgentID: sub.AgentID, TranscriptPath: sub.Path, ObservedAt: r.AdmittedAt, Origin: archive.SessionOriginImport}
		childDigest, err := r.stageSelectedChild(ctx, reg, candidate, cfg.EffectiveSkillEvidence())
		if err != nil {
			return err
		}
		candidate.StagedImportSHA256 = childDigest
		w.stagedChildren[sub.AgentID] = candidate
		children = append(children, candidate)
	}
	digest, err := r.Store.SaveImportSourceSet(ctx, reg, bundle, preserved, cfg.EffectiveSkillEvidence(), children...)
	if err != nil {
		return fmt.Errorf("persist imported evidence before admission: %w", err)
	}
	w.stagedSHA256 = digest
	if r.AfterStage != nil {
		return r.AfterStage(w.id)
	}
	return nil
}

func (r Registration) stageSelectedChild(ctx context.Context, parent archive.SessionRegistration, candidate state.SubagentCandidate, mode config.SkillEvidence) (string, error) {
	owner := parent
	owner.ArchiveSessionID, owner.NativeSessionID = candidate.ArchiveSessionID, candidate.NativeSessionID
	owner.ParentSessionID = parent.ArchiveSessionID
	if stage, found, err := r.Store.LoadImportStage(ctx, owner); err != nil {
		return "", err
	} else if found {
		return stage.SHA256, nil
	}
	reg, bundle, err := collector.ReadImportSubagentBundle(ctx, parent, candidate, collector.Options{Sources: r.Sources, CursorDatabase: r.CursorDatabase})
	if err != nil {
		return "", fmt.Errorf("stage imported child: %w", err)
	}
	return r.Store.SaveImportStage(ctx, reg, bundle, mode)
}

func (r Registration) cancelPrepared(works []*parentWork) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, w := range works {
		if w.stagedSHA256 == "" {
			if err := r.cancelUnlinkedChildren(ctx, w); err != nil {
				return err
			}
			continue
		}
		reg := r.registration(w.c, w.id, w.repoKey)
		reg.StagedImportSHA256 = w.stagedSHA256
		if err := r.Store.CancelImportStage(ctx, reg); err != nil {
			return err
		}
	}
	return nil
}

// A stop can interrupt a group's preparation before its parent journal exists.
// Each completed child still has its own cancellation journal and exact owner.
func (r Registration) cancelUnlinkedChildren(ctx context.Context, w *parentWork) error {
	for _, child := range w.stagedChildren {
		reg := r.registration(w.c, child.ArchiveSessionID, w.repoKey)
		reg.NativeSessionID = child.NativeSessionID
		reg.ParentSessionID = w.id
		reg.StagedImportSHA256 = child.StagedImportSHA256
		if err := r.Store.CancelImportStage(ctx, reg); err != nil {
			return err
		}
	}
	return nil
}

func (r Registration) stageGroup(ctx context.Context, works []*parentWork) error {
	if r.DurableSources {
		for _, w := range works {
			if w.id != "" && w.stagedSHA256 == "" && !w.projectEvidenceStale {
				if err := r.stage(ctx, w); err != nil {
					if !errors.Is(err, state.ErrDurableStorageCapacity) || r.OnCapacity == nil {
						return err
					}
					if recoveryErr := r.OnCapacity(ctx); recoveryErr != nil {
						return errors.Join(err, recoveryErr)
					}
					if err := r.stage(ctx, w); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func (r Registration) cleanObsoleteStages(ctx context.Context, works []*parentWork) (err error) {
	for _, w := range works {
		if w.stageObsolete {
			reg := r.registration(w.c, w.id, w.repoKey)
			reg.StagedImportSHA256 = w.stagedSHA256
			stage, found, e := r.Store.LoadImportStage(ctx, reg)
			if e == nil && found {
				e = r.Store.DropUnadmittedImportStage(ctx, stage)
			}
			if err == nil {
				err = e
			}
			w.stageObsolete = false
		}
	}
	return err
}
