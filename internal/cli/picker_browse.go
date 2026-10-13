package cli

import (
	"context"
	"errors"
	"io"
	"math"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// boundedArchiveChoices keeps a separate remote frontier for each displayed
// scope. It never initializes the complete summary cache before search.
func boundedArchiveChoices(ctx context.Context, env metadataCacheDependencies, store storage.ObjectStore, opts listOptions, scope sessionScope, format listFormatOptions, stderr io.Writer, command string) (*scopeChoices, bool, error) {
	if !reader.CatalogAuthority(store) || opts.imported || opts.hookCaptured || opts.verbose || opts.filter.Model != "" || opts.filter.Skill != "" || opts.filter.SkillSHA256 != "" || opts.filter.SkillUsage != "" || !opts.filter.From.IsZero() || !opts.filter.To.IsZero() || opts.filter.RequireCompleteCoverage || opts.filter.Replays != reader.ReplaysHidden {
		return nil, false, nil
	}
	pageSize := defaultListLimit
	if command == "list" {
		if opts.limit < 1 || opts.limit > 100 {
			return nil, false, nil
		}
		pageSize = opts.limit
	}
	q, err := pickerScopeQuery(scope, opts.filter.Harness, pageSize)
	if err != nil {
		return nil, false, nil
	}
	q.Membership = "browse"
	snapshot, err := catalog.OpenSnapshot(ctx, store, nil)
	if err != nil {
		return nil, true, err
	}
	page, err := snapshot.PickerPage(ctx, q, "")
	if errors.Is(err, catalog.ErrPickerUnsupported) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	type state struct {
		rows  []catalog.PickerItem
		next  string
		more  bool
		total int
	}
	states := map[int]*state{}
	index := viewScope
	if scope.All || scope.Label == "" {
		index = viewAll
	}
	count := func(p catalog.PickerPage) int {
		if p.Total > uint64(math.MaxInt) {
			return -1
		}
		return int(p.Total)
	}
	states[index] = &state{rows: page.Rows, next: page.Next, more: page.More, total: count(page)}
	choices := &scopeChoices{ctx: ctx, scope: scope.only(), format: format, current: index}
	var loadErr error
	load := func(readCtx context.Context, s sessionScope, i int, older bool) error {
		st := states[i]
		if st != nil && !older {
			return snapshot.ValidateContinuation(readCtx)
		}
		query, e := pickerScopeQuery(s, opts.filter.Harness, pageSize)
		if e != nil {
			return e
		}
		query.Membership = "browse"
		next := ""
		if st != nil {
			next = st.next
		}
		p, e := snapshot.PickerPage(readCtx, query, next)
		if e != nil {
			return e
		}
		if st == nil {
			st = &state{}
		}
		st.rows = append(st.rows, p.Rows...)
		st.next, st.more, st.total = p.Next, p.More, count(p)
		states[i] = st
		return nil
	}
	choices.rowsForContext = func(readCtx context.Context, s sessionScope) scopeView {
		i := viewScope
		if s.All {
			i = viewAll
		}
		if e := load(readCtx, s, i, false); e != nil {
			loadErr = e
			return scopeView{incomplete: true, total: -1, note: e.Error()}
		}
		st := states[i]
		rows := make([]listRow, 0, len(st.rows))
		for _, item := range st.rows {
			// Only bounded display facts are formatted here. Full matcher fields are
			// supplied by the complete search loader, never by this projection.
			item := item
			row := formatSessionRows([]archive.Metadata{pickerPresentation(item.Row)}, format)[0]
			row.Index = len(rows) + 1
			row.Children = int(item.Row.OrdinaryChildren)
			row.pickerSelection = &handoffPickerSelection{item: &item, snapshot: snapshot, store: store}
			rows = append(rows, row)
		}
		return scopeView{rows: rows, total: st.total, truncated: st.more, hidden: -1, note: "Subagent count is not loaded; search includes archived subagents.", searchContext: func(searchCtx context.Context) ([]listRow, error) {
			sessions, e := loadSessionsForBrowseContext(searchCtx, env, store, opts, stderr, command)
			if e != nil {
				return nil, e
			}
			result := archiveSearch(sessions, s, format)()
			for j := range result {
				batch, e := snapshot.PickerLookup(searchCtx, []catalog.PickerIdentity{{Harness: result[j].HarnessKey, SessionID: result[j].SessionID}})
				if e != nil {
					return nil, e
				}
				if len(batch.Results) != 1 || !batch.Results[0].Found {
					return nil, errors.New("search selection no longer exists; refresh")
				}
				item := batch.Results[0].Item
				result[j].pickerSelection = &handoffPickerSelection{item: &item, snapshot: snapshot, store: store}
			}
			return result, nil
		}}
	}
	_ = choices.shown()
	if loadErr != nil {
		return nil, true, loadErr
	}
	if index == viewScope && page.Empty {
		choices.current, choices.fellBack = viewAll, true
		_ = choices.shown()
	}
	choices.loadOlder = &browserLoadAction{Label: "Older sessions", Available: func() bool { return choices.shown().truncated }, LoadContext: func(readCtx context.Context) (bool, error) {
		i := choices.current
		s := choices.scope
		if i == viewAll {
			s = s.everything()
		}
		if e := load(readCtx, s, i, true); e != nil {
			return false, e
		}
		choices.built[i] = nil
		return states[i].more, nil
	}}
	choices.loadOlder.Load = func() (bool, error) { return choices.loadOlder.LoadContext(ctx) }
	return choices, true, loadErr
}

func readPickerSessionView(ctx context.Context, store storage.ObjectStore, row listRow) (sessionView, error) {
	if sel := row.pickerSelection; sel != nil && sel.item != nil {
		hydrated, err := sel.snapshot.HydratePicker(ctx, sel.item.Reference)
		if err != nil {
			return sessionView{}, err
		}
		metadata, ok, err := sel.snapshot.ResolvedMetadata(ctx, hydrated.Row)
		if err != nil {
			return sessionView{}, err
		}
		if !ok {
			return sessionView{}, errors.New("picker selection lacks verified body provenance")
		}
		return metadataWithLinks(ctx, store, metadata), nil
	}
	return readSessionView(ctx, store, row.HarnessKey, row.SessionID)
}
