package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/nativesessions"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/termlaunch"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type nativeFixture struct {
	env    Env
	data   string
	cwd    string
	claude string
	codex  string
	at     time.Time
}

func newNativeFixture(t *testing.T) *nativeFixture {
	t.Helper()
	f := &nativeFixture{data: filepath.Join(t.TempDir(), "absent"), cwd: t.TempDir(), claude: t.TempDir(), codex: t.TempDir(), at: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	f.env = testEnv(t, f.data, f.at)
	f.env.WorkingDir = func() (string, error) { return f.cwd, nil }
	f.env.currentBranch = func(string) string { return "main" }
	f.env.nativeStoreRoots = []nativesessions.StoreRoot{{Harness: "claude", Depth: 1, Suffix: ".jsonl", Path: f.claude}, {Harness: "codex", Prefix: "rollout-", Suffix: ".jsonl", Path: f.codex, Recursive: true}}
	f.env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		t.Fatal("native route opened storage/credentials")
		return nil, errors.New("forbidden")
	}
	return f
}

func (f *nativeFixture) add(t *testing.T, harness, id, prompt string, age time.Duration) string {
	t.Helper()
	dir := f.codex
	name := "rollout-2026-10-01-" + id + ".jsonl"
	var lines []map[string]any
	if harness == "claude" {
		dir = filepath.Join(f.claude, "project")
		name = id + ".jsonl"
		lines = []map[string]any{{"type": "user", "sessionId": id, "cwd": f.cwd, "gitBranch": "topic", "timestamp": f.at.Add(-age).Format(time.RFC3339), "message": map[string]any{"role": "user", "content": prompt}}}
	} else {
		lines = []map[string]any{{"type": "session_meta", "timestamp": f.at.Add(-age).Format(time.RFC3339), "payload": map[string]any{"id": id, "cwd": f.cwd}}, {"type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": prompt}}}}}
	}
	if e := os.MkdirAll(dir, 0o700); e != nil {
		t.Fatal(e)
	}
	var raw []byte
	for _, line := range lines {
		b, e := json.Marshal(line)
		if e != nil {
			t.Fatal(e)
		}
		raw = append(raw, b...)
		raw = append(raw, '\n')
	}
	path := filepath.Join(dir, name)
	if e := os.WriteFile(path, raw, 0o600); e != nil {
		t.Fatal(e)
	}
	at := f.at.Add(-age)
	if e := os.Chtimes(path, at, at); e != nil {
		t.Fatal(e)
	}
	return path
}

func TestNativeHandoffIdentityWordsAndLatestWithoutArchive(t *testing.T) {
	t.Parallel()
	for _, harness := range []string{"claude", "codex"} {
		t.Run(harness, func(t *testing.T) {
			t.Parallel()
			f := newNativeFixture(t)
			id := "12345678-1234-1234-1234-123456789012"
			f.add(t, harness, id, "Fix OAuth callback", time.Hour)
			for _, args := range [][]string{{id, "--harness", harness}, {id[:8], "--harness", harness}, {"OAuth", "--harness", harness}, {"--latest", "--harness", harness}} {
				out, errOut, code := runHandoff(t, f.env, args...)
				if code != 0 || !strings.Contains(out, "OAuth callback") || !strings.Contains(errOut, "archiving is not configured") {
					t.Fatalf("args=%v code=%d out=%s err=%s", args, code, out, errOut)
				}
			}
			if _, e := os.Stat(f.data); !errors.Is(e, os.ErrNotExist) {
				t.Fatalf("created archive data: %v", e)
			}
		})
	}
}

func TestNativeIdentityBypassesInitialPreviewWindow(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t)
	for i := range 75 {
		f.add(t, "claude", fmt.Sprintf("native-%04d", i), fmt.Sprintf("work item %d", i), time.Duration(i+1)*time.Hour)
	}
	out, errOut, code := runHandoff(t, f.env, "native-0074", "--harness", "claude")
	if code != 0 || !strings.Contains(out, "work item 74") || strings.Contains(errOut, "labels inspected") {
		t.Fatalf("identity reads previews: %d %s %s", code, out, errOut)
	}
	out, errOut, code = runHandoff(t, f.env, "work item 74", "--harness", "claude")
	if code == 0 || !strings.Contains(errOut, "50 loaded local previews") || !strings.Contains(errOut, "not exhaustive") || out != "" {
		t.Fatalf("word query claims exhaustiveness: %d %s %s", code, out, errOut)
	}
}

