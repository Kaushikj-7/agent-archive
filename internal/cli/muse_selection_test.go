package cli

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/reader"
)

// These are metadata projection fixtures, not proof of native child admission.
// Roles and complete child coverage remain unavailable until qualified upstream.
func TestMuseViewsPreserveRootChildReplayAndDateSelection(t *testing.T) {
	t.Parallel()
	env, store := statsEnv(t)
	const rootID = "00000000000000000000000000000001"
	const childID = "00000000000000000000000000000002"
	const grandID = "00000000000000000000000000000003"
	const orphanID = "00000000000000000000000000000004"
	const replayID = "00000000000000000000000000000005"
	old := statsFetchFrom(statsNow, time.UTC, 1).Add(-24 * time.Hour)
	var metadata []archive.Metadata
	for _, row := range []struct {
		id     string
		parent string
		name   string
		at     time.Time
		input  int64
		replay *archive.Replay
	}{
		{rootID, "", "ROOT_VIEW_MARKER", statsNow, 10, nil},
		{childID, rootID, "CHILD_VIEW_MARKER", old, 20, nil},
		{grandID, childID, "GRANDCHILD_VIEW_MARKER", old.Add(-24 * time.Hour), 30, nil},
		{orphanID, "00000000000000000000000000000099", "ORPHAN_VIEW_MARKER", statsNow, 40, nil},
		{replayID, "", "REPLAY_VIEW_MARKER", statsNow, 50, &archive.Replay{RunID: "synthetic-replay"}},
	} {
		var observations archive.ObservationAccumulator
		must(t, observations.Add("synthetic-model", row.input, 1))
		m := archive.Metadata{SchemaVersion: archive.MetadataSchemaVersion, SessionID: row.id, NativeSessionID: row.id, MachineID: "synthetic-machine", ProjectID: "synthetic", ProjectName: "synthetic", Name: row.name, Harness: archive.Harness{Name: "muse"}, CapturedAt: row.at, StartedAt: row.at, MetadataDerivedAt: row.at, ParentSessionID: row.parent, NativeChild: row.parent != "", Replay: row.replay, Parser: archive.ParserInfo{Name: "muse", Version: "synthetic-projection", Status: archive.ParserStatusPartial}, FilterVersion: archive.FilterVersion, Counts: archive.Counts{Turns: new(1)}, TokenObservations: observations.Summary(), CaptureGaps: []archive.CaptureGap{{Code: "child_coverage_unavailable"}}, SourceBundle: archive.SourceReference{Key: "sessions/muse/" + row.id + "/source.jsonl.gz", SHA256: strings.Repeat("a", 64), CompressedBytes: 1}}
		metadata = append(metadata, m)
		raw, err := json.Marshal(m)
		must(t, err)
		key, err := archive.MetadataObjectKey("muse", row.id)
		must(t, err)
		must(t, store.Put(t.Context(), key, raw))
	}
	list := func(extra ...string) (string, listDocument) {
		t.Helper()
		var out, errOut bytes.Buffer
		args := append([]string{"list", "--all-projects", "--harness", "muse", "--limit", "0", "--no-pager"}, extra...)
		if code := Run(args, nil, &out, &errOut, env); code != 0 {
			t.Fatalf("list %v: %d %s", extra, code, errOut.String())
		}
		var doc listDocument
		if slices.Contains(extra, "--json") {
			must(t, json.Unmarshal(out.Bytes(), &doc))
		}
		return out.String(), doc
	}
	for _, indexed := range []bool{false, true} {
		if indexed {
			_, err := reader.RebuildIndex(t.Context(), store, "sessions")
			must(t, err)
		}
		text, _ := list()
		if !strings.Contains(text, "ROOT_VIEW_MARKER") {
			t.Fatal("text list lost the ordinary root")
		}
		for _, hidden := range []string{"CHILD_VIEW_MARKER", "GRANDCHILD_VIEW_MARKER", "ORPHAN_VIEW_MARKER", "REPLAY_VIEW_MARKER"} {
			if strings.Contains(text, hidden) {
				t.Fatalf("default text list exposed %s", hidden)
			}
		}
		for _, tc := range []struct {
			mode replaysFlag
			want []string
		}{
			{replaysHide, []string{rootID, childID, grandID, orphanID}},
			{replaysInclude, []string{rootID, childID, grandID, orphanID, replayID}},
			{replaysOnly, []string{replayID}},
		} {
			_, doc := list("--json", "--replays", string(tc.mode))
			var ids []string
			for _, m := range doc.Sessions {
				ids = append(ids, m.SessionID)
			}
			slices.Sort(ids)
			if !reflect.DeepEqual(ids, tc.want) {
				t.Fatalf("indexed=%v replay=%s: list identities %v", indexed, tc.mode, ids)
			}
		}
		// An explicit search can find a child the ordinary text table hides.
		text, _ = list("CHILD_VIEW_MARKER")
		if !strings.Contains(text, "CHILD_VIEW_MARKER") {
			t.Fatal("explicit child search stayed hidden")
		}
		for _, tc := range []struct {
			mode     replaysFlag
			sessions int
			orphans  int
			input    int64
		}{
			{replaysHide, 2, 1, 100},
			{replaysInclude, 3, 1, 150},
			{replaysOnly, 1, 0, 50},
		} {
			args := []string{"--harness", "muse", "--days", "1", "--replays", string(tc.mode), "--no-cache"}
			var doc statsDocument
			must(t, json.Unmarshal([]byte(mustRunStats(t, env, 0, append(args, "--json")...)), &doc))
			if doc.Coverage.Sessions != tc.sessions || doc.Coverage.OrphanSubagents != tc.orphans || doc.TokenObservations == nil || doc.TokenObservations.Input == nil || *doc.TokenObservations.Input != tc.input || doc.Overview.Cost.Value != nil || doc.Overview.Tokens.Value != nil {
				t.Fatalf("indexed=%v replay=%s changed root/date/orphan accounting: %+v", indexed, tc.mode, doc.Coverage)
			}
			page := mustRunStats(t, env, 0, append(args, "--html", "--include-names")...)
			checkPageAgainstJSON(t, parsePage(t, page), doc)
			text := mustRunStats(t, env, 120, append(args, "--no-pager")...)
			for _, qualifier := range []string{"full usage, composition and cost unavailable", "observation-only sessions"} {
				if !strings.Contains(text, qualifier) || !strings.Contains(page, qualifier) {
					t.Fatal("text or HTML lost observation availability")
				}
			}
		}
	}
	// Explicit child reads retain their own identity and the unsupported coverage
	// gap; parent relationships never create observed role or complete coverage.
	for _, m := range metadata {
		shown, errOut, code := runShow(t, env, m.SessionID, "--harness", "muse", "--json")
		if code != 0 {
			t.Fatalf("direct show: %d %s", code, errOut)
		}
		var got map[string]any
		must(t, json.Unmarshal([]byte(shown), &got))
		if got["session_id"] != m.SessionID || got["native_session_id"] != m.NativeSessionID || got["role"] != nil || got["child_coverage"] != nil || !strings.Contains(shown, "child_coverage_unavailable") {
			t.Fatal("direct metadata changed identity or invented child facts")
		}
		records, errOut, code := evalLines(t, env, m.SessionID, "--harness", "muse", "--detail", "metadata")
		if code != 0 || len(records) != 1 {
			t.Fatalf("direct eval: %d %s", code, errOut)
		}
		raw, err := json.Marshal(records[0])
		must(t, err)
		var export archive.EvalExport
		must(t, json.Unmarshal(raw, &export))
		if export.SessionID != m.SessionID || export.ParentSessionID != m.ParentSessionID || export.MachineID != m.MachineID || !reflect.DeepEqual(export.CaptureGaps, m.CaptureGaps) || !reflect.DeepEqual(export.TokenObservations, m.TokenObservations) || records[0]["role"] != nil || records[0]["child_coverage"] != nil {
			t.Fatal("eval metadata changed ownership/coverage projections")
		}
	}
	_, complete := list("--json", "--complete")
	if len(complete.Sessions) != 0 {
		t.Fatal("unavailable child coverage satisfied complete coverage filter")
	}
}
