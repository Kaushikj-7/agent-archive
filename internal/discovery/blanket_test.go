package discovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/agents/codex"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestBlanketUnknownPhysicalProjectsPublishAndReadBackWithoutConfigGrowth(t *testing.T) {
	for _, ingress := range []string{"discovery", "hook"} {
		t.Run(ingress, func(t *testing.T) {
			store, cfg, at, codex := fixture(t)
			parent := t.TempDir()
			cfg.Archive.Projects = append(cfg.Archive.Projects, archive.ProjectActivation{Root: parent, ProjectID: archive.ProjectID(parent), Included: true, ActivatedAt: at})
			setTestCodexScope(&cfg, config.CodexAllProjects)
			if e := config.ReconcileDiscovery(&cfg, config.Config{}, at); e != nil {
				t.Fatal(e)
			}
			// Establish the publication capabilities before measuring permission growth.
			cfg.CodexHistoryProtection = true
			cfg.DurableStorageProtection = true
			cfg.PublicationCompositionProtection = true
			if e := config.Save(store.Home(), cfg); e != nil {
				t.Fatal(e)
			}
			if e := config.WithPublicationComposition(store.Home(), func(config.PublicationCompositionGuard) error { return nil }); e != nil {
				t.Fatal(e)
			}
			beforeCfg, _, beforeErr := config.Load(store.Home())
			if beforeErr != nil {
				t.Fatal(beforeErr)
			}
			cfg = beforeCfg
			before, _ := json.Marshal(beforeCfg)
			want := map[string]bool{}
			for i := range 4 {
				project := filepath.Join(parent, fmt.Sprintf("project-%d", i))
				if e := os.MkdirAll(project, 0700); e != nil {
					t.Fatal(e)
				}
				if i < 2 {
					if e := os.Mkdir(filepath.Join(project, ".git"), 0700); e != nil {
						t.Fatal(e)
					}
				}
				canonical, e := filepath.EvalSymlinks(project)
				if e != nil {
					t.Fatal(e)
				}
				want[canonical] = true
				native := fmt.Sprintf("00000000-0000-0000-0000-%012d", i+1)
				if ingress == "hook" {
					path := filepath.Join(codex, "sessions", "rollout-2026-10-01T12-00-00-"+native+".jsonl")
					if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
						t.Fatal(e)
					}
					if e := os.WriteFile(path, nil, 0600); e != nil {
						t.Fatal(e)
					}
					if e := handleCodexHook(store.Home(), map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": native, "cwd": project, "transcript_path": path}, at.Add(time.Minute)); e != nil {
						t.Fatal(e)
					}
				}
				writeRollout(t, codex, project, at.Add(time.Minute), i+1, "sessions")
			}
			if ingress == "discovery" {
				h, e := run(context.Background(), store, cfg, Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return at.Add(2 * time.Minute) }}, syntheticSupport)
				if e != nil || h.Registered != 4 {
					t.Fatalf("scan %#v %v", h, e)
				}
			}
			regs, e := store.LoadRegistrations()
			if e != nil || len(regs) != 4 {
				t.Fatalf("registrations %#v %v", regs, e)
			}
			for _, r := range regs {
				if !want[r.ProjectRoot] || r.ProjectID != archive.ProjectID(r.ProjectRoot) || r.CodexAdmission == nil || !cfg.AcceptSession(r) {
					t.Fatalf("physical identity/proof %#v", r)
				}
				delete(want, r.ProjectRoot)
			}
			objects := storagetest.NewMemoryStore()
			result, e := collector.Run(context.Background(), store, objects, collector.Options{Parsers: builtin.NewBuiltins(), Sources: builtin.NewBuiltins(), MachineID: cfg.MachineID, AcceptSession: cfg.AcceptSession, Now: func() time.Time { return at.Add(3 * time.Minute) }})
			if e != nil || len(result.Published) != 4 || len(result.Errors) != 0 {
				for _, issue := range result.Errors {
					var native *agentapi.SourceError
					if errors.As(issue, &native) {
						t.Logf("native cause: %v", native.Err)
					}
				}
				t.Fatalf("publication %#v %v errors=%v", result, e, result.Errors)
			}
			for _, r := range regs {
				b, e := store.PublishedMetadata(r.ArchiveSessionID)
				if e != nil {
					t.Fatal(e)
				}
				var meta archive.Metadata
				if e = json.Unmarshal(b, &meta); e != nil {
					t.Fatal(e)
				}
				assertBlanketNativeAnalysis(t, meta)
				source, e := objects.Get(context.Background(), meta.SourceBundle.Key)
				if e != nil || len(source) == 0 {
					t.Fatalf("readback %v", e)
				}
				validatePublishedDiscovery(t, b, source)
			}
			afterCfg, _, afterErr := config.Load(store.Home())
			if afterErr != nil {
				t.Fatal(afterErr)
			}
			after, _ := json.Marshal(afterCfg)
			if !bytes.Equal(before, after) {
				t.Fatalf("discovered projects wrote permissions: before=%s after=%s", before, after)
			}
		})
	}
}

