package discovery

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
	"github.com/wangjohn/agent-archive/internal/testutil/recoverytest"
)

func TestCopiedNativeSourcesUseOriginalCreationConsent(t *testing.T) {
	t.Parallel()
	for _, recent := range []bool{false, true} {
		t.Run(map[bool]string{false: "old_copy", true: "recent_copy"}[recent], func(t *testing.T) {
			t.Parallel()
			store, cfg, at, root := fixture(t)
			external := t.TempDir()
			created := at.Add(-time.Hour)
			if recent {
				created = at.Add(time.Minute)
			}
			id := writeRollout(t, external, cfg.Archive.Projects[0].Root, created, 1, "sessions")
			name := "rollout-2026-10-01T12-00-00-" + id + ".jsonl"
			raw, err := os.ReadFile(filepath.Join(external, "sessions", name))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(root, "sessions"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "sessions", name), raw, 0600); err != nil {
				t.Fatal(err)
			}
			h, err := run(context.Background(), store, cfg, Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return at.Add(2 * time.Minute) }}, syntheticSupport)
			want := 0
			if recent {
				want = 1
			}
			if err != nil || h.Registered != want {
				t.Fatalf("copied source consent: %#v %v", h, err)
			}
			regs, err := store.LoadRegistrations()
			if err != nil || len(regs) != want {
				t.Fatalf("registrations=%v error=%v", regs, err)
			}
			if recent && (regs[0].Origin != archive.SessionOriginDiscovery || !regs[0].SessionStartedAt.Equal(created)) {
				t.Fatal("copy changed native attribution")
			}
		})
	}
}

func TestUnknownDiscoveryIdentityRequiresCensusBeforeAllocation(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	native := writeRollout(t, root, cfg.Archive.Projects[0].Root, at.Add(time.Minute), 1, "sessions")
	options := Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return at.Add(2 * time.Minute) }}
	adapters := []SourceAdapter{codexAdapter{supported: syntheticSupport}}
	h, err := runWithAdapters(context.Background(), store, cfg, options, adapters)
	if err != nil || h.Registered != 0 || h.Outcomes["admission_retry"] == 0 {
		t.Fatalf("unknown identity allocated before census: %#v %v", h, err)
	}
	regs, err := store.LoadRegistrations()
	if err != nil || len(regs) != 0 {
		t.Fatalf("unexpected registrations: %v %v", regs, err)
	}
	if err := recoverytest.Exhaust(context.Background(), store, state.SessionIndexRecoverySlice, false); err != nil {
		t.Fatal(err)
	}
	h, err = runWithAdapters(context.Background(), store, cfg, options, adapters)
	if err != nil || h.Registered != 1 {
		t.Fatalf("census absence did not admit: %#v %v", h, err)
	}
	if _, found, err := store.ArchiveSessionID(sessionKey("codex", native)); err != nil || !found {
		t.Fatalf("missing owner: %v %v", found, err)
	}
}

// Discovery's wait for one key's census proof must not refuse live hooks'
// unrelated misses, such as a Claude Code subagent's first SubagentStop,
// until the collector's next pass.
func TestDiscoveryAbsenceRequestKeepsHookAdmissionOpen(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	if err := recoverytest.Exhaust(context.Background(), store, state.SessionIndexRecoverySlice, false); err != nil {
		t.Fatal(err)
	}
	native := writeRollout(t, root, cfg.Archive.Projects[0].Root, at.Add(time.Minute), 1, "sessions")
	options := Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return at.Add(2 * time.Minute) }}
	adapters := []SourceAdapter{codexAdapter{supported: syntheticSupport}}
	h, err := runWithAdapters(context.Background(), store, cfg, options, adapters)
	if err != nil || h.Registered != 0 || h.Outcomes["admission_retry"] == 0 {
		t.Fatalf("unknown identity allocated before census: %#v %v", h, err)
	}
	if status, err := store.SessionIndexRecoveryStatus(); err != nil || !status.Complete {
		t.Fatalf("discovery withdrew the complete census: %#v %v", status, err)
	}
	child := sessionKey("claude", "claude-parent:subagent:agent-1")
	if _, _, err := store.EnsureArchiveSessionID(child); err != nil {
		t.Fatalf("hook miss refused while discovery waits: %v", err)
	}
	if complete, err := store.RecoverSessionIndexScheduled(context.Background(), state.SessionIndexRecoverySlice); err != nil || !complete {
		t.Fatalf("one recovery slice: complete=%v err=%v", complete, err)
	}
	h, err = runWithAdapters(context.Background(), store, cfg, options, adapters)
	if err != nil || h.Registered != 1 {
		t.Fatalf("certified absence did not admit: %#v %v", h, err)
	}
	if _, found, err := store.ArchiveSessionID(sessionKey("codex", native)); err != nil || !found {
		t.Fatalf("missing owner: %v %v", found, err)
	}
}

