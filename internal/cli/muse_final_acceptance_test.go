package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/discovery"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
	"github.com/wangjohn/agent-archive/internal/trace"
)

// Count every listing entry point: embedding MemoryStore without overriding
// ListPage and ListRange would let large cohorts bypass a List-only assertion.
type museAcceptanceStore struct {
	*storagetest.MemoryStore
	puts         atomic.Int64
	lists        atomic.Int64
	sourcePuts   atomic.Int64
	metadataPuts atomic.Int64
}

func (s *museAcceptanceStore) Put(ctx context.Context, key string, data []byte) error {
	s.puts.Add(1)
	if strings.HasSuffix(key, "/metadata.json") {
		s.metadataPuts.Add(1)
	} else if strings.HasPrefix(key, "sessions/") {
		s.sourcePuts.Add(1)
	}
	return s.MemoryStore.Put(ctx, key, data)
}

func (s *museAcceptanceStore) List(ctx context.Context, prefix string) ([]storage.Object, error) {
	s.lists.Add(1)
	return s.MemoryStore.List(ctx, prefix)
}

func (s *museAcceptanceStore) ListPage(ctx context.Context, prefix, continuation string, limit int32) (storage.ObjectPage, error) {
	s.lists.Add(1)
	return s.MemoryStore.ListPage(ctx, prefix, continuation, limit)
}

func (s *museAcceptanceStore) ListRange(ctx context.Context, prefix, after, through string) ([]storage.Object, error) {
	s.lists.Add(1)
	return s.MemoryStore.ListRange(ctx, prefix, after, through)
}

// Observe the existing SourceEnvironment without replacing source passes,
// snapshots, filters or admission evidence; concrete proof identity survives.
type museAcceptanceSources struct {
	agentapi.SourcesLookup
	opened map[string][]*agentapi.NativeReadBudget
}

func (s *museAcceptanceSources) LookupSources(name string) (agentapi.SourceProvider, agentapi.TranscriptFilter, bool) {
	provider, filter, ok := s.SourcesLookup.LookupSources(name)
	if !ok {
		return provider, filter, false
	}
	return museAcceptanceProvider{SourceProvider: provider, name: name, owner: s}, filter, true
}

type museAcceptanceProvider struct {
	agentapi.SourceProvider
	name  string
	owner *museAcceptanceSources
}

func (p museAcceptanceProvider) OpenPass(ctx context.Context, env agentapi.SourceEnvironment) (agentapi.SourcePass, error) {
	p.owner.opened[p.name] = append(p.owner.opened[p.name], env.ReadBudget)
	return p.SourceProvider.OpenPass(ctx, env)
}

func (s *museAcceptanceSources) requireShared(t *testing.T, budget *agentapi.NativeReadBudget) {
	t.Helper()
	for _, name := range []string{"claude", "codex", "cursor", "muse"} {
		if len(s.opened[name]) == 0 {
			t.Fatalf("%s source pass was not observed", name)
		}
		for _, actual := range s.opened[name] {
			if actual == nil || actual != budget {
				t.Fatalf("%s used a different source ledger", name)
			}
		}
	}
}

func (s *museAcceptanceStore) reset() {
	s.puts.Store(0)
	s.lists.Store(0)
	s.sourcePuts.Store(0)
	s.metadataPuts.Store(0)
}

func museAcceptanceQueue(t *testing.T, f museRootCaptureFixture, owners int) {
	t.Helper()
	for i := range owners {
		id := fmt.Sprintf("00000000-0000-7000-8000-%012x", i+1)
		path := filepath.Join(f.root, "2026", "10", "15", id, "session.jsonl")
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, museNativeBytes(t, "exec", id, f.project), 0600); err != nil {
			t.Fatal(err)
		}
		env := f.env
		env.LookupEnv = func(key string) (string, bool) {
			switch museTestEnvironmentKey(key) {
			case museTestSessionID, museTestThreadID:
				return id, true
			default:
				return "", false
			}
		}
		payload, err := json.Marshal(map[string]any{"hook_event_name": "SessionStart", "session_id": id, "transcript_path": nil, "cwd": f.project, "source": "startup"})
		if err != nil {
			t.Fatal(err)
		}
		var stderr bytes.Buffer
		if code := runHookCommand([]string{"--harness", "muse"}, bytes.NewReader(payload), &stderr, env); code != 0 || stderr.Len() != 0 {
			t.Fatalf("owner%d hook: code=%d stderr=%s", i, code, stderr.String())
		}
	}
	regs, err := f.store.LoadRegistrations()
	if err != nil || len(regs) != 0 {
		t.Fatalf("hooks created owners before admission: %d %v", len(regs), err)
	}
}