func TestBlanketDiscoveryRespectsSourceReenableAndLegacyIdentity(t *testing.T) {
	store, cfg, at, codex := fixture(t)
	owner := cfg.Archive.Projects[0].Root
	child := filepath.Join(owner, "nested-repo")
	if e := os.MkdirAll(filepath.Join(child, ".git"), 0700); e != nil {
		t.Fatal(e)
	}
	native := writeRollout(t, codex, child, at.Add(time.Minute), 1, "sessions")
	h, e := run(context.Background(), store, cfg, Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return at.Add(2 * time.Minute) }}, syntheticSupport)
	if e != nil || h.Registered != 1 {
		t.Fatalf("selected admission %#v %v", h, e)
	}
	regs, _ := store.LoadRegistrations()
	before := regs[0]
	old := cfg
	setTestCodexScope(&cfg, config.CodexAllProjects)
	if e := config.ReconcileDiscovery(&cfg, old, at.Add(3*time.Minute)); e != nil {
		t.Fatal(e)
	}
	if e := config.Save(store.Home(), cfg); e != nil {
		t.Fatal(e)
	}
	// Change the file to force observation under the new physical resolver.
	writeRollout(t, codex, child, at.Add(time.Minute), 1, "sessions")
	_, e = run(context.Background(), store, cfg, Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return at.Add(5 * time.Minute) }}, syntheticSupport)
	if e != nil {
		t.Fatal(e)
	}
	after, _, e := store.LoadRegistration(before.ArchiveSessionID)
	if e != nil || after.NativeSessionID != native || after.ProjectRoot != owner || after.CodexAdmission != nil || after.DiscoveryGeneration != before.DiscoveryGeneration {
		t.Fatalf("legacy identity migrated %#v %v", after, e)
	}
	old = cfg
	d := *cfg.Discovery
	d.Enabled = false
	cfg.Discovery = &d
	_ = config.ReconcileDiscovery(&cfg, old, at.Add(6*time.Minute))
	_ = config.Save(store.Home(), cfg)
	writeRollout(t, codex, child, at.Add(7*time.Minute), 2, "sessions")
	old = cfg
	d = *cfg.Discovery
	d.Enabled = true
	cfg.Discovery = &d
	_ = config.ReconcileDiscovery(&cfg, old, at.Add(8*time.Minute))
	_ = config.Save(store.Home(), cfg)
	h, e = run(context.Background(), store, cfg, Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return at.Add(9 * time.Minute) }}, syntheticSupport)
	if e != nil || h.Registered != 0 {
		t.Fatalf("source off-period swept %#v %v", h, e)
	}
}

