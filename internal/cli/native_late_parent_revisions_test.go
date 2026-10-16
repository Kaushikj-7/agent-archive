package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"os"
	"path/filepath"
	"reflect"
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

func TestNativeLateParentPreservedReadback(t *testing.T) {
	for _, name := range []nativeParentTiming{nativeParentSettled, nativeParentPreparing, nativeParentStrongerPolicyDuringRepair, nativeParentStrongerPolicyLegacyRepair, nativeParentLegacyMarkerSettled, nativeParentLegacyMarkerMidcursor, nativeParentLegacyMarkerStrongerPolicy, nativeParentLegacyMarkerNamingCache} {
		during := name == nativeParentPreparing
		tighten := strings.Contains(string(name), "stronger_policy")
		legacyMarker := strings.HasPrefix(string(name), "legacy_marker_")
		t.Run(string(name), func(t *testing.T) {
			origin := archive.SessionOriginHook
			canonical := func() string { p, err := filepath.EvalSymlinks(t.TempDir()); must(t, err); return p }
			home, userHome, project := canonical(), canonical(), canonical()
			nativeHome := filepath.Join(userHome, ".codex")
			must(t, os.MkdirAll(filepath.Join(nativeHome, "sessions"), 0700))
			at := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
			skill := filepath.Join(project, ".agents", "skills", "synthetic-policy", "SKILL.md")
			must(t, os.MkdirAll(filepath.Dir(skill), 0700))
			must(t, os.WriteFile(skill, []byte("---\nname: synthetic-policy\n---\nsynthetic-parent-policy-private-body\n"), 0600))
			const thread = "11111111-1111-4111-8111-111111111111"
			const physical = "22222222-2222-4222-8222-222222222222"
			const parentNative = "44444444-4444-4444-8444-444444444444"
			meta := func(base map[string]any, ordinal int) []byte {
				payload := map[string]any{"id": thread, "timestamp": at.Format(time.RFC3339Nano), "cwd": project, "source": "cli", "originator": "codex_cli_rs", "cli_version": "0.160.0", "history_mode": "paginated"}
				payload["parent_thread_id"] = parentNative
				payload["subagent_history_start_ordinal"] = 0
				if base != nil {
					payload["history_base"] = base
				}
				raw, err := json.Marshal(map[string]any{"type": "session_meta", "ordinal": ordinal, "timestamp": at.Format(time.RFC3339Nano), "payload": payload})
				must(t, err)
				return append(raw, '\n')
			}
			task := []byte(fmt.Sprintf(`{"type":"event_msg","ordinal":1,"timestamp":%q,"payload":{"type":"task_started","turn_id":"11111111-1111-4111-8111-111111111111","root_turn_id":"11111111-1111-4111-8111-111111111111","started_at":%q}}`+"\n", at.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano)))
			seed := filepath.Join(nativeHome, "sessions", "rollout-2026-10-02T12-00-00-"+thread+".jsonl")
			prefix := append(meta(nil, 0), task...)
			must(t, os.WriteFile(seed, append(prefix, []byte(`{"type":"response_item","ordinal":2,"payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"synthetic outgoing-only DB_PASSWORD=hunter2hunter2"}]}}`+"\n")...), 0600))
			current := filepath.Join(nativeHome, "sessions", "rollout-2026-10-02T12-00-00-"+physical+".jsonl")
			raw := meta(map[string]any{"thread_id": thread, "end_ordinal_exclusive": 2, "end_byte_offset": len(prefix)}, 2)
			must(t, os.WriteFile(current, append(raw, []byte(`{"type":"response_item","ordinal":3,"payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"synthetic current-only DB_PASSWORD=hunter2hunter2"}]}}`+"\n")...), 0600))
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
			reg := archive.SessionRegistration{ArchiveSessionID: "admitted-history", NativeSessionID: thread, Harness: archive.Harness{Name: "codex"}, ProjectID: archive.ProjectID(project), ProjectRoot: project, TranscriptPath: seed, SessionStartedAt: at, RegisteredAt: at.Add(time.Second), AdmittedAt: at.Add(time.Second), Origin: origin, NativeChild: true, ParentNativeSessionID: parentNative, NativeSourceHome: nativeHome, DestinationID: cfg.DestinationID()}
			must(t, local.SaveRegistration(reg))
			cloud := storagetest.NewMemoryStore()
			counted := &nativeRepairPutStore{MemoryStore: cloud}
			env := setupTestEnv(t, home, userHome, newFakeKeychain(), at.Add(2*time.Minute))
			env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return counted, nil }
			published := false
			var preparing state.PendingPublication
			for range 12 {
				result, err := runOnePass(env, true)
				must(t, err)
				for _, issue := range result.Errors {
					if !errors.Is(issue, archive.ErrHistoryMutationPending) && !agentapi.HasFailure(issue, agentapi.Unavailable) {
						t.Fatal(result.Errors)
					}
				}
				if during {
					p, found, err := local.LoadPending(reg.ArchiveSessionID)
					must(t, err)
					if found && p.History != nil && p.History.Preparing && p.History.PrivacyCursor > 0 {
						preparing = p
						published = true
						break
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
			var beforeBytes []byte
			if during {
				beforeBytes = preparing.MetadataBytes
			} else {
				beforeBytes, err = local.PublishedMetadata(reg.ArchiveSessionID)
				must(t, err)
			}
			var before archive.Metadata
			must(t, json.Unmarshal(beforeBytes, &before))
			originalSources := map[string]archive.SourceBundle{}
			if during {
				for _, input := range preparing.History.Inputs {
					raw, err := local.ReadPendingSource(reg.ArchiveSessionID, state.PendingSource{Reference: input.Reference, Name: input.Reference.SHA256 + ".gz"})
					must(t, err)
					originalSources[input.RevisionID], err = archive.ReadSourceBundle(bytes.NewReader(raw), archive.DecodeOptions{})
					must(t, err)
				}
			} else {
				originalSources[before.History.CurrentRevision], err = reader.LoadSource(t.Context(), cloud, before, reader.Limits{})
				must(t, err)
				for _, revision := range before.History.Preserved {
					originalSources[revision.RevisionID], err = reader.LoadRevision(t.Context(), cloud, before, revision.RevisionID, reader.Limits{})
					must(t, err)
				}
			}

			if legacyMarker {
				legacyParser := "0.22.0"
				if name == nativeParentLegacyMarkerNamingCache {
					legacyParser = "0.25.0"
				}
				before = legacyNativeChildEnvelope(t, local, cloud, reg, before, originalSources, legacyParser)
				interrupted := false
				for range 8 {
					result, err := runOnePass(env, true)
					must(t, err)
					for _, issue := range result.Errors {
						if !errors.Is(issue, archive.ErrHistoryMutationPending) {
							t.Fatal("legacy ownership migration", issue)
						}
					}
					if name != nativeParentLegacyMarkerSettled && name != nativeParentLegacyMarkerNamingCache {
						pending, found, err := local.LoadPending(reg.ArchiveSessionID)
						must(t, err)
						if found && pending.History.Preparing && pending.History.PrivacyCursor > 0 {
							// An older descriptor omitted original parent provenance.
							for i := range pending.History.Inputs {
								pending.History.Inputs[i].ParentSessionID = nil
							}
							must(t, local.SavePending(reg.ArchiveSessionID, pending))
							local, err = state.Open(home)
							must(t, err)
							interrupted = true
							break
						}
					}
				}
				if name != nativeParentLegacyMarkerSettled && name != nativeParentLegacyMarkerNamingCache && !interrupted {
					t.Fatal("legacy control never interrupted ownership preparation")
				}
			}

			if name == nativeParentLegacyMarkerNamingCache {
				owner, found, err := local.LoadRegistration(reg.ArchiveSessionID)
				must(t, err)
				if !found || !owner.NativeChild {
					t.Fatal("naming-era cache skipped legacy child ownership migration")
				}
			}

			parentMeta, err := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{"id": parentNative, "timestamp": at.Format(time.RFC3339Nano), "cwd": project, "source": "cli", "originator": "codex_cli_rs", "cli_version": "dev"}})
			must(t, err)
			parentTask := strings.ReplaceAll(string(task), thread, parentNative)
			parentPath := filepath.Join(nativeHome, "sessions", "rollout-"+parentNative+".jsonl")
			must(t, os.WriteFile(parentPath, append(append(parentMeta, '\n'), []byte(parentTask+`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"synthetic parent prompt"}]}}`+"\n")...), 0600))
			env.Now = func() time.Time { return at.Add(time.Hour) }
			var out, errOut bytes.Buffer
			if code := Run([]string{"backfill", "--yes", "--background", "--harness", "codex", "--include-temp", "--project", project}, nil, &out, &errOut, env); code != 0 {
				t.Fatal("late parent import failed", code, out.String(), errOut.String())
			}
			regs, err := local.LoadRegistrations()
			must(t, err)
			var parentReg archive.SessionRegistration
			for _, candidate := range regs {
				if candidate.NativeSessionID == parentNative {
					parentReg = candidate
				}
			}
			if parentReg.ArchiveSessionID == "" || parentReg.ImportBatch.IsZero() {
				t.Fatal("parent not independently imported")
			}
			policyChanged := false
			strongerPasses := 0
			newerToken := ""
			missingSources := map[string][]byte{}
			for range 12 {
				result, err := runOnePass(env, true)
				must(t, err)
				for _, issue := range result.Errors {
					if !errors.Is(issue, archive.ErrHistoryMutationPending) {
						t.Fatal("late parent maintenance", result)
					}
				}
				if policyChanged && strongerPasses < 2 {
					strongerPasses++
					requested, found, err := local.LoadRequest(reg.ArchiveSessionID)
					must(t, err)
					if !found || requested.Token != newerToken {
						t.Fatal("retained successor acknowledged a newer native request", requested)
					}
					if strongerPasses == 1 {
						for _, key := range counted.puts {
							if strings.HasPrefix(key, "sessions/codex/"+reg.ArchiveSessionID+"/") {
								t.Fatal("stricter preparation uploaded child publication", key)
							}
						}
					}
					if strongerPasses == 2 {
						for path, content := range missingSources {
							must(t, os.WriteFile(path, content, 0600))
						}
					}
				}
				if tighten && !policyChanged {
					pending, found, err := local.LoadPending(reg.ArchiveSessionID)
					must(t, err)
					if found && pending.Bundle.ParentSessionID != "" && pending.History.Preparing && pending.History.PrivacyCursor > 0 {
						if name == nativeParentStrongerPolicyLegacyRepair {
							for i := range pending.History.Inputs {
								pending.History.Inputs[i].ParentSessionID = nil
							}
							must(t, local.SavePending(reg.ArchiveSessionID, pending))
						}
						for _, path := range []string{seed, current} {
							content, err := os.ReadFile(path)
							must(t, err)
							missingSources[path] = content
							must(t, os.Remove(path))
						}
						must(t, local.SaveRequest(reg.ArchiveSessionID, "synthetic-newer-native-read", env.now().Add(time.Minute)))
						requested, found, err := local.LoadRequest(reg.ArchiveSessionID)
						must(t, err)
						if !found {
							t.Fatal("newer request not recorded")
						}
						newerToken = requested.Token
						counted.puts = nil
						local, err = state.Open(home)
						must(t, err)
						saved := mustLoadConfig(t, home)
						saved.SkillEvidence = config.SkillEvidenceNone
						must(t, config.Save(home, saved))
						policyChanged = true
					}
				}
			}

			if tighten && !policyChanged {
				t.Fatal("control never interrupted an actual parent repair cursor")
			}
			linked, found, err := local.LoadRegistration(reg.ArchiveSessionID)
			must(t, err)
			if !found || linked.ParentSessionID != parentReg.ArchiveSessionID {
				t.Fatal("late parent did not resolve", linked)
			}
			// This current segment reverts the original outgoing prompt. The
			// actual publication must retain that outgoing revision independently.
			if _, pending, err := local.LoadRequest(reg.ArchiveSessionID); err != nil || pending {
				t.Fatal("newer link request was lost or never acknowledged", pending, err)
			}
			for range 3 {
				loads := state.PublishedStateLoads()
				result, err := runOnePass(env, true)
				must(t, err)
				if len(result.Errors) != 0 || len(result.Published) != 0 {
					t.Fatal("repaired child did not settle", result)
				}
				// Each CLI pass opens a cold Store: validate the source-set child
				// once. Allow at most four legacy parent validations for label
				// observation, link settlement, verification and retention.
				if got := state.PublishedStateLoads() - loads; got < 1 || got > 5 {
					t.Fatalf("settled cold pass loaded published state %d times, want 1..5", got)
				}
			}
			key, err := archive.MetadataObjectKey(reg.Harness.Name, reg.ArchiveSessionID)
			must(t, err)
			metadata, err := reader.ReadMetadata(t.Context(), cloud, key)
			must(t, err)
			if !metadata.CapturedAt.Equal(before.CapturedAt) || !reflect.DeepEqual(metadata.Counts, before.Counts) || !reflect.DeepEqual(metadata.ModelTokens, before.ModelTokens) {
				t.Fatal("parent repair changed child age or own activity")
			}
			for _, gap := range metadata.CaptureGaps {
				if gap.Code == "native_parent_link_pending" {
					t.Fatal("resolved parent remains pending")
				}
			}
			if !metadata.NativeChild || metadata.ParentSessionID != parentReg.ArchiveSessionID {
				t.Fatal("parent repair not published", metadata.ParentSessionID)
			}
			currentBundle, err := reader.LoadSource(t.Context(), cloud, metadata, reader.Limits{})
			must(t, err)
			original := originalSources[metadata.History.CurrentRevision]
			if !reflect.DeepEqual(original.NativeRecords, currentBundle.NativeRecords) || !reflect.DeepEqual(original.Ordinals, currentBundle.Ordinals) || !reflect.DeepEqual(original.History, currentBundle.History) {
				t.Fatal("late parent repair changed active native evidence or raw ownership")
			}
			verifyPolicy := func(bundle archive.SourceBundle) {
				t.Helper()
				encoded, err := json.Marshal(bundle)
				must(t, err)
				if bytes.Contains(encoded, []byte("hunter2hunter2")) {
					t.Fatal("native secret survived")
				}
				if tighten {
					if bytes.Contains(encoded, []byte("synthetic-parent-policy-private-body")) {
						t.Fatal("stricter policy retained skill body")
					}
					for _, item := range bundle.SupplementalEvidence {
						if item.Kind == archive.EvidenceKindSkillSnapshot || item.Kind == archive.EvidenceKindSkillInventory {
							t.Fatal("stricter policy incomplete", item.Kind)
						}
					}
				}
			}
			verifyPolicy(currentBundle)
			if !currentBundle.NativeChild {
				t.Fatal("active source ownership marker missing")
			}
			encoded, err := json.Marshal(currentBundle)
			must(t, err)
			if !strings.Contains(string(encoded), "synthetic current-only") || strings.Contains(string(encoded), "synthetic outgoing-only") || metadata.History == nil || len(metadata.History.Preserved) == 0 {
				t.Fatal("revert lost revision boundary/alternative", metadata.History)
			}
			must(t, os.Remove(seed))
			out.Reset()
			errOut.Reset()
			if code := Run([]string{"backfill", "undo", parentReg.ImportBatch.Recorded(), "--yes"}, nil, &out, &errOut, env); code != 0 {
				t.Fatal("parent undo failed", code, out.String(), errOut.String())
			}
			recovered := false
			for _, revision := range metadata.History.Preserved {
				previous, err := reader.LoadRevision(t.Context(), cloud, metadata, revision.RevisionID, reader.Limits{})
				must(t, err)
				verifyPolicy(previous)
				if !previous.NativeChild {
					t.Fatal("preserved source ownership marker missing")
				}
				original := originalSources[revision.RevisionID]
				if !previous.Capture.CapturedAt.Equal(original.Capture.CapturedAt) || !reflect.DeepEqual(original.NativeRecords, previous.NativeRecords) || !reflect.DeepEqual(original.Ordinals, previous.Ordinals) || !reflect.DeepEqual(original.History, previous.History) {
					t.Fatal("late parent repair changed preserved evidence, age or raw ownership")
				}
				data, err := json.Marshal(previous)
				must(t, err)
				recovered = recovered || strings.Contains(string(data), "synthetic outgoing-only")
			}
			if !recovered {
				t.Fatal("outgoing evidence unreadable after native dependency deletion")
			}
			// The preserved revisions keep their original ages and raw ownership.
			for _, old := range before.History.Preserved {
				for _, next := range metadata.History.Preserved {
					if old.RevisionID == next.RevisionID && !old.CapturedAt.Equal(next.CapturedAt) {
						t.Fatal("parent repair changed preserved capture age")
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

// Count actual publication writes without changing bounded object-read ports.
type nativeRepairPutStore struct {
	*storagetest.MemoryStore
	puts []string
}

func (s *nativeRepairPutStore) Put(ctx context.Context, key string, data []byte) error {
	s.puts = append(s.puts, key)
	return s.MemoryStore.Put(ctx, key, data)
}

type nativeParentTiming string

const (
	nativeParentSettled                    nativeParentTiming = "settled"
	nativeParentPreparing                  nativeParentTiming = "preparing"
	nativeParentStrongerPolicyDuringRepair nativeParentTiming = "stronger_policy_during_repair"
	nativeParentStrongerPolicyLegacyRepair nativeParentTiming = "stronger_policy_legacy_repair"
	nativeParentLegacyMarkerSettled        nativeParentTiming = "legacy_marker_settled"
	nativeParentLegacyMarkerMidcursor      nativeParentTiming = "legacy_marker_midcursor"
	nativeParentLegacyMarkerStrongerPolicy nativeParentTiming = "legacy_marker_stronger_policy"
	nativeParentLegacyMarkerNamingCache    nativeParentTiming = "legacy_marker_naming_cache"
)
