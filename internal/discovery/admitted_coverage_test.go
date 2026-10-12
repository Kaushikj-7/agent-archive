package discovery

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

func TestHistoricalOwnerDoesNotRequestNativeCoverage(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	project := cfg.Archive.Projects[0].Root
	wanted := writeRollout(t, root, project, at, 1, "sessions")
	const projectID = "11111111111111111111111111111111"
	cfg.Discovery.Enabled = false
	cfg.HistoricalProjectProtection = true
	cfg.HistoricalProjects = []config.HistoricalProject{{ID: projectID, Label: "Retired", Permissions: []config.HistoricalProjectPermission{{DestinationID: cfg.DestinationID(), Enabled: true}}}}
	reg := archive.SessionRegistration{ArchiveSessionID: "historical", NativeSessionID: wanted, Harness: archive.Harness{Name: "codex"}, ProjectID: projectID, Origin: archive.SessionOriginImport, ImportBatch: archive.NewImportBatch("2026-10-01-1"), AdmittedAt: at, RegisteredAt: at, SessionStartedAt: at, DestinationID: cfg.DestinationID(), TranscriptPath: filepath.Join(root, "sessions", "rollout-2026-10-01T12-00-00-"+wanted+".jsonl"), StagedImportSHA256: strings.Repeat("a", 64), HistoricalProject: &archive.HistoricalProjectProof{Version: 1, ProjectID: projectID, DestinationID: cfg.DestinationID(), OriginalCwd: project, CanonicalCwd: project, Label: "Retired"}}
	if !cfg.AcceptSession(reg) {
		t.Fatal("historical retained maintenance must remain authorized")
	}
	l, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := l.Close(); err != nil {
			t.Error(err)
		}
	}()
	sequence := l.coverage.Sequence
	budget := l.NativeReadBudget().Available()
	if err := l.PrepareRegistered(t.Context(), cfg, []archive.SessionRegistration{reg}, Options{}); err != nil {
		t.Fatal(err)
	}
	if len(l.coverage.Requests) != 0 || l.coverage.Sequence != sequence || l.coverageDirty || len(l.observations) != 0 || l.NativeReadBudget().Available() != budget {
		t.Fatal("historical retained owner triggered native coverage or source observation")
	}
	// Disabled future discovery must still prepare an independently admitted
	// live owner beside the historical owner in the same installation.
	liveID := writeRollout(t, root, project, at, 2, "sessions")
	live := archive.SessionRegistration{ArchiveSessionID: "live", NativeSessionID: liveID, Harness: archive.Harness{Name: "codex"}, ProjectRoot: project, ProjectID: archive.ProjectID(project), Origin: archive.SessionOriginHook, AdmittedAt: at, RegisteredAt: at, SessionStartedAt: at, DestinationID: cfg.DestinationID(), TranscriptPath: filepath.Join(root, "sessions", "rollout-2026-10-01T12-00-00-"+liveID+".jsonl")}
	if !cfg.AcceptSession(live) {
		t.Fatal("mixed live owner lost capture permission")
	}
	if err := l.PrepareRegistered(t.Context(), cfg, []archive.SessionRegistration{reg, live}, Options{Now: func() time.Time { return at.Add(time.Minute) }}); err != nil {
		t.Fatal(err)
	}
	set, err := l.Thread(t.Context(), liveID)
	if err != nil || !set.Complete || len(set.Candidates) != 1 {
		t.Fatalf("mixed live owner lost complete native coverage: %+v %v", set, err)
	}
	if _, found := l.coverage.Requests[wanted]; found {
		t.Fatal("mixed coverage requested historical owner")
	}
}

func TestHistoricalPolicySkipsOnlyUnsolicitedDisabledDiscovery(t *testing.T) {
	t.Parallel()
	for _, scenario := range []struct {
		name       string
		historical bool
		enabled    bool
		observed   bool
	}{
		{name: "historical disabled", historical: true},
		{name: "historical explicitly enabled", historical: true, enabled: true, observed: true},
		{name: "legacy disabled", observed: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			store, cfg, at, root := fixture(t)
			writeRollout(t, root, cfg.Archive.Projects[0].Root, at, 1, "sessions")
			cfg.HistoricalProjectProtection = scenario.historical
			cfg.Discovery.Enabled = scenario.enabled
			l, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := l.Close(); err != nil {
					t.Error(err)
				}
			}()
			health, err := Run(t.Context(), store, cfg, Options{Rollouts: l, Now: func() time.Time { return at.Add(time.Minute) }})
			if err != nil {
				t.Fatal(err)
			}
			if (health.Entries > 0) != scenario.observed || (len(l.observations) > 0) != scenario.observed {
				t.Fatalf("native inventory eligibility changed: %+v", health)
			}
		})
	}
}

func TestAdmittedCoverageSliceDoesNotAdmitOtherOwners(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	project := cfg.Archive.Projects[0].Root
	wanted := writeRollout(t, root, project, at.Add(time.Minute), 1, "sessions")
	other := writeRollout(t, root, project, at.Add(time.Minute), 2, "sessions")
	reg := archive.SessionRegistration{ArchiveSessionID: "admitted", NativeSessionID: wanted, Harness: archive.Harness{Name: "codex"}, ProjectRoot: project, ProjectID: archive.ProjectID(project), Origin: archive.SessionOriginHook, AdmittedAt: at.Add(time.Minute), RegisteredAt: at.Add(time.Minute), SessionStartedAt: at.Add(time.Minute), DestinationID: cfg.DestinationID(), TranscriptPath: filepath.Join(root, "sessions", "rollout-2026-10-01T12-00-00-"+wanted+".jsonl")}
	if err := store.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	l, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := l.Close(); err != nil {
			t.Error(err)
		}
	}()
	budget := l.NativeReadBudget()
	cfg.Discovery.Enabled = false
	if err := l.PrepareRegistered(t.Context(), cfg, []archive.SessionRegistration{reg}, Options{Now: func() time.Time { return at.Add(2 * time.Minute) }}); err != nil {
		t.Fatal(err)
	}
	set, err := l.Thread(t.Context(), wanted)
	if err != nil || !set.Complete || len(set.Candidates) != 1 {
		t.Fatalf("qualified admitted source: %+v %v", set, err)
	}
	if l.NativeReadBudget() != budget {
		t.Fatal("replaced shared budget")
	}
	if _, found := l.coverage.Requests[other]; found {
		t.Fatal("unadmitted source requested")
	}
	regs, err := store.LoadRegistrations()
	if err != nil || len(regs) != 1 || regs[0].ArchiveSessionID != reg.ArchiveSessionID {
		t.Fatalf("inventory admitted a new owner: %+v %v", regs, err)
	}
	cfg.Harnesses = []string{"claude"}
	excluded := reg
	excluded.NativeSessionID = other
	sequence := l.coverage.Sequence
	if err := l.PrepareRegistered(t.Context(), cfg, []archive.SessionRegistration{excluded}, Options{}); err != nil {
		t.Fatal(err)
	}
	if sequence != l.coverage.Sequence {
		t.Fatal("excluded owner requested")
	}
	cfg.Harnesses = []string{"codex"}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	missing := reg
	missing.NativeSessionID = "00000000-0000-0000-0000-000000000003"
	if err := l.PrepareRegistered(ctx, cfg, []archive.SessionRegistration{missing}, Options{}); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if request := l.coverage.Requests[missing.NativeSessionID]; request.CompleteEpoch == l.coverage.Epoch {
		t.Fatal("cancellation invented complete coverage")
	}
}
