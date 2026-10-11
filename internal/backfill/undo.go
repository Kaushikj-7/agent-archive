package backfill

import (
	"context"
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"io"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/retention"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// UndoPlan is what `backfill undo` does to one import. Its sessions are the
// registrations that carry the import's ID, which are the source of truth:
// the batch file's lists can miss sessions a crash registered, and name ones
// that were never registered.
type UndoPlan struct {
	Batch Batch
	// Project is the --project directory, resolved as the import resolved
	// project roots, or "" for the whole import.
	Project string
	// Sessions are removed in this order: subagents before their parents.
	Sessions []UndoSession
	// ExcludeProjects are the projects the import added (or took over, see
	// TakenOver) that are still included and no other import still has
	// sessions in; undo marks them Included: false and keeps their entries.
	ExcludeProjects []archive.ProjectActivation
	// KeepProjects are projects the import added (or took over) that stay
	// included because another import still has sessions there: excluding
	// them would stop those sessions updating without the other import
	// being undone. The batch records them (ProjectsKept) and the imports
	// they are kept for (ProjectsKeptFor); the undo of the last of those
	// with sessions there excludes them.
	KeepProjects []KeptProject
	// RemoveApps are apps the import added to ImportedHarnesses that no
	// imported session left after the undo needs.
	RemoveApps []string
	// TakenOver names, for each project in ExcludeProjects or KeepProjects
	// that the import did not add, the earlier imports whose undo kept it
	// included for this import's sessions. The plan lists them apart.
	TakenOver map[string][]string
	// RemoveKeptOut are excluded projects an import added only to keep
	// folders inside a plain folder it added out of capture (ProjectsKeptOut)
	// that nothing included contains once this undo's exclusions are made.
	// Removing them changes no capture: nothing configured and included is
	// above them. Undo removes them from the configuration.
	RemoveKeptOut []archive.ProjectActivation
	// RestoreRetention is set when the import raised the archive-wide
	// retention and it is still what the import set: undo puts the earlier
	// value back. A --project undo leaves retention alone.
	RestoreRetention *RetentionChange
	// RetentionDeletes counts the sessions, in the current destination and
	// not removed by this undo, that the restored (shorter) retention makes
	// old enough to delete: the next collector pass deletes them.
	RetentionDeletes int
	// RetentionChangedSince is set when the import raised retention and it
	// was changed again afterwards, so undo leaves it as it is.
	RetentionChangedSince bool
	// RetentionKept is set by WithRetentionKept: retention is still what the
	// import set and could go back (RetentionDeletes counts what that would
	// delete), but this undo leaves it. A later undo of the same import can
	// still restore it.
	RetentionKept *RetentionChange
	// Settled is what an earlier run of this undo changed in the
	// configuration but stopped before recording in the batch: projects it
	// excluded, and the retention it put back. Nothing is left to change
	// for them; the caller records them in the batch (Batch.RecordUndone),
	// so a later undo never excludes such a project again once setup
	// includes it, nor restores retention twice.
	Settled UndoChanges
	// HookCapturedStopping counts the hook-captured sessions in
	// ExcludeProjects. Excluding the projects stops them uploading; they are
	// not deleted.
	HookCapturedStopping int

	view Plan
}

// KeptProject is a project undo leaves included, and why: the other
// imports that still have sessions there, and how many.
type KeptProject struct {
	Project  archive.ProjectActivation
	Imports  []string
	Sessions int
}

// SharedBatchIDError means sessions registered outside an import carry its
// ID: an earlier import had the same ID and its batch file is gone. Undo
// selects sessions by that ID and cannot tell the two imports apart, so it
// refuses rather than remove both.
type SharedBatchIDError struct {
	Batch    string
	Sessions int
}

func (e *SharedBatchIDError) Error() string {
	return fmt.Sprintf("import %s shares its ID with an earlier import whose file is no longer in imports/: %s carrying the ID %s registered outside import %s's own run. Undo cannot tell the two imports apart, so it removes neither. Nothing was changed",
		e.Batch, CountNoun(e.Sessions, "session"), wasWere(e.Sessions), e.Batch)
}

// UndoSession is one registered session of the import.
type UndoSession struct {
	Registration archive.SessionRegistration
	// InCurrentDestination is set when the session's objects are in the
	// bucket configured now. Otherwise they are in a destination this machine
	// no longer uses, and the session is only forgotten locally.
	InCurrentDestination bool
	// Resumed is set when the app ran the session again after the import
	// (see resumedSinceImport), so it has content newer than the import.
	Resumed bool
	// ResumeUnknown is set for a Cursor database chat whose database could
	// not be read (Cursor held it locked), so whether it was resumed is not
	// known; nothing else showed that it was.
	ResumeUnknown bool
}

// PlanUndo decides what undoing b does. project limits it to one project
// directory, resolved as BuildPlan resolves --project. batches are every
// import, b among them, which decide the projects b's undo takes over from
// earlier undos (see undoProjects). It writes nothing.
//
// It refuses (SharedBatchIDError) when a session carrying b's ID was
// admitted before b started or after it completed: it belongs to an earlier
// import that had the same ID.
func PlanUndo(env Environment, store *state.Store, cfg config.Config, batches []Batch, b Batch, project string) (UndoPlan, error) {
	// Sessions are selected by the ID they carry (InBatch), so a batch
	// without a valid one selects nothing it could prove is its own.
	if !ValidBatchID(b.ID) {
		return UndoPlan{}, fmt.Errorf("import %q has no valid import ID; nothing was changed", b.ID)
	}
	if project != "" {
		if abs, err := filepath.Abs(project); err == nil {
			project = abs
		}
		project = env.resolved(project)
	}
	p := UndoPlan{
		Batch:   b,
		Project: project,
		view: Plan{
			GeneratedAt:   env.now(),
			Home:          env.Home,
			RetentionDays: cfg.RetentionDays,
			resolvedHome:  env.resolved(env.Home),
			Destination: Destination{
				Provider: cfg.Storage.Provider,
				Bucket:   cfg.Storage.Bucket,
				Prefix:   cfg.Storage.Prefix,
			},
		},
	}
	inProject := func(root string) bool {
		return p.Project == "" || root == p.Project || env.resolved(root) == p.Project
	}

	regs, err := store.LoadRegistrations()
	if err != nil {
		return UndoPlan{}, err
	}
	regs, err = importUndoRegistrations(store, regs, b.ID)
	if err != nil {
		return UndoPlan{}, err
	}
	if n := sessionsOutsideBatch(regs, b); n > 0 {
		return UndoPlan{}, &SharedBatchIDError{Batch: b.ID, Sessions: n}
	}
	requests, err := store.LoadRequests()
	if err != nil {
		return UndoPlan{}, err
	}
	requested := map[string]state.Request{}
	for _, req := range requests {
		requested[req.ArchiveSessionID] = req
	}
	matchedRoots := map[string]bool{}
	for _, reg := range regs {
		if !reg.InBatch(b.ID) {
			continue
		}
		if _, seen := matchedRoots[reg.ProjectRoot]; !seen {
			matchedRoots[reg.ProjectRoot] = inProject(reg.ProjectRoot)
		}
	}
	selected := selectUndoRegistrations(regs, b.ID, matchedRoots)
	p.Sessions, err = loadUndoSessions(env, store, cfg, b, selected, requested)
	if err != nil {
		return UndoPlan{}, err
	}

	p.ExcludeProjects, p.KeepProjects, p.TakenOver, p.Settled.Excluded = undoProjects(cfg, regs, batches, b, inProject)
	p.RemoveKeptOut = keptOutToRemove(env, cfg, regs, batches, b, p.Sessions, p.ExcludeProjects)
	excludedRoots := map[string]bool{}
	for _, project := range p.ExcludeProjects {
		excludedRoots[project.Root] = true
	}
	// Excluding a project also stops its hook-captured sessions uploading.
	for _, reg := range regs {
		if !reg.Imported() && reg.ParentSessionID == "" && excludedRoots[reg.ProjectRoot] {
			p.HookCapturedStopping++
		}
	}

	if r := b.Retention; r != nil && !r.Restored && p.Project == "" {
		switch {
		case cfg.RetentionDays == r.To:
			restore := *r
			p.RestoreRetention = &restore
			if p.RetentionDeletes, err = retentionDeletes(store, cfg, regs, p.Sessions, restore, env.now()); err != nil {
				return UndoPlan{}, err
			}
		case b.UndoneAt != nil && cfg.RetentionDays == r.From:
			// An earlier run of this undo put it back and stopped before
			// recording so.
			p.Settled.RetentionRestored = true
		default:
			p.RetentionChangedSince = true
		}
	}

	// An app the import added stays while any imported session of it is
	// left: another import's, or this one's outside --project.
	p.RemoveApps = undoAppsToRemove(cfg, b, regs, p.Sessions)
	return p, nil
}

// selectUndoRegistrations decides which registrations an undo may remove.
// A subagent follows its selected parent; an orphan is selected by its own
// project so an interrupted earlier undo can finish removing it.
func selectUndoRegistrations(regs []archive.SessionRegistration, batchID string, matchedRoots map[string]bool) []archive.SessionRegistration {
	parents := map[string]bool{}
	for _, reg := range regs {
		if reg.InBatch(batchID) && reg.ParentSessionID == "" && matchedRoots[reg.ProjectRoot] {
			parents[reg.ArchiveSessionID] = true
		}
	}
	var selected []archive.SessionRegistration
	for _, reg := range regs {
		if !reg.InBatch(batchID) {
			continue
		}
		if reg.ParentSessionID != "" {
			if !parents[reg.ParentSessionID] && !matchedRoots[reg.ProjectRoot] {
				continue
			}
		} else if !parents[reg.ArchiveSessionID] {
			continue
		}
		selected = append(selected, reg)
	}
	return selected
}

// loadUndoSessions reads live resume evidence after selection. Removal order
// remains subagents first, then parents, each in registration order.
func loadUndoSessions(env Environment, store *state.Store, cfg config.Config, b Batch, regs []archive.SessionRegistration, requested map[string]state.Request) ([]UndoSession, error) {
	// A registration with a destination ID is judged by that ID alone;
	// legacy ones also need the batch destination to match.
	sameDestination := b.DestinationID == cfg.DestinationID()
	var parents, children []UndoSession
	for _, reg := range regs {
		resumed, unknown, err := resumedSinceImport(env, store, reg, requested[reg.ArchiveSessionID])
		if err != nil {
			return nil, err
		}
		s := UndoSession{Registration: reg, InCurrentDestination: (reg.DestinationID != "" || sameDestination) && cfg.InCurrentDestination(reg), Resumed: resumed, ResumeUnknown: unknown}
		if reg.ParentSessionID != "" {
			children = append(children, s)
		} else {
			parents = append(parents, s)
		}
	}
	return orderUndoChildrenFirst(slices.Concat(children, parents))
}

// undoAppsToRemove is the configuration decision after session selection.
// An app stays while any imported session outside this undo still needs it.
func undoAppsToRemove(cfg config.Config, b Batch, regs []archive.SessionRegistration, sessions []UndoSession) []string {
	undone := map[string]bool{}
	for _, s := range sessions {
		undone[s.Registration.ArchiveSessionID] = true
	}
	var remove []string
	for _, app := range b.AppsAdded {
		if !slices.Contains(cfg.ImportedHarnesses, app) {
			continue
		}
		needed := slices.ContainsFunc(regs, func(reg archive.SessionRegistration) bool {
			return reg.Imported() && !undone[reg.ArchiveSessionID] && archive.CanonicalHarness(reg.Harness.Name) == app
		})
		if !needed {
			remove = append(remove, app)
		}
	}
	return remove
}

// keptOutToRemove returns the excluded projects b, or an import undone
// already, added to keep a folder out of capture (ProjectsKeptOut) that are
// no longer needed once exclude is excluded: recorded by no import still in
// place, still excluded (setup did not include them), no
// session registered in them, and no included project, other than those
// being excluded, containing them. For such a folder the nearest configured
// project, with or without the entry, is excluded or absent, so removing it
// changes no capture; it only puts the configuration back.
//
// An import is still in place when it was never undone (b aside), or when
// any session of it stays registered once removing are removed: a --project
// undo marks the import undone but leaves its other projects' sessions, and
// setup may include their folders again.
func keptOutToRemove(env Environment, cfg config.Config, regs []archive.SessionRegistration, batches []Batch, b Batch, removing []UndoSession, exclude []archive.ProjectActivation) []archive.ProjectActivation {
	removed := map[string]bool{}
	for _, s := range removing {
		removed[s.Registration.ArchiveSessionID] = true
	}
	inPlace := func(o Batch) bool {
		if o.ID != b.ID && o.UndoneAt == nil {
			return true
		}
		return slices.ContainsFunc(regs, func(reg archive.SessionRegistration) bool {
			return reg.InBatch(o.ID) && !removed[reg.ArchiveSessionID]
		})
	}
	// Only an entry of an import being undone, or undone already, goes: an
	// import still in place keeps its entries even while setup has its
	// folder excluded, so including the folder again keeps them out.
	keptOut, live := map[string]bool{}, map[string]bool{}
	for _, o := range append(slices.Clone(batches), b) {
		for _, id := range o.ProjectsKeptOut {
			if inPlace(o) {
				live[id] = true
			} else {
				keptOut[id] = true
			}
		}
	}
	for id := range live {
		delete(keptOut, id)
	}
	if len(keptOut) == 0 {
		return nil
	}
	excluding := map[string]bool{}
	for _, project := range exclude {
		excluding[project.ProjectID] = true
	}
	var out []archive.ProjectActivation
	for _, g := range cfg.Archive.Projects {
		if !keptOut[g.ProjectID] || g.Included {
			continue
		}
		if slices.ContainsFunc(regs, func(reg archive.SessionRegistration) bool {
			return reg.ProjectID == g.ProjectID || reg.ProjectRoot == g.Root
		}) {
			continue
		}
		resolved := env.resolved(g.Root)
		covered := slices.ContainsFunc(cfg.Archive.Projects, func(q archive.ProjectActivation) bool {
			return q.ProjectID != g.ProjectID && q.Included && !excluding[q.ProjectID] &&
				(local.PathWithin(g.Root, q.Root) || local.PathWithin(resolved, env.resolved(q.Root)))
		})
		if !covered {
			out = append(out, g)
		}
	}
	return out
}

// retentionDeletes counts independent owners in cfg's destination,
// other than those undo removes, that retention of r.From days deletes and
// retention of r.To days does not: those restoring r.From costs. A session
// ages as retention ages it: from its last capture, or its admission when it
// has none (see retention.sweepSession). A capture is never earlier than the
// admission, so only a session admitted over r.From days ago can be one; only
// those have their published state read.
func retentionDeletes(store *state.Store, cfg config.Config, regs []archive.SessionRegistration, removing []UndoSession, r RetentionChange, now time.Time) (int, error) {
	if !config.RetentionShorter(r.From, r.To) {
		return 0, nil
	}
	removed := map[string]bool{}
	for _, s := range removing {
		removed[s.Registration.ArchiveSessionID] = true
	}
	n := 0
	preview, err := retention.NewExpiryPreview(store, now, cfg.AcceptSession)
	if err != nil {
		return 0, err
	}
	for _, reg := range regs {
		if !independentImportOwner(reg) || removed[reg.ArchiveSessionID] || !cfg.InCurrentDestination(reg) {
			continue
		}
		after, err := preview.SessionExpired(reg, config.RetentionDuration(r.From))
		if err != nil {
			return 0, err
		}
		before, err := preview.SessionExpired(reg, config.RetentionDuration(r.To))
		if err != nil {
			return 0, err
		}
		if after && !before {
			n++
		}
	}

	return n, nil
}

// sessionsOutsideBatch counts the sessions (not subagents) that carry b's ID
// but were admitted before b started or after it completed. Every session
// b's own run registers is admitted at or after StartedAt (the admission is
// stamped when the batch is opened, and a continued batch keeps its start)
// and before CompletedAt, so any other one is an earlier import's that had
// the same ID.
func sessionsOutsideBatch(regs []archive.SessionRegistration, b Batch) int {
	n := 0
	for _, reg := range regs {
		if !reg.InBatch(b.ID) || !independentImportOwner(reg) || reg.AdmittedAt.IsZero() {
			continue
		}
		if reg.AdmittedAt.Before(b.StartedAt) || b.CompletedAt != nil && reg.AdmittedAt.After(*b.CompletedAt) {
			n++
		}
	}
	return n
}

// undoProjects splits the projects undoing b could exclude into those it
// excludes and those it keeps. The candidates are the included projects in
// scope that b added, and those an earlier undo kept (ProjectsKept) for b
// (ProjectsKeptFor) that b has sessions in: b's undo takes them over, and
// takenOver names, for each, the imports whose undo kept it. A project any
// undo has excluded (ProjectsExcluded of any batch) is never a candidate:
// it is included only because setup included it again.
//
// A candidate stays included while another import has sessions there.
// Excluding it would stop those sessions updating (the configuration no
// longer accepts them), silently, as part of undoing an import they are not
// in. Hook-captured sessions do not keep a project: the import added it, so
// undoing the import is what stops them, as the plan says.
func undoProjects(cfg config.Config, regs []archive.SessionRegistration, batches []Batch, b Batch, inProject func(string) bool) (exclude []archive.ProjectActivation, keep []KeptProject, takenOver map[string][]string, settled []string) {
	inside := func(reg archive.SessionRegistration, project archive.ProjectActivation) bool {
		return reg.ProjectID == project.ProjectID || reg.ProjectRoot == project.Root
	}
	// A project any undo has excluded is included now only because setup
	// included it again: it is the person's, whichever import added it.
	excludedByUndo := map[string]bool{}
	for _, o := range batches {
		for _, id := range o.ProjectsExcluded {
			excludedByUndo[id] = true
		}
	}
	for _, id := range b.ProjectsExcluded {
		excludedByUndo[id] = true
	}
	takenOver = map[string][]string{}
	for _, project := range cfg.Archive.Projects {
		if !inProject(project.Root) || excludedByUndo[project.ProjectID] {
			continue
		}
		candidate := slices.Contains(b.ProjectsAdded, project.ProjectID)
		var from []string
		if !candidate && slices.ContainsFunc(regs, func(reg archive.SessionRegistration) bool {
			return reg.InBatch(b.ID) && independentImportOwner(reg) && inside(reg, project)
		}) {
			// b takes the project over from each earlier undo that kept it
			// for b: one that names b among the imports it kept it for, or,
			// written before those were recorded, names none.
			for _, o := range batches {
				if o.ID == b.ID || o.UndoneAt == nil || !slices.Contains(o.ProjectsKept, project.ProjectID) {
					continue
				}
				if keptFor, recorded := o.ProjectsKeptFor[project.ProjectID]; recorded && !slices.Contains(keptFor, b.ID) {
					continue
				}
				from = append(from, o.ID)
			}
			candidate = len(from) > 0
		}
		if !candidate {
			continue
		}
		if !project.Included {
			// Excluded already. After an earlier run of this undo, that run
			// most likely excluded it and stopped before recording so
			// (settled): recording it keeps a later undo from excluding it
			// again once setup includes it. Otherwise there is nothing to do.
			if b.UndoneAt != nil {
				settled = append(settled, project.ProjectID)
			}
			continue
		}
		if len(from) > 0 {
			takenOver[project.ProjectID] = from
		}
		kept := KeptProject{Project: project}
		for _, reg := range regs {
			if reg.Imported() && !reg.ImportBatch.IsZero() && !reg.InBatch(b.ID) && independentImportOwner(reg) && inside(reg, project) {
				kept.Sessions++
				kept.Imports = addUnique(kept.Imports, reg.ImportBatch.Recorded())
			}
		}
		if kept.Sessions == 0 {
			exclude = append(exclude, project)
			continue
		}
		sort.Strings(kept.Imports)
		keep = append(keep, kept)
	}
	return exclude, keep, takenOver, settled
}

// RecordKept records in b the projects its undo keeps included, and the
// imports it keeps each for (ProjectsKept, ProjectsKeptFor), so that only
// those imports' undos take them over.
func (b *Batch) RecordKept(kept []KeptProject) {
	for _, k := range kept {
		id := k.Project.ProjectID
		b.ProjectsKept = addUnique(b.ProjectsKept, id)
		if b.ProjectsKeptFor == nil {
			b.ProjectsKeptFor = map[string][]string{}
		}
		imports := addUnique(append([]string(nil), b.ProjectsKeptFor[id]...), k.Imports...)
		sort.Strings(imports)
		b.ProjectsKeptFor[id] = imports
	}
}

// resumedSinceImport reports whether the app ran an imported session again
// after the import. Two signals, either of which is enough:
//
//   - The transcript was written after the import: its modification time is
//     later than AdmittedAt. Discovery read every transcript before the
//     import was stamped, and nothing in the archive writes to one, so only
//     the app did. This covers apps without hooks, hooks whose payload left
//     no evidence, and content not yet uploaded. For a chat read from
//     Cursor's database, which has no file of its own, it is the chat's
//     lastUpdatedAt, read in place from the database under env.Home.
//   - Hook evidence other than subagent links, waiting in a request, built
//     into a pending publication, or already published. Backfill records
//     none (its links are archive-generated, provenance hook:subagent-link),
//     so any came from a hook. This covers a resume whose transcript moved.
//
// The superseded-source ledger is not a signal: a parent is republished
// when its subagents publish (a link-only change), and a parser upgrade
// republishes everything, neither of which is a resume.
func resumedSinceImport(env Environment, store *state.Store, reg archive.SessionRegistration, req state.Request) (resumed, unknown bool, err error) {
	if reg.SourceKind == archive.SourceKindCursorSQLite && !reg.AdmittedAt.IsZero() {
		observed, err := observeSource(context.Background(), env.Sources, agentapi.SourceEnvironment{Database: env.cursorStateDatabase()}, reg.Harness.Name, agentapi.SourceRef{Kind: reg.SourceKind, Key: reg.SourceKey})
		if fatalSourceFailure(err) {
			return false, false, err
		}
		if err == nil && observed.Activity.After(reg.AdmittedAt) {
			return true, false, nil
		}
		// A chat or database that is gone was not resumed; one that could
		// not be read may have been.
		unknown = err != nil && !isNotExist(err)
	}
	resumed, err = resumedByEvidence(env, store, reg, req)
	return resumed, unknown && !resumed, err
}

// resumedByEvidence is resumedSinceImport's transcript time and hook
// evidence signals.
func resumedByEvidence(env Environment, store *state.Store, reg archive.SessionRegistration, req state.Request) (bool, error) {
	if reg.TranscriptPath != "" && !reg.AdmittedAt.IsZero() {
		if info, err := env.stat(reg.TranscriptPath); err == nil && info.ModTime().After(reg.AdmittedAt) {
			return true, nil
		}
	}
	if hasHookEvidence(req.HookEvidence) {
		return true, nil
	}
	// Only the stored bundles' evidence is decoded: decoding the whole
	// pending and published bundles cost several times their size per
	// session, partly under collector.lock.
	evidence, err := store.StoredEvidence(reg.ArchiveSessionID)
	if err != nil {
		return false, err
	}
	return hasHookEvidence(evidence), nil
}

func hasHookEvidence(evidence []archive.SupplementalEvidence) bool {
	return slices.ContainsFunc(evidence, func(e archive.SupplementalEvidence) bool {
		return strings.HasPrefix(e.Provenance, "hook:") && e.Provenance != "hook:subagent-link"
	})
}

// WithRetentionKept returns the plan leaving retention as it is instead of
// restoring the shorter value from before the import. `backfill undo --yes`
// uses it unless --restore-retention is given: the shorter retention
// deletes sessions that are not from the import (hook-captured ones, other
// imports'), which an unattended run must not do silently.
func (p UndoPlan) WithRetentionKept() UndoPlan {
	if p.RestoreRetention != nil {
		p.RetentionKept, p.RestoreRetention = p.RestoreRetention, nil
	}
	return p
}

// Empty reports whether the plan has no sessions or configuration changes left.
func (p UndoPlan) Empty() bool {
	return len(p.Sessions) == 0 && len(p.ExcludeProjects) == 0 && len(p.RemoveApps) == 0 && len(p.RemoveKeptOut) == 0 && p.RestoreRetention == nil
}

// UndoCounts summarises an undo's sessions. Sessions and Subagents count
// every one removed; Resumed counts the sessions (not subagents) resumed
// since the import; Deleted and Forgotten split them all by whether the
// bucket is called, and DeletedSessions and ForgottenSessions split the
// sessions alone.
type UndoCounts struct {
	Sessions  int
	Subagents int
	Resumed   int
	// ResumeUnknown counts the Cursor database chats whether resumed could
	// not be checked.
	ResumeUnknown     int
	Deleted           int
	Forgotten         int
	DeletedSessions   int
	ForgottenSessions int
}

// Counts tallies the plan's sessions and subagents into UndoCounts.
func (p UndoPlan) Counts() UndoCounts {
	var c UndoCounts
	for _, s := range p.Sessions {
		parent := !s.Registration.IsChild()
		if parent {
			c.Sessions++
			if s.Resumed {
				c.Resumed++
			}
			if s.ResumeUnknown {
				c.ResumeUnknown++
			}
		} else {
			c.Subagents++
		}
		if s.InCurrentDestination {
			c.Deleted++
			if parent {
				c.DeletedSessions++
			}
		} else {
			c.Forgotten++
			if parent {
				c.ForgottenSessions++
			}
		}
	}
	return c
}

// ProjectDisplay is the --project directory as the plan shows it, with ~.
func (p UndoPlan) ProjectDisplay() string { return p.view.display(p.Project) }

// Grew reports whether p would remove anything confirmed did not show: a
// session, a bucket delete for a session shown as only forgotten, a project,
// a kept-out entry, or an app. Undo checks this after confirming, under the
// locks, and asks for a new run rather than removing more than was confirmed.
// A session resumed meanwhile does not count: the person already confirmed
// deleting it, and one in active use would otherwise abort every run.
func (p UndoPlan) Grew(confirmed UndoPlan) bool {
	sessions := map[string]UndoSession{}
	for _, s := range confirmed.Sessions {
		sessions[s.Registration.ArchiveSessionID] = s
	}
	for _, s := range p.Sessions {
		before, ok := sessions[s.Registration.ArchiveSessionID]
		if !ok || s.InCurrentDestination && !before.InCurrentDestination {
			return true
		}
	}
	for _, project := range p.ExcludeProjects {
		if !slices.ContainsFunc(confirmed.ExcludeProjects, func(c archive.ProjectActivation) bool { return c.ProjectID == project.ProjectID }) {
			return true
		}
	}
	for _, app := range p.RemoveApps {
		if !slices.Contains(confirmed.RemoveApps, app) {
			return true
		}
	}
	for _, project := range p.RemoveKeptOut {
		if !slices.ContainsFunc(confirmed.RemoveKeptOut, func(c archive.ProjectActivation) bool { return c.ProjectID == project.ProjectID }) {
			return true
		}
	}
	if p.RestoreRetention != nil && (confirmed.RestoreRetention == nil || p.RetentionDeletes > confirmed.RetentionDeletes) {
		return true
	}
	return false
}

// UndoChanges is what UndoPlan.ApplyToConfig changed; the caller records it
// in the batch once the configuration is saved.
type UndoChanges struct {
	// Excluded are the IDs of the projects excluded.
	Excluded []string
	// RetentionRestored is set when retention was put back to the value
	// before the import.
	RetentionRestored bool
	// RemovedKeptOut are the IDs of the kept-out projects removed.
	RemovedKeptOut []string
}

// ApplyToConfig excludes the plan's projects, removes its apps, and restores
// the retention from before the import, in cfg, which the caller has just
// reloaded under the locks. Retention is restored only while it is still
// what the import set.
func (p UndoPlan) ApplyToConfig(cfg *config.Config) UndoChanges {
	changes := UndoChanges{Excluded: []string{}}
	for i, project := range cfg.Archive.Projects {
		if project.Included && slices.ContainsFunc(p.ExcludeProjects, func(e archive.ProjectActivation) bool { return e.ProjectID == project.ProjectID }) {
			cfg.Archive.Projects[i].Included = false
			changes.Excluded = append(changes.Excluded, project.ProjectID)
		}
	}
	cfg.ImportedHarnesses = slices.DeleteFunc(cfg.ImportedHarnesses, func(app string) bool { return slices.Contains(p.RemoveApps, app) })
	cfg.Archive.Projects = slices.DeleteFunc(cfg.Archive.Projects, func(project archive.ProjectActivation) bool {
		remove := !project.Included && slices.ContainsFunc(p.RemoveKeptOut, func(g archive.ProjectActivation) bool { return g.ProjectID == project.ProjectID })
		if remove {
			changes.RemovedKeptOut = append(changes.RemovedKeptOut, project.ProjectID)
		}
		return remove
	})
	if r := p.RestoreRetention; r != nil && cfg.RetentionDays == r.To {
		cfg.RetentionDays = r.From
		changes.RetentionRestored = true
	}
	return changes
}

// RecordUndone records in b what an undo changed in the configuration, once
// it is saved, so no later undo changes it again.
func (b *Batch) RecordUndone(c UndoChanges) {
	b.ProjectsExcluded = addUnique(b.ProjectsExcluded, c.Excluded...)
	if c.RetentionRestored && b.Retention != nil {
		b.Retention.Restored = true
	}
}

// UndoResult is what Remove did.
type UndoResult struct {
	// Deleted were removed from the bucket and forgotten; Forgotten, from a
	// previous destination, were only forgotten locally.
	Deleted   []string
	Forgotten []string
	// Failed maps each session left registered to why; running undo again
	// retries it.
	Failed map[string]error
}

// Remove deletes each session of the plan from bucket, as whole-session
// retention does, and forgets it locally with an undo removal record.
// Subagents go before their parents. A failure leaves that session
// registered and moves on to the next. The caller holds collector.lock, so
// no pass publishes a session between its deletion and being forgotten.
// bucket may be nil when no session is in the current destination.
func (p UndoPlan) Remove(ctx context.Context, store *state.Store, bucket storage.ObjectStore, now time.Time) UndoResult {
	result := UndoResult{Failed: map[string]error{}}
	for _, s := range p.Sessions {
		reg := s.Registration
		if err := store.CheckDurableSessionRead(reg.ArchiveSessionID); err != nil {
			result.Failed[reg.ArchiveSessionID] = fmt.Errorf("recover local publication before undo: %w", err)
			continue
		}
		if s.InCurrentDestination {
			if err := retention.DeleteWholeSession(ctx, bucket, reg.Harness.Name, reg.ArchiveSessionID); err != nil {
				result.Failed[reg.ArchiveSessionID] = fmt.Errorf("delete from the bucket: %w", err)
				continue
			}
		}
		app := archive.CanonicalHarness(reg.Harness.Name)
		// deferForWork is off: undo removes the session whatever a hook
		// queued for it meanwhile. ForgetSession also drops the parent's
		// subagent candidates that were never registered.
		if _, err := store.ForgetIdleSession(reg.ArchiveSessionID, agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness(reg.Harness.Name)), NativeID: reg.NativeSessionID}, false, &state.RemovalRecord{
			Harness: app, Reason: state.RemovalReasonUndo, At: now,
		}); err != nil {
			result.Failed[reg.ArchiveSessionID] = fmt.Errorf("forget locally: %w", err)
			continue
		}
		if s.InCurrentDestination {
			result.Deleted = append(result.Deleted, reg.ArchiveSessionID)
		} else {
			result.Forgotten = append(result.Forgotten, reg.ArchiveSessionID)
		}
	}
	return result
}