func TestNativeCurrentUsesExactIdentityAndLatestSkipsIt(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t)
	id := "native-current"
	f.add(t, "claude", id, "Current work", time.Minute)
	f.add(t, "claude", "native-older", "Older work", time.Hour)
	f.env.LookupEnv = agentEnv(map[string]string{"CLAUDE_CODE_SESSION_ID": id[:8]})
	_, errOut, code := runHandoff(t, f.env, "--to", "codex")
	if code == 0 || !strings.Contains(errOut, "could not") {
		t.Fatalf("guessed current: %d %s", code, errOut)
	}
	f.env.LookupEnv = agentEnv(map[string]string{"CLAUDE_CODE_SESSION_ID": id})
	out, errOut, code := runHandoff(t, f.env, "--latest", "--harness", "claude")
	if code != 0 || !strings.Contains(out, "Older work") {
		t.Fatalf("latest did not skip current: %d %s %s", code, out, errOut)
	}
}

func TestNativeHandoffRefusesArchiveIncompleteSetupAndBadConfig(t *testing.T) {
	t.Parallel()
	for _, name := range []nativeSetupCase{nativeSetupArchive, nativeSetupDraft, nativeSetupJournal, nativeSetupBadConfig, nativeSetupConfigured} {
		t.Run(string(name), func(t *testing.T) {
			t.Parallel()
			f := newNativeFixture(t)
			f.add(t, "claude", "native-session", "Private native work", time.Hour)
			args := []string{"native-session"}
			switch name {
			case nativeSetupArchive:
				args = append(args, "--source", "archive")
			case nativeSetupDraft, nativeSetupJournal, nativeSetupBadConfig, nativeSetupConfigured:
				if e := os.MkdirAll(f.data, 0o700); e != nil {
					t.Fatal(e)
				}
				file, content := "setup-draft.json", "{}"
				if name == nativeSetupJournal {
					file = "setup-transaction.json"
				}
				if name == nativeSetupBadConfig {
					file, content = "config.json", "bad"
				}
				if name == nativeSetupConfigured {
					file = "config.json"
					f.env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
						return nil, errors.New("configured storage unavailable")
					}
				}
				if e := os.WriteFile(filepath.Join(f.data, file), []byte(content), 0o600); e != nil {
					t.Fatal(e)
				}
			}
			out, errOut, code := runHandoff(t, f.env, args...)
			if code == 0 || strings.Contains(out, "Private native work") || strings.Contains(errOut, "Searching Claude") {
				t.Fatalf("unexpected native fallback %d %s %s", code, out, errOut)
			}
		})
	}
}

func TestNativeScopedMissDoesNotBroadenAndDuplicateFails(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t)
	f.add(t, "claude", "native-session", "Elsewhere task", time.Hour)
	f.env.WorkingDir = func() (string, error) { return t.TempDir(), nil }
	_, errOut, code := runHandoff(t, f.env, "Elsewhere")
	if code == 0 || !strings.Contains(errOut, "--all-projects") {
		t.Fatalf("scope miss %d %s", code, errOut)
	}
	out, errOut, code := runHandoff(t, f.env, "Elsewhere", "--all-projects")
	if code != 0 || !strings.Contains(out, "Elsewhere task") {
		t.Fatalf("explicit all %d %s %s", code, out, errOut)
	}
	f.env.WorkingDir = func() (string, error) { return f.cwd, nil }
	path := f.add(t, "claude", "duplicate-id", "Duplicate task", time.Hour)
	other := filepath.Join(f.claude, "other")
	if e := os.Mkdir(other, 0o700); e != nil {
		t.Fatal(e)
	}
	raw, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(other, "duplicate-id.jsonl"), raw, 0o600); e != nil {
		t.Fatal(e)
	}
	_, errOut, code = runHandoff(t, f.env, "duplicate-id", "--harness", "claude")
	if code == 0 || !strings.Contains(errOut, "conflicts") || !strings.Contains(errOut, "--file") {
		t.Fatalf("duplicate picked: %d %s", code, errOut)
	}
}

