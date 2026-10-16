package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/nativesessions"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/termlaunch"
	"github.com/wangjohn/agent-archive/internal/testutil/orbifold"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

const museSyntheticID = "00000000-0000-7000-8000-000000000001"

type museFrameKind string

const musePermissionFrame museFrameKind = "session_permission_transaction"

type museNativeMutation string

type museNativeWriteMutation string

const (
	museLeadingBlank      museNativeMutation      = "leading blank"
	museLeadingWhitespace museNativeMutation      = "leading whitespace"
	museSkippedOversized  museNativeMutation      = "skipped oversized"
	museLaterUnknown      museNativeMutation      = "later unknown"
	museLaterFork         museNativeMutation      = "later fork"
	museTruncated         museNativeMutation      = "truncated"
	museAppend            museNativeWriteMutation = "append"
	museTruncate          museNativeWriteMutation = "truncate"
	museReplace           museNativeWriteMutation = "replace"
	museSymlink           museNativeWriteMutation = "symlink"
)

type museFilterCounter struct {
	agentapi.TranscriptFilter
	calls  atomic.Int64
	before func()
}

func (f *museFilterCounter) Filter(ctx context.Context, in agentapi.NativeInput, c agentapi.FilterContext) (archive.FilteredTranscript, error) {
	f.calls.Add(1)
	if f.before != nil {
		f.before()
	}
	return f.TranscriptFilter.Filter(ctx, in, c)
}

func museCountingRegistry(tb testing.TB) (*builtin.Registry, *museFilterCounter) {
	tb.Helper()
	var bindings []builtin.Integration
	var counter *museFilterCounter
	for _, d := range productionAgents.Catalog().All() {
		binding, ok := productionAgents.Lookup(string(d.ID))
		if !ok {
			tb.Fatal("binding")
		}
		binding.Descriptor = agentmeta.Descriptor{ID: d.ID}
		if d.ID == agentmeta.Muse {
			counter = &museFilterCounter{TranscriptFilter: binding.Filter}
			binding.Filter = counter
		}
		bindings = append(bindings, binding)
	}
	r, err := builtin.New(agentmeta.Builtins(), bindings)
	if err != nil {
		tb.Fatal(err)
	}
	return r, counter
}

// museNativeBytes derives synthetic variants and recomputes the native frame
// checksum; production decoding never uses this fixture constructor.
func museNativeBytes(tb testing.TB, lane, id, workspace string) []byte {
	tb.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "agents", "muse", "testdata", "synthetic-"+lane+"-conversation.jsonl"))
	if err != nil {
		tb.Fatal(err)
	}
	workspaceJSON, _ := json.Marshal(workspace)
	raw = bytes.ReplaceAll(raw, []byte(museSyntheticID), []byte(id))
	raw = bytes.ReplaceAll(raw, []byte(`"/work/synthetic-muse"`), workspaceJSON)
	lines := bytes.Split(raw, []byte("\n"))
	var frame struct {
		Kind        museFrameKind `json:"retained_frame"`
		Schema      int           `json:"frame_schema_version"`
		Ordinal     int           `json:"outer_log_ordinal"`
		Transaction string        `json:"transaction_id"`
		Children    []struct {
			Index  int    `json:"child_index"`
			Record string `json:"record_json"`
		} `json:"children"`
		Hash string `json:"content_sha256"`
	}
	if err := json.Unmarshal(lines[0], &frame); err != nil {
		tb.Fatal(err)
	}
	if frame.Kind != musePermissionFrame {
		tb.Fatal("unexpected synthetic frame")
	}
	h := sha256.New()
	h.Write([]byte("muse.session_permission_transaction.v1\x00"))
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], 1)
	h.Write(buf[:])
	transaction, err := hex.DecodeString(strings.ReplaceAll(frame.Transaction, "-", ""))
	if err != nil {
		tb.Fatal(err)
	}
	h.Write(transaction)
	binary.BigEndian.PutUint32(buf[:4], 2)
	h.Write(buf[:4])
	for i, child := range frame.Children {
		binary.BigEndian.PutUint32(buf[:4], uint32(i))
		h.Write(buf[:4])
		binary.BigEndian.PutUint64(buf[:], uint64(len(child.Record)))
		h.Write(buf[:])
		h.Write([]byte(child.Record))
	}
	frame.Hash = "sha256:" + hex.EncodeToString(h.Sum(nil))
	lines[0], err = json.Marshal(frame)
	if err != nil {
		tb.Fatal(err)
	}
	return bytes.Join(lines, []byte("\n"))
}

