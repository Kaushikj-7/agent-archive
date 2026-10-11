package cli

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestAdmittedHistoryUsesTrustedReadHomesWithDiscoveryDisabled(t *testing.T) {
	t.Parallel()
	for _, origin := range []archive.SessionOrigin{archive.SessionOriginHook, archive.SessionOriginImport} {
		t.Run(string(origin), func(t *testing.T) {
			t.Parallel()
			canonical := func() string { p, err := filepath.EvalSymlinks(t.TempDir()); must(t, err); return p }
			home, userHome, project := canonical(), canonical(), canonical()
			nativeHome := filepath.Join(userHome, ".codex")
			must(t, os.MkdirAll(filepath.Join(nativeHome, "sessions"), 0700))
			at := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
			const thread = "11111111-1111-4111-8111-111111111111"
			const physical = "22222222-2222-4222-8222-222222222222"
			meta := func(base map[string]any, ordinal int) []byte {
				payload := map[string]any{"id": thread, "timestamp": at.Format(time.RFC3339Nano), "cwd": project, "source": "cli", "originator": "codex_cli_rs", "cli_version": "0.160.0", "history_mode": "paginated"}
				if base != nil {
					payload["history_base"] = base
				}
				raw, err := json.Marshal(map[string]any{"type": "session_meta", "ordinal": ordinal, "timestamp": at.Format(time.RFC3339Nano), "payload": payload})
				must(t, err)
				return append(raw, '\n')
			}
			task := []byte(fmt.Sprintf(`{"type":"event_msg","ordinal":1,"timestamp":%q,"payload":{"type":"task_started","turn_id":"synthetic-task","root_turn_id":"synthetic-task","started_at":%q}}`+"\n", at.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano)))
			seed := filepath.Join(nativeHome, "sessions", "rollout-2026-10-02T12-00-00-"+thread+".jsonl")
			prefix := append(meta(nil, 0), task...)
			must(t, os.WriteFile(seed, append(prefix, []byte(`{"type":"response_item","ordinal":2,"payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"synthetic outgoing-only"}]}}`+"\n")...), 0600))
			current := filepath.Join(nativeHome, "sessions", "rollout-2026-10-02T12-00-00-"+physical+".jsonl")
			raw := meta(map[string]any{"thread_id": thread, "end_ordinal_exclusive": 2, "end_byte_offset": len(prefix)}, 2)
			must(t, os.WriteFile(current, append(raw, []byte(`{"type":"response_item","ordinal":3,"payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"synthetic current-only"}]}}`+"\n")...), 0600))
			db, err := sql.Open("sqlite", filepath.Join(nativeHome, "state_5.sqlite"))
			must(t, err)
			defer func() { _ = db.Close() }()
			_, err = db.ExecContext(t.Context(), "CREATE TABLE threads(id TEXT PRIMARY KEY,rollout_path TEXT)")
			must(t, err)
			_, err = db.ExecContext(t.Context(), "INSERT INTO threads VALUES(?,?)", thread, current)
			must(t, err)
			cfg := config.Config{MachineID: "synthetic-machine", Storage: credentialsTestConfig(), Harnesses: []string{"codex"}, ImportedHarnesses: []string{"codex"}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: project, ProjectID: archive.ProjectID(project), Included: true, ActivatedAt: at.Add(-time.Hour)}}}}
			must(t, config.Save(home, cfg))
			local, err := state.Open(home)
			must(t, err)
			reg := archive.SessionRegistration{ArchiveSessionID: "admitted-history", NativeSessionID: thread, Harness: archive.Harness{Name: "codex"}, ProjectID: archive.ProjectID(project), ProjectRoot: project, TranscriptPath: seed, SessionStartedAt: at, RegisteredAt: at.Add(time.Second), AdmittedAt: at.Add(time.Second), Origin: origin}
			if origin == archive.SessionOriginHook {
				must(t, local.SaveRegistration(reg))
			}
			cloud := storagetest.NewMemoryStore()
			env := setupTestEnv(t, home, userHome, newFakeKeychain(), at.Add(2*time.Minute))
			env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return cloud, nil }
			if origin == archive.SessionOriginImport {
				// Admit through actual backfill while the producer is ordinary, then
				// materialize its related history without changing admission.
				must(t, os.Remove(current))
				ordinary := meta(nil, 0)
				var record map[string]any
				must(t, json.Unmarshal(ordinary, &record))
				delete(record["payload"].(map[string]any), "history_mode")
				ordinary, err = json.Marshal(record)
				must(t, err)
				body := append(append(ordinary, '\n'), task...)
				body = append(body, []byte(`{"type":"response_item","ordinal":2,"payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"synthetic outgoing-only"}]}}`+"\n")...)
				must(t, os.WriteFile(seed, body, 0600))
				_, err = db.ExecContext(t.Context(), "UPDATE threads SET rollout_path=? WHERE id=?", seed, thread)
				must(t, err)
				var out, errOut bytes.Buffer
				if code := Run([]string{"backfill", "--yes", "--background", "--harness", "codex", "--include-temp", "--project", project}, nil, &out, &errOut, env); code != 0 {
					t.Fatal("actual import failed", code, errOut.String(), out.String())
				}
				registrations, err := local.LoadRegistrations()
				must(t, err)
				if len(registrations) != 1 || registrations[0].Origin != archive.SessionOriginImport {
					t.Fatal("backfill did not admit one native session", len(registrations), out.String())
				}
				reg = registrations[0]
				// Settle the confirmed frozen selection before testing subsequent
				// native revision migration; it is independently durable now.
				first, err := runOnePass(env, true)
				must(t, err)
				if len(first.Published) != 1 || len(first.Errors) != 0 {
					t.Fatalf("staged admission did not settle %+v", first)
				}
				env.Now = func() time.Time { return at.Add(6 * time.Minute) }
				prefix = append(meta(nil, 0), task...)
				must(t, os.WriteFile(seed, append(prefix, []byte(`{"type":"response_item","ordinal":2,"payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"synthetic outgoing-only"}]}}`+"\n")...), 0600))
				must(t, os.WriteFile(current, append(raw, []byte(`{"type":"response_item","ordinal":3,"payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"synthetic current-only"}]}}`+"\n")...), 0600))
				_, err = db.ExecContext(t.Context(), "UPDATE threads SET rollout_path=? WHERE id=?", current, thread)
				must(t, err)
			}
			// A fresh eligible-looking native file is still only a read fact:
			// discovery is disabled and it has no admitted archive owner.
			const unadmitted = "33333333-3333-4333-8333-333333333333"
			newRecord := meta(nil, 0)
			var unowned map[string]any
			must(t, json.Unmarshal(newRecord, &unowned))
			unowned["payload"].(map[string]any)["id"] = unadmitted
			newRecord, err = json.Marshal(unowned)
			must(t, err)
			unownedPath := filepath.Join(nativeHome, "sessions", "rollout-2026-10-02T12-00-00-"+unadmitted+".jsonl")
			must(t, os.WriteFile(unownedPath, append(append(newRecord, '\n'), task...), 0600))
			_, err = db.ExecContext(t.Context(), "INSERT INTO threads VALUES(?,?)", unadmitted, unownedPath)
			must(t, err)
			published := false
			for range 12 {
				result, err := runOnePass(env, true)
				must(t, err)
				for _, issue := range result.Errors {
					if !errors.Is(issue, archive.ErrHistoryMutationPending) && !agentapi.HasFailure(issue, agentapi.Unavailable) {
						t.Fatal(result.Errors)
					}
				}
				if len(result.Published) > 0 {
					published = true
					break
				}
			}
			if !published {
				t.Fatal("admitted history did not publish with discovery disabled")
			}
			after, found, err := local.LoadRegistration(reg.ArchiveSessionID)
			must(t, err)
			if !found || after.CodexBinding == nil || after.CodexBinding.Home != nativeHome || after.Origin != origin || !after.AdmittedAt.Equal(reg.AdmittedAt) || after.NativeSessionID != thread {
				t.Fatal("trusted-home migration changed admission", after)
			}
			registrations, err := local.LoadRegistrations()
			must(t, err)
			if len(registrations) != 1 {
				t.Fatal("read-home migration created discovery owners")
			}
			// This current segment reverts the original outgoing prompt. The
			// actual publication must retain that outgoing revision independently.
			objects, err := cloud.List(t.Context(), "")
			must(t, err)
			for _, object := range objects {
				if strings.HasSuffix(object.Key, "metadata.json") {
					metadata, err := reader.ReadMetadata(t.Context(), cloud, object.Key)
					must(t, err)
					currentBundle, err := reader.LoadSource(t.Context(), cloud, metadata, reader.Limits{})
					must(t, err)
					encoded, err := json.Marshal(currentBundle)
					must(t, err)
					if !strings.Contains(string(encoded), "synthetic current-only") || strings.Contains(string(encoded), "synthetic outgoing-only") || metadata.History == nil || len(metadata.History.Preserved) == 0 {
						t.Fatal("revert lost revision boundary/alternative", metadata.History)
					}
					must(t, os.Remove(seed))
					recovered := false
					for _, revision := range metadata.History.Preserved {
						previous, err := reader.LoadRevision(t.Context(), cloud, metadata, revision.RevisionID, reader.Limits{})
						must(t, err)
						data, err := json.Marshal(previous)
						must(t, err)
						recovered = recovered || strings.Contains(string(data), "synthetic outgoing-only")
					}
					if !recovered {
						t.Fatal("outgoing evidence unreadable after native dependency deletion")
					}
				}
			}
			saved := mustLoadConfig(t, home)
			if saved.Discovery != nil && saved.Discovery.Enabled {
				t.Fatal("read-home migration enabled discovery permission")
			}
		})
	}
}
