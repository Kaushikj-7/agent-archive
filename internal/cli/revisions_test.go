package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agents/codex"
	"github.com/wangjohn/agent-archive/internal/agents/nativecodec"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

const revisionActive = "22222222-2222-4222-8222-222222222222"

const revisionOld = "33333333-3333-4333-8333-333333333333"

func revisionFixture(t *testing.T, legacy bool) (Env, *storagetest.MemoryStore, string, archive.Metadata) {
	t.Helper()
	const id = "0123456789abcdef0123456789abcdef"
	const thread = "11111111-1111-4111-8111-111111111111"
	home, project := t.TempDir(), t.TempDir()
	at := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, project, at.Add(-24*time.Hour))
	store := storagetest.NewMemoryStore()
	env := testEnv(t, home, at.Add(time.Hour))
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return store, nil }
	makeBundle := func(revision, prompt, filter string, captured time.Time, schema int) archive.Metadata {
		raw := `{"type":"turn_context","payload":{"model":"synthetic"}}` + "\n" + `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"` + prompt + `"}]}}` + "\n"
		filtered, err := (codex.Filter{}).FilterJSONL(strings.NewReader(raw))
		must(t, err)
		bundle, err := archive.NewSourceBundle(archive.SessionRegistration{ArchiveSessionID: id, NativeSessionID: thread, ProjectID: archive.ProjectID(project), ProjectRoot: project, Harness: archive.Harness{Name: "codex"}, SessionStartedAt: at.Add(-time.Hour)}, codex.Filter{}, filtered, captured, nil)
		must(t, err)
		bundle.SchemaVersion = schema
		bundle.Capture.FilterVersion = filter
		if schema == archive.HistorySourceSchemaVersion {
			bundle.Ordinals = []uint64{0, 1}
			bundle.History = &archive.SourceHistory{ThreadID: thread, ActiveRolloutID: revision, Spans: []archive.HistorySpan{{ThreadID: thread, RolloutID: revision, EndRecord: 2, EndOrdinal: 2}}}
		}
		packed, err := archive.BuildCompressedSource(bundle)
		must(t, err)
		key, err := archive.SourceObjectKey(bundle, packed.SHA256)
		must(t, err)
		must(t, store.Put(t.Context(), key, packed.Bytes))
		analysis, err := nativecodec.ParseCodex(t.Context(), bundle)
		must(t, err)
		meta, err := archive.BuildMetadataWithAnalysis(bundle, analysis, nil, "synthetic-machine", at.Add(-time.Hour), at.Add(time.Hour), archive.SourceReference{Key: key, SHA256: packed.SHA256, CompressedBytes: len(packed.Bytes)}, archive.ParserInfo{})
		must(t, err)
		return meta
	}
	oldSchema := archive.HistorySourceSchemaVersion
	if legacy {
		oldSchema = archive.SourceSchemaVersion
	}
	old := makeBundle(revisionOld, "old preserved prompt", "14", at, oldSchema)
	active := makeBundle(revisionActive, "active prompt", "15", at.Add(time.Hour), archive.HistorySourceSchemaVersion)
	active.History.Preserved = []archive.RevisionReference{{RevisionID: revisionOld, CapturedAt: old.CapturedAt, Source: old.SourceBundle}}
	if !legacy {
		active.History.Preserved[0].FilterVersion = "14"
		active.History.Preserved[0].SourceSchemaVersion = oldSchema
	}
	active.LinkedSessions = []archive.LinkedSessionReference{{SessionID: "unavailable-child", Relationship: "child", Status: archive.LinkedSessionPublished, ObservedAt: at}}
	data, err := json.Marshal(active)
	must(t, err)
	key, err := archive.MetadataObjectKey("codex", id)
	must(t, err)
	must(t, store.Put(t.Context(), key, data))
	return env, store, id, active
}