func TestDiscoveryBothIndexLossRecoversExistingIdentity(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	native := writeRollout(t, root, cfg.Archive.Projects[0].Root, at.Add(time.Minute), 1, "sessions")
	options := Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return at.Add(2 * time.Minute) }}
	if _, err := run(context.Background(), store, cfg, options, syntheticSupport); err != nil {
		t.Fatal(err)
	}
	regs, err := store.LoadRegistrations()
	if err != nil || len(regs) != 1 {
		t.Fatalf("initial owner: %v %v", regs, err)
	}
	before := regs[0]
	// Registrations remain authoritative even if every derived lookup is lost.
	for _, directory := range []string{"sessions", "sessions-v1"} {
		if err := os.RemoveAll(filepath.Join(store.Home(), directory)); err != nil {
			t.Fatal(err)
		}
	}
	if _, found, err := store.ArchiveSessionID(sessionKey("codex", native)); err != nil || found {
		t.Fatalf("lost derived indexes unexpectedly retained lookup: %v %v", found, err)
	}
	// Publication enumerates authoritative registrations, not identity indexes.
	result, err := collector.Run(context.Background(), store, storagetest.NewMemoryStore(), collector.Options{Sources: builtin.NewBuiltins(), MachineID: cfg.MachineID, AcceptSession: cfg.AcceptSession, Now: func() time.Time { return at.Add(3 * time.Minute) }})
	if err != nil || len(result.Published) != 1 {
		t.Fatalf("lost indexes blocked registered publication: %#v %v", result, err)
	}
	owner, found, err := store.ArchiveSessionID(sessionKey("codex", native))
	if err != nil || !found || owner != before.ArchiveSessionID {
		t.Fatalf("collector startup did not restore authority: owner=%q found=%v err=%v", owner, found, err)
	}
	adapters := []SourceAdapter{codexAdapter{supported: syntheticSupport}}
	h, err := runWithAdapters(context.Background(), store, cfg, options, adapters)
	if err != nil || h.Registered != 0 {
		t.Fatalf("lost indexes caused allocation: %#v %v", h, err)
	}
	if err := recoverytest.Exhaust(context.Background(), store, state.SessionIndexRecoverySlice, false); err != nil {
		t.Fatal(err)
	}
	h, err = runWithAdapters(context.Background(), store, cfg, options, adapters)
	if err != nil || h.Registered != 0 {
		t.Fatalf("recovered identity reallocated: %#v %v", h, err)
	}
	owner, found, err = store.ArchiveSessionID(sessionKey("codex", native))
	if err != nil || !found || owner != before.ArchiveSessionID {
		t.Fatalf("recovered owner=%q found=%v err=%v", owner, found, err)
	}
	regs, err = store.LoadRegistrations()
	if err != nil || len(regs) != 1 || regs[0].ArchiveSessionID != before.ArchiveSessionID || !regs[0].AdmittedAt.Equal(before.AdmittedAt) || regs[0].DestinationID != before.DestinationID {
		t.Fatalf("authority changed: %v %v", regs, err)
	}
}