func TestBlanketExistingProofResumesAfterExclusionLiftAndScopeReduction(t *testing.T) {
	store, cfg, at, codex := fixture(t)
	parent := t.TempDir()
	project := filepath.Join(parent, "repository")
	if e := os.MkdirAll(filepath.Join(project, ".git"), 0700); e != nil {
		t.Fatal(e)
	}
	setTestCodexScope(&cfg, config.CodexAllProjects)
	if e := config.ReconcileDiscovery(&cfg, config.Config{}, at); e != nil {
		t.Fatal(e)
	}
	if e := config.Save(store.Home(), cfg); e != nil {
		t.Fatal(e)
	}
	native := writeRollout(t, codex, project, at.Add(time.Minute), 1, "sessions")
	h, e := run(context.Background(), store, cfg, Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return at.Add(2 * time.Minute) }}, syntheticSupport)
	if e != nil || h.Registered != 1 {
		t.Fatalf("admission %#v %v", h, e)
	}
	regs, _ := store.LoadRegistrations()
	original := regs[0]
	objects := storagetest.NewMemoryStore()
	old := cfg
	cfg.Archive.Projects = []archive.ProjectActivation{{Root: project, Included: false}}
	_ = config.ReconcileDiscovery(&cfg, old, at.Add(3*time.Minute))
	_ = config.Save(store.Home(), cfg)
	writeRollout(t, codex, project, at.Add(4*time.Minute), 2, "sessions")
	result, e := collector.Run(context.Background(), store, objects, collector.Options{Parsers: builtin.NewBuiltins(), Sources: builtin.NewBuiltins(), MachineID: cfg.MachineID, AcceptSession: cfg.AcceptSession, Now: func() time.Time { return at.Add(5 * time.Minute) }})
	if e != nil || len(result.Published) != 0 {
		t.Fatalf("excluded publication %#v %v", result, e)
	}
	old = cfg
	cfg.Archive.Projects = nil
	_ = config.ReconcileDiscovery(&cfg, old, at.Add(6*time.Minute))
	_ = config.Save(store.Home(), cfg)
	h, e = run(context.Background(), store, cfg, Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return at.Add(7 * time.Minute) }}, syntheticSupport)
	if e != nil || h.Registered != 0 {
		t.Fatalf("lift admission %#v %v", h, e)
	}
	result, e = collector.Run(context.Background(), store, objects, collector.Options{Parsers: builtin.NewBuiltins(), Sources: builtin.NewBuiltins(), MachineID: cfg.MachineID, AcceptSession: cfg.AcceptSession, Now: func() time.Time { return at.Add(7 * time.Minute) }})
	if e != nil || len(result.Published) != 1 {
		t.Fatalf("existing proof failed to resume %#v %v", result, e)
	}
	old = cfg
	setTestCodexScope(&cfg, config.CodexIncludedProjects)
	cfg.Archive.Projects = []archive.ProjectActivation{{Root: parent, Included: true, ActivatedAt: at.Add(8 * time.Minute)}}
	_ = config.ReconcileDiscovery(&cfg, old, at.Add(8*time.Minute))
	_ = config.Save(store.Home(), cfg)
	if !cfg.AcceptSession(original) {
		t.Fatal("remaining parent scope rejected existing physical identity")
	}
	path := original.TranscriptPath
	f, e := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal(e)
	}
	_, e = f.WriteString("{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"SYNTHETIC_CONTINUATION\"}]}}\n")
	_ = f.Close()
	if e != nil {
		t.Fatal(e)
	}
	h, e = run(context.Background(), store, cfg, Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return at.Add(10 * time.Minute) }}, syntheticSupport)
	if e != nil || h.Registered != 0 {
		t.Fatalf("selected continuation %#v %v", h, e)
	}
	if e = handleCodexHook(store.Home(), map[string]any{"hook_event_name": "SessionStart", "source": "resume", "session_id": native, "cwd": project, "transcript_path": path}, at.Add(11*time.Minute)); e != nil {
		t.Fatal(e)
	}
	after, _, _ := store.LoadRegistration(original.ArchiveSessionID)
	if after.ProjectRoot != original.ProjectRoot || after.CodexAdmission.Generation != original.CodexAdmission.Generation || !after.AdmittedAt.Equal(original.AdmittedAt) {
		t.Fatalf("scope reduction rewrote ownership %#v", after)
	}
	result, e = collector.Run(context.Background(), store, objects, collector.Options{Parsers: builtin.NewBuiltins(), Sources: builtin.NewBuiltins(), MachineID: cfg.MachineID, AcceptSession: cfg.AcceptSession, Now: func() time.Time { return at.Add(12 * time.Minute) }})
	if e != nil || len(result.Published) != 1 {
		t.Fatalf("selected publication %#v %v", result, e)
	}
	b, e := store.PublishedMetadata(original.ArchiveSessionID)
	if e != nil {
		t.Fatal(e)
	}
	var meta archive.Metadata
	_ = json.Unmarshal(b, &meta)
	assertBlanketNativeAnalysis(t, meta)
	if source, e := objects.Get(context.Background(), meta.SourceBundle.Key); e != nil || len(source) == 0 {
		t.Fatalf("readback %v", e)
	}
	regs, _ = store.LoadRegistrations()
	if len(regs) != 1 {
		t.Fatal("excluded-period unknown start swept on lift")
	}
}

func setTestCodexScope(c *config.Config, scope config.CodexCaptureScope) {
	p := config.CodexCaptureConfig{}
	if c.CodexCapture != nil {
		p = *c.CodexCapture
	}
	p.Scope = scope
	c.CodexCapture = &p
}

