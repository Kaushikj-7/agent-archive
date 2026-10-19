package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/state/statetest"
)

// One session can record several capture gaps. The app line counts sessions
// with a gap, not gap entries; the detail line reports both.
func TestStatusCountsSessionsWithCaptureGapsNotGapEntries(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	env, home, _, _ := publishedThroughSync(t, now)
	reg, _ := onlyRegistration(t, home)
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	bundle, publishedAt, found, err := store.LoadLastPublished(reg.ArchiveSessionID)
	if err != nil || !found {
		t.Fatalf("setup: %v", err)
	}
	// Two gaps inside the bundle plus the blocked reason: three gaps, one
	// session.
	bundle.Capture.Gaps = append(bundle.Capture.Gaps,
		archive.CaptureGap{Code: "sensitive_content_redacted", Detail: "test"},
		archive.CaptureGap{Code: "hidden_instruction_omitted", Detail: "test"})
	if err := statetest.SaveBlocked(store, reg.ArchiveSessionID, bundle, publishedAt, state.BlockedReasonRecordTooLarge); err != nil {
		t.Fatal(err)
	}
	view, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	app := view.Apps[0]
	if app.Sessions != 1 || len(app.CaptureGaps) < 3 || app.SessionsWithCaptureGaps != 1 {
		t.Fatalf("sessions=%d gaps=%d sessions with gaps=%d", app.Sessions, len(app.CaptureGaps), app.SessionsWithCaptureGaps)
	}
	var out strings.Builder
	if code := runStatusCommand([]string{"codex"}, &out, os.Stderr, env); code != 0 {
		t.Fatalf("status exit=%d output=%s", code, out.String())
	}
	text := out.String()
	if !strings.Contains(text, "included projects   1 session · 1 archived") {
		t.Fatalf("app line counts gap entries as sessions:\n%s", text)
	}
	if want := "· 1 session has capture gaps\n"; !strings.Contains(text, want) {
		t.Fatalf("missing %q:\n%s", want, text)
	}
	out.Reset()
	if code := runStatusCommand([]string{"--verbose"}, &out, os.Stderr, env); code != 0 {
		t.Fatalf("status --verbose exit=%d output=%s", code, out.String())
	}
	if want := "1 session with a capture gap (" + strconv.Itoa(len(app.CaptureGaps)) + " gaps recorded"; !strings.Contains(out.String(), want) {
		t.Fatalf("status --verbose is missing %q:\n%s", want, out.String())
	}
}

// The storage line leaves out an empty prefix. storage_verified_at stays
// setup's check; storage_access_confirmed_at, and the text Access line, take
// the collector's later verified health for the same configuration, and
// nothing else.
func TestStatusStorageLineAndAccessConfirmation(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	env, home, _, _ := publishedThroughSync(t, now)
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Storage.Prefix = ""
	cfg.StorageVerifiedAt = time.Time{}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	checked := now.Add(-time.Minute).Truncate(time.Second)
	write := func(h storageHealth) {
		t.Helper()
		if err := local.Write(filepath.Join(home, "storage-health.json"), h); err != nil {
			t.Fatal(err)
		}
	}
	text := func() string {
		t.Helper()
		// --verbose keeps when access was checked beside the last upload.
		var out strings.Builder
		if code := runStatusCommand([]string{"--verbose"}, &out, os.Stderr, env); code != 0 {
			t.Fatalf("status exit=%d output=%s", code, out.String())
		}
		return out.String()
	}

	// None of these is evidence for this configuration: another
	// configuration's health, health without a configuration ID (which
	// status --json still reports as verified), and a failed check.
	for name, tc := range map[string]struct {
		health storageHealth
		line   string
	}{
		"other configuration": {storageHealth{ConfigurationID: "other", State: "verified", CheckedAt: checked, Context: "background_collector"}, "! s3://bucket +storage settings changed since the last check · uploaded just now\n"},
		"no configuration":    {storageHealth{ConfigurationID: "", State: "verified", CheckedAt: checked, Context: "background_collector"}, "· s3://bucket +not checked yet · uploaded just now\n"},
		"failed check":        {storageHealth{ConfigurationID: configurationID(cfg), State: "authentication_failed", CheckedAt: checked, Context: "background_collector"}, "✗ s3://bucket +sign-in failed, checked 1 minute ago · uploaded just now\n"},
	} {
		write(tc.health)
		view, err := readStatus(env)
		if err != nil {
			t.Fatal(err)
		}
		if !view.StorageAccessConfirmedAt.IsZero() || view.StorageAccessConfirmedBy != "" || !view.StorageVerifiedAt.IsZero() {
			t.Fatalf("%s: confirmed=%v by=%q verified=%v", name, view.StorageAccessConfirmedAt, view.StorageAccessConfirmedBy, view.StorageVerifiedAt)
		}
		if out := text(); !regexp.MustCompile(tc.line).MatchString(out) {
			t.Fatalf("%s: access line:\n%s", name, out)
		}
	}

	write(storageHealth{ConfigurationID: configurationID(cfg), State: "verified", CheckedAt: checked, Context: "background_collector"})
	view, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if !view.StorageAccessConfirmedAt.Equal(checked) || view.StorageAccessConfirmedBy != storageAccessConfirmedByCollector || !view.StorageVerifiedAt.IsZero() {
		t.Fatalf("confirmed=%v by=%q verified=%v", view.StorageAccessConfirmedAt, view.StorageAccessConfirmedBy, view.StorageVerifiedAt)
	}
	out := text()
	if !regexp.MustCompile("✓ s3://bucket +reachable, checked 1 minute ago · uploaded just now\n").MatchString(out) {
		t.Fatalf("access line:\n%s", out)
	}

	// A later setup check wins over older collector health, and
	// storage_verified_at keeps meaning setup's check.
	cfg.StorageVerifiedAt = checked.Add(30 * time.Second)
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	if view, err = readStatus(env); err != nil || !view.StorageVerifiedAt.Equal(cfg.StorageVerifiedAt) || !view.StorageAccessConfirmedAt.Equal(cfg.StorageVerifiedAt) || view.StorageAccessConfirmedBy != storageAccessConfirmedBySetup {
		t.Fatalf("verified=%v confirmed=%v by=%q err=%v", view.StorageVerifiedAt, view.StorageAccessConfirmedAt, view.StorageAccessConfirmedBy, err)
	}
	if out := text(); !regexp.MustCompile("✓ s3://bucket +reachable, checked by setup just now · uploaded just now\n").MatchString(out) {
		t.Fatalf("access line:\n%s", out)
	}
}

func TestStorageLabel(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		cfg  credentials.Config
		want string
	}{
		{credentials.Config{Provider: "s3", Bucket: "b"}, "s3 / b"},
		{credentials.Config{Provider: "r2", Bucket: "b", Prefix: "agent-archive/"}, "r2 / b / agent-archive/"},
	} {
		if got := storageLabel(tc.cfg); got != tc.want {
			t.Errorf("storageLabel(%+v) = %q, want %q", tc.cfg, got, tc.want)
		}
	}
}
