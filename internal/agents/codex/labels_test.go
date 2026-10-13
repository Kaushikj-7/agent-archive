package codex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/codexmeta"
	_ "github.com/wangjohn/agent-archive/internal/testutil/golden"
)

const labelTestID = "01900000-0000-7000-8000-000000000001"

func labelFixture(t *testing.T) (string, agentapi.LabelRequest) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "sessions")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout.jsonl")
	reg := archive.SessionRegistration{ArchiveSessionID: "synthetic-label", NativeSessionID: labelTestID, Harness: archive.Harness{Name: "codex", Version: "0.159.2"}, TranscriptPath: path}
	bundle := archive.SourceBundle{SchemaVersion: archive.SourceSchemaVersion, ArchiveSessionID: reg.ArchiveSessionID, NativeSessionID: labelTestID, ProjectID: "project", Capture: archive.SourceCapture{Harness: reg.Harness, AdapterName: "codex", AdapterVersion: "0.16.0", FilterVersion: archive.FilterVersion, CapturedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}, NativeRecords: []map[string]any{{"type": "session_meta", "payload": map[string]any{"id": labelTestID, "cli_version": "0.159.2"}}, {"type": "event_msg", "payload": map[string]any{"type": "user_message", "message": "Invented prompt"}}}}
	return root, agentapi.LabelRequest{Registration: reg, Bundle: bundle}
}

func labelIndex(t *testing.T, root string, names ...string) {
	t.Helper()
	out := []byte{}
	for i, name := range names {
		b, _ := json.Marshal(map[string]any{"id": labelTestID, "thread_name": name, "updated_at": time.Date(2030-i, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)})
		out = append(out, b...)
		out = append(out, '\n')
	}
	if err := os.WriteFile(filepath.Join(root, "session_index.jsonl"), out, 0600); err != nil {
		t.Fatal(err)
	}
}

func labelDB(t *testing.T, root string, request agentapi.LabelRequest, mode codexmeta.HistoryMode, name, title string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(root, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), "CREATE TABLE threads(id TEXT PRIMARY KEY,history_mode TEXT,name TEXT,title TEXT,first_user_message TEXT,preview TEXT,source TEXT,cli_version TEXT,rollout_path TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), "INSERT INTO threads VALUES(?,?,?,?,?,?,?,?,?)", labelTestID, mode, name, title, "Invented prompt", "Invented prompt", "\"cli\"", "0.159.2", request.Registration.TranscriptPath); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func lookupLabel(root string, request agentapi.LabelRequest) (archive.SessionLabel, bool) {
	m := (LabelProvider{}).LookupLabels(context.Background(), agentapi.LabelEnvironment{Homes: []string{root}, VerifiedLegacyStorageHomes: []string{root}}, []agentapi.LabelRequest{request})
	l, ok := m[request.Registration.ArchiveSessionID]
	return l, ok
}

func TestLabelsIndexUsesLastUsablePhysicalRecord(t *testing.T) {
	t.Parallel()
	root, request := labelFixture(t)
	labelIndex(t, root, "Old name", "Latest physical name", "")
	got, ok := lookupLabel(root, request)
	if !ok || got.Name != "Latest physical name" {
		t.Fatalf("%+v %v", got, ok)
	}
	labelIndex(t, root, "Invented prompt")
	got, ok = lookupLabel(root, request)
	if !ok || got.State != archive.SessionLabelAbsent {
		t.Fatalf("%+v %v", got, ok)
	}
}

func TestLabelsIndexRequiresVerifiedLegacyContext(t *testing.T) {
	t.Parallel()
	root, request := labelFixture(t)
	labelIndex(t, root, "Index name")
	request.Bundle.Capture.FilterVersion = "16"
	if _, ok := lookupLabel(root, request); ok {
		t.Fatal("older filtered source supplied no native history-mode proof")
	}
	request.Bundle.Capture.FilterVersion = archive.FilterVersion
	request.Bundle.NativeRecords[0]["payload"].(map[string]any)["history_mode"] = "paginated"
	if _, ok := lookupLabel(root, request); ok {
		t.Fatal("missing paginated DB was interpreted as legacy index")
	}
}

