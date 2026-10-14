package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestRealCLIArchiveBrowsersStartWithoutFullArchiveReads(t *testing.T) {
	t.Parallel()
	a := newScopedArchive(t)
	for i := range 105 {
		a.addMetadata(t, fmt.Sprintf("browse-%03d", i), fmt.Sprintf("Title %03d", i), a.label)
	}
	// The identical list/show universe uses one real Commit/upgrade constructor.
	// Every command receives private mutable objects, providers and local caches.
	seed := scopedPickerCatalog(t, a.mem)
	for _, command := range []string{"list", "show"} {
		t.Run(command, func(t *testing.T) {
			t.Parallel()
			remote := cloneScopedFixtureCatalog(t, seed.ObjectStore)
			counts := &catalogListReads{MeasuredStore: storagetest.NewMeasuredStore(remote.ObjectStore, 0), paths: map[string]int{}}
			wrapped, err := catalog.Wrap(counts)
			if err != nil {
				t.Fatal(err)
			}
			env := scopedFixtureEnv(t, a)
			env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return wrapped, nil }
			input := strings.NewReader("q\n")
			var out, stderr bytes.Buffer
			env.IsTerminal = func(stream any) bool { return stream == any(input) || stream == any(&out) }
			if code := Run([]string{command}, input, &out, &stderr, env); code != 0 {
				t.Fatal(code, stderr.String())
			}
			if !strings.Contains(out.String(), "Older sessions") {
				t.Fatal("no remote Older action", out.String())
			}
			if counts.Metrics().Lists != 0 || counts.paths["body"] != 0 || counts.paths["other"] != 0 {
				t.Fatalf("first screen initialized complete archive: %+v %v", counts.Metrics(), counts.paths)
			}
		})
	}
}