func TestBlanketLegacyOwnerPublicationHonorsStoredCwdExceptions(t *testing.T) {
	for _, childGrant := range []bool{false, true} {
		t.Run(fmt.Sprintf("child-grant-%t", childGrant), func(t *testing.T) {
			store, cfg, at, codex := fixture(t)
			owner := cfg.Archive.Projects[0].Root
			private := filepath.Join(owner, "private")
			cwd := filepath.Join(private, "allowed")
			if err := os.MkdirAll(filepath.Join(cwd, ".git"), 0700); err != nil {
				t.Fatal(err)
			}
			writeRollout(t, codex, cwd, at.Add(time.Minute), 1, "sessions")
			h, err := run(context.Background(), store, cfg, Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return at.Add(2 * time.Minute) }}, syntheticSupport)
			if err != nil || h.Registered != 1 {
				t.Fatalf("legacy admission %#v %v", h, err)
			}
			regs, err := store.LoadRegistrations()
			if err != nil || len(regs) != 1 || regs[0].ProjectRoot != owner || regs[0].DiscoveryCwd != cwd || regs[0].CodexAdmission != nil {
				t.Fatalf("legacy owner/cwd %#v %v", regs, err)
			}
			original := regs[0]
			objects := storagetest.NewMemoryStore()
			collect := func(now time.Time) collector.Result {
				t.Helper()
				result, err := collector.Run(context.Background(), store, objects, collector.Options{Parsers: builtin.NewBuiltins(), Sources: builtin.NewBuiltins(), MachineID: cfg.MachineID, AcceptSession: cfg.AcceptSession, Now: func() time.Time { return now }})
				if err != nil || len(result.Errors) != 0 {
					t.Fatalf("publication %#v %v errors=%v", result, err, result.Errors)
				}
				return result
			}
			if result := collect(at.Add(3 * time.Minute)); len(result.Published) != 1 {
				t.Fatalf("selected-mode positive publication %#v", result)
			}
			old := cfg
			setTestCodexScope(&cfg, config.CodexAllProjects)
			cfg.Archive.Projects = append(cfg.Archive.Projects, archive.ProjectActivation{Root: private, Included: false})
			if childGrant {
				cfg.Archive.Projects = append(cfg.Archive.Projects, archive.ProjectActivation{Root: cwd, Included: true, ActivatedAt: at.Add(4 * time.Minute)})
			}
			if err := config.ReconcileDiscovery(&cfg, old, at.Add(4*time.Minute)); err != nil {
				t.Fatal(err)
			}
			if err := config.Save(store.Home(), cfg); err != nil {
				t.Fatal(err)
			}
			f, err := os.OpenFile(original.TranscriptPath, os.O_WRONLY|os.O_APPEND, 0600)
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.WriteString("{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"SYNTHETIC_LEGACY_CONTINUATION\"}]}}\n")
			_ = f.Close()
			if err != nil {
				t.Fatal(err)
			}
			// A changed transcript first enters the collector's stable-source
			// wait. Advance that actual scheduled stage before judging permission.
			collect(at.Add(5 * time.Minute))
			result := collect(at.Add(7 * time.Minute))
			want := 0
			if childGrant {
				want = 1
			}
			if len(result.Published) != want {
				t.Fatalf("current stored-cwd exception publication %#v, want %d", result, want)
			}
			after, found, err := store.LoadRegistration(original.ArchiveSessionID)
			if err != nil || !found || after.ProjectRoot != original.ProjectRoot || after.CodexAdmission != nil || after.DiscoveryCwd != original.DiscoveryCwd || !after.AdmittedAt.Equal(original.AdmittedAt) {
				t.Fatalf("legacy admission/owner mutated %#v %v", after, err)
			}
			if childGrant {
				raw, err := store.PublishedMetadata(original.ArchiveSessionID)
				if err != nil {
					t.Fatal(err)
				}
				var metadata archive.Metadata
				if err := json.Unmarshal(raw, &metadata); err != nil {
					t.Fatal(err)
				}
				source, err := objects.Get(context.Background(), metadata.SourceBundle.Key)
				if err != nil {
					t.Fatal(err)
				}
				bundle, err := archive.ReadSourceBundle(bytes.NewReader(source), archive.DecodeOptions{})
				if err != nil {
					t.Fatal(err)
				}
				decoded, err := json.Marshal(bundle)
				if err != nil || !bytes.Contains(decoded, []byte("SYNTHETIC_LEGACY_CONTINUATION")) {
					t.Fatalf("actual continuation readback missing: %v", err)
				}
				validatePublishedDiscovery(t, raw, source)
			}
		})
	}
}

