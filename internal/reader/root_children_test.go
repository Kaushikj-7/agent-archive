package reader

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestRootChildSelectionKeepsDescendantsAndExistingOrphans(t *testing.T) {
	store := &indexedCountingStore{countingStore: newCountingStore()}
	for _, row := range []struct {
		id      string
		parent  string
		harness string
		model   string
		recent  bool
	}{
		{"root", "", "codex", "keep", true},
		{"child", "root", "codex", "keep", false},
		{"grandchild", "child", "codex", "keep", false},
		{"foreign-child", "root", "claude", "keep", false},
		{"other-model", "root", "codex", "other", false},
		{"orphan", "missing", "codex", "keep", true},
		{"old-ancestor", "", "codex", "keep", false},
		{"recent-child", "old-ancestor", "codex", "keep", true},
		{"cycle-recent", "cycle-old", "codex", "keep", true},
		{"cycle-old", "cycle-recent", "codex", "keep", false},
		{"unrelated", "", "codex", "keep", false},
	} {
		capture := baseTime.Add(-time.Hour)
		if row.recent {
			capture = baseTime
		}
		key := putSession(t, store, row.harness, row.id, capture)
		data, err := store.Get(t.Context(), key)
		if err != nil {
			t.Fatal(err)
		}
		var m archive.Metadata
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatal(err)
		}
		m.ParentSessionID = row.parent
		m.Models = []archive.ModelSummary{{Attributes: map[string]string{"gen_ai.request.model": row.model}}}
		data, err = json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Put(t.Context(), key, data); err != nil {
			t.Fatal(err)
		}
	}
	for _, indexed := range []bool{false, true} {
		if indexed {
			if _, err := RebuildIndex(t.Context(), store, "sessions"); err != nil {
				t.Fatal(err)
			}
		}
		for _, tc := range []struct {
			filter Filter
			want   []string
		}{
			{Filter{From: baseTime, To: baseTime}, []string{"child", "cycle-old", "cycle-recent", "foreign-child", "grandchild", "orphan", "other-model", "recent-child", "root"}},
			{Filter{From: baseTime, Harness: "codex"}, []string{"child", "cycle-old", "cycle-recent", "grandchild", "orphan", "other-model", "recent-child", "root"}},
			{Filter{From: baseTime, Model: "keep"}, []string{"child", "cycle-old", "cycle-recent", "foreign-child", "grandchild", "orphan", "recent-child", "root"}},
			{Filter{From: baseTime.Add(time.Hour)}, nil},
		} {
			store.reset()
			got, err := SelectMetadata(t.Context(), store, "sessions", MetadataQuery{Filter: tc.filter, IncludeRootChildren: true}, ListOptions{})
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, m := range got.Sessions {
				ids = append(ids, m.SessionID)
			}
			slices.Sort(ids)
			if !reflect.DeepEqual(ids, tc.want) {
				t.Fatalf("indexed=%v filter=%+v: got=%v want=%v", indexed, tc.filter, ids, tc.want)
			}
			if indexed && tc.filter.Model == "" {
				_, gets := store.counts()
				if len(gets) != len(tc.want) {
					t.Fatalf("selected body reads=%d want=%d", len(gets), len(tc.want))
				}
			}
		}
		generic, err := SelectMetadata(t.Context(), store, "sessions", MetadataQuery{Filter: Filter{From: baseTime}}, ListOptions{})
		if err != nil || len(generic.Sessions) != 4 {
			t.Fatalf("ordinary capture filter changed: %+v %v", generic.Sessions, err)
		}
	}
}

func TestRootChildSelectionHandlesDeepChainsInOneFieldPass(t *testing.T) {
	items := make([]archive.Metadata, 10000)
	for i := range items {
		items[i] = archive.Metadata{SessionID: fmt.Sprintf("s%d", i), CapturedAt: baseTime.Add(-time.Hour)}
		if i > 0 {
			items[i].ParentSessionID = items[i-1].SessionID
		}
	}
	items[0].CapturedAt = baseTime
	calls := 0
	got := SelectRootChildren(items, Filter{From: baseTime}, func(m archive.Metadata) (string, string, time.Time) {
		calls++
		return m.SessionID, m.ParentSessionID, m.CapturedAt
	})
	if len(got) != len(items) || calls != len(items) || got[len(got)-1].SessionID != items[len(items)-1].SessionID {
		t.Fatalf("deep closure length=%d calls=%d", len(got), calls)
	}
}

