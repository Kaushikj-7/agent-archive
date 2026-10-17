package cli

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// Build the archived fixture through the same builtin source, filter and parser
// as local handoff. No consumer test interprets Muse's native records itself.
func museViewsFixture(t *testing.T) (handoffFixture, archive.Metadata, string) {
	t.Helper()
	env, data, workspace, counter := museNativeEnv(t)
	path := writeMuseNative(t, data, museSyntheticID, museNativeFixtureBytes(t, "synthetic-own-tools.jsonl", museSyntheticID, workspace))
	var filtered archive.FilteredTranscript
	counter.after = func(out archive.FilteredTranscript) { filtered = out }
	_, stderr, code := runHandoff(t, env, "--file", path, "--harness", "muse", "--format", "json")
	if code != 0 || counter.calls.Load() != 1 {
		t.Fatalf("filter fixture: %d %s", code, stderr)
	}
	f := newHandoffFixture(t, false)
	reg := archive.SessionRegistration{ArchiveSessionID: f.id, NativeSessionID: museSyntheticID, ProjectID: "local", ProjectRoot: workspace, Harness: archive.Harness{Name: "muse"}, SessionStartedAt: filtered.NativeStartAt}
	b, err := archive.NewSourceBundle(reg, counter.TranscriptFilter, filtered, f.env.Now(), nil)
	must(t, err)
	packed, err := archive.BuildCompressedSource(b)
	must(t, err)
	key, err := archive.SourceObjectKey(b, packed.SHA256)
	must(t, err)
	must(t, f.mem.Put(t.Context(), key, packed.Bytes))
	a, err := analyzeSource(t.Context(), env.Agents, b)
	must(t, err)
	parser, _ := env.agentRegistry().LookupParser("muse")
	m, err := archive.BuildMetadataWithAnalysis(b, a, nil, "synthetic-machine", filtered.NativeStartAt, f.env.Now(), archive.SourceReference{Key: key, SHA256: packed.SHA256, CompressedBytes: len(packed.Bytes)}, archive.ParserInfo{Name: "muse", Version: parser.Version()})
	must(t, err)
	putMuseViewsMetadata(t, f, m)
	must(t, os.Remove(path))
	return f, m, path
}

func putMuseViewsMetadata(t *testing.T, f handoffFixture, m archive.Metadata) {
	t.Helper()
	raw, err := json.Marshal(m)
	must(t, err)
	key, err := archive.MetadataObjectKey("muse", m.SessionID)
	must(t, err)
	must(t, f.mem.Put(t.Context(), key, raw))
}

func TestMuseArchivedViewsAgreeAfterNativeLoss(t *testing.T) {
	t.Parallel()
	f, m, _ := museViewsFixture(t)
	f.env.Now = func() time.Time { return m.CapturedAt }
	shown, stderr, code := runShow(t, f.env, f.id, "--harness", "muse", "--transcript", "--json", "--max-bytes", "0")
	if code != 0 {
		t.Fatalf("show: %d %s", code, stderr)
	}
	var sidecar archive.Metadata
	must(t, json.NewDecoder(strings.NewReader(shown)).Decode(&sidecar))
	if !reflect.DeepEqual(sidecar.Counts, m.Counts) || !reflect.DeepEqual(sidecar.CaptureGaps, m.CaptureGaps) || sidecar.NativeSessionID != museSyntheticID {
		t.Fatal("show changed identity, counts or gaps")
	}
	for _, detail := range []string{"metadata", "full"} {
		records, stderr, code := evalLines(t, f.env, f.id, "--harness", "muse", "--detail", detail)
		if code != 0 || len(records) != 1 {
			t.Fatalf("eval %s: %d %s", detail, code, stderr)
		}
		raw, err := json.Marshal(records[0])
		must(t, err)
		var record archive.EvalExport
		must(t, json.Unmarshal(raw, &record))
		if record.SessionID != m.SessionID || record.NativeSessionID != m.NativeSessionID || record.Harness != m.Harness || !reflect.DeepEqual(record.Counts, m.Counts) || !reflect.DeepEqual(record.CaptureGaps, m.CaptureGaps) || !reflect.DeepEqual(record.TokenObservations, m.TokenObservations) {
			t.Fatalf("eval %s disagrees with reader metadata", detail)
		}
		if (record.Prompts != nil) != (detail == "full") || (detail == "full" && len(*record.Prompts) != *m.Counts.Turns) {
			t.Fatalf("eval %s prompt projection", detail)
		}
	}
	args := []string{"--harness", "muse", "--days", "1", "--no-cache"}
	var doc statsDocument
	must(t, json.Unmarshal([]byte(mustRunStats(t, f.env, 0, append(args, "--json")...)), &doc))
	if doc.Coverage.Sessions != 1 || doc.Overview.Cost.Value != nil || doc.Overview.Tokens.Value != nil || doc.TokenObservations == nil || !reflect.DeepEqual(doc.TokenObservations.Input, m.TokenObservations.Input) || !reflect.DeepEqual(doc.TokenObservations.Output, m.TokenObservations.Output) {
		t.Fatal("stats changed the observed subset or priced unavailable usage")
	}
	page := mustRunStats(t, f.env, 0, append(args, "--html", "--include-names")...)
	checkPageAgainstJSON(t, parsePage(t, page), doc)
	for _, view := range []string{"overview", "detail", "projects", "models", "agents"} {
		text := mustRunStats(t, f.env, 120, append(args, "--view", view, "--no-pager")...)
		for _, expected := range []string{"full usage, composition and cost unavailable", "excludes 1 observation-only sessions"} {
			if !strings.Contains(text, expected) || !strings.Contains(page, expected) {
				t.Fatalf("%s lost availability qualifier %q", view, expected)
			}
		}
	}
}

