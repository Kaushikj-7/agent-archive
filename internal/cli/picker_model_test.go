package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/state"
)

type modelArchive struct {
	items   []catalog.PickerItem
	facts   map[pickerIdentity]catalog.PickerItem
	err     error
	lookups int
}

func (a *modelArchive) PickerPage(ctx context.Context, q catalog.PickerQuery, next string) (catalog.PickerPage, error) {
	if ctx.Err() != nil {
		return catalog.PickerPage{}, ctx.Err()
	}
	if a.err != nil {
		return catalog.PickerPage{}, a.err
	}
	n, _ := strconv.Atoi(next)
	end := min(n+q.Limit, len(a.items))
	p := catalog.PickerPage{Rows: a.items[n:end], More: end < len(a.items), Total: uint64(len(a.items)), Empty: len(a.items) == 0}
	if p.More {
		p.Next = strconv.Itoa(end)
	}
	return p, nil
}

func (a *modelArchive) PickerLookup(ctx context.Context, ids []catalog.PickerIdentity) (catalog.PickerIdentityBatch, error) {
	if ctx.Err() != nil {
		return catalog.PickerIdentityBatch{}, ctx.Err()
	}
	a.lookups += len(ids)
	b := catalog.PickerIdentityBatch{}
	for _, id := range ids {
		item, ok := a.facts[pickerID(id.Harness, id.SessionID)]
		b.Results = append(b.Results, catalog.PickerIdentityResult{Identity: id, Found: ok, Item: item})
	}
	return b, nil
}

func (a *modelArchive) HydratePicker(context.Context, catalog.PickerReference) (catalog.PickerHydration, error) {
	return catalog.PickerHydration{}, errors.New("fake hydration")
}

func modelIDs(v modelPickerView) []string {
	out := []string{}
	for _, r := range v.Rows {
		out = append(out, r.row.metadata.Harness.Name+"/"+r.row.metadata.SessionID)
	}
	return out
}

func TestPickerModelAllPagesEqualExhaustiveUnion(t *testing.T) {
	for _, source := range []string{"auto", "local", "archive"} {
		for _, delta := range []time.Duration{-3 * time.Hour, 0, 3 * time.Hour} {
			t.Run(source+"/"+delta.String(), func(t *testing.T) {
				f := newPickerFixture(t)
				archived, err := loadSessionsForBrowse(f.env, f.mem, listOptions{}, io.Discard, "handoff")
				if err != nil {
					t.Fatal(err)
				}
				regs, err := state.OpenReadOnly(f.home).LoadRegistrations()
				if err != nil {
					t.Fatal(err)
				}
				a := &modelArchive{facts: map[pickerIdentity]catalog.PickerItem{}}
				for _, md := range archived {
					if md.IsChild() || md.IsReplay() {
						continue
					}
					key, _ := archive.MetadataObjectKey(md.Harness.Name, md.SessionID)
					promptState := catalog.PickerPromptsUnknown
					if archivedWithoutPrompt(md) {
						promptState = catalog.PickerPromptsZero
					}
					r := catalog.PickerRow{SessionKey: key, SessionID: md.SessionID, Harness: md.Harness.Name, Activity: lastActivity(md), CapturedAt: md.CapturedAt, Root: true, Title: md.Title, ProjectID: md.ProjectID, PromptState: promptState}
					item := catalog.PickerItem{Row: r}
					a.facts[pickerID(md.Harness.Name, md.SessionID)] = item
					if r.PromptState != catalog.PickerPromptsZero {
						a.items = append(a.items, item)
					}
				}
				// Position 51/101 association must come from exact lookup rather than page.
				for i := range 110 {
					md := archived[0]
					md.SessionID = fmt.Sprintf("remote-%03d", i)
					md.CapturedAt = md.CapturedAt.Add(-time.Duration(i+1) * time.Minute)
					archived = append(archived, md)
					key, _ := archive.MetadataObjectKey(md.Harness.Name, md.SessionID)
					a.items = append(a.items, catalog.PickerItem{Row: catalog.PickerRow{SessionKey: key, SessionID: md.SessionID, Harness: md.Harness.Name, Activity: lastActivity(md), CapturedAt: md.CapturedAt, Root: true}})
				}
				sortByActivity(archived)
				sort.SliceStable(a.items, func(i, j int) bool {
					return pickerCandidateBefore(pickerCandidate{item: &a.items[i], row: handoffPickerRow{active: a.items[i].Row.Activity}}, pickerCandidate{item: &a.items[j], row: handoffPickerRow{active: a.items[j].Row.Activity}})
				})
				at := f.env.now().Add(delta)
				if err := os.Chtimes(filepath.Join(f.project, "codex.jsonl"), at, at); err != nil {
					t.Fatal(err)
				}
				activity := pickerActivities(t.Context(), f.env, regs)
				p := handoffPicker{ctx: t.Context(), env: f.env, home: f.home, source: source, archiveRead: true}
				want, _, _ := p.rows(regs, archived, 1000)
				wantIDs := []string{}
				for _, r := range want {
					wantIDs = append(wantIDs, r.metadata.Harness.Name+"/"+r.metadata.SessionID)
				}
				m := &pickerModel{archive: a, regs: regs, activity: activity, source: source, pageSize: 7, labelBudget: 50, label: func(ctx context.Context, r archive.SessionRegistration, at time.Time) (archive.Metadata, bool, error) {
					labelPicker := p
					labelPicker.ctx = ctx
					return labelPicker.buildLocalMetadataResult(r, at)
				}}
				if err = m.prepare(t.Context()); err != nil {
					t.Fatal(err)
				}
				v, err := m.First(t.Context(), sessionScope{All: true})
				if err != nil {
					t.Fatal(err)
				}
				for v.More {
					v, err = m.Older(t.Context(), sessionScope{All: true})
					if err != nil {
						t.Fatal(err)
					}
				}
				if got := modelIDs(v); !reflect.DeepEqual(got, wantIDs) {
					t.Fatalf("union got %v want %v", got, wantIDs)
				}
			})
		}
	}
}