func TestBoundedArchiveScopeContinuationsAreIndependentAndSelectionPinned(t *testing.T) {
	t.Parallel()
	a := newScopedArchive(t)
	for i := range 130 {
		project := a.label
		if i >= 60 {
			project = "elsewhere"
		}
		a.addMetadata(t, fmt.Sprintf("scope-%03d", i), fmt.Sprintf("Title %03d", i), project)
	}
	remote := scopedPickerCatalog(t, a.mem)
	scope := sessionScope{Label: a.label, Dir: a.dir, ProjectIDs: []string{archive.ProjectID(a.dir)}}
	choices, used, err := boundedArchiveChoices(catalog.WithReadView(t.Context()), a.env, remote, listOptions{filter: reader.Filter{Replays: reader.ReplaysHidden}}, scope, listFormatOptions{Numbered: true}, io.Discard, "show")
	if err != nil || !used {
		t.Fatal(used, err)
	}
	scoped := append([]listRow(nil), choices.shown().rows...)
	if len(scoped) != 50 {
		t.Fatal("scope prefix", len(scoped))
	}
	if _, err = choices.loadOlder.LoadContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(choices.shown().rows) != 61 {
		t.Fatal("scope continuation", len(choices.shown().rows))
	}
	choices.toggle()
	if len(choices.shown().rows) != 50 {
		t.Fatal("all scope inherited another frontier", len(choices.shown().rows))
	}
	if _, err = choices.loadOlder.LoadContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(choices.shown().rows) != 100 {
		t.Fatal("all continuation", len(choices.shown().rows))
	}
	choices.toggle()
	if len(choices.shown().rows) != 61 {
		t.Fatal("scope frontier lost")
	}
	for i, r := range scoped {
		if choices.shown().rows[i].SessionID != r.SessionID || choices.shown().rows[i].Index != r.Index {
			t.Fatal("prefix renumbered")
		}
	}
	row := choices.shown().rows[0]
	key, _ := archive.MetadataObjectKey(row.HarnessKey, row.SessionID)
	if err = remote.DeleteSession(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if _, err = readPickerSessionView(t.Context(), remote, row); err == nil {
		t.Fatal("pinned selected row silently substituted latest authority")
	}
}

func TestBoundedArchiveSearchLoadsOlderChildrenAndUncappedFieldsOnDemand(t *testing.T) {
	t.Parallel()
	a := newScopedArchive(t)
	for i := range 55 {
		a.addMetadata(t, fmt.Sprintf("ordinary-%03d", i), fmt.Sprintf("Title %03d", i), a.label)
	}
	a.addMetadata(t, "older-parent", "older parent", a.label, func(m *archive.Metadata) {
		at := a.base.CapturedAt.Add(-2 * time.Hour)
		m.EndedAt = &at
		m.CapturedAt = at
	})
	a.addMetadata(t, "older-child", "childneedle", a.label, func(m *archive.Metadata) {
		m.ParentSessionID = "older-parent"
		at := a.base.CapturedAt.Add(-2 * time.Hour)
		m.EndedAt = &at
		m.CapturedAt = at
	})
	a.addMetadata(t, "newest-long-title", strings.Repeat("uncapped ", 300)+"latefieldneedle", a.label, func(m *archive.Metadata) { at := a.base.CapturedAt.Add(time.Hour); m.EndedAt = &at; m.CapturedAt = at })
	remote := scopedPickerCatalog(t, a.mem)
	counts := &catalogListReads{MeasuredStore: storagetest.NewMeasuredStore(remote.ObjectStore, 0), paths: map[string]int{}}
	wrapped, err := catalog.Wrap(counts)
	if err != nil {
		t.Fatal(err)
	}
	choices, used, err := boundedArchiveChoices(catalog.WithReadView(t.Context()), a.env, wrapped, listOptions{filter: reader.Filter{Replays: reader.ReplaysHidden}}, sessionScope{All: true}, listFormatOptions{Numbered: true}, io.Discard, "show")
	if !used || err != nil {
		t.Fatal(used, err)
	}
	initial := choices.shown().rows
	olderIDs := map[string]bool{"older-parent": true, "older-child": true}
	for _, r := range initial {
		if olderIDs[r.SessionID] {
			t.Fatal("older fixtures unexpectedly loaded initially")
		}
	}
	if counts.paths["body"] != 0 || counts.Metrics().Lists != 0 {
		t.Fatal("startup read complete archive")
	}
	universe, err := choices.shown().searchContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	rows, matched := filterRows(withTableNumbers(initial, universe), parseSessionQuery("childneedle"))
	if len(rows) != 2 || matched != 1 || rows[0].SessionID != "older-parent" || rows[1].SessionID != "older-child" || rows[1].Depth != 1 {
		t.Fatalf("older parent grouping lost: %+v", rows)
	}
	rows, matched = filterRows(withTableNumbers(initial, universe), parseSessionQuery("latefieldneedle"))
	if len(rows) != 1 || matched != 1 || rows[0].SessionID != "newest-long-title" {
		t.Fatal("bounded display fields replaced full search", rows)
	}
	if rows[0].pickerSelection == nil || rows[0].pickerSelection.item == nil {
		t.Fatal("search discarded immutable selection")
	}
}

func TestBoundedBareShowKeepsSelectedParentAndLinkedChildInOneView(t *testing.T) {
	t.Parallel()
	a := newScopedArchive(t)
	a.addMetadata(t, "bounded-linked-child", "Child", a.label, subagentOf(a.id))
	parent := a.base
	parent.LinkedSessions = []archive.LinkedSessionReference{{SessionID: "bounded-linked-child", Relationship: "subagent", Status: archive.LinkedSessionPublished}}
	parentKey, _ := archive.MetadataObjectKey("codex", a.id)
	raw, err := json.Marshal(parent)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.mem.Put(t.Context(), parentKey, raw); err != nil {
		t.Fatal(err)
	}
	remote := scopedPickerCatalog(t, a.mem)
	entry, _, err := remote.Writer.Find(t.Context(), parentKey)
	if err != nil || entry == nil {
		t.Fatal(err)
	}
	childKey, _ := archive.MetadataObjectKey("codex", "bounded-linked-child")
	var mutationErr error
	hook := &bareSelectionStore{MeasuredStore: storagetest.NewMeasuredStore(remote.ObjectStore, 0), parentKey: entry.Metadata.Key, mutate: func() { mutationErr = remote.DeleteSession(t.Context(), childKey) }}
	wrapped, err := catalog.Wrap(hook)
	if err != nil {
		t.Fatal(err)
	}
	input := &bareSelectionInput{Reader: strings.NewReader("1\n"), arm: func() { hook.armed.Store(true) }}
	var output, stderr bytes.Buffer
	env := a.env
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return wrapped, nil }
	env.IsTerminal = func(stream any) bool { return stream == any(input) || stream == any(&output) }
	if code := Run([]string{"show", "--json", "--harness", "codex"}, input, &output, &stderr, env); code != 0 {
		t.Fatal(code, stderr.String())
	}
	start := strings.Index(output.String(), "{")
	if start < 0 {
		t.Fatal("selected JSON missing")
	}
	var got sessionView
	if err = json.Unmarshal(output.Bytes()[start:], &got); err != nil {
		t.Fatal(err)
	}
	if mutationErr != nil || !hook.mutated.Load() || len(got.LinkedAvailability) != 1 || got.LinkedAvailability[0].State != reader.LinkedStateMetadataAvailable {
		t.Fatal("selected parent and children used different roots", mutationErr, got.LinkedAvailability)
	}
	fresh, err := readSessionView(catalog.NewReadView(t.Context()), remote, "codex", a.id)
	if err != nil || len(fresh.LinkedAvailability) != 1 || fresh.LinkedAvailability[0].State != reader.LinkedStateUnavailableOrExpired {
		t.Fatal("mutation did not produce the fresh-root oracle", err, fresh.LinkedAvailability)
	}
}