func TestBlanketWorktreeMainExclusionSurvivesUnrelatedCheckoutInclusion(t *testing.T) {
	store, cfg, at, codex := fixture(t)
	base := t.TempDir()
	private := filepath.Join(base, "private")
	projects := filepath.Join(base, "projects")
	main := filepath.Join(private, "repository")
	checkout := filepath.Join(projects, "checkout")
	gd := filepath.Join(main, ".git", "worktrees", "one")
	allowed := filepath.Join(main, "allowed")
	for _, dir := range []string{gd, checkout, allowed} {
		if e := os.MkdirAll(dir, 0700); e != nil {
			t.Fatal(e)
		}
	}
	for path, body := range map[string]string{filepath.Join(checkout, ".git"): "gitdir: " + gd, filepath.Join(gd, "commondir"): "../..", filepath.Join(gd, "gitdir"): filepath.Join(checkout, ".git")} {
		if e := os.WriteFile(path, []byte(body), 0600); e != nil {
			t.Fatal(e)
		}
	}
	cfg.Archive.Projects = []archive.ProjectActivation{{Root: private, Included: false}, {Root: projects, Included: true, ActivatedAt: at}}
	setTestCodexScope(&cfg, config.CodexAllProjects)
	if e := config.ReconcileDiscovery(&cfg, config.Config{}, at); e != nil {
		t.Fatal(e)
	}
	if e := config.Save(store.Home(), cfg); e != nil {
		t.Fatal(e)
	}
	writeRollout(t, codex, checkout, at.Add(time.Minute), 1, "sessions")
	h, e := run(context.Background(), store, cfg, Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return at.Add(2 * time.Minute) }}, syntheticSupport)
	if e != nil || h.Registered != 0 {
		t.Fatalf("mapped main exclusion bypassed %#v %v", h, e)
	}
	if e := handleCodexHook(store.Home(), map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "excluded-hook", "cwd": checkout, "transcript_path": filepath.Join(checkout, "new.jsonl")}, at.Add(2*time.Minute)); e != nil {
		t.Fatal(e)
	}
	regs, _ := store.LoadRegistrations()
	if len(regs) != 0 {
		t.Fatal("hook bypassed mapped-main exclusion")
	}
	old := cfg
	cfg.Archive.Projects = append(cfg.Archive.Projects, archive.ProjectActivation{Root: allowed, Included: true, ActivatedAt: at.Add(2 * time.Minute)})
	if e := config.ReconcileDiscovery(&cfg, old, at.Add(2*time.Minute)); e != nil {
		t.Fatal(e)
	}
	if e := config.Save(store.Home(), cfg); e != nil {
		t.Fatal(e)
	}
	writeRollout(t, codex, allowed, at.Add(3*time.Minute), 3, "sessions")
	h, e = run(context.Background(), store, cfg, Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return at.Add(4 * time.Minute) }}, syntheticSupport)
	if e != nil || h.Registered != 1 {
		t.Fatalf("deliberate child inclusion rejected %#v %v", h, e)
	}
	regs, _ = store.LoadRegistrations()
	canonical, _ := filepath.EvalSymlinks(main)
	if len(regs) != 1 || regs[0].ProjectRoot != canonical || regs[0].CodexAdmission == nil {
		t.Fatalf("physical child identity %#v", regs)
	}
	objects := storagetest.NewMemoryStore()
	result, e := collector.Run(context.Background(), store, objects, collector.Options{Parsers: builtin.NewBuiltins(), Sources: builtin.NewBuiltins(), MachineID: cfg.MachineID, AcceptSession: cfg.AcceptSession, Now: func() time.Time { return at.Add(5 * time.Minute) }})
	if e != nil || len(result.Published) != 1 {
		t.Fatalf("permitted child publication %#v %v", result, e)
	}
	b, e := store.PublishedMetadata(regs[0].ArchiveSessionID)
	if e != nil {
		t.Fatal(e)
	}
	var meta archive.Metadata
	_ = json.Unmarshal(b, &meta)
	assertBlanketNativeAnalysis(t, meta)
	if source, e := objects.Get(context.Background(), meta.SourceBundle.Key); e != nil || len(source) == 0 {
		t.Fatalf("readback %v", e)
	}
}

