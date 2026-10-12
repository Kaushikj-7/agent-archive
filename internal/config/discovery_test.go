package config

import (
	"encoding/json"
	"github.com/wangjohn/agent-archive/internal/archive"
	_ "github.com/wangjohn/agent-archive/internal/testutil/golden"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type scopeChange string

const (
	scopeDisable     scopeChange = "disable"
	scopeReinclude   scopeChange = "reinclude"
	scopeDestination scopeChange = "destination"
	scopeExclusion   scopeChange = "exclusion"
	scopeHome        scopeChange = "home"
	scopeAgent       scopeChange = "agent"
)

func discoveryConfig(t *testing.T) (Config, time.Time) {
	t.Helper()
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	cfg := Config{RetentionDays: 90, Harnesses: []string{"codex"}, SkillEvidence: SkillEvidenceMetadata, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: "/included", Included: true, ActivatedAt: at}}}, Discovery: &DiscoveryConfig{Enabled: true, CodexHomes: []string{"/synthetic/codex"}}}
	if err := ReconcileDiscovery(&cfg, Config{}, at); err != nil {
		t.Fatal(err)
	}
	return cfg, at
}

func TestDiscoveryIntervalsExcludePauseAndKeepDelayedAuthorizedStarts(t *testing.T) {
	t.Parallel()
	cfg, at := discoveryConfig(t)
	home := t.TempDir()
	if err := Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	_, err := SetPaused(home, true, at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err = SetPaused(home, false, at.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		start   time.Time
		allowed bool
	}{{at.Add(-time.Nanosecond), false}, {at, true}, {at.Add(time.Hour - time.Nanosecond), true}, {at.Add(time.Hour), false}, {at.Add(90 * time.Minute), false}, {at.Add(2 * time.Hour), true}, {at.Add(4 * time.Hour), false}} {
		_, ok := cfg.DiscoveryGeneration("codex", "/included", tc.start, at.Add(3*time.Hour))
		if ok != tc.allowed {
			t.Errorf("start %v: %t", tc.start, ok)
		}
	}
}

func TestDiscoveryScopeChangesRotateForwardOnlyGenerations(t *testing.T) {
	t.Parallel()
	old, at := discoveryConfig(t)
	for _, mode := range []scopeChange{scopeDisable, scopeReinclude, scopeDestination, scopeExclusion, scopeHome, scopeAgent} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			next := old
			d := *old.Discovery
			next.Discovery = &d
			next.Archive.Projects = append([]archive.ProjectActivation(nil), old.Archive.Projects...)
			switch mode {
			case scopeDisable:
				next.Discovery.Enabled = false
			case scopeReinclude:
				next.Archive.Projects[0].Included = false
			case scopeDestination:
				next.Storage.Bucket = "new"
			case scopeExclusion:
				next.Archive.Projects = append(next.Archive.Projects, archive.ProjectActivation{Root: "/included/private", Included: false})
			case scopeHome:
				next.Discovery.CodexHomes = []string{"/other"}
			case scopeAgent:
				next.Harnesses = []string{"claude"}
			}
			if err := ReconcileDiscovery(&next, old, at.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			if mode == scopeDisable || mode == scopeReinclude || mode == scopeAgent {
				prior := next
				next = old
				next.Discovery = &d
				next.Discovery.Enabled = true
				if err := ReconcileDiscovery(&next, prior, at.Add(2*time.Hour)); err != nil {
					t.Fatal(err)
				}
			}
			if _, allowed := next.DiscoveryGeneration("codex", "/included", at.Add(time.Minute), at.Add(3*time.Hour)); allowed {
				t.Fatal("old start authorized after scope change")
			}
			if len(next.Discovery.Authorizations) > 0 && next.Discovery.Authorizations[0].Generation == old.Discovery.Authorizations[0].Generation {
				t.Fatal("generation reused")
			}
		})
	}
}