// RenderUndo prints what the undo will do, before it asks. Like the import
// plan, it names projects and counts only.
func RenderUndo(w io.Writer, p UndoPlan) {
	c := p.Counts()
	scope := "import " + p.Batch.ID
	if p.Project != "" {
		scope += " in " + p.view.display(p.Project)
	}
	terminal.Printf(w, "Undo %s, started %s.\n\n", scope, p.Batch.StartedAt.In(p.view.GeneratedAt.Location()).Format("2006-01-02 15:04"))
	// Every change is a bullet under one heading.
	headed := false
	bullet := func(format string, args ...any) {
		if !headed {
			headed = true
			terminal.Println(w, "If you continue:")
		}
		terminal.Printf(w, "  • "+format, args...)
	}
	renderUndoSessions(w, p, c, bullet)
	renderUndoExcluded(w, p, bullet)
	if len(p.RemoveApps) > 0 {
		names := make([]string, 0, len(p.RemoveApps))
		for _, app := range p.RemoveApps {
			name := harnessNames[app]
			if name == "" {
				name = app
			}
			names = append(names, name)
		}
		bullet("%s imports without hooks are no longer published.\n", joinAnd(names))
	}
	renderUndoKept(w, p, bullet)
	if n := len(p.RemoveKeptOut); n > 0 {
		bullet("%s the import added as excluded, to keep folders inside a project\n    out of capture, %s removed from setup again; nothing there is captured\n    either way.\n", CountNoun(n, "project"), IsAre(n))
	}
	switch r := p.RestoreRetention; {
	case r != nil && r.To == 0:
		bullet("Retention goes back from Keep forever to %s. The next collector pass may delete %s.\n", config.RetentionLabel(r.From), CountNoun(p.RetentionDeletes, "session"))
	case r != nil && p.RetentionDeletes > 0:
		bullet("Retention goes back from %d to %d days, as it was before the import\n    raised it. The next collector pass then deletes %s older than\n    %d days from %s.\n", r.To, r.From, CountNoun(p.RetentionDeletes, "session"), r.From, p.view.destination())
	case r != nil:
		bullet("Retention goes back from %d to %d days, as it was before the import\n    raised it. No session is old enough for that to delete it now.\n", r.To, r.From)
	case p.RetentionKept != nil:
		r := p.RetentionKept
		bullet("Retention stays at %s: --yes does not shorten it. To put back\n    the %d days from before the import, which %s, run\n    agent-archive backfill undo %s --restore-retention\n", config.RetentionLabel(r.To), r.From, retentionDeletesPhrase(p.RetentionDeletes, r.From), p.Batch.ID)
	case p.RetentionChangedSince:
		bullet("Retention stays at %s: it was changed after the import raised it.\n", config.RetentionLabel(p.view.RetentionDays))
	}
	bullet("Hook-captured sessions and the apps' own files are not touched.\n")
	if c.Sessions+c.Subagents > 0 {
		bullet("These sessions are not imported again unless you run\n    agent-archive backfill --include-removed.\n")
	}
}

