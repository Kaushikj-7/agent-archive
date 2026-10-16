package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

type handoffPickerSelection struct {
	model    *pickerModel
	item     *catalog.PickerItem
	snapshot *catalog.Snapshot
	store    storage.ObjectStore
	reg      *archive.SessionRegistration
}

// pickerActivities retains native provider batching, but splits harnesses so
// an ArchiveSessionID shared by different agents cannot overwrite activity.
func pickerActivities(ctx context.Context, env handoffSelectDependencies, regs []archive.SessionRegistration) map[pickerIdentity]time.Time {
	groups := pickerActivityGroups(regs)
	out := map[pickerIdentity]time.Time{}
	for h, group := range groups {
		for id, at := range collector.LastActivities(ctx, group, env.cursorDatabase(), registryFor(env)) {
			out[pickerID(h, id)] = at
		}
	}
	return out
}

var errPickerActivityUnavailable = errors.New("local session activity is unavailable; retry")

func pickerActivitiesResult(ctx context.Context, env handoffSelectDependencies, regs []archive.SessionRegistration) (map[pickerIdentity]time.Time, error) {
	out := map[pickerIdentity]time.Time{}
	for harness, group := range pickerActivityGroups(regs) {
		observed, err := collector.LastActivitiesWithError(ctx, group, env.cursorDatabase(), registryFor(env))
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("%w: %w", errPickerActivityUnavailable, err)
		}
		for id, active := range observed {
			out[pickerID(harness, id)] = active
		}
	}
	return out, ctx.Err()
}

func pickerActivityGroups(regs []archive.SessionRegistration) map[string][]archive.SessionRegistration {
	groups := map[string][]archive.SessionRegistration{}
	for _, r := range regs {
		if topLevelRegistration(r) && r.Replay == nil {
			h := archive.CanonicalHarness(r.Harness.Name)
			groups[h] = append(groups[h], r)
		}
	}
	return groups
}

// handoffChoicesForStore keeps supported offline scopes on the bounded native
// model without interpreting an unavailable archive as an empty archive.
func handoffChoicesForStore(ctx context.Context, env handoffSelectDependencies, store storage.ObjectStore, home string, opts handoffOptions, scope sessionScope, regs []archive.SessionRegistration, format listFormatOptions, stderr io.Writer, archiveErr error) (*scopeChoices, *pickerModel, bool, error) {
	if archiveErr != nil {
		used := opts.source == "archive" || errors.Is(archiveErr, context.Canceled) || errors.Is(archiveErr, context.DeadlineExceeded) || supportsBoundedHandoffScope(scope, opts.harness)
		return nil, nil, used, archiveErr
	}
	choices, model, _, used, err := boundedHandoffChoices(ctx, env, store, home, opts, scope, regs, format, stderr)
	return choices, model, used, err
}

