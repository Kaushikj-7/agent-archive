package cli

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type trustedHomeSpelling string

const (
	trustedHomePhysical trustedHomeSpelling = "physical"
	trustedHomeSymlink  trustedHomeSpelling = "symlink"
	trustedHomePlatform trustedHomeSpelling = "platform"
)

// Exercise the production registry and command through planning, confirmed
// admission and verified readback, using synthetic native files and storage.
func TestBackfillImportsHistoryThroughTrustedHomeAlias(t *testing.T) {
	// Run owns process-wide trace counters, so command tests stay sequential.
	for _, spelling := range []trustedHomeSpelling{trustedHomePhysical, trustedHomeSymlink, trustedHomePlatform} {
		t.Run(string(spelling), func(t *testing.T) {
			canonical := func(path string) string { p, err := filepath.EvalSymlinks(path); must(t, err); return p }
			data, rawUser := canonical(t.TempDir()), t.TempDir()
			physicalUser := canonical(rawUser)
			var user string
			switch spelling {
			case trustedHomePhysical:
				user = physicalUser
			case trustedHomeSymlink:
				user = filepath.Join(t.TempDir(), "trusted-home")
				must(t, os.Symlink(physicalUser, user))
			case trustedHomePlatform:
				user = rawUser
				if user == physicalUser {
					t.Skip("temporary home has no platform alias")
				}
			}
			at := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
			cwd := filepath.Join(physicalUser, "retired-project")
			root := filepath.Join(user, ".codex")
			must(t, os.MkdirAll(filepath.Join(root, "sessions"), 0700))
			const thread = "11111111-1111-4111-8111-111111111111"
			const middle = "22222222-2222-4222-8222-222222222222"
			const tip = "33333333-3333-4333-8333-333333333333"
			header := func(ordinal int, base any) []byte {
				payload := map[string]any{"id": thread, "cwd": cwd, "timestamp": "2026-10-11T12:00:00Z", "source": "cli", "originator": "codex_cli_rs", "cli_version": "0.160.0", "history_mode": "paginated"}
				if base != nil {
					payload["history_base"] = base
				}
				raw, err := json.Marshal(map[string]any{"type": "session_meta", "ordinal": ordinal, "payload": payload})
				must(t, err)
				return append(raw, '\n')
			}
			message := func(ordinal int) []byte {
				return []byte(fmt.Sprintf(`{"type":"response_item","ordinal":%d,"payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Synthetic alias history %d"}]}}`+"\n", ordinal, ordinal))
			}
			first := append(header(0, nil), []byte(`{"type":"event_msg","ordinal":1,"payload":{"type":"task_started","turn_id":"synthetic-task","root_turn_id":"synthetic-task","started_at":"2026-10-11T12:00:00Z"}}`+"\n")...)
			mid := append(header(2, map[string]any{"thread_id": thread, "end_ordinal_exclusive": 2, "end_byte_offset": len(first)}), message(3)...)
			last := append(header(4, map[string]any{"thread_id": middle, "end_ordinal_exclusive": 4, "end_byte_offset": len(mid)}), message(5)...)
			var selected string
			for id, raw := range map[string][]byte{thread: first, middle: mid, tip: last} {
				path := filepath.Join(root, "sessions", "rollout-"+id+".jsonl")
				must(t, os.WriteFile(path, raw, 0600))
				if id == tip {
					selected = path
				}
			}
			db, err := sql.Open("sqlite", filepath.Join(root, "state_5.sqlite"))
			must(t, err)
			defer func() { must(t, db.Close()) }()
			_, err = db.ExecContext(t.Context(), "CREATE TABLE threads(id TEXT PRIMARY KEY,rollout_path TEXT)")
			must(t, err)
			_, err = db.ExecContext(t.Context(), "INSERT INTO threads VALUES(?,?)", thread, selected)
			must(t, err)
			cfg := config.Config{MachineID: "synthetic-machine", Storage: credentialsTestConfig(), Harnesses: []string{"codex"}, Archive: archive.Config{Enabled: true}}
			must(t, config.Save(data, cfg))
			bucket := storagetest.NewMemoryStore()
			env := setupTestEnv(t, data, user, newFakeKeychain(), at)
			env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return bucket, nil }
			env.repoKey = func(string) string { t.Fatal("historical import invoked Git"); return "" }
			mapping := writeHistoricalMapping(t, cwd)
			var plan backfill.Plan
			env.observeBackfillPlan = func(p backfill.Plan) { plan = p }
			beforeData, beforeNative := snapshotArchiveTree(t, data), snapshotTree(t, physicalUser)
			var out, stderr bytes.Buffer
			if code := Run([]string{"backfill", "--dry-run", "--json", "--harness", "codex", "--mapping-file", mapping}, nil, &out, &stderr, env); code != 0 {
				t.Fatal("alias preview failed", code, stderr.String(), out.String())
			}
			planned := 0
			for _, candidate := range plan.Candidates {
				if candidate.Skip == "" {
					planned++
				}
			}
			if planned != 1 {
				t.Fatalf("alias preview did not select one logical owner: %+v", plan.Candidates)
			}
			if snapshotArchiveTree(t, data) != beforeData || snapshotTree(t, physicalUser) != beforeNative || bucketSnapshot(t, bucket) != "" {
				t.Fatal("preview changed native, archive or remote data")
			}
			out.Reset()
			stderr.Reset()
			if code := Run([]string{"backfill", "--yes", "--harness", "codex", "--mapping-file", mapping}, nil, &out, &stderr, env); code != 0 {
				t.Fatal("alias confirmed import failed", code, stderr.String(), out.String())
			}
			local := state.OpenReadOnly(data)
			regs, err := local.LoadRegistrations()
			must(t, err)
			if len(regs) != 1 || regs[0].NativeSessionID != thread {
				t.Fatal("wrong logical owner", regs)
			}
			key, err := archive.MetadataObjectKey("codex", regs[0].ArchiveSessionID)
			must(t, err)
			metadata, err := reader.ReadMetadata(t.Context(), bucket, key)
			must(t, err)
			source, err := reader.LoadSource(t.Context(), bucket, metadata, reader.Limits{})
			must(t, err)
			encoded, err := json.Marshal(source)
			must(t, err)
			if !bytes.Contains(encoded, []byte("Synthetic alias history 3")) || !bytes.Contains(encoded, []byte("Synthetic alias history 5")) {
				t.Fatal("verified history lost required revisions")
			}
			if snapshotTree(t, physicalUser) != beforeNative {
				t.Fatal("import changed native data")
			}
		})
	}
}
