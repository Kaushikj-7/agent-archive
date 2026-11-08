package cli

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/platform"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestCapabilityProfilesDoNotClaimUnverifiedNativeEvidence(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"codex", "claude", "cursor", "agy"} {
		profile := captureCapabilityProfile(productionAgents, name)
		if profile.Transcript.State != capabilityDocumented {
			t.Fatalf("%s profile=%#v", name, profile)
		}
		expectedSubagent := capabilityUnavailable
		if name == "claude" {
			expectedSubagent = capabilityFixtureValidated
		}
		if profile.SkillEvidence.State != capabilityUnavailable || profile.SubagentLinkage.State != expectedSubagent {
			t.Fatalf("%s invented native capability: %#v", name, profile)
		}
	}
	if !strings.Contains(captureCapabilityProfile(productionAgents, "claude").SubagentLinkage.Evidence, "unverified") {
		t.Fatal("fixture coverage claimed live verification")
	}
	// Cursor fresh start rests on the documented transcript_path, not on
	// cursor_version or on an unverified reading of sessionStart semantics.
	cursorStart := captureCapabilityProfile(productionAgents, "cursor").FreshStart
	if cursorStart.State != capabilityDocumented || !strings.Contains(cursorStart.Evidence, "transcript_path") {
		t.Fatalf("Cursor start=%#v", cursorStart)
	}
	if strings.Contains(cursorStart.Evidence, "cursor_version") {
		t.Fatal("Cursor fresh start claimed a version field as evidence")
	}
}

func TestInstalledVersionSupportNeedsMatchingVerifiedCapture(t *testing.T) {
	t.Parallel()
	discovery := applicationDiscovery{Installed: true, Version: "agent 1.2.3", VersionState: "observed"}
	if got := installedVersionSupport(discovery, nil); got != "unverified" {
		t.Fatal(got)
	}
	if got := installedVersionSupport(discovery, []string{"1.2.3"}); got != "verified_by_capture" {
		t.Fatal(got)
	}
	if got := installedVersionSupport(discovery, []string{"2.0.0"}); got != "unverified" {
		t.Fatal(got)
	}
}

func TestNormalizedVersionKeepsEveryComponentAndFallsBack(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"1.2.3.4":            "1.2.3.4",
		"v1.2.3":             "1.2.3",
		"codex-cli 1.2.3":    "1.2.3",
		"1.2.3-beta.1+build": "1.2.3-beta.1+build",
		"  2.0  ":            "2.0",
		"nightly-abc":        "nightly-abc",
	}
	for input, want := range cases {
		if got := normalizedVersion(input); got != want {
			t.Errorf("normalizedVersion(%q) = %q, want %q", input, got, want)
		}
	}
	if installedVersionSupport(applicationDiscovery{Installed: true, Version: "1.2.3.4", VersionState: "observed"}, []string{"2.3.4"}) != "unverified" {
		t.Fatal("four-component version matched its own suffix")
	}
}

func TestInstalledVersionSupportReportsWhyUnverified(t *testing.T) {
	t.Parallel()
	cli := applicationDiscovery{Installed: true, Version: "1.2.3", VersionKind: versionKindCLI, VersionState: "observed"}
	if state, reason := installedVersionSupportDetail(cli, nil); state != "unverified" || reason != supportReasonNoVerifiedCapture {
		t.Fatalf("%s %s", state, reason)
	}
	if state, reason := installedVersionSupportDetail(cli, []string{"1.2.4"}); state != "unverified" || reason != supportReasonNoMatchingVersion {
		t.Fatalf("%s %s", state, reason)
	}
	if state, reason := installedVersionSupportDetail(cli, []string{"1.2.4", "1.2.3"}); state != "verified_by_capture" || reason != "" {
		t.Fatalf("%s %s", state, reason)
	}
	// Cursor: the app bundle's CFBundleShortVersionString against the hook's
	// cursor_version. Same scheme compares; a different scheme is flagged.
	bundle := applicationDiscovery{Installed: true, Version: "1.6.45", VersionKind: versionKindAppBundle, VersionState: "observed"}
	if state, reason := installedVersionSupportDetail(bundle, []string{"1.6.45"}); state != "verified_by_capture" || reason != "" {
		t.Fatalf("%s %s", state, reason)
	}
	if state, reason := installedVersionSupportDetail(bundle, []string{"2025.09.12-nightly"}); state != "unverified" || reason != supportReasonNoMatchingVersion {
		t.Fatalf("%s %s", state, reason)
	}
	if state, reason := installedVersionSupportDetail(bundle, []string{"agent-build-7f3a"}); state != "unverified" || reason != supportReasonVersionSourceMismatch {
		t.Fatalf("%s %s", state, reason)
	}
	if state, reason := installedVersionSupportDetail(applicationDiscovery{VersionState: "absent"}, []string{"1.6.45"}); state != "absent" || reason != "" {
		t.Fatalf("%s %s", state, reason)
	}
}