// Keep the concrete builtin Filter and snapshots: decorating those objects can
// legitimately refuse their leased-proof identity and would test another path.
func museAcceptanceOptions(t *testing.T, f museRootCaptureFixture, ledgers *[]*agentapi.NativeReadBudget) collector.Options {
	t.Helper()
	return collector.Options{Sources: f.registry, Parsers: f.registry, Decoders: f.registry, MachineID: f.cfg.MachineID,
		NativeRoots: f.cfg.NativeRoots, AcceptSession: f.cfg.AcceptSession,
		Now: func() time.Time { return f.observed }, RepoKey: func(string) string { return "" },
		NativeLocatorFactory: func(ctx context.Context, pass collector.NativeLocatorPass) (agentapi.NativeLocatorLookup, func() error, error) {
			*ledgers = append(*ledgers, pass.ReadBudget)
			inventory, err := discovery.NewNativeInventory(ctx, f.home, pass.Roots, pass.Queries, pass.ReadBudget, builtin.ValidateNativeLocator)
			if err != nil {
				return nil, nil, err
			}
			return inventory, inventory.Close, nil
		}}
}

func museAcceptanceReleased(t *testing.T, ledgers []*agentapi.NativeReadBudget) int64 {
	t.Helper()
	if len(ledgers) == 0 {
		t.Fatal("shared factory ledger was never observed")
	}
	var peak int64
	for _, ledger := range ledgers {
		used, high := ledger.Charged()
		if used != 0 || high > agentapi.PassReadBudget {
			t.Fatalf("shared ledger used=%d peak=%d limit=%d", used, high, agentapi.PassReadBudget)
		}
		peak = max(peak, high)
	}
	return peak
}

func museAcceptanceCollect(t *testing.T, f museRootCaptureFixture, remote *museAcceptanceStore, opts collector.Options, want int) {
	t.Helper()
	published := map[string]bool{}
	for tick := 0; tick < 128 && len(published) < want; tick++ {
		result, err := collector.Run(t.Context(), f.store, remote, opts)
		result.CloseStagedResults()
		if err != nil {
			t.Fatalf("tick%d result=%+v run=%v", tick, result, err)
		}
		for key, failure := range result.Errors {
			// Bounded discovery may require more than one inventory tick.
			// This is not successful per-owner evidence: the final union must
			// still contain every actual publication before the loop ends.
			if key != "muse-startup" || agentapi.Failure(failure) != agentapi.Unavailable {
				t.Fatalf("tick%d unexpected %s: %v", tick, key, failure)
			}
			t.Logf("cold tick%d pending startup proof: %v", tick, failure)
		}
		for _, id := range result.Published {
			published[id] = true
		}
	}
	if len(published) != want {
		t.Fatalf("complete published union=%d want=%d", len(published), want)
	}
}