// renderUndoSessions is RenderUndo's sessions: deleted, or forgotten from a
// previous destination, and how many were resumed.
func renderUndoSessions(w io.Writer, p UndoPlan, c UndoCounts, bullet func(string, ...any)) {
	sessions := SessionsAndSubagents(c.Sessions, c.Subagents)
	switch {
	case c.Deleted > 0 && c.Forgotten == 0:
		bullet("%s %s deleted from\n    %s.\n", sessions, IsAre(c.Sessions+c.Subagents), p.view.destination())
	case c.Deleted > 0:
		bullet("%s %s deleted from\n    %s.\n", CountNoun(c.Deleted, "session"), IsAre(c.Deleted), p.view.destination())
		bullet("%s from a previous storage destination %s forgotten on this machine\n    only; nothing is deleted from that destination.\n", CountNoun(c.Forgotten, "session"), IsAre(c.Forgotten))
	case c.Forgotten > 0:
		bullet("%s from a previous storage destination %s forgotten on this machine\n    only; nothing is deleted from that destination.\n", sessions, IsAre(c.Sessions+c.Subagents))
	}
	if c.Resumed > 0 {
		terminal.Printf(w, "    This includes %s resumed since the import, with %s newer content.\n", CountNoun(c.Resumed, "session"), theirIts(c.Resumed))
	}
	if n := c.ResumeUnknown; n > 0 {
		terminal.Printf(w, "    Whether %s resumed since the import could not be checked:\n    Cursor's database could not be read.\n", CountNoun(n, "Cursor chat")+" "+wasWere(n))
	}
}

