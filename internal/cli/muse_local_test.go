package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/termlaunch"
)

func museLocalFixture(t *testing.T, name string) string {
	t.Helper()
	raw, e := os.ReadFile(filepath.Join("..", "agents", "muse", "testdata", name+".jsonl"))
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(t.TempDir(), "1999", "01", "01", "unrelated", "session.jsonl")
	must(t, os.MkdirAll(filepath.Dir(path), 0700))
	must(t, os.WriteFile(path, raw, 0600))
	misleading := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	must(t, os.Chtimes(path, misleading, misleading))
	return path
}

func TestMuseExplicitFileHandoffUsesReservedStartAndNoSetup(t *testing.T) {
	t.Parallel()
	for _, lane := range []string{"exec", "msp"} {
		for _, content := range []string{"empty", "conversation"} {
			t.Run(lane+"/"+content, func(t *testing.T) {
				t.Parallel()
				path := museLocalFixture(t, "synthetic-"+lane+"-"+content)
				home := filepath.Join(t.TempDir(), "not-created")
				env := testEnv(t, home, time.Now())
				env.LookupEnv = agentEnv(map[string]string{envNonInteractive: "1"})
				out, errOut, code := runHandoff(t, env, "--file", path, "--harness", "muse", "--format", "json")
				if code != 0 {
					t.Fatalf("code %d: %s", code, errOut)
				}
				var h archive.Handoff
				if e := json.Unmarshal([]byte(out), &h); e != nil {
					t.Fatal(e)
				}
				if h.Session.StartedAt == nil || !h.Session.StartedAt.Equal(time.UnixMicro(1792083855882704)) || h.Session.NativeSessionID != "00000000-0000-7000-8000-000000000001" || !h.ToolResultsUnavailable {
					t.Fatalf("handoff %+v", h)
				}
				if content == "conversation" && (!strings.Contains(out, "Explain the synthetic parser.") || !strings.Contains(out, "It validates bounded native records")) {
					t.Fatalf("conversation missing: %s", out)
				}
				for _, code := range []string{"muse_local_conversation_only", "usage_cache_reasoning_cost_unavailable", "tool_facts_unavailable", "model_switches_unavailable", "child_coverage_unavailable"} {
					if !strings.Contains(out, code) {
						t.Fatalf("missing availability code %s", code)
					}
				}
				if !strings.Contains(out, "muse_local_conversation_only") {
					t.Fatal("missing availability gap")
				}
				md, errOut, code := runHandoff(t, env, "--file", path, "--harness", "muse")
				if code != 0 || !strings.Contains(md, "Started 2026-10-15 17:04 UTC") || !strings.Contains(md, "muse_local_conversation_only") {
					t.Fatalf("markdown %d %s %s", code, errOut, md)
				}
				if _, e := os.Stat(home); !os.IsNotExist(e) {
					t.Fatalf("local read wrote setup data: %v", e)
				}
				output := filepath.Join(t.TempDir(), "handoff.md")
				_, errOut, code = runHandoff(t, env, "--file", path, "--harness", "muse", "--output", output)
				if code != 0 {
					t.Fatalf("output %d: %s", code, errOut)
				}
				if raw, e := os.ReadFile(output); e != nil || !bytes.Contains(raw, []byte("muse_local_conversation_only")) {
					t.Fatalf("output artifact %v", e)
				}
			})
		}
	}
}

func TestMuseInvalidFileAndBackfillCreateNoArtifacts(t *testing.T) {
	t.Parallel()
	path := museLocalFixture(t, "synthetic-exec-conversation")
	must(t, os.WriteFile(path, []byte("{\"unsupported\":true}\n"), 0600))
	home := filepath.Join(t.TempDir(), "not-created")
	env := testEnv(t, home, time.Now())
	output := filepath.Join(t.TempDir(), "result.md")
	if _, _, code := runHandoff(t, env, "--file", path, "--harness", "muse", "--output", output); code == 0 {
		t.Fatal("invalid input succeeded")
	}
	if _, e := os.Stat(output); !os.IsNotExist(e) {
		t.Fatalf("invalid output created: %v", e)
	}
	homeReads := 0
	env.Home = func() (string, error) { homeReads++; return home, nil }
	var out, errOut bytes.Buffer
	if code := Run([]string{"backfill", "--yes", "--background", "--harness", "muse"}, nil, &out, &errOut, env); code == 0 {
		t.Fatalf("backfill admitted Muse: %s", out.String())
	}
	if homeReads != 0 || !strings.Contains(errOut.String(), "muse") {
		t.Fatalf("backfill reached home/journal before rejecting harness: reads=%d error=%s", homeReads, errOut.String())
	}
	if _, e := os.Stat(home); !os.IsNotExist(e) {
		t.Fatalf("unsupported backfill wrote state: %v", e)
	}
}

func TestMuseExplicitFileHandoffToSupportedDestination(t *testing.T) {
	t.Parallel()
	path := museLocalFixture(t, "synthetic-exec-conversation")
	env := testEnv(t, filepath.Join(t.TempDir(), "never-set-up"), time.Now())
	env.LookPath = func(name string) (string, error) { return "/opt/bin/" + name, nil }
	env.Environ = func() []string { return nil }
	env.Executable = func() (string, error) { return "/opt/agent-archive", nil }
	env.WorkingDir = func() (string, error) { return filepath.Dir(path), nil }
	var spec termlaunch.Spec
	var tmux []string
	env.OpenTerminal = openInFakeTmux(&spec, &tmux)
	_, errOut, code := runHandoff(t, env, "--file", path, "--harness", "muse", "--to", "codex")
	if code != 0 || len(tmux) == 0 {
		t.Fatalf("destination code %d: %s", code, errOut)
	}
}