func TestBoundedArchiveRowsKeepActivityAgeAndUnambiguousShortIDs(t *testing.T) {
	t.Parallel()
	a := newScopedArchive(t)
	active := a.env.now().Add(-5 * time.Minute)
	captured := a.env.now().Add(-30 * 24 * time.Hour)
	for _, id := range []string{"collision-a", "collision-b"} {
		a.addMetadata(t, id, id, a.label, func(m *archive.Metadata) { m.EndedAt = &active; m.CapturedAt = captured })
	}
	remote := scopedPickerCatalog(t, a.mem)
	format := listFormatOptions{Now: a.env.now(), Numbered: true}
	choices, used, err := boundedArchiveChoices(catalog.WithReadView(t.Context()), a.env, remote, listOptions{filter: reader.Filter{Replays: reader.ReplaysHidden}}, sessionScope{All: true}, format, io.Discard, "show")
	if !used || err != nil {
		t.Fatal(used, err)
	}
	found := map[string]listRow{}
	for _, r := range choices.shown().rows {
		found[r.SessionID] = r
	}
	for _, id := range []string{"collision-a", "collision-b"} {
		r, ok := found[id]
		if !ok || r.When != relativeAge(format.Now, active) || r.CapturedAt != formatTimeOrNever(captured) {
			t.Fatal("activity display used capture time", r)
		}
	}
	if found["collision-a"].ShortID == found["collision-b"].ShortID {
		t.Fatal("loaded short IDs are ambiguous")
	}
}

