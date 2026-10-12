package backfill

import (
	"context"
	"errors"
	"path/filepath"
	"slices"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
)

// prepareRecoveryInventory separates observed repository membership from
// committed capture policy. Discovery has finished; output filters have not run.
// Gaps stay per app: each session's recorded recovery is blocked only by the
// gaps that can affect it (see recoveryGaps.blocking).
func prepareRecoveryInventory(ctx context.Context, r *resolver, items []*work, unread unreadable, dbGaps recoveryGaps) {
	items = recoveryEvidenceItems(items)
	slugs := addedSourceSlugs(items)
	var inventoryGaps recoveryGaps
	if r.inventoryAdded != nil {
		// Transcripts added while planning become evidence; any other
		// membership change leaves every app's inventory incomplete.
		added, ok := r.inventoryAdded(ctx)
		if !ok {
			inventoryGaps.add("", CauseNativeInventoryChanged)
		} else {
			var witnesses []*work
			witnesses, inventoryGaps = addedWitnesses(r, items, added)
			items = append(items, witnesses...)
		}
	}
	projects, witnesses, gaps := recoveryWitnessInventory(r, items)
	gaps.merge(inventoryGaps)
	gaps.merge(dbGaps)
	gaps.addUnreadable(unread)
	r.recoveryGaps = gaps
	selectedRoots := map[string]bool{}
	r.requireWitnessFormats = func(root string) {
		selectedRoots[root] = true
		for _, w := range witnesses[root] {
			w.proposedWitness = true
		}
	}
	r.proposedRootEligible = func(root string) bool {
		for _, w := range witnesses[root] {
			if w.validated && w.importable() && !w.vanished && !w.evidenceOnly {
				return true
			}
		}
		return false
	}
	ownershipCurrent := recoveryOwnershipCurrent(r, items)
	validation := &recoveryWitnessValidation{ctx: ctx}
	r.recoverySourcesReset = func(ctx context.Context) {
		validation.ctx = ctx
		validation.checked, validation.current = false, false
		if r.workspaceReset != nil {
			r.workspaceReset()
		}
	}
	r.recoverySourcesCurrent = func(h harness, proof archive.ProjectResolution) bool {
		if !validation.checked {
			validation.checked = true
			validation.database = r.databaseRecoveryCurrent == nil || r.databaseRecoveryCurrent(validation.ctx)
			var current bool
			var added addedSources
			current, added, validation.databaseRoots = recoveryWitnessesCurrent(validation.ctx, r, witnesses, selectedRoots, validation.database)
			validation.current = current && ownershipCurrent(validation.ctx)
			if validation.current {
				validation.addedKeys, validation.addedGaps = addedRecoveryBlocks(validation.ctx, r, slugs, witnesses, added)
				validation.current = validation.ctx.Err() == nil
			}
		}
		return validation.current && (validation.database || !cursorEvidenceRequired(h, validation.databaseRoots[proof.Root])) && addedAllows(validation.addedKeys, validation.addedGaps, h, proof)
	}
	r.mappingRecovery = r.recovery
	r.recovery = sourcefacts.NewRecoveryResolver(projects, r.filters.ProjectMappings, r.env.resolved, r.env.RepositoryIdentity, nil)
	// A proposed root whose checkout has no repository key cannot own a
	// recorded key; it must not block recovery into configured projects.
	r.recovery.Proposed = proposedRoots(r, projects)
	r.recovery.MaxOperations = 1024
	r.recovery.Validate = r.env.RepositoryIdentityCurrent
	r.recovery.ResetValidationContext(ctx)
	r.gapProjects = projects
	r.gapRecovery = map[bool]*sourcefacts.RecoveryResolver{}
	// Ordinary path ownership still uses committed r.cfg exclusively.
	r.cache = map[string]resolution{}
}

