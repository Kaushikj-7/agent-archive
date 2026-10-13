package cli

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
)

// pickerArchive keeps all classification and selection operations in one view.
type pickerArchive interface {
	PickerPage(context.Context, catalog.PickerQuery, string) (catalog.PickerPage, error)
	PickerLookup(context.Context, []catalog.PickerIdentity) (catalog.PickerIdentityBatch, error)
	HydratePicker(context.Context, catalog.PickerReference) (catalog.PickerHydration, error)
}
type pickerIdentity struct{ harness, session string }

func pickerID(h, s string) pickerIdentity { return pickerIdentity{archive.CanonicalHarness(h), s} }

type pickerCandidate struct {
	row     handoffPickerRow
	item    *catalog.PickerItem
	ordinal int
}

// modelPickerView distinguishes an exact prefix from work awaiting continuation.
// Incomplete never proves emptiness, even when Rows is empty.
type modelPickerView struct {
	Rows                                   []pickerCandidate
	More, Incomplete, Empty                bool
	Total                                  int
	LabelAttempts, RemotePages, Suppressed int
}
type pickerScopeState struct {
	query               catalog.PickerQuery
	locals              []pickerCandidate
	localPos            int
	buffer              []catalog.PickerItem
	next                string
	started, remoteDone bool
	suppress            map[pickerIdentity]bool
	emitted             map[string]bool
	view                modelPickerView
}
type pickerModel struct {
	observeActivity       func(context.Context, []archive.SessionRegistration) (map[pickerIdentity]time.Time, error)
	absenceUnknown        bool
	searchError           func() error
	archive               pickerArchive
	regs                  []archive.SessionRegistration
	facts                 map[pickerIdentity]*catalog.PickerItem
	activity              map[pickerIdentity]time.Time
	labels                map[localLabelKey]localLabelResult
	label                 func(context.Context, archive.SessionRegistration, time.Time) (archive.Metadata, bool)
	source, harness       string
	pageSize, labelBudget int
	states                map[string]*pickerScopeState
}

func pickerScopeQuery(s sessionScope, harness string, limit int) (catalog.PickerQuery, error) {
	if s.narrowed() && s.Dir == "" {
		return catalog.PickerQuery{}, catalog.ErrPickerUnsupported
	}
	scope := catalog.PickerScope{All: !s.narrowed()}
	if !scope.All {
		scope.RepoKey, scope.ProjectIDs = s.RepoKey, slices.Clone(s.ProjectIDs)
	}
	return catalog.PickerQuery{Scope: scope, Membership: "handoff", Harness: harness, Limit: limit}, nil
}