// renderUndoExcluded is RenderUndo's excluded projects: those the import
// added, and apart from them those it took over from an earlier undo that
// kept them for its sessions.
func renderUndoExcluded(w io.Writer, p UndoPlan, bullet func(string, ...any)) {
	n := len(p.ExcludeProjects)
	if n == 0 {
		return
	}
	var added, takenOver []string
	for _, project := range p.ExcludeProjects {
		if from, ok := p.TakenOver[project.ProjectID]; ok {
			takenOver = append(takenOver, fmt.Sprintf("%s (left included by the undo of %s)", p.view.display(project.Root), importsNoun(from)))
		} else {
			added = append(added, p.view.display(project.Root))
		}
	}
	sort.Strings(added)
	sort.Strings(takenOver)
	if k := len(added); k > 0 {
		bullet("%s the import added %s excluded from capture; setup can\n    include %s again:\n", CountNoun(k, "project"), IsAre(k), themIt(k))
		for _, root := range added {
			terminal.Printf(w, "      %s\n", root)
		}
	}
	if k := len(takenOver); k > 0 {
		bullet("%s an earlier undo left included for this import's sessions\n    %s excluded from capture; setup can include %s again:\n", CountNoun(k, "project"), IsAre(k), themIt(k))
		for _, line := range takenOver {
			terminal.Printf(w, "      %s\n", line)
		}
	}
	if h := p.HookCapturedStopping; h > 0 {
		where, verb, what := "these projects", "stop", "they are"
		if n == 1 {
			where = "this project"
		}
		if h == 1 {
			verb, what = "stops", "it is"
		}
		terminal.Printf(w, "    %s in %s %s uploading; %s not deleted.\n", CountNoun(h, "hook-captured session"), where, verb, what)
	}
}

