package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/capture"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/state/statetest"
	"github.com/wangjohn/agent-archive/internal/storage"
)

func TestRecoverCrashWindowsQueueHooksUntilReceiptCompletes(t *testing.T) {
	t.Parallel()
	for _, seam := range []string{"generation-journal", "generation-active"} {
		t.Run(seam, func(t *testing.T) {
			t.Parallel()
			env, home, path, id := recoverFixture(t)
			if seam == "generation-active" {
				seedPackedRecoveryRegistrations(t, home, id)
			}
			var out, errOut bytes.Buffer
			if code := Run([]string{"recover", id, "--confirm"}, nil, &out, &errOut, env); code != 0 {
				t.Fatalf("confirm: %d %s", code, errOut.String())
			}
			store, err := state.Open(home)
			must(t, err)
			next, _, err := store.GenerationSuccessor(id)
			must(t, err)
			old, _, err := store.LoadRegistration(id)
			must(t, err)
			reg, _, err := store.LoadRegistration(next)
			must(t, err)
			pending, _, err := store.LoadPending(next)
			must(t, err)
			request, _, err := store.LoadRequest(next)
			must(t, err)
			// Reconstruct the exact durable seam from the fixed confirmation
			// snapshot. This requires no production fault-injection API.
			journalPath := filepath.Join(home, "generation-recovery", id+".json")
			raw, err := os.ReadFile(journalPath)
			must(t, err)
			var journal map[string]any
			must(t, json.Unmarshal(raw, &journal))
			delete(journal, "complete")
			durablePending, err := os.ReadFile(filepath.Join(home, "pending", next+".json"))
			must(t, err)
			journal["registration"], journal["pending"], journal["request"] = reg, json.RawMessage(durablePending), request
			must(t, local.Write(journalPath, journal))
			if seam == "generation-journal" {
				old.CaptureFrozen = false
				must(t, local.Write(filepath.Join(home, "registrations", id+".json"), old))
				must(t, os.Remove(filepath.Join(home, "registrations", next+".json")))
				for _, dir := range []string{"generation-heads", "generation-nodes"} {
					must(t, os.RemoveAll(filepath.Join(home, dir)))
				}
				// Restore the predecessor's qualified index without changing
				// its native key or requiring knowledge of the filename hash.
				files, err := filepath.Glob(filepath.Join(home, "sessions-v1", "*.json"))
				must(t, err)
				if len(files) != 1 {
					t.Fatalf("qualified index fixture: %v", files)
				}
				for _, file := range files {
					data, err := os.ReadFile(file)
					must(t, err)
					var entry map[string]any
					must(t, json.Unmarshal(data, &entry))
					if entry["archive_session_id"] == next {
						entry["archive_session_id"] = id
						must(t, local.Write(file, entry))
					}
				}
			}
			hookAt := env.now().Add(time.Minute)
			payload := map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native", "cwd": old.ProjectRoot, "transcript_path": path}
			if err := handleTestHookEvent(home, "codex", payload, hookAt); !errors.Is(err, state.ErrSessionIndexRecoveryRequired) {
				t.Fatalf("hook entered unfinished transition: %v", err)
			}
			if seam == "generation-active" {
				feedbackPath := filepath.Join(t.TempDir(), "feedback.txt")
				must(t, os.WriteFile(feedbackPath, []byte("feedback during interrupted recovery"), 0600))
				if code := Run([]string{"feedback", next, "--file", feedbackPath}, nil, &out, &errOut, env); code != 0 {
					t.Fatalf("feedback: %d %s", code, errOut.String())
				}
			}
			must(t, store.ResumeGenerationRecoveries(t.Context()))
			for range 20 {
				complete, err := store.RecoverSessionIndexScheduled(t.Context(), state.SessionIndexRecoverySlice)
				must(t, err)
				if complete {
					break
				}
			}
			must(t, capture.ReplayAdmissionIntentsWithin(t.Context(), home, hookAt.Add(time.Second), productionAgents))
			active, found, err := store.ArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "native"})
			if err != nil || !found || active != next {
				t.Fatalf("route: %s %v %v", active, found, err)
			}
			after, _, err := store.LoadPending(next)
			must(t, err)
			if !bytes.Equal(after.SourceBytes, pending.SourceBytes) || !after.Bundle.Capture.CapturedAt.Equal(pending.Bundle.Capture.CapturedAt) {
				t.Fatal("replay changed fixed publication")
			}
			currentRequest, _, err := store.LoadRequest(next)
			must(t, err)
			observed := 0
			for _, evidence := range currentRequest.HookEvidence {
				if evidence.Kind == archive.EvidenceKindLifecycleHook && evidence.ObservedAt.Equal(hookAt) {
					observed++
				}
			}
			if observed != 1 {
				t.Fatalf("queued hook copies: %d", observed)
			}
			if seam == "generation-active" {
				feedback := 0
				for _, evidence := range currentRequest.HookEvidence {
					if evidence.Kind == archive.EvidenceKindExplicitFeedback {
						feedback++
					}
				}
				if feedback != 1 {
					t.Fatalf("journal replay lost feedback: %d", feedback)
				}
			}
			must(t, capture.ReplayAdmissionIntentsWithin(t.Context(), home, hookAt.Add(2*time.Second), productionAgents))
			regs, err := store.LoadRegistrations()
			want := 2
			if seam == "generation-active" {
				want += 4096
			}
			if err != nil || len(regs) != want {
				t.Fatalf("duplicate generations: %d %v", len(regs), err)
			}
		})
	}
}

