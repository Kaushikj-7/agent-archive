package cli

import (
	"bytes"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/filechange"
	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

const (
	sharedInstallFirstID  agentmeta.ID = "first"
	sharedInstallSecondID agentmeta.ID = "second"
)

type sharedInstallCase string

const (
	sharedInstallIdentical    sharedInstallCase = "identical"
	sharedInstallAbsent       sharedInstallCase = "absent"
	sharedInstallByteConflict sharedInstallCase = "conflicting-bytes"
	sharedInstallModeConflict sharedInstallCase = "conflicting-mode"
	sharedInstallAlias        sharedInstallCase = "physical-alias"
	sharedInstallHardlink     sharedInstallCase = "hardlink-destinations"
	sharedInstallDifferent    sharedInstallCase = "different-locations"
)

// Regression SHARED-INSTALL-01: selected managed owners may share one native file.
type sharedInstallHooks struct {
	sharedRemovalHooks
	path string
}

func (h sharedInstallHooks) Location(agentapi.HookLocations) string { return h.path }

func TestSharedInstallPlansReachJournal(t *testing.T) {
	for _, kind := range []sharedInstallCase{sharedInstallIdentical, sharedInstallAbsent, sharedInstallByteConflict, sharedInstallModeConflict, sharedInstallAlias, sharedInstallHardlink, sharedInstallDifferent} {
		t.Run(string(kind), func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(t.TempDir(), "settings")
			before := []byte("original shared settings")
			after := []byte("identical installed settings")
			if kind != sharedInstallAbsent {
				if err := os.WriteFile(path, before, 0600); err != nil {
					t.Fatal(err)
				}
			}
			secondPath := path
			secondAfter := after
			secondMode := os.FileMode(0600)
			if kind == sharedInstallByteConflict {
				secondAfter = []byte("conflicting installed settings")
			}
			if kind == sharedInstallModeConflict {
				secondMode = 0644
			}
			if kind == sharedInstallAlias {
				secondPath = filepath.Join(filepath.Dir(path), "alias")
				if err := os.Symlink(path, secondPath); err != nil {
					t.Fatal(err)
				}
			}
			if kind == sharedInstallHardlink {
				secondPath = filepath.Join(filepath.Dir(path), "hardlink")
				if err := os.Link(path, secondPath); err != nil {
					t.Fatal(err)
				}
			}
			if kind == sharedInstallDifferent {
				secondPath = filepath.Join(filepath.Dir(path), "other")
				if err := os.WriteFile(secondPath, before, 0600); err != nil {
					t.Fatal(err)
				}
			}
			ports := sharedRemovalLookup{first: sharedRemovalHooks{after: after, mode: 0600}, second: sharedRemovalHooks{after: secondAfter, mode: secondMode}}
			plan, err := hooks.Plan(hooks.Files{"first": path, "second": secondPath}, hooks.Hook{Ports: ports}, []string{"first", "second"})
			if kind == sharedInstallByteConflict || kind == sharedInstallModeConflict {
				if err == nil || len(plan) != 0 {
					t.Errorf("conflicting plans accepted: plans=%d err=%v", len(plan), err)
				}
				got, readErr := os.ReadFile(path)
				if readErr != nil || !bytes.Equal(got, before) || setupjournal.TransactionPending(home) {
					t.Fatal("conflict changed settings or journal")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := 1
			if kind == sharedInstallDifferent || kind == sharedInstallHardlink {
				want = 2
			}
			if len(plan) != want || plan[0].Path != path {
				t.Fatalf("plans=%+v want count=%d first path=%s", plan, want, path)
			}
			err = setupjournal.Commit(home, setupjournal.Journal{FilesOnly: true, Changes: plan}, nil)
			got, readErr := os.ReadFile(path)
			t.Logf("actual Plan/Commit: kind=%s plans=%d commitErr=%v beforeRetained=%t afterRetained=%t journalPending=%t", kind, len(plan), err, bytes.Equal(got, before), bytes.Equal(got, after), setupjournal.TransactionPending(home))
			if err != nil || readErr != nil || !bytes.Equal(got, after) || setupjournal.TransactionPending(home) {
				t.Errorf("agreeing selected owners cannot install: err=%v data=%q", err, got)
			}
			second, readErr := os.ReadFile(secondPath)
			if readErr != nil || !bytes.Equal(second, after) {
				t.Fatalf("second location bytes=%q err=%v", second, readErr)
			}
		})
	}
}

func TestSharedInstallOwnersReachActualSetup(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(fmt.Sprintf("conflict=%t", conflict), func(t *testing.T) {
			home, userHome := t.TempDir(), t.TempDir()
			at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
			path := filepath.Join(userHome, "shared-native", "settings")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			before := []byte("original shared settings")
			after := []byte("identical installed settings")
			if err := os.WriteFile(path, before, 0600); err != nil {
				t.Fatal(err)
			}
			descriptors := append(agentmeta.Builtins().All(), agentmeta.Descriptor{ID: sharedInstallFirstID, DisplayName: "First"}, agentmeta.Descriptor{ID: sharedInstallSecondID, DisplayName: "Second"})
			catalog, err := agentmeta.New(descriptors)
			if err != nil {
				t.Fatal(err)
			}
			var bindings []builtin.Integration
			for _, d := range agentmeta.Builtins().All() {
				b, _ := productionAgents.Lookup(string(d.ID))
				b.Descriptor = agentmeta.Descriptor{ID: d.ID}
				bindings = append(bindings, b)
			}
			for _, id := range []agentmeta.ID{sharedInstallFirstID, sharedInstallSecondID} {
				installed := after
				if conflict && id == sharedInstallSecondID {
					installed = []byte("conflicting installed settings")
				}
				bindings = append(bindings, builtin.Integration{Descriptor: agentmeta.Descriptor{ID: id}, Hooks: sharedInstallHooks{sharedRemovalHooks: sharedRemovalHooks{after: installed, mode: 0600}, path: path}})
			}
			registry, err := builtin.New(catalog, bindings)
			if err != nil {
				t.Fatal(err)
			}
			env := setupTestEnv(t, home, userHome, newFakeKeychain(), at)
			env.Agents = registry
			exe, err := env.executable()
			if err != nil {
				t.Fatal(err)
			}
			next := config.Config{MachineID: "machine", Harnesses: []string{"first", "second"}, NoSkills: true, Storage: credentials.Config{Provider: credentials.ProviderS3, Bucket: "synthetic-bucket", Region: "region", AWSProfile: "profile"}, Archive: archive.Config{SchemaVersion: 1, Enabled: true, MachineID: "machine", Projects: []archive.ProjectActivation{{Root: t.TempDir(), Included: true}}}}
			err = applySetup(home, userHome, exe, config.Config{}, &next, nil, env)
			got, readErr := os.ReadFile(path)
			_, found, cfgErr := config.Load(home)
			t.Logf("actual applySetup: err=%v beforeRetained=%t configCommitted=%t cfgErr=%v journalPending=%t", err, bytes.Equal(got, before), found, cfgErr, setupjournal.TransactionPending(home))
			if conflict {
				if err == nil || readErr != nil || !bytes.Equal(got, before) || found || cfgErr != nil || setupjournal.TransactionPending(home) {
					t.Fatalf("conflicting setup reached effects: err=%v bytes=%q committed=%t", err, got, found)
				}
				return
			}
			if err != nil || readErr != nil || !bytes.Equal(got, after) || !found || cfgErr != nil {
				t.Errorf("actual setup cannot install identical shared plans: %v", err)
			}
			saved, _, err := config.Load(home)
			if err != nil || saved.HookFiles["first"] != path || saved.HookFiles["second"] != path {
				t.Fatalf("selected ownership paths lost: %+v %v", saved.HookFiles, err)
			}

		})
	}
}

// Regression SHARED-UNINSTALL-HARDLINK-01: both rename destinations need removal.
func TestSharedUninstallHardlinkWriteDestinations(t *testing.T) {
	for _, sameOwner := range []bool{false, true} {
		t.Run(fmt.Sprintf("sameOwner=%t", sameOwner), func(t *testing.T) {
			dir := t.TempDir()
			first, second := filepath.Join(dir, "first"), filepath.Join(dir, "second")
			before, after := []byte("before"), []byte("removed")
			if err := os.WriteFile(first, before, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(first, second); err != nil {
				t.Fatal(err)
			}
			ports := sharedRemovalLookup{first: sharedRemovalHooks{after: after, mode: 0600}, second: sharedRemovalHooks{after: after, mode: 0600}}
			files := hooks.Files{"first": first, "second": second}
			var legacy hooks.Files
			if sameOwner {
				files = hooks.Files{"first": first}
				legacy = hooks.Files{"first": second}
			}
			installed := []string{"first", "second"}
			if sameOwner {
				installed = []string{"first"}
			}
			changes, skipped, err := planUninstallHooks(files, legacy, hooks.Hook{Ports: ports}, installed)
			if err != nil {
				t.Fatal(err)
			}
			if len(changes) != 2 || len(skipped) != 0 {
				t.Fatalf("plans=%d skipped=%v", len(changes), skipped)
			}
			if err := hooks.Apply(changes); err != nil {
				t.Fatal(err)
			}
			left, _ := os.ReadFile(first)
			right, _ := os.ReadFile(second)
			t.Logf("sameOwner=%t plans=%d skipped=%v first=%q second=%q", sameOwner, len(changes), skipped, left, right)
			if !bytes.Equal(left, after) || !bytes.Equal(right, after) {
				t.Errorf("SHARED-UNINSTALL-HARDLINK-01: normal removal skipped distinct atomic-rename destination")
			}
		})
	}
}

// Regression SFV-SHARED-DESELECT-01: actual setup can retire shared owners.
type sfvRemovalPort struct {
	syntheticHooks
	path  string
	prior []byte
}

func (h sfvRemovalPort) Location(agentapi.HookLocations) string { return h.path }

func (h sfvRemovalPort) Plan(r agentapi.HookPlanRequest) ([]filechange.Change, error) {
	if r.File.ReadError != nil {
		return nil, r.File.ReadError
	}
	after := []byte("shared-installed-owner-bytes")
	if r.Action == agentapi.HookRemove {
		if !bytes.Equal(r.File.Bytes, after) {
			return nil, nil
		}
		after = append([]byte(nil), h.prior...)
	}
	return []filechange.Change{{Path: r.File.Path, Before: append([]byte(nil), r.File.Bytes...), After: after, Existed: r.File.Present, Mode: r.File.Mode}}, nil
}

func TestSFVSharedSetupDeselect(t *testing.T) {
	for _, owners := range []int{1, 2} {
		t.Run(fmt.Sprintf("owners-%d", owners), func(t *testing.T) {
			home, userHome := t.TempDir(), t.TempDir()
			at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
			path := filepath.Join(userHome, "shared-native", "settings")
			before := []byte("foreign settings preserved byte for byte")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, before, 0640); err != nil {
				t.Fatal(err)
			}
			descriptors := append(agentmeta.Builtins().All(), agentmeta.Descriptor{ID: sharedInstallFirstID, DisplayName: "First"}, agentmeta.Descriptor{ID: sharedInstallSecondID, DisplayName: "Second"})
			catalog, err := agentmeta.New(descriptors)
			if err != nil {
				t.Fatal(err)
			}
			var bindings []builtin.Integration
			for _, d := range agentmeta.Builtins().All() {
				b, _ := productionAgents.Lookup(string(d.ID))
				b.Descriptor = agentmeta.Descriptor{ID: d.ID}
				bindings = append(bindings, b)
			}
			for _, id := range []agentmeta.ID{sharedInstallFirstID, sharedInstallSecondID} {
				bindings = append(bindings, builtin.Integration{Descriptor: agentmeta.Descriptor{ID: id}, Hooks: sfvRemovalPort{path: path, prior: before}})
			}
			registry, err := builtin.New(catalog, bindings)
			if err != nil {
				t.Fatal(err)
			}
			env := setupTestEnv(t, home, userHome, newFakeKeychain(), at)
			env.Agents = registry
			exe, err := env.executable()
			if err != nil {
				t.Fatal(err)
			}
			apps := []string{"first"}
			if owners == 2 {
				apps = append(apps, "second")
			}
			next := config.Config{MachineID: "machine", Harnesses: apps, NoSkills: true, Storage: credentials.Config{Provider: credentials.ProviderS3, Bucket: "synthetic-bucket", Region: "region", AWSProfile: "profile"}, Archive: archive.Config{SchemaVersion: 1, Enabled: true, MachineID: "machine", Projects: []archive.ProjectActivation{{Root: t.TempDir(), Included: true}}}}
			if err := applySetup(home, userHome, exe, config.Config{}, &next, nil, env); err != nil {
				t.Fatal("initial actual setup", err)
			}
			saved, found, err := config.Load(home)
			if err != nil || !found {
				t.Fatal("initial configuration", err)
			}
			installed, err := os.ReadFile(path)
			if err != nil || string(installed) != "shared-installed-owner-bytes" {
				t.Fatal("initial shared install", err)
			}
			changed := saved
			changed.Harnesses = []string{"codex"}
			err = applySetup(home, userHome, exe, saved, &changed, nil, env)
			after, _ := os.ReadFile(path)
			current, _, cfgErr := config.Load(home)
			t.Logf("owners=%d actual reconfigure error=%v foreignRestored=%t oldInstalledRetained=%t oldConfigRetained=%t cfgErr=%v journalPending=%t", owners, err, bytes.Equal(after, before), bytes.Equal(after, installed), reflect.DeepEqual(current, saved), cfgErr, setupjournal.TransactionPending(home))
			if err != nil {
				t.Errorf("SFV-SHARED-DESELECT-01: identical shared removal plans prevent actual setup deselection: %v", err)
			}
			if err == nil && (!bytes.Equal(after, before) || !reflect.DeepEqual(current.Harnesses, []string{"codex"})) {
				t.Fatal("successful control lost prior settings or next config")
			}
		})
	}
}

// Regression SFV-SHARED-SWITCH-01: compose retiring and installing native owners.
type sfvSwitchPort struct {
	syntheticHooks
	path  string
	token []byte
}

func (h sfvSwitchPort) Location(agentapi.HookLocations) string { return h.path }

func (h sfvSwitchPort) Plan(r agentapi.HookPlanRequest) ([]filechange.Change, error) {
	if r.File.ReadError != nil {
		return nil, r.File.ReadError
	}
	after := append([]byte(nil), r.File.Bytes...)
	if r.Action == agentapi.HookRemove {
		if !bytes.Contains(after, h.token) {
			return nil, nil
		}
		after = bytes.ReplaceAll(after, h.token, nil)
	} else if !bytes.Contains(after, h.token) {
		after = append(after, h.token...)
	}
	return []filechange.Change{{Path: r.File.Path, Before: append([]byte(nil), r.File.Bytes...), After: after, Existed: r.File.Present, Mode: r.File.Mode}}, nil
}

func TestSFVSharedSetupSwitch(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(fmt.Sprintf("shared-%t", shared), func(t *testing.T) {
			home, userHome := t.TempDir(), t.TempDir()
			at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
			first := filepath.Join(userHome, "first-settings")
			second := filepath.Join(userHome, "second-settings")
			if shared {
				second = first
			}
			prior := []byte("foreign settings preserved byte for byte")
			a := []byte("|owned-first|")
			b := []byte("|owned-second|")
			for _, path := range []string{first, second} {
				if err := os.WriteFile(path, prior, 0640); err != nil {
					t.Fatal(err)
				}
			}
			descriptors := append(agentmeta.Builtins().All(), agentmeta.Descriptor{ID: sharedInstallFirstID, DisplayName: "First"}, agentmeta.Descriptor{ID: sharedInstallSecondID, DisplayName: "Second"})
			catalog, err := agentmeta.New(descriptors)
			if err != nil {
				t.Fatal(err)
			}
			var bindings []builtin.Integration
			for _, desc := range agentmeta.Builtins().All() {
				x, _ := productionAgents.Lookup(string(desc.ID))
				x.Descriptor = agentmeta.Descriptor{ID: desc.ID}
				bindings = append(bindings, x)
			}
			bindings = append(bindings, builtin.Integration{Descriptor: agentmeta.Descriptor{ID: sharedInstallFirstID}, Hooks: sfvSwitchPort{path: first, token: a}}, builtin.Integration{Descriptor: agentmeta.Descriptor{ID: sharedInstallSecondID}, Hooks: sfvSwitchPort{path: second, token: b}})
			registry, err := builtin.New(catalog, bindings)
			if err != nil {
				t.Fatal(err)
			}
			env := setupTestEnv(t, home, userHome, newFakeKeychain(), at)
			env.Agents = registry
			exe, err := env.executable()
			if err != nil {
				t.Fatal(err)
			}
			next := config.Config{MachineID: "machine", Harnesses: []string{"first"}, NoSkills: true, Storage: credentials.Config{Provider: credentials.ProviderS3, Bucket: "synthetic-bucket", Region: "region", AWSProfile: "profile"}, Archive: archive.Config{SchemaVersion: 1, Enabled: true, MachineID: "machine", Projects: []archive.ProjectActivation{{Root: t.TempDir(), Included: true}}}}
			if err := applySetup(home, userHome, exe, config.Config{}, &next, nil, env); err != nil {
				t.Fatal("initial actual setup", err)
			}
			saved, found, err := config.Load(home)
			if err != nil || !found {
				t.Fatal("initial config", err)
			}
			installed, err := os.ReadFile(first)
			if err != nil || !bytes.Equal(installed, append(append([]byte(nil), prior...), a...)) {
				t.Fatal("initial owner bytes", err)
			}
			changed := saved
			changed.Harnesses = []string{"second"}
			err = applySetup(home, userHome, exe, saved, &changed, nil, env)
			left, _ := os.ReadFile(first)
			right, _ := os.ReadFile(second)
			current, _, cfgErr := config.Load(home)
			t.Logf("shared=%t actual switch error=%v oldInstalledRetained=%t oldConfigRetained=%t cfgErr=%v journalPending=%t first=%q second=%q", shared, err, bytes.Equal(left, installed), reflect.DeepEqual(current, saved), cfgErr, setupjournal.TransactionPending(home), left, right)
			if err != nil {
				t.Errorf("SFV-SHARED-SWITCH-01: installation and removal plans collide in actual setup: %v", err)
			}
			if err == nil {
				expected := append(append([]byte(nil), prior...), b...)
				if !bytes.Equal(right, expected) || (!shared && !bytes.Equal(left, prior)) || !reflect.DeepEqual(current.Harnesses, []string{"second"}) {
					t.Fatal("switch lost foreign settings or next config")
				}
			}
		})
	}
}