// renderUndoKept is RenderUndo's kept projects, with the imports that keep
// each and, for one taken over, whose undo kept it before.
func renderUndoKept(w io.Writer, p UndoPlan, bullet func(string, ...any)) {
	n := len(p.KeepProjects)
	if n == 0 {
		return
	}
	which := "the import added"
	if slices.ContainsFunc(p.KeepProjects, func(k KeptProject) bool { _, ok := p.TakenOver[k.Project.ProjectID]; return ok }) {
		which = "the import added or took over"
	}
	bullet("%s %s %s included: other imports still have\n    sessions there, which excluding %s would stop updating:\n", CountNoun(n, "project"), which, stayStays(n), themIt(n))
	kept := make([]string, 0, n)
	for _, k := range p.KeepProjects {
		line := fmt.Sprintf("%s (%s from %s", p.view.display(k.Project.Root), CountNoun(k.Sessions, "session"), importsNoun(k.Imports))
		if from, ok := p.TakenOver[k.Project.ProjectID]; ok {
			line += "; left included by the undo of " + importsNoun(from)
		}
		kept = append(kept, line+")")
	}
	sort.Strings(kept)
	for _, line := range kept {
		terminal.Printf(w, "      %s\n", line)
	}
	terminal.Printf(w, "    Undoing the last of those imports excludes %s.\n", themIt(n))
}