func TestRevisionInventoryAndConsumers(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "history", true: "legacy"}[legacy], func(t *testing.T) {
			env, store, id, metadata := revisionFixture(t, legacy)
			var out, errs bytes.Buffer
			run := func(args ...string) int { out.Reset(); errs.Reset(); return Run(args, nil, &out, &errs, env) }
			if code := run("show", id, "--revisions", "--json"); code != 0 {
				t.Fatalf("inventory=%d %s", code, &errs)
			}
			var inventory struct {
				HasHistory bool `json:"has_history"`
				Revisions  []struct {
					RevisionID string `json:"revision_id"`
					Active     bool   `json:"active"`
					Filter     string `json:"filter_version"`
				} `json:"revisions"`
			}
			must(t, json.Unmarshal(out.Bytes(), &inventory))
			if !inventory.HasHistory || len(inventory.Revisions) != 2 || inventory.Revisions[0].RevisionID != revisionActive || !inventory.Revisions[0].Active || inventory.Revisions[1].RevisionID != revisionOld || inventory.Revisions[1].Active {
				t.Fatalf("inventory=%s", &out)
			}
			if legacy && inventory.Revisions[1].Filter != "" {
				t.Fatal("borrowed active provenance")
			}
			if code := run("show", id, "--revision", revisionOld, "--json"); code != 0 {
				t.Fatalf("show=%d %s", code, &errs)
			}
			var view sessionView
			must(t, json.Unmarshal(out.Bytes(), &view))
			if view.SelectedRevisionID != revisionOld || view.FilterVersion != "14" || view.CapturedAt.Hour() != 0 || len(view.LinkedSessions) != 0 || len(view.LinkedAvailability) != 0 || strings.Contains(out.String(), "active prompt") {
				t.Fatalf("historical metadata=%s", &out)
			}
			if code := run("show", id, "--revision", revisionOld, "--transcript", "--json"); code != 0 || !strings.Contains(out.String(), "old preserved prompt") || strings.Contains(out.String(), "active prompt") {
				t.Fatalf("transcript=%d %s %s", code, &out, &errs)
			}
			if code := run("handoff", id, "--source", "archive", "--revision", revisionOld, "--format", "json"); code != 0 {
				t.Fatalf("handoff=%d %s", code, &errs)
			}
			var handoff archive.Handoff
			must(t, json.Unmarshal(out.Bytes(), &handoff))
			if handoff.Session.RevisionID != revisionOld || handoff.Session.FilterVersion != "14" || handoff.Session.CapturedAt.Hour() != 0 || !strings.Contains(out.String(), "old preserved prompt") || strings.Contains(out.String(), "active prompt") {
				t.Fatalf("handoff provenance=%s", &out)
			}
			if code := run("eval", "export", id, "--revision", revisionOld); code != 0 {
				t.Fatalf("eval=%d %s %s", code, &out, &errs)
			}
			var record archive.EvalExport
			must(t, json.Unmarshal(out.Bytes(), &record))
			must(t, archive.ValidateEvalExport(record))
			if record.RevisionID != revisionOld || record.FilterVersion != "14" || record.CapturedAt.Hour() != 0 || !strings.Contains(out.String(), "old preserved prompt") || strings.Contains(out.String(), "active prompt") {
				t.Fatalf("eval provenance=%s", &out)
			}
			if code := run("show", id, "--transcript", "--json"); code != 0 || !strings.Contains(out.String(), "active prompt") {
				t.Fatalf("default=%d %s %s", code, &out, &errs)
			}
			must(t, store.Delete(t.Context(), metadata.SourceBundle.Key))
			if code := run("show", id, "--revision", revisionOld, "--transcript", "--json"); code != 0 {
				t.Fatalf("historical read depended on unavailable active source: %d %s", code, &errs)
			}
		})
	}
}