// boundedHandoffChoices is the real caller adapter. Unsupported formats and
// named scopes return used=false to retain the complete compatibility loader.
func boundedHandoffChoices(ctx context.Context, env handoffSelectDependencies, store storage.ObjectStore, home string, opts handoffOptions, scope sessionScope, regs []archive.SessionRegistration, format listFormatOptions, stderr io.Writer) (*scopeChoices, *pickerModel, *catalog.Snapshot, bool, error) {
	if !reader.CatalogAuthority(store) {
		return nil, nil, nil, false, nil
	}
	if !pickerScopeSupported(scope, opts.harness, defaultListLimit) {
		return nil, nil, nil, false, nil
	}
	snapshot, err := catalog.OpenSnapshot(ctx, store, nil)
	if err != nil {
		return nil, nil, nil, true, err
	}
	// Probe supported format before local inventory reads. Unsupported is a
	// compatibility decision, whereas an I/O/budget failure remains an error.
	// The model retains one extra remote lookahead. Cursor limits are sealed,
	// so the supported-format probe must use the same query as model.state.
	q, _ := pickerScopeQuery(scope, opts.harness, min(100, defaultListLimit+1))
	first, err := snapshot.PickerPage(ctx, q, "")
	if errors.Is(err, catalog.ErrPickerUnsupported) {
		return nil, nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, nil, true, fmt.Errorf("read first handoff page: %w", err)
	}
	p := handoffPicker{ctx: ctx, env: env, home: home, harness: opts.harness, source: opts.source, archiveRead: true}
	model := &pickerModel{archive: snapshot, regs: slices.Clone(regs), observeActivity: func(ctx context.Context, regs []archive.SessionRegistration) (map[pickerIdentity]time.Time, error) {
		return pickerActivitiesResult(ctx, env, regs)
	}, source: opts.source, harness: opts.harness, pageSize: defaultListLimit, labelBudget: defaultListLimit, label: func(ctx context.Context, r archive.SessionRegistration, t time.Time) (archive.Metadata, bool, error) {
		return p.withContext(ctx).buildLocalMetadataResult(r, t)
	}}
	if err = model.prepare(ctx); err != nil {
		return nil, nil, nil, true, fmt.Errorf("classify handoff registrations: %w", err)
	}
	state, err := model.state(scope)
	if err != nil {
		return nil, nil, nil, true, err
	}
	state.buffer, state.next, state.remoteDone = first.Rows, first.Next, !first.More
	state.view.RemotePages = 1
	var operationErr error
	searchState := &pickerSearchState{}
	rowsForContext := func(readCtx context.Context, s sessionScope) scopeView {
		v, e := pickerVisibleView(readCtx, model, s)
		if e != nil {
			operationErr = fmt.Errorf("build handoff picker rows: %w", e)
			return scopeView{incomplete: true, loadError: e, total: -1, truncated: true, note: fmt.Sprintf("Could not load sessions: %v", e)}
		}
		rows := make([]handoffPickerRow, len(v.Rows))
		for i, c := range v.Rows {
			rows[i] = c.row
		}
		out := formatHandoffRows(rows, format)
		for i, c := range v.Rows {
			out[i].pickerSelection = &handoffPickerSelection{model: model, item: c.item, snapshot: snapshot, store: store, reg: c.row.reg}
			if c.item != nil {
				out[i].Children = int(c.item.Row.OrdinaryChildren)
			}
		}
		note := ""
		if v.Incomplete {
			note = "Local eligibility is incomplete."
			if v.More {
				note += " Older sessions continues the scan."
			}
		}
		return scopeView{rows: out, total: v.Total, truncated: v.More, note: note, incomplete: v.Incomplete, searchNotice: "Complete archive search; local labels use at most 50 new native read attempts per search.", searchPartial: opts.source != "archive", searchContext: boundedHandoffSearch(env, store, snapshot, model, opts, s, regs, format, p, searchState, stderr)}
	}
	choices := &scopeChoices{ctx: ctx, scope: scope.only(), format: format, validate: snapshot.ValidateContinuation, current: pickerScopeIndex(scope), rowsForContext: rowsForContext}
	initial := choices.shown()
	if operationErr != nil {
		return nil, nil, nil, true, operationErr
	}
	// Only a proven complete empty union permits widening to all projects.
	if choices.current == viewScope && len(initial.rows) == 0 && !initial.incomplete {
		choices.current = viewAll
		choices.fellBack = true
		_ = choices.shown()
	}
	choices.loadOlder = &browserLoadAction{Label: "Older sessions", Available: func() bool { return choices.shown().truncated }, LoadContext: func(operationCtx context.Context) (bool, error) {
		s := choices.scope
		if choices.current == viewAll {
			s = s.everything()
		}
		if e := snapshot.ValidateContinuation(operationCtx); e != nil {
			return false, e
		}
		view, e := model.Older(operationCtx, s)
		if e != nil {
			return false, e
		}
		choices.built[choices.current] = nil
		return view.More, nil
	}}
	choices.loadOlder.Load = func() (bool, error) { return choices.loadOlder.LoadContext(ctx) }
	// Search failure belongs to the browser operation, rather than poisoning
	// valid browse selections after the filter is cancelled.
	model.searchError = func() error { return nil }
	return choices, model, snapshot, true, operationErr
}