func TestNativePickerLoadsOlderOnlyOnExplicitAction(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t)
	for i := range 53 {
		f.add(t, "claude", fmt.Sprintf("native-%04d", i), fmt.Sprintf("task %d", i), time.Duration(i+1)*time.Hour)
	}
	f.env.IsTerminal = func(any) bool { return true }
	var out, errOut bytes.Buffer
	code := Run([]string{"handoff", "--no-preamble"}, strings.NewReader("o\n53\n"), &out, &errOut, f.env)
	if code != 0 || !strings.Contains(out.String(), "task 52") || !strings.Contains(out.String(), "Load older sessions") || !strings.Contains(errOut.String(), "53 of 53") {
		t.Fatalf("older picker %d %s %s", code, out.String(), errOut.String())
	}
}

func TestNativeLaunchRecipesPrivateFilesAndActiveCheckoutWarning(t *testing.T) {
	t.Parallel()
	for _, harness := range []string{"claude", "codex"} {
		t.Run(harness, func(t *testing.T) {
			t.Parallel()
			f := newNativeFixture(t)
			id := "12345678-1234-1234-1234-123456789012"
			f.add(t, harness, id, "Continue safe work", time.Minute)
			if e := os.Mkdir(f.data, 0o700); e != nil {
				t.Fatal(e)
			}
			f.env.Executable = func() (string, error) { return "/usr/bin/agent-archive", nil }
			f.env.LookPath = func(name string) (string, error) { return "/usr/bin/" + name, nil }
			var file string
			f.env.OpenTerminal = func(spec termlaunch.Spec) (string, error) {
				file = filepath.Join(spec.ScriptDir, launchHandoffName)
				return "fake terminal", nil
			}
			dest := "codex"
			if harness == "codex" {
				dest = "claude"
			}
			_, errOut, code := runHandoff(t, f.env, id, "--harness", harness, "--to", dest)
			if code != 0 || !strings.Contains(errOut, "both agents can edit") {
				t.Fatalf("launch %d %s", code, errOut)
			}
			raw, e := os.ReadFile(file)
			if e != nil {
				t.Fatal(e)
			}
			if strings.Contains(string(raw), "agent-archive show") || !strings.Contains(string(raw), "--source local --max-bytes 0") || !strings.Contains(string(raw), id) {
				t.Fatalf("wrong recipe %s", raw)
			}
			for path, mode := range map[string]os.FileMode{file: 0o600, filepath.Dir(file): 0o700} {
				info, e := os.Stat(path)
				if e != nil || info.Mode().Perm() != mode {
					t.Fatalf("private permissions %s %v", path, e)
				}
			}
			entries, e := os.ReadDir(f.data)
			if e != nil || len(entries) != 0 {
				t.Fatalf("unconfigured data home writes %v %v", entries, e)
			}
		})
	}
}

func TestNativeSelectedTranscriptReplacementAndIdentityRewriteFail(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t)
	path := f.add(t, "claude", "native-source", "Original work", time.Hour)
	files := f.env.nativeFiles()
	r, e := nativesessions.Discover(context.Background(), productionAgents, files, f.env.nativeStoreRoots, nativesessions.Scope{Directories: []string{f.cwd}}, nativesessions.Limits{Files: 100, HeaderBytes: nativeWindowBytes, RecordBytes: nativeWindowBytes, TotalBytes: nativeReadBudget, Workers: 2})
	if e != nil || len(r.Candidates) != 1 {
		t.Fatalf("discover %v %v", r, e)
	}
	c := r.Candidates[0]
	raw, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	if e := os.Rename(path, path+".original"); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(path, raw, 0o600); e != nil {
		t.Fatal(e)
	}
	if e := os.Chtimes(path, c.ModifiedAt, c.ModifiedAt); e != nil {
		t.Fatal(e)
	}
	if _, e := handoffFromNative(context.Background(), c, files, f.env); e == nil {
		t.Fatal("replacement accepted")
	}
}

func TestNativeTempCleanupOnlyOwnedOldPrivateDirectories(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t)
	root, e := nativeTempRoot(f.env.tempDir())
	if e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"launch-old", "launch-fresh", "unrelated"} {
		dir := filepath.Join(root, name)
		if e := os.Mkdir(dir, 0o700); e != nil {
			t.Fatal(e)
		}
		if name != "launch-fresh" {
			old := f.at.Add(-8 * 24 * time.Hour)
			if e := os.Chtimes(dir, old, old); e != nil {
				t.Fatal(e)
			}
		}
	}
	outside := t.TempDir()
	if e := os.Symlink(outside, filepath.Join(root, "launch-link")); e != nil {
		t.Fatal(e)
	}
	pruneNativeHandoffs(f.env.tempDir(), f.at)
	if _, e := os.Stat(filepath.Join(root, "launch-old")); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("old not removed")
	}
	for _, path := range []string{filepath.Join(root, "launch-fresh"), filepath.Join(root, "unrelated"), outside} {
		if _, e := os.Stat(path); e != nil {
			t.Fatal(e)
		}
	}
}

