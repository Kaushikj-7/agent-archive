package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/stats"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestRealCLICatalogStatsKeepsOldDescendants(t *testing.T) {
	env, legacy := statsEnv(t)
	old := statsFetchFrom(statsNow, time.UTC, 1).Add(-24 * time.Hour)
	var complete []archive.Metadata
	for _, session := range []syntheticSession{
		{id: "root", harness: "claude", project: "p", captured: statsNow, models: []string{"claude-opus-5"}, perModel: []modelTokenSpec{{model: "claude-opus-5", input: 100, output: 10}}},
		{id: "child", harness: "claude", project: "p", parent: "root", captured: old, models: []string{"claude-opus-5"}, perModel: []modelTokenSpec{{model: "claude-opus-5", input: 200, output: 20}}},
		{id: "grandchild", harness: "claude", project: "p", parent: "child", captured: old.Add(-24 * time.Hour), models: []string{"claude-opus-5"}, perModel: []modelTokenSpec{{model: "claude-opus-5", input: 300, output: 30}}},
	} {
		metadata := session.build()
		metadata.ProjectID = session.project
		bundle := archive.SourceBundle{
			SchemaVersion:    archive.SourceSchemaVersion,
			ArchiveSessionID: session.id, NativeSessionID: session.id,
			ProjectID: session.project, ParentSessionID: session.parent,
			Capture: archive.SourceCapture{
				Harness: metadata.Harness, AdapterName: "synthetic-stats",
				CapturedAt: session.captured,
			},
		}
		packed, err := archive.BuildCompressedSource(bundle)
		if err != nil {
			t.Fatal(err)
		}
		sourceKey, err := archive.SourceObjectKey(bundle, packed.SHA256)
		if err != nil {
			t.Fatal(err)
		}
		if err = legacy.Put(t.Context(), sourceKey, packed.Bytes); err != nil {
			t.Fatal(err)
		}
		metadata.SourceBundle = archive.SourceReference{
			Key: sourceKey, SHA256: packed.SHA256, CompressedBytes: len(packed.Bytes),
		}
		raw, err := json.Marshal(metadata)
		if err != nil {
			t.Fatal(err)
		}
		key, err := archive.MetadataObjectKey(session.harness, session.id)
		if err != nil {
			t.Fatal(err)
		}
		if err = legacy.Put(t.Context(), key, raw); err != nil {
			t.Fatal(err)
		}
		complete = append(complete, metadata)
	}
	remote := privateCatalogFromLegacy(t, legacy)
	counts := &catalogListReads{MeasuredStore: storagetest.NewMeasuredStore(remote.ObjectStore, 0), paths: map[string]int{}}
	wrapped, err := catalog.Wrap(counts)
	if err != nil {
		t.Fatal(err)
	}
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return wrapped, nil }
	want, err := json.Marshal(stats.Compute(complete, stats.Options{Now: statsNow, Days: 1, Location: time.UTC, AllRows: true}))
	if err != nil {
		t.Fatal(err)
	}
	for attempt, uncached := range []bool{true, false, false} {
		counts.Reset()
		counts.paths = map[string]int{}
		args := []string{"--days", "1", "--json", "--all"}
		if uncached {
			args = append(args, "--no-cache")
		}
		out, errOut, code := runStats(t, env, 0, args...)
		if code != 0 || errOut != "" {
			t.Fatal(code, errOut)
		}
		var doc statsDocument
		if err = json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatal(err)
		}
		got, err := json.Marshal(doc.Stats)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) || counts.Metrics().Lists != 0 {
			t.Fatal("catalog stats differ from complete accounting", counts.Metrics())
		}
		if attempt == 2 && (counts.paths["body"] != 0 || counts.Metrics().Gets > 16) {
			t.Fatal("reusable complete SQL stats fetched bodies or walked the full tree", counts.paths, counts.Metrics())
		}
		if attempt == 1 {
			// An existing supported text-list request establishes the complete
			// SQL universe; stats itself never builds one just for reuse.
			var out, errs bytes.Buffer
			if code := Run([]string{"list", "--all-projects", "--json", "__no_matching_session__"}, nil, &out, &errs, env); code != 0 || errs.Len() != 0 {
				t.Fatal("complete SQL prerequisite", code, errs.String())
			}
		}
	}
}

func TestRealCLICatalogLongTitlesWithHarnessUseSearch(t *testing.T) {
	for _, title := range []string{"Review the complete synthetic publication workflow", "syntheticpublicationworkflowtitlelongerthanthirtytwo", strings.Repeat("synthetic", 150)} {
		fixture := newShowLimitFixture(t, 1)
		key, err := archive.MetadataObjectKey("codex", fixture.id)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := fixture.mem.Get(t.Context(), key)
		if err != nil {
			t.Fatal(err)
		}
		var metadata archive.Metadata
		if err = json.Unmarshal(raw, &metadata); err != nil {
			t.Fatal(err)
		}
		metadata.Title = title
		raw, err = json.Marshal(metadata)
		if err != nil {
			t.Fatal(err)
		}
		if err = fixture.mem.Put(t.Context(), key, raw); err != nil {
			t.Fatal(err)
		}
		remote := privateCatalogFromLegacy(t, fixture.mem)
		fixture.env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return remote, nil }
		for _, transcript := range []bool{false, true} {
			args := []string{title, "--harness", "codex", "--json"}
			if transcript {
				args = append(args, "--transcript")
			}
			out, errOut, code := runShow(t, fixture.env, args...)
			if code != 0 || errOut != "" || !strings.Contains(out, fixture.id) || transcript && !strings.Contains(out, "reply-0") {
				t.Fatalf("catalog long title transcript=%v code=%d stderr=%s", transcript, code, errOut)
			}
		}
	}
}