func TestDiscoveryWriterMarkerPreservesPolicyAndRejectsOldWriters(t *testing.T) {
	t.Parallel()
	cfg, at := discoveryConfig(t)
	home := t.TempDir()
	for _, policy := range []SkillEvidence{SkillEvidenceNone, SkillEvidenceMetadata, SkillEvidenceBody} {
		cfg.SkillEvidence = policy
		if err := Save(home, cfg); err != nil {
			t.Fatal(err)
		}
		loaded, _, err := Load(home)
		if err != nil || loaded.EffectiveSkillEvidence() != policy {
			t.Fatalf("policy %q: %v", policy, err)
		}
		// This is the base binary's exact supported-enum check, before any write.
		if ValidSkillEvidence(loaded.SkillEvidence) {
			t.Fatal("old writer would accept and erase authorization")
		}
		loaded.Discovery.Enabled = false
		if err := ReconcileDiscovery(&loaded, cfg, at.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := Save(home, loaded); err != nil {
			t.Fatal(err)
		}
		loaded, _, err = Load(home)
		if err != nil || !strings.HasSuffix(string(loaded.SkillEvidence), discoveryWriterMarker) {
			t.Fatal("disable erased rollback guard")
		}
	}
	raw, _ := os.ReadFile(filepath.Join(home, "config.json"))
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	delete(document, "discovery")
	raw, _ = json.Marshal(document)
	if err := os.WriteFile(filepath.Join(home, "config.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(home); err == nil {
		t.Fatal("marker without authorization accepted")
	}
}

func TestDiscoveryClockReversalCannotPartiallyResume(t *testing.T) {
	t.Parallel()
	cfg, at := discoveryConfig(t)
	home := t.TempDir()
	if err := Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := SetPaused(home, true, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := SetPaused(home, false, at.Add(time.Minute)); err == nil {
		t.Fatal("backwards clock resumed")
	}
	cfg, _, err := Load(home)
	if err != nil || !cfg.Paused {
		t.Fatal("failed resume changed committed permission")
	}
}

func TestUnrelatedProjectChangePreservesDiscoveryGeneration(t *testing.T) {
	previous := Config{Harnesses: []string{"codex"}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: "/one", ProjectID: "one", Included: true}, {Root: "/two", ProjectID: "two", Included: true}}}, Discovery: &DiscoveryConfig{Enabled: true, CodexHomes: []string{"/codex"}}}
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if err := ReconcileDiscovery(&previous, Config{}, at); err != nil {
		t.Fatal(err)
	}
	next := previous
	next.Archive.Projects = append([]archive.ProjectActivation(nil), previous.Archive.Projects...)
	next.Archive.Projects[1].Included = false
	if err := ReconcileDiscovery(&next, previous, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(next.Discovery.Authorizations) != 1 || next.Discovery.Authorizations[0].Generation != previous.Discovery.Authorizations[0].Generation {
		t.Fatal("unrelated scope revoked delayed eligible starts")
	}
	before := next
	next.Archive.Projects = append(append([]archive.ProjectActivation(nil), next.Archive.Projects...), archive.ProjectActivation{Root: "/one/excluded", Included: false})
	if err := ReconcileDiscovery(&next, before, at.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if next.Discovery.Authorizations[0].Generation == before.Discovery.Authorizations[0].Generation {
		t.Fatal("nested rule change kept broader old scope")
	}
}

func TestDiscoveryPauseRejectsLostConsentFloor(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"before", "equal", "prior_history", "multiple_scopes"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			cfg, at := discoveryConfig(t)
			pauseAt := at.Add(-time.Hour)
			if mode == "equal" {
				pauseAt = at
			}
			if mode == "prior_history" {
				cfg.Discovery.Authorizations[0].NativeStartFloor = at.Add(-3 * time.Hour)
				cfg.Discovery.Authorizations[0].Intervals = []DiscoveryInterval{{Start: at.Add(-3 * time.Hour), End: at.Add(-2 * time.Hour)}, {Start: at}}
			}
			if mode == "multiple_scopes" {
				earlier := cfg.Discovery.Authorizations[0]
				earlier.ProjectRoot = "/earlier"
				earlier.Generation = "earlier-generation"
				earlier.NativeStartFloor = at.Add(-2 * time.Hour)
				earlier.Intervals = []DiscoveryInterval{{Start: at.Add(-2 * time.Hour)}}
				cfg.Discovery.Authorizations = append([]DiscoveryAuthorization{earlier}, cfg.Discovery.Authorizations...)
			}
			home := t.TempDir()
			if err := Save(home, cfg); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(home, "config.json")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := SetPaused(home, true, pauseAt); err == nil {
				// On the defective implementation this grants a native start before
				// the active consent interval, including within a prior pause gap.
				resumed, resumeErr := SetPaused(home, false, at.Add(-30*time.Minute))
				_, allowed := resumed.DiscoveryGeneration("codex", "/included", at.Add(-15*time.Minute), at.Add(time.Hour))
				t.Fatalf("clock-inconsistent pause accepted: resume error=%v, old native start authorized=%t", resumeErr, allowed)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(before) {
				t.Fatalf("rejected pause changed committed configuration: %v", err)
			}
			loaded, found, err := Load(home)
			if err != nil || !found || loaded.Paused || !strings.HasSuffix(string(loaded.SkillEvidence), discoveryWriterMarker) {
				t.Fatalf("rejected pause changed protected state: %+v %v", loaded, err)
			}
			if _, allowed := loaded.DiscoveryGeneration("codex", "/included", at.Add(-15*time.Minute), at.Add(time.Hour)); allowed {
				t.Fatal("rejected pause widened permission")
			}
			if err := ReconcileDiscovery(&loaded, loaded, at.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			if _, allowed := loaded.DiscoveryGeneration("codex", "/included", at.Add(-15*time.Minute), at.Add(time.Hour)); allowed {
				t.Fatal("reconciliation widened retained permission")
			}
		})
	}
}

func TestDiscoveryClockReversalDoesNotMutateSharedScopes(t *testing.T) {
	t.Parallel()
	for _, paused := range []bool{true, false} {
		t.Run(map[bool]string{true: "pause", false: "resume"}[paused], func(t *testing.T) {
			t.Parallel()
			cfg, at := discoveryConfig(t)
			earlier := cfg.Discovery.Authorizations[0]
			earlier.ProjectRoot = "/earlier"
			earlier.Generation = "earlier-generation"
			earlier.NativeStartFloor = at.Add(-2 * time.Hour)
			earlier.Intervals = []DiscoveryInterval{{Start: at.Add(-2 * time.Hour)}}
			cfg.Discovery.Authorizations = append([]DiscoveryAuthorization{earlier}, cfg.Discovery.Authorizations...)
			cfg.Paused = !paused
			if !paused {
				cfg.Discovery.Authorizations[0].Intervals[0].End = at.Add(-time.Hour)
				cfg.Discovery.Authorizations[1].Intervals[0].End = at.Add(time.Hour)
			}
			shared := cfg
			before, err := json.Marshal(shared)
			if err != nil {
				t.Fatal(err)
			}
			if err := transitionDiscoveryPause(&cfg, paused, at.Add(-30*time.Minute)); err == nil {
				t.Fatal("clock reversal accepted")
			}
			after, err := json.Marshal(shared)
			if err != nil || string(after) != string(before) {
				t.Fatalf("clock reversal partially mutated shared authorizations: %v", err)
			}
		})
	}
}