func TestRootChildDuplicateIDsUseFullMetadataFallback(t *testing.T) {
	store := &indexedCountingStore{countingStore: newCountingStore()}
	putSession(t, store, "codex", "same", baseTime)
	putSession(t, store, "claude", "same", baseTime)
	if _, err := RebuildIndex(t.Context(), store, "sessions"); err != nil {
		t.Fatal(err)
	}
	fallback := false
	got, err := SelectMetadata(t.Context(), store, "sessions", MetadataQuery{Filter: Filter{From: baseTime}, IncludeRootChildren: true}, ListOptions{CompatibilityScan: func(string) { fallback = true }})
	if err != nil || !fallback || len(got.Sessions) != 2 {
		t.Fatalf("duplicate identities: fallback=%v sessions=%d error=%v", fallback, len(got.Sessions), err)
	}
}

type rootChildFixtureKind string

const (
	rootChildFixtureRoot       rootChildFixtureKind = "root"
	rootChildFixtureOrdinary   rootChildFixtureKind = "ordinary"
	rootChildFixtureReplay     rootChildFixtureKind = "replay"
	rootChildFixtureIncomplete rootChildFixtureKind = "incomplete"
	rootChildFixtureOtherSkill rootChildFixtureKind = "other-skill"
	rootChildFixtureOtherModel rootChildFixtureKind = "other-model"
)

func TestRootChildExpansionRetainsNonDatePredicates(t *testing.T) {
	store := &indexedCountingStore{countingStore: newCountingStore()}
	for _, kind := range []rootChildFixtureKind{rootChildFixtureRoot, rootChildFixtureOrdinary, rootChildFixtureReplay, rootChildFixtureIncomplete, rootChildFixtureOtherSkill, rootChildFixtureOtherModel} {
		id := string(kind)
		captured := baseTime.Add(-time.Hour)
		if kind == rootChildFixtureRoot {
			captured = baseTime
		}
		key := putSession(t, store, "codex", id, captured)
		data, err := store.Get(t.Context(), key)
		if err != nil {
			t.Fatal(err)
		}
		var m archive.Metadata
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatal(err)
		}
		if kind != rootChildFixtureRoot {
			m.ParentSessionID = "root"
		}
		m.Parser.Status = archive.ParserStatusComplete
		m.SkillsUsed = []archive.SkillUse{{Name: "keep"}}
		m.Models = []archive.ModelSummary{{Attributes: map[string]string{"gen_ai.request.model": "keep"}}}
		switch kind {
		case rootChildFixtureRoot, rootChildFixtureOrdinary:
			// These retain the common complete, ordinary metadata settings.
		case rootChildFixtureReplay:
			m.Replay = &archive.Replay{RunID: "synthetic"}
		case rootChildFixtureIncomplete:
			m.Parser.Status = archive.ParserStatusPartial
		case rootChildFixtureOtherSkill:
			m.SkillsUsed = nil
		case rootChildFixtureOtherModel:
			m.Models = nil
		}
		data, err = json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Put(t.Context(), key, data); err != nil {
			t.Fatal(err)
		}
	}
	for _, indexed := range []bool{false, true} {
		if indexed {
			if _, err := RebuildIndex(t.Context(), store, "sessions"); err != nil {
				t.Fatal(err)
			}
		}
		for _, supported := range []bool{false, true} {
			filter := Filter{From: baseTime, Replays: ReplaysHidden}
			want := 5
			if !supported {
				filter = Filter{From: baseTime, Replays: ReplaysHidden, Harness: "codex", Model: "keep", Skill: "keep", RequireCompleteCoverage: true}
				want = 2
			}
			store.reset()
			got, err := SelectMetadata(t.Context(), store, "sessions", MetadataQuery{Filter: filter, IncludeRootChildren: true}, ListOptions{})
			if err != nil || len(got.Sessions) != want {
				t.Fatalf("indexed=%v supported=%v: sessions=%d want=%d err=%v", indexed, supported, len(got.Sessions), want, err)
			}
			if !supported {
				lists, _ := store.counts()
				if len(lists) != 1 {
					t.Fatalf("unsupported fallback repeated canonical discovery: %v", lists)
				}
			}
		}
	}
}
