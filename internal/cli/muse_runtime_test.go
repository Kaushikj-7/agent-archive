package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/nativesessions"
)

const unsupportedMuseDestination handoffDestination = handoffDestination(agentmeta.Muse)

func TestMusePresenceCannotSelectCurrentOrLatestSession(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"MUSE_THREAD_ID", "MUSE_SESSION_ID"} {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			env := testEnv(t, t.TempDir(), time.Now())
			env.LookupEnv = agentEnv(map[string]string{key: "opaque-not-a-uuid"})
			env.IsTerminal = func(any) bool { return true }
			mode, err := env.nonInteractive()
			if err != nil || !mode.on || !strings.Contains(mode.reason, key) || env.interactive(nil) {
				t.Fatalf("presence lost: %+v %v", mode, err)
			}
			if len(currentSessions(env)) != 0 || projectRuntime(env, "") != "" {
				t.Fatal("presence became current or project identity")
			}
			if id, ok, err := currentHandoffSession(env, t.TempDir(), handoffOptions{}); err != nil || ok || id != "" {
				t.Fatalf("presence selected current registration: %s %v %v", id, ok, err)
			}
			for _, name := range []string{"muse", "codex"} {
				candidate := nativesessions.Candidate{NativeID: "opaque-not-a-uuid", Ref: nativesessions.Ref{Harness: name}}
				if selected, err := nativeCurrent([]nativesessions.Candidate{candidate}, "", env); err != nil || selected != nil || isCurrentNative(candidate, runtimeObservations(env)) {
					t.Fatalf("presence selected native session: %+v %v", selected, err)
				}
				if isCallingAgent(env, archive.SessionRegistration{Harness: archive.Harness{Name: name}, NativeSessionID: candidate.NativeID}) {
					t.Fatal("presence claimed a calling session")
				}
			}
			env.LookupEnv = agentEnv(map[string]string{key: "opaque-not-a-uuid", envNonInteractive: "0"})
			if mode, err := env.nonInteractive(); err != nil || mode.on || !env.interactive(nil) {
				t.Fatalf("explicit interaction override lost: %+v %v", mode, err)
			}
		})
	}
}

func TestMuseRuntimeCannotConfigureCaptureOrLaunch(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	// Setup takes its ordinary coordination lock before validating answers.
	// Precreate it so the snapshot pins substantive setup effects separately.
	must(t, os.WriteFile(filepath.Join(home, "setup.lock"), nil, 0o600))
	env := testEnv(t, home, time.Now())
	executable := testExecutable(t)
	env.Executable = func() (string, error) { return executable, nil }
	if got := env.setupNames(); !slices.Equal(got, []string{"codex", "claude", "cursor"}) {
		t.Fatal(got)
	}
	cfg := config.Config{}
	if problems := setupApps(env.setupNames(), &cfg, "muse", nil, false); len(problems) == 0 || len(cfg.Harnesses) != 0 {
		t.Fatalf("runtime-only setup accepted: %+v %v", cfg, problems)
	}
	var stdout, stderr bytes.Buffer
	code := Run([]string{"setup", "--yes", "--provider", "s3", "--bucket", "synthetic", "--region", "us-east-1", "--aws-profile", "test", "--apps", "muse", "--project", t.TempDir()}, strings.NewReader(""), &stdout, &stderr, env)
	if code == 0 || !strings.Contains(stderr.String(), "--apps takes") {
		t.Fatalf("setup accepted runtime-only integration: %d %s %s", code, &stdout, &stderr)
	}
	entries, err := os.ReadDir(home)
	if err != nil || len(entries) != 1 || entries[0].Name() != "setup.lock" {
		t.Fatalf("unsupported setup changed the home snapshot: %v %v", entries, err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"_hook", "--harness", "muse"}, strings.NewReader(`{"session_id":"opaque","hook_event_name":"SessionStart"}`), &stdout, &stderr, env); code != 0 || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("unsupported hook: %d %s %s", code, &stdout, &stderr)
	}
	entries, err = os.ReadDir(home)
	if err != nil || len(entries) != 1 || entries[0].Name() != "setup.lock" {
		t.Fatalf("runtime-only hook changed the home snapshot: %v %v", entries, err)
	}
	lock, err := os.ReadFile(filepath.Join(home, "setup.lock"))
	if err != nil || len(lock) != 0 {
		t.Fatalf("ordinary lock contents changed: %q %v", lock, err)
	}
	if _, err := buildLaunchSpec(unsupportedMuseDestination, "prompt", "/context.md", home, nil, env); err == nil {
		t.Fatal("runtime-only integration launched")
	}
	opts := handoffOptions{to: "muse"}
	if message := validateHandoffOptionsWithCatalog(&opts, false, env.agentRegistry().Catalog()); message == "" {
		t.Fatal("runtime-only launch flag accepted")
	}
	if p, f, ok := env.agentRegistry().LookupSources("muse"); !ok || p == nil || f == nil {
		t.Fatal("explicit local source unavailable")
	}
}