func museNativeEnv(t *testing.T) (Env, string, string, *museFilterCounter) {
	t.Helper()
	data := t.TempDir()
	workspace := t.TempDir()
	state := filepath.Join(t.TempDir(), "absent")
	env := testEnv(t, state, time.Now())
	env.LookupEnv = agentEnv(map[string]string{"XDG_DATA_HOME": data})
	env.WorkingDir = func() (string, error) { return workspace, nil }
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		t.Fatal("local read opened storage/auth")
		return nil, errors.New("forbidden")
	}
	registry, counter := museCountingRegistry(t)
	env.Agents = registry
	return env, data, workspace, counter
}

func writeMuseNative(tb testing.TB, data, id string, raw []byte) string {
	tb.Helper()
	path := filepath.Join(data, "muse", "sessions", "1999", "01", "01", id, "session.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		tb.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		tb.Fatal(err)
	}
	return path
}

func TestMuseAutomaticNativeHandoffUsesWholeFilterOnceWithoutSetup(t *testing.T) {
	t.Parallel()
	for _, lane := range []string{"exec", "msp"} {
		t.Run(lane, func(t *testing.T) {
			t.Parallel()
			env, data, workspace, counter := museNativeEnv(t)
			writeMuseNative(t, data, museSyntheticID, museNativeBytes(t, lane, museSyntheticID, workspace))
			meter := &nativeReadMeter{}
			env.nativeFS = meter
			for _, query := range []string{museSyntheticID, museSyntheticID[:32], "--latest"} {
				before := counter.calls.Load()
				_, beforeOpens, _, _ := meter.counts()
				out, errOut, code := runHandoff(t, env, query, "--source", "local", "--harness", "muse", "--format", "json")
				if code != 0 || !strings.Contains(out, "Explain the synthetic parser.") || !strings.Contains(out, "2026-10-15T17:04:15.882704Z") || counter.calls.Load() != before+1 {
					t.Fatalf("%s: %d calls=%d %s %s", query, code, counter.calls.Load(), out, errOut)
				}
				_, afterOpens, active, _ := meter.counts()
				if afterOpens-beforeOpens != 3 || active != 0 {
					t.Fatalf("selected opens=%d active=%d, want census+source+verification", afterOpens-beforeOpens, active)
				}
			}
			if _, err := os.Stat(museNativeArchiveHome(t, env)); !os.IsNotExist(err) {
				t.Fatalf("local read wrote state: %v", err)
			}
		})
	}
}

func museNativeArchiveHome(t *testing.T, env Env) string {
	t.Helper()
	home, err := env.Home()
	if err != nil {
		t.Fatal(err)
	}
	return home
}

func TestMuseAutomaticHandoffRefusesStrictInvalidSourcesBeforeArtifacts(t *testing.T) {
	t.Parallel()
	for _, mutation := range []museNativeMutation{museLeadingBlank, museLeadingWhitespace, museSkippedOversized, museLaterUnknown, museLaterFork, museTruncated} {
		t.Run(string(mutation), func(t *testing.T) {
			t.Parallel()
			env, data, workspace, counter := museNativeEnv(t)
			raw := museNativeBytes(t, "exec", museSyntheticID, workspace)
			switch mutation {
			case museLeadingBlank:
				raw = append([]byte("\n"), raw...)
			case museLeadingWhitespace:
				raw = append([]byte(" "), raw...)
			case museSkippedOversized:
				raw = append([]byte(strings.Repeat("x", int(nativeWindowBytes)+1)+"\n"), raw...)
			case museLaterUnknown:
				raw = append(raw, []byte("{}\n")...)
			case museLaterFork:
				raw = append(raw, []byte(`{"record_type":"session.fork.created"}`+"\n")...)
			case museTruncated:
				raw = raw[:len(raw)-1]
			}
			writeMuseNative(t, data, museSyntheticID, raw)
			artifact := filepath.Join(t.TempDir(), "handoff.md")
			out, errOut, code := runHandoff(t, env, museSyntheticID, "--source", "local", "--harness", "muse", "--output", artifact)
			if code == 0 || out != "" {
				t.Fatalf("invalid accepted %d %s %s", code, out, errOut)
			}
			if _, err := os.Stat(artifact); !os.IsNotExist(err) {
				t.Fatalf("artifact created: %v", err)
			}
			if mutation != museSkippedOversized && counter.calls.Load() != 1 {
				t.Fatalf("full filter calls %d", counter.calls.Load())
			}
		})
	}
}

