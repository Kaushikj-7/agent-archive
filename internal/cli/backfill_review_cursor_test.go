package cli

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/state"
)

func addRecoveryReviewCursorChat(t *testing.T, f *recoveryReviewFixture) {
	t.Helper()
	path := f.env.cursorDatabase()
	must(t, os.MkdirAll(filepath.Dir(path), 0700))
	db, err := sql.Open("sqlite", path)
	must(t, err)
	defer func() { must(t, db.Close()) }()
	_, err = db.ExecContext(t.Context(), "CREATE TABLE cursorDiskKV (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)")
	must(t, err)
	composer, err := json.Marshal(map[string]any{"_v": 18, "composerId": "synthetic-cursor-only", "createdAt": time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC).UnixMilli(), "status": "completed", "workspaceIdentifier": map[string]any{"uri": "file://" + f.missing}, "fullConversationHeadersOnly": []map[string]any{{"bubbleId": "m1", "type": 1}}})
	must(t, err)
	_, err = db.ExecContext(t.Context(), "INSERT INTO cursorDiskKV VALUES (?, ?)", "composerData:synthetic-cursor-only", composer)
	must(t, err)
	_, err = db.ExecContext(t.Context(), "INSERT INTO cursorDiskKV VALUES (?, ?)", "bubbleId:synthetic-cursor-only:m1", `{"_v":3,"bubbleId":"m1","type":1,"text":"Private synthetic mixed-cwd prompt"}`)
	must(t, err)
}

func TestRecoveryReviewDatabaseChoicesRespectSourceCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name       string
		withFile   bool
		sameKey    bool
		historical bool
		want       int
	}{
		{name: "database historical", historical: true, want: 1},
		{name: "mixed configured", withFile: true, want: 1},
		{name: "mixed historical", withFile: true, historical: true, want: 2},
		{name: "shared-key mixed configured", withFile: true, sameKey: true, want: 1},
		{name: "shared-key mixed historical", withFile: true, sameKey: true, historical: true, want: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRecoveryReviewFixture(t)
			addRecoveryReviewCursorChat(t, f)
			const fileID = "55555555-5555-4555-8555-555555555555"
			if tc.withFile {
				f.codex(t, fileID, f.missing, "")
				if tc.sameKey {
					path := filepath.Join(f.userHome, ".codex", "sessions", "rollout-"+fileID+".jsonl")
					data, err := os.ReadFile(path)
					must(t, err)
					must(t, os.WriteFile(path, bytes.ReplaceAll(data, []byte(`,"git":{"repository_url":"https://example.test/acme/repo"}`), nil), 0600))
				}
			}
			output := filepath.Join(t.TempDir(), "mixed-reviewed.json")
			input := "1\nexport\n"
			if tc.historical {
				input = "new\nRetired mixed project\nexport\n"
			}
			before := snapshotTree(t, f.home)
			out, errout, code := f.command(t, input, "backfill", "review-projects", "--output", output)
			if code != 0 || strings.Count(out, "Choose an exact destination") != 1 || !strings.Contains(out, "Database-only Cursor candidates: 1; use a historical destination") {
				t.Fatal("review did not disclose the supported database assignment route", code, out, errout)
			}
			if strings.Contains(out, "compatible exact assignment for at least one observed group") != tc.withFile || strings.Contains(out, "Private synthetic mixed-cwd prompt") || strings.Contains(out, "synthetic-cursor-only") || snapshotTree(t, f.home) != before {
				t.Fatal("database group created unsupported configured authority or hid a compatible file choice", out)
			}
			if tc.sameKey && !strings.Contains(out, "Candidates: 2") {
				t.Fatal("shared-key fixture did not fold file/database counts into one group", out)
			}
			data, err := os.ReadFile(output)
			must(t, err)
			var assignments backfill.Filters
			must(t, backfill.ReadMappingFile(bytes.NewReader(data), &assignments))
			if len(assignments.ProjectMappings)+len(assignments.HistoricalMappings) != 1 || (assignments.HistoricalMappings[f.missing] != "") != tc.historical {
				t.Fatal("mixed-cwd review widened or changed its exact assignment", assignments)
			}
			var plan backfill.Plan
			f.env.observeBackfillPlan = func(observed backfill.Plan) { plan = observed }
			out, errout, code = f.command(t, "", "backfill", "--yes", "--background", "--mapping-file", output)
			regs, err := state.OpenReadOnly(f.home).LoadRegistrations()
			must(t, err)
			if code != 0 || len(regs) != tc.want {
				t.Fatal("mixed-cwd assignment did not preserve per-source import support", code, regs, out, errout)
			}
			for _, reg := range regs {
				if (reg.HistoricalProject != nil) != tc.historical || !tc.historical && reg.NativeSessionID != fileID {
					t.Fatal("configured choice acquired a database-only owner or historical choice lost its identity", reg)
				}
			}
			if !tc.historical && plan.Skipped()[backfill.SkipWorktreeUnresolved] != 1 {
				t.Fatal("configured choice concealed the unresolved database-only source", plan.Skipped())
			}
		})
	}
}