func TestHandoffLoadsConfigOnceAcrossResolutionAndLaunch(t *testing.T) {
	t.Parallel()
	for _, configured := range []bool{false, true} {
		t.Run(strconv.FormatBool(configured), func(t *testing.T) {
			t.Parallel()
			f := newNativeFixture(t)
			id := "native-source"
			f.add(t, "claude", id, "Widget work", time.Hour)
			env := f.env
			if configured {
				old := newHandoffFixture(t, false)
				env = old.env
				id = old.id
			}
			calls := 0
			env.handoffConfigLoad = func(home string) (config.Config, bool, error) { calls++; return config.Load(home) }
			env.Executable = func() (string, error) { return "/usr/bin/agent-archive", nil }
			env.LookPath = func(name string) (string, error) { return "/usr/bin/" + name, nil }
			env.OpenTerminal = func(termlaunch.Spec) (string, error) { return "fake terminal", nil }
			_, errOut, code := runHandoff(t, env, id, "--to", "codex")
			if code != 0 || calls != 1 {
				t.Fatalf("configured=%v code=%d configloads=%d stderr=%s", configured, code, calls, errOut)
			}
		})
	}
}

func TestNativeLatestExcludesKnownSubagentRollouts(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t)
	id := "12345678-1234-1234-1234-123456789012"
	f.add(t, "codex", id, "Top-level work", time.Hour)
	sub := f.add(t, "codex", "87654321-1234-1234-1234-123456789012", "Subagent work", time.Minute)
	raw, err := os.ReadFile(sub)
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.Replace(raw, []byte(`"payload":{"cwd"`), []byte(`"payload":{"source":{"subagent":{"agent_spawn":{}}},"cwd"`), 1)
	if err := os.WriteFile(sub, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := runHandoff(t, f.env, "--latest", "--harness", "codex")
	if code != 0 || !strings.Contains(out, "Top-level work") || strings.Contains(out, "Subagent work") {
		t.Fatalf("subagent latest code=%d out=%s stderr=%s", code, out, errOut)
	}
}

func TestNativeSelectorNeedsOnlyConsumerOwnedReadCapabilities(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t)
	f.add(t, "claude", "native-source", "Widget work", time.Hour)
	deps := nativeOnlyDependencies{sessionBrowserDependencies: f.env, files: f.env.nativeFiles(), roots: f.env.nativeStoreRoots, cwd: f.cwd, temp: f.env.tempDir(), nowValue: f.at}
	var out, errOut bytes.Buffer
	target, picked, code := resolveNativeHandoff(handoffOptions{sessionID: "native-source", harness: "claude", source: "local"}, false, newTypedInput(strings.NewReader("")), &out, &errOut, deps)
	if code != 0 || !picked || target.native == nil || target.native.NativeID != "native-source" {
		t.Fatalf("narrow dependencies selection code=%d picked=%v stderr=%s", code, picked, errOut.String())
	}
}