// prepare classifies every relevant registration exactly, including uploaded
// copies outside the first page and zero-prompt/child/replay copies.
func (m *pickerModel) prepare(ctx context.Context) error {
	if m.pageSize == 0 {
		m.pageSize = defaultListLimit
	}
	if m.labelBudget == 0 {
		m.labelBudget = defaultListLimit
	}
	if m.pageSize < 1 || m.pageSize > 100 || m.labelBudget < 0 {
		return catalog.ErrPickerUnsupported
	}
	m.facts = map[pickerIdentity]*catalog.PickerItem{}
	ids := []catalog.PickerIdentity{}
	seen := map[pickerIdentity]bool{}
	for _, reg := range m.regs {
		id := pickerID(reg.Harness.Name, reg.ArchiveSessionID)
		if !topLevelRegistration(reg) || reg.Replay != nil || (m.harness != "" && id.harness != archive.CanonicalHarness(m.harness)) || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, catalog.PickerIdentity{Harness: id.harness, SessionID: id.session})
	}
	if m.archive == nil {
		for _, id := range ids {
			m.facts[pickerID(id.Harness, id.SessionID)] = nil
		}
		ids = nil
	}
	for len(ids) > 0 {
		n := min(100, len(ids))
		batch, err := m.archive.PickerLookup(ctx, ids[:n])
		if err != nil {
			return err
		}
		for _, r := range batch.Results {
			id := pickerID(r.Identity.Harness, r.Identity.SessionID)
			if r.Found {
				item := r.Item
				m.facts[id] = &item
			} else {
				m.facts[id] = nil
			}
		}
		ids = ids[n:]
	}
	if m.observeActivity != nil {
		var err error
		m.activity, err = m.observeActivity(ctx, m.regs)
		if err != nil {
			return err
		}
	}
	if m.labels == nil {
		m.labels = map[localLabelKey]localLabelResult{}
	}
	m.states = map[string]*pickerScopeState{}
	return ctx.Err()
}
func pickerPresentation(r catalog.PickerRow) archive.Metadata {
	m := archive.Metadata{SessionID: r.SessionID, Harness: archive.Harness{Name: r.Harness}, ProjectID: r.ProjectID, RepoKey: r.RepoKey, ProjectName: r.ProjectName, CapturedAt: r.CapturedAt, Name: r.Name, Title: r.Title}
	if r.PromptCount != nil {
		n := int(*r.PromptCount)
		m.Counts.Turns = &n
	}
	return m
}
func pickerCandidateBefore(a, b pickerCandidate) bool {
	if !a.row.active.Equal(b.row.active) {
		return a.row.active.After(b.row.active)
	}
	if a.item != nil && b.item == nil {
		return true
	}
	if a.item == nil && b.item != nil {
		return false
	}
	if a.item != nil {
		ar, br := a.item.Row, b.item.Row
		if !ar.Activity.Equal(br.Activity) {
			return ar.Activity.After(br.Activity)
		}
		if !ar.CapturedAt.Equal(br.CapturedAt) {
			return ar.CapturedAt.After(br.CapturedAt)
		}
		return ar.SessionKey < br.SessionKey
	}
	return a.ordinal < b.ordinal
}
func (m *pickerModel) state(s sessionScope) (*pickerScopeState, error) {
	q, err := pickerScopeQuery(s, m.harness, m.pageSize)
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(s)
	key := string(raw)
	if st := m.states[key]; st != nil {
		return st, nil
	}
	st := &pickerScopeState{query: q, suppress: map[pickerIdentity]bool{}, emitted: map[string]bool{}, view: modelPickerView{Total: -1}}
	// Last registration wins, matching the existing registration join, while
	// canonical identities keep another harness with the same ID independent.
	latest := map[pickerIdentity]int{}
	for i, r := range m.regs {
		latest[pickerID(r.Harness.Name, r.ArchiveSessionID)] = i
	}
	for i := range m.regs {
		reg := &m.regs[i]
		id := pickerID(reg.Harness.Name, reg.ArchiveSessionID)
		item, classified := m.facts[id]
		if !classified || latest[id] != i {
			continue
		}
		md := registrationMetadata(*reg)
		if item != nil {
			md = pickerPresentation(item.Row)
		}
		inScope := s.contains(md, reg)
		active, readable := m.activity[id]
		// Membership conflicts suppress the archived copy even without native activity.
		if item != nil && (!inScope || readable || m.source == "local") {
			st.suppress[id] = true
		}
		if !inScope || !readable {
			continue
		}
		if item != nil && (!item.Row.Root || item.Row.Replay) {
			continue
		}
		c := pickerCandidate{row: handoffPickerRow{metadata: md, active: active, registered: true, reg: reg}, item: item, ordinal: i}
		if item == nil || item.Row.PromptState == "zero" {
			if m.source == "archive" {
				continue
			}
			c.row.unbuilt = reg
			c.row.notUploaded = item == nil && !m.absenceUnknown
		}
		st.locals = append(st.locals, c)
	}
	sort.SliceStable(st.locals, func(i, j int) bool { return pickerCandidateBefore(st.locals[i], st.locals[j]) })
	if m.source == "local" {
		st.remoteDone = true
	}
	m.states[key] = st
	return st, nil
}
func (m *pickerModel) First(ctx context.Context, s sessionScope) (modelPickerView, error) {
	st, err := m.state(s)
	if err != nil {
		return modelPickerView{}, err
	}
	if st.started {
		if ctx.Err() != nil {
			return modelPickerView{}, ctx.Err()
		}
		if validator, ok := m.archive.(interface{ ValidateContinuation(context.Context) error }); ok {
			if err := validator.ValidateContinuation(ctx); err != nil {
				return modelPickerView{}, err
			}
		}
		return clonePickerView(st.view), nil
	}
	return m.Older(ctx, s)
}
func clonePickerView(v modelPickerView) modelPickerView { v.Rows = slices.Clone(v.Rows); return v }
func (m *pickerModel) Older(ctx context.Context, s sessionScope) (modelPickerView, error) {
	st, err := m.state(s)
	if err != nil {
		return modelPickerView{}, err
	}
	// Work only in a copy: errors must not consume cursors or a visible prefix.
	priorEmitted := st.emitted
	copyState := *st
	st = &copyState
	st.buffer = slices.Clone(st.buffer)
	st.view = clonePickerView(st.view)
	st.emitted = map[string]bool{}
	for k, v := range priorEmitted {
		st.emitted[k] = v
	}
	st.started = true
	st.view.Incomplete = false
	target := len(st.view.Rows) + m.pageSize
	attempts, pages := 0, 0
	// Suppression refill has a finite additive registration-dependent bound.
	pageBudget := 2 + (len(st.suppress)+m.pageSize-1)/m.pageSize
	for len(st.view.Rows) < target {
		if err = ctx.Err(); err != nil {
			return modelPickerView{}, err
		}
		for len(st.buffer) == 0 && !st.remoteDone {
			if pages >= pageBudget {
				st.view.Incomplete = true
				break
			}
			page, e := m.archive.PickerPage(ctx, st.query, st.next)
			if e != nil {
				return modelPickerView{}, e
			}
			pages++
			st.view.RemotePages++
			st.buffer = page.Rows
			st.next = page.Next
			st.remoteDone = !page.More
			for len(st.buffer) > 0 && st.suppress[pickerID(st.buffer[0].Row.Harness, st.buffer[0].Row.SessionID)] {
				st.buffer = st.buffer[1:]
				st.view.Suppressed++
			}
		}
		for len(st.buffer) > 0 && st.suppress[pickerID(st.buffer[0].Row.Harness, st.buffer[0].Row.SessionID)] {
			st.buffer = st.buffer[1:]
			st.view.Suppressed++
		}
		if st.view.Incomplete {
			break
		}
		if len(st.buffer) == 0 && !st.remoteDone {
			continue
		}
		var remote pickerCandidate
		if len(st.buffer) > 0 {
			item := st.buffer[0]
			remote = pickerCandidate{row: handoffPickerRow{metadata: pickerPresentation(item.Row), active: item.Row.Activity}, item: &item}
		}
		local := st.localPos < len(st.locals) && (len(st.buffer) == 0 || pickerCandidateBefore(st.locals[st.localPos], remote))
		if !local && len(st.buffer) == 0 {
			break
		}
		c := remote
		if local {
			c = st.locals[st.localPos]
			if c.row.unbuilt != nil {
				reg := *c.row.unbuilt
				key := pickerLocalLabelKey(reg, c.row.active, m.source)
				result, cached := m.labels[key]
				if !cached {
					if attempts >= m.labelBudget {
						st.view.Incomplete = true
						break
					}
					attempts++
					st.view.LabelAttempts++
					result.metadata, result.hasPrompt = m.label(ctx, reg, c.row.active)
					if err = ctx.Err(); err != nil {
						return modelPickerView{}, err
					}
					m.labels[key] = result
				}
				st.localPos++
				if !result.hasPrompt {
					continue
				}
				c.row.metadata = result.metadata
				c.row.unbuilt = nil
			} else {
				st.localPos++
			}
		} else {
			st.buffer = st.buffer[1:]
		}
		key := c.row.metadata.Harness.Name + "\x00" + c.row.metadata.SessionID
		if c.item != nil {
			key = c.item.Row.SessionKey
		}
		if st.emitted[key] {
			continue
		}
		st.emitted[key] = true
		st.view.Rows = append(st.view.Rows, c)
	}
	st.view.More = st.view.Incomplete || st.localPos < len(st.locals) || len(st.buffer) > 0 || !st.remoteDone
	st.view.Empty = len(st.view.Rows) == 0 && !st.view.More
	if !st.view.More {
		st.view.Total = len(st.view.Rows)
	}
	raw, _ := json.Marshal(s)
	m.states[string(raw)] = st
	return clonePickerView(st.view), nil
}

// Refresh requires a new captured archive view supplied by the caller. Reusing
// an expired snapshot cannot extend its lifetime or manufacture fresh references.
func (m *pickerModel) Refresh(ctx context.Context, s sessionScope, fresh pickerArchive) (modelPickerView, error) {
	candidate := *m
	candidate.archive = fresh
	if err := candidate.prepare(ctx); err != nil {
		return modelPickerView{}, err
	}
	view, err := candidate.First(ctx, s)
	if err != nil {
		return modelPickerView{}, err
	}
	*m = candidate
	return view, nil
}
func pickerLocalLabelKey(reg archive.SessionRegistration, active time.Time, source string) localLabelKey {
	raw, _ := json.Marshal(reg)
	return localLabelKey{sessionID: reg.ArchiveSessionID, harness: archive.CanonicalHarness(reg.Harness.Name), active: active, registration: string(raw), policy: source}
}

// Select preserves the opaque authority supplied with the offered candidate.
func (m *pickerModel) Select(ctx context.Context, item catalog.PickerItem) (catalog.PickerHydration, error) {
	return m.archive.HydratePicker(ctx, item.Reference)
}