func TestSupportedLaunchRemovesOnlyMuseSessionEnvironment(t *testing.T) {
	t.Parallel()
	env := testEnv(t, t.TempDir(), time.Now())
	env.LookPath = func(string) (string, error) { return filepath.Join(t.TempDir(), "codex"), nil }
	kept := []string{"MUSE_NO_AUTO_UPDATE=1", "MUSE_CONFIG_DIR=/config", "META_API_KEY=synthetic", "XDG_DATA_HOME=/data", "PATH=/bin", "UNRELATED=value"}
	env.Environ = func() []string {
		return append([]string{"MUSE_THREAD_ID=child", "MUSE_SESSION_ID=root"}, kept...)
	}
	spec, err := buildLaunchSpec(handoffDestinationCodex, "prompt", "/context.md", "/project", nil, env)
	if err != nil || !slices.Equal(spec.Env, kept) {
		t.Fatalf("launch environment %v %v", spec.Env, err)
	}
}

func TestMusePresenceCommandDoesNotPrompt(t *testing.T) {
	t.Parallel()
	t.Run("configured archive with another agent available", func(t *testing.T) {
		t.Parallel()
		env, _, _ := publishedFixture(t)
		project := t.TempDir()
		env.WorkingDir = func() (string, error) { return project, nil }
		// The fixture has a Codex owner with this native ID. Muse presence
		// cannot turn that other agent's identity into a current selection.
		env.LookupEnv = agentEnv(map[string]string{"MUSE_THREAD_ID": "native-1"})
		out, stderr, code := ttyRun(t, env, "", "handoff", "--to", "codex")
		if code != 2 || out != "" || !strings.Contains(stderr, noCurrentSessionMessage) {
			t.Fatalf("presence command selected or prompted: %d %q %q", code, out, stderr)
		}
		// Runtime-only presence adds no capture-status application.
		var stdout, errOut bytes.Buffer
		if code := Run([]string{"status", "muse"}, strings.NewReader(""), &stdout, &errOut, env); code != 2 || !strings.Contains(errOut.String(), "unknown app") {
			t.Fatalf("runtime-only status advertised capture: %d %s %s", code, &stdout, &errOut)
		}
	})
	t.Run("before setup", func(t *testing.T) {
		t.Parallel()
		env := testEnv(t, t.TempDir(), time.Now())
		project := t.TempDir()
		env.WorkingDir = func() (string, error) { return project, nil }
		env.LookupEnv = agentEnv(map[string]string{"MUSE_SESSION_ID": "opaque-marker"})
		out, stderr, code := ttyRun(t, env, "", "handoff", "--to", "codex")
		if code != 1 || out != "" || !strings.Contains(stderr, "no verified local sessions") {
			t.Fatalf("unconfigured presence selected or prompted: %d %q %q", code, out, stderr)
		}
	})
}