func proposedRoots(r *resolver, projects []archive.ProjectActivation) map[string]bool {
	configured := map[string]bool{}
	for _, p := range r.cfg.Archive.Projects {
		configured[p.Root] = true
	}
	proposed := map[string]bool{}
	for _, p := range projects {
		if !configured[p.Root] {
			proposed[p.Root] = true
		}
	}
	return proposed
}

type recoveryWitnessValidation struct {
	ctx     context.Context
	checked bool
	current bool
	// database reports whether the Cursor database epoch is still current;
	// databaseRoots are the destination roots whose witness proof rests on
	// Cursor database chats alone (computed only when it is not).
	database      bool
	databaseRoots map[string]bool
	// addedKeys and addedGaps are what transcripts added since planning
	// block (addedRecoveryBlocks).
	addedKeys map[string]bool
	addedGaps recoveryGaps
}

// cursorEvidenceRequired reports whether a recovery for a session of h into
// a destination whose proof rests on Cursor database chats (dependsOnCursor)
// needs the Cursor database epoch to still be current. A changed epoch blocks
// Cursor's own sessions, sessions of unknown app, and any recovery whose
// destination was proposed by Cursor database evidence. It does not block
// another app's recovery into a destination its own evidence proposes: that
// proof used Cursor's chats only as possible clones, and a change can at most
// add or remove one, the per-app gap residual (dev/specs/backfill.md,
// "Recorded project recovery").
func cursorEvidenceRequired(h harness, dependsOnCursor bool) bool {
	return h == "" || h == harnessCursor || dependsOnCursor
}

// recoveryGap is one reason the witness inventory may omit a clone root.
// Agent names the app whose evidence is missing ("" when not attributable);
// the plan reports it only as an app name (RecoveryEvidenceGap).
type recoveryGap struct {
	Agent string
	Cause DiagnosticDetail
}

// recoveryGaps is the bounded set of witness evidence gaps for one plan:
// missing evidence could name a second clone holding the same recorded
// repository key. Which sessions a gap blocks is per app (see blocking).
type recoveryGaps map[recoveryGap]bool

// recoveryGapCauses orders causes for the single diagnostic a candidate
// carries. Budget comes first because it also selects the budget outcome.
var recoveryGapCauses = []DiagnosticDetail{
	CauseRecoveryBudget,
	CauseWitnessLimit,
	CauseNativeStoreUnreadable,
	CauseCursorDatabaseUnavailable,
	CauseNativeInventoryChanged,
	CauseCursorChatFolderUnavailable,
	CauseSessionFolderUnknown,
}

func (g *recoveryGaps) add(agent string, cause DiagnosticDetail) {
	if *g == nil {
		*g = recoveryGaps{}
	}
	(*g)[recoveryGap{Agent: agent, Cause: cause}] = true
}

func (g *recoveryGaps) merge(other recoveryGaps) {
	for gap := range other {
		g.add(gap.Agent, gap.Cause)
	}
}

func (g *recoveryGaps) addUnreadable(unread unreadable) {
	for agent := range unread.folderAgents {
		g.add(agent, CauseNativeStoreUnreadable)
	}
	if unread.folders > 0 && len(unread.folderAgents) == 0 {
		g.add("", CauseNativeStoreUnreadable)
	}
	for agent, unreadable := range unread.stores {
		if unreadable {
			g.add(agent, CauseNativeStoreUnreadable)
		}
	}
}

// cause is the highest-priority gap cause, or "" for a complete inventory.
func (g *recoveryGaps) cause() DiagnosticDetail {
	for _, cause := range recoveryGapCauses {
		for gap := range *g {
			if gap.Cause == cause {
				return cause
			}
		}
	}
	return ""
}