func TestAbsentIdentityNeedsCompletedRecoveryMarker(t *testing.T) {
	t.Parallel()
	store, _, _, _ := fixture(t)
	key := sessionKey("codex", "new-session")
	unlock, err := local.NamedLock(store.Home(), "hooks.lock")
	if err != nil {
		t.Fatal(err)
	}
	if absent, err := store.SessionIndexAbsent(key); err != nil || absent {
		t.Fatalf("missing indexes prove absence: %v %v", absent, err)
	}
	err = store.RequestSessionIndexRecovery(key)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := recoverytest.Exhaust(context.Background(), store, state.SessionIndexRecoverySlice, false); err != nil {
		t.Fatal(err)
	}
	if absent, err := store.SessionIndexAbsent(key); err != nil || !absent {
		t.Fatalf("census absence missing: %v %v", absent, err)
	}
	if err := store.MarkSessionIndexRecoveryNeeded(); err != nil {
		t.Fatal(err)
	}
	if absent, err := store.SessionIndexAbsent(key); absent || !errors.Is(err, state.ErrSessionIndexRecoveryRequired) {
		t.Fatalf("incomplete census authorizes: %v %v", absent, err)
	}
}

func TestPausedContinuationCannotReplaceRegisteredLocator(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	writeRollout(t, root, cfg.Archive.Projects[0].Root, at.Add(time.Minute), 1, "sessions")
	if _, err := run(context.Background(), store, cfg, Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return at.Add(2 * time.Minute) }}, syntheticSupport); err != nil {
		t.Fatal(err)
	}
	regs, err := store.LoadRegistrations()
	if err != nil || len(regs) != 1 {
		t.Fatalf("initial registration: %v %v", regs, err)
	}
	before := regs[0]
	archived := filepath.Join(root, "archived_sessions", filepath.Base(before.TranscriptPath))
	if err := os.MkdirAll(filepath.Dir(archived), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(before.TranscriptPath, archived); err != nil {
		t.Fatal(err)
	}
	if _, err := config.SetPaused(store.Home(), true, at.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	header := sourcefacts.ReadHeader(context.Background(), root, archived)
	source := SourceDescriptor{Kind: archive.SourceKindFile, StableKey: header.Meta.ID, Root: root, Locator: archived, Priority: 1}
	if _, _, err := admit(store, candidateFromHeader(header, source), before.ProjectRoot, before.DiscoveryGeneration, at.Add(3*time.Minute)); err == nil {
		t.Fatal("paused continuation changed registration")
	}
	after, found, err := store.LoadRegistration(before.ArchiveSessionID)
	if err != nil || !found || after.TranscriptPath != before.TranscriptPath {
		t.Fatalf("paused locator mutation: %v %v", after, err)
	}
}

func TestDiscoveryCannotSupplyUnconfinedLocatorToPathlessHook(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	project := cfg.Archive.Projects[0].Root
	native := writeRollout(t, root, project, at.Add(time.Minute), 1, "sessions")
	// Actual shipped decoding and capture create the pathless hook owner.
	payload := map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": native, "cwd": project}
	if err := handleCodexHook(store.Home(), payload, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	regs, err := store.LoadRegistrations()
	if err != nil || len(regs) != 1 || regs[0].Origin != archive.SessionOriginHook || regs[0].TranscriptPath != "" {
		t.Fatalf("pathless hook fixture: %#v %v", regs, err)
	}
	before := regs[0]
	h, err := runWithAdapters(context.Background(), store, cfg, Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return at.Add(2 * time.Minute) }}, []SourceAdapter{codexAdapter{supported: syntheticSupport}})
	if err != nil || h.Registered != 0 {
		t.Fatalf("continuation scan: %#v %v", h, err)
	}
	// A swapped leaf would be accepted by the legacy hook provider if discovery
	// supplied its path without retained confinement. No such path may be adopted.
	path := filepath.Join(root, "sessions", "rollout-2026-10-01T12-00-00-"+native+".jsonl")
	outside := filepath.Join(t.TempDir(), filepath.Base(path))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	after, found, err := store.LoadRegistration(before.ArchiveSessionID)
	if err != nil || !found {
		t.Fatal("lost hook owner", err)
	}
	_, err = collector.ReadLocalBundle(context.Background(), store.Home(), after, at.Add(3*time.Minute), "", builtin.NewBuiltins())
	if !errors.Is(err, collector.ErrNoTranscript) {
		t.Fatalf("discovery supplied unconfined hook source: path=%q bundle error=%v", after.TranscriptPath, err)
	}
	if after.TranscriptPath != "" || after.Origin != before.Origin || !after.AdmittedAt.Equal(before.AdmittedAt) || after.DestinationID != before.DestinationID {
		t.Fatalf("hook attribution changed: %#v", after)
	}
}