func TestNativeSelectedSnapshotIncludesNewCompleteAppendAndRejectsIdentityChange(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t)
	path := f.add(t, "claude", "native-source", "Original work", time.Hour)
	files := f.env.nativeFiles()
	r, err := nativesessions.Discover(context.Background(), productionAgents, files, f.env.nativeStoreRoots, nativesessions.Scope{Directories: []string{f.cwd}}, nativesessions.Limits{Files: 100, HeaderBytes: nativeWindowBytes, RecordBytes: nativeWindowBytes, TotalBytes: nativeReadBudget, Workers: 2})
	if err != nil || len(r.Candidates) != 1 {
		t.Fatalf("discover %+v %v", r, err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte(`{"type":"assistant","message":{"role":"assistant","content":"Appended complete reply"}}` + "\n")); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	target, err := handoffFromNative(context.Background(), r.Candidates[0], files, f.env)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, record := range target.bundle.NativeRecords {
		encoded, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(encoded, []byte("Appended complete reply")) {
			found = true
		}
	}
	if !found {
		t.Fatal("selected snapshot omitted earlier append")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.ReplaceAll(raw, []byte(`"sessionId":"native-source"`), []byte(`"sessionId":"native-other"`))
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := handoffFromNative(context.Background(), r.Candidates[0], files, f.env); err == nil {
		t.Fatal("changed native identity accepted")
	}
}

// The native selector's interface deliberately offers no store/capture capability.
type nativeOnlyDependencies struct {
	sessionBrowserDependencies
	files    nativesessions.FileSystem
	roots    []nativesessions.StoreRoot
	cwd      string
	temp     string
	nowValue time.Time
}

func (n nativeOnlyDependencies) nativeFiles() nativesessions.FileSystem { return n.files }

func (n nativeOnlyDependencies) nativeRoots(string) ([]nativesessions.StoreRoot, error) {
	return n.roots, nil
}

func (n nativeOnlyDependencies) workingDir() (string, error) { return n.cwd, nil }

func (n nativeOnlyDependencies) lookupEnv(string) (string, bool) { return "", false }

func (n nativeOnlyDependencies) now() time.Time { return n.nowValue }

func (n nativeOnlyDependencies) tempDir() string { return n.temp }

var _ nativeHandoffDependencies = nativeOnlyDependencies{}

type nativeSetupCase string

const (
	nativeSetupArchive    nativeSetupCase = "archive"
	nativeSetupDraft      nativeSetupCase = "draft"
	nativeSetupJournal    nativeSetupCase = "journal"
	nativeSetupBadConfig  nativeSetupCase = "bad-config"
	nativeSetupConfigured nativeSetupCase = "configured"
)

func TestNativeRecipeResolvesOriginalCheckoutAndQuotesShellWords(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t)
	f.cwd = filepath.Join(f.cwd, "source ' $(`touch injected`) checkout")
	if err := os.Mkdir(f.cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	id := "12345678-1234-1234-1234-123456789012"
	f.add(t, "claude", id, "Original source evidence", time.Hour)
	destination := t.TempDir()
	f.env.WorkingDir = func() (string, error) { return destination, nil }
	candidate := nativesessions.Candidate{NativeID: id, Directory: f.cwd, Ref: nativesessions.Ref{Harness: "claude"}}
	prompt := launchHandoffPrompt("record", archive.Handoff{}, handoffTarget{native: &candidate}, "agent-archive")
	args := nativeRecipeArguments(t, prompt, destination)
	out, errOut, code := runHandoff(t, f.env, args[2:]...)
	if code != 0 || !strings.Contains(out, "Original source evidence") {
		t.Fatalf("recipe failed %d %s %s", code, out, errOut)
	}
	candidate.NativeID = "hostile'$(touch injected)`touch injected`"
	args = nativeRecipeArguments(t, launchHandoffPrompt("record", archive.Handoff{}, handoffTarget{native: &candidate}, "agent-archive"), destination)
	if args[2] != candidate.NativeID || args[len(args)-1] != f.cwd {
		t.Fatalf("shell altered words %q", args)
	}
	if _, err := os.Stat(filepath.Join(destination, "injected")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("shell executed recipe input: %v", err)
	}
}

func nativeRecipeArguments(t *testing.T, prompt, destination string) []string {
	t.Helper()
	_, rest, ok := strings.Cut(prompt, "run agent-archive ")
	if !ok {
		t.Fatal("missing retrieval recipe")
	}
	recipe, _, ok := strings.Cut(rest, ". Native discovery")
	if !ok {
		t.Fatal("missing retrieval recipe boundary")
	}
	cmd := exec.CommandContext(context.Background(), "sh", "-c", "set -- agent-archive "+recipe+"; printf '%s\\0' \"$@\"")
	cmd.Dir = destination
	raw, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
}

// A later complete record cannot change the selected native identity.
// Regression: 2026-10 review BH-01.
func TestNativeFullReadRejectsIdentityConflictBeyondHeader(t *testing.T) {
	t.Parallel()
	for _, harness := range []string{"claude", "codex"} {
		t.Run(harness, func(t *testing.T) {
			t.Parallel()
			f := newNativeFixture(t)
			id := "12345678-1234-1234-1234-123456789012"
			path := f.add(t, harness, id, "Selected work", time.Hour)
			file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			padding := `{"type":"assistant","message":{"role":"assistant","content":"` + strings.Repeat("x", int(nativeWindowBytes)) + `"}}` + "\n"
			conflict := `{"type":"user","sessionId":"different-session","message":{"role":"user","content":"Unrelated work"}}` + "\n"
			if harness == "codex" {
				conflict = `{"type":"session_meta","payload":{"id":"different-session","cwd":"/other"}}` + "\n"
			}
			if _, err := file.WriteString(padding + conflict); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			out, errOut, code := runHandoff(t, f.env, id, "--harness", harness)
			if code == 0 || out != "" || !strings.Contains(errOut, "identity") {
				t.Fatalf("conflicting identity accepted: code=%d out=%s stderr=%s", code, out, errOut)
			}
		})
	}
}

// Partial labels must be visible without reading the uninspected middle.
// Regression: 2026-10 review BH-02.
func TestNativePickerReportsPartialPreviewLabels(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t)
	path := f.add(t, "claude", "native-source", "Selected work", time.Hour)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(strings.Repeat("x", int(3*nativeWindowBytes)) + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	f.env.IsTerminal = func(any) bool { return true }
	var out, errOut bytes.Buffer
	code := Run([]string{"handoff", "--no-preamble"}, strings.NewReader("q\n"), &out, &errOut, f.env)
	if code != 0 || !strings.Contains(out.String(), "preview partial") || !strings.Contains(errOut.String(), "1 partial") {
		t.Fatalf("partial label unreported: code=%d out=%s stderr=%s", code, out.String(), errOut.String())
	}
}

// A directory alias must select the same canonical checkout as its target.
// Regression: 2026-10 review BH-03.
func TestNativeProjectAcceptsSymlinkToCheckout(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t)
	f.add(t, "claude", "native-source", "Canonical checkout work", time.Hour)
	alias := filepath.Join(t.TempDir(), "checkout-alias")
	if err := os.Symlink(f.cwd, alias); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := runHandoff(t, f.env, "native-source", "--project", alias)
	if code != 0 || !strings.Contains(out, "Canonical checkout work") {
		t.Fatalf("directory alias rejected: code=%d out=%s stderr=%s", code, out, errOut)
	}
}

