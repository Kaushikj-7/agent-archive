package hooks

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testFiles(home string) Files {
	return ResolveFiles(home, func(string) (string, bool) { return "", false }, testPorts)
}

func TestPlanApplyAndRollback(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude", "settings.json")
	must(t, os.MkdirAll(filepath.Dir(path), 0700))
	original := []byte(`{"permissions":{"allow":["Read"]}}`)
	must(t, os.WriteFile(path, original, 0600))
	plan, e := Plan(testFiles(home), testHook("/Applications/agent-archive"), []string{"claude", "codex", "cursor"})
	if e != nil {
		t.Fatal(e)
	}
	if e = Apply(plan); e != nil {
		t.Fatal(e)
	}
	if e = Rollback(plan); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(path)
	if string(b) != string(original) {
		t.Fatal("original changed")
	}
	if _, e = os.Stat(filepath.Join(home, ".codex", "hooks.json")); !os.IsNotExist(e) {
		t.Fatal("new file not removed")
	}
}

func TestConcurrentEditPreserved(t *testing.T) {
	home := t.TempDir()
	plan, _ := Plan(testFiles(home), testHook("/bin/agent-archive"), []string{"codex"})
	path := plan[0].Path
	must(t, os.MkdirAll(filepath.Dir(path), 0700))
	must(t, os.WriteFile(path, []byte(`{"changed":true}`), 0600))
	if Apply(plan) == nil {
		t.Fatal("overwrote concurrent edit")
	}
}

func TestPlanRemovalStripsOnlyOurEntries(t *testing.T) {
	home := t.TempDir()
	claudePath := filepath.Join(home, ".claude", "settings.json")
	must(t, os.MkdirAll(filepath.Dir(claudePath), 0700))
	// An unrelated hook on an event we also use, plus unrelated settings.
	original := []byte(`{"permissions":{"allow":["Read"]},"hooks":{"Stop":[{"hooks":[{"type":"command","command":"say done"}]}]}}`)
	must(t, os.WriteFile(claudePath, original, 0600))
	cursorPath := filepath.Join(home, ".cursor", "hooks.json")
	must(t, os.MkdirAll(filepath.Dir(cursorPath), 0700))
	must(t, os.WriteFile(cursorPath, []byte(`{"version":1,"hooks":{"stop":[{"command":"echo unrelated"}]}}`), 0600))

	plan, err := Plan(testFiles(home), testHook("/Applications/agent-archive"), []string{"claude", "codex", "cursor"})
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(plan); err != nil {
		t.Fatal(err)
	}

	removal, err := PlanRemoval(testFiles(home), Hook{Ports: testPorts}, []string{"claude", "codex", "cursor"})
	if err != nil {
		t.Fatal(err)
	}
	if len(removal) != 3 {
		t.Fatalf("expected one removal per installed harness, got %d", len(removal))
	}
	if err := Apply(removal); err != nil {
		t.Fatal(err)
	}

	// Setup created the Codex file, so removing our hooks removes it.
	if _, err := os.Stat(filepath.Join(home, ".codex", "hooks.json")); !os.IsNotExist(err) {
		t.Fatalf("the hook file setup created is still there: %v", err)
	}
	for _, path := range []string{claudePath, cursorPath} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if strings.Contains(string(b), Owner) || strings.Contains(string(b), "_hook") {
			t.Fatalf("%s still contains our handler:\n%s", path, b)
		}
	}
	claude, _ := os.ReadFile(claudePath)
	if !strings.Contains(string(claude), `"say done"`) || !strings.Contains(string(claude), `"Read"`) {
		t.Fatalf("unrelated claude hook or settings lost:\n%s", claude)
	}
	if strings.Contains(string(claude), `"SessionStart"`) {
		t.Fatalf("an event that only held our handler should be dropped:\n%s", claude)
	}
	cursor, _ := os.ReadFile(cursorPath)
	if !strings.Contains(string(cursor), `"echo unrelated"`) || !strings.Contains(string(cursor), `"version":1`) {
		t.Fatalf("unrelated cursor hook or version lost:\n%s", cursor)
	}

	// A second removal finds nothing of ours and plans no rewrite at all.
	again, err := PlanRemoval(testFiles(home), Hook{Ports: testPorts}, []string{"claude", "codex", "cursor"})
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("expected no changes once our entries are gone, got %d", len(again))
	}
}

