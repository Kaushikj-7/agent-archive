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
	groups := map[string][]archive.SessionRegistration{}
	for _, r := range regs {
		if topLevelRegistration(r) && r.Replay == nil {
			h := archive.CanonicalHarness(r.Harness.Name)
			groups[h] = append(groups[h], r)
		}
	}
	out := map[pickerIdentity]time.Time{}
	for h, group := range groups {
		for id, at := range collector.LastActivities(ctx, group, env.cursorDatabase(), registryFor(env)) {
			out[pickerID(h, id)] = at
		}
	}
	return out
}

// boundedHandoffChoices is the real caller adapter. Unsupported formats and
// named scopes return used=false to retain the complete compatibility loader.
func boundedHandoffChoices(ctx context.Context, env handoffSelectDependencies, store storage.ObjectStore, home string, opts handoffOptions, scope sessionScope, regs []archive.SessionRegistration, format listFormatOptions, stderr io.Writer) (*scopeChoices, *pickerModel, *catalog.Snapshot, bool, error) {
	if !reader.CatalogAuthority(store) {
		return nil, nil, nil, false, nil
	}
	if _, err := pickerScopeQuery(scope, opts.harness, defaultListLimit); err != nil {
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
		return pickerActivities(ctx, env, regs), ctx.Err()
	}, source: opts.source, harness: opts.harness, pageSize: defaultListLimit, labelBudget: defaultListLimit, label: func(ctx context.Context, r archive.SessionRegistration, t time.Time) (archive.Metadata, bool) {
		p.ctx = ctx
		return p.buildLocalMetadata(r, t)
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
	choices := &scopeChoices{ctx: ctx, scope: scope.only(), format: format, validate: snapshot.ValidateContinuation}
	if scope.Label == "" || scope.All {
		choices.current = viewAll
	}
	searchState := &pickerSearchState{}
	choices.rowsForContext = func(readCtx context.Context, s sessionScope) scopeView {
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
			note = "Local eligibility is incomplete; Older sessions continues the scan."
		}
		return scopeView{rows: out, total: v.Total, truncated: v.More, note: note, incomplete: v.Incomplete, searchNotice: "Local-only search labels are limited to the first 50 picker rows.", searchContext: boundedHandoffSearch(env, store, snapshot, model, opts, s, regs, format, p, searchState, stderr)}
	}
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
func boundedLocalHandoffChoices(ctx context.Context, env handoffSelectDependencies, home string, opts handoffOptions, scope sessionScope, regs []archive.SessionRegistration, format listFormatOptions) (*scopeChoices, *pickerModel, error) {
	p := handoffPicker{ctx: ctx, env: env, home: home, harness: opts.harness, source: "local", localLabels: map[localLabelKey]localLabelResult{}}
	m := &pickerModel{absenceUnknown: true, regs: slices.Clone(regs), observeActivity: func(ctx context.Context, regs []archive.SessionRegistration) (map[pickerIdentity]time.Time, error) {
		return pickerActivities(ctx, env, regs), ctx.Err()
	}, source: "local", harness: opts.harness, pageSize: defaultListLimit, labelBudget: defaultListLimit, label: func(ctx context.Context, r archive.SessionRegistration, at time.Time) (archive.Metadata, bool) {
		p.ctx = ctx
		return p.buildLocalMetadata(r, at)
	}}
	if err := m.prepare(ctx); err != nil {
		return nil, nil, err
	}
	var loadErr error
	c := &scopeChoices{ctx: ctx, scope: scope.only(), format: format}
	if scope.All || scope.Label == "" {
		c.current = viewAll
	}
	c.rowsForContext = func(readCtx context.Context, s sessionScope) scopeView {
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
		note := "Archive association is unknown while offline."
		if v.Incomplete {
			note += " Local eligibility is incomplete; Older sessions continues the scan."
		}
		return scopeView{rows: out, total: v.Total, truncated: v.More, incomplete: v.Incomplete, note: note, searchNotice: "Local-only search labels are limited to the first 50 picker rows.", searchContext: func(searchCtx context.Context) ([]listRow, error) {
			p.ctx = searchCtx
			p.scope = s
			p.readLimit = handoffTitleScanLimit
			p.localLabels = m.labels
			rows, _, _ := p.rows(regs, nil, math.MaxInt)
			result := formatHandoffRows(rows, format)
			for i, row := range rows {
				result[i].pickerSelection = &handoffPickerSelection{model: m, reg: row.reg}
			}
			return result, searchCtx.Err()
		}}
	}
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
		p.ctx = ctx
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
		roots, _, _ := p.rows(regs, state.archived, math.MaxInt)
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
			selection := &handoffPickerSelection{model: model, snapshot: snapshot, store: store}
			if i < len(roots) {
				selection.reg = roots[i].reg
			}
			if result.Found {
				item := result.Item
				selection.item = &item
			} else if selection.reg == nil {
				return errors.New("search selection no longer exists; refresh")
			}
			rows[i].pickerSelection = selection
		}
	}
	return ctx.Err()
}
