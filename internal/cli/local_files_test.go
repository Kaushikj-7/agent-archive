package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/capture"
)

// damagedLocalFiles are the local files a command reads, relative to the data
// directory, and whether each is advisory: status must report around an
// advisory one (exit 0, with a warning naming it), and no command may fail
// on it.
var damagedLocalFiles = []struct {
	name     string
	advisory bool
}{
	{"config.json", false},
	{"setup-draft.json", true},
	{"setup-transaction.json", false},
	{"storage-health.json", true},
	{"capture-diagnostics.json", true},
	{"status.json", true},
	{"application-versions.json", true},
	{"discovery-refused-starts.json", true},
	{"registrations/x.json", true},
	{"requests/x.json", true},
	{collectorLockRecordName, true},
}

// damagedFileCommands are the commands run over each damaged file, with the
// answers a terminal would give.
var damagedFileCommands = []struct {
	args  []string
	stdin string
}{
	{[]string{"status"}, ""},
	{[]string{"status", "--json"}, ""},
	{[]string{"setup"}, strings.Repeat("\n", 20)},
	{[]string{"pause"}, ""},
	{[]string{"resume"}, ""},
	{[]string{"sync"}, ""},
	{[]string{"list"}, ""},
	{[]string{"uninstall", "--yes"}, ""},
	{[]string{"uninstall", "--delete-local-data", "--yes"}, ""},
}

// Every local file whose damage can stop a command is named, with a way out,
// by the command it stops; an advisory one stops nothing (U-20, U-21). The
// damage is the two shapes a file takes: bytes that are not JSON (a
// truncated write), and JSON of another shape (a newer version's file).
func TestDamagedLocalFilesAreNamedOrReadAround(t *testing.T) {
	t.Parallel()
	type damageKind struct {
		name    string
		content string
	}
	// What each command exits with on an undamaged installation. The fixture
	// is the same for every damage and file, so it is run once per command.
	healthy := make([]int, len(damagedFileCommands))
	for i, command := range damagedFileCommands {
		healthy[i], _ = runOnFixture(t, command.args, command.stdin, "", "")
	}
	for _, damage := range []damageKind{{"truncated", `{"version":1,"step":`}, {"other shape", `[1,2,3]`}} {
		for _, file := range damagedLocalFiles {
			for i, command := range damagedFileCommands {
				name := damage.name + "/" + file.name + "/" + strings.Join(command.args, " ")
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					healthyCode := healthy[i]
					code, output := runOnFixture(t, command.args, command.stdin, file.name, damage.content)
					if file.advisory && command.args[0] == "status" && code != healthyCode {
						t.Fatalf("exit %d, %d when healthy:\n%s", code, healthyCode, output)
					}
					// The collector's own state reader names the session, not
					// the file, for a registration or request of another shape
					// (internal/state, which it keeps rather than moving it
					// aside, as a newer version's file); the ID is the file name.
					named := strings.Contains(output, file.name) || (damage.name == "other shape" && command.args[0] == "sync" && strings.Contains(output, `"x"`))
					if code != healthyCode && !named {
						t.Fatalf("exit %d (%d when healthy) without naming %s:\n%s", code, healthyCode, file.name, output)
					}
					if file.advisory && command.args[0] == "status" && file.name != collectorLockRecordName && !strings.Contains(output, file.name) {
						t.Fatalf("status does not name the damaged %s:\n%s", file.name, output)
					}
				})
			}
		}
	}
}

// runOnFixture runs args on a fresh installation whose file (relative to the
// data directory) holds content, when file is not "".
func runOnFixture(t *testing.T, args []string, stdin, file, content string) (int, string) {
	t.Helper()
	project, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	home, _, env := installedFixture(t, newFakeKeychain(), s3SetupInput("b", "us-east-1", "p", false, true, false, project))
	env.Now = func() time.Time { return time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC) }
	if file != "" {
		path := filepath.Join(home, file)
		must(t, os.MkdirAll(filepath.Dir(path), 0o700))
		must(t, os.WriteFile(path, []byte(content), 0o600))
	}
	var out, errOut bytes.Buffer
	code := Run(args, strings.NewReader(stdin), &out, &errOut, env)
	return code, out.String() + errOut.String()
}

// U-20: a damaged or newer-version saved setup is named, and setup offers to
// move it aside and go on; status names it too. Declining stops, naming it.
func TestSetupOffersToMoveAnUnusableDraftAside(t *testing.T) {
	t.Parallel()
	for _, draft := range []string{`{"version":1,"step":`, `{"version":2,"step":1}`} {
		home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
		env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
		path := filepath.Join(home, "setup-draft.json")
		must(t, os.WriteFile(path, []byte(draft), 0o600))

		var out, errOut bytes.Buffer
		if code := Run([]string{"status"}, nil, &out, &errOut, env); code != 0 || !strings.Contains(out.String(), path) || !strings.Contains(out.String(), "move it aside") {
			t.Fatalf("status exit %d:\n%s%s", code, &out, &errOut)
		}
		output := setupRun(t, env, "n\n", 1)
		if !strings.Contains(output, path) || strings.Contains(output, "Run agent-archive setup to continue.") && !strings.Contains(output, "move it aside or delete it") {
			t.Fatalf("declined:\n%s", output)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("declining moved the draft: %v", err)
		}
		output = setupRun(t, env, "y\n"+s3SetupInput("b", "us-east-1", "p", false, true, false, project), 0)
		if !strings.Contains(output, path) || !strings.Contains(output, "Moved it to") {
			t.Fatalf("accepted:\n%s", output)
		}
		aside, _ := filepath.Glob(path + ".*" + movedAsideSuffix)
		if len(aside) != 1 {
			t.Fatalf("moved aside: %v", aside)
		}
		if data, err := os.ReadFile(aside[0]); err != nil || string(data) != draft {
			t.Fatalf("the moved copy is not the draft: %q %v", data, err)
		}
	}
}

// U-21: a capture-diagnostics.json that no longer decodes is replaced by the
// next diagnostic (it used to be read first, fail, and never heal), and by
// setup's prune.
func TestDamagedCaptureDiagnosticsHeal(t *testing.T) {
	t.Parallel()
	project, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	home, _, _ := installedFixture(t, newFakeKeychain(), s3SetupInput("b", "us-east-1", "p", false, true, false, project))
	for _, damage := range []string{"{truncated", `{"not":"a list"}`} {
		must(t, os.WriteFile(capture.DiagnosticsPath(home), []byte(damage), 0o600))
		must(t, capture.RecordDiagnostic(home, capture.Diagnostic{Code: capture.DiagnosticUnknownSessionStart, Harness: "claude", ProjectRoot: project, ObservedAt: time.Now()}))
		diagnostics, err := capture.ReadDiagnostics(home)
		if err != nil || len(diagnostics) != 1 {
			t.Fatalf("after a diagnostic over %q: %v %v", damage, diagnostics, err)
		}
		must(t, os.WriteFile(capture.DiagnosticsPath(home), []byte(damage), 0o600))
		must(t, capture.PruneDiagnostics(home, nil))
		if _, err := capture.ReadDiagnostics(home); err != nil {
			t.Fatalf("after a prune over %q: %v", damage, err)
		}
	}
}
