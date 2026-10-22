package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/capture"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/discovery"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
	"github.com/wangjohn/agent-archive/internal/trace"
)

func TestMuseReplayWholeHookCandidateOwnerPublicationAndList(t *testing.T) {
	// Sequential: the production Filter trace counter is process-wide.
	for _, test := range []struct {
		name      string
		value     string
		protected bool
	}{
		{"identifier", "run-11:a.b_2", true}, {"malformed", "PRIVATE marker $(command)", true},
		{"empty", "", true}, {"whitespace", " \t ", true}, {"ordinary_old_fence", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newMuseRootCaptureFixture(t)
			f.cfg.NativeReplayProtection = test.protected
			must(t, config.Save(f.home, f.cfg))
			want := archive.ParseReplay(test.value)
			f.env.LookupEnv = func(key string) (string, bool) {
				if key == archive.ReplayEnv {
					return test.value, true
				}
				return museTestIdentityEnvironment(key)
			}
			payload, err := json.Marshal(map[string]any{"hook_event_name": "SessionStart", "session_id": "00000000-0000-7000-8000-000000000001", "transcript_path": nil, "cwd": f.project, "source": "startup"})
			must(t, err)
			var stderr bytes.Buffer
			if code := runHookCommand([]string{"--harness", "muse"}, bytes.NewReader(payload), &stderr, f.env); code != 0 || stderr.Len() != 0 {
				t.Fatalf("hook %d: %s", code, &stderr)
			}
			entries, err := os.ReadDir(filepath.Join(f.home, "admission-intents"))
			must(t, err)
			if len(entries) != 1 {
				t.Fatalf("startup candidate count %d", len(entries))
			}
			candidatePath := filepath.Join(f.home, "admission-intents", entries[0].Name())
			raw, err := os.ReadFile(candidatePath)
			must(t, err)
			var candidate agentapi.AdmissionIntent
			must(t, json.Unmarshal(raw, &candidate))
			if !museReplayEqual(candidate.Replay, want) {
				t.Fatal("hook lost normalized Replay before concrete source inventory")
			}
			if want != nil && want.RunID == "" && bytes.Contains(raw, []byte(test.value)) {
				t.Fatal("malformed marker leaked")
			}
			cloud := storagetest.NewMemoryStore()
			f.env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return cloud, nil }
			calls := 0
			opts := collector.Options{Sources: f.registry, Parsers: f.registry, Decoders: f.registry, MachineID: f.cfg.MachineID, NativeRoots: f.cfg.NativeRoots, AcceptSession: f.cfg.AcceptSession, Now: f.env.Now, NativeLocatorFactory: func(ctx context.Context, pass collector.NativeLocatorPass) (agentapi.NativeLocatorLookup, func() error, error) {
				calls++
				if len(pass.Queries) != 1 || pass.Queries[0].Purpose != agentapi.LocatorStartupCandidate {
					t.Fatal("startup inventory scope changed")
				}
				inventory, err := discovery.NewNativeInventory(ctx, f.home, pass.Roots, pass.Queries, pass.ReadBudget, builtin.ValidateNativeLocator)
				if err != nil {
					return nil, nil, err
				}
				return inventory, inventory.Close, nil
			}}
			stopTrace := trace.Enable()
			result, err := collector.Run(t.Context(), f.store, cloud, opts)
			var measured bytes.Buffer
			trace.Write(&measured)
			stopTrace()
			defer result.CloseStagedResults()
			if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 || calls != 1 {
				t.Fatalf("whole publication: %+v %v inventory=%d", result, err, calls)
			}
			source, err := os.ReadFile(f.path)
			must(t, err)
			if museTraceFilterInput(t, measured.String()) != int64(len(source)) {
				t.Fatal("startup concrete Filter count changed")
			}
			regs, err := f.store.LoadRegistrations()
			must(t, err)
			if len(regs) != 1 || !museReplayEqual(regs[0].Replay, want) || regs[0].NativeAdmission == nil || regs[0].NativeBinding == nil || regs[0].NativeBinding.Version != 2 {
				t.Fatal("first owner lost Replay/proof")
			}
			owner := regs[0]
			metadataRaw, err := f.store.PublishedMetadata(owner.ArchiveSessionID)
			must(t, err)
			var metadata archive.Metadata
			must(t, json.Unmarshal(metadataRaw, &metadata))
			if !museReplayEqual(metadata.Replay, want) {
				t.Fatal("published metadata lost Replay")
			}
			packed, err := cloud.Get(t.Context(), metadata.SourceBundle.Key)
			must(t, err)
			if len(packed) == 0 {
				t.Fatal("source PUT missing")
			}
			_, err = verifyPublicationsWithin(t.Context(), f.home, f.cfg, f.env, f.store, cloud)
			must(t, err)
			normal := listedIDs(t, f.env)
			replays := listedIDs(t, f.env, "--replays", "only")
			if want == nil {
				if strings.Join(normal, "") != owner.ArchiveSessionID || len(replays) != 0 {
					t.Fatal("ordinary list classification changed")
				}
			} else if len(normal) != 0 || strings.Join(replays, "") != owner.ArchiveSessionID {
				t.Fatal("replay list classification lost")
			}
			// A later marker in an actual owner continuation never relabels first owner.
			f.env.LookupEnv = func(key string) (string, bool) {
				if key == archive.ReplayEnv {
					return "different-later-run", true
				}
				return museTestIdentityEnvironment(key)
			}
			if code := runHookCommand([]string{"--harness", "muse"}, bytes.NewReader(payload), &stderr, f.env); code != 0 {
				t.Fatal("continuation failed")
			}
			must(t, capture.ReplayAdmissionIntentsWithin(t.Context(), f.home, f.observed, f.registry))
			after, found, err := f.store.LoadRegistration(owner.ArchiveSessionID)
			must(t, err)
			if !found || !museReplayEqual(after.Replay, want) {
				t.Fatal("continuation relabeled first owner")
			}
			request, requested, requestErr := f.store.LoadRequest(owner.ArchiveSessionID)
			must(t, requestErr)
			if !requested || len(request.Reasons) == 0 {
				t.Fatal("late marker discarded valid ordinary continuation effects")
			}
			stopPayload, err := json.Marshal(map[string]any{"hook_event_name": "Stop", "session_id": "00000000-0000-7000-8000-000000000001", "transcript_path": nil, "cwd": f.project})
			must(t, err)
			if code := runHookCommand([]string{"--harness", "muse"}, bytes.NewReader(stopPayload), &stderr, f.env); code != 0 {
				t.Fatal("late-marker stop failed")
			}
			must(t, capture.ReplayAdmissionIntentsWithin(t.Context(), f.home, f.observed, f.registry))
			request, requested, requestErr = f.store.LoadRequest(owner.ArchiveSessionID)
			must(t, requestErr)
			if !requested || !request.Urgent() || len(request.HookEvidence) < 2 {
				t.Fatal("late marker discarded ordinary stop/final-response effects")
			}
			// Simulate another startup captured before the first owner committed. Its
			// conflicting provenance must remain pending instead of being falsely ACKed.
			if want == nil {
				candidate.Replay = &archive.Replay{RunID: "conflicting-run"}
			} else {
				candidate.Replay = nil
			}
			conflict, err := json.Marshal(candidate)
			must(t, err)
			must(t, os.WriteFile(candidatePath, conflict, 0600))
			conflictOpts := opts
			conflictOpts.NativeLocatorFactory = nil // normal inventory may also service the existing continuation request
			conflictResult, runErr := collector.Run(t.Context(), f.store, cloud, conflictOpts)
			defer conflictResult.CloseStagedResults()
			if runErr != nil || len(conflictResult.Errors) == 0 {
				t.Fatalf("conflicting startup silently accepted: %+v %v", conflictResult, runErr)
			}
			retained, err := os.ReadFile(candidatePath)
			must(t, err)
			if !bytes.Equal(retained, conflict) {
				t.Fatal("conflicting startup mutated or ACKed")
			}
			after, found, err = f.store.LoadRegistration(owner.ArchiveSessionID)
			must(t, err)
			if !found || !museReplayEqual(after.Replay, want) {
				t.Fatal("startup retry relabeled first owner")
			}
			if want != nil {
				// Explicit malformed fixture: model a replay owner beside an older readable
				// config. Ordinary Save must never create this downgrade in production.
				beforeOwner, err := os.ReadFile(filepath.Join(f.home, "registrations", owner.ArchiveSessionID+".json"))
				must(t, err)
				downgraded := f.cfg
				downgraded.NativeReplayProtection = false
				configRaw, err := json.Marshal(downgraded)
				must(t, err)
				must(t, os.WriteFile(filepath.Join(f.home, "config.json"), configRaw, 0600))
				f.env.LookupEnv = museTestIdentityEnvironment // absence of an incoming marker cannot bypass owner fence
				if code := runHookCommand([]string{"--harness", "muse"}, bytes.NewReader(stopPayload), &stderr, f.env); code != 0 {
					t.Fatal("missing owner floor refusal failed")
				}
				afterOwner, err := os.ReadFile(filepath.Join(f.home, "registrations", owner.ArchiveSessionID+".json"))
				must(t, err)
				pending, err := os.ReadDir(filepath.Join(f.home, "admission-intents"))
				must(t, err)
				if !bytes.Equal(beforeOwner, afterOwner) || len(pending) != 1 {
					t.Fatal("replay owner without strong floor accepted continuation write")
				}
				pendingBytes, err := os.ReadFile(candidatePath)
				must(t, err)
				if !bytes.Equal(pendingBytes, conflict) {
					t.Fatal("missing-fence owner mutated pending startup")
				}
			}

		})
	}
}