// setupTransitionHooks uses native token ownership, including optional deletion.
type setupTransitionHooks struct {
	syntheticHooks
	path         string
	token        []byte
	deleteEmpty  bool
	removalAfter []byte
	installMode  os.FileMode
}

func (h setupTransitionHooks) Location(agentapi.HookLocations) string { return h.path }

func (h setupTransitionHooks) Plan(r agentapi.HookPlanRequest) ([]filechange.Change, error) {
	after := bytes.Clone(r.File.Bytes)
	if r.Action == agentapi.HookRemove {
		if h.removalAfter != nil {
			after = bytes.Clone(h.removalAfter)
		} else {
			if !bytes.Contains(after, h.token) {
				return nil, nil
			}
			after = bytes.ReplaceAll(after, h.token, nil)
		}
	} else if !bytes.Contains(after, h.token) {
		after = append(after, h.token...)
	}
	mode := r.File.Mode
	if r.Action == agentapi.HookInstall && h.installMode != 0 {
		mode = h.installMode
	}
	return []filechange.Change{{Path: r.File.Path, Before: bytes.Clone(r.File.Bytes), After: after, Existed: r.File.Present, Mode: mode, Delete: r.Action == agentapi.HookRemove && h.deleteEmpty && len(after) == 0 && r.File.Regular}}, nil
}