func (nativeOnlyDependencies) runtimeLookup() agentapi.RuntimeLookup { return productionAgents }

// Words that match several local sessions where nothing can ask print them
// in the piped list's columns, with the whole native ID to name. The picker
// shows the readable title and groups the sessions by day.
func TestNativeLocalMatchesUseTheSessionListWords(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t)
	recent := "11111111-1111-1111-1111-111111111111"
	older := "22222222-2222-2222-2222-222222222222"
	f.add(t, "claude", recent, "# fix the OAuth callback", time.Hour)
	f.add(t, "codex", older, "Retry the OAuth refresh", 30*time.Hour)
	_, errOut, code := runHandoff(t, f.env, "OAuth")
	table := "  WHEN  AGENT   ID                                    TITLE\n" +
		"  1h    claude  " + recent + "  # fix the OAuth callback\n" +
		"  Wed   codex   " + older + "  Retry the OAuth refresh\n"
	if code != 1 || !strings.Contains(errOut, table) || !strings.Contains(errOut, "2 matches in 2 loaded local previews") {
		t.Fatalf("code=%d stderr:\n%s", code, errOut)
	}
	f.env.TerminalSize = fixedTerminal{80, 40}.terminalSize
	out, errOut, code := runPicker(t, f.env, "q\n")
	if code != 0 {
		t.Fatalf("picker code=%d stderr=%s\n%s", code, errOut, out)
	}
	var lines []string
	for line := range strings.SplitSeq(out, "\n") {
		if line = strings.TrimRight(line, " "); strings.HasSuffix(line, "DAY") || strings.Contains(line, "OAuth") {
			lines = append(lines, line)
		}
	}
	want := []string{"TODAY", "1  ● Claude  Fix the OAuth callback", "YESTERDAY", "2  ● Codex   Retry the OAuth refresh"}
	if len(lines) != len(want) {
		t.Fatalf("picker lines %q, want %q:\n%s", lines, want, out)
	}
	for i, line := range lines {
		if !strings.HasPrefix(line, want[i]) {
			t.Fatalf("picker line %q, want it to start %q:\n%s", line, want[i], out)
		}
	}
	if !strings.HasSuffix(lines[1], " 1h") || !strings.HasSuffix(lines[3], " Wed") {
		t.Fatalf("short times: %q\n%s", lines, out)
	}
}
