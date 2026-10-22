package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

// Characterization of the macOS scheduler (PR 5a-0 of
// dev/proposals/implemented/platform-abstraction.md): how the CLI reads launchd's answer.
// testdata/scheduler/launchctl-print/ holds launchctl print output in the
// shapes launchd gives (its README says how they were made); the placeholder
// @PLIST@ stands for the plist launchd loaded the job from. This file runs
// the commands and names no scheduler code, so it stays as it is when that
// code moves; scheduler_jobstate_internal_test.go holds what does not.

// status --json background has six values. Five come from launchctl's answer,
// and the sixth (broken) from the LaunchAgent's program, which launchd still
// calls loaded. status only asks (launchctl print, once) and never changes a
// job. The golden files record what else status says for each: its state and
// next action, and the human screen's headline and collector row.
//
// Not parallel: it replaces launchctl.
func TestStatusBackgroundForEveryJobState(t *testing.T) {
	project := t.TempDir()
	_, _, env := installedFixture(t, newFakeKeychain(), s3SetupInput("b", "us-east-1", "p", false, true, false, project))
	home, err := env.Home()
	must(t, err)
	userHome, err := env.UserHomeDir()
	must(t, err)
	label := wantCollectorLabel(t, home, false)
	plist := filepath.Join(userHome, "Library", "LaunchAgents", label+".plist")
	if _, err := os.Stat(plist); err != nil {
		t.Fatalf("setup left no plist where launchd is told to look: %v", err)
	}
	cfg, _, err := config.Load(home)
	must(t, err)

	for _, job := range []struct {
		file   string
		failed error // how launchctl exited
		want   string
	}{
		{"running.txt", nil, "running"},
		{"loaded-not-running.txt", nil, "loaded"},
		{"missing.txt", errors.New("exit status 113"), "missing"},
		{"another-installation.txt", nil, "another_installation"},
		{"no-path-line.txt", nil, "unknown"},
		{"failed-unrelated.txt", errors.New("exit status 1"), "unknown"},
		// Last, since it deletes the program the LaunchAgent names.
		{"running.txt", nil, "broken"},
	} {
		name := strings.TrimSuffix(job.file, ".txt")
		if job.want == "broken" {
			name = "broken"
			must(t, os.Remove(cfg.InstalledExecutable))
		}
		recorded, err := os.ReadFile(filepath.Join("testdata", "scheduler", "launchctl-print", job.file))
		must(t, err)
		var calls []string
		env := launchdAnswering(t, env, func(args ...string) ([]byte, error) {
			calls = append(calls, strings.Join(args, " "))
			return []byte(strings.ReplaceAll(string(recorded), "@PLIST@", plist)), job.failed
		})
		decoded := statusJSON(t, env)
		if decoded["background"] != job.want {
			t.Errorf("%s: status --json background = %v, want %s", job.file, decoded["background"], job.want)
		}
		var human strings.Builder
		if code := Run([]string{"status", "--verbose"}, strings.NewReader(""), &human, os.Stderr, env); code != 0 {
			t.Fatalf("status exit %d", code)
		}
		// One print for each of the two runs, and nothing else.
		if want := fmt.Sprintf("print gui/%d/%s", os.Getuid(), label); len(calls) != 2 || calls[0] != want || calls[1] != want {
			t.Errorf("%s: status ran launchctl %q, want two prints of %s", job.file, calls, want)
		}
		// The screen leads with the problem, under the title and a blank
		// line, and lists the collector's row among the storage rows.
		lines := strings.Split(human.String(), "\n")
		if len(lines) < 3 || lines[1] != "" {
			t.Fatalf("%s: status screen without a title and a blank line:\n%s", job.file, human.String())
		}
		var rows []string
		for _, line := range lines {
			if strings.Contains(line, "Background collector") {
				rows = append(rows, strings.TrimSpace(line))
			}
		}
		text := fmt.Sprintf("background: %v\nstate: %v\ncode: %v\nnext_action: %v\nheadline: %s\nscreen: %s\n", decoded["background"], decoded["state"], decoded["code"], decoded["next_action"], strings.TrimSpace(lines[2]), strings.Join(rows, " | "))
		text = strings.NewReplacer(canonical(t, project), "@PROJECT@", project, "@PROJECT@", plist, "@PLIST@", label, "@LABEL@", userHome, "@USER_HOME@", cfg.InstalledExecutable, "@EXECUTABLE@").Replace(text)
		golden.Check(t, filepath.Join("testdata", "scheduler", "status", name+".txt"), []byte(text))
	}
}
