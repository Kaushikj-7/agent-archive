package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

// Before setup, the read commands a new user tries first point at the
// handoff that already works on this machine's own sessions, not only
// at setup. Exit 1 and an empty stdout are unchanged, so --json keeps
// exactly the shape it had: nothing on stdout, the message on stderr.
func TestReadCommandsPreSetupMentionHandoff(t *testing.T) {
	t.Parallel()
	env := testEnv(t, t.TempDir(), time.Now())
	for _, args := range [][]string{
		{"list"}, {"list", "--json"},
		{"show", "session-1"}, {"show", "session-1", "--json"},
		{"stats"}, {"stats", "--json"},
	} {
		var out, errOut bytes.Buffer
		if code := Run(args, nil, &out, &errOut, env); code != 1 {
			t.Fatalf("%v: code=%d stderr=%q", args, code, errOut.String())
		}
		if out.Len() != 0 {
			t.Fatalf("%v: stdout=%q, want empty", args, out.String())
		}
		if !strings.Contains(errOut.String(), "`agent-archive handoff`") ||
			!strings.Contains(errOut.String(), "`agent-archive setup`") {
			t.Fatalf("%v: stderr=%q, want handoff and setup guidance", args, errOut.String())
		}
	}
}

// Commands that write to the archive keep the terse not-set-up answer:
// handoff is not a substitute for collecting or importing.
func TestWriteCommandsPreSetupKeepTerseMessage(t *testing.T) {
	t.Parallel()
	env := testEnv(t, t.TempDir(), time.Now())
	for _, args := range [][]string{{"sync"}, {"backfill", "history"}} {
		var out, errOut bytes.Buffer
		if code := Run(args, nil, &out, &errOut, env); code != 1 {
			t.Fatalf("%v: code=%d stderr=%q", args, code, errOut.String())
		}
		if strings.Contains(errOut.String(), "handoff") {
			t.Fatalf("%v: stderr=%q, want no handoff hint", args, errOut.String())
		}
		if !strings.Contains(errOut.String(), "agent-archive setup") {
			t.Fatalf("%v: stderr=%q, want setup guidance", args, errOut.String())
		}
	}
}

// A bare invocation before setup leads with the same handoff and setup
// guidance, before the command list, and still exits 0.
func TestNoArgsPreSetupLeadsWithHandoff(t *testing.T) {
	t.Parallel()
	env := setupTestEnv(t, t.TempDir(), t.TempDir(), newFakeKeychain(), time.Now())
	var out bytes.Buffer
	if code := Run(nil, nil, &out, nil, env); code != 0 {
		t.Fatalf("code=%d out=%q", code, out.String())
	}
	first, _, _ := strings.Cut(out.String(), "\n")
	if !strings.Contains(first, "`agent-archive handoff`") ||
		!strings.Contains(first, "`agent-archive setup`") {
		t.Fatalf("first line=%q, want handoff and setup guidance", first)
	}
}

// While setup is under way, handoff refuses its native pre-setup path, so
// list, show, stats, and a bare invocation must not send anyone there:
// they print the plain setup guidance, and handoff's own refusal agrees.
func TestPreSetupGuidanceWithPendingSetupDoesNotRecommendHandoff(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, file, refusal string }{
		{"draft", "setup-draft.json", "saved setup is pending or inaccessible"},
		{"journal", "setup-transaction.json", "setup recovery is pending"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			must(t, os.WriteFile(filepath.Join(home, tc.file), []byte("{}"), 0o600))
			env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now())
			for _, args := range [][]string{
				{"list"}, {"list", "--json"},
				{"show", "session-1"}, {"show", "session-1", "--json"},
				{"stats"}, {"stats", "--json"},
			} {
				var out, errOut bytes.Buffer
				if code := Run(args, nil, &out, &errOut, env); code != 1 {
					t.Fatalf("%v: code=%d stderr=%q", args, code, errOut.String())
				}
				if out.Len() != 0 || errOut.String() != setupUnfinishedMessage+"\n" {
					t.Fatalf("%v: stdout=%q stderr=%q, want only %q", args, out.String(), errOut.String(), setupUnfinishedMessage)
				}
			}
			var out bytes.Buffer
			if code := Run(nil, nil, &out, nil, env); code != 0 || !strings.HasPrefix(out.String(), setupUnfinishedMessage+"\n\nAgent Archive") {
				t.Fatalf("bare: code=%d out=%q", code, out.String())
			}
			var errOut bytes.Buffer
			if code := Run([]string{"handoff", "--latest"}, nil, &out, &errOut, env); code != 1 || !strings.Contains(errOut.String(), tc.refusal) {
				t.Fatalf("handoff: code=%d stderr=%q, want refusal %q", code, errOut.String(), tc.refusal)
			}
		})
	}
}

// After uninstall the configuration stays, with archiving off: handoff
// then reads the configured archive rather than this machine's own
// sessions, so a bare invocation keeps the plain setup line it printed
// before the handoff hint existed.
func TestNoArgsAfterUninstallDoesNotRecommendHandoff(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	cfg := config.Config{MachineID: "synthetic-machine", Storage: credentialsTestConfig(), Harnesses: []string{"codex"}, Archive: archive.Config{Enabled: false}}
	must(t, config.Save(home, cfg))
	env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now())
	var out bytes.Buffer
	if code := Run(nil, nil, &out, nil, env); code != 0 || !strings.HasPrefix(out.String(), "Not set up yet — run agent-archive setup.\n\nAgent Archive") {
		t.Fatalf("code=%d out=%q", code, out.String())
	}
	if strings.Contains(out.String(), "`agent-archive handoff`") {
		t.Fatalf("out=%q, want no handoff hint", out.String())
	}
}