func TestLabelsIndexMatchesNativeRequiredStringAndUUIDSemantics(t *testing.T) {
	t.Parallel()
	root, request := labelFixture(t)
	entries := []map[string]any{
		{"id": labelTestID, "thread_name": "Old name", "updated_at": "unknown"},
		{"id": strings.ToUpper(labelTestID), "thread_name": "Current name", "updated_at": ""},
		{"id": labelTestID, "thread_name": "Missing required stamp"},
	}
	data := []byte{}
	for _, entry := range entries {
		encoded, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, encoded...)
		data = append(data, '\n')
	}
	if err := os.WriteFile(filepath.Join(root, "session_index.jsonl"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if label, ok := lookupLabel(root, request); !ok || label.Name != "Current name" {
		t.Fatalf("%+v %v", label, ok)
	}
}

func TestLabelsSettledReadDoesNotMutateNativeHome(t *testing.T) {
	t.Parallel()
	root, request := labelFixture(t)
	labelIndex(t, root, "Index name")
	db := labelDB(t, root, request, codexmeta.CodexHistoryPaginated, "Database name", "Preview title")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	type fileState struct {
		Size     int64
		Modified time.Time
		SHA      [32]byte
	}
	snapshot := func() map[string]fileState {
		t.Helper()
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]fileState{}
		for _, entry := range entries {
			info, err := entry.Info()
			if err != nil {
				t.Fatal(err)
			}
			var sum [32]byte
			if info.Mode().IsRegular() {
				data, err := os.ReadFile(filepath.Join(root, entry.Name()))
				if err != nil {
					t.Fatal(err)
				}
				sum = sha256.Sum256(data)
			}
			out[entry.Name()] = fileState{Size: info.Size(), Modified: info.ModTime(), SHA: sum}
		}
		return out
	}
	before := snapshot()
	if label, ok := lookupLabel(root, request); !ok || label.Name != "Database name" {
		t.Fatalf("%+v %v", label, ok)
	}
	if !reflect.DeepEqual(before, snapshot()) {
		t.Fatal("read-only resolver changed native contents, metadata or directory entries")
	}
}

func TestLabelsSettledCanonicalStoragePrecedence(t *testing.T) {
	// Run precedence checks before the parallel full-history fixtures: native
	// database observations intentionally become unavailable after 100 ms.
	for _, tc := range []struct {
		mode  codexmeta.HistoryMode
		name  string
		title string
		want  string
	}{{codexmeta.CodexHistoryLegacy, "Unused name", "Database title", "Database title"}, {codexmeta.CodexHistoryLegacy, "Unused name", "Invented prompt", "Index name"}, {codexmeta.CodexHistoryPaginated, "Canonical name", "Stale title", "Canonical name"}, {codexmeta.CodexHistoryPaginated, "Invented prompt", "Stale title", "Invented prompt"}, {codexmeta.CodexHistoryPaginated, "", "Stale title", ""}} {
		t.Run(string(tc.mode)+tc.want, func(t *testing.T) {
			root, request := labelFixture(t)
			labelIndex(t, root, "Index name")
			db := labelDB(t, root, request, tc.mode, tc.name, tc.title)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			got, ok := lookupLabel(root, request)
			if !ok || got.Name != tc.want {
				t.Fatalf("%+v %v want %q", got, ok, tc.want)
			}
		})
	}
}

func TestLabelsSettledDatabaseUnavailableAfterDeadline(t *testing.T) {
	root, request := labelFixture(t)
	labelIndex(t, root, "Index name")
	db := labelDB(t, root, request, codexmeta.CodexHistoryLegacy, "Unused name", "Invented prompt")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc)
	}{
		{"expired", func() (context.Context, context.CancelFunc) {
			return context.WithDeadline(context.Background(), time.Unix(0, 0))
		}},
		{"canceled", func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx, cancel
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := tc.ctx()
			defer cancel()
			rows, settled, absent := readLabelDatabase(ctx, root, []agentapi.LabelRequest{request})
			if len(rows) != 0 || settled || absent {
				t.Fatalf("interrupted database supplied authority: rows=%v settled=%v absent=%v", rows, settled, absent)
			}
			if labels := lookupLabelHome(ctx, root, []agentapi.LabelRequest{request}, true); len(labels) != 0 {
				t.Fatalf("interrupted database allowed index fallback: %+v", labels)
			}
		})
	}
}