// witnessGap classifies an item that cannot be counted as a witness. ok is
// false for a usable item. A Cursor database chat with no folder evidence at
// all names no root, so it is a non-witness rather than a gap.
func witnessGap(w *work) (cause DiagnosticDetail, ok bool) {
	database := w.c.SourceKind == archive.SourceKindCursorSQLite
	switch {
	case w.appendedOnly() && !database && !w.unsafe && !w.t.identityMismatch && w.res.skip != SkipProjectUnknown:
		// A pure append cannot change header facts; the file stays clone
		// evidence (non-eligible) instead of hiding every app's recoveries.
		return "", false
	case w.vanished || w.sourceChanged:
		if database {
			return CauseCursorDatabaseUnavailable, true
		}
		return CauseNativeInventoryChanged, true
	case w.unsafe || w.t.identityMismatch:
		if database {
			return CauseCursorDatabaseUnavailable, true
		}
		return CauseSessionFolderUnknown, true
	case w.res.skip != SkipProjectUnknown:
		return "", false
	case !database:
		return CauseSessionFolderUnknown, true
	case cursorChatFolderless(w.chat, w.messageFolders):
		return "", false
	default:
		return CauseCursorChatFolderUnavailable, true
	}
}

// cursorChatFolderless reports a Cursor database chat with no folder evidence:
// no folder, no workspace reference and no message folder. Such a chat (for
// example a subagent composer) cannot name a clone root, so it cannot hide a
// second clone. A workspace reference stays evidence even when unreadable or
// of a shape this release cannot resolve (a workspaceIdentifier whose URI is
// remote or unknown), and several message folders stay evidence even though
// none is chosen.
func cursorChatFolderless(chat CursorDatabaseChat, messageFolders []string) bool {
	return chat.Folder == "" && chat.WorkspaceID == "" && !chat.WorkspaceIdentifier && len(messageFolders) == 0
}

func recoveryWitnessInventory(r *resolver, items []*work) ([]archive.ProjectActivation, map[string][]*work, recoveryGaps) {
	projects := slices.Clone(r.cfg.Archive.Projects)
	configured := map[string]bool{}
	for _, p := range projects {
		configured[r.env.resolved(p.Root)] = true
	}
	observed := map[string]bool{}
	witnesses := map[string][]*work{}
	var gaps recoveryGaps
	for _, w := range items {
		if cause, gap := witnessGap(w); gap {
			gaps.add(string(w.t.harness), cause)
			continue
		}
		if w.res.skip == SkipProjectUnknown {
			continue // folderless: a non-witness, not a gap
		}
		if w.res.skip != "" || w.res.kind != ProjectKindRepository || !r.env.exists(w.res.root) || configured[r.env.resolved(w.res.root)] {
			continue
		}
		// Pending or oversized sources can establish clone uncertainty, but cannot
		// authorize a new destination on their own.
		if _, existing := observed[w.res.root]; !existing && len(observed) >= 1024 {
			gaps.add("", CauseWitnessLimit)
			continue
		}
		// Appended sources and an incomplete source's witnesses count as
		// possible clones but cannot propose a destination.
		eligible := !w.t.capturePending && !w.tooLarge && !w.sourceChanged && !w.evidenceOnly
		observed[w.res.root] = observed[w.res.root] || eligible
		witnesses[w.res.root] = append(witnesses[w.res.root], w)
	}
	for root, eligible := range observed {
		projects = append(projects, archive.ProjectActivation{Root: root, ProjectID: archive.ProjectID(root), Included: eligible})
	}
	return projects, witnesses, gaps
}

// recoveryWitnessesCurrent renews every witness root: each must still have one
// current witness. Cursor database chats are current with their epoch
// (databaseCurrent). When the epoch changed, a root whose only current
// evidence (for a selected destination, its only current evidence able to
// propose it) is Cursor database chats is returned in cursorRoots instead of
// failing every recovery; recoverySourcesCurrent decides whom it blocks. The
// native inventory may only have grown; added is what it gained.
func recoveryWitnessesCurrent(ctx context.Context, r *resolver, witnesses map[string][]*work, selectedRoots map[string]bool, databaseCurrent bool) (current bool, added addedSources, cursorRoots map[string]bool) {
	if r.inventoryAdded != nil {
		var ok bool
		if added, ok = r.inventoryAdded(ctx); !ok {
			return false, addedSources{}, nil
		}
	}
	renewal := witnessRenewal{ctx: ctx, r: r, ownership: newResolver(r.env, r.cfg, r.filters), databaseCurrent: databaseCurrent}
	for root, group := range witnesses {
		found, proposes, database, ok := renewal.root(group, selectedRoots[root])
		if !ok || (!found && !database) {
			return false, addedSources{}, nil
		}
		if !databaseCurrent && database && (!found || (selectedRoots[root] && !proposes)) {
			if cursorRoots == nil {
				cursorRoots = map[string]bool{}
			}
			cursorRoots[root] = true
		}
	}
	return true, added, cursorRoots
}