func (r handoffResolver) selectedPicker(sel *handoffPickerSelection) (handoffTarget, error) {
	// An archived association is checked before any native/source read, even
	// when auto will ultimately prefer the readable current local transcript.
	var metadata archive.Metadata
	if sel.item != nil {
		hydrated, err := sel.model.Select(r.ctx, *sel.item)
		if err != nil {
			return handoffTarget{}, err
		}
		body, ok, err := sel.snapshot.ResolvedMetadata(r.ctx, hydrated.Row)
		if err != nil {
			return handoffTarget{}, err
		}
		if !ok {
			return handoffTarget{}, errors.New("picker selection lacks verified body provenance")
		}
		metadata = body
		if metadata.IsReplay() {
			return handoffTarget{}, errors.New("selected session is now a replay; refresh")
		}
	}
	if r.source != "archive" && sel.reg != nil {
		regs, err := state.OpenReadOnly(r.home).LoadRegistrations()
		if err != nil {
			return handoffTarget{}, err
		}
		id := pickerID(sel.reg.Harness.Name, sel.reg.ArchiveSessionID)
		for _, reg := range regs {
			if pickerID(reg.Harness.Name, reg.ArchiveSessionID) == id && topLevelRegistration(reg) && reg.Replay == nil {
				target, e := r.localTarget(reg)
				if e == nil && hasPrompt(target.analysis) {
					return target, nil
				}
				if e == nil {
					e = errors.New("selected local session no longer has a prompt")
				}
				if r.source == "local" {
					return handoffTarget{}, e
				}
				break
			}
		}
		if r.source == "local" {
			return handoffTarget{}, errNotRegisteredHere
		}
	}
	if r.source == "local" || sel.item == nil {
		return handoffTarget{}, errNotRegisteredHere
	}
	if archivedWithoutPrompt(metadata) {
		return handoffTarget{}, errors.New("selected archive copy has no prompt; the local session is no longer readable")
	}
	// No same-ID refresh on disappearing immutable source bytes.
	bundle, err := reader.LoadSource(r.ctx, sel.store, metadata, reader.Limits{})
	if err != nil {
		return handoffTarget{}, err
	}
	return analyzeTarget(r.ctx, parsersFor(r.env), handoffTarget{bundle: bundle, metadata: &metadata, source: "archive"})
}

// boundedLocalHandoffChoices preserves offline auto/local selection while
// withholding uploaded/absent claims when archive association is unknown.
func boundedLocalHandoffChoices(ctx context.Context, env handoffSelectDependencies, home string, opts handoffOptions, scope sessionScope, regs []archive.SessionRegistration, format listFormatOptions, _ io.Writer) (*scopeChoices, *pickerModel, error) {
	p := handoffPicker{ctx: ctx, env: env, home: home, harness: opts.harness, source: opts.source, localLabels: map[localLabelKey]localLabelResult{}}
	m := &pickerModel{absenceUnknown: true, regs: slices.Clone(regs), observeActivity: func(ctx context.Context, regs []archive.SessionRegistration) (map[pickerIdentity]time.Time, error) {
		return pickerActivitiesResult(ctx, env, regs)
	}, source: opts.source, harness: opts.harness, pageSize: defaultListLimit, labelBudget: defaultListLimit, label: func(ctx context.Context, r archive.SessionRegistration, at time.Time) (archive.Metadata, bool, error) {
		return p.withContext(ctx).buildLocalMetadataResult(r, at)
	}}
	if err := m.prepare(ctx); err != nil {
		return nil, nil, err
	}
	var loadErr error
	rowsForContext := func(readCtx context.Context, s sessionScope) scopeView {
		v, err := pickerVisibleView(readCtx, m, s)
		if err != nil {
			loadErr = err
			return scopeView{incomplete: true, loadError: err, total: -1, truncated: true}
		}
		rows := make([]handoffPickerRow, len(v.Rows))
		for i, r := range v.Rows {
			rows[i] = r.row
		}
		out := formatHandoffRows(rows, format)
		for i, r := range v.Rows {
			out[i].pickerSelection = &handoffPickerSelection{model: m, reg: r.row.reg}
		}
		note := ""
		if opts.source != "local" {
			note = "Archive association is unknown while offline."
		}
		if v.Incomplete && v.More {
			note += " Local eligibility is incomplete; Older sessions continues the scan."
		}
		return scopeView{rows: out, total: v.Total, truncated: v.More, incomplete: v.Incomplete, note: note, searchNotice: "Local labels use at most 50 new native read attempts per search; other local eligibility may be unknown.", searchPartial: true, searchContext: func(searchCtx context.Context) ([]listRow, error) {
			p := p.withContext(searchCtx)
			p.scope = s
			p.readLimit = handoffTitleScanLimit
			p.searchLabelBudget = handoffTitleScanLimit
			p.localLabels = m.labels
			var readErr error
			p.readError = &readErr
			rows, _, _ := p.rows(regs, nil, math.MaxInt)
			if readErr != nil {
				return nil, readErr
			}
			result := formatHandoffRows(rows, format)
			for i, row := range rows {
				result[i].pickerSelection = &handoffPickerSelection{model: m, reg: row.reg}
			}
			return result, searchCtx.Err()
		}}
	}
	c := &scopeChoices{ctx: ctx, scope: scope.only(), format: format, current: pickerScopeIndex(scope), rowsForContext: rowsForContext}
	initial := c.shown()
	if loadErr != nil {
		return nil, nil, loadErr
	}
	if c.current == viewScope && initial.holdsNothing() {
		c.current = viewAll
		c.fellBack = true
		_ = c.shown()
	}
	c.loadOlder = &browserLoadAction{Label: "Older sessions", Available: func() bool { return c.shown().truncated }, LoadContext: func(operationCtx context.Context) (bool, error) {
		s := c.scope
		if c.current == viewAll {
			s = s.everything()
		}
		view, err := m.Older(operationCtx, s)
		if err != nil {
			return false, err
		}
		c.built[c.current] = nil
		return view.More, nil
	}}
	c.loadOlder.Load = func() (bool, error) { return c.loadOlder.LoadContext(ctx) }
	m.searchError = func() error { return nil }
	return c, m, loadErr
}