func TestDiscoverCommandVersionTriesEveryPresentCandidate(t *testing.T) {
	// Not parallel: boundedVersionCommand gives a script's output 250 ms
	// after it exits, and a busy parallel run (-race, 18 tests at once) can
	// starve it past that, so the working script reads as failing.
	if runtime.GOOS == "windows" {
		t.Skip("shell scripts")
	}
	dir := t.TempDir()
	script := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
		return path
	}
	broken := script("broken", "exit 1")
	working := script("working", "echo 'tool 1.2.3'")
	missing := filepath.Join(dir, "missing")
	got := discoverCommandVersion("tool", [][]string{{missing, "--version"}, {broken, "--version"}, {working, "--version"}})
	if !got.Installed || got.Version != "tool 1.2.3" || got.VersionState != "observed" || got.VersionKind != versionKindCLI || got.VersionSource != working+" --version" {
		t.Fatalf("%+v", got)
	}
	got = discoverCommandVersion("tool", [][]string{{broken, "--version"}, {script("broken2", "exit 2"), "--version"}})
	if !got.Installed || got.Version != "" || got.VersionState != "unknown" || got.VersionKind != versionKindCLI {
		t.Fatalf("present-but-failing candidates: %+v", got)
	}
	got = discoverCommandVersion("tool", [][]string{{missing, "--version"}, {dir, "--version"}})
	if got.Installed || got.VersionState != "absent" {
		t.Fatalf("absent candidates: %+v", got)
	}
}