// importsNoun names import IDs: "import A", or "imports A and B".
func importsNoun(ids []string) string {
	if len(ids) == 1 {
		return "import " + ids[0]
	}
	return "imports " + joinAnd(ids)
}

// retentionDeletesPhrase says what restoring a retention of from days
// deletes.
func retentionDeletesPhrase(n, from int) string {
	if n == 0 {
		return "deletes no session now"
	}
	return fmt.Sprintf("deletes %s older than %d days", CountNoun(n, "session"), from)
}

// UndoQuestion is the confirmation undo asks. When restoring retention
// deletes sessions the undo does not name (they are not from the import),
// the question says how many, so the confirmation covers them too.
func UndoQuestion(p UndoPlan) string {
	c := p.Counts()
	retention := ""
	if r := p.RestoreRetention; r != nil && p.RetentionDeletes > 0 {
		retention = fmt.Sprintf(" Restoring retention to %d days also deletes %s older than that.", r.From, CountNoun(p.RetentionDeletes, "other session"))
	}
	if c.Sessions+c.Subagents == 0 {
		if len(p.ExcludeProjects) == 0 && p.RestoreRetention != nil {
			if retention != "" {
				return fmt.Sprintf("Restore retention to %d days? That deletes %s older than that. This cannot be undone.", p.RestoreRetention.From, CountNoun(p.RetentionDeletes, "session"))
			}
			return fmt.Sprintf("Restore retention to %d days?", p.RestoreRetention.From)
		}
		if retention != "" {
			return fmt.Sprintf("Exclude %s?%s This cannot be undone.", CountNoun(len(p.ExcludeProjects), "project"), retention)
		}
		return fmt.Sprintf("Exclude %s?", CountNoun(len(p.ExcludeProjects), "project"))
	}
	// Sessions are named when there are any; otherwise only subagents are
	// left.
	deleted, forgotten := CountNoun(c.DeletedSessions, "session"), CountNoun(c.ForgottenSessions, "session")
	if c.Sessions == 0 {
		deleted, forgotten = CountNoun(c.Deleted, "subagent transcript"), CountNoun(c.Forgotten, "subagent transcript")
	}
	switch {
	case c.Deleted == 0:
		return fmt.Sprintf("Forget %s?%s This cannot be undone.", forgotten, retention)
	case c.Forgotten == 0:
		return fmt.Sprintf("Delete %s from the archive?%s This cannot be undone.", deleted, retention)
	}
	return fmt.Sprintf("Delete %s from the archive and forget %s from a previous destination?%s This cannot be undone.", deleted, forgotten, retention)
}

