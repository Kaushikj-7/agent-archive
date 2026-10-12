package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

func blanketConfig(t *testing.T) (Config, time.Time) {
	t.Helper()
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	c := Config{RetentionDays: 90, MachineID: "test", Harnesses: []string{"codex"}, Archive: archive.Config{Enabled: true}, Discovery: &DiscoveryConfig{Enabled: true, CodexHomes: []string{"/approved"}}}
	setTestCodexScope(&c, CodexAllProjects)
	if e := ReconcileDiscovery(&c, Config{}, at); e != nil {
		t.Fatal(e)
	}
	return c, at
}

func TestCodexScopePauseSourceAndDestinationWindows(t *testing.T) {
	c, at := blanketConfig(t)
	root := "/new/project"
	start := at.Add(time.Minute)
	if _, ok := c.CodexDiscoveryGeneration(root, root, start, start); !ok {
		t.Fatal("new project rejected")
	}
	if err := transitionDiscoveryPause(&c, true, at.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	c.Paused = true
	if err := transitionDiscoveryPause(&c, false, at.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	c.Paused = false
	for _, tc := range []struct {
		at      time.Time
		allowed bool
	}{{start, true}, {at.Add(3 * time.Minute), false}, {at.Add(5 * time.Minute), true}} {
		_, ok := c.CodexGeneration(root, root, tc.at, at.Add(time.Hour))
		if ok != tc.allowed {
			t.Fatalf("start %s allowed=%t", tc.at, ok)
		}
	}
	previous := c
	d := *c.Discovery
	d.Enabled = false
	c.Discovery = &d
	if e := ReconcileDiscovery(&c, previous, at.Add(6*time.Minute)); e != nil {
		t.Fatal(e)
	}
	if _, ok := c.CodexGeneration(root, root, start, at.Add(time.Hour)); !ok {
		t.Fatal("discovery off removed hook permission")
	}
	previous = c
	d = *c.Discovery
	d.Enabled = true
	c.Discovery = &d
	if e := ReconcileDiscovery(&c, previous, at.Add(8*time.Minute)); e != nil {
		t.Fatal(e)
	}
	if _, ok := c.CodexDiscoveryGeneration(root, root, start, at.Add(time.Hour)); ok {
		t.Fatal("source re-enable swept history")
	}
	previous = c
	c.Storage.Bucket = "other"
	if e := ReconcileDiscovery(&c, previous, at.Add(10*time.Minute)); e != nil {
		t.Fatal(e)
	}
	if _, ok := c.CodexGeneration(root, root, start, at.Add(time.Hour)); ok {
		t.Fatal("destination changed but old start admitted")
	}
}

func TestCodexNearestRulesAndForwardSubtreeBarriers(t *testing.T) {
	c, at := blanketConfig(t)
	start := at.Add(time.Minute)
	token, _ := c.CodexGeneration("/other", "/other", start, start)
	previous := c
	c.Archive.Projects = []archive.ProjectActivation{{Root: "/excluded", Included: false}, {Root: "/excluded/child", Included: true, ActivatedAt: at}}
	if e := ReconcileDiscovery(&c, previous, at.Add(2*time.Minute)); e != nil {
		t.Fatal(e)
	}
	if _, ok := c.CodexGeneration("/excluded/repo", "/excluded/repo", start, start); ok {
		t.Fatal("blanket bypassed exclusion")
	}
	if _, ok := c.CodexGeneration("/excluded/child", "/excluded/child", start, start); !ok {
		t.Fatal("nearest child inclusion rejected")
	}
	after, _ := c.CodexGeneration("/other", "/other", start, start)
	if token != after {
		t.Fatal("unrelated edit invalidated delayed admission")
	}
	previous = c
	c.Archive.Projects = nil
	if e := ReconcileDiscovery(&c, previous, at.Add(3*time.Minute)); e != nil {
		t.Fatal(e)
	}
	if _, ok := c.CodexGeneration("/excluded/repo", "/excluded/repo", start, at.Add(time.Hour)); ok {
		t.Fatal("lifting exclusion admitted excluded-period start")
	}
	if _, ok := c.CodexGeneration("/excluded/repo", "/excluded/repo", at.Add(4*time.Minute), at.Add(time.Hour)); !ok {
		t.Fatal("forward reinclude rejected")
	}
}

func TestCodexFenceSurvivesDisableSkillEditsAndRollback(t *testing.T) {
	c, at := blanketConfig(t)
	home := t.TempDir()
	if e := Save(home, c); e != nil {
		t.Fatal(e)
	}
	previous := c
	setTestCodexScope(&c, CodexIncludedProjects)
	c.SkillEvidence = SkillEvidenceNone
	if e := ReconcileDiscovery(&c, previous, at.Add(time.Minute)); e != nil {
		t.Fatal(e)
	}
	if e := Save(home, c); e != nil {
		t.Fatal(e)
	}
	legacy := Config{RetentionDays: 90, MachineID: "test", Archive: archive.Config{Enabled: true}}
	if e := Save(home, legacy); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(filepath.Join(home, "config.json"))
	if e != nil {
		t.Fatal(e)
	}
	var old struct {
		SchemaVersion int `json:"schema_version"`
	}
	if json.Unmarshal(b, &old) == nil {
		t.Fatal("integer writer accepted fence")
	}
	var wire struct {
		Version json.RawMessage `json:"schema_version"`
	}
	if e := json.Unmarshal(b, &wire); e != nil {
		t.Fatal(e)
	}
	var fence writerVersion
	_ = json.Unmarshal(wire.Version, &fence)
	if fence.Version != 3 || fence.Writer != codexWriter {
		t.Fatal(string(b))
	}
	got, _, e := Load(home)
	if e != nil || got.EffectiveCodexCaptureScope() != CodexIncludedProjects || got.CodexCapture.Authorization != nil {
		t.Fatalf("rollback widened scope: %#v %v", got, e)
	}
}

func TestCodexProofDoesNotExpandOtherAgentsOrImports(t *testing.T) {
	c, at := blanketConfig(t)
	r := archive.SessionRegistration{Harness: archive.Harness{Name: "codex"}, ProjectRoot: "/new", ProjectID: archive.ProjectID("/new"), AdmittedAt: at, DestinationID: c.DestinationID(), Origin: archive.SessionOriginDiscovery, CodexAdmission: &archive.CodexAdmissionProof{Generation: "committed", Revision: "revision", Cwd: "/new"}}
	if !c.AcceptSession(r) {
		t.Fatal("blanket proof not consumed")
	}
	r.Harness.Name = "claude"
	if c.AcceptSession(r) {
		t.Fatal("cross-agent expansion")
	}
	r.Harness.Name = "codex"
	r.CodexAdmission = nil
	if c.AcceptSession(r) {
		t.Fatal("legacy registration gained proof")
	}
}

func TestCodexWorktreeCheckoutAndMainRulesHaveExplicitPrecedence(t *testing.T) {
	c, at := blanketConfig(t)
	main, checkout := "/repositories/main", "/worktrees/check"
	for _, tc := range []struct {
		rules   []archive.ProjectActivation
		allowed bool
	}{
		{[]archive.ProjectActivation{{Root: main, Included: false}}, false},
		{[]archive.ProjectActivation{{Root: main, Included: false}, {Root: checkout, Included: true, ActivatedAt: at}}, false},
		{[]archive.ProjectActivation{{Root: main, Included: true, ActivatedAt: at}, {Root: checkout, Included: false}}, false},
		{[]archive.ProjectActivation{{Root: "/worktrees", Included: false}, {Root: checkout, Included: true, ActivatedAt: at}}, true},
	} {
		c.Archive.Projects = tc.rules
		if c.CodexProjectAllowed(main, checkout, at.Add(time.Minute)) != tc.allowed {
			t.Fatalf("rules %#v", tc.rules)
		}
	}
}

func TestCodexReincludeBarrierPreservesDeliberateChildGrant(t *testing.T) {
	c, at := blanketConfig(t)
	old := c
	c.Archive.Projects = []archive.ProjectActivation{{Root: "/parent", Included: false}, {Root: "/parent/child", Included: true, ActivatedAt: at}}
	if e := ReconcileDiscovery(&c, old, at.Add(time.Minute)); e != nil {
		t.Fatal(e)
	}
	old = c
	c.Archive.Projects = []archive.ProjectActivation{{Root: "/parent/child", Included: true, ActivatedAt: at}}
	if e := ReconcileDiscovery(&c, old, at.Add(3*time.Minute)); e != nil {
		t.Fatal(e)
	}
	if _, ok := c.CodexGeneration("/parent/child", "/parent/child", at.Add(2*time.Minute), at.Add(time.Hour)); !ok {
		t.Fatal("lifting parent exclusion withdrew valid child start")
	}
	if _, ok := c.CodexGeneration("/parent/other", "/parent/other", at.Add(2*time.Minute), at.Add(time.Hour)); ok {
		t.Fatal("lifting parent exclusion admitted excluded sibling")
	}
}

func setTestCodexScope(c *Config, scope CodexCaptureScope) {
	p := CodexCaptureConfig{}
	if c.CodexCapture != nil {
		p = *c.CodexCapture
	}
	p.Scope = scope
	c.CodexCapture = &p
}

func TestCodexProtectedDraftNormalizationRetainsPolicyAndRejectsFutureWriter(t *testing.T) {
	t.Parallel()
	for _, scope := range []CodexCaptureScope{CodexAllProjects, CodexIncludedProjects} {
		cfg, _ := blanketConfig(t)
		cfg.CodexCapture.Scope = scope
		cfg.SkillEvidence = SkillEvidenceNone
		original := cfg
		data, err := json.Marshal(struct {
			Config Config `json:"config"`
		}{cfg})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.SkillEvidence != original.SkillEvidence || cfg.SchemaVersion != original.SchemaVersion {
			t.Fatal("draft normalization mutated caller")
		}
		var draft struct {
			Config Config `json:"config"`
		}
		if err := json.Unmarshal(data, &draft); err != nil {
			t.Fatal(err)
		}
		if draft.Config.SchemaVersion != 3 || draft.Config.EffectiveSkillEvidence() != SkillEvidenceNone || draft.Config.EffectiveCodexCaptureScope() != scope || draft.Config.CodexCapture.Authorization.Generation != cfg.CodexCapture.Authorization.Generation {
			t.Fatal("draft lost blanket fence or evidence")
		}
		cfg.SchemaVersion = 4
		if _, err := json.Marshal(cfg); err == nil {
			t.Fatal("future schema normalized downward")
		}
		if err := Save(t.TempDir(), cfg); err == nil {
			t.Fatal("future schema saved downward")
		}
	}
}

func TestCodexRetargetedExclusionKeepsPhysicalHistoryBarrier(t *testing.T) {
	for _, discovery := range []bool{false, true} {
		t.Run(map[bool]string{false: "hook-only", true: "discovery"}[discovery], func(t *testing.T) {
			c, at := blanketConfig(t)
			if !discovery {
				c.Discovery = nil
			}
			base := t.TempDir()
			original, replacement, alias := filepath.Join(base, "original"), filepath.Join(base, "replacement"), filepath.Join(base, "alias")
			for _, path := range []string{original, replacement} {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(original, alias); err != nil {
				t.Fatal(err)
			}
			previous := c
			c.Archive.Projects = []archive.ProjectActivation{{Root: alias, Included: false}}
			if err := ReconcileDiscovery(&c, previous, at.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			original, err := filepath.EvalSymlinks(original)
			if err != nil {
				t.Fatal(err)
			}
			generation := c.CodexGeneration
			if discovery {
				generation = c.CodexDiscoveryGeneration
			}
			if _, ok := generation(original, original, at.Add(2*time.Minute), at.Add(4*time.Minute)); ok {
				t.Fatal("original exclusion did not apply")
			}
			previous = c
			if err := os.Remove(alias); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(replacement, alias); err != nil {
				t.Fatal(err)
			}
			if err := ReconcileDiscovery(&c, previous, at.Add(3*time.Minute)); err != nil {
				t.Fatal(err)
			}
			generation = c.CodexGeneration
			if discovery {
				generation = c.CodexDiscoveryGeneration
			}
			if _, ok := generation(original, original, at.Add(2*time.Minute), at.Add(4*time.Minute)); ok {
				t.Fatal("retarget admitted original excluded-period start")
			}
			if _, ok := generation(original, original, at.Add(4*time.Minute), at.Add(4*time.Minute)); !ok {
				t.Fatal("forward original physical start did not reopen")
			}
			replacement, err = filepath.EvalSymlinks(replacement)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := generation(replacement, replacement, at.Add(4*time.Minute), at.Add(4*time.Minute)); ok {
				t.Fatal("retarget lifted replacement exclusion")
			}
		})
	}
}
