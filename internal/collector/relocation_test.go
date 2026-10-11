package collector

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"github.com/wangjohn/agent-archive/internal/state"
)

const relocatedThread = "01a10dee-f063-7743-8b7b-1ccf59d1a8c7"

// relocationFixture is an unbound legacy Codex registration whose rollout
// sits in a configured Codex home's sessions/ directory, and that home's
// archived_sessions/ locator for the same file.
func relocationFixture(t *testing.T) (*sessionScan, string, string) {
	t.Helper()
	codexHome, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	name := "rollout-2026-10-05T14-18-45-" + relocatedThread + ".jsonl"
	original := filepath.Join(codexHome, "sessions", "2026", "10", "05", name)
	archived := filepath.Join(codexHome, "archived_sessions", name)
	for _, dir := range []string{filepath.Dir(original), filepath.Dir(archived)} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	local := newTestStore(t)
	reg := registration(t, original)
	reg.NativeSessionID = relocatedThread
	reg.Origin = archive.SessionOriginImport
	reg.AdmittedAt = reg.RegisteredAt
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{MachineID: "machine", Harnesses: []string{"codex"}, ImportedHarnesses: []string{"codex"}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: reg.ProjectRoot, ProjectID: reg.ProjectID, Included: true, ActivatedAt: reg.SessionStartedAt}}}}
	if err := config.Save(local.Home(), cfg); err != nil {
		t.Fatal(err)
	}
	return &sessionScan{local: local, reg: reg, opts: Options{ConfiguredCodexHomes: []string{codexHome}}}, original, archived
}

func storedPath(t *testing.T, local *state.Store, id string) string {
	t.Helper()
	reg, found, err := local.LoadRegistration(id)
	if err != nil || !found {
		t.Fatalf("load registration: %v %v", found, err)
	}
	return reg.TranscriptPath
}