// SessionsAndSubagents counts sessions and subagent transcripts together,
// naming only the kinds there are: "2 sessions and 1 subagent transcript".
func SessionsAndSubagents(sessions, subagents int) string {
	switch {
	case subagents == 0:
		return CountNoun(sessions, "session")
	case sessions == 0:
		return CountNoun(subagents, "subagent transcript")
	}
	return CountNoun(sessions, "session") + " and " + CountNoun(subagents, "subagent transcript")
}

// IsAre is the verb that agrees with a count of n.
func IsAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

func stayStays(n int) string {
	if n == 1 {
		return "stays"
	}
	return "stay"
}

func wasWere(n int) string {
	if n == 1 {
		return "was"
	}
	return "were"
}

func theirIts(n int) string {
	if n == 1 {
		return "its"
	}
	return "their"
}

func themIt(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}

// Delete descendants before selected ancestors. Only exact batch membership
// supplied by selection participates; external parents are never pulled in.
func orderUndoChildrenFirst(sessions []UndoSession) ([]UndoSession, error) {
	index := make(map[string]int, len(sessions))
	for i, s := range sessions {
		index[s.Registration.ArchiveSessionID] = i
	}
	depth := map[string]int{}
	visiting := map[string]bool{}
	var walk func(string, int) (int, error)
	walk = func(id string, level int) (int, error) {
		if level > 64 || visiting[id] {
			return 0, errors.New("invalid nested child relationship")
		}
		if d, ok := depth[id]; ok {
			return d, nil
		}
		i, found := index[id]
		if !found {
			return 0, nil
		}
		visiting[id] = true
		reg := sessions[i].Registration
		d := 0
		if reg.IsChild() {
			d = 1
		}
		if _, found := index[reg.ParentSessionID]; found {
			parent, err := walk(reg.ParentSessionID, level+1)
			if err != nil {
				return 0, err
			}
			d = parent + 1
		}
		if d > 64 {
			return 0, errors.New("invalid nested child relationship")
		}
		delete(visiting, id)
		depth[id] = d
		return d, nil
	}
	for id := range index {
		if _, err := walk(id, 0); err != nil {
			return nil, err
		}
	}
	slices.SortStableFunc(sessions, func(a, b UndoSession) int {
		return depth[b.Registration.ArchiveSessionID] - depth[a.Registration.ArchiveSessionID]
	})
	return sessions, nil
}