func TestPickerModelAttemptBudgetCannotProveEmpty(t *testing.T) {
	a := &modelArchive{facts: map[pickerIdentity]catalog.PickerItem{}}
	m := &pickerModel{archive: a, pageSize: 50, labelBudget: 50, activity: map[pickerIdentity]time.Time{}, label: func(_ context.Context, r archive.SessionRegistration, _ time.Time) (archive.Metadata, bool, error) {
		return archive.Metadata{SessionID: r.ArchiveSessionID, Harness: r.Harness}, r.ArchiveSessionID == "local-100", nil
	}}
	for i := range 101 {
		r := archive.SessionRegistration{ArchiveSessionID: fmt.Sprintf("local-%03d", i), Harness: archive.Harness{Name: "codex"}}
		m.regs = append(m.regs, r)
		m.activity[pickerID("codex", r.ArchiveSessionID)] = time.Unix(int64(1000-i), 0)
	}
	if err := m.prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	v, err := m.First(t.Context(), sessionScope{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Rows) != 0 || !v.Incomplete || v.Empty || v.LabelAttempts != 50 {
		t.Fatalf("false emptiness/prefix: %+v", v)
	}
	v, err = m.Older(t.Context(), sessionScope{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if !v.Incomplete || v.LabelAttempts != 100 {
		t.Fatalf("continuation %+v", v)
	}
	v, err = m.Older(t.Context(), sessionScope{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if v.More || len(v.Rows) != 1 || v.LabelAttempts != 101 {
		t.Fatalf("final %+v", v)
	}
}

func TestPickerModelErrorAtomicScopeAndIdentity(t *testing.T) {
	a := &modelArchive{facts: map[pickerIdentity]catalog.PickerItem{}}
	for i := range 4 {
		a.items = append(a.items, catalog.PickerItem{Row: catalog.PickerRow{SessionKey: strconv.Itoa(i), SessionID: strconv.Itoa(i), Harness: "codex", Activity: time.Unix(int64(10-i), 0)}})
	}
	m := &pickerModel{archive: a, pageSize: 2, labelBudget: 50, source: "archive"}
	if err := m.prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	s := sessionScope{All: true}
	first, err := m.First(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	a.err = errors.New("failed Older")
	if _, err = m.Older(t.Context(), s); err == nil {
		t.Fatal("missing failure")
	}
	cached, err := m.First(t.Context(), s)
	if err != nil || !reflect.DeepEqual(modelIDs(first), modelIDs(cached)) {
		t.Fatal("failed page consumed prefix")
	}
	a.err = nil
	v, err := m.Older(t.Context(), s)
	if err != nil || len(v.Rows) != 4 {
		t.Fatalf("retry %+v %v", v, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = m.Older(ctx, s); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestPickerModelHarnessIdentityAndCachePolicy(t *testing.T) {
	active := time.Unix(100, 0)
	a := archive.SessionRegistration{ArchiveSessionID: "shared", Harness: archive.Harness{Name: "codex"}}
	b := a
	b.Harness.Name = "claude"
	if pickerID(a.Harness.Name, a.ArchiveSessionID) == pickerID(b.Harness.Name, b.ArchiveSessionID) {
		t.Fatal("cross-harness identity")
	}
	ka, kb := pickerLocalLabelKey(a, active, "auto"), pickerLocalLabelKey(b, active, "auto")
	if ka == kb {
		t.Fatal("cross-harness label cache")
	}
	b = a
	b.TranscriptPath = "changed"
	if ka == pickerLocalLabelKey(b, active, "auto") {
		t.Fatal("registration rewrite reused labels")
	}
	if ka == pickerLocalLabelKey(a, active, "local") {
		t.Fatal("policy cache reused")
	}
}

func TestPickerModelBackwardOverrideSuppressesPastPage101(t *testing.T) {
	a := &modelArchive{facts: map[pickerIdentity]catalog.PickerItem{}}
	for i := range 130 {
		r := catalog.PickerRow{SessionKey: fmt.Sprintf("sessions/codex/%03d/metadata.json", i), SessionID: fmt.Sprintf("id-%03d", i), Harness: "codex", Root: true, Activity: time.Unix(int64(10000-i), 0), CapturedAt: time.Unix(int64(20000-i), 0)}
		item := catalog.PickerItem{Row: r}
		a.items = append(a.items, item)
		a.facts[pickerID(r.Harness, r.SessionID)] = item
	}
	regs := []archive.SessionRegistration{{ArchiveSessionID: "id-000", Harness: archive.Harness{Name: "codex"}}, {ArchiveSessionID: "id-101", Harness: archive.Harness{Name: "codex"}}}
	activity := map[pickerIdentity]time.Time{pickerID("codex", "id-000"): time.Unix(1, 0), pickerID("codex", "id-101"): time.Unix(20000, 0)}
	m := &pickerModel{archive: a, regs: regs, activity: activity, source: "archive", pageSize: 50, labelBudget: 50}
	if err := m.prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	v, err := m.First(t.Context(), sessionScope{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if v.Rows[0].row.metadata.SessionID != "id-101" || v.Rows[1].row.metadata.SessionID != "id-001" || v.Rows[49].row.metadata.SessionID != "id-049" {
		t.Fatalf("wrong exact first page %v", modelIDs(v))
	}
	for v.More {
		v, err = m.Older(t.Context(), sessionScope{All: true})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(v.Rows) != 130 || v.Rows[129].row.metadata.SessionID != "id-000" || v.Suppressed != 2 {
		t.Fatalf("backward override reappeared: rows=%d suppression=%d last=%s", len(v.Rows), v.Suppressed, v.Rows[len(v.Rows)-1].row.metadata.SessionID)
	}
}

func TestPickerModelEqualActivityKeepsArchiveProvenance(t *testing.T) {
	a := &modelArchive{facts: map[pickerIdentity]catalog.PickerItem{}}
	regs := []archive.SessionRegistration{}
	activity := map[pickerIdentity]time.Time{}
	for i, id := range []string{"newer-archive", "older-archive", "local-first", "local-second"} {
		r := archive.SessionRegistration{ArchiveSessionID: id, Harness: archive.Harness{Name: "codex"}}
		regs = append(regs, r)
		activity[pickerID("codex", id)] = time.Unix(100, 0)
		if i < 2 {
			item := catalog.PickerItem{Row: catalog.PickerRow{SessionKey: id, SessionID: id, Harness: "codex", Root: true, Activity: time.Unix(int64(20-i), 0), CapturedAt: time.Unix(int64(i), 0)}}
			a.items = append(a.items, item)
			a.facts[pickerID("codex", id)] = item
		}
	}
	m := &pickerModel{archive: a, regs: regs, activity: activity, source: "auto", pageSize: 2, labelBudget: 50, label: func(_ context.Context, r archive.SessionRegistration, at time.Time) (archive.Metadata, bool, error) {
		return archive.Metadata{SessionID: r.ArchiveSessionID, Harness: r.Harness, CapturedAt: at}, true, nil
	}}
	if err := m.prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	v, err := m.First(t.Context(), sessionScope{All: true})
	if err != nil {
		t.Fatal(err)
	}
	for v.More {
		v, err = m.Older(t.Context(), sessionScope{All: true})
		if err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"codex/newer-archive", "codex/older-archive", "codex/local-first", "codex/local-second"}
	if !reflect.DeepEqual(modelIDs(v), want) {
		t.Fatal(modelIDs(v))
	}
}

func TestPickerModelRegistrationScopePrecedence(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "child")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	key, other := archive.RepoKey("https://example.test/repo.git"), archive.RepoKey("https://example.test/other.git")
	a := &modelArchive{facts: map[pickerIdentity]catalog.PickerItem{}}
	m := &pickerModel{archive: a, source: "local", pageSize: 50, labelBudget: 50, activity: map[pickerIdentity]time.Time{}}
	for i, reg := range []archive.SessionRegistration{{RepoKey: key, ProjectRoot: "/different"}, {RepoKey: other, ProjectRoot: root}, {ProjectRoot: root}, {ProjectID: "project"}, {ProjectRoot: "/different", ProjectID: "project"}} {
		reg.ArchiveSessionID = fmt.Sprintf("scope-%d", i)
		reg.Harness.Name = "codex"
		m.regs = append(m.regs, reg)
		m.activity[pickerID("codex", reg.ArchiveSessionID)] = time.Unix(100, 0)
		item := catalog.PickerItem{Row: catalog.PickerRow{SessionKey: reg.ArchiveSessionID, SessionID: reg.ArchiveSessionID, Harness: "codex", Root: true, RepoKey: other, ProjectID: "project", Activity: time.Unix(50, 0)}}
		a.facts[pickerID("codex", reg.ArchiveSessionID)] = item
	}
	if err := m.prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, s := range []sessionScope{{Label: "repo", Dir: dir, RepoKey: key, ProjectIDs: []string{"project"}}, {Label: "directory", Dir: dir, ProjectIDs: []string{"project"}}, {All: true}} {
		v, err := m.First(t.Context(), s)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]bool{}
		for _, c := range v.Rows {
			got[c.row.metadata.SessionID] = true
		}
		for i := range m.regs {
			reg := &m.regs[i]
			md := pickerPresentation(a.facts[pickerID("codex", reg.ArchiveSessionID)].Row)
			if want := s.contains(md, reg); got[reg.ArchiveSessionID] != want {
				t.Fatalf("scope %+v reg %+v got %v want %v", s, reg, got[reg.ArchiveSessionID], want)
			}
		}
	}
	if _, err := m.First(t.Context(), sessionScope{Label: "Named"}); !errors.Is(err, catalog.ErrPickerUnsupported) {
		t.Fatal("named scope did not retain compatibility", err)
	}
}

func TestPickerModelZeroPromptRevivalSourcePolicy(t *testing.T) {
	for _, source := range []string{"auto", "local", "archive"} {
		t.Run(source, func(t *testing.T) {
			reg := archive.SessionRegistration{ArchiveSessionID: "revive", Harness: archive.Harness{Name: "codex"}}
			item := catalog.PickerItem{Row: catalog.PickerRow{SessionKey: "physical", SessionID: "revive", Harness: "codex", Root: true, PromptState: catalog.PickerPromptsZero, Activity: time.Unix(10, 0), CapturedAt: time.Unix(20, 0)}}
			a := &modelArchive{facts: map[pickerIdentity]catalog.PickerItem{pickerID("codex", "revive"): item}}
			m := &pickerModel{archive: a, regs: []archive.SessionRegistration{reg}, activity: map[pickerIdentity]time.Time{pickerID("codex", "revive"): time.Unix(30, 0)}, source: source, pageSize: 50, labelBudget: 50, label: func(_ context.Context, r archive.SessionRegistration, at time.Time) (archive.Metadata, bool, error) {
				return archive.Metadata{SessionID: r.ArchiveSessionID, Harness: r.Harness, CapturedAt: at}, true, nil
			}}
			if err := m.prepare(t.Context()); err != nil {
				t.Fatal(err)
			}
			v, err := m.First(t.Context(), sessionScope{All: true})
			if err != nil {
				t.Fatal(err)
			}
			if source == "archive" {
				if !v.Empty || v.LabelAttempts != 0 {
					t.Fatal(v)
				}
			} else {
				if len(v.Rows) != 1 || v.Rows[0].item.Row.CapturedAt != item.Row.CapturedAt || v.Rows[0].row.notUploaded || v.LabelAttempts != 1 {
					t.Fatal(v)
				}
			}
		})
	}
}

func TestPickerActivitiesSharedIDStaysHarnessQualified(t *testing.T) {
	f := newHandoffFixture(t, false)
	base := f.env.now()
	id := f.addSession(t, "claude", "different-agent", "Human prompt", base.Add(2*time.Hour))
	regs, err := state.OpenReadOnly(f.home).LoadRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	for i := range regs {
		if regs[i].ArchiveSessionID == id {
			regs[i].ArchiveSessionID = f.id
		}
	}
	activities := pickerActivities(t.Context(), f.env, regs)
	codex, ok := activities[pickerID("codex", f.id)]
	if !ok {
		t.Fatal("missing codex activity")
	}
	claude, ok := activities[pickerID("claude", f.id)]
	if !ok || !claude.Equal(base.Add(2*time.Hour)) || codex.Equal(claude) {
		t.Fatalf("cross harness overwrite: %v", activities)
	}
}

func TestPickerModelScopeContinuationsAndRefreshActivity(t *testing.T) {
	a := &modelArchive{facts: map[pickerIdentity]catalog.PickerItem{}}
	regs := []archive.SessionRegistration{}
	active := time.Unix(100, 0)
	for i := range 5 {
		regs = append(regs, archive.SessionRegistration{ArchiveSessionID: fmt.Sprintf("refresh-%d", i), Harness: archive.Harness{Name: "codex"}, ProjectRoot: "/repo"})
	}
	reads := 0
	m := &pickerModel{archive: a, regs: regs, source: "local", pageSize: 2, labelBudget: 50, observeActivity: func(_ context.Context, regs []archive.SessionRegistration) (map[pickerIdentity]time.Time, error) {
		out := map[pickerIdentity]time.Time{}
		for i, r := range regs {
			out[pickerID("codex", r.ArchiveSessionID)] = active.Add(-time.Duration(i) * time.Second)
		}
		return out, nil
	}, label: func(_ context.Context, r archive.SessionRegistration, at time.Time) (archive.Metadata, bool, error) {
		reads++
		return archive.Metadata{SessionID: r.ArchiveSessionID, Harness: r.Harness, CapturedAt: at}, true, nil
	}}
	if err := m.prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	scoped := sessionScope{Label: "repo", Dir: "/repo/sub", ProjectIDs: []string{"project"}}
	v, err := m.First(t.Context(), scoped)
	if err != nil || len(v.Rows) != 2 {
		t.Fatal(v, err)
	}
	v, err = m.Older(t.Context(), scoped)
	if err != nil || len(v.Rows) != 4 {
		t.Fatal(v, err)
	}
	all, err := m.First(t.Context(), scoped.everything())
	if err != nil || len(all.Rows) != 2 {
		t.Fatal("global borrowed scoped cursor", all, err)
	}
	v, err = m.First(t.Context(), scoped)
	if err != nil || len(v.Rows) != 4 || reads != 5 {
		t.Fatalf("toggle reread or lost prefix reads=%d view=%+v err=%v", reads, v, err)
	}
	active = active.Add(time.Hour)
	v, err = m.Refresh(t.Context(), scoped, a)
	if err != nil || len(v.Rows) != 2 || reads != 8 || !v.Rows[0].row.active.Equal(active) {
		t.Fatalf("refresh failed to observe activity/read new safe labels: %+v %v reads=%d", v, err, reads)
	}
}

func TestPickerModelRawSessionPrefixTieOrder(t *testing.T) {
	active := time.Unix(100, 0)
	candidates := []pickerCandidate{}
	for _, id := range []string{"a-b", "a"} {
		key, _ := archive.MetadataObjectKey("codex", id)
		item := catalog.PickerItem{Row: catalog.PickerRow{SessionKey: key, SessionID: id, Harness: "codex", Activity: active, CapturedAt: active}}
		candidates = append(candidates, pickerCandidate{item: &item, row: handoffPickerRow{active: active}})
	}
	sort.Slice(candidates, func(i, j int) bool { return pickerCandidateBefore(candidates[i], candidates[j]) })
	if candidates[0].item.Row.SessionID != "a" {
		t.Fatal("physical path slash replaced raw session tie order")
	}
}

func TestPickerModelMoreRequiresEligibleUnsuppressedLookahead(t *testing.T) {
	key, other := archive.RepoKey("https://example.test/repo"), archive.RepoKey("https://example.test/other")
	a := &modelArchive{facts: map[pickerIdentity]catalog.PickerItem{}}
	for i := range 3 {
		id := strconv.Itoa(i)
		item := catalog.PickerItem{Row: catalog.PickerRow{SessionKey: id, SessionID: id, Harness: "codex", Root: true, RepoKey: key, Activity: time.Unix(int64(10-i), 0)}}
		a.items = append(a.items, item)
		a.facts[pickerID("codex", id)] = item
	}
	reg := archive.SessionRegistration{ArchiveSessionID: "2", Harness: archive.Harness{Name: "codex"}, RepoKey: other}
	m := &pickerModel{archive: a, regs: []archive.SessionRegistration{reg}, source: "archive", pageSize: 2, labelBudget: 50, activity: map[pickerIdentity]time.Time{pickerID("codex", "2"): time.Unix(50, 0)}}
	if err := m.prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	v, err := m.First(t.Context(), sessionScope{Label: "repo", Dir: "/repo", RepoKey: key})
	if err != nil {
		t.Fatal(err)
	}
	if v.More || v.Incomplete || len(v.Rows) != 2 || v.Total != 2 || v.Suppressed != 1 {
		t.Fatalf("suppressed lookahead was reported as eligible More: %+v", v)
	}
}

// validatingModelArchive exercises operations that finish from cached candidates.
type validatingModelArchive struct {
	*modelArchive
	validationErr error
	readErr       error
}

func (a *validatingModelArchive) ValidateRead(context.Context) error {
	return a.readErr
}

func (a *validatingModelArchive) ValidateContinuation(context.Context) error {
	return a.validationErr
}

func TestPickerModelCachedOlderRejectsChangedAuthorityAtomically(t *testing.T) {
	for _, source := range []string{"local", "auto"} {
		t.Run(source, func(t *testing.T) {
			a := &validatingModelArchive{modelArchive: &modelArchive{facts: map[pickerIdentity]catalog.PickerItem{}, items: []catalog.PickerItem{{Row: catalog.PickerRow{SessionKey: "remote", SessionID: "remote", Harness: "codex", Activity: time.Unix(1, 0)}}}}}
			regs := []archive.SessionRegistration{}
			activity := map[pickerIdentity]time.Time{}
			for i := range 4 {
				id := strconv.Itoa(i)
				reg := archive.SessionRegistration{ArchiveSessionID: id, Harness: archive.Harness{Name: "codex"}}
				regs = append(regs, reg)
				activity[pickerID("codex", id)] = time.Unix(int64(10-i), 0)
			}
			m := &pickerModel{archive: a, regs: regs, activity: activity, source: source, pageSize: 2, labelBudget: 50, label: func(_ context.Context, reg archive.SessionRegistration, at time.Time) (archive.Metadata, bool, error) {
				return archive.Metadata{SessionID: reg.ArchiveSessionID, Harness: reg.Harness, CapturedAt: at}, true, nil
			}}
			if err := m.prepare(t.Context()); err != nil {
				t.Fatal(err)
			}
			scope := sessionScope{All: true}
			first, err := m.First(t.Context(), scope)
			if err != nil {
				t.Fatal(err)
			}
			a.validationErr = catalog.ErrStaleCursor
			if _, err = m.Older(t.Context(), scope); !errors.Is(err, catalog.ErrStaleCursor) {
				t.Fatalf("cached continuation accepted changed head: %v", err)
			}
			a.validationErr = nil
			a.readErr = catalog.ErrStaleCursor
			if _, err = m.Older(t.Context(), scope); !errors.Is(err, catalog.ErrStaleCursor) {
				t.Fatalf("cached continuation accepted expired lifetime: %v", err)
			}
			a.readErr = nil
			unchanged, err := m.First(t.Context(), scope)
			if err != nil || !reflect.DeepEqual(modelIDs(first), modelIDs(unchanged)) {
				t.Fatalf("failed Older consumed prefix: %v %v", modelIDs(unchanged), err)
			}
			last, err := m.Older(t.Context(), scope)
			if err != nil || len(last.Rows) != 4 {
				t.Fatalf("retry failed: %+v %v", last, err)
			}
		})
	}
}

func TestPickerModelOfflineAutoCannotProveCombinedScopeEmpty(t *testing.T) {
	for _, source := range []string{"auto", "local"} {
		t.Run(source, func(t *testing.T) {
			m := &pickerModel{absenceUnknown: true, source: source, pageSize: 50, labelBudget: 50}
			if err := m.prepare(t.Context()); err != nil {
				t.Fatal(err)
			}
			v, err := m.First(t.Context(), sessionScope{All: true})
			if err != nil {
				t.Fatal(err)
			}
			if source == "local" {
				if !v.Empty || v.Incomplete || v.Total != 0 {
					t.Fatal(v)
				}
			} else {
				if v.Empty || !v.Incomplete || v.Total != -1 || v.More {
					t.Fatal(v)
				}
				fresh := &modelArchive{facts: map[pickerIdentity]catalog.PickerItem{}}
				v, err = m.Refresh(t.Context(), sessionScope{All: true}, fresh)
				if err != nil || !v.Empty || v.Incomplete || v.Total != 0 || m.absenceUnknown {
					t.Fatalf("fresh complete view retained offline uncertainty: %+v %v", v, err)
				}
			}
		})
	}
}

func TestPickerModelLabelCancellationDoesNotConsumeCandidate(t *testing.T) {
	reg := archive.SessionRegistration{ArchiveSessionID: "retry", Harness: archive.Harness{Name: "codex"}}
	m := &pickerModel{source: "local", regs: []archive.SessionRegistration{reg}, activity: map[pickerIdentity]time.Time{pickerID("codex", "retry"): time.Unix(100, 0)}, pageSize: 50, labelBudget: 50, label: func(context.Context, archive.SessionRegistration, time.Time) (archive.Metadata, bool, error) {
		return archive.Metadata{}, false, context.Canceled
	}}
	if err := m.prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	scope := sessionScope{All: true}
	if _, err := m.First(t.Context(), scope); !errors.Is(err, context.Canceled) || len(m.labels) != 0 {
		t.Fatalf("cancellation consumed or cached candidate: %v labels=%v", err, m.labels)
	}
	m.label = func(context.Context, archive.SessionRegistration, time.Time) (archive.Metadata, bool, error) {
		return archive.Metadata{SessionID: reg.ArchiveSessionID, Harness: reg.Harness}, true, nil
	}
	v, err := m.First(t.Context(), scope)
	if err != nil || len(v.Rows) != 1 || v.Empty || v.LabelAttempts != 1 {
		t.Fatalf("canceled first page was not retryable: %+v %v", v, err)
	}
}