// witnessRenewal renews witness roots within one bounded check budget.
type witnessRenewal struct {
	ctx             context.Context
	r               *resolver
	ownership       *resolver
	checks          int
	databaseCurrent bool
}

// root renews one root's witnesses. found is a current witness (a Cursor
// database chat counts only while its epoch is current), proposes a current
// non-database witness able to propose the root, database a Cursor database
// chat still owned by the root. ok is false when the budget or context ended.
func (v *witnessRenewal) root(group []*work, selected bool) (found, proposes, database, ok bool) {
	for _, w := range group {
		if selected && !w.importable() {
			continue
		}
		if v.checks >= 1024 || v.ctx.Err() != nil {
			return false, false, false, false
		}
		v.checks++
		if w.t.cwd != "" {
			fresh := v.ownership.resolve(w.t.cwd)
			if fresh.root != w.res.root || fresh.kind != w.res.kind || fresh.skip != w.res.skip {
				continue
			}
		}
		if w.c.SourceKind == archive.SourceKindCursorSQLite {
			if v.databaseCurrent {
				return true, proposes, database, true
			}
			database = true
			continue
		}
		if (w.sourceCurrent(v.r.env) || w.stillAppendedOnly(v.r.env) || w.addedSourceGrown(v.r.env)) && (w.workspaceCurrent == nil || w.workspaceCurrent(v.ctx)) {
			found = true
			proposes = proposes || proposesRoot(w)
			if v.databaseCurrent || !selected || proposes {
				break
			}
		}
	}
	return found, proposes, database, true
}

// proposesRoot reports a witness that can make its root an eligible proposed
// destination on its own (see recoveryWitnessInventory and
// proposedRootEligible).
func proposesRoot(w *work) bool {
	return w.validated && w.importable() && !w.vanished && !w.evidenceOnly && !w.t.capturePending && !w.tooLarge && !w.sourceChanged
}

// bindRecoveryPolicy binds admitted imports to the exact prospective config,
// while Context and the validation closure retain all hidden clone evidence.
func bindRecoveryPolicy(cfg config.Config, p *Plan) error {
	projected := cfg
	projected.Archive.Projects = slices.Clone(cfg.Archive.Projects)
	projected.ImportedHarnesses = slices.Clone(cfg.ImportedHarnesses)
	if _, err := ApplyToConfig(&projected, *p, p.GeneratedAt); err != nil {
		return err
	}
	policy := sourcefacts.RecoveryContext(projected.Archive.Projects, nil, filepath.Clean)
	for i := range p.Candidates {
		c := &p.Candidates[i]
		if c.ProjectResolution != nil {
			c.ProjectResolution.PolicyContext = policy
		}
	}
	base := cfg
	base.Archive.Projects = slices.Clone(cfg.Archive.Projects)
	base.ImportedHarnesses = slices.Clone(cfg.ImportedHarnesses)
	p.policyConfig = &base
	return nil
}

// errRecoveryChanged refuses an import none of whose sessions is still current.
var errRecoveryChanged = errors.New("project or source evidence changed or is unavailable; run backfill again. Nothing was changed")