func TestBatchedScopedFixtureMatchesEagerUniverseAndKeepsMutationsPrivate(t *testing.T) {
	t.Parallel()
	a := newScopedArchive(t)
	eager, batched := a, a
	eager.mem, batched.mem = cloneScopedFixtureStore(t, a.mem), cloneScopedFixtureStore(t, a.mem)
	for _, id := range []string{"batch-one", "batch-two"} {
		eager.add(t, id, id, a.label)
		batched.addMetadata(t, id, id, a.label)
	}
	// Both constructors still normalize every source, rebuild the final index,
	// commit every row, and perform the real guarded scoped upgrade.
	eagerCatalog := scopedPickerCatalog(t, eager.mem)
	batchedCatalog := scopedPickerCatalog(t, batched.mem)
	eagerLegacy, err := reader.ListRecent(t.Context(), eager.mem, archiveSessionsPrefix, reader.Filter{}, 0, reader.ListOptions{})
	must(t, err)
	batchedLegacy, err := reader.ListRecent(t.Context(), batched.mem, archiveSessionsPrefix, reader.Filter{}, 0, reader.ListOptions{})
	must(t, err)
	if !reflect.DeepEqual(eagerLegacy, batchedLegacy) || len(eagerLegacy.Sessions) != 3 {
		t.Fatal("batched metadata changed the complete indexed universe")
	}
	query := catalog.PickerQuery{Scope: catalog.PickerScope{All: true}, Membership: catalog.PickerBrowse, Limit: 50}
	eagerSnapshot, err := catalog.OpenSnapshot(t.Context(), eagerCatalog, nil)
	must(t, err)
	batchedSnapshot, err := catalog.OpenSnapshot(t.Context(), batchedCatalog, nil)
	must(t, err)
	eagerPage, err := eagerSnapshot.PickerPage(t.Context(), query, "")
	must(t, err)
	batchedPage, err := batchedSnapshot.PickerPage(t.Context(), query, "")
	must(t, err)
	if len(eagerPage.Rows) != 3 || len(batchedPage.Rows) != 3 || eagerPage.Total != batchedPage.Total || eagerPage.More != batchedPage.More {
		t.Fatal("batched catalog changed the captured page universe")
	}
	for i := range eagerPage.Rows {
		eagerRow, batchedRow := eagerPage.Rows[i].Row, batchedPage.Rows[i].Row
		// Independent real constructors allocate new object/revision identities;
		// their body hashes and all presentation facts must still agree.
		eagerRow.Metadata.Key, batchedRow.Metadata.Key = "", ""
		eagerRow.Revision, batchedRow.Revision = "", ""
		if !reflect.DeepEqual(eagerRow, batchedRow) {
			t.Fatal("batched catalog changed captured row facts")
		}
	}
	key, err := archive.MetadataObjectKey("codex", "batch-one")
	must(t, err)
	must(t, eagerCatalog.DeleteSession(t.Context(), key))
	must(t, batchedSnapshot.ValidateContinuation(t.Context()))
	for _, item := range batchedPage.Rows {
		if item.Row.SessionID == "batch-one" {
			_, err = batchedSnapshot.HydratePicker(t.Context(), item.Reference)
			must(t, err)
		}
	}
	// Source metadata and bytes returned to callers remain separate copies too.
	raw, err := eager.mem.Get(t.Context(), key)
	must(t, err)
	raw[0] = '!'
	must(t, eager.mem.Put(t.Context(), key, raw))
	other, err := batched.mem.Get(t.Context(), key)
	must(t, err)
	if !json.Valid(other) {
		t.Fatal("fixture mutation escaped its private store")
	}
	_, err = a.mem.Get(t.Context(), key)
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("fixture construction mutated the seed store", err)
	}
}

func cloneScopedFixtureStore(t *testing.T, source storage.ObjectStore) *storagetest.MemoryStore {
	t.Helper()
	var memory *storagetest.MemoryStore
	switch source := source.(type) {
	case *storagetest.MemoryStore:
		memory = source
	case *qualifiedCLIStore:
		memory = source.MemoryStore
	case *scopedMigrationCLIStore:
		memory = source.MemoryStore
	}
	if memory != nil {
		clone, err := storagetest.CloneMemoryStore(t.Context(), memory)
		must(t, err)
		return clone
	}
	clone := storagetest.NewMemoryStore()
	objects, err := source.List(t.Context(), "")
	must(t, err)
	for _, object := range objects {
		raw, err := source.Get(t.Context(), object.Key)
		must(t, err)
		must(t, clone.Put(t.Context(), object.Key, raw))
	}
	return clone
}