func TestMuseAutomaticIdentityBeyondLabelsAndIncompleteLatest(t *testing.T) {
	t.Parallel()
	env, data, workspace, counter := museNativeEnv(t)
	for i := range 75 {
		id := fmt.Sprintf("00000000-0000-7000-8000-%012d", i+1)
		path := writeMuseNative(t, data, id, museNativeBytes(t, "exec", id, workspace))
		at := time.Unix(2000000000-int64(i), 0)
		must(t, os.Chtimes(path, at, at))
	}
	prefixID := "87654321-0000-7000-8000-000000000001"
	prefixPath := writeMuseNative(t, data, prefixID, museNativeBytes(t, "exec", prefixID, workspace))
	old := time.Unix(1999999900, 0)
	must(t, os.Chtimes(prefixPath, old, old))
	id := "00000000-0000-7000-8000-000000000075"
	out, errOut, code := runHandoff(t, env, id, "--harness", "muse")
	if code != 0 || !strings.Contains(out, "Explain the synthetic parser.") || counter.calls.Load() != 1 {
		t.Fatalf("exact beyond labels %d %s %s", code, out, errOut)
	}
	out, errOut, code = runHandoff(t, env, "87654321", "--harness", "muse")
	if code != 0 || counter.calls.Load() != 2 {
		t.Fatalf("unique prefix beyond labels %d %s %s", code, out, errOut)
	}
	out, errOut, code = runHandoff(t, env, "synthetic parser", "--harness", "muse")
	if code == 0 || out != "" || !strings.Contains(errOut, "50 loaded local previews") || counter.calls.Load() != 2 {
		t.Fatalf("word window %d %s %s", code, out, errOut)
	}
	writeMuseNative(t, data, "00000000-0000-7000-8000-000000000099", []byte("{}\n"))
	out, errOut, code = runHandoff(t, env, "--latest", "--harness", "muse")
	if code == 0 || out != "" || !strings.Contains(errOut, "incomplete") || counter.calls.Load() != 2 {
		t.Fatalf("latest incomplete %d %s %s", code, out, errOut)
	}
}

func TestNativeDataHomePrecedenceIsSingleQualifiedLocation(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	cwd := t.TempDir()
	xdg := t.TempDir()
	for _, tc := range []struct {
		name  string
		value string
		home  string
		cwd   string
		want  string
	}{
		{"absolute precedence", xdg, home, cwd, xdg}, {"trim Unicode", "\u2003" + xdg + "\u3000", home, cwd, xdg}, {"HOME default", "", home, cwd, filepath.Join(home, ".local", "share")}, {"blank default", "\u2003\t", home, cwd, filepath.Join(home, ".local", "share")}, {"relative", "relative-data", home, cwd, filepath.Join(cwd, "relative-data")}, {"relative changed CWD", "relative-data", home, home, filepath.Join(home, "relative-data")}, {"relative invalid CWD", "relative-data", home, "relative", ""}, {"invalid HOME", "", "relative", cwd, ""},
		{"non Unicode HOME", "", string([]byte{47, 255}), cwd, ""},
		{"non Unicode XDG", string([]byte{47, 255}), home, cwd, ""}, {"invalid data", "bad\x00data", home, cwd, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := Env{LookupEnv: agentEnv(map[string]string{"XDG_DATA_HOME": tc.value}), WorkingDir: func() (string, error) { return tc.cwd, nil }, UserHomeDir: func() (string, error) { return tc.home, nil }}
			if got := env.nativeDataHome(tc.home); got != tc.want {
				t.Fatalf("data home %q want %q", got, tc.want)
			}
			roots, err := env.nativeRoots("muse")
			if tc.want == "" {
				if err == nil || len(roots) != 0 {
					t.Fatalf("unqualified roots %+v %v", roots, err)
				}
				return
			}
			if err != nil || len(roots) != 1 || roots[0].Path != filepath.Join(tc.want, "muse", "sessions") {
				t.Fatalf("roots %+v %v", roots, err)
			}
			for _, name := range []string{"claude", "codex"} {
				r, e := env.nativeRoots(name)
				if e != nil || len(r) == 0 {
					t.Fatalf("existing %s roots %+v %v", name, r, e)
				}
			}
		})
	}
}

