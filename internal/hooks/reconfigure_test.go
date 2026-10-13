package hooks

import (
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"os"
	"path/filepath"
	"testing"
)

// An intermediate native removal must not discard the permissions of a file
// that the same atomic setup change keeps for the next selected owner.
func TestReconfigurationNativeOwnerSwitchKeepsExistingMode(t *testing.T) {
	t.Parallel()
	t.Run("genuinely-absent", func(t *testing.T) {
		t.Parallel()
		files := Files{"codex": filepath.Join(t.TempDir(), "settings")}
		hook := Hook{Ports: testPorts, Executable: "/agent-archive", DataHome: t.TempDir()}
		changes, err := PlanReconfiguration(files, nil, hook, hook, []string{"codex"}, nil)
		must(t, err)
		if len(changes) != 1 || changes[0].Existed || changes[0].Mode != 0600 {
			t.Fatal("genuinely absent native destination lost creation defaults")
		}
	})
	for _, alias := range []bool{false, true} {
		t.Run(map[bool]string{false: "same-path", true: "symlink"}[alias], func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "settings")
			if err := os.WriteFile(path, nil, 0640); err != nil {
				t.Fatal(err)
			}
			must(t, os.Chmod(path, 0640))
			initialInfo, err := os.Stat(path)
			must(t, err)
			if initialInfo.Mode().Perm() != 0640 {
				t.Fatalf("initial target mode = %04o, want 0640", initialInfo.Mode().Perm())
			}
			nextPath := path
			if alias {
				nextPath = path + "-alias"
				if err := os.Symlink(path, nextPath); err != nil {
					t.Fatal(err)
				}
			}
			hook := Hook{Ports: testPorts, Executable: "/agent-archive", DataHome: t.TempDir()}
			previous := Files{"claude": path}
			initial, err := Plan(previous, hook, []string{"claude"})
			must(t, err)
			must(t, Apply(initial))
			retirement, found, err := PlanRemovalOf(previous, hook, "claude")
			must(t, err)
			if !found || !retirement.Delete {
				t.Fatal("native removal should delete the owned-only regular file")
			}
			files := Files{"codex": nextPath}
			changes, err := PlanReconfiguration(files, previous, hook, hook, []string{"codex"}, []string{"claude"})
			must(t, err)
			if len(changes) != 1 {
				t.Fatalf("owner switch planned %d changes", len(changes))
			}
			if changes[0].Delete || changes[0].Mode != 0640 {
				t.Fatalf("atomic owner switch: delete=%t mode=%04o", changes[0].Delete, changes[0].Mode)
			}
			must(t, Apply(changes))
			installed, err := Installed(files, hook, "codex")
			must(t, err)
			info, err := os.Stat(path)
			must(t, err)
			if !installed || info.Mode().Perm() != 0640 {
				t.Fatal("next native owner or existing permissions lost")
			}
			if !alias {
				retirement, err := PlanReconfiguration(nil, files, hook, hook, nil, []string{"codex"})
				must(t, err)
				if len(retirement) != 1 || !retirement[0].Delete {
					t.Fatal("pure native deselection no longer deletes an owned-only regular file")
				}
			}
			if alias {
				info, err := os.Lstat(nextPath)
				must(t, err)
				if info.Mode()&os.ModeSymlink == 0 {
					t.Fatal("native settings alias replaced")
				}
			}
		})
	}
}

type nativeAliasLookup struct{ port agentapi.HookConfigurator }

func (p nativeAliasLookup) LookupHooks(string) (agentapi.HookConfigurator, bool) { return p.port, true }

func (p nativeAliasLookup) HookAgents() []string { return []string{"first", "second"} }

// Regression ALIAS-RETIRE-01 covers both owner orders before effects.
func TestReconfigurationSharedNativeAliasRetirementRefusesDeletion(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(map[bool]string{false: "regular-first", true: "alias-first"}[reverse], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings")
			alias := path + "-alias"
			must(t, os.WriteFile(path, nil, 0640))
			must(t, os.Symlink(path, alias))
			port, _ := testPorts.LookupHooks("claude")
			hook := Hook{Ports: nativeAliasLookup{port}, Executable: "/agent-archive", DataHome: t.TempDir()}
			files := Files{"first": path, "second": alias}
			initial, err := Plan(files, hook, []string{"first", "second"})
			must(t, err)
			must(t, Apply(initial))
			names := []string{"first", "second"}
			if reverse {
				names = []string{"second", "first"}
			}
			original, err := os.ReadFile(path)
			must(t, err)
			changes, err := PlanReconfiguration(nil, files, hook, hook, nil, names)
			if err == nil || len(changes) != 0 {
				t.Fatal("conflicting deletion accepted")
			}
			got, err := os.ReadFile(path)
			must(t, err)
			if string(got) != string(original) {
				t.Fatal("refusal changed bytes")
			}
			if _, err := os.Stat(alias); err != nil {
				t.Fatalf("shared retirement left alias dangling: %v", err)
			}
		})
	}
}

func TestReconfigurationNativeAliasOnlyRetirementKeepsTarget(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "settings")
	alias := path + "-alias"
	must(t, os.WriteFile(path, nil, 0640))
	must(t, os.Chmod(path, 0640))
	initialInfo, err := os.Stat(path)
	must(t, err)
	if initialInfo.Mode().Perm() != 0640 {
		t.Fatalf("initial target mode = %04o, want 0640", initialInfo.Mode().Perm())
	}
	must(t, os.Symlink(path, alias))
	hook := Hook{Ports: testPorts, Executable: "/agent-archive", DataHome: t.TempDir()}
	files := Files{"claude": alias}
	changes, err := Plan(files, hook, []string{"claude"})
	must(t, err)
	must(t, Apply(changes))
	changes, err = PlanReconfiguration(nil, files, hook, hook, nil, []string{"claude"})
	must(t, err)
	if len(changes) != 1 || changes[0].Delete {
		t.Fatal("alias-only retirement lost native target preservation")
	}
	must(t, Apply(changes))
	info, err := os.Stat(path)
	must(t, err)
	if info.Mode().Perm() != 0640 {
		t.Fatal("target mode lost")
	}
	data, err := os.ReadFile(alias)
	must(t, err)
	if !Empty(data) {
		t.Fatal("owned-only target not emptied")
	}
	info, err = os.Lstat(alias)
	must(t, err)
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("alias replaced")
	}
}
