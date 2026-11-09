package cli

import (
	"slices"
	"strings"
	"testing"
)

// Setup refuses an app whose hook file holds another installation's hooks as
// soon as the apps are chosen: before the storage and retention questions,
// not after all three steps. The answers are saved, so the next run stops
// again at once unless the apps change, and choosing only apps without
// another installation's hooks goes ahead.
func TestSetupRefusesAnotherInstallationsAppBeforeStorageQuestions(t *testing.T) {
	t.Parallel()
	primary, secondary, userHome := twoInstallations(t)
	setupRun(t, primary, s3SetupInput("b", "us-east-1", "p", false, true, false, t.TempDir()), 0)
	before := hookFileSnapshot(t, userHome)
	secondHome, _ := secondary.home()

	// Claude Code, whose file holds the first installation's hooks.
	output := setupRun(t, secondary, s3SetupInput("b", "us-east-1", "p", false, true, false, t.TempDir()), 1)
	if strings.Contains(output, "Connect storage") || !strings.Contains(output, "Setup installs no Claude Code hooks beside them") {
		t.Fatalf("setup did not refuse before the storage step:\n%s", output)
	}
	if !strings.Contains(output, "your answers are saved") {
		t.Fatalf("setup does not say the answers are kept:\n%s", output)
	}
	draft, found, problem, err := readDraft(secondHome)
	if err != nil || !found || problem != "" || !slices.Equal(draft.Config.Harnesses, []string{"claude"}) {
		t.Fatalf("draft %+v found=%v problem=%q err=%v", draft.Config.Harnesses, found, problem, err)
	}
	// Continuing the saved setup stops at once, before any storage question.
	output = setupRun(t, secondary, "\n", 1)
	if strings.Contains(output, "Connect storage") || !strings.Contains(output, "Setup installs no Claude Code hooks beside them") {
		t.Fatalf("continuing the saved setup did not refuse at once:\n%s", output)
	}
	if after := hookFileSnapshot(t, userHome); !equalMaps(before, after) {
		t.Fatal("a refused setup changed hook files")
	}

	// Changing the apps to Codex only, which nothing else hooks, goes ahead.
	// The warning about Claude Code's file still comes first.
	input := strings.Join([]string{"capture", "y", "y", "n", "n", "n", "included-projects", "", "s3-existing", "b", "p", "us-east-1", "y"}, "\n") + "\n"
	output = setupRun(t, secondary, input, 0)
	if !strings.Contains(output, "hooks of another agent-archive installation") {
		t.Fatalf("setup did not warn about the other installation's hooks:\n%s", output)
	}
}