func TestClaudeDesktopBundledCLIsNewestVersionFirst(t *testing.T) {
	t.Parallel()
	userHome := t.TempDir()
	root := filepath.Join(userHome, "Library", "Application Support", "Claude", "claude-code")
	for _, dir := range []string{"2.1.99", "2.1.275", "2.1.280", "2.1.100", "not-a-version", "backup-2.1.300", "2.1.300.bak"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "3.0.0"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got := claudeDesktopBundledCLIs(userHome)
	var want []string
	for _, version := range []string{"2.1.280", "2.1.275", "2.1.100", "2.1.99"} {
		want = append(want, filepath.Join(root, version, "claude.app", "Contents", "MacOS", "claude"))
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	if got := claudeDesktopBundledCLIs(t.TempDir()); len(got) != 0 {
		t.Fatalf("no desktop app: %v", got)
	}
}

func TestVersionCandidatesPreferStandaloneOverBundled(t *testing.T) {
	t.Parallel()
	userHome := t.TempDir()
	bundled := filepath.Join(userHome, "Library", "Application Support", "Claude", "claude-code", "2.1.280")
	if err := os.MkdirAll(bundled, 0o755); err != nil {
		t.Fatal(err)
	}
	var claude []string
	for _, candidate := range claudeVersionCandidates(userHome, platform.Darwin) {
		claude = append(claude, candidate[0])
	}
	want := []string{"claude", filepath.Join(userHome, ".local", "bin", "claude"), filepath.Join(userHome, ".claude", "local", "claude"), filepath.Join(bundled, "claude.app", "Contents", "MacOS", "claude")}
	if !reflect.DeepEqual(claude, want) {
		t.Fatalf("claude candidates %v", claude)
	}
	var codex []string
	for _, candidate := range codexVersionCandidates(userHome, platform.Darwin) {
		codex = append(codex, candidate[0])
	}
	want = []string{
		"/Applications/Codex.app/Contents/Resources/codex",
		filepath.Join(userHome, "Applications", "Codex.app", "Contents", "Resources", "codex"),
		"codex",
		"/Applications/ChatGPT.app/Contents/Resources/codex",
		filepath.Join(userHome, "Applications", "ChatGPT.app", "Contents", "Resources", "codex"),
	}
	if !reflect.DeepEqual(codex, want) {
		t.Fatalf("codex candidates %v", codex)
	}
}

// On Linux, and on a system the program does not know, no macOS app
// location is probed: the candidates are the CLIs on
// PATH and the standalone install paths, even when a Claude desktop folder
// exists under the home directory.
func TestVersionCandidatesSkipMacOSAppLocationsOnLinux(t *testing.T) {
	t.Parallel()
	userHome := t.TempDir()
	bundled := filepath.Join(userHome, "Library", "Application Support", "Claude", "claude-code", "2.1.280")
	if err := os.MkdirAll(bundled, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, goos := range []platform.OS{platform.Linux, platform.Unknown} {
		var claude, codex []string
		for _, candidate := range claudeVersionCandidates(userHome, goos) {
			claude = append(claude, candidate[0])
		}
		for _, candidate := range codexVersionCandidates(userHome, goos) {
			codex = append(codex, candidate[0])
		}
		wantClaude := []string{"claude", filepath.Join(userHome, ".local", "bin", "claude"), filepath.Join(userHome, ".claude", "local", "claude")}
		if !reflect.DeepEqual(claude, wantClaude) {
			t.Errorf("%s: claude candidates %v, want %v", goos, claude, wantClaude)
		}
		if want := []string{"codex"}; !reflect.DeepEqual(codex, want) {
			t.Errorf("%s: codex candidates %v, want %v", goos, codex, want)
		}
	}
}

// Cursor is found only through its macOS app bundle. On Linux (and on a
// system the program does not know) the probe is
// skipped and says so: neither installed nor absent, so setup shows "version
// not detected" rather than "not found", and version support is "unknown"
// rather than "absent". The same bundle in the home folder is found on macOS.
func TestDiscoverCursorVersionOnLinuxClaimsNothing(t *testing.T) {
	t.Parallel()
	userHome := t.TempDir()
	bundle := filepath.Join(userHome, "Applications", "Cursor.app")
	if err := os.MkdirAll(filepath.Join(bundle, "Contents"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, goos := range []platform.OS{platform.Linux, platform.Unknown} {
		got := discoverApplicationsFor(userHome, goos)["cursor"]
		if got.Installed || got.VersionState != "unknown" || got.Version != "" {
			t.Errorf("%s: %+v, want not installed with state unknown", goos, got)
		}
		if state, reason := installedVersionSupportDetail(got, []string{"1.6.45"}); state != "unknown" || reason != "" {
			t.Errorf("%s: support %s %s", goos, state, reason)
		}
		if appWithVersion("cursor", got) != "Cursor (version not detected)" {
			t.Errorf("%s: review row %q", goos, appWithVersion("cursor", got))
		}
	}
	if got := discoverCursorVersion(userHome, platform.Darwin); !got.Installed || got.VersionKind != versionKindAppBundle {
		t.Errorf("darwin: a bundle in ~/Applications is not found: %+v", got)
	}
}

func TestCompareDottedVersions(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		a    string
		b    string
		want int
	}{
		{"2.1.280", "2.1.275", 1},
		{"2.1.99", "2.1.100", -1},
		{"1.2", "1.2.0", 0},
		{"1.2.3-alpha", "1.2.3", 0},
		{"codex-cli 0.155.0-alpha.9.2", "0.154.9", 1},
	} {
		if got := compareDottedVersions(tt.a, tt.b); got != tt.want {
			t.Fatalf("compare(%q, %q) = %d want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestVersionDirPatternIsAnchored(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]bool{"2.1.280": true, "0.155.0-alpha.9.2": true, "backup-2.1.300": false, "2.1.300.bak": false, "v2.1.300": false, "2": false} {
		if got := versionDirPattern.MatchString(name); got != want {
			t.Fatalf("%q: got %v want %v", name, got, want)
		}
	}
}

// Regression: pre-release review, carried over from agent-skills (e371b6a).
func TestVersionSupportUsesPublishedVersionNotResumedRegistration(t *testing.T) {
	t.Parallel()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	must(t, os.Chmod(home, 0700))
	at := time.Now().UTC()
	cfg := config.Config{MachineID: "machine", Storage: credentialsTestConfig(), Harnesses: []string{"codex"}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: project, Included: true, ActivatedAt: at.Add(-time.Hour)}}}}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	localStore, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(project, "synthetic.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"turn_context","model":"synthetic"}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	reg := archive.SessionRegistration{ArchiveSessionID: "s", NativeSessionID: "n", ProjectID: "p", ProjectRoot: project, Harness: archive.Harness{Name: "codex", Version: "1.2.3"}, TranscriptPath: path, SessionStartedAt: at, RegisteredAt: at}
	if err := localStore.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	remote := storagetest.NewMemoryStore()
	result, err := collector.Run(context.Background(), localStore, remote, collector.Options{Parsers: productionAgents, Sources: productionAgents, MachineID: cfg.MachineID, Now: func() time.Time { return at }})
	if err != nil || len(result.Errors) > 0 {
		t.Fatalf("%+v %v", result, err)
	}
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), at)
	if summary, err := verifyPublications(home, cfg, env, localStore, remote); err != nil || summary.Verified != 1 {
		t.Fatalf("%+v %v", summary, err)
	}
	reg.Harness.Version = "2.0.0"
	if err := localStore.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	if err := recordApplicationDiscoveries(home, map[string]applicationDiscovery{"codex": {Installed: true, Version: "2.0.0", VersionState: "observed"}}, at); err != nil {
		t.Fatal(err)
	}
	view, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if view.Apps[0].VersionSupport != "unverified" {
		t.Fatalf("unpublished resumed version verified: %+v", view.Apps[0])
	}
}

// Regression: pre-release review, carried over from agent-skills (e371b6a).
func TestMissingVersionDiscoveryIsUnknown(t *testing.T) {
	t.Parallel()
	if got := installedVersionSupport(applicationDiscovery{}, nil); got != "unknown" {
		t.Fatal(got)
	}
	if got := installedVersionSupport(applicationDiscovery{Installed: true, Version: "1.0.0"}, []string{"11.0.0"}); got != "unverified" {
		t.Fatal(got)
	}
	var output cappedBuffer
	if _, err := output.Write([]byte(strings.Repeat("x", 5000))); err == nil || output.Len() > 4096 {
		t.Fatal("version output not bounded")
	}
}

const (
	versionKindCLI       = "cli"
	versionKindAppBundle = "app_bundle"
)