// Following a renamed rollout is best effort after a successful read: it
// moves only an unbound registration's locator, only to the same file name in
// the same trusted home, and anything that stops it leaves the registration
// as it is without failing the scan.
func TestFollowRelocatedRolloutIsGuardedAndBestEffort(t *testing.T) {
	t.Parallel()
	t.Run("follows", func(t *testing.T) {
		t.Parallel()
		scan, _, archived := relocationFixture(t)
		if err := scan.followRelocatedRollout(archived); err != nil {
			t.Fatal(err)
		}
		if got := storedPath(t, scan.local, scan.id()); got != archived || scan.reg.TranscriptPath != archived {
			t.Fatalf("stored %s, scan %s", got, scan.reg.TranscriptPath)
		}
	})
	refused := func(t *testing.T, scan *sessionScan, target string) {
		t.Helper()
		before := scan.reg
		if err := scan.followRelocatedRollout(target); err != nil {
			t.Fatalf("a refused relocation failed the scan: %v", err)
		}
		if got := storedPath(t, scan.local, scan.id()); got != before.TranscriptPath || scan.reg.TranscriptPath != before.TranscriptPath {
			t.Fatalf("relocated to %s (scan %s)", got, scan.reg.TranscriptPath)
		}
	}
	t.Run("another file name", func(t *testing.T) {
		t.Parallel()
		scan, _, archived := relocationFixture(t)
		refused(t, scan, filepath.Join(filepath.Dir(archived), "rollout-2026-10-05T14-18-45-01a10d7d-bf2d-7612-8a01-4b02cfd5aa2d.jsonl"))
	})
	t.Run("another home", func(t *testing.T) {
		t.Parallel()
		scan, _, archived := relocationFixture(t)
		other, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		// Trusted too, but not the home the registration belongs to.
		scan.opts.ConfiguredCodexHomes = append(scan.opts.ConfiguredCodexHomes, other)
		refused(t, scan, filepath.Join(other, "archived_sessions", filepath.Base(archived)))
	})
	t.Run("outside the rollout directories", func(t *testing.T) {
		t.Parallel()
		scan, original, archived := relocationFixture(t)
		refused(t, scan, filepath.Join(filepath.Dir(filepath.Dir(archived)), "elsewhere", filepath.Base(original)))
	})
	t.Run("bound registration", func(t *testing.T) {
		t.Parallel()
		scan, original, archived := relocationFixture(t)
		binding := &archive.CodexSourceBinding{Version: 1, NativeThreadID: relocatedThread, NativeCreatedAt: scan.reg.SessionStartedAt, Cwd: scan.reg.ProjectRoot, ProducerSource: "vscode", PhysicalRolloutID: relocatedThread, Path: original}
		if _, err := scan.local.UpdateRegistration(scan.id(), func(r *archive.SessionRegistration) error { r.CodexBinding = binding; return nil }); err != nil {
			t.Fatal(err)
		}
		scan.reg.CodexBinding = binding
		refused(t, scan, archived)
	})
	t.Run("registration changed meanwhile", func(t *testing.T) {
		t.Parallel()
		scan, original, archived := relocationFixture(t)
		moved := filepath.Join(filepath.Dir(original), "..", filepath.Base(original))
		if _, err := scan.local.UpdateRegistration(scan.id(), func(r *archive.SessionRegistration) error { r.TranscriptPath = moved; return nil }); err != nil {
			t.Fatal(err)
		}
		if err := scan.followRelocatedRollout(archived); err != nil {
			t.Fatalf("a compare-and-swap conflict failed the scan: %v", err)
		}
		if got := storedPath(t, scan.local, scan.id()); got != moved || scan.reg.TranscriptPath != original {
			t.Fatalf("conflict overwritten: stored %s, scan %s", got, scan.reg.TranscriptPath)
		}
	})
	t.Run("hooks lock busy", func(t *testing.T) {
		t.Parallel()
		scan, _, archived := relocationFixture(t)
		unlock, err := local.NamedLock(scan.local.Home(), "hooks.lock")
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
		refused(t, scan, archived)
	})
	t.Run("capture paused", func(t *testing.T) {
		t.Parallel()
		scan, _, archived := relocationFixture(t)
		cfg, _, err := config.Load(scan.local.Home())
		if err != nil {
			t.Fatal(err)
		}
		cfg.Paused = true
		if err := config.Save(scan.local.Home(), cfg); err != nil {
			t.Fatal(err)
		}
		refused(t, scan, archived)
	})
	t.Run("capture no longer accepted", func(t *testing.T) {
		t.Parallel()
		scan, _, archived := relocationFixture(t)
		cfg, _, err := config.Load(scan.local.Home())
		if err != nil {
			t.Fatal(err)
		}
		cfg.Archive.Projects[0].Included = false
		if err := config.Save(scan.local.Home(), cfg); err != nil {
			t.Fatal(err)
		}
		refused(t, scan, archived)
	})
	t.Run("setup pending", func(t *testing.T) {
		t.Parallel()
		scan, _, archived := relocationFixture(t)
		if err := os.WriteFile(setupjournal.JournalPath(scan.local.Home()), []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
		refused(t, scan, archived)
	})
	t.Run("registration removed meanwhile", func(t *testing.T) {
		t.Parallel()
		scan, original, archived := relocationFixture(t)
		if err := os.Remove(filepath.Join(scan.local.Home(), "registrations", scan.id()+".json")); err != nil {
			t.Fatal(err)
		}
		if err := scan.followRelocatedRollout(archived); err != nil {
			t.Fatalf("a removed registration failed the scan: %v", err)
		}
		if _, found, err := scan.local.LoadRegistration(scan.id()); err != nil || found || scan.reg.TranscriptPath != original {
			t.Fatalf("removed registration recreated or followed: found=%v err=%v scan=%s", found, err, scan.reg.TranscriptPath)
		}
	})
}