type setupTransitionCase string

const (
	setupTransitionDeselect            setupTransitionCase = "deselect"
	setupTransitionPartial             setupTransitionCase = "partial"
	setupTransitionSwitch              setupTransitionCase = "switch"
	setupTransitionAliasSwitch         setupTransitionCase = "alias-switch"
	setupTransitionHardlinkSwitch      setupTransitionCase = "hardlink-switch"
	setupTransitionMovedSelected       setupTransitionCase = "moved-selected"
	setupTransitionDeleteInstall       setupTransitionCase = "delete-install"
	setupTransitionConflictingRemovals setupTransitionCase = "conflicting-removals"
	setupTransitionConflictingInstalls setupTransitionCase = "conflicting-installs"
	setupTransitionConflictingModes    setupTransitionCase = "conflicting-modes"
)

func TestSetupSharedDestinationTransitions(t *testing.T) {
	for _, kind := range []setupTransitionCase{setupTransitionDeselect, setupTransitionPartial, setupTransitionSwitch, setupTransitionAliasSwitch, setupTransitionHardlinkSwitch, setupTransitionMovedSelected, setupTransitionDeleteInstall, setupTransitionConflictingRemovals, setupTransitionConflictingInstalls, setupTransitionConflictingModes} {
		t.Run(string(kind), func(t *testing.T) {
			home, userHome := t.TempDir(), t.TempDir()
			path := filepath.Join(userHome, "settings")
			prior := []byte("foreign bytes\n")
			if kind == setupTransitionDeleteInstall {
				prior = nil
			}
			if err := os.WriteFile(path, prior, 0640); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0640); err != nil {
				t.Fatal(err)
			}
			secondPath := path
			if kind == setupTransitionAliasSwitch {
				secondPath = filepath.Join(userHome, "alias")
				if err := os.Symlink(path, secondPath); err != nil {
					t.Fatal(err)
				}
			}
			if kind == setupTransitionHardlinkSwitch {
				secondPath = filepath.Join(userHome, "hardlink")
				if err := os.Link(path, secondPath); err != nil {
					t.Fatal(err)
				}
			}
			firstToken, secondToken := []byte("|first|"), []byte("|first|")
			if kind == setupTransitionSwitch || kind == setupTransitionAliasSwitch || kind == setupTransitionHardlinkSwitch || kind == setupTransitionDeleteInstall {
				secondToken = []byte("|second|")
			}
			firstPort := setupTransitionHooks{path: path, token: firstToken, deleteEmpty: true}
			secondPort := setupTransitionHooks{path: secondPath, token: secondToken, deleteEmpty: true}
			makeRegistry := func() *builtin.Registry {
				t.Helper()
				catalog, err := agentmeta.New(append(agentmeta.Builtins().All(), agentmeta.Descriptor{ID: sharedInstallFirstID, DisplayName: "First"}, agentmeta.Descriptor{ID: sharedInstallSecondID, DisplayName: "Second"}))
				if err != nil {
					t.Fatal(err)
				}
				var bindings []builtin.Integration
				for _, d := range agentmeta.Builtins().All() {
					b, _ := productionAgents.Lookup(string(d.ID))
					b.Descriptor = agentmeta.Descriptor{ID: d.ID}
					bindings = append(bindings, b)
				}
				bindings = append(bindings, builtin.Integration{Descriptor: agentmeta.Descriptor{ID: sharedInstallFirstID}, Hooks: firstPort}, builtin.Integration{Descriptor: agentmeta.Descriptor{ID: sharedInstallSecondID}, Hooks: secondPort})
				registry, err := builtin.New(catalog, bindings)
				if err != nil {
					t.Fatal(err)
				}
				return registry
			}
			env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
			env.Agents = makeRegistry()
			exe, err := env.executable()
			if err != nil {
				t.Fatal(err)
			}
			initial := []string{"first"}
			if kind == setupTransitionDeselect || kind == setupTransitionPartial || kind == setupTransitionConflictingRemovals {
				initial = append(initial, "second")
			}
			next := config.Config{MachineID: "machine", Harnesses: initial, NoSkills: true, Storage: credentials.Config{Provider: credentials.ProviderS3, Bucket: "synthetic-bucket", Region: "region", AWSProfile: "profile"}, Archive: archive.Config{SchemaVersion: 1, Enabled: true, MachineID: "machine", Projects: []archive.ProjectActivation{{Root: t.TempDir(), Included: true}}}}
			if err := applySetup(home, userHome, exe, config.Config{}, &next, nil, env); err != nil {
				t.Fatal(err)
			}
			saved, _, err := config.Load(home)
			if err != nil {
				t.Fatal(err)
			}
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			changed := saved
			changed.Harnesses = []string{"second"}
			if kind == setupTransitionDeselect || kind == setupTransitionConflictingRemovals {
				changed.Harnesses = []string{"codex"}
			}
			if kind == setupTransitionMovedSelected {
				firstPort.path = filepath.Join(userHome, "moved")
				changed.Harnesses = []string{"first"}
			}
			if kind == setupTransitionConflictingRemovals {
				firstPort.removalAfter = []byte("first removal")
				secondPort.removalAfter = []byte("second removal")
			}
			if kind == setupTransitionConflictingInstalls {
				secondPort.token = []byte("|conflict|")
				changed.Harnesses = []string{"first", "second"}
			}
			if kind == setupTransitionConflictingModes {
				secondPort.installMode = 0600
				changed.Harnesses = []string{"first", "second"}
			}
			env.Agents = makeRegistry()
			err = applySetup(home, userHome, exe, saved, &changed, nil, env)
			current, _, cfgErr := config.Load(home)
			if cfgErr != nil {
				t.Fatal(cfgErr)
			}
			conflict := kind == setupTransitionConflictingRemovals || kind == setupTransitionConflictingInstalls || kind == setupTransitionConflictingModes
			if setupjournal.TransactionPending(home) {
				t.Fatal("journal remains pending")
			}
			if conflict {
				got, readErr := os.ReadFile(path)
				if err == nil || readErr != nil || !bytes.Equal(got, original) || !reflect.DeepEqual(current, saved) {
					t.Fatalf("conflict reached effects: err=%v bytes=%q", err, got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(current.Harnesses, changed.Harnesses) {
				t.Fatal("next configuration not committed")
			}
			expectedFirst := prior
			expectedSecond := append(bytes.Clone(prior), secondToken...)
			if kind == setupTransitionPartial {
				expectedFirst = expectedSecond
			}
			if (secondPath == path || kind == setupTransitionAliasSwitch) && kind != setupTransitionDeselect && kind != setupTransitionMovedSelected {
				expectedFirst = expectedSecond
			}
			got, readErr := os.ReadFile(path)
			if readErr != nil || !bytes.Equal(got, expectedFirst) {
				t.Fatalf("first bytes=%q expected=%q err=%v", got, expectedFirst, readErr)
			}
			info, statErr := os.Stat(path)
			if statErr != nil || info.Mode().Perm() != 0640 {
				t.Fatalf("foreign mode changed: %v %v", info, statErr)
			}
			if kind == setupTransitionMovedSelected {
				got, err := os.ReadFile(firstPort.path)
				if err != nil || !bytes.Equal(got, firstToken) || current.HookFiles["first"] != firstPort.path {
					t.Fatalf("moved owner bytes=%q err=%v", got, err)
				}
			} else if kind != setupTransitionDeselect {
				got, err := os.ReadFile(secondPath)
				if err != nil || !bytes.Equal(got, expectedSecond) {
					t.Fatalf("second bytes=%q err=%v", got, err)
				}
			}
			if kind == setupTransitionAliasSwitch {
				info, err := os.Lstat(secondPath)
				if err != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Fatal("alias replaced")
				}
			}
		})
	}
}

type relocatedNativeSetupHooks struct {
	agentapi.HookConfigurator
	path string
}

func (h relocatedNativeSetupHooks) Location(agentapi.HookLocations) string { return h.path }

// Regression MODE-SWITCH-01: virtual retirement cannot apply native creation
// permissions to an existing destination retained by actual setup.
func TestSetupNativeOwnerSwitchKeepsExistingMode(t *testing.T) {
	t.Parallel()
	for _, alias := range []bool{false, true} {
		t.Run(fmt.Sprintf("alias-%t", alias), func(t *testing.T) {
			t.Parallel()
			home, userHome := t.TempDir(), t.TempDir()
			path := filepath.Join(userHome, "settings")
			if err := os.WriteFile(path, nil, 0640); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0640); err != nil {
				t.Fatal(err)
			}
			nextPath := path
			if alias {
				nextPath += "-alias"
				if err := os.Symlink(path, nextPath); err != nil {
					t.Fatal(err)
				}
			}
			var bindings []builtin.Integration
			for _, d := range agentmeta.Builtins().All() {
				b, _ := productionAgents.Lookup(string(d.ID))
				b.Descriptor = agentmeta.Descriptor{ID: d.ID}
				bindings = append(bindings, b)
			}
			first, _ := productionAgents.Lookup("claude")
			second, _ := productionAgents.Lookup("codex")
			bindings = append(bindings,
				builtin.Integration{Descriptor: agentmeta.Descriptor{ID: sharedInstallFirstID}, Hooks: relocatedNativeSetupHooks{HookConfigurator: first.Hooks, path: path}},
				builtin.Integration{Descriptor: agentmeta.Descriptor{ID: sharedInstallSecondID}, Hooks: relocatedNativeSetupHooks{HookConfigurator: second.Hooks, path: nextPath}})
			catalog, err := agentmeta.New(append(agentmeta.Builtins().All(), agentmeta.Descriptor{ID: sharedInstallFirstID, DisplayName: "First"}, agentmeta.Descriptor{ID: sharedInstallSecondID, DisplayName: "Second"}))
			if err != nil {
				t.Fatal(err)
			}
			registry, err := builtin.New(catalog, bindings)
			if err != nil {
				t.Fatal(err)
			}
			env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
			env.Agents = registry
			exe, err := env.executable()
			if err != nil {
				t.Fatal(err)
			}
			next := config.Config{MachineID: "machine", Harnesses: []string{"first"}, NoSkills: true, Storage: credentials.Config{Provider: credentials.ProviderS3, Bucket: "synthetic-bucket", Region: "region", AWSProfile: "profile"}, Archive: archive.Config{SchemaVersion: 1, Enabled: true, MachineID: "machine", Projects: []archive.ProjectActivation{{Root: t.TempDir(), Included: true}}}}
			if err := applySetup(home, userHome, exe, config.Config{}, &next, nil, env); err != nil {
				t.Fatal(err)
			}
			saved, _, err := config.Load(home)
			if err != nil {
				t.Fatal(err)
			}
			changed := saved
			changed.Harnesses = []string{"second"}
			if err := applySetup(home, userHome, exe, saved, &changed, nil, env); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0640 {
				t.Fatalf("existing native settings mode lost: %v %v", info, err)
			}
			current, _, err := config.Load(home)
			if err != nil || !reflect.DeepEqual(current.Harnesses, changed.Harnesses) || setupjournal.TransactionPending(home) {
				t.Fatal("owner switch configuration or journal incomplete", err)
			}
			installed, err := hooks.Installed(hooks.Files{"second": nextPath}, env.installation(home, userHome).hook(exe), "second")
			if err != nil || !installed {
				t.Fatal("next native owner is not installed", err)
			}
		})
	}
}