func TestBlanketHookLocatorDiscoveryRejectsExcludedCwdAndDifferentPhysicalProject(t *testing.T) {
	for _, tc := range []struct {
		name             string
		nestedRepository bool
		excluded         bool
		allowed          bool
	}{{name: "excluded-after-reduction", excluded: true}, {name: "nested-repository", nestedRepository: true}, {name: "allowed-after-reduction", allowed: true}} {
		t.Run(tc.name, func(t *testing.T) {
			store, cfg, at, codex := fixture(t)
			project := cfg.Archive.Projects[0].Root
			child := filepath.Join(project, "child")
			for _, dir := range []string{filepath.Join(project, ".git"), child} {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			setTestCodexScope(&cfg, config.CodexAllProjects)
			if err := config.ReconcileDiscovery(&cfg, config.Config{}, at); err != nil {
				t.Fatal(err)
			}
			if err := config.Save(store.Home(), cfg); err != nil {
				t.Fatal(err)
			}
			native := "00000000-0000-0000-0000-000000000001"
			if err := handleCodexHook(store.Home(), map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": native, "cwd": project}, at.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			regs, err := store.LoadRegistrations()
			if err != nil || len(regs) != 1 || regs[0].CodexAdmission == nil || regs[0].TranscriptPath != "" {
				t.Fatalf("initial hook proof %#v %v", regs, err)
			}
			before := regs[0]
			old := cfg
			if tc.nestedRepository {
				if err := os.Mkdir(filepath.Join(child, ".git"), 0700); err != nil {
					t.Fatal(err)
				}
			} else {
				setTestCodexScope(&cfg, config.CodexIncludedProjects)
				if tc.excluded {
					cfg.Archive.Projects = append(cfg.Archive.Projects, archive.ProjectActivation{Root: child, Included: false})
				}
			}
			if err := config.ReconcileDiscovery(&cfg, old, at.Add(2*time.Minute)); err != nil {
				t.Fatal(err)
			}
			if err := config.Save(store.Home(), cfg); err != nil {
				t.Fatal(err)
			}
			writeRollout(t, codex, child, at.Add(3*time.Minute), 1, "sessions")
			// A discovery observation cannot establish the source authority of a
			// pathless hook owner. The permitted control obtains its locator from
			// an actual hook before discovery evaluates the same continuation.
			if tc.allowed {
				path := filepath.Join(codex, "sessions", "rollout-2026-10-01T12-00-00-"+native+".jsonl")
				if err := handleCodexHook(store.Home(), map[string]any{"hook_event_name": "SessionStart", "source": "resume", "session_id": native, "cwd": child, "transcript_path": path}, at.Add(3*time.Minute)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := run(context.Background(), store, cfg, Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return at.Add(4 * time.Minute) }}, syntheticSupport); err != nil {
				t.Fatal(err)
			}
			after, found, err := store.LoadRegistration(before.ArchiveSessionID)
			if err != nil || !found || after.ProjectRoot != before.ProjectRoot || *after.CodexAdmission != *before.CodexAdmission || !after.AdmittedAt.Equal(before.AdmittedAt) || after.Origin != before.Origin || after.DestinationID != before.DestinationID {
				t.Fatalf("continuation changed ownership %#v %v", after, err)
			}
			allowed := tc.allowed
			if (after.TranscriptPath != "") != allowed {
				t.Fatalf("locator grant for %s: %q", tc.name, after.TranscriptPath)
			}
			objects := storagetest.NewMemoryStore()
			result, err := collector.Run(context.Background(), store, objects, collector.Options{Parsers: builtin.NewBuiltins(), Sources: builtin.NewBuiltins(), MachineID: cfg.MachineID, AcceptSession: cfg.AcceptSession, Now: func() time.Time { return at.Add(5 * time.Minute) }})
			expected := 0
			if allowed {
				expected = 1
			}
			if err != nil || len(result.Published) != expected {
				t.Fatalf("publication %#v %v errors=%v", result, err, result.Errors)
			}
			regs, err = store.LoadRegistrations()
			if err != nil || len(regs) != 1 {
				t.Fatalf("duplicate identity %#v %v", regs, err)
			}
		})
	}
}

func TestBlanketLegacyContinuationResumesAfterExclusionLiftWithoutAdmittingExcludedHistory(t *testing.T) {
	for _, hook := range []bool{false, true} {
		t.Run(fmt.Sprintf("hook-%t", hook), func(t *testing.T) {
			store, cfg, at, codex := fixture(t)
			owner := cfg.Archive.Projects[0].Root
			cwd := filepath.Join(owner, "repository")
			if err := os.MkdirAll(filepath.Join(cwd, ".git"), 0700); err != nil {
				t.Fatal(err)
			}
			start := at.Add(time.Minute)
			native := writeRollout(t, codex, cwd, start, 1, "sessions")
			scanAt := func(now time.Time) {
				t.Helper()
				if _, err := run(context.Background(), store, cfg, Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return now }}, syntheticSupport); err != nil {
					t.Fatal(err)
				}
			}
			scanAt(at.Add(2 * time.Minute))
			regs, err := store.LoadRegistrations()
			if err != nil || len(regs) != 1 || regs[0].CodexAdmission != nil || regs[0].ProjectRoot != owner {
				t.Fatalf("selected admission %#v %v", regs, err)
			}
			before := regs[0]
			old := cfg
			setTestCodexScope(&cfg, config.CodexAllProjects)
			cfg.Archive.Projects = append(cfg.Archive.Projects, archive.ProjectActivation{Root: cwd, Included: false})
			if err := config.ReconcileDiscovery(&cfg, old, at.Add(3*time.Minute)); err != nil {
				t.Fatal(err)
			}
			if err := config.Save(store.Home(), cfg); err != nil {
				t.Fatal(err)
			}
			payload := map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": native, "cwd": cwd}
			if err := handleCodexHook(store.Home(), payload, at.Add(4*time.Minute)); err != nil {
				t.Fatal(err)
			}
			excluded, _, err := store.LoadRegistration(before.ArchiveSessionID)
			if err != nil || !excluded.HookObservedAt.Equal(before.HookObservedAt) || cfg.AcceptSession(excluded) {
				t.Fatalf("excluded continuation changed %#v %v", excluded, err)
			}
			writeRollout(t, codex, cwd, at.Add(4*time.Minute), 2, "sessions")
			old = cfg
			cfg.Archive.Projects = cfg.Archive.Projects[:1]
			if err := config.ReconcileDiscovery(&cfg, old, at.Add(5*time.Minute)); err != nil {
				t.Fatal(err)
			}
			if err := config.Save(store.Home(), cfg); err != nil {
				t.Fatal(err)
			}
			if hook {
				observed := at.Add(6 * time.Minute)
				if err := handleCodexHook(store.Home(), payload, observed); err != nil {
					t.Fatal(err)
				}
				after, _, err := store.LoadRegistration(before.ArchiveSessionID)
				if err != nil || !after.HookObservedAt.Equal(observed) {
					t.Fatalf("permitted legacy hook did not resume %#v %v", after, err)
				}
			} else {
				if err := os.Remove(before.TranscriptPath); err != nil {
					t.Fatal(err)
				}
				writeRollout(t, codex, cwd, start, 1, "archived_sessions")
			}
			scanAt(at.Add(7 * time.Minute))
			regs, err = store.LoadRegistrations()
			if err != nil || len(regs) != 1 {
				t.Fatalf("excluded-period unknown history admitted %#v %v", regs, err)
			}
			after := regs[0]
			if after.ArchiveSessionID != before.ArchiveSessionID || after.ProjectRoot != before.ProjectRoot || after.ProjectID != before.ProjectID || after.Origin != before.Origin || after.DestinationID != before.DestinationID || after.CodexAdmission != nil || !after.AdmittedAt.Equal(before.AdmittedAt) || !after.SessionStartedAt.Equal(before.SessionStartedAt) || after.DiscoveryGeneration != before.DiscoveryGeneration {
				t.Fatalf("legacy admission changed %#v", after)
			}
			if !hook && after.TranscriptPath == before.TranscriptPath {
				t.Fatal("permitted legacy discovery did not replace missing locator")
			}
			objects := storagetest.NewMemoryStore()
			result := collectNativeFixture(t, store, objects, codex, at.Add(8*time.Minute))
			if len(result.Errors) != 0 || len(result.Published) != 1 {
				t.Fatalf("resumed publication %#v %v", result, err)
			}
			raw, err := store.PublishedMetadata(before.ArchiveSessionID)
			if err != nil {
				t.Fatal(err)
			}
			var metadata archive.Metadata
			if err := json.Unmarshal(raw, &metadata); err != nil {
				t.Fatal(err)
			}
			source, err := objects.Get(context.Background(), metadata.SourceBundle.Key)
			if err != nil {
				t.Fatal(err)
			}
			validatePublishedDiscovery(t, raw, source)
		})
	}
}