func TestLabelsUnfinishedIndexAndLiveWALAreUnavailable(t *testing.T) {
	t.Parallel()
	root, request := labelFixture(t)
	labelIndex(t, root, "Name")
	f, err := os.OpenFile(filepath.Join(root, "session_index.jsonl"), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString("{\"id\":")
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := lookupLabel(root, request); ok {
		t.Fatal("unfinished append was authoritative")
	}
	labelIndex(t, root, "Name")
	db := labelDB(t, root, request, codexmeta.CodexHistoryPaginated, "Live name", "")
	if _, err := db.ExecContext(context.Background(), "PRAGMA journal_mode=WAL"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), "UPDATE threads SET name='Changed live name'"); err != nil {
		t.Fatal(err)
	}
	if _, ok := lookupLabel(root, request); ok {
		t.Fatal("live WAL was authoritative")
	}
}

func TestLabelsRejectUnknownProducerRelocationAndHome(t *testing.T) {
	t.Parallel()
	root, request := labelFixture(t)
	labelIndex(t, root, "Name")
	request.Bundle.Capture.Harness.Version = "0.160.0"
	if _, ok := lookupLabel(root, request); ok {
		t.Fatal("unknown producer accepted")
	}
	request.Bundle.Capture.Harness.Version = "0.159.2"
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte("sqlite_home = '/elsewhere'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, ok := lookupLabel(root, request); ok {
		t.Fatal("relocation accepted")
	}
	if err := os.Remove(filepath.Join(root, "config.toml")); err != nil {
		t.Fatal(err)
	}
	request.Registration.TranscriptPath = "/outside/session.jsonl"
	if _, ok := lookupLabel(root, request); ok {
		t.Fatal("foreign home accepted")
	}
}

func TestLabelsCanonicalDatabaseRequiresRetainedSupportedProducer(t *testing.T) {
	t.Parallel()
	root, request := labelFixture(t)
	db := labelDB(t, root, request, codexmeta.CodexHistoryPaginated, "Canonical name", "")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	request.Bundle.Capture.Harness.Version = "0.160.0"
	if label, ok := lookupLabel(root, request); ok {
		t.Fatalf("unknown retained producer accepted: %+v", label)
	}
}

func TestLabelsCanonicalDatabaseRejectsUnknownValueShape(t *testing.T) {
	t.Parallel()
	root, request := labelFixture(t)
	db := labelDB(t, root, request, codexmeta.CodexHistoryPaginated, "Canonical name", "")
	if _, err := db.ExecContext(context.Background(), "UPDATE threads SET name=CAST(name AS BLOB)"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if label, ok := lookupLabel(root, request); ok {
		t.Fatalf("unknown database name shape accepted: %+v", label)
	}
}

func TestLabelsHomeBudgetFollowsPersistentTargetOrder(t *testing.T) {
	t.Parallel()
	firstRoot, first := labelFixture(t)
	secondRoot, second := labelFixture(t)
	second.Registration.ArchiveSessionID = "second"
	env := agentapi.LabelEnvironment{Homes: []string{firstRoot, secondRoot}}
	for _, requests := range [][]agentapi.LabelRequest{{first, second}, {second, first}} {
		ctx, cancel := context.WithCancel(context.Background())
		var homes []string
		provider := LabelProvider{homeLookup: func(_ context.Context, root string, _ []agentapi.LabelRequest) map[string]archive.SessionLabel {
			homes = append(homes, root)
			cancel()
			return nil
		}}
		provider.LookupLabels(ctx, env, requests)
		cancel()
		want := firstRoot
		if requests[0].Registration.ArchiveSessionID == "second" {
			want = secondRoot
		}
		if len(homes) != 1 || homes[0] != want {
			t.Fatalf("budget selected %v, expected first target home %s", homes, want)
		}
	}
}

func TestLabelsAbsentDatabaseRequiresPositiveProducingStorageProof(t *testing.T) {
	t.Parallel()
	root, request := labelFixture(t)
	labelIndex(t, root, "Stale index name")
	env := agentapi.LabelEnvironment{Homes: []string{root}}
	if labels := (LabelProvider{}).LookupLabels(context.Background(), env, []agentapi.LabelRequest{request}); len(labels) != 0 {
		t.Fatalf("local absence inferred producing placement: %+v", labels)
	}
	env.VerifiedLegacyStorageHomes = []string{t.TempDir()}
	if labels := (LabelProvider{}).LookupLabels(context.Background(), env, []agentapi.LabelRequest{request}); len(labels) != 0 {
		t.Fatal("another home's proof authorized this home")
	}
	env.VerifiedLegacyStorageHomes = []string{root}
	if labels := (LabelProvider{}).LookupLabels(context.Background(), env, []agentapi.LabelRequest{request}); labels[request.Registration.ArchiveSessionID].Name != "Stale index name" {
		t.Fatalf("positive producing proof unavailable: %+v", labels)
	}
}

func TestLabelsUnknownFilteredHistoryModeCannotProveLegacyAbsence(t *testing.T) {
	t.Parallel()
	for _, mode := range []any{"unsupported", nil, true} {
		root, request := labelFixture(t)
		labelIndex(t, root, "Index name")
		request.Bundle.NativeRecords[0]["payload"].(map[string]any)["history_mode"] = mode
		var native bytes.Buffer
		for _, record := range request.Bundle.NativeRecords {
			if err := json.NewEncoder(&native).Encode(record); err != nil {
				t.Fatal(err)
			}
		}
		filtered, err := (Filter{}).FilterJSONL(&native)
		if err != nil {
			t.Fatal(err)
		}
		request.Bundle.Capture.Gaps = filtered.Gaps
		request.Bundle.NativeRecords = nil
		for _, raw := range filtered.Records {
			var record map[string]any
			if err := json.Unmarshal(raw, &record); err != nil {
				t.Fatal(err)
			}
			request.Bundle.NativeRecords = append(request.Bundle.NativeRecords, record)
		}
		if _, ok := lookupLabel(root, request); ok {
			t.Fatalf("omitted mode %v authorized index-only label", mode)
		}
	}
}

func TestLabelsCanonicalDatabaseRequiresPinnedColumnAffinity(t *testing.T) {
	t.Parallel()
	root, request := labelFixture(t)
	db := labelDB(t, root, request, codexmeta.CodexHistoryPaginated, "Canonical name", "")
	if _, err := db.ExecContext(context.Background(), "ALTER TABLE threads RENAME TO old_threads"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), "CREATE TABLE threads(id TEXT PRIMARY KEY,history_mode TEXT,name BLOB,title TEXT,first_user_message TEXT,preview TEXT,source TEXT,cli_version TEXT,rollout_path TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), "INSERT INTO threads SELECT * FROM old_threads"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if label, ok := lookupLabel(root, request); ok {
		t.Fatalf("unknown declared name schema accepted: %+v", label)
	}
}