func TestMuseAutomaticScopePickerAndDuplicates(t *testing.T) {
	t.Parallel()
	env, data, workspace, counter := museNativeEnv(t)
	nested := filepath.Join(workspace, "nested")
	must(t, os.Mkdir(nested, 0700))
	elsewhere := t.TempDir()
	writeMuseNative(t, data, museSyntheticID, museNativeBytes(t, "exec", museSyntheticID, nested))
	otherID := "00000000-0000-7000-8000-000000000002"
	writeMuseNative(t, data, otherID, museNativeBytes(t, "msp", otherID, elsewhere))
	out, errOut, code := runPicker(t, env, "1\np\n", "--harness", "muse", "--source", "local")
	if code != 0 || !strings.Contains(out, "Explain the synthetic parser.") || counter.calls.Load() != 1 {
		t.Fatalf("picker %d %s %s", code, out, errOut)
	}
	out, errOut, code = runHandoff(t, env, otherID, "--harness", "muse")
	if code == 0 || out != "" {
		t.Fatalf("broadened %d %s %s", code, out, errOut)
	}
	for _, scope := range [][]string{{"--project", elsewhere}, {"--all-projects"}} {
		args := append([]string{otherID, "--harness", "muse"}, scope...)
		out, errOut, code = runHandoff(t, env, args...)
		if code != 0 || !strings.Contains(out, "Explain the synthetic parser.") {
			t.Fatalf("scope %d %s %s", code, out, errOut)
		}
	}
	path := filepath.Join(data, "muse", "sessions", "1999", "01", "02", museSyntheticID, "session.jsonl")
	must(t, os.MkdirAll(filepath.Dir(path), 0700))
	must(t, os.WriteFile(path, museNativeBytes(t, "exec", museSyntheticID, nested), 0600))
	out, errOut, code = runHandoff(t, env, museSyntheticID, "--harness", "muse")
	if code == 0 || out != "" || !strings.Contains(errOut, "ambiguous") {
		t.Fatalf("duplicate %d %s %s", code, out, errOut)
	}
}

