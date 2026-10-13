package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	for _, command := range []string{"list", "show"} {
		t.Run(command, func(t *testing.T) {
			a := newScopedArchive(t)
			for i := range 105 {
				a.add(t, fmt.Sprintf("browse-%03d", i), fmt.Sprintf("Title %03d", i), a.label)
			}
			remote := scopedPickerCatalog(t, a.mem)
			counts := &catalogListReads{MeasuredStore: storagetest.NewMeasuredStore(remote.ObjectStore, 0), paths: map[string]int{}}
			wrapped, err := catalog.Wrap(counts)
			if err != nil {
				t.Fatal(err)
			}
			env := a.env
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
	a := newScopedArchive(t)
	for i := range 130 {
		project := a.label
		if i >= 60 {
			project = "elsewhere"
		}
		a.add(t, fmt.Sprintf("scope-%03d", i), fmt.Sprintf("Title %03d", i), project)
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
	a := newScopedArchive(t)
	for i := range 55 {
		a.add(t, fmt.Sprintf("ordinary-%03d", i), fmt.Sprintf("Title %03d", i), a.label)
	}
	a.add(t, "older-parent", "older parent", a.label, func(m *archive.Metadata) {
		at := a.base.CapturedAt.Add(-2 * time.Hour)
		m.EndedAt = &at
		m.CapturedAt = at
	})
	a.add(t, "older-child", "childneedle", a.label, func(m *archive.Metadata) {
		m.ParentSessionID = "older-parent"
		at := a.base.CapturedAt.Add(-2 * time.Hour)
		m.EndedAt = &at
		m.CapturedAt = at
	})
	a.add(t, "newest-long-title", strings.Repeat("uncapped ", 300)+"latefieldneedle", a.label, func(m *archive.Metadata) { at := a.base.CapturedAt.Add(time.Hour); m.EndedAt = &at; m.CapturedAt = at })
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
	for _, r := range initial {
		if r.SessionID == "older-parent" || r.SessionID == "older-child" {
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
	a := newScopedArchive(t)
	a.add(t, "bounded-linked-child", "Child", a.label, subagentOf(a.id))
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