func TestPlanRemovalSkipsMissingAndUnrelatedFiles(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".codex", "hooks.json")
	must(t, os.MkdirAll(filepath.Dir(path), 0700))
	unrelated := []byte(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"say done"}]}]}}`)
	must(t, os.WriteFile(path, unrelated, 0600))
	plan, err := PlanRemoval(testFiles(home), Hook{Ports: testPorts}, []string{"codex", "claude", "cursor"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 0 {
		t.Fatalf("expected nothing to remove, got %d changes", len(plan))
	}
	b, _ := os.ReadFile(path)
	if string(b) != string(unrelated) {
		t.Fatal("planning a removal must not touch the file")
	}
}

func TestInstalledChecksCommandsForEveryHarness(t *testing.T) {
	for _, app := range []string{"codex", "claude", "cursor"} {
		t.Run(app, func(t *testing.T) {
			home := t.TempDir()
			plan, err := Plan(testFiles(home), testHook("/Applications/agent-archive"), []string{app})
			if err != nil {
				t.Fatal(err)
			}
			if err := Apply(plan); err != nil {
				t.Fatal(err)
			}
			ok, err := Installed(testFiles(home), testHook("/Applications/agent-archive"), app)
			if err != nil || !ok {
				t.Fatalf("installed=%v err=%v", ok, err)
			}
			wrong := strings.ReplaceAll(string(plan[0].After), " _hook --harness ", " wrong-command --harness ")
			if err := os.WriteFile(plan[0].Path, []byte(wrong), 0600); err != nil {
				t.Fatal(err)
			}
			ok, err = Installed(testFiles(home), testHook("/Applications/agent-archive"), app)
			if err != nil || ok {
				t.Fatalf("broken installed=%v err=%v", ok, err)
			}
		})
	}
}

// A settings file managed by a dotfile manager (stow, chezmoi) is a symlink
// into a repository. Installing, rolling back, and uninstalling must all
// write through the link: the link survives and the repository copy is the
// one that changes, keeping its own mode.
func TestHookFilesAreWrittenThroughSymlinks(t *testing.T) {
	home := t.TempDir()
	dotfiles := filepath.Join(home, "dotfiles")
	must(t, os.MkdirAll(dotfiles, 0700))
	must(t, os.MkdirAll(filepath.Join(home, ".claude"), 0700))
	target := filepath.Join(dotfiles, "settings.json")
	original := []byte("{\n  \"model\": \"opus\"\n}\n")
	if err := os.WriteFile(target, original, 0644); err != nil {
		t.Fatal(err)
	}
	must(t, os.Chmod(target, 0644))
	initialInfo, err := os.Stat(target)
	must(t, err)
	if initialInfo.Mode().Perm() != 0644 {
		t.Fatalf("initial target mode = %04o, want 0644", initialInfo.Mode().Perm())
	}
	link := filepath.Join(home, ".claude", "settings.json")
	if err := os.Symlink("../dotfiles/settings.json", link); err != nil {
		t.Fatal(err)
	}
	stillLinked := func(step string) {
		t.Helper()
		info, err := os.Lstat(link)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%s replaced the symlink with a plain file", step)
		}
		if info, _ := os.Stat(target); info.Mode().Perm() != 0644 {
			t.Fatalf("%s changed the target's mode to %v", step, info.Mode().Perm())
		}
		entries, _ := os.ReadDir(dotfiles)
		if len(entries) != 1 {
			t.Fatalf("%s left temporary files beside the target: %v", step, entries)
		}
	}
	plan, err := Plan(testFiles(home), testHook("/usr/local/bin/agent-archive"), []string{"claude"})
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(plan); err != nil {
		t.Fatal(err)
	}
	stillLinked("Apply")
	if b, _ := os.ReadFile(target); !strings.Contains(string(b), Owner) {
		t.Fatalf("the link's target was not updated:\n%s", b)
	}
	if err := Rollback(plan); err != nil {
		t.Fatal(err)
	}
	stillLinked("Rollback")
	if b, _ := os.ReadFile(target); string(b) != string(original) {
		t.Fatalf("rollback did not restore the target:\n%s", b)
	}
	if err := Apply(plan); err != nil {
		t.Fatal(err)
	}
	removal, err := PlanRemoval(testFiles(home), Hook{Ports: testPorts}, []string{"claude"})
	if err != nil || len(removal) != 1 {
		t.Fatalf("removal %d changes, err %v", len(removal), err)
	}
	if err := Apply(removal); err != nil {
		t.Fatal(err)
	}
	stillLinked("uninstall")
	if b, _ := os.ReadFile(target); string(b) != string(original) {
		t.Fatalf("uninstall did not restore the target:\n%s", b)
	}
}

// A new hook file created through a symlink that names a missing file is
// created at the link's target, and rollback removes it there.
func TestNewHookFileThroughDanglingSymlink(t *testing.T) {
	home := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(home, ".codex"), 0700))
	target := filepath.Join(home, "dotfiles", "codex-hooks.json")
	must(t, os.MkdirAll(filepath.Dir(target), 0700))
	link := filepath.Join(home, ".codex", "hooks.json")
	must(t, os.Symlink(target, link))
	plan, err := Plan(testFiles(home), testHook("/usr/local/bin/agent-archive"), []string{"codex"})
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(plan); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("link replaced")
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("target not created: %v", err)
	}
	if err := Rollback(plan); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatal("rollback removed the link itself")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("rollback left the created target")
	}
}

// Status must not report hooks as broken because the user (or the app) put
// their own handler before or after ours, while still catching a duplicate
// of ours or a stale one in an event we no longer use.
func TestInstalledIgnoresWhereOurHandlerSits(t *testing.T) {
	exe := "/usr/local/bin/agent-archive"
	for _, app := range []string{"claude", "codex", "cursor"} {
		t.Run(app, func(t *testing.T) {
			home := t.TempDir()
			files := testFiles(home)
			plan, _ := Plan(files, testHook(exe), []string{app})
			if err := Apply(plan); err != nil {
				t.Fatal(err)
			}
			stop := "Stop"
			user := `{"hooks":[{"type":"command","command":"say done"}]}`
			if app == "cursor" {
				stop, user = "stop", `{"command":"say done"}`
			}
			edit := func(mutate func(root map[string]any, hs map[string]any)) {
				t.Helper()
				var root map[string]any
				b, _ := os.ReadFile(files[app])
				must(t, json.Unmarshal(b, &root))
				mutate(root, root["hooks"].(map[string]any))
				b, _ = json.MarshalIndent(root, "", "\t")
				must(t, os.WriteFile(files[app], b, 0600))
			}
			var entry any
			must(t, json.Unmarshal([]byte(user), &entry))
			edit(func(_, hs map[string]any) { hs[stop] = append(hs[stop].([]any), entry) })
			if ok, err := Installed(files, testHook(exe), app); !ok || err != nil {
				t.Fatalf("user handler after ours: installed=%v err=%v", ok, err)
			}
			edit(func(_, hs map[string]any) {
				list := hs[stop].([]any)
				hs[stop] = append([]any{list[len(list)-1]}, list[:len(list)-1]...)
			})
			if ok, err := Installed(files, testHook(exe), app); !ok || err != nil {
				t.Fatalf("user handler before ours: installed=%v err=%v", ok, err)
			}
			if ok, _ := Installed(files, Hook{Ports: testPorts, Executable: exe, DataHome: "/elsewhere"}, app); ok {
				t.Fatal("a command without the data directory counts as installed")
			}
			edit(func(_, hs map[string]any) {
				list := hs[stop].([]any)
				hs["PreToolUse"] = []any{list[len(list)-1]}
			})
			if ok, _ := Installed(files, testHook(exe), app); ok {
				t.Fatal("our handler in an event setup does not use counts as installed")
			}
			edit(func(_, hs map[string]any) {
				delete(hs, "PreToolUse")
				list := hs[stop].([]any)
				hs[stop] = append(list, list[len(list)-1])
			})
			if ok, _ := Installed(files, testHook(exe), app); ok {
				t.Fatal("a duplicate of our handler counts as installed")
			}
		})
	}
}

func TestResolveFilesFollowsTheAppsConfigDirectories(t *testing.T) {
	env := map[string]string{"CLAUDE_CONFIG_DIR": "/cfg/claude", "CODEX_HOME": "/cfg/codex"}
	files := ResolveFiles("/Users/u", func(k string) (string, bool) { v, ok := env[k]; return v, ok }, testPorts)
	want := Files{"claude": "/cfg/claude/settings.json", "codex": "/cfg/codex/hooks.json", "cursor": "/Users/u/.cursor/hooks.json"}
	for app, path := range want {
		if files[app] != path {
			t.Errorf("%s: %s, want %s", app, files[app], path)
		}
	}
	files = testFiles("/Users/u")
	if files["claude"] != "/Users/u/.claude/settings.json" || files["codex"] != "/Users/u/.codex/hooks.json" {
		t.Errorf("defaults: %v", files)
	}
}

// Apply names the file a concurrent edit touched, and callers can tell the
// case apart to say which command to rerun.
func TestConcurrentEditIsReportedWithItsPath(t *testing.T) {
	home := t.TempDir()
	plan, _ := Plan(testFiles(home), testHook("/bin/agent-archive"), []string{"codex"})
	must(t, os.MkdirAll(filepath.Dir(plan[0].Path), 0700))
	must(t, os.WriteFile(plan[0].Path, []byte(`{"changed":true}`), 0600))
	err := Apply(plan)
	if !errors.Is(err, ErrChanged) || !strings.Contains(err.Error(), plan[0].Path) || strings.Contains(err.Error(), "setup") {
		t.Fatalf("error %v", err)
	}
}

// must fails the test on a fixture setup error.
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// A relative link is resolved from the directory it really sits in. With
// ~/.claude itself a link into a dotfiles repository, settings.json ->
// ../shared/settings.json names dotfiles/shared/settings.json, not
// ~/shared/settings.json.
//
// Regression: hook ownership review, 2026-09 (1a9420b).
func TestRelativeLinkInsideALinkedDirectory(t *testing.T) {
	home := t.TempDir()
	dotfiles := filepath.Join(home, "dotfiles")
	must(t, os.MkdirAll(filepath.Join(dotfiles, "claude"), 0700))
	must(t, os.MkdirAll(filepath.Join(dotfiles, "shared"), 0700))
	target := filepath.Join(dotfiles, "shared", "settings.json")
	original := []byte("{\n  \"model\": \"opus\"\n}\n")
	must(t, os.WriteFile(target, original, 0600))
	if err := os.Symlink("../shared/settings.json", filepath.Join(dotfiles, "claude", "settings.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dotfiles, "claude"), filepath.Join(home, ".claude")); err != nil {
		t.Fatal(err)
	}
	hook := testHook("/usr/local/bin/agent-archive")
	plan, err := Plan(testFiles(home), hook, []string{"claude"})
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(plan); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "shared")); !os.IsNotExist(err) {
		t.Fatal("a stray file was written beside the linked directory")
	}
	if ok, err := Installed(testFiles(home), hook, "claude"); !ok || err != nil {
		t.Fatalf("installed=%v err=%v", ok, err)
	}
	if err := Rollback(plan); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != string(original) {
		t.Fatalf("rollback did not restore the target:\n%s", b)
	}
}
