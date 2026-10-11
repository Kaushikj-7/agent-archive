package cli

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// Regression: 2026-10 the owner's imported agent-created Codex threads
// failed every pass ("current header unavailable"), and two of them, which
// Codex had since renamed into archived_sessions/, kept a transcript_missing
// gap at their old sessions/ locator. An agent-created thread is a top-level
// thread captured like a user's, and a registration follows its rollout into
// archived_sessions/ and back by its thread ID: the registration's locator
// moves, the gap clears, and unchanged content is not uploaded again. The
// legacy variant is a registration imported before sources were bound, as
// the owner's were; in the stale variant Codex's thread index still names
// the old locator, so only the rollout's ID finds it.
func TestImportedAgentCreatedThreadFollowsArchivedRollout(t *testing.T) {
	type variant struct {
		legacy bool
		stale  bool
	}
	for _, tc := range []variant{{false, false}, {true, false}, {false, true}, {true, true}} {
		legacy := tc.legacy
		t.Run(fmt.Sprintf("legacy=%v/stale=%v", tc.legacy, tc.stale), func(t *testing.T) {
			canonical := func() string { p, e := filepath.EvalSymlinks(t.TempDir()); must(t, e); return p }
			home, userHome, project := canonical(), canonical(), canonical()
			native := filepath.Join(userHome, ".codex")
			sessions := filepath.Join(native, "sessions", "2026", "10", "02")
			must(t, os.MkdirAll(sessions, 0700))
			must(t, os.MkdirAll(filepath.Join(native, "archived_sessions"), 0700))
			at := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
			const thread = "01a10dee-f063-7743-8b7b-1ccf59d1a8c7"
			name := "rollout-2026-10-02T12-00-00-" + thread + ".jsonl"
			original, archived := filepath.Join(sessions, name), filepath.Join(native, "archived_sessions", name)
			stamp := at.Format(time.RFC3339Nano)
			meta, e := json.Marshal(map[string]any{"type": "session_meta", "timestamp": stamp, "payload": map[string]any{"id": thread, "session_id": thread, "timestamp": stamp, "cwd": project, "source": "vscode", "originator": "Codex Desktop", "cli_version": "0.160.0", "thread_source": "agent_created_thread"}})
			must(t, e)
			task := fmt.Sprintf(`{"type":"event_msg","timestamp":%q,"payload":{"type":"task_started","turn_id":%q,"started_at":%q}}`, stamp, thread, stamp)
			prompt := `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Synthetic handed-off task"}]}}`
			must(t, os.WriteFile(original, []byte(string(meta)+"\n"+task+"\n"+prompt+"\n"), 0600))
			db, e := sql.Open("sqlite", filepath.Join(native, "state_5.sqlite"))
			must(t, e)
			defer func() { _ = db.Close() }()
			_, e = db.ExecContext(t.Context(), "PRAGMA journal_mode=WAL; CREATE TABLE threads(id TEXT PRIMARY KEY, rollout_path TEXT)")
			must(t, e)
			_, e = db.ExecContext(t.Context(), "INSERT INTO threads VALUES(?,?)", thread, original)
			must(t, e)

			cfg := config.Config{MachineID: "synthetic-machine", Storage: credentialsTestConfig(), Harnesses: []string{"codex"}, ImportedHarnesses: []string{"codex"}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: project, ProjectID: archive.ProjectID(project), Included: true, ActivatedAt: at.Add(time.Hour)}}}}
			must(t, config.Save(home, cfg))
			cloud := storagetest.NewMemoryStore()
			now := at.Add(2 * time.Hour)
			env := setupTestEnv(t, home, userHome, newFakeKeychain(), now)
			env.Now = func() time.Time { return now }
			env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return cloud, nil }
			var out, errOut bytes.Buffer
			if code := Run([]string{"backfill", "--yes", "--background", "--harness", "codex", "--project", project}, nil, &out, &errOut, env); code != 0 {
				t.Fatalf("import failed %d %s %s", code, out.String(), errOut.String())
			}
			reg := theRegistration(t, home)
			if reg.NativeSessionID != thread || reg.Origin != archive.SessionOriginImport || reg.NativeChild || reg.TranscriptPath != original {
				t.Fatalf("agent-created thread not imported as a top-level thread: %+v", reg)
			}
			// settle runs passes until one publishes nothing, defers nothing
			// and fails nothing, and returns every pass's publications.
			settle := func(stage string) []string {
				t.Helper()
				var published []string
				for range 8 {
					now = now.Add(10 * time.Minute)
					result, err := runOnePass(env, true)
					if err != nil || len(result.Errors) > 0 {
						t.Fatalf("%s: %v %v", stage, result.Errors, err)
					}
					published = append(published, result.Published...)
					if len(result.Published) == 0 && len(result.Deferred) == 0 && len(result.Waiting) == 0 {
						return published
					}
				}
				t.Fatalf("%s: passes did not settle", stage)
				return nil
			}
			if published := settle("import"); len(published) != 1 {
				t.Fatalf("import published %v", published)
			}
			if legacy {
				// The owner's registrations predate source bindings.
				local, err := state.Open(home)
				must(t, err)
				reg = theRegistration(t, home)
				reg.CodexBinding = nil
				raw, err := json.Marshal(reg)
				must(t, err)
				must(t, os.WriteFile(filepath.Join(local.Home(), "registrations", reg.ArchiveSessionID+".json"), raw, 0600))
			}
			keys := func() []string {
				objects, err := cloud.List(t.Context(), "sessions/")
				must(t, err)
				var out []string
				for _, object := range objects {
					out = append(out, object.Key)
				}
				slices.Sort(out)
				return out
			}
			uploaded := keys()
			type rename struct {
				from string
				to   string
			}
			for _, move := range []rename{{original, archived}, {archived, original}} {
				must(t, os.Rename(move.from, move.to))
				if !tc.stale {
					_, e = db.ExecContext(t.Context(), "UPDATE threads SET rollout_path=? WHERE id=?", move.to, thread)
					must(t, e)
				}
				stage := "move to " + filepath.Base(filepath.Dir(move.to))
				if published := settle(stage); len(published) != 0 {
					t.Fatalf("%s republished unchanged content: %v", stage, published)
				}
				after := theRegistration(t, home)
				if after.ArchiveSessionID != reg.ArchiveSessionID || after.NativeSessionID != thread || after.Origin != reg.Origin || !after.AdmittedAt.Equal(reg.AdmittedAt) || after.TranscriptPath != move.to {
					t.Fatalf("%s: registration %+v", stage, after)
				}
				// A legacy registration keeps its unbound representation unless a
				// current locator lets the source bind it; either way it follows.
				if after.CodexBinding == nil && !legacy || after.CodexBinding != nil && (after.CodexBinding.Path != move.to || after.CodexBinding.NativeThreadID != thread) {
					t.Fatalf("%s: binding %+v", stage, after.CodexBinding)
				}
				signature, found, err := state.OpenReadOnly(home).LoadScanSignature(reg.ArchiveSessionID)
				if err != nil || !found || signature.Blocked != "" || signature.Failed {
					t.Fatalf("%s: scan signature %+v %v", stage, signature, err)
				}
				if now := keys(); !slices.Equal(now, uploaded) {
					t.Fatalf("%s uploaded again: %v, was %v", stage, now, uploaded)
				}
			}
		})
	}
}