// A rendered scope reuses the model's published immutable view. Validation
// belongs to the asynchronous scope/Older action, not table reconstruction.
func pickerVisibleView(ctx context.Context, m *pickerModel, scope sessionScope) (modelPickerView, error) {
	raw, _ := json.Marshal(scope)
	if st := m.states[string(raw)]; st != nil && st.started {
		return clonePickerView(st.view), ctx.Err()
	}
	return m.First(ctx, scope)
}

type pickerSearchState struct {
	archived []archive.Metadata
	loaded   bool
}

func boundedHandoffSearch(env handoffSelectDependencies, store storage.ObjectStore, snapshot *catalog.Snapshot, model *pickerModel, opts handoffOptions, scope sessionScope, regs []archive.SessionRegistration, format listFormatOptions, p handoffPicker, state *pickerSearchState, stderr io.Writer) func(context.Context) ([]listRow, error) {
	return func(ctx context.Context) ([]listRow, error) {
		p := p.withContext(ctx)
		if !state.loaded {
			archived, err := reader.ListMetadataWithOptions(ctx, store, archiveSessionsPrefix, reader.Filter{Harness: opts.harness, Replays: reader.ReplaysHidden}, reader.ListOptions{Cache: listCache(env, false), Skipped: warnSkippedSidecar(stderr, "handoff")})
			if err != nil {
				return nil, err
			}
			sortByActivity(archived)
			state.archived = archived
			state.loaded = true
		}
		p.scope = scope
		p.localLabels = model.labels
		p.readLimit = handoffTitleScanLimit
		p.searchLabelBudget = handoffTitleScanLimit
		var readErr error
		p.readError = &readErr
		roots, _, _ := p.rows(regs, state.archived, math.MaxInt)
		if readErr != nil {
			return nil, readErr
		}
		result := formatHandoffRows(roots, format)
		if opts.source != "local" {
			children := formatSessionRows(scope.filter(subagentSessions(state.archived)), format)
			for i := range children {
				children[i].Index = 0
			}
			result = append(result, children...)
		}
		if err := pinHandoffSearchRows(ctx, snapshot, model, store, result, roots); err != nil {
			return nil, err
		}
		return result, ctx.Err()
	}
}

func pinHandoffSearchRows(ctx context.Context, snapshot *catalog.Snapshot, model *pickerModel, store storage.ObjectStore, rows []listRow, roots []handoffPickerRow) error {
	for start := 0; start < len(rows); start += 100 {
		end := min(start+100, len(rows))
		ids := make([]catalog.PickerIdentity, end-start)
		for i := start; i < end; i++ {
			ids[i-start] = catalog.PickerIdentity{Harness: rows[i].HarnessKey, SessionID: rows[i].SessionID}
		}
		batch, err := snapshot.PickerLookup(ctx, ids)
		if err != nil {
			return err
		}
		if len(batch.Results) != len(ids) {
			return errors.New("search identity lookup is incomplete; retry")
		}
		for offset, result := range batch.Results {
			i := start + offset
			var reg *archive.SessionRegistration
			if i < len(roots) {
				reg = roots[i].reg
			}
			var item *catalog.PickerItem
			if result.Found {
				found := result.Item
				item = &found
			} else if reg == nil {
				return errors.New("search selection no longer exists; refresh")
			}
			selection := &handoffPickerSelection{model: model, snapshot: snapshot, store: store, reg: reg, item: item}
			rows[i].pickerSelection = selection
		}
	}
	return ctx.Err()
}

func pickerScopeSupported(scope sessionScope, harness string, limit int) bool {
	_, err := pickerScopeQuery(scope, harness, limit)
	return err == nil
}

func pickerScopeIndex(scope sessionScope) int {
	if scope.Label == "" || scope.All {
		return viewAll
	}
	return viewScope
}

func supportsBoundedHandoffScope(scope sessionScope, harness string) bool {
	_, err := pickerScopeQuery(scope, harness, defaultListLimit+1)
	return err == nil
}