// Regression ALIAS-RETIRE-01: original symlink deletion restrictions survive shared retirement.
func TestSetupNativeAliasRetirementRefusesDeletion(t *testing.T) {
	t.Parallel()
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverse-%t", reverse), func(t *testing.T) {
			t.Parallel()
			home, userHome := t.TempDir(), t.TempDir()
			path := filepath.Join(userHome, "settings")
			if err := os.WriteFile(path, nil, 0640); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0640); err != nil {
				t.Fatal(err)
			}
			nextPath := path + "-alias"
			if err := os.Symlink(path, nextPath); err != nil {
				t.Fatal(err)
			}
			var bindings []builtin.Integration
			for _, d := range agentmeta.Builtins().All() {
				b, _ := productionAgents.Lookup(string(d.ID))
				b.Descriptor = agentmeta.Descriptor{ID: d.ID}
				bindings = append(bindings, b)
			}
			first, _ := productionAgents.Lookup("claude")
			bindings = append(bindings,
				builtin.Integration{Descriptor: agentmeta.Descriptor{ID: sharedInstallFirstID}, Hooks: relocatedNativeSetupHooks{HookConfigurator: first.Hooks, path: path}},
				builtin.Integration{Descriptor: agentmeta.Descriptor{ID: sharedInstallSecondID}, Hooks: relocatedNativeSetupHooks{HookConfigurator: first.Hooks, path: nextPath}})
			catalog, err := agentmeta.New(append(agentmeta.Builtins().All(), agentmeta.Descriptor{ID: sharedInstallFirstID, DisplayName: "First"}, agentmeta.Descriptor{ID: sharedInstallSecondID, DisplayName: "Second"}))
			if err != nil {
				t.Fatal(err)
			}
			registry, err := builtin.New(catalog, bindings)
			if err != nil {
				t.Fatal(err)
			}
			env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
			env.Agents = registry
			exe, err := env.executable()
			if err != nil {
				t.Fatal(err)
			}
			apps := []string{"first", "second"}
			if reverse {
				apps = []string{"second", "first"}
			}
			next := config.Config{MachineID: "machine", Harnesses: apps, NoSkills: true, Storage: credentials.Config{Provider: credentials.ProviderS3, Bucket: "synthetic-bucket", Region: "region", AWSProfile: "profile"}, Archive: archive.Config{SchemaVersion: 1, Enabled: true, MachineID: "machine", Projects: []archive.ProjectActivation{{Root: t.TempDir(), Included: true}}}}
			if err := applySetup(home, userHome, exe, config.Config{}, &next, nil, env); err != nil {
				t.Fatal(err)
			}
			saved, _, err := config.Load(home)
			if err != nil {
				t.Fatal(err)
			}
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			changed := saved
			changed.Harnesses = []string{"codex"}
			if err := applySetup(home, userHome, exe, saved, &changed, nil, env); err == nil {
				t.Fatal("conflicting deletion accepted")
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0640 {
				t.Fatalf("existing native settings mode lost: %v %v", info, err)
			}
			current, _, err := config.Load(home)
			if err != nil || !reflect.DeepEqual(current, saved) || setupjournal.TransactionPending(home) {
				t.Fatal("refusal changed configuration or left journal pending", err)
			}
			got, err := os.ReadFile(nextPath)
			if err != nil || !bytes.Equal(got, original) {
				t.Fatal("alias target changed", err)
			}
		})
	}
}