func seedPackedRecoveryRegistrations(t *testing.T, home, id string) {
	t.Helper()
	store, err := state.Open(home)
	must(t, err)
	original, found, err := store.LoadRegistration(id)
	must(t, err)
	if !found {
		t.Fatal("packed recovery parent missing")
	}
	for i := range 4096 {
		reg := original
		reg.ArchiveSessionID = fmt.Sprintf("packed-fixture-%06d", i)
		reg.NativeSessionID = fmt.Sprintf("packed-fixture-native-%06d", i)
		data, err := json.Marshal(reg)
		must(t, err)
		must(t, os.WriteFile(filepath.Join(home, "registrations", reg.ArchiveSessionID+".json"), data, 0600))
	}
	must(t, store.MarkSessionIndexRecoveryNeeded())
	for range 20 {
		complete, err := store.RecoverSessionIndexScheduled(t.Context(), state.SessionIndexRecoverySlice)
		must(t, err)
		if complete {
			return
		}
	}
	t.Fatal("packed recovery fixture did not complete its census")
}

func recoverFixture(t *testing.T) (Env, string, string, string) {
	t.Helper()
	env, home, path, id, _ := recoverFixtureWithStorage(t)
	return env, home, path, id
}

func recoverFixtureWithStorage(t *testing.T) (Env, string, string, string, storage.ObjectStore) {
	t.Helper()
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	env, home, path, bucket := publishedThroughSync(t, at)
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	reg, _ := onlyRegistration(t, home)
	bundle, publishedAt, found, err := store.LoadLastPublished(reg.ArchiveSessionID)
	if err != nil || !found {
		t.Fatal(err)
	}
	if err := statetest.SaveBlocked(store, reg.ArchiveSessionID, bundle, publishedAt, state.BlockedReasonTranscriptRewritten); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"type":"turn_context","model":"gpt-test"}
{"type":"response_item","id":"now","payload":{"type":"message","role":"assistant","content":"current recoverable activity"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	env.Now = func() time.Time { return at.Add(time.Hour) }
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { t.Fatal("recovery opened storage"); return nil, nil }
	return env, home, path, reg.ArchiveSessionID, bucket
}

func TestRecoverFrozenFeedbackRetainsIdentityAgeAndRetry(t *testing.T) {
	t.Parallel()
	env, home, _, id, bucket := recoverFixtureWithStorage(t)
	var out, errOut bytes.Buffer
	if code := Run([]string{"recover", id, "--confirm"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("recover: %d %s", code, errOut.String())
	}
	store, err := state.Open(home)
	must(t, err)
	before, _, _, err := store.LoadLastPublished(id)
	must(t, err)
	path := filepath.Join(t.TempDir(), "feedback.txt")
	must(t, os.WriteFile(path, []byte("review preserved history; password=synthetic-secret-value"), 0600))
	if code := Run([]string{"feedback", id, "--file", path}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("feedback: %d %s", code, errOut.String())
	}
	cfg, _, err := config.Load(home)
	must(t, err)
	opts := collector.Options{Sources: registryFor(env), Parsers: parsersFor(env), MachineID: cfg.MachineID, Now: env.Now, RepoKey: env.repoKey, SkillEvidence: cfg.EffectiveSkillEvidence(), RequireSkillUse: cfg.RequireSkillUse}
	result, err := collector.Run(t.Context(), store, failingPutStore{bucket}, opts)
	if err != nil || result.Errors[id] == nil {
		t.Fatalf("expected frozen upload retry: %#v %v", result, err)
	}
	if _, found, err := store.LoadRequest(id); err != nil || !found {
		t.Fatalf("failed upload acknowledged feedback: %v", err)
	}
	result, err = collector.Run(t.Context(), store, bucket, opts)
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("retry: %#v %v", result, err)
	}
	after, _, _, err := store.LoadLastPublished(id)
	must(t, err)
	if after.ArchiveSessionID != id || !after.Capture.CapturedAt.Equal(before.Capture.CapturedAt) {
		t.Fatal("feedback changed frozen identity or retention age")
	}
	count := 0
	for _, evidence := range after.SupplementalEvidence {
		if evidence.Kind == archive.EvidenceKindExplicitFeedback {
			count++
			text, _ := evidence.Payload["text"].(string)
			if strings.Contains(text, "synthetic-secret-value") || !strings.Contains(text, "[REDACTED]") {
				t.Fatalf("feedback privacy: %s", text)
			}
		}
	}
	if count != 1 {
		t.Fatalf("feedback copies: %d", count)
	}
	if _, found, err := store.LoadRequest(id); err != nil || found {
		t.Fatalf("successful feedback left request: %v", err)
	}
	key, err := archive.MetadataObjectKey("codex", id)
	must(t, err)
	raw, err := bucket.Get(t.Context(), key)
	must(t, err)
	var metadata archive.Metadata
	must(t, json.Unmarshal(raw, &metadata))
	if metadata.SessionID != id || !metadata.CapturedAt.Equal(before.Capture.CapturedAt) {
		t.Fatal("metadata changed frozen identity or age")
	}
}

func TestRecoverReplayRefusesPredecessorPublicationKey(t *testing.T) {
	t.Parallel()
	env, home, _, id, bucket := recoverFixtureWithStorage(t)
	var out, errOut bytes.Buffer
	if code := Run([]string{"recover", id, "--confirm"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("recover: %d %s", code, errOut.String())
	}
	store, err := state.Open(home)
	must(t, err)
	next, _, err := store.GenerationSuccessor(id)
	must(t, err)
	reg, _, err := store.LoadRegistration(next)
	must(t, err)
	pending, _, err := store.LoadPending(next)
	must(t, err)
	request, _, err := store.LoadRequest(next)
	must(t, err)
	key, err := archive.MetadataObjectKey(reg.Harness.Name, id)
	must(t, err)
	before, err := bucket.Get(t.Context(), key)
	must(t, err)
	pending.MetadataKey = key
	var corruptMetadata archive.Metadata
	must(t, json.Unmarshal(pending.MetadataBytes, &corruptMetadata))
	corruptMetadata.SessionID = id
	pending.MetadataBytes, err = json.Marshal(corruptMetadata)
	must(t, err)
	journalPath := filepath.Join(home, "generation-recovery", id+".json")
	raw, err := os.ReadFile(journalPath)
	must(t, err)
	var journal map[string]any
	must(t, json.Unmarshal(raw, &journal))
	delete(journal, "complete")
	durablePending, err := os.ReadFile(filepath.Join(home, "pending", next+".json"))
	must(t, err)
	var corruptPending map[string]any
	must(t, json.Unmarshal(durablePending, &corruptPending))
	corruptPending["metadata_key"] = pending.MetadataKey
	corruptPending["metadata_bytes"] = pending.MetadataBytes
	journal["registration"], journal["pending"], journal["request"] = reg, corruptPending, request
	must(t, local.Write(journalPath, journal))
	err = store.ResumeGenerationRecoveries(t.Context())
	if errors.Is(err, state.ErrSessionIndexRecoveryRequired) {
		after, err := bucket.Get(t.Context(), key)
		must(t, err)
		if !bytes.Equal(before, after) {
			t.Fatal("rejected journal altered predecessor metadata")
		}
		return
	}
	must(t, err)
	// Demonstrate the consequence through the ordinary publication path when
	// replay accepts the corrupt fixed transaction, rather than only its cache.
	cfg, _, err := config.Load(home)
	must(t, err)
	opts := collector.Options{Sources: registryFor(env), Parsers: parsersFor(env), MachineID: cfg.MachineID, Now: env.Now, RepoKey: env.repoKey, SkillEvidence: cfg.EffectiveSkillEvidence(), RequireSkillUse: cfg.RequireSkillUse, AcceptSession: func(reg archive.SessionRegistration) bool { return reg.ArchiveSessionID == next }}
	result, err := collector.Run(t.Context(), store, bucket, opts)
	must(t, err)
	if len(result.Errors) != 0 {
		t.Fatalf("publish accepted journal: %#v", result.Errors)
	}
	after, err := bucket.Get(t.Context(), key)
	must(t, err)
	var metadata archive.Metadata
	must(t, json.Unmarshal(after, &metadata))
	if bytes.Equal(before, after) {
		t.Fatal("corrupt journal accepted without fail-closed validation")
	}
	t.Fatalf("corrupt journal replaced predecessor metadata with successor source %s", metadata.SourceBundle.Key)
}

func TestRecoverPreviewConfirmAndHookRouting(t *testing.T) {
	t.Parallel()
	env, home, path, id := recoverFixture(t)
	before, err := os.ReadFile(home + "/config.json")
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"recover", id}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("preview exit %d %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "feedback and handoffs") || !strings.Contains(out.String(), "--confirm") {
		t.Fatalf("incomplete preview %s", out.String())
	}
	after, _ := os.ReadFile(home + "/config.json")
	if !bytes.Equal(before, after) {
		t.Fatal("preview changed config")
	}
	store := state.OpenReadOnly(home)
	if _, found, err := store.GenerationSuccessor(id); err != nil || found {
		t.Fatal("preview created recovery", err)
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"recover", id, "--confirm"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("confirm exit %d %s", code, errOut.String())
	}
	cfg, _, err := config.Load(home)
	if err != nil || !cfg.GenerationProtection || !cfg.DurableStorageProtection || !cfg.SourceSetPublicationProtection || cfg.SchemaVersion != 17 {
		t.Fatalf("missing downgrade protection %#v %v", cfg, err)
	}
	next, found, err := store.GenerationSuccessor(id)
	if err != nil || !found {
		t.Fatalf("successor missing %v", err)
	}
	for range 2 {
		if code := Run([]string{"recover", id, "--confirm"}, nil, &out, &errOut, env); code != 0 {
			t.Fatalf("repeat %d %s", code, errOut.String())
		}
	}
	regs, err := store.LoadRegistrations()
	if err != nil || len(regs) != 2 {
		t.Fatalf("duplicate registration %#v %v", regs, err)
	}
	at := env.now().Add(time.Minute)
	payload := map[string]any{"hook_event_name": "Stop", "session_id": "native", "cwd": regs[0].ProjectRoot, "transcript_path": path}
	// Use the original project independent of sorted IDs.
	old, _, _ := store.LoadRegistration(id)
	payload["cwd"] = old.ProjectRoot
	if err := handleTestHookEvent(home, "codex", payload, at); err != nil {
		t.Fatal(err)
	}
	key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "native"}
	active, found, err := store.ArchiveSessionID(key)
	if err != nil || !found || active != next {
		t.Fatalf("hook map %s %v", active, err)
	}
	request, found, err := store.LoadRequest(next)
	if err != nil || !found || request.ArchiveSessionID != next {
		t.Fatalf("hook request %#v %v", request, err)
	}
	if _, found, err := store.LoadRequest(id); err != nil || found {
		t.Fatalf("hook wrote historical request %v", err)
	}
	// Full local reconstruction preserves active mapping after ordinary hooks.
	mutable, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := mutable.MarkSessionIndexRecoveryNeeded(); err != nil {
		t.Fatal(err)
	}
	if _, err := mutable.RecoverSessionIndexScheduled(context.Background(), state.SessionIndexRecoverySlice); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverRefusesDifferentNativeIdentity(t *testing.T) {
	t.Parallel()
	for _, confirm := range []bool{false, true} {
		t.Run(strconv.FormatBool(confirm), func(t *testing.T) {
			t.Parallel()
			env, home, path, id := recoverFixture(t)
			must(t, os.WriteFile(path, []byte(`{"type":"session_meta","payload":{"id":"different-native"}}
{"type":"response_item","payload":{"type":"message","role":"user","content":"unrelated session"}}`), 0600))
			before, err := os.ReadFile(filepath.Join(home, "config.json"))
			must(t, err)
			args := []string{"recover", id}
			if confirm {
				args = append(args, "--confirm")
			}
			var out, errOut bytes.Buffer
			if code := Run(args, nil, &out, &errOut, env); code != 1 {
				t.Fatalf("different native identity accepted: exit %d %s %s", code, out.String(), errOut.String())
			}
			after, err := os.ReadFile(filepath.Join(home, "config.json"))
			must(t, err)
			if !bytes.Equal(before, after) {
				t.Fatal("refusal changed writer protection")
			}
			store := state.OpenReadOnly(home)
			if _, found, err := store.GenerationSuccessor(id); err != nil || found {
				t.Fatalf("refusal created successor: %v", err)
			}
		})
	}
}

type recoveryRefusalMode string

const recoveryRefusalPaused recoveryRefusalMode = "paused"

const recoveryRefusalDestination recoveryRefusalMode = "destination"

const recoveryRefusalSubagent recoveryRefusalMode = "subagent"

const recoveryRefusalPending recoveryRefusalMode = "pending"

func TestRecoverRefusesPauseDestinationAndSubagent(t *testing.T) {
	t.Parallel()
	for _, mode := range []recoveryRefusalMode{recoveryRefusalPaused, recoveryRefusalDestination, recoveryRefusalSubagent, recoveryRefusalPending} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			env, home, _, id := recoverFixture(t)
			store, err := state.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			cfg, _, err := config.Load(home)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case recoveryRefusalPaused:
				cfg.Paused = true
			case recoveryRefusalDestination:
				cfg.Storage.Bucket = "another-destination"
			case recoveryRefusalSubagent:
				_, err = store.UpdateRegistration(id, func(reg *archive.SessionRegistration) error { reg.ParentSessionID = "parent"; return nil })
				if err != nil {
					t.Fatal(err)
				}
			case recoveryRefusalPending:
				if err := store.SavePending(id, state.PendingPublication{SourceKey: "source", MetadataKey: "metadata", SourceSHA256: "sha", SourceBytes: []byte("synthetic"), MetadataBytes: []byte(`{}`)}); err != nil {
					t.Fatal(err)
				}
			}
			if err := config.Save(home, cfg); err != nil {
				t.Fatal(err)
			}
			var out, errOut bytes.Buffer
			if code := Run([]string{"recover", id, "--confirm"}, nil, &out, &errOut, env); code != 1 {
				t.Fatalf("%s accepted exit %d %s %s", mode, code, out.String(), errOut.String())
			}
			if _, found, err := store.GenerationSuccessor(id); err != nil || found {
				t.Fatal("refusal registered successor", err)
			}
		})
	}
}
