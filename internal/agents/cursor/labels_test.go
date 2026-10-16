package cursor

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
)

// labelChatID returns a synthetic composer ID.
func labelChatID(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }

// labelStateDB writes a synthetic state.vscdb with composerData rows and
// closes it, as Cursor leaves it when it quits.
func labelStateDB(t *testing.T, rows map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.vscdb")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `CREATE TABLE cursorDiskKV (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)`); err != nil {
		t.Fatal(err)
	}
	for id, value := range rows {
		if _, err := db.ExecContext(t.Context(), `INSERT INTO cursorDiskKV (key, value) VALUES (?, ?)`, "composerData:"+id, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func labelRequest(n int) agentapi.LabelRequest {
	id := labelChatID(n)
	reg := archive.SessionRegistration{ArchiveSessionID: fmt.Sprintf("archive-%d", n), NativeSessionID: id, Harness: archive.Harness{Name: "cursor"}, Origin: archive.SessionOriginImport}
	return agentapi.LabelRequest{Registration: reg, Context: agentapi.LabelContext{NativeID: id, Ordinary: true, Contract: "collector-contract"}}
}

func TestCursorLabelProviderReadsChatNamesFromComposerData(t *testing.T) {
	t.Parallel()
	composer := func(n int, name string) string {
		return fmt.Sprintf(`{"_v":18,"composerId":%q,"fullConversationHeadersOnly":[]%s}`, labelChatID(n), name)
	}
	db := labelStateDB(t, map[string]string{
		labelChatID(1): composer(1, `,"name":"  Invented chat name  "`),
		labelChatID(2): composer(2, `,"name":""`),
		labelChatID(3): composer(3, ``),
		labelChatID(4): composer(4, `,"name":null`),
		labelChatID(5): composer(5, `,"name":42`),
		labelChatID(7): `{"composerId":"` + labelChatID(8) + `","name":"Another chat's name"}`,
		labelChatID(9): `not json`,
	})
	requests := []agentapi.LabelRequest{}
	for n := 1; n <= 9; n++ {
		requests = append(requests, labelRequest(n))
	}
	before, _ := os.ReadDir(filepath.Dir(db))
	// Codex homes are not where Cursor keeps its database.
	got := (LabelProvider{}).LookupLabels(t.Context(), agentapi.LabelEnvironment{Homes: []string{"/nonexistent/codex"}, CursorDatabase: db}, requests)
	absent := func(n int) archive.SessionLabel {
		return archive.SessionLabel{NativeID: labelChatID(n), State: archive.SessionLabelAbsent, Source: archive.SessionLabelDatabase, Contract: LabelContract}
	}
	want := map[string]archive.SessionLabel{
		"archive-1": {NativeID: labelChatID(1), State: archive.SessionLabelPresent, Name: "Invented chat name", Source: archive.SessionLabelDatabase, Contract: LabelContract},
		"archive-2": absent(2),
		"archive-3": absent(3),
		"archive-4": absent(4),
	}
	if len(got) != len(want) {
		t.Fatalf("labels = %#v, want %#v", got, want)
	}
	for id, label := range want {
		if got[id] != label {
			t.Errorf("%s = %#v, want %#v", id, got[id], label)
		}
	}
	// A non-string name (5), a missing row (6), another chat's row (7) and an
	// unreadable value (9) are unavailable, never absence.
	after, _ := os.ReadDir(filepath.Dir(db))
	if len(after) != len(before) {
		t.Fatalf("lookup created files beside the database: %v -> %v", before, after)
	}
}

func TestCursorLabelProviderReadsALiveWALDatabaseInPlace(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.vscdb")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	for _, stmt := range []string{`PRAGMA journal_mode=WAL`, `CREATE TABLE cursorDiskKV (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)`} {
		if _, err := db.ExecContext(t.Context(), stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO cursorDiskKV VALUES (?, ?)`, "composerData:"+labelChatID(1), `{"name":"Live chat"}`); err != nil {
		t.Fatal(err)
	}
	got := (LabelProvider{}).LookupLabels(t.Context(), agentapi.LabelEnvironment{CursorDatabase: path}, []agentapi.LabelRequest{labelRequest(1)})
	if got["archive-1"].Name != "Live chat" {
		t.Fatalf("labels = %#v", got)
	}
}

func TestCursorLabelProviderLooksUpNothingOutsideItsBounds(t *testing.T) {
	t.Parallel()
	calls := 0
	provider := LabelProvider{read: func(_ context.Context, _ string, ids []string, limit int64) (map[string]cursorstore.ComposerRow, error) {
		calls++
		if limit != labelRowBytes {
			t.Errorf("row limit = %d", limit)
		}
		out := map[string]cursorstore.ComposerRow{}
		for _, id := range ids {
			out[id] = cursorstore.ComposerRow{Value: []byte(`{"name":"Invented"}`)}
		}
		return out, nil
	}}
	many := []agentapi.LabelRequest{}
	for n := range labelRequests + 1 {
		many = append(many, labelRequest(n))
	}
	other := labelRequest(1)
	other.Registration.Harness.Name = "codex"
	badID := labelRequest(2)
	badID.Registration.NativeSessionID, badID.Context.NativeID = "not-a-composer", "not-a-composer"
	stale := labelRequest(3)
	stale.Context.NativeID = labelChatID(4)
	for name, tc := range map[string]struct {
		env      agentapi.LabelEnvironment
		requests []agentapi.LabelRequest
	}{
		"no database":        {agentapi.LabelEnvironment{Homes: []string{"/codex"}}, []agentapi.LabelRequest{labelRequest(1)}},
		"too many requests":  {agentapi.LabelEnvironment{CursorDatabase: "/db"}, many},
		"another harness":    {agentapi.LabelEnvironment{CursorDatabase: "/db"}, []agentapi.LabelRequest{other}},
		"not a composer ID":  {agentapi.LabelEnvironment{CursorDatabase: "/db"}, []agentapi.LabelRequest{badID}},
		"context of another": {agentapi.LabelEnvironment{CursorDatabase: "/db"}, []agentapi.LabelRequest{stale}},
	} {
		if got := provider.LookupLabels(t.Context(), tc.env, tc.requests); len(got) != 0 {
			t.Errorf("%s: labels = %#v", name, got)
		}
	}
	if calls != 0 {
		t.Fatalf("database read %d times for requests it cannot answer", calls)
	}
	if got := provider.LookupLabels(t.Context(), agentapi.LabelEnvironment{CursorDatabase: "/db"}, many[:labelRequests]); len(got) != labelRequests || calls != 1 {
		t.Fatalf("a full batch got %d labels in %d reads", len(got), calls)
	}
}

func TestCursorLabelProviderLeavesOversizedOrUnreadableRowsUnavailable(t *testing.T) {
	t.Parallel()
	provider := LabelProvider{read: func(context.Context, string, []string, int64) (map[string]cursorstore.ComposerRow, error) {
		return map[string]cursorstore.ComposerRow{labelChatID(1): {Oversized: true}}, nil
	}}
	if got := provider.LookupLabels(t.Context(), agentapi.LabelEnvironment{CursorDatabase: "/db"}, []agentapi.LabelRequest{labelRequest(1)}); len(got) != 0 {
		t.Fatalf("oversized row labels = %#v", got)
	}
	missing := filepath.Join(t.TempDir(), "state.vscdb")
	if got := (LabelProvider{}).LookupLabels(t.Context(), agentapi.LabelEnvironment{CursorDatabase: missing}, []agentapi.LabelRequest{labelRequest(1)}); len(got) != 0 {
		t.Fatalf("missing database labels = %#v", got)
	}
	if entries, _ := os.ReadDir(filepath.Dir(missing)); len(entries) != 0 {
		t.Fatalf("lookup created %v", entries)
	}
}

func TestCursorLabelProviderDerivesContextFromTheRegistration(t *testing.T) {
	t.Parallel()
	reg := labelRequest(1).Registration
	got, ok := (LabelProvider{}).RegistrationLabelContext(reg)
	if !ok || got.NativeID != reg.NativeSessionID || !got.Ordinary {
		t.Fatalf("context = %#v, %v", got, ok)
	}
	reg.Harness.Name = "codex"
	if _, ok := (LabelProvider{}).RegistrationLabelContext(reg); ok {
		t.Fatal("a Codex registration got Cursor lookup context")
	}
}

func labelBundle(sourceFormat string, records []map[string]any, labels ...archive.SessionLabel) archive.SourceBundle {
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	b := archive.SourceBundle{SchemaVersion: archive.SourceSchemaVersion, ArchiveSessionID: "archive-1", NativeSessionID: labelChatID(1), ProjectID: "project", Capture: archive.SourceCapture{Harness: archive.Harness{Name: "cursor"}, AdapterName: "cursor", SourceFormat: sourceFormat, FilterVersion: archive.FilterVersion, CapturedAt: at}, NativeRecords: records}
	for _, label := range labels {
		b.SupplementalEvidence = append(b.SupplementalEvidence, label.Evidence(at, "cursor"))
	}
	return b
}

func TestCursorParserNamesATranscriptSessionFromItsChatName(t *testing.T) {
	t.Parallel()
	records := []map[string]any{{"role": "user", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "Invented prompt"}}}}}
	present := archive.SessionLabel{NativeID: labelChatID(1), State: archive.SessionLabelPresent, Name: "Invented chat name", Source: archive.SessionLabelDatabase, Contract: LabelContract}
	for name, tc := range map[string]struct {
		bundle archive.SourceBundle
		want   string
	}{
		"chat name":            {labelBundle("cursor-jsonl", records, present), "Invented chat name"},
		"no label":             {labelBundle("cursor-jsonl", records), ""},
		"confirmed absent":     {labelBundle("cursor-jsonl", records, archive.SessionLabel{NativeID: labelChatID(1), State: archive.SessionLabelAbsent, Source: archive.SessionLabelDatabase, Contract: LabelContract}), ""},
		"other contract":       {labelBundle("cursor-jsonl", records, archive.SessionLabel{NativeID: labelChatID(1), State: archive.SessionLabelPresent, Name: "Unpinned", Source: archive.SessionLabelDatabase, Contract: "codex-files-159.2-v2"}), ""},
		"index source":         {labelBundle("cursor-jsonl", records, archive.SessionLabel{NativeID: labelChatID(1), State: archive.SessionLabelPresent, Name: "Unpinned", Source: archive.SessionLabelIndex, Contract: LabelContract}), ""},
		"composer record wins": {labelBundle("cursor-composer", []map[string]any{{"type": "session", "session_id": labelChatID(1), "name": "Composer name"}}, present), "Composer name"},
	} {
		analysis, err := (Parser{}).Parse(context.Background(), tc.bundle)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if analysis.Facts.Name != tc.want {
			t.Errorf("%s: name = %q, want %q", name, analysis.Facts.Name, tc.want)
		}
	}
}

func TestCursorLabelContractIsAValidLabelToken(t *testing.T) {
	t.Parallel()
	label := archive.SessionLabel{NativeID: labelChatID(1), State: archive.SessionLabelAbsent, Source: archive.SessionLabelDatabase, Contract: LabelContract}
	if _, ok := archive.FilterSessionLabel(label); !ok || LabelContract != "cursor-composer-name-v1" {
		t.Fatalf("contract %q is not a pinned label token", LabelContract)
	}
}