func TestMuseWarmUnchangedCohortsConsumeNoFilterInputUploadOrStorageListing(t *testing.T) {
	// Sequential: trace is process-wide. Full campaign explicitly selects the
	// larger durable-hook cohorts; their absence is never passing evidence.
	cohorts := []int{1}
	if os.Getenv("AGENT_ARCHIVE_PERF") != "" {
		cohorts = append(cohorts, 128, 512)
	}
	for _, owners := range cohorts {
		t.Run(fmt.Sprintf("owners%d", owners), func(t *testing.T) {
			f := newMuseRootCaptureFixture(t)
			museAcceptanceQueue(t, f, owners)
			remote := &museAcceptanceStore{MemoryStore: storagetest.NewMemoryStore()}
			var ledgers []*agentapi.NativeReadBudget
			opts := museAcceptanceOptions(t, f, &ledgers)
			disable := trace.Enable()
			defer disable()
			museAcceptanceCollect(t, f, remote, opts, owners)
			var first bytes.Buffer
			trace.Write(&first)
			disable()
			if museTraceFilterInput(t, first.String()) <= 0 || remote.sourcePuts.Load() < int64(owners) || remote.metadataPuts.Load() < int64(owners) {
				t.Fatalf("initial Filter/upload positive control absent: source=%d metadata=%d trace=%s", remote.sourcePuts.Load(), remote.metadataPuts.Load(), first.String())
			}
			regs, err := f.store.LoadRegistrations()
			if err != nil || len(regs) != owners {
				t.Fatalf("admitted owner count=%d want=%d: %v", len(regs), owners, err)
			}
			for _, reg := range regs {
				if reg.NativeAdmission == nil || reg.NativeBinding == nil || reg.NativeBinding.Checkpoint == nil {
					t.Fatal("publication bypassed root admission or source checkpoint")
				}
			}
			before := snapshotTree(t, filepath.Join(f.home, "published"))
			remote.reset()
			disable = trace.Enable()
			defer disable()
			started := time.Now()
			for tick := range 3 {
				result, err := collector.Run(t.Context(), f.store, remote, opts)
				result.CloseStagedResults()
				if err != nil || len(result.Errors) != 0 || len(result.Published) != 0 || len(result.Skipped) != owners {
					t.Fatalf("unchanged tick%d result=%+v run=%v", tick, result, err)
				}
			}
			warmElapsed := time.Since(started)
			var warm bytes.Buffer
			trace.Write(&warm)
			if museTraceFilterInput(t, warm.String()) != 0 || remote.puts.Load() != 0 || remote.lists.Load() != 0 || before != snapshotTree(t, filepath.Join(f.home, "published")) {
				t.Fatalf("unchanged Filter input/storage/publication changed: puts=%d lists=%d trace=%s", remote.puts.Load(), remote.lists.Load(), warm.String())
			}
			disable()
			// Removing a real success signature forces the same concrete
			// source path to read again, proving zero warm bytes is observable.
			if err := f.store.RemoveScanSignature(regs[0].ArchiveSessionID); err != nil {
				t.Fatal(err)
			}
			disable = trace.Enable()
			defer disable()
			control, controlErr := collector.Run(t.Context(), f.store, remote, opts)
			control.CloseStagedResults()
			var reread bytes.Buffer
			trace.Write(&reread)
			if controlErr != nil || len(control.Errors) != 0 || museTraceFilterInput(t, reread.String()) <= 0 {
				t.Fatalf("forced concrete reread not observed: %+v %v trace=%s", control, controlErr, reread.String())
			}
			peak := museAcceptanceReleased(t, ledgers)
			t.Logf("owners=%d unchanged_ticks=3 filter_input_bytes=0 all_storage_puts=0 all_storage_lists=0 shared_peak_bytes=%d warm_wall=%s full_record_visits=unavailable filter_invocations=unavailable", owners, peak, warmElapsed)
		})
	}
}