func TestMuseNativeLabelsReadOnlyBatchesAndZeroBudget(t *testing.T) {
	t.Parallel()
	env, data, workspace, counter := museNativeEnv(t)
	for i := range 75 {
		id := fmt.Sprintf("00000000-0000-7000-8000-%012d", i+1)
		writeMuseNative(t, data, id, museNativeBytes(t, "exec", id, workspace))
	}
	meter := &nativeReadMeter{}
	env.nativeFS = meter
	roots, err := env.nativeRoots("muse")
	must(t, err)
	result, err := nativesessions.Discover(t.Context(), env.agentRegistry(), meter, roots, nativesessions.Scope{Directories: []string{workspace}}, nativesessions.Limits{Files: 10000, HeaderBytes: nativeWindowBytes, RecordBytes: nativeWindowBytes, TotalBytes: nativeReadBudget, Workers: 2})
	must(t, err)
	if !result.Coverage.IdentityComplete || len(result.Candidates) != 75 {
		t.Fatalf("census %+v", result.Coverage)
	}
	catalog := &nativePreviewCatalog{sources: env.agentRegistry(), previews: env.agentRegistry(), ctx: t.Context(), files: meter, candidates: result.Candidates, reserved: result.Coverage.ReservedBytes, stderr: io.Discard, now: time.Now()}
	more, err := catalog.load()
	must(t, err)
	read, opens, active, _ := meter.counts()
	if !more || catalog.next != 50 || active != 0 || counter.calls.Load() != 0 {
		t.Fatalf("first batch %d %d %d", catalog.next, active, counter.calls.Load())
	}
	for range 10 {
		filterRows(catalog.rows, parseSessionQuery("0000"))
		must(t, printSessionTable(io.Discard, catalog.rows, listFormatOptions{Now: time.Now()}))
	}
	afterRead, afterOpens, _, _ := meter.counts()
	if afterRead != read || afterOpens != opens {
		t.Fatal("render/filter read transcript")
	}
	_, err = catalog.load()
	must(t, err)
	afterRead, _, active, _ = meter.counts()
	if catalog.next != 75 || afterRead <= read || active != 0 || catalog.reserved > nativeReadBudget || counter.calls.Load() != 0 {
		t.Fatal("older batch exceeded bounds")
	}
	exhausted := &nativePreviewCatalog{sources: env.agentRegistry(), previews: env.agentRegistry(), ctx: t.Context(), files: meter, candidates: result.Candidates, reserved: nativeReadBudget, stderr: io.Discard, now: time.Now()}
	beforeBytes, beforeOpens, _, _ := meter.counts()
	_, err = exhausted.load()
	must(t, err)
	afterBytes, afterOpen, _, _ := meter.counts()
	if len(exhausted.rows) != 75 || afterBytes != beforeBytes || afterOpen != beforeOpens {
		t.Fatal("zero budget opened source")
	}
}

func TestMuseBackfillRefusesBeforeHomeOrStorage(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"muse", "muse-code"} {
		for _, mode := range []string{"--dry-run", "--yes"} {
			t.Run(name+mode, func(t *testing.T) {
				t.Parallel()
				env := testEnv(t, t.TempDir(), time.Now())
				env.Home = func() (string, error) {
					t.Fatal("unsupported import resolved archive home")
					return "", errors.New("forbidden")
				}
				env.UserHomeDir = func() (string, error) {
					t.Fatal("unsupported import resolved user home")
					return "", errors.New("forbidden")
				}
				env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
					t.Fatal("unsupported import opened storage")
					return nil, errors.New("forbidden")
				}
				var out, errOut bytes.Buffer
				if code := Run([]string{"backfill", mode, "--harness", name}, nil, &out, &errOut, env); code == 0 || !strings.Contains(errOut.String(), "historical import is unavailable for muse") {
					t.Fatalf("unsupported import %d %s %s", code, out.String(), errOut.String())
				}
			})
		}
	}
}

func TestMuseHandoffDiscoveryDoesNotEnterImportProjectsOrRecovery(t *testing.T) {
	t.Parallel()
	env, _, _, _ := museNativeEnv(t)
	bf := env.backfillEnvironment(t.TempDir(), config.Config{})
	if got := strings.Join(bf.Discovery.DiscoveryAgents(), ","); got != "claude,codex,cursor" {
		t.Fatalf("import projection %s", got)
	}
	if _, ok := bf.Imports.LookupImport("muse"); ok {
		t.Fatal("Muse import capability")
	}
	for _, name := range bf.Discovery.DiscoveryAgents() {
		if _, ok := bf.Imports.LookupImport(name); !ok {
			t.Fatalf("missing import %s", name)
		}
	}
	env.Agents = fourthRegistry(t, &orbifold.Ports{})
	extension := env.backfillEnvironment(t.TempDir(), config.Config{})
	if names := strings.Join(extension.Discovery.DiscoveryAgents(), ","); names != "claude,codex,cursor,orbifold" {
		t.Fatalf("extension import omitted: %s", names)
	}
	scoped := selectedDiscoveryApps{DiscoveryLookup: extension.Discovery, apps: []string{"muse", "orbifold"}}
	if names := strings.Join(scoped.DiscoveryAgents(), ","); names != "orbifold" {
		t.Fatalf("setup scope broadened import set: %s", names)
	}
}