func museReplayEqual(a, b *archive.Replay) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func TestMuseReplayMarkerRefusesOldFenceWithoutOrdinaryFallback(t *testing.T) {
	t.Parallel()
	f := newMuseRootCaptureFixture(t)
	if f.cfg.NativeReplayProtection {
		t.Fatal("fixture unexpectedly autoenabled stronger floor")
	}
	f.env.LookupEnv = func(key string) (string, bool) {
		if key == archive.ReplayEnv {
			return "run-old-fence", true
		}
		return museTestIdentityEnvironment(key)
	}
	payload, err := json.Marshal(map[string]any{"hook_event_name": "SessionStart", "session_id": "00000000-0000-7000-8000-000000000001", "transcript_path": nil, "cwd": f.project, "source": "startup"})
	must(t, err)
	var stderr bytes.Buffer
	if code := runHookCommand([]string{"--harness", "muse"}, bytes.NewReader(payload), &stderr, f.env); code != 0 {
		t.Fatal("safe refusal should remain quiet")
	}
	regs, err := f.store.LoadRegistrations()
	must(t, err)
	if len(regs) != 0 {
		t.Fatal("old floor admitted replay as ordinary")
	}
	entries, err := os.ReadDir(filepath.Join(f.home, "admission-intents"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatal("old floor emitted candidate")
	}
}
