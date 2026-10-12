package cli

import (
	"context"
	"io"
	"math"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// notUploadedHint marks a picker row for a session the archive does not have
// yet. It is drawn where list draws the first skill: dim, after the title.
const notUploadedHint = " · not yet uploaded"

// handoffSelectDependencies is what the handoff picker reads: the archive,
// and each local session's source for its activity and first prompt.
type handoffSelectDependencies interface {
	readOnlyStoreDependencies
	sessionSelectionDependencies
	sessionBrowserDependencies
	cursorDatabase() string
}

// chooseHandoffSession fills in the session a handoff with no selector hands
// off: with --to, the agent session the command runs in; otherwise, or when
// there is none, the picker on a terminal. done is set when the command should
// exit with code instead.
func chooseHandoffSession(opts *handoffOptions, home string, interactive bool, in *typedInput, stdout, stderr io.Writer, env handoffCommandDependencies) (code int, done bool) {
	if opts.to != "" {
		id, ok, err := currentHandoffSession(env, home, *opts)
		if err != nil {
			terminal.Printf(stderr, "agent-archive: handoff: %v\n", err)
			return 1, true
		}
		switch {
		case ok:
			terminal.Printf(stderr, "handoff: using the session this command runs in, %s\n", id)
			opts.sessionID = id
			return 0, false
		case projectRuntime(env, opts.harness) != "":
			opts.latest, opts.harness = true, projectRuntime(env, opts.harness)
			return 0, false
		case !interactive:
			env.newCommandFlags("handoff", stderr).usageError("%s", noCurrentSessionMessage)
			return 2, true
		}
	}
	id, harness, selected, code := selectHandoffSession(env, home, *opts, in, stdout, stderr)
	if code != 0 || !selected {
		return code, true
	}
	opts.sessionID, opts.harness = id, harness
	return 0, false
}

// currentHandoffSession finds the registered session named by the calling
// agent's exact runtime observation. ok is false when no variable
// is set or none names a session registered on this machine.
func currentHandoffSession(env currentSessionDependencies, home string, opts handoffOptions) (sessionID string, ok bool, err error) {
	var regs []archive.SessionRegistration
	loaded := false
	for _, observation := range runtimeObservations(env) {
		value := observation.NativeID
		if value == "" || (opts.harness != "" && opts.harness != string(observation.Agent)) {
			continue
		}
		if !loaded {
			if regs, err = state.OpenReadOnly(home).LoadRegistrations(); err != nil {
				return "", false, err
			}
			loaded = true
		}
		// A resumed session can be registered more than once; the newest
		// registration is the one running.
		var found *archive.SessionRegistration
		for i, reg := range regs {
			if topLevelRegistration(reg) && reg.NativeSessionID == value && archive.CanonicalHarness(reg.Harness.Name) == string(observation.Agent) &&
				(found == nil || reg.RegisteredAt.After(found.RegisteredAt)) {
				found = &regs[i]
			}
		}
		if found != nil {
			return found.ArchiveSessionID, true, nil
		}
	}
	return "", false, nil
}

func topLevelRegistration(reg archive.SessionRegistration) bool {
	return !reg.IsChild() && reg.SubagentID == ""
}

// selectHandoffSession is handoff's one-shot picker. It lists this machine's
// sessions, including ones not uploaded yet, with the archive's, newest
// activity first. An archive that cannot be read leaves the local ones.
// selected is false when nothing matches or the user quits.
func selectHandoffSession(env handoffSelectDependencies, home string, opts handoffOptions, in *typedInput, stdout, stderr io.Writer) (sessionID, harness string, selected bool, code int) {
	store, cfg, found, err := handoffPickerStore(env, opts)
	if !found {
		if err != nil {
			terminal.Printf(stderr, "agent-archive: handoff: %v\n", err)
		} else {
			terminal.Println(stderr, notSetUpMessage)
		}
		return "", "", false, 1
	}
	regs, regErr := state.OpenReadOnly(home).LoadRegistrations()
	if regErr != nil {
		terminal.Printf(stderr, "agent-archive: handoff: %v\n", regErr)
		return "", "", false, 1
	}
	scope, scopeErr := scopeFor(env, opts.project, opts.allProjects)
	if scopeErr != nil {
		terminal.Printf(stderr, "agent-archive: handoff: %v\n", scopeErr)
		return "", "", false, 1
	}
	stop := startActivity(stdout, "Finding sessions…")
	var archived []archive.Metadata
	if err == nil {
		archived, err = loadSessionsForBrowse(env, store, listOptions{filter: reader.Filter{Harness: opts.harness, Replays: reader.ReplaysHidden}}, stderr, "handoff")
	}
	picker := handoffPicker{localLabels: make(map[localLabelKey]localLabelResult), ctx: context.Background(), env: env, home: home, harness: opts.harness, source: opts.source, archiveRead: err == nil}
	format := listFormatOptions{Now: env.now(), Projects: projectLabels(cfg), Style: styleFor(stdout), GroupByProject: true, Numbered: true, DimID: true, Children: childCounts(archived),
		NarrowHint: "Narrow with --harness, or name a session: agent-archive handoff SESSION_ID."}
	choices := pagedHandoffChoices(scope, format, picker, regs, archived)
	stop()
	if err != nil {
		terminal.Printf(stderr, "agent-archive: handoff: note: the archive could not be read, so only this machine's sessions are listed: %v\n", err)
	}
	if len(choices.shown().rows) == 0 {
		terminal.Println(stdout, "No sessions match.")
		return "", "", false, 0
	}
	row, selected, code := runBrowser(context.Background(), env, in.prompter(stdout), stdout, stderr, browserSpec{Mode: pickSession, Verb: "Hand off", Choices: choices, Command: "handoff"})
	if code != 0 {
		return "", "", false, code
	}
	return row.SessionID, row.HarnessKey, selected, 0
}

// pagedHandoffChoices exposes older rows only after an explicit request. The
// archive universe and search remain complete; local labels retain their existing
// initial bound and are read farther only when the visible table expands.
func pagedHandoffChoices(scope sessionScope, format listFormatOptions, picker handoffPicker, regs []archive.SessionRegistration, archived []archive.Metadata) *scopeChoices {
	limit := defaultListLimit
	choices := newScopeChoices(scope, format, false, func(s sessionScope) scopeView {
		picker.scope = s
		rows, total, truncated := picker.rows(regs, archived, limit)
		return scopeView{rows: formatHandoffRows(rows, format), total: total, truncated: truncated, hidden: handoffHiddenSubagents(picker, s, archived),
			search: handoffSearch(picker, s, regs, archived, format)}
	})
	choices.loadOlder = &browserLoadAction{Label: "Older sessions", Available: func() bool {
		return choices.shown().truncated
	}, Load: func() (bool, error) {
		limit += defaultListLimit
		choices.built = [2]*scopeChoice{}
		return choices.shown().truncated, nil
	}}
	return choices
}

// handoffSearch lists the sessions of a scope for the filter to search: every
// top-level session the picker could offer (not only the first screens),
// numbered as the picker numbers them, then the archive's subagents, which
// have no number. A session not uploaded yet is searched only within the
// picker's limit, as a title search reads them (handoffTitleScanLimit): its
// title takes reading and filtering its transcript, which the first key
// typed into the filter must not wait for every one of.
func handoffSearch(picker handoffPicker, scope sessionScope, regs []archive.SessionRegistration, archived []archive.Metadata, format listFormatOptions) func() []listRow {
	picker.scope, picker.readLimit = scope, handoffTitleScanLimit
	return func() []listRow {
		rows, _, _ := picker.rows(regs, archived, math.MaxInt)
		out := formatHandoffRows(rows, format)
		if picker.source == "local" {
			return out
		}
		subagents := slices.DeleteFunc(subagentSessions(archived), func(m archive.Metadata) bool { return !scope.contains(m, nil) })
		children := formatSessionRows(subagents, format)
		for i := range children {
			children[i].Index = 0
		}
		return append(out, children...)
	}
}

// handoffHiddenSubagents is how many of the archive's subagent sessions in a
// scope the picker's rows leave out, for the footer to name as list's does.
// They are none with --source local, whose search does not offer them.
func handoffHiddenSubagents(picker handoffPicker, scope sessionScope, archived []archive.Metadata) int {
	if picker.source == "local" {
		return 0
	}
	hidden := 0
	for _, m := range subagentSessions(archived) {
		if scope.contains(m, nil) {
			hidden++
		}
	}
	return hidden
}

// handoffPickerRow is one session the handoff picker offers.
type handoffPickerRow struct {
	metadata archive.Metadata
	active   time.Time
	// registered is set for a session registered on this machine.
	registered bool
	// unbuilt holds a registration the archive lacks until its metadata is
	// built from the transcript.
	unbuilt *archive.SessionRegistration
	// notUploaded is set when the archive was read and lacks the session.
	notUploaded bool
	// reg is the session's registration on this machine, when it has one.
	reg *archive.SessionRegistration
	// noPrompt is set for an archived session whose metadata holds no prompt.
	noPrompt bool
}

// archivedWithoutPrompt reports whether an archived session holds no prompt,
// so has nothing to hand off: its metadata counts no human prompt
// (counts.turns, which a text transcript or a failed parse leaves unknown).
func archivedWithoutPrompt(m archive.Metadata) bool {
	return m.Counts.Turns != nil && *m.Counts.Turns == 0
}

type localLabelKey struct {
	sessionID string
	active    time.Time
}

type localLabelResult struct {
	metadata  archive.Metadata
	hasPrompt bool
}

type handoffPicker struct {
	// Shared by picker scope/search views; only small safe row facts are cached.
	localLabels map[localLabelKey]localLabelResult
	ctx         context.Context
	env         handoffSelectDependencies
	home        string
	harness     string
	source      string
	// archiveRead is false when the archive could not be listed, so a
	// session missing from it is not known to be missing.
	archiveRead bool
	// scope limits the rows to one repository's or project's sessions; the
	// zero value offers every session.
	scope sessionScope
	// readLimit, when set, bounds the rows a session the archive lacks may
	// be read into (its transcript read to title it): past it, such a
	// session is left out, as a limit would leave it out. 0 for no bound.
	readLimit int
}

// rows merges registrations with archived sessions, joined on the archive
// session ID, and returns the first limit by most recent activity. A
// registered session's activity is its source's, newer than the archive's
// copy, read for all registrations at once (collector.LastActivities opens
// the Cursor database once). Only a registration the archive lacks has its
// transcript read, and only while rows are still needed, to title it and pass
// over one with no prompt yet. An archived session with no prompt is passed
// over too, unless it is registered here and its transcript, read as one the
// archive lacks is, has one by now. Subagents, archived or registered, are
// never offered. total counts the sessions that can be offered, or is -1 when
// some past the limit were not read to tell; truncated is set when any are
// left out.
func (p handoffPicker) rows(regs []archive.SessionRegistration, archived []archive.Metadata, limit int) (rows []handoffPickerRow, total int, truncated bool) {
	regs = slices.DeleteFunc(slices.Clone(regs), func(reg archive.SessionRegistration) bool {
		return !topLevelRegistration(reg) || reg.Replay != nil || (p.harness != "" && archive.CanonicalHarness(reg.Harness.Name) != p.harness)
	})
	registered := make(map[string]*archive.SessionRegistration, len(regs))
	for i := range regs {
		registered[regs[i].ArchiveSessionID] = &regs[i]
	}
	all := make([]handoffPickerRow, 0, len(archived)+len(regs))
	index := map[string]int{}
	// uploaded holds every archived session, in scope or not: a registered
	// session the archive has is not one it lacks.
	uploaded := map[string]bool{}
	for _, m := range topLevelSessions(archived) {
		if m.IsReplay() {
			continue
		}
		uploaded[m.SessionID] = true
		if !p.scope.contains(m, registered[m.SessionID]) {
			continue
		}
		index[m.SessionID] = len(all)
		all = append(all, handoffPickerRow{metadata: m, active: lastActivity(m), noPrompt: archivedWithoutPrompt(m)})
	}
	// A copy, which leaves registered pointing at the registrations as they were.
	regs = slices.DeleteFunc(slices.Clone(regs), func(reg archive.SessionRegistration) bool {
		if uploaded[reg.ArchiveSessionID] {
			_, inScope := index[reg.ArchiveSessionID]
			return !inScope
		}
		return !p.scope.contains(registrationMetadata(reg), &reg)
	})
	activity := collector.LastActivities(p.ctx, regs, p.env.cursorDatabase(), registryFor(p.env))
	for _, reg := range regs {
		active, ok := activity[reg.ArchiveSessionID]
		if !ok {
			// No transcript (yet, or any more): nothing to hand off.
			continue
		}
		if i, found := index[reg.ArchiveSessionID]; found {
			all[i].active, all[i].registered, all[i].reg = active, true, &reg
			if all[i].noPrompt && p.source != "archive" {
				// Archived before its first prompt: the transcript may hold one now.
				all[i].unbuilt = &reg
			}
			continue
		}
		if p.source != "archive" {
			all = append(all, handoffPickerRow{active: active, registered: true, reg: &reg, unbuilt: &reg, notUploaded: p.archiveRead})
		}
	}
	all = slices.DeleteFunc(all, func(row handoffPickerRow) bool {
		return (p.source == "local" && !row.registered) || (row.noPrompt && row.unbuilt == nil)
	})
	sort.SliceStable(all, func(i, j int) bool { return all[i].active.After(all[j].active) })
	rows = make([]handoffPickerRow, 0, min(limit, len(all)))
	for i, row := range all {
		if len(rows) == limit {
			// Every archived session left over can be offered; one not
			// yet uploaded may have no prompt, and was not read to see.
			rest := all[i:]
			if slices.ContainsFunc(rest, func(row handoffPickerRow) bool { return row.unbuilt != nil }) {
				return rows, -1, true
			}
			return rows, len(rows) + len(rest), true
		}
		if row.unbuilt != nil {
			if p.readLimit > 0 && len(rows) >= p.readLimit {
				continue
			}
			metadata, ok := p.localMetadata(*row.unbuilt, row.active)
			if !ok {
				continue
			}
			row.metadata, row.unbuilt = metadata, nil
		}
		rows = append(rows, row)
	}
	return rows, len(rows), false
}

// localMetadata is the part of a session's metadata the picker shows, built
// from its transcript as it is now. ok is false when the transcript cannot be
// read or holds no prompt yet.
func (p handoffPicker) localMetadata(reg archive.SessionRegistration, active time.Time) (archive.Metadata, bool) {
	key := localLabelKey{sessionID: reg.ArchiveSessionID, active: active}
	if result, ok := p.localLabels[key]; ok {
		return result.metadata, result.hasPrompt
	}
	metadata, ok := p.buildLocalMetadata(reg, active)
	if p.localLabels != nil {
		p.localLabels[key] = localLabelResult{metadata: metadata, hasPrompt: ok}
	}
	return metadata, ok
}

func (p handoffPicker) buildLocalMetadata(reg archive.SessionRegistration, active time.Time) (archive.Metadata, bool) {
	bundle, err := collector.ReadLocalBundle(p.ctx, p.home, reg, p.env.now().UTC(), p.env.cursorDatabase(), registryFor(p.env))
	if err != nil {
		return archive.Metadata{}, false
	}
	analysis, err := analyzeSource(p.ctx, parsersFor(p.env), bundle)
	if err != nil {
		return archive.Metadata{}, false
	}
	labels, ok := archive.LabelsFromAnalysis(analysis)
	if !ok {
		return archive.Metadata{}, false
	}
	project := ""
	if reg.ProjectRoot != "" {
		project = filepath.Base(filepath.Clean(reg.ProjectRoot))
	}
	return archive.Metadata{SessionID: reg.ArchiveSessionID, NativeSessionID: reg.NativeSessionID, ProjectID: reg.ProjectID,
		ProjectName: project, Harness: reg.Harness, CapturedAt: active, Name: labels.Name, Title: labels.Title,
		Branch: labels.Branch, PullRequests: labels.PullRequests, Origin: reg.Origin, RepoKey: reg.RepoKey}, true
}

// registrationMetadata is the part of a registered session's metadata that
// says which project it belongs to, for a scope to decide on before its
// transcript is read.
func registrationMetadata(reg archive.SessionRegistration) archive.Metadata {
	name := ""
	if reg.ProjectRoot != "" {
		name = filepath.Base(filepath.Clean(reg.ProjectRoot))
	}
	return archive.Metadata{ProjectID: reg.ProjectID, RepoKey: reg.RepoKey, ProjectName: name}
}

// activeNow reports whether a session was last active within
// activeSourceWindow of now: the test that makes handoff ask about a shared
// checkout, and puts a dot on the row.
func activeNow(now, active time.Time) bool {
	return !active.IsZero() && now.Sub(active).Abs() <= activeSourceWindow
}

// formatHandoffRows is formatSessionRows with each row's time taken from its
// latest activity, a mark on sessions the archive does not have yet, and a
// dot on those active now.
func formatHandoffRows(rows []handoffPickerRow, format listFormatOptions) []listRow {
	sessions := make([]archive.Metadata, len(rows))
	for i, row := range rows {
		sessions[i] = row.metadata
	}
	out := formatSessionRows(sessions, format)
	for i, row := range rows {
		out[i].When = relativeAge(format.Now, row.active)
		if row.notUploaded {
			out[i].SkillHint = notUploadedHint
		}
		out[i].Live = row.registered && activeNow(format.Now, row.active)
	}
	return out
}

func handoffPickerStore(env readOnlyStoreDependencies, opts handoffOptions) (storage.ObjectStore, config.Config, bool, error) {
	if opts.config == nil {
		return openReadOnlyStore(env)
	}
	cfg, found := opts.config.cfg, opts.config.found
	if !found {
		return nil, cfg, false, nil
	}
	store, err := env.openStore(cfg)
	return store, cfg, true, err
}