func TestMuseSelectedSourceChangesCloseAllHandlesAndCreateNoArtifact(t *testing.T) {
	t.Parallel()
	for _, mutation := range []museNativeWriteMutation{museAppend, museTruncate, museReplace, museSymlink} {
		t.Run(string(mutation), func(t *testing.T) {
			t.Parallel()
			env, data, workspace, counter := museNativeEnv(t)
			raw := museNativeBytes(t, "exec", museSyntheticID, workspace)
			path := writeMuseNative(t, data, museSyntheticID, raw)
			meter := &nativeReadMeter{}
			env.nativeFS = meter
			counter.before = func() {
				switch mutation {
				case museAppend:
					f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
					must(t, err)
					_, err = f.Write([]byte("{}\n"))
					must(t, err)
					must(t, f.Close())
				case museTruncate:
					must(t, os.Truncate(path, 1))
				case museSymlink:
					must(t, os.Rename(path, path+".old"))
					must(t, os.Symlink(path+".old", path))
				case museReplace:
					must(t, os.Rename(path, path+".old"))
					must(t, os.WriteFile(path, raw, 0600))
				}
			}
			artifact := filepath.Join(t.TempDir(), "handoff.md")
			out, errOut, code := runHandoff(t, env, museSyntheticID, "--harness", "muse", "--output", artifact)
			_, _, active, _ := meter.counts()
			if code == 0 || out != "" || active != 0 || counter.calls.Load() != 1 {
				t.Fatalf("changed source accepted %d active=%d filter=%d %s %s", code, active, counter.calls.Load(), out, errOut)
			}
			if _, err := os.Stat(artifact); !os.IsNotExist(err) {
				t.Fatalf("artifact %v", err)
			}
		})
	}
}

func TestMuseAutomaticHandoffSupportedDestinationAndMuseRefusal(t *testing.T) {
	t.Parallel()
	env, data, workspace, counter := museNativeEnv(t)
	writeMuseNative(t, data, museSyntheticID, museNativeBytes(t, "exec", museSyntheticID, workspace))
	env.LookPath = func(name string) (string, error) { return "/opt/bin/" + name, nil }
	env.Environ = func() []string { return nil }
	env.Executable = func() (string, error) { return "/opt/agent-archive", nil }
	var spec termlaunch.Spec
	var tmux []string
	env.OpenTerminal = openInFakeTmux(&spec, &tmux)
	_, errOut, code := runHandoff(t, env, museSyntheticID, "--harness", "muse", "--to", "codex")
	if code != 0 || len(tmux) == 0 || counter.calls.Load() != 1 {
		t.Fatalf("launch %d %s", code, errOut)
	}
	_, errOut, code = runHandoff(t, env, museSyntheticID, "--harness", "muse", "--to", "muse")
	if code == 0 || counter.calls.Load() != 1 {
		t.Fatalf("unsupported launch %d %s", code, errOut)
	}
}

func TestMuseBrowserFilterScrollAndRedrawDoNotReadTranscripts(t *testing.T) {
	t.Parallel()
	env, data, workspace, counter := museNativeEnv(t)
	writeMuseNative(t, data, museSyntheticID, museNativeBytes(t, "exec", museSyntheticID, workspace))
	meter := &nativeReadMeter{}
	env.nativeFS = meter
	fake := newFakeKeys("/0000", "\x1b[B", "\x1b[6~", string(fakeResize), "\x1b", "q")
	var beforeBytes int64
	var beforeOpens int
	env.openKeys = func(io.Reader) (keyTerminal, bool) {
		beforeBytes, beforeOpens, _, _ = meter.counts()
		return fake, true
	}
	env.IsTerminal = func(any) bool { return true }
	env.TerminalSize = fixedTerminal{100, 12}.terminalSize
	var out, errOut strings.Builder
	code := Run([]string{"handoff", "--harness", "muse", "--source", "local", "--no-preamble"}, strings.NewReader(""), &out, &errOut, env)
	afterBytes, afterOpens, active, _ := meter.counts()
	if code != 0 || beforeBytes != afterBytes || beforeOpens != afterOpens || active != 0 || counter.calls.Load() != 0 {
		t.Fatalf("redraw read source %d %d/%d %d/%d active=%d %s", code, beforeBytes, beforeOpens, afterBytes, afterOpens, active, errOut.String())
	}
}