func TestLabelsCodexInterpreterRejectsNonNativeOwnerAndUnknownContract(t *testing.T) {
	t.Parallel()
	root, request := labelFixture(t)
	request.Registration.NativeSessionID = "generic-thread"
	request.Bundle.NativeSessionID = "generic-thread"
	request.Bundle.NativeRecords[0]["payload"].(map[string]any)["id"] = "generic-thread"
	if _, ok := lookupLabel(root, request); ok {
		t.Fatal("generic ID elevated to native UUID")
	}
	label := archive.SessionLabel{NativeID: labelTestID, State: archive.SessionLabelPresent, Name: "Unsupported native interpretation", Source: archive.SessionLabelDatabase, Contract: "unknown-provider-v1"}
	_, request = labelFixture(t)
	request.Bundle.SupplementalEvidence = []archive.SupplementalEvidence{label.Evidence(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), "codex")}
	analysis, err := (Parser{}).Parse(context.Background(), request.Bundle)
	if err != nil {
		t.Fatal(err)
	}
	if analysis.Facts.Name == label.Name {
		t.Fatal("opaque generic contract elevated to known Codex semantics")
	}
}

func TestLabelsGroupPriorityUsesVerifiedHomeWithoutNativeIO(t *testing.T) {
	t.Parallel()
	root, first := labelFixture(t)
	otherRoot, other := labelFixture(t)
	env := agentapi.LabelEnvironment{Homes: []string{root, otherRoot}}
	provider := LabelProvider{}
	firstGroup := provider.LabelRequestGroup(env, first)
	if len(firstGroup) != 64 {
		t.Fatal("verified home supplied no content-free group")
	}
	second := first
	second.Registration.ArchiveSessionID = "other-admitted-id"
	if provider.LabelRequestGroup(env, second) != firstGroup {
		t.Fatal("one home's requests did not share priority")
	}
	if group := provider.LabelRequestGroup(env, other); group == "" || group == firstGroup {
		t.Fatal("separate approved homes shared priority")
	}
	first.Registration.TranscriptPath = "/outside/transcript.jsonl"
	if provider.LabelRequestGroup(env, first) != "" {
		t.Fatal("unverified home supplied native priority")
	}
}
