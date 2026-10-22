package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// This joins the local and retained consumer paths using synthetic native
// evidence. It neither admits a native child nor enables archive capture.
func TestMuseSelectedLocalAndArchivedViewsShareAnalysis(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		fixture string
		named   bool
		calls   int
	}{
		{"synthetic-own-tools.jsonl", true, 3},
		{"synthetic-exec-conversation.jsonl", false, 0},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()
			env, data, workspace, counter := museNativeEnv(t)
			archiveHome := museNativeArchiveHome(t, env)
			raw := museNativeFixtureBytes(t, tc.fixture, museSyntheticID, workspace)
			if tc.named {
				// Append the supported automatic-name shape while preserving all
				// conversation, tool and observation records of this fixture.
				lines := bytes.Split(bytes.TrimSuffix(raw, []byte("\n")), []byte("\n"))
				var named map[string]any
				must(t, json.Unmarshal(lines[len(lines)-1], &named))
				named["id"] = "eeeeeeee-eeee-4eee-8eee-000000000001"
				named["sequence"] = named["sequence"].(float64) + 1
				named["recorded_at"] = named["recorded_at"].(float64) + 1
				named["payload_type"] = "session.name.changed"
				named["payload"] = map[string]any{"session_id": museSyntheticID, "new_name": "Selected synthetic ÉCOLE", "previous_name": nil, "source": "automatic", "operation_id": "00000000-0000-4000-8000-000000000003"}
				line, err := json.Marshal(named)
				must(t, err)
				raw = append(append(raw, line...), '\n')
			}
			path := writeMuseNative(t, data, museSyntheticID, raw)
			before, err := os.Stat(path)
			must(t, err)
			var listed, listErr bytes.Buffer
			if code := Run([]string{"list", "--source", "local", "--harness", "muse", "--json"}, nil, &listed, &listErr, env); code != 0 || counter.calls.Load() != 0 {
				t.Fatalf("bounded local list: %d %s", code, listErr.String())
			}
			var list struct {
				Domain   string         `json:"domain"`
				Source   string         `json:"source"`
				Sessions []localReadRow `json:"sessions"`
			}
			must(t, json.Unmarshal(listed.Bytes(), &list))
			if list.Domain != "local-read-list" || list.Source != "local" || len(list.Sessions) != 1 || list.Sessions[0].NativeID != museSyntheticID || list.Sessions[0].Title != "" || (list.Sessions[0].Name != "") != tc.named {
				t.Fatal("bounded list invented a prompt title or lost available native identity/name")
			}
			var filtered archive.FilteredTranscript
			counter.after = func(out archive.FilteredTranscript) { filtered = out }
			shown, errOut, code := runShow(t, env, museSyntheticID, "--source", "local", "--harness", "muse", "--transcript", "--json", "--max-bytes", "0")
			if code != 0 || counter.calls.Load() != 1 {
				t.Fatalf("selected local show: %d %s", code, errOut)
			}
			var local struct {
				Domain            string                           `json:"domain"`
				Source            string                           `json:"source"`
				Session           localReadRow                     `json:"session"`
				View              normalizedOutput                 `json:"view"`
				TokenObservations *archive.TokenObservationSummary `json:"token_observations"`
				Observability     map[string]archive.Availability  `json:"observability"`
			}
			must(t, json.Unmarshal([]byte(shown), &local))
			if local.Domain != "local-read-show" || local.Source != "local" || local.Session.NativeID != museSyntheticID || len(local.View.Turns) < 1 || len(local.View.ToolCalls) != tc.calls {
				t.Fatal("local full view lost supported conversation/tool occurrences")
			}
			f := newHandoffFixture(t, false)
			reg := archive.SessionRegistration{ArchiveSessionID: f.id, NativeSessionID: museSyntheticID, ProjectID: "local", ProjectRoot: workspace, Harness: archive.Harness{Name: "muse"}, SessionStartedAt: filtered.NativeStartAt}
			bundle, err := archive.NewSourceBundle(reg, counter.TranscriptFilter, filtered, env.Now(), nil)
			must(t, err)
			analysis, err := analyzeSource(t.Context(), env.Agents, bundle)
			must(t, err)
			labels, _ := archive.LabelsFromAnalysis(analysis)
			if local.Session.Name != labels.Name || local.Session.Title != labels.Title || labels.Title == "" || !reflect.DeepEqual(local.Session.Gaps, bundle.Capture.Gaps) || !reflect.DeepEqual(local.TokenObservations, analysis.TokenObservations) {
				t.Fatal("selected local labels, observations or source gaps disagree with shared Analysis")
			}
			if !local.Observability["structured_counts"].Available() || local.Observability["tool_results"].Available() || local.Observability["files_touched"].Available() || local.Observability["tool_errors"].Available() {
				t.Fatal("local view changed supported count or unavailable tool-result facts")
			}
			display := archive.DisplayTitle(archive.Metadata{Name: labels.Name, Title: labels.Title})
			text, errOut, code := runShow(t, env, museSyntheticID, "--source", "local", "--harness", "muse", "--no-pager")
			if code != 0 || !strings.Contains(text, display) || counter.calls.Load() != 2 {
				t.Fatalf("local label display: %d %s", code, errOut)
			}
			localRecords, errOut, code := evalLines(t, env, "--file", path, "--harness", "muse", "--detail", "full", "--max-bytes", "0")
			if code != 0 || len(localRecords) != 1 || counter.calls.Load() != 3 {
				t.Fatalf("local full export: %d %s", code, errOut)
			}
			var localExport archive.EvalExport
			encoded, err := json.Marshal(localRecords[0])
			must(t, err)
			must(t, json.Unmarshal(encoded, &localExport))
			// Bounded preview search remains name-only. It cannot promote a
			// prompt to an observed title without full conversation analysis.
			var search, searchErr bytes.Buffer
			if code := Run([]string{"list", "--source", "local", "--harness", "muse", "--json", labels.Title}, nil, &search, &searchErr, env); code != 0 || counter.calls.Load() != 3 {
				t.Fatalf("bounded prompt search: %d %s", code, searchErr.String())
			}
			var promptSearch struct {
				Sessions []localReadRow `json:"sessions"`
			}
			must(t, json.Unmarshal(search.Bytes(), &promptSearch))
			if len(promptSearch.Sessions) != 0 {
				t.Fatal("bounded preview search acquired a full prompt title")
			}
			unchanged, err := os.ReadFile(path)
			must(t, err)
			after, err := os.Stat(path)
			must(t, err)
			if !bytes.Equal(raw, unchanged) || !before.ModTime().Equal(after.ModTime()) || before.Mode() != after.Mode() {
				t.Fatal("local consumers mutated native source")
			}
			if _, err := os.Stat(archiveHome); !os.IsNotExist(err) {
				t.Fatalf("local consumers wrote archive state: %v", err)
			}
			packed, err := archive.BuildCompressedSource(bundle)
			must(t, err)
			key, err := archive.SourceObjectKey(bundle, packed.SHA256)
			must(t, err)
			must(t, f.mem.Put(t.Context(), key, packed.Bytes))
			parser, _ := env.agentRegistry().LookupParser("muse")
			metadata, err := archive.BuildMetadataWithAnalysis(bundle, analysis, nil, "synthetic-machine", filtered.NativeStartAt, env.Now(), archive.SourceReference{Key: key, SHA256: packed.SHA256, CompressedBytes: len(packed.Bytes)}, archive.ParserInfo{Name: "muse", Version: parser.Version()})
			must(t, err)
			putMuseViewsMetadata(t, f, metadata)
			must(t, os.Remove(path))
			archived, errOut, code := runShow(t, f.env, f.id, "--harness", "muse", "--transcript", "--json", "--max-bytes", "0")
			if code != 0 {
				t.Fatalf("retained show: %d %s", code, errOut)
			}
			decoder := json.NewDecoder(strings.NewReader(archived))
			var sidecar archive.Metadata
			var retained normalizedOutput
			must(t, decoder.Decode(&sidecar))
			must(t, decoder.Decode(&retained))
			localBody, err := json.Marshal(local.View)
			must(t, err)
			retainedBody, err := json.Marshal(retained)
			must(t, err)
			if !bytes.Equal(localBody, retainedBody) || retained.Trimmed != nil || sidecar.Name != labels.Name || sidecar.Title != labels.Title || sidecar.NativeSessionID != local.Session.NativeID || sidecar.SessionID == local.Session.NativeID {
				t.Fatal("local and retained normalized bodies, labels or identity domains disagree")
			}
			records, errOut, code := evalLines(t, f.env, f.id, "--harness", "muse", "--detail", "full", "--max-bytes", "0")
			if code != 0 || len(records) != 1 {
				t.Fatalf("retained full export: %d %s", code, errOut)
			}
			encoded, err = json.Marshal(records[0])
			must(t, err)
			var retainedExport archive.EvalExport
			must(t, json.Unmarshal(encoded, &retainedExport))
			if localExport.Source != archive.EvalExportSourceLocal || localExport.SessionID != museSyntheticID || retainedExport.Source != archive.EvalExportSourceArchive || retainedExport.SessionID != f.id || retainedExport.NativeSessionID != museSyntheticID {
				t.Fatal("local and retained exports confused native/archive identity domains")
			}
			if !reflect.DeepEqual(localExport.Prompts, retainedExport.Prompts) || !reflect.DeepEqual(localExport.FinalResponse, retainedExport.FinalResponse) || !reflect.DeepEqual(localExport.Counts, retainedExport.Counts) || !reflect.DeepEqual(localExport.Models, retainedExport.Models) || !reflect.DeepEqual(localExport.TokenObservations, retainedExport.TokenObservations) || !reflect.DeepEqual(localExport.CaptureGaps, retainedExport.CaptureGaps) || !reflect.DeepEqual(localExport.ToolsUsed, retainedExport.ToolsUsed) || !reflect.DeepEqual(localExport.FilesEdited, retainedExport.FilesEdited) {
				t.Fatal("local and retained full exports disagree on supported own activity/availability")
			}
			if tc.named && (local.TokenObservations == nil || localExport.Counts.ToolCalls == nil || *localExport.Counts.ToolCalls != tc.calls) {
				t.Fatal("rich fixture lost its observed input/output or own tool count")
			}
			if !slices.ContainsFunc(retainedExport.CaptureGaps, func(g archive.CaptureGap) bool { return g.Code == "child_coverage_unavailable" }) || counter.calls.Load() != 3 {
				t.Fatal("retained readers lost unavailable coverage or reopened native filtering")
			}
		})
	}
}