func cloneScopedFixtureCatalog(t *testing.T, source storage.ObjectStore) *catalog.Store {
	t.Helper()
	raw := &scopedMigrationCLIStore{&qualifiedCLIStore{cloneScopedFixtureStore(t, source)}}
	remote, err := catalog.Wrap(raw)
	must(t, err)
	return remote
}

func scopedFixtureEnv(t *testing.T, source scopedArchive) Env {
	t.Helper()
	sourceHome, err := source.env.readHome()
	must(t, err)
	cfg, found, err := config.Load(sourceHome)
	must(t, err)
	if !found {
		t.Fatal("scoped fixture configuration is missing")
	}
	home := t.TempDir()
	must(t, config.Save(home, cfg))
	env := testEnv(t, home, source.env.now())
	dir := source.dir
	env.WorkingDir = func() (string, error) { return dir, nil }
	return env
}

func TestScopedCatalogClonesKeepHeadCoordinatorAndBodiesPrivate(t *testing.T) {
	t.Parallel()
	a := newScopedArchive(t)
	a.addMetadata(t, "clone-one", "Clone one", a.label)
	seed := scopedPickerCatalog(t, a.mem)
	first := cloneScopedFixtureCatalog(t, seed.ObjectStore)
	second := cloneScopedFixtureCatalog(t, seed.ObjectStore)
	query := catalog.PickerQuery{Scope: catalog.PickerScope{All: true}, Membership: catalog.PickerBrowse, Limit: 50}
	snapshot, err := catalog.OpenSnapshot(t.Context(), second, nil)
	must(t, err)
	page, err := snapshot.PickerPage(t.Context(), query, "")
	must(t, err)
	if len(page.Rows) != 2 {
		t.Fatal("clone lost the complete constructor universe")
	}
	key, err := archive.MetadataObjectKey("codex", "clone-one")
	must(t, err)
	must(t, first.DeleteSession(t.Context(), key))
	_, err = first.Writer.Coordinator().Seal(t.Context())
	must(t, err)
	must(t, second.Writer.Coordinator().Active(t.Context()))
	must(t, seed.Writer.Coordinator().Active(t.Context()))
	must(t, snapshot.ValidateContinuation(t.Context()))
	for _, item := range page.Rows {
		// Corrupting an immutable object in the first clone must not affect
		// either another clone's hydration or the real constructor seed.
		must(t, first.ObjectStore.Put(t.Context(), item.Row.Metadata.Key, []byte("corrupt clone body")))
		_, err = snapshot.HydratePicker(t.Context(), item.Reference)
		must(t, err)
		body, err := seed.ObjectStore.Get(t.Context(), item.Row.Metadata.Key)
		must(t, err)
		if storage.SHA256Hex(body) != item.Row.Metadata.SHA256 {
			t.Fatal("body mutation escaped its private clone")
		}
	}
	firstEnv, secondEnv := scopedFixtureEnv(t, a), scopedFixtureEnv(t, a)
	firstHome, err := firstEnv.readHome()
	must(t, err)
	secondHome, err := secondEnv.readHome()
	must(t, err)
	if firstHome == secondHome {
		t.Fatal("clones share a local metadata cache home")
	}
	firstScope, err := scopeFor(firstEnv, "", false)
	must(t, err)
	secondScope, err := scopeFor(secondEnv, "", false)
	must(t, err)
	if !reflect.DeepEqual(firstScope, secondScope) {
		t.Fatal("private clone environments changed project membership")
	}
}