// A sidecar refresh must invalidate warm search/catalog projections even when
// its verified source pointer and capture timestamp have not changed.
func TestMuseMetadataOnlyRefreshUpdatesArchivedSearch(t *testing.T) {
	t.Parallel()
	f, m, _ := museViewsFixture(t)
	m.Name, m.Title = "Initial generated name", "Initial prompt title"
	putMuseViewsMetadata(t, f, m)
	for _, query := range []string{m.Name, m.Title} {
		_, stderr, code := runShow(t, f.env, query, "--harness", "muse", "--json")
		if code != 0 {
			t.Fatalf("warm %q: %d %s", query, code, stderr)
		}
	}
	m.Name, m.Title = "Renamed ÉCOLE session", "Refreshed prompt title"
	m.Models = []archive.ModelSummary{{Attributes: map[string]string{"gen_ai.request.model": "synthetic-refreshed-model"}, Source: archive.ModelSummarySourceNativeTranscript, ResponseModelStatus: archive.ResponseModelStatusNotExposed}}
	putMuseViewsMetadata(t, f, m)
	for _, query := range []string{m.Name, m.Title, f.id} {
		out, stderr, code := runShow(t, f.env, query, "--harness", "muse", "--json")
		if code != 0 {
			t.Fatalf("refreshed %q: %d %s", query, code, stderr)
		}
		var got archive.Metadata
		must(t, json.Unmarshal([]byte(out), &got))
		if got.Name != m.Name || got.Title != m.Title || !reflect.DeepEqual(got.Models, m.Models) || got.SourceBundle != m.SourceBundle {
			t.Fatal("metadata-only refresh kept a stale projection or changed source")
		}
	}
	for _, query := range []string{"Initial generated name", "Initial prompt title"} {
		_, _, code := runShow(t, f.env, query, "--harness", "muse", "--json")
		if code == 0 {
			t.Fatalf("stale search still selected %q", query)
		}
	}
	var doc statsDocument
	must(t, json.Unmarshal([]byte(mustRunStats(t, f.env, 0, "--json", "--harness", "muse", "--model", "synthetic-refreshed-model")), &doc))
	if doc.Coverage.Sessions != 1 {
		t.Fatal("stats model filter used stale metadata")
	}
	for _, detail := range []string{"metadata", "full"} {
		records, stderr, code := evalLines(t, f.env, f.id, "--harness", "muse", "--detail", detail)
		if code != 0 || len(records) != 1 {
			t.Fatalf("refreshed eval %s: %d %s", detail, code, stderr)
		}
		raw, err := json.Marshal(records[0])
		must(t, err)
		var record archive.EvalExport
		must(t, json.Unmarshal(raw, &record))
		if !reflect.DeepEqual(record.Models, m.Models) {
			t.Fatalf("eval %s used stale models", detail)
		}
	}
}

func TestMuseMissingArchivedSourceKeepsMetadataReadable(t *testing.T) {
	t.Parallel()
	f, m, _ := museViewsFixture(t)
	must(t, f.mem.Delete(t.Context(), m.SourceBundle.Key))
	out, stderr, code := runShow(t, f.env, f.id, "--harness", "muse", "--json")
	if code != 0 || !strings.Contains(out, m.NativeSessionID) {
		t.Fatalf("metadata show: %d %s", code, stderr)
	}
	for _, detail := range []string{"metadata", "full"} {
		records, stderr, code := evalLines(t, f.env, f.id, "--harness", "muse", "--detail", detail)
		if len(records) != 1 || stderr != "" {
			t.Fatalf("eval %s: %d %s", detail, code, stderr)
		}
		if detail == "metadata" {
			if code != 0 || records[0]["session_id"] != f.id {
				t.Fatal("source loss hid metadata")
			}
		} else if code == 0 || records[0]["error"].(map[string]any)["code"] != "read_failed" {
			t.Fatal("missing source became an empty successful transcript")
		}
		raw, err := json.Marshal(records[0])
		must(t, err)
		if strings.Contains(string(raw), "no-session-log") || strings.Contains(string(raw), "logging disabled") {
			t.Fatal("archive source loss invented a native logging mode")
		}
	}
}