func TestRevisionSelectionFailsClosed(t *testing.T) {
	env, store, id, metadata := revisionFixture(t, false)
	for _, command := range [][]string{{"show", id, "--revision", "sessions/codex/arbitrary"}, {"handoff", id, "--source", "archive", "--revision", "unknown"}, {"eval", "export", id, "--revision", "unknown"}} {
		var out, errs bytes.Buffer
		if code := Run(command, nil, &out, &errs, env); code != 1 || strings.Contains(out.String(), "active prompt") {
			t.Fatalf("%v = %d %s %s", command, code, &out, &errs)
		}
	}
	metadata.History.Preserved[0].RevisionID = metadata.History.CurrentRevision
	data, err := json.Marshal(metadata)
	must(t, err)
	key, err := archive.MetadataObjectKey("codex", id)
	must(t, err)
	must(t, store.Put(t.Context(), key, data))
	for _, args := range [][]string{{"show", id, "--revisions", "--json"}, {"show", id, "--revision", revisionActive}} {
		var out, errs bytes.Buffer
		if code := Run(args, nil, &out, &errs, env); code != 1 {
			t.Fatalf("malformed accepted %d %s %s", code, &out, &errs)
		}
	}
}

func TestRevisionInvalidCombinationsBeforeSetup(t *testing.T) {
	env := testEnv(t, t.TempDir(), time.Now())
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		t.Fatal("opened storage for usage error")
		return nil, nil
	}
	for _, args := range [][]string{{"show", "--revisions"}, {"show", "id", "--revisions", "--transcript"}, {"show", "id", "--revisions", "--revision", revisionOld}, {"handoff", "id", "--revision", revisionOld}, {"handoff", "--latest", "--source", "archive", "--revision", revisionOld}, {"eval", "export", "0123456789abcdef0123456789abcdef", "--revision", revisionOld, "--detail", "metadata"}, {"eval", "export", "--scan", "--revision", revisionOld}, {"eval", "export", "--ids-from", "-", "--revision", revisionOld}} {
		var out, errs bytes.Buffer
		if code := Run(args, nil, &out, &errs, env); code != 2 {
			t.Fatalf("%v = %d %s", args, code, &errs)
		}
	}
}

func TestRevisionInventoryWithoutHistory(t *testing.T) {
	_, _, _, metadata := revisionFixture(t, true)
	metadata.SchemaVersion = archive.MetadataSchemaVersion
	metadata.History = nil
	var out, errs bytes.Buffer
	if code := printRevisionInventory(&out, &errs, metadata, false); code != 0 || !strings.Contains(out.String(), "No native revision history") {
		t.Fatalf("human inventory=%d %s %s", code, &out, &errs)
	}
	out.Reset()
	errs.Reset()
	if code := printRevisionInventory(&out, &errs, metadata, true); code != 0 {
		t.Fatalf("JSON inventory=%d %s", code, &errs)
	}
	var result struct {
		HasHistory bool              `json:"has_history"`
		Revisions  []json.RawMessage `json:"revisions"`
	}
	must(t, json.Unmarshal(out.Bytes(), &result))
	if result.HasHistory || result.Revisions == nil || len(result.Revisions) != 0 {
		t.Fatalf("empty inventory=%s", &out)
	}
}

func TestEmptyRevisionSelectorBeforeSetup(t *testing.T) {
	env := testEnv(t, t.TempDir(), time.Now())
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		t.Fatal("opened storage for an empty revision selector")
		return nil, nil
	}
	for _, args := range [][]string{
		{"show", "0123456789abcdef0123456789abcdef", "--revision", ""},
		{"handoff", "0123456789abcdef0123456789abcdef", "--source", "archive", "--revision", ""},
		{"eval", "export", "0123456789abcdef0123456789abcdef", "--revision", ""},
	} {
		var out, errs bytes.Buffer
		if code := Run(args, nil, &out, &errs, env); code != 2 || !strings.Contains(errs.String(), "--revision needs a nonempty revision ID") {
			t.Errorf("%v = %d %s", args, code, &errs)
		}
	}
}
