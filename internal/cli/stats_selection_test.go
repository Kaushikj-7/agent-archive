package cli

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/stats"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestStatsKeepsOldChildrenOfRecentRoot(t *testing.T) {
	env, store := statsEnv(t)
	old := statsFetchFrom(statsNow, time.UTC, 1).Add(-24 * time.Hour)
	var complete []archive.Metadata
	for _, s := range []syntheticSession{
		{id: "root", harness: "claude", project: "p", captured: statsNow, models: []string{"claude-opus-5"}, perModel: []modelTokenSpec{{model: "claude-opus-5", input: 100, output: 10}}},
		{id: "child", harness: "claude", project: "p", parent: "root", captured: old, models: []string{"claude-opus-5"}, perModel: []modelTokenSpec{{model: "claude-opus-5", input: 200, output: 20}}},
		{id: "grandchild", harness: "claude", project: "p", parent: "child", captured: old.Add(-24 * time.Hour), models: []string{"claude-opus-5"}, perModel: []modelTokenSpec{{model: "claude-opus-5", input: 300, output: 30}}},
	} {
		s.publish(t, store)
		complete = append(complete, s.build())
	}
	for _, indexed := range []bool{false, true} {
		if indexed {
			if _, err := reader.RebuildIndex(t.Context(), store, "sessions"); err != nil {
				t.Fatal(err)
			}
		}
		out, errOut, code := runStats(t, env, 0, "--days", "1", "--json", "--all", "--no-cache")
		if code != 0 || errOut != "" {
			t.Fatalf("indexed=%v: code=%d stderr=%s", indexed, code, errOut)
		}
		var doc statsDocument
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatal(err)
		}
		want := stats.Compute(complete, stats.Options{Now: statsNow, Days: 1, Location: time.UTC, AllRows: true})
		gotJSON, err := json.Marshal(doc.Stats)
		if err != nil {
			t.Fatal(err)
		}
		wantJSON, err := json.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		if string(gotJSON) != string(wantJSON) {
			t.Errorf("indexed=%v: stats differ from complete root accounting: got=%s want=%s", indexed, gotJSON, wantJSON)
		}
	}
}

func TestStatsIndexedFetchEqualsExhaustiveAcrossShapesAndWindows(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	for _, shape := range statsShapes() {
		t.Run(shape.name, func(t *testing.T) {
			store := storagetest.NewMemoryStore()
			for _, s := range shape.build(now, time.UTC) {
				data, err := json.Marshal(s.build())
				if err != nil {
					t.Fatal(err)
				}
				key, err := archive.MetadataObjectKey(s.harness, s.id)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.Put(t.Context(), key, data); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := reader.RebuildIndex(t.Context(), store, "sessions"); err != nil {
				t.Fatal(err)
			}
			windows, _ := statsWindowCycle(200)
			filter := reader.Filter{From: statsFetchFrom(now, time.UTC, slices.Max(windows))}
			complete, err := reader.ListMetadataWithOptions(t.Context(), store, "sessions", reader.Filter{}, reader.ListOptions{})
			if err != nil {
				t.Fatal(err)
			}
			exhaustive := fetched(complete, now, time.UTC, windows)
			indexed, err := reader.SelectMetadata(t.Context(), store, "sessions", reader.MetadataQuery{Filter: filter, IncludeRootChildren: true}, reader.ListOptions{CompatibilityScan: func(reason string) { t.Fatalf("unexpected fallback: %s", reason) }})
			if err != nil {
				t.Fatal(err)
			}
			for _, days := range windows {
				got, err := json.Marshal(stats.Compute(indexed.Sessions, stats.Options{Now: now, Days: days, Location: time.UTC, AllRows: true}))
				if err != nil {
					t.Fatal(err)
				}
				want, err := json.Marshal(stats.Compute(exhaustive, stats.Options{Now: now, Days: days, Location: time.UTC, AllRows: true}))
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != string(want) {
					t.Fatalf("window %d indexed stats differ from exhaustive", days)
				}
			}
		})
	}
}