// Native Codex children own their import permission and project responsibility.
// Other subagents continue to follow their imported parent.
func independentImportOwner(reg archive.SessionRegistration) bool {
	return reg.NativeChild || reg.ParentSessionID == ""
}

func importUndoRegistrations(store *state.Store, regs []archive.SessionRegistration, batchID string) ([]archive.SessionRegistration, error) {
	cfg, found, err := config.Load(store.Home())
	if err != nil {
		return nil, err
	}
	if !found || !cfg.ImportStagingProtection {
		return regs, nil
	}

	// Include selected evidence prepared before an admission crash. Undo owns
	// its durable cleanup even when no registration reached disk yet.
	seen := map[string]bool{}
	for _, reg := range regs {
		seen[reg.ArchiveSessionID] = true
	}
	for after := ""; ; {
		stages, next, err := store.ImportStages(context.Background(), after, 50)
		if err != nil {
			return nil, err
		}
		for _, stage := range stages {
			reg := stage.Registration
			if reg.InBatch(batchID) && !seen[reg.ArchiveSessionID] {
				reg.StagedImportSHA256 = stage.SHA256
				regs = append(regs, reg)
				seen[reg.ArchiveSessionID] = true
			}
		}
		if next == "" {
			break
		}
		after = next
	}
	return regs, nil
}