func TestMuseAndExistingAgentsSharePressureAndRecoverWithoutNewReserve(t *testing.T) {
	// Sequential: use the same real Muse root admission as the cohort test,
	// with ordinary legacy file registrations for the three existing agents.
	f := newMuseRootCaptureFixture(t)
	museAcceptanceQueue(t, f, 1)
	// This pressure scenario grows an active synthetic root. Remove the
	// fixture's known final session.end row before its first admission,
	// rather than assuming an ended producer can emit another turn.
	raw, readErr := os.ReadFile(f.path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	lines := bytes.Split(bytes.TrimSuffix(raw, []byte("\n")), []byte("\n"))
	if len(lines) != 8 || !bytes.Contains(lines[len(lines)-1], []byte(`"payload_type":"session.end"`)) {
		t.Fatal("synthetic conversation's final end row changed")
	}
	if err := os.WriteFile(f.path, append(bytes.Join(lines[:len(lines)-1], []byte("\n")), '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	f.cfg.Harnesses = []string{"claude", "codex", "cursor", "muse"}
	if err := config.Save(f.home, f.cfg); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		content string
	}{
		{"claude", `{"type":"assistant","sessionId":"synthetic-claude","message":{"role":"assistant","content":[{"type":"text","text":"visible"}]}}` + "\n"},
		{"codex", `{"type":"turn_context","model":"synthetic"}` + "\n" + `{"type":"response_item","id":"m1","payload":{"type":"message","role":"assistant","content":"visible"}}` + "\n"},
		{"cursor", `{"role":"user","message":{"content":[{"type":"text","text":"visible"}]}}` + "\n"},
	} {
		path := filepath.Join(f.project, tc.name+".jsonl")
		if err := os.WriteFile(path, []byte(tc.content), 0600); err != nil {
			t.Fatal(err)
		}
		reg := archive.SessionRegistration{ArchiveSessionID: "synthetic-" + tc.name, NativeSessionID: "synthetic-" + tc.name,
			Harness: archive.Harness{Name: tc.name}, ProjectRoot: f.project, ProjectID: archive.ProjectID(f.project),
			DestinationID: f.cfg.DestinationID(), TranscriptPath: path, SessionStartedAt: f.birth,
			RegisteredAt: f.observed, AdmittedAt: f.observed}
		if err := f.store.SaveRegistration(reg); err != nil {
			t.Fatal(err)
		}
	}
	remote := &museAcceptanceStore{MemoryStore: storagetest.NewMemoryStore()}
	var ledgers []*agentapi.NativeReadBudget
	opts := museAcceptanceOptions(t, f, &ledgers)
	museAcceptanceCollect(t, f, remote, opts, 4)
	regs, err := f.store.LoadRegistrations()
	if err != nil || len(regs) != 4 {
		t.Fatalf("four concrete agents were not admitted: %d %v", len(regs), err)
	}
	var museOwner archive.SessionRegistration
	for _, reg := range regs {
		if reg.Harness.Name == "muse" {
			museOwner = reg
		}
	}
	if museOwner.ArchiveSessionID == "" {
		t.Fatal("actual admitted Muse owner absent")
	}
	musePublishedPath := filepath.Join(f.home, "published", museOwner.ArchiveSessionID+".json")
	before := snapshotTree(t, musePublishedPath)
	// Grow only after the small baseline is committed. Four MiB remains for
	// ordinary durable-state checks; the one-MiB visible record in each real
	// source must compete for decoding/build headroom on the same ledger.
	visible := strings.Repeat("bounded synthetic growth ", (1<<20)/25)
	for _, reg := range regs {
		var row []byte
		var marshalErr error
		switch agentmeta.ID(reg.Harness.Name) {
		case agentmeta.Muse:
			row, marshalErr = json.Marshal(map[string]any{
				"schema_version": 1, "id": "00000000-0000-0000-0000-0000000003f2",
				"stream":   map[string]any{"kind": "session", "id": reg.NativeSessionID},
				"sequence": 9, "recorded_at": int64(1792083865882704), "record_type": "event", "durability": "durable", "causation_id": nil,
				"payload_type": "runtime.session", "payload_schema_version": 1,
				"payload": map[string]any{"kind": "run", "run_id": "33333333-3333-4333-8333-333333333333",
					"source_run_record_id": "77777777-7777-4777-8777-777777777777", "source_run_record_sequence": 3,
					"event": map[string]any{"kind": "assistant_message_committed", "message_id": "synthetic-growth", "response_id": "synthetic-growth-response", "text": visible}},
			})
		case agentmeta.Claude:
			row, marshalErr = json.Marshal(map[string]any{"type": "assistant", "sessionId": reg.NativeSessionID,
				"message": map[string]any{"role": "assistant", "content": []map[string]any{{"type": "text", "text": visible}}}})
		case agentmeta.Codex:
			row, marshalErr = json.Marshal(map[string]any{"type": "response_item", "id": "growth",
				"payload": map[string]any{"type": "message", "role": "assistant", "content": visible}})
		case agentmeta.Cursor:
			row, marshalErr = json.Marshal(map[string]any{"role": "assistant", "message": map[string]any{"content": []map[string]any{{"type": "text", "text": visible}}}})
		default:
			t.Fatalf("unexpected agent %s", reg.Harness.Name)
		}
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		file, openErr := os.OpenFile(reg.TranscriptPath, os.O_APPEND|os.O_WRONLY, 0)
		if openErr != nil {
			t.Fatal(openErr)
		}
		_, writeErr := file.Write(append(row, '\n'))
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			t.Fatalf("grow %s: %v %v", reg.Harness.Name, writeErr, closeErr)
		}
		t.Logf("agent=%s appended_record_bytes=%d", reg.Harness.Name, len(row)+1)
	}
	ready := f.observed.Add(time.Hour)
	opts.Now = func() time.Time { return ready }
	for _, reg := range regs {
		if err := f.store.RemoveScanSignature(reg.ArchiveSessionID); err != nil {
			t.Fatal(err)
		}
	}
	// Borrow existing pass lifetime headroom; no replacement ledger, cache,
	// raised limit or changed session ceiling. Return it after the actual pass.
	factory := opts.NativeLocatorFactory
	var pressureBudget *agentapi.NativeReadBudget
	var held int64
	defer func() {
		if pressureBudget != nil && held > 0 {
			pressureBudget.ReleaseUnscoped(held)
		}
	}()
	opts.NativeLocatorFactory = func(ctx context.Context, pass collector.NativeLocatorPass) (agentapi.NativeLocatorLookup, func() error, error) {
		lookup, closeLookup, err := factory(ctx, pass)
		if err != nil {
			return nil, nil, err
		}
		pressureBudget = pass.ReadBudget
		held = pressureBudget.Available() - (4 << 20)
		if held <= 0 || !pressureBudget.ReserveUnscoped(held) {
			_ = closeLookup()
			t.Fatal("could not occupy the existing shared ledger")
		}
		return lookup, closeLookup, nil
	}
	sources := &museAcceptanceSources{SourcesLookup: f.registry, opened: map[string][]*agentapi.NativeReadBudget{}}
	opts.Sources = sources
	remote.reset()
	pressure, pressureErr := collector.Run(t.Context(), f.store, remote, opts)
	pressure.CloseStagedResults()
	if pressureBudget == nil {
		t.Fatal("pressure pass bypassed shared factory ledger")
	}
	occupied := held
	pressureBudget.ReleaseUnscoped(held)
	held = 0
	sources.requireShared(t, pressureBudget)
	if pressureErr != nil || len(pressure.Errors) != 0 || len(pressure.Published) != 3 || len(pressure.Deferred) != 1 || pressure.Deferred[museOwner.ArchiveSessionID] != collector.DeferredReadBudget {
		t.Fatalf("ordinary headroom/native retry: %+v %v", pressure, pressureErr)
	}
	ordinaryPublished := map[string]bool{}
	for _, id := range pressure.Published {
		ordinaryPublished[id] = true
	}
	settled := map[string]string{}
	for _, reg := range regs {
		if reg.Harness.Name != "muse" {
			if !ordinaryPublished[reg.ArchiveSessionID] {
				t.Fatalf("%s growth did not publish under native pressure", reg.Harness.Name)
			}
			settled[reg.ArchiveSessionID] = snapshotTree(t, filepath.Join(f.home, "published", reg.ArchiveSessionID+".json"))
		}
		if _, blocked, err := f.store.LoadBlocked(reg.ArchiveSessionID); err != nil || blocked {
			t.Fatalf("%s pressure became a session limit: blocked=%v %v", reg.Harness.Name, blocked, err)
		}
	}
	if before != snapshotTree(t, musePublishedPath) || remote.sourcePuts.Load() != 3 || remote.metadataPuts.Load() != 3 {
		t.Fatalf("pressure lost Muse baseline or duplicated ordinary upload: source=%d metadata=%d", remote.sourcePuts.Load(), remote.metadataPuts.Load())
	}
	opts.NativeLocatorFactory = factory
	sources.opened = map[string][]*agentapi.NativeReadBudget{}
	remote.reset()
	disable := trace.Enable()
	defer disable()
	recovered, recoverErr := collector.Run(t.Context(), f.store, remote, opts)
	recovered.CloseStagedResults()
	var recoveryTrace bytes.Buffer
	trace.Write(&recoveryTrace)
	if recoverErr != nil || len(recovered.Errors) != 0 || len(recovered.Deferred) != 0 || recovered.Scanned != 4 || len(recovered.Published) != 1 || recovered.Published[0] != museOwner.ArchiveSessionID || len(recovered.Skipped) != 3 {
		t.Fatalf("released pressure did not publish Muse beside settled agents: %+v %v", recovered, recoverErr)
	}
	sources.requireShared(t, ledgers[len(ledgers)-1])
	sourceInfo, statErr := os.Stat(museOwner.TranscriptPath)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if museTraceFilterInput(t, recoveryTrace.String()) != sourceInfo.Size() || remote.sourcePuts.Load() != 1 || remote.metadataPuts.Load() != 1 {
		t.Fatalf("recovery repeated Filter input/upload: source=%d metadata=%d filter_bytes=%d want_muse_bytes=%d trace=%s", remote.sourcePuts.Load(), remote.metadataPuts.Load(), museTraceFilterInput(t, recoveryTrace.String()), sourceInfo.Size(), recoveryTrace.String())
	}
	for id, snapshot := range settled {
		if snapshot != snapshotTree(t, filepath.Join(f.home, "published", id+".json")) {
			t.Fatal("recovery rewrote a settled ordinary snapshot")
		}
	}
	peak := museAcceptanceReleased(t, ledgers)
	t.Logf("agents=claude,codex,cursor,muse shared_limit=%d cache_reserve=%d session_ceiling=%d occupied=%d ordinary_growth_publications=3 deferred_muse=1 recovered_muse=1 shared_peak_bytes=%d released=0", agentapi.PassReadBudget, agentapi.PassCacheReserve, collector.SessionReadCeiling, occupied, peak)
}