func TestBlanketPausedConsentFloorFlowsThroughHookDiscoveryAndPublication(t *testing.T) {
	for _, ingress := range []string{"hook", "discovery"} {
		t.Run(ingress, func(t *testing.T) {
			store, cfg, at, codex := fixture(t)
			cfg.Paused = true
			setTestCodexScope(&cfg, config.CodexAllProjects)
			floor := at.Add(time.Hour)
			if err := config.ReconcileDiscovery(&cfg, config.Config{}, floor); err != nil {
				t.Fatal(err)
			}
			if err := config.Save(store.Home(), cfg); err != nil {
				t.Fatal(err)
			}
			if _, err := config.SetPaused(store.Home(), false, floor.Add(-time.Nanosecond)); err == nil {
				t.Fatal("pre-consent resume succeeded")
			}
			var err error
			cfg, err = config.SetPaused(store.Home(), false, floor.Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			project := t.TempDir()
			if err := os.Mkdir(filepath.Join(project, ".git"), 0700); err != nil {
				t.Fatal(err)
			}
			for i, started := range []time.Time{floor.Add(-time.Minute), floor.Add(30 * time.Second), floor.Add(2 * time.Minute)} {
				native := fmt.Sprintf("00000000-0000-0000-0000-%012d", i+1)
				if ingress == "hook" {
					path := filepath.Join(codex, "sessions", "rollout-2026-10-01T12-00-00-"+native+".jsonl")
					if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, nil, 0600); err != nil {
						t.Fatal(err)
					}
					if err := handleCodexHook(store.Home(), map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": native, "cwd": project, "transcript_path": path}, started); err != nil {
						t.Fatal(err)
					}
				}
				writeRollout(t, codex, project, started, i+1, "sessions")
			}
			if ingress == "discovery" {
				h, err := run(context.Background(), store, cfg, Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return floor.Add(3 * time.Minute) }}, syntheticSupport)
				if err != nil || h.Registered != 1 {
					t.Fatalf("floor discovery %#v %v", h, err)
				}
			}
			regs, err := store.LoadRegistrations()
			if err != nil || len(regs) != 1 {
				t.Fatalf("admitted paused/old starts: %#v %v", regs, err)
			}
			if !regs[0].SessionStartedAt.Equal(floor.Add(2*time.Minute)) || regs[0].CodexAdmission == nil {
				t.Fatal("lost original admission")
			}
			objects := storagetest.NewMemoryStore()
			result, err := collector.Run(context.Background(), store, objects, collector.Options{Parsers: builtin.NewBuiltins(), Sources: builtin.NewBuiltins(), MachineID: cfg.MachineID, AcceptSession: cfg.AcceptSession, Now: func() time.Time { return floor.Add(4 * time.Minute) }})
			if err != nil || len(result.Published) != 1 || len(result.Errors) != 0 {
				t.Fatalf("floor publication %#v %v", result, err)
			}
			raw, err := store.PublishedMetadata(regs[0].ArchiveSessionID)
			if err != nil {
				t.Fatal(err)
			}
			var meta archive.Metadata
			if err := json.Unmarshal(raw, &meta); err != nil {
				t.Fatal(err)
			}
			assertBlanketNativeAnalysis(t, meta)
			source, err := objects.Get(context.Background(), meta.SourceBundle.Key)
			if err != nil || len(source) == 0 {
				t.Fatal("readback failed", err)
			}
			validatePublishedDiscovery(t, raw, source)
		})
	}
}

// Blanket publication must derive native facts through the injected parser port.
func assertBlanketNativeAnalysis(t *testing.T, meta archive.Metadata) {
	t.Helper()
	if meta.Parser.Version != (codex.Parser{}).Version() || meta.Parser.Status != archive.ParserStatusPartial || meta.Counts.Messages == nil || *meta.Counts.Messages < 1 {
		t.Fatalf("blanket publication lacks native analysis: parser=%#v counts=%#v", meta.Parser, meta.Counts)
	}
}