// errRecoveryUnbound refuses an import some of whose sessions changed when the
// plan has no bound recovery policy to narrow it with.
var errRecoveryUnbound = errors.New("project or source evidence changed for some sessions, and this plan cannot leave them out; run backfill again. Nothing was changed")

// AdmitRecovery requires a non-nil context and renews recovered ownership
// before committing proposed capture configuration. It performs repository
// work outside the short hooks lock hold.
//
// A session whose project or source evidence is no longer current is left out
// of the returned plan (as source_changed) instead of refusing the whole
// import, as planning leaves out a transcript that changed while it was read;
// changed counts them. The returned plan imports a subset of the confirmed one
// and is bound to the configuration that subset needs: a proposed project only
// the left-out sessions needed is no longer added (bindRecoveryPolicy). It
// refuses when the context ends, or when no session would be left.
func (p Plan) AdmitRecovery(ctx context.Context) (admitted Plan, changed int, err error) {
	if ctx == nil {
		return Plan{}, 0, errors.New("recovery validation requires a context")
	}
	if err := ctx.Err(); err != nil {
		return Plan{}, 0, err
	}
	for _, c := range p.Imported() {
		if c.projectResolutionReset != nil {
			c.projectResolutionReset(ctx)
		}
	}
	stale := map[int]bool{}
	for i, c := range p.Candidates {
		if c.Skip != "" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return Plan{}, 0, err
		}
		if c.projectResolutionCurrent != nil && !c.projectResolutionCurrent() {
			stale[i] = true
		}
	}
	// A check that ended with the context says nothing about the evidence.
	if err := ctx.Err(); err != nil {
		return Plan{}, 0, err
	}
	if len(stale) == 0 {
		return p, 0, nil
	}
	if len(stale) == len(p.Imported()) {
		return Plan{}, 0, errRecoveryChanged
	}
	if p.policyConfig == nil {
		// Without the configuration it was bound to, a narrowed plan cannot be
		// bound again, so the import cannot leave the changed sessions out.
		return Plan{}, 0, errRecoveryUnbound
	}
	admitted = p
	admitted.Candidates = slices.Clone(p.Candidates)
	for i := range admitted.Candidates {
		c := &admitted.Candidates[i]
		if c.ProjectResolution != nil {
			// Bound again below for the narrowed plan; the confirmed plan's
			// resolutions, which bindRecoveryPolicy rewrites for every
			// candidate, are left as they were.
			resolution := *c.ProjectResolution
			c.ProjectResolution = &resolution
		}
		if stale[i] {
			c.Skip = SkipSourceChanged
			c.Diagnostic = candidateDiagnostic(c.Skip, "", "")
		}
	}
	if droppedNestsWithCapture(p, admitted) {
		return Plan{}, 0, errRecoveryChanged
	}
	if err := bindRecoveryPolicy(*p.policyConfig, &admitted); err != nil {
		return Plan{}, 0, err
	}
	return admitted, len(stale), nil
}

// droppedNestsWithCapture reports whether a project only the left-out
// sessions needed nests with a plain folder the admitted plan still adds.
// The confirmed plan decided what to keep out of each added plain folder
// with the projects it adds as its own (planNested skips their roots, and
// capturedAnyway counts them); without the dropped project, the plain folder
// would capture the dropped project's folder, which neither the person
// confirmed nor the plan checked. Such an import is refused whole, as before
// partial admission, so the next plan decides it again.
func droppedNestsWithCapture(confirmed, admitted Plan) bool {
	added := admitted.Projects()
	kept := map[string]bool{}
	for _, s := range added {
		kept[s.Root] = true
	}
	var dropped []string
	for _, s := range confirmed.Projects() {
		if !kept[s.Root] && !s.Included {
			dropped = append(dropped, s.Root)
		}
	}
	for _, s := range added {
		if s.Included || !capturesSubfolders(s.Kind) {
			continue
		}
		for _, root := range dropped {
			if local.PathWithin(root, s.Root) || local.PathWithin(s.Root, root) {
				return true
			}
		}
	}
	return false
}