func TestMuseBrowserPastEndLoadsOneBoundedPreviewBatch(t *testing.T) {
	t.Parallel()
	env, data, workspace, counter := museNativeEnv(t)
	for i := range 75 {
		id := fmt.Sprintf("00000000-0000-7000-8000-%012d", i+1)
		writeMuseNative(t, data, id, museNativeBytes(t, "exec", id, workspace))
	}
	meter := &nativeReadMeter{}
	env.nativeFS = meter
	fake := newFakeKeys("\x1b[F", string(fakeResize), strings.Repeat("\x1b[B", 100), "\x1b[F", strings.Repeat("\x1b[B", 100), "/0000", "\x1b", "q")
	var initialBytes int64
	var initialOpens int
	fake.onResize = func() {
		read, opens, active, _ := meter.counts()
		if read != initialBytes || opens != initialOpens || active != 0 {
			t.Errorf("movement and redraw before past-end read source: bytes=%d/%d opens=%d/%d active=%d", read, initialBytes, opens, initialOpens, active)
		}
	}
	env.openKeys = func(io.Reader) (keyTerminal, bool) {
		initialBytes, initialOpens, _, _ = meter.counts()
		return fake, true
	}
	env.IsTerminal = func(any) bool { return true }
	env.TerminalSize = fixedTerminal{100, 12}.terminalSize
	var out, errOut strings.Builder
	code := Run([]string{"handoff", "--harness", "muse", "--source", "local", "--no-preamble"}, strings.NewReader(""), &out, &errOut, env)
	read, opens, active, _ := meter.counts()
	if code != 0 || initialOpens != 75+50 || opens != 75+75 || read <= initialBytes || read > nativeReadBudget || active != 0 || counter.calls.Load() != 0 {
		t.Fatalf("past-end bounds code=%d initial opens=%d total opens=%d bytes=%d active=%d filters=%d stderr=%s", code, initialOpens, opens, read, active, counter.calls.Load(), errOut.String())
	}
	if strings.Count(errOut.String(), "labels inspected for") != 2 || !strings.Contains(errOut.String(), "labels inspected for 50 of 75") || !strings.Contains(errOut.String(), "labels inspected for 75 of 75") {
		t.Fatalf("expected initial50 plus exactly one older25 batch: %s", errOut.String())
	}
}

type museRecheckFailureFS struct {
	*nativeReadMeter
	counter *museFilterCounter
	cancel  context.CancelFunc
}

func (f museRecheckFailureFS) OpenRegular(path string) (transcriptio.File, error) {
	if f.counter.calls.Load() > 0 {
		if f.cancel != nil {
			f.cancel()
		} else {
			return nil, os.ErrPermission
		}
	}
	return f.nativeReadMeter.OpenRegular(path)
}

func TestMuseSelectedPathRecheckFailureAndCancellationCloseHandles(t *testing.T) {
	t.Parallel()
	for _, cancelled := range []bool{false, true} {
		t.Run(strconv.FormatBool(cancelled), func(t *testing.T) {
			t.Parallel()
			env, data, workspace, counter := museNativeEnv(t)
			writeMuseNative(t, data, museSyntheticID, museNativeBytes(t, "exec", museSyntheticID, workspace))
			roots, err := env.nativeRoots("muse")
			must(t, err)
			meter := &nativeReadMeter{}
			result, err := nativesessions.Discover(t.Context(), env.agentRegistry(), meter, roots, nativesessions.Scope{Directories: []string{workspace}}, nativesessions.Limits{Files: 10000, HeaderBytes: nativeWindowBytes, RecordBytes: nativeWindowBytes, TotalBytes: nativeReadBudget, Workers: 2})
			must(t, err)
			if len(result.Candidates) != 1 {
				t.Fatal("candidate")
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var cancelRecheck context.CancelFunc
			if cancelled {
				cancelRecheck = cancel
			}
			files := museRecheckFailureFS{nativeReadMeter: meter, counter: counter, cancel: cancelRecheck}
			_, err = handoffFromNative(ctx, result.Candidates[0], files, env)
			_, _, active, _ := meter.counts()
			if err == nil || active != 0 || counter.calls.Load() != 1 {
				t.Fatalf("recheck accepted/failed cleanup err=%v active=%d filter=%d", err, active, counter.calls.Load())
			}
		})
	}
}
