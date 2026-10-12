package collector

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/codex"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

type nativeOwnerTaskChange string

const (
	nativeOwnerTaskUnchanged nativeOwnerTaskChange = "unchanged"
	nativeOwnerTaskTimestamp nativeOwnerTaskChange = "timestamp"
	nativeOwnerTaskTurnID    nativeOwnerTaskChange = "turn_id"
	nativeOwnerTaskAppend    nativeOwnerTaskChange = "append"
)

// Replacement occurs before opening the next snapshot, so within-read file
// change checks cannot substitute for immutable admission evidence comparison.
func TestNativeOwnerAdmissionRejectsChangedFirstTask(t *testing.T) {
	const thread = "11111111-1111-4111-8111-111111111111"
	const parent = "22222222-2222-4222-8222-222222222222"
	const other = "33333333-3333-4333-8333-333333333333"
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for _, shape := range []string{"child", "fork", "ordinary"} {
		for _, change := range []nativeOwnerTaskChange{nativeOwnerTaskUnchanged, nativeOwnerTaskTimestamp, nativeOwnerTaskTurnID, nativeOwnerTaskAppend} {
			t.Run(shape+"/"+string(change), func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "rollout-"+thread+".jsonl")
				meta := map[string]any{"id": thread, "timestamp": at.Format(time.RFC3339Nano), "cwd": dir, "source": "cli", "originator": "codex_cli_rs", "cli_version": "dev"}
				if shape == "child" {
					meta["parent_thread_id"] = parent
					meta["session_id"] = parent
					meta["subagent_history_start_ordinal"] = 1
				}
				if shape == "fork" {
					meta["forked_from_id"] = parent
					meta["forked_from_ordinal_exclusive"] = 1
				}
				write := func(start time.Time, turn string, appendTask bool) {
					t.Helper()
					header, err := json.Marshal(map[string]any{"type": "session_meta", "payload": meta})
					if err != nil {
						t.Fatal(err)
					}
					task := func(start time.Time, turn string) []byte {
						raw, err := json.Marshal(map[string]any{"type": "event_msg", "payload": map[string]any{"type": "task_started", "turn_id": turn, "started_at": start.Format(time.RFC3339Nano)}})
						if err != nil {
							t.Fatal(err)
						}
						return append(raw, '\n')
					}
					rows := append(append(header, '\n'), task(start, turn)...)
					if appendTask {
						rows = append(rows, task(at.Add(time.Minute), other)...)
					}
					if err := os.WriteFile(path, rows, 0600); err != nil {
						t.Fatal(err)
					}
				}
				read := func(binding *archive.CodexSourceBinding) (*archive.CodexSourceBinding, error) {
					t.Helper()
					pass, err := (codex.SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{RequireConfinedHistory: true, Policy: transcriptio.OpenPolicy{Root: dir, RejectSymlinks: true}})
					if err != nil {
						t.Fatal(err)
					}
					defer func() {
						if err := pass.Close(); err != nil {
							t.Error(err)
						}
					}()
					snap, err := pass.Read(t.Context(), agentapi.SourceRef{Kind: archive.SourceKindFile, Path: path}, agentapi.ReadLimits{RawBytes: 8 << 20, RecordBytes: archive.MaxRecordBytes})
					if err != nil {
						t.Fatal(err)
					}
					defer func() {
						if err := snap.Close(); err != nil {
							t.Error(err)
						}
					}()
					r := providerReader{admission: agentapi.SourceAdmission{NativeID: thread, Binding: binding}}
					return r.snapshotBinding(t.Context(), snap)
				}
				write(at, thread, false)
				admitted, err := read(nil)
				if err != nil || admitted == nil || admitted.FirstNativeTaskID != thread {
					t.Fatalf("first admission: %+v %v", admitted, err)
				}
				if admitted.RequiresOwnTask() != (shape != "ordinary") {
					t.Fatalf("wrong ownership shape %+v", admitted)
				}
				start, turn := at, thread
				if change == nativeOwnerTaskTimestamp {
					start = at.Add(time.Second)
				}
				if change == nativeOwnerTaskTurnID {
					turn = other
				}
				write(start, turn, change == nativeOwnerTaskAppend)
				current, err := read(admitted)
				reject := shape != "ordinary" && (change == nativeOwnerTaskTimestamp || change == nativeOwnerTaskTurnID)
				if reject {
					if !agentapi.HasFailure(err, agentapi.Unavailable) || current != nil {
						t.Fatalf("changed task accepted: %+v %v", current, err)
					}
				} else if err != nil || current == nil || !current.FirstNativeTaskAt.Equal(admitted.FirstNativeTaskAt) || current.FirstNativeTaskID != admitted.FirstNativeTaskID {
					t.Fatalf("unchanged/append/ordinary compatibility: %+v %v", current, err)
				}
			})
		}
	}
}
