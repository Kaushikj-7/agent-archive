package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/local"
)

func TestAppSelectionSuggestionsAndManualFallback(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		detected []string
		existing []string
		input    string
		want     []string
		prompt   string
		manual   bool
	}{
		{"accept detected", []string{"claude", "codex", "codex"}, nil, "\n", []string{"codex", "claude"}, "Include Codex and Claude Code?", false},
		{"single app", []string{"cursor"}, nil, "y\n", []string{"cursor"}, "Include Cursor?", false},
		{"choose another app", []string{"codex", "claude"}, nil, "n\nn\nn\ny\nn\n", []string{"cursor"}, "Include Codex and Claude Code?", true},
		{"nothing detected", nil, nil, "n\ny\nn\nn\n", []string{"claude"}, "No apps found automatically.", true},
		{"keep prior selection", []string{"claude"}, []string{"claude"}, "\n", []string{"claude"}, "Change which apps are included?", false},
		{"add newly found apps", []string{"codex", "claude", "cursor", "agy"}, []string{"cursor"}, "\n", []string{"codex", "claude", "cursor", "agy"}, "Add them?", false},
		{"decline newly found apps", []string{"codex", "claude", "cursor", "agy"}, []string{"cursor"}, "n\n\n", []string{"cursor"}, "Change which apps are included?", false},
		{"decline found apps then pick a subset", []string{"codex", "claude", "cursor", "agy"}, []string{"cursor"}, "n\ny\nn\ny\ny\nn\n", []string{"claude", "cursor"}, "Change which apps are included?", true},
		{"add one newly found app", []string{"claude"}, []string{"cursor"}, "y\n", []string{"claude", "cursor"}, "Add it?", false},
		{"add to prior selection", nil, []string{"cursor"}, "y\ny\ny\n\ny\n", []string{"codex", "claude", "cursor", "agy"}, "Change which apps are included?", true},
		{"keep full prior selection", nil, []string{"cursor", "claude", "codex", "agy"}, "\n", []string{"codex", "claude", "cursor", "agy"}, "Keep Codex, Claude Code, Cursor, and Antigravity CLI?", false},
		{"trim full prior selection", nil, []string{"cursor", "claude", "codex", "agy"}, "n\nn\n\n\n\n", []string{"claude", "cursor", "agy"}, "Choose which apps to include:", true},
		{"retry empty selection", nil, nil, "n\nn\nn\nn\ny\nn\nn\nn\n", []string{"codex"}, "Choose at least one app to continue.", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			got, err := promptHarnesses(allHarnesses, newPrompter(strings.NewReader(tt.input), &out), tt.detected, tt.existing)
			if err != nil || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v, %v; want %v", got, err, tt.want)
			}
			if !setupContainsText(out.String(), tt.prompt) {
				t.Fatalf("missing prompt: %s", &out)
			}
			if setupContainsText(out.String(), "Choose which apps to include:") != tt.manual {
				t.Fatalf("incorrect manual fallback: %s", &out)
			}
		})
	}
}

func TestDetectedAppsSetupSkipsIndividualQuestions(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now())
	env.DetectHarnesses = func(string) []string { return []string{"codex", "claude"} }
	input := strings.Join([]string{"y", "included-projects", project, "", "s3-existing", "profile", "test-bucket", "us-east-1", "y"}, "\n") + "\n"
	output := setupRun(t, env, input, 0)
	// The Sessions row is left out while it shows the default.
	for _, unwanted := range []string{"Detected settings", "capture policy", "Include Cursor?", "Include Codex?", "All new sessions, with or without skills"} {
		if setupContainsText(output, unwanted) {
			t.Fatalf("unexpected %q in %s", unwanted, output)
		}
	}
	for _, want := range []string{"Capture sessions from Codex and Claude Code?", "Keep for 90 days", "Start archiving? 1) Start archiving (default) 2) Edit a setting", "[q] Cancel; keep draft"} {
		if !setupContainsText(output, want) {
			t.Fatalf("missing %q in %s", want, output)
		}
	}
}

func TestSetupReviewMarksOnlyChangedValues(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	old := config.Config{
		Harnesses:     []string{"cursor"},
		RetentionDays: 90,
		Archive:       archive.Config{Projects: []archive.ProjectActivation{{Root: project, Included: true}}},
		Storage:       credentials.Config{Provider: credentials.ProviderR2, Bucket: "agent-archive", R2AccountID: "oldaccount", Prefix: defaultPrefix},
	}
	next := old
	next.Storage.R2AccountID = "newaccount"
	var out bytes.Buffer
	p := newPrompter(strings.NewReader(""), &out)
	showSetupReview(p, next, setupReview{existing: old, reconfiguring: true, discoveries: map[string]applicationDiscovery{"cursor": {Installed: true, Version: "3.21.13", VersionState: "observed"}}})
	got := out.String()
	for _, want := range []string{"Apps Cursor", "* Storage r2://agent-archive/agent-archive/ · account newaccount", "was r2://agent-archive/agent-archive/  account oldaccount", "was r2://agent-archive/agent-archive/  account oldaccount"} {
		if !setupContainsText(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Count(got, "* ") != 1 || setupContainsText(got, "\x1b[") {
		t.Fatalf("only the storage should be marked, with no color in a buffer:\n%s", got)
	}
}

func TestInteractiveReviewCanChangeSkillEvidence(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	p := newPrompter(strings.NewReader("skills\nnone\n"), &out)
	draft := setupDraft{Config: config.Config{SkillEvidence: config.SkillEvidenceMetadata}}
	if err := editSetupReview(allHarnesses, p, &draft, t.TempDir(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if draft.Config.SkillEvidence != config.SkillEvidenceNone {
		t.Fatalf("policy = %q\n%s", draft.Config.SkillEvidence, &out)
	}
	if !setupContainsText(out.String(), "outside selected projects") {
		t.Fatalf("scope missing: %s", &out)
	}
}

func TestResumePrePolicyFreshDraftDefaultsToMetadata(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now())
	input := strings.TrimSuffix(s3SetupInput("bucket", "us-east-1", "profile", true, false, false, project), "y\n") + "q\n"
	setupRun(t, env, input, 0)
	draft, found, problem, err := readDraft(home)
	if err != nil || !found || problem != "" {
		t.Fatalf("draft: found=%v problem=%q err=%v", found, problem, err)
	}
	draft.Config.SkillEvidence = "" // A draft saved before the policy existed.
	draft.Config.Discovery = nil    // Discovery protection did not exist either.
	draft.Config.CodexCapture = nil // Neither did scope protection.
	draft.Config.SchemaVersion = 1
	if err := local.Write(draftPath(home), draft); err != nil {
		t.Fatal(err)
	}
	output := setupRun(t, env, "continue\nq\n", 0)
	if !setupContainsText(output, "Skills") || !setupContainsText(output, "Metadata, including user folders") || setupContainsText(output, "kept from previous setup") {
		t.Fatalf("fresh draft did not use metadata:\n%s", output)
	}
	resumed, _, _, err := readDraft(home)
	if err != nil || resumed.Config.EffectiveSkillEvidence() != config.SkillEvidenceMetadata || (resumed.Config.Discovery != nil && resumed.Config.Discovery.Enabled) {
		t.Fatalf("resumed policy = %q, err = %v", resumed.Config.SkillEvidence, err)
	}
}

func TestMenuAcceptsNumbersKeysAndPrefixes(t *testing.T) {
	t.Parallel()
	options := []option{{"capture", "Apps and projects"}, {"storage", "Storage"}, {"retention", "Retention"}, {"all", "All settings"}}
	tests := []struct {
		name    string
		input   string
		want    string
		retried bool
	}{
		{"number", "2\n", "storage", false},
		{"blank takes default", "\n", "capture", false},
		{"key", "Retention\n", "retention", false},
		{"unique prefix", "st\n", "storage", false},
		{"out of range retries", "5\n4\n", "all", true},
		{"sentence retries", "edit capture\n1\n", "capture", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			got, err := newPrompter(strings.NewReader(tt.input), &out).guidedMenu("What would you like to change?", "capture", options...)
			if err != nil || got != tt.want {
				t.Fatalf("got %q, %v; want %q\n%s", got, err, tt.want, &out)
			}
			for _, want := range []string{"  1) Apps and projects (default)\n", "  4) All settings\n", "Choose [1]: "} {
				if !setupContainsText(out.String(), want) {
					t.Fatalf("missing %q in %s", want, &out)
				}
			}
			if setupContainsText(out.String(), "Enter one of the available choices.") != tt.retried {
				t.Fatalf("unexpected retry behavior: %s", &out)
			}
		})
	}
}

func TestMenuRejectsAmbiguousPrefix(t *testing.T) {
	t.Parallel()
	options := []option{{"retention", "Retention"}, {"region", "Region"}}
	var out bytes.Buffer
	got, err := newPrompter(strings.NewReader("r\n2\n"), &out).guidedMenu("Change?", "", options...)
	if err != nil || got != "region" || !setupContainsText(out.String(), "Choose: ") {
		t.Fatalf("got %q, %v\n%s", got, err, &out)
	}
}

func TestReviewActionMapsChoices(t *testing.T) {
	t.Parallel()
	for input, want := range map[string]string{"1\n": "start", "\n": "start", "2\n": "edit", "q\n": "cancel", "y\n": "start", "n\n": "cancel", "e\n": "edit"} {
		got, err := reviewAction(newPrompter(strings.NewReader(input), &bytes.Buffer{}), false, false, false)
		if err != nil || got != want {
			t.Fatalf("input %q: got %q, %v; want %q", input, got, err, want)
		}
	}
}

// With a ✗ on the checklist, starting is neither offered nor accepted: the
// first choice checks again, and y is asked again rather than taken.
func TestReviewActionRefusesStartWhenBlocked(t *testing.T) {
	t.Parallel()
	for input, want := range map[string]string{"1\n": "check", "\n": "check", "c\n": "check", "2\n": "edit", "e\n": "edit", "q\n": "cancel", "n\n": "cancel", "y\ne\n": "edit", "yes\nq\n": "cancel"} {
		var out bytes.Buffer
		got, err := reviewAction(newPrompter(strings.NewReader(input), &out), false, true, false)
		if err != nil || got != want {
			t.Fatalf("input %q: got %q, %v; want %q\n%s", input, got, err, want, &out)
		}
		if setupContainsText(out.String(), "start archiving") || !setupContainsText(out.String(), "1) Check again") {
			t.Fatalf("input %q offered start:\n%s", input, &out)
		}
	}
}

func TestNewlyFoundAppsOmitAppsNotDetected(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	got, err := promptHarnesses(allHarnesses, newPrompter(strings.NewReader("\n"), &out), []string{"claude", "cursor"}, []string{"cursor"})
	if err != nil || !reflect.DeepEqual(got, []string{"claude", "cursor"}) {
		t.Fatalf("got %v, %v", got, err)
	}
	if setupContainsText(out.String(), "Codex") || setupContainsText(out.String(), "Not included") {
		t.Fatalf("an app neither included nor detected was advertised:\n%s", &out)
	}
}

// An app declined at "Also found on this computer" is remembered through the
// setup draft and the saved configuration, is not offered again, and leaves
// the declined list once the user includes it.
func TestSetupRemembersDeclinedApps(t *testing.T) {
	t.Parallel()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	env.DetectHarnesses = func(string) []string { return []string{"cursor"} }
	setupRun(t, env, strings.Join([]string{"y", "included-projects", project, "", "s3-existing", "profile", "test-bucket", "us-east-1", "y"}, "\n")+"\n", 0)

	env.DetectHarnesses = func(string) []string { return []string{"codex", "claude", "cursor"} }
	// Apps and projects; decline the found apps; change nothing else; keep
	// the project; add none; cancel at the review so the draft is kept.
	output := setupRun(t, env, "capture\nn\n\ny\n\nn\n", 0)
	if !setupContainsText(output, "Add them?") {
		t.Fatalf("found apps not offered:\n%s", output)
	}
	draft, err := os.ReadFile(filepath.Join(home, "setup-draft.json"))
	if err != nil || !setupContainsText(string(draft), `"declined_harnesses"`) {
		t.Fatalf("draft lost the declined apps: %v\n%s", err, draft)
	}
	// Continue the draft and start archiving.
	setupRun(t, env, "\ny\n", 0)
	cfg, _, _ := config.Load(home)
	if !reflect.DeepEqual(cfg.Harnesses, []string{"cursor"}) || !reflect.DeepEqual(cfg.DeclinedHarnesses, []string{"codex", "claude"}) {
		t.Fatalf("harnesses %v declined %v", cfg.Harnesses, cfg.DeclinedHarnesses)
	}

	output = setupRun(t, env, "capture\n\ny\n\ny\n", 0)
	if setupContainsText(output, "Also found") || !setupContainsText(output, "Included: Cursor. Not included: Codex, Claude Code, and Antigravity CLI.") {
		t.Fatalf("declined apps offered again:\n%s", output)
	}

	// Include Claude Code by hand: it is no longer declined; Codex still is.
	setupRun(t, env, "capture\ny\nn\ny\ny\nn\ny\n\ny\n", 0)
	cfg, _, _ = config.Load(home)
	if !reflect.DeepEqual(cfg.Harnesses, []string{"claude", "cursor"}) || !reflect.DeepEqual(cfg.DeclinedHarnesses, []string{"codex"}) {
		t.Fatalf("harnesses %v declined %v", cfg.Harnesses, cfg.DeclinedHarnesses)
	}
}

// The review screen's app edit has no detection, so it never offers found
// apps, and it keeps the declined list apart from any app it includes.
func TestReviewEditNeverOffersFoundApps(t *testing.T) {
	t.Parallel()
	draft := setupDraft{Config: config.Config{Harnesses: []string{"cursor"}, DeclinedHarnesses: []string{"codex", "claude"}}}
	var out bytes.Buffer
	p := newPrompter(strings.NewReader("apps\ny\nn\ny\ny\nn\n"), &out)
	if err := editSetupReview(allHarnesses, p, &draft, t.TempDir(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if setupContainsText(out.String(), "Also found") || !setupContainsText(out.String(), "Change which apps are included?") {
		t.Fatalf("unexpected prompts:\n%s", &out)
	}
	if !reflect.DeepEqual(draft.Config.Harnesses, []string{"claude", "cursor"}) || !reflect.DeepEqual(draft.Config.DeclinedHarnesses, []string{"codex"}) {
		t.Fatalf("harnesses %v declined %v", draft.Config.Harnesses, draft.Config.DeclinedHarnesses)
	}
}

// An app removed by hand in the capture step is declined too, so the next
// reconfigure does not offer it back as found.
func TestSetupRemembersAppRemovedByHand(t *testing.T) {
	t.Parallel()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	env.DetectHarnesses = func(string) []string { return []string{"codex", "claude"} }
	setupRun(t, env, strings.Join([]string{"y", "included-projects", project, "", "s3-existing", "profile", "test-bucket", "us-east-1", "y"}, "\n")+"\n", 0)

	// Apps and projects; change apps: Codex no, Claude Code yes, Cursor no;
	// keep the project; add none; start archiving.
	setupRun(t, env, "capture\ny\nn\ny\nn\nn\ny\n\ny\n", 0)
	cfg, _, _ := config.Load(home)
	if !reflect.DeepEqual(cfg.Harnesses, []string{"claude"}) || !reflect.DeepEqual(cfg.DeclinedHarnesses, []string{"codex"}) {
		t.Fatalf("harnesses %v declined %v", cfg.Harnesses, cfg.DeclinedHarnesses)
	}

	output := setupRun(t, env, "capture\n\ny\n\nn\n", 0)
	if setupContainsText(output, "Also found") || !setupContainsText(output, "Included: Claude Code. Not included: Codex, Cursor, and Antigravity CLI.") {
		t.Fatalf("removed app offered again:\n%s", output)
	}
}

// An app removed in the review screen's app edit is declined, and a later
// capture step with it detected does not offer it back.
func TestReviewEditRemovalIsNotOfferedAgain(t *testing.T) {
	t.Parallel()
	draft := setupDraft{Config: config.Config{Harnesses: []string{"codex", "claude"}}}
	p := newPrompter(strings.NewReader("apps\ny\nn\ny\nn\nn\n"), &bytes.Buffer{})
	if err := editSetupReview(allHarnesses, p, &draft, t.TempDir(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(draft.Config.Harnesses, []string{"claude"}) || !reflect.DeepEqual(draft.Config.DeclinedHarnesses, []string{"codex"}) {
		t.Fatalf("harnesses %v declined %v", draft.Config.Harnesses, draft.Config.DeclinedHarnesses)
	}
	var out bytes.Buffer
	if err := chooseHarnesses(allHarnesses, newPrompter(strings.NewReader("\n"), &out), []string{"codex", "claude"}, &draft.Config); err != nil {
		t.Fatal(err)
	}
	if setupContainsText(out.String(), "Also found") || !reflect.DeepEqual(draft.Config.Harnesses, []string{"claude"}) {
		t.Fatalf("removed app offered again: %v\n%s", draft.Config.Harnesses, &out)
	}
}

func TestSetupReviewShowsDeclinedApps(t *testing.T) {
	t.Parallel()
	old := config.Config{Harnesses: []string{"cursor"}, RetentionDays: 90, Storage: credentials.Config{Provider: credentials.ProviderR2, Bucket: "b", R2AccountID: "a", Prefix: defaultPrefix}}
	next := old
	next.DeclinedHarnesses = []string{"codex", "claude"}
	var out bytes.Buffer
	showSetupReview(newPrompter(strings.NewReader(""), &out), next, setupReview{existing: old, reconfiguring: true})
	got := out.String()
	if !setupContainsText(got, "* Skipped    Codex and Claude Code (setup will not offer again; to add back, choose Apps and projects in agent-archive setup)") || setupContainsText(got, "Nothing above differs") {
		t.Fatalf("declining found apps not shown as a change:\n%s", got)
	}
}

func TestDecliningSuggestedProjectUsesManualSelection(t *testing.T) {
	t.Parallel()
	project, other := t.TempDir(), t.TempDir()
	other, _ = filepath.EvalSymlinks(other)
	if err := os.Mkdir(filepath.Join(project, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	env := setupTestEnv(t, t.TempDir(), t.TempDir(), newFakeKeychain(), time.Now())
	env.WorkingDir = func() (string, error) { return project, nil }
	env.DetectHarnesses = func(string) []string { return []string{"codex"} }
	var cfg config.Config
	var out bytes.Buffer
	err := chooseCapture(newPrompter(strings.NewReader("n\ny\nincluded-projects\nspecific\n1\np\n"+other+"\n\n"), &out), &cfg, t.TempDir(), env, nil)
	if err != nil || len(cfg.Archive.Projects) != 1 || cfg.Archive.Projects[0].Root != other {
		t.Fatalf("config=%+v err=%v", cfg, err)
	}
}

// Leaving every project out asks for one again instead of ending setup,
// inside a repository and outside one.
//
// Regression: 2026-09 onboarding review #7.
func TestLeavingEveryProjectOutAsksAgain(t *testing.T) {
	t.Parallel()
	for _, inRepo := range []bool{true, false} {
		t.Run(map[bool]string{true: "repository", false: "no-repository"}[inRepo], func(t *testing.T) {
			t.Parallel()
			other := t.TempDir()
			other, _ = filepath.EvalSymlinks(other)
			env := setupTestEnv(t, t.TempDir(), t.TempDir(), newFakeKeychain(), time.Now())
			env.DetectHarnesses = func(string) []string { return []string{"codex"} }
			input := "y\nincluded-projects\n" + other + "\nspecific\n1\n\n1\n\n"
			if inRepo {
				current := gitRepo(t)
				env.WorkingDir = func() (string, error) { return current, nil }
				input = "n\ny\nincluded-projects\nspecific\n1\n\np\n" + other + "\n\n"
			} else {
				env.WorkingDir = func() (string, error) { return other, nil }
			}
			var cfg config.Config
			var out bytes.Buffer
			err := chooseCapture(newPrompter(strings.NewReader(input), &out), &cfg, t.TempDir(), env, nil)
			if err != nil || len(cfg.Archive.Projects) != 1 || cfg.Archive.Projects[0].Root != other || !setupContainsText(out.String(), "choose at least one project") {
				t.Fatalf("config=%+v err=%v\n%s", cfg, err, &out)
			}
		})
	}
}

// a includes every listed project, and each line says how many sessions the
// apps' history holds for it.
//
// Regression: 2026-09 onboarding review #8.
func TestRecentProjectsShowCountsAndATakesAll(t *testing.T) {
	t.Parallel()
	one, two := gitRepo(t), gitRepo(t)
	known := []backfill.KnownProject{{Root: one, Sessions: 12}, {Root: two, Sessions: 1}}
	var out bytes.Buffer
	projects, err := selectSetupProjects(newPrompter(strings.NewReader("all\n"), &out), nil, nil, known, "", "", nil)
	if err != nil || includedProjects(projects) != 2 || projects[0].Root != one || projects[1].Root != two {
		t.Fatalf("projects=%+v err=%v", projects, err)
	}
	if !setupContainsText(out.String(), "12 sessions") || !setupContainsText(out.String(), "1 session") || !setupContainsText(out.String(), "All 2 found projects (default)") {
		t.Fatalf("output:\n%s", &out)
	}
}

func TestRecentProjectsEnterTakesDefaultAll(t *testing.T) {
	t.Parallel()
	one, two := gitRepo(t), gitRepo(t)
	known := []backfill.KnownProject{{Root: one}, {Root: two}}
	var out bytes.Buffer
	projects, err := selectSetupProjects(newPrompter(strings.NewReader("\n"), &out), nil, nil, known, "", "", nil)
	if err != nil || includedProjects(projects) != 2 || projects[0].Root != one || projects[1].Root != two {
		t.Fatalf("projects=%+v err=%v\n%s", projects, err, &out)
	}
	if !setupContainsText(out.String(), "Which projects?") || !setupContainsText(out.String(), "All 2 found projects") {
		t.Fatalf("output:\n%s", &out)
	}
}

func TestRecentProjectsDefaultRemainsAfterInvalidAnswer(t *testing.T) {
	t.Parallel()
	for _, first := range []string{"99", "/does/not/exist"} {
		t.Run(first, func(t *testing.T) {
			t.Parallel()
			one, two := gitRepo(t), gitRepo(t)
			known := []backfill.KnownProject{{Root: one}, {Root: two}}
			var out bytes.Buffer
			projects, err := selectSetupProjects(newPrompter(strings.NewReader(first+"\n\n"), &out), nil, nil, known, "", "", nil)
			if err != nil || includedProjects(projects) != 2 || strings.Count(out.String(), "Which projects?") != 2 {
				t.Fatalf("projects=%+v err=%v\n%s", projects, err, &out)
			}
		})
	}
}

func TestRecentProjectsDefaultCannotFinishWithNoProject(t *testing.T) {
	t.Parallel()
	gone, fallback := gitRepo(t), gitRepo(t)
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	known := []backfill.KnownProject{{Root: gone}}
	var out bytes.Buffer
	projects, err := selectSetupProjects(newPrompter(strings.NewReader("\nspecific\np\n"+fallback+"\n1\n\n"), &out), nil, nil, known, "", "", nil)
	if err != nil || includedProjects(projects) != 1 || projects[0].Root != fallback || !setupContainsText(out.String(), "Repair the path or leave it out") {
		t.Fatalf("projects=%+v err=%v\n%s", projects, err, &out)
	}
}

// Switching a listed project off undoes only this answer: a project the
// list added is dropped, and an exclusion setup kept stays an exclusion,
// so a later backfill still skips it. With nothing left, it asks again.
func TestLeavingAListedProjectOutRestoresItsState(t *testing.T) {
	t.Parallel()
	current, excluded := gitRepo(t), gitRepo(t)
	result := []archive.ProjectActivation{
		{ProjectID: archive.ProjectID(current), Root: current, Included: true},
		{ProjectID: archive.ProjectID(excluded), Root: excluded},
	}
	known := []backfill.KnownProject{{Root: excluded, Sessions: 3}}
	var out bytes.Buffer
	projects, err := selectSetupProjects(newPrompter(strings.NewReader("specific\n2\n1 2\n\n1\n\n"), &out), result, result, known, current, "", nil)
	want := result
	if err != nil || !reflect.DeepEqual(projects, want) {
		t.Fatalf("projects=%+v err=%v\n%s", projects, err, &out)
	}
}

func TestStorageHelpReturnsToSelection(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	cfg, _, _, err := promptStorage(newPrompter(strings.NewReader("help\ns3-existing\nprofile\nbucket\nus-east-1\n"), &out), credentials.Config{}, Env{AWSProfiles: func() ([]AWSProfile, error) { return nil, nil }}, "")
	if err != nil || cfg.Provider != "s3" || !setupContainsText(out.String(), bucketDocURL) {
		t.Fatalf("cfg=%+v err=%v output=%s", cfg, err, &out)
	}
}

// The storage menu shown again after the instructions still offers them.
func TestStorageHelpStaysOnTheMenu(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	cfg, _, _, err := promptStorage(newPrompter(strings.NewReader("help\nhelp\ns3-existing\nprofile\nbucket\nus-east-1\n"), &out), credentials.Config{}, Env{AWSProfiles: func() ([]AWSProfile, error) { return nil, nil }}, "")
	if err != nil || cfg.Provider != "s3" || strings.Count(out.String(), bucketDocURL) != 2 || strings.Count(out.String(), "  [h] Setup instructions") != 3 {
		t.Fatalf("cfg=%+v err=%v output=%s", cfg, err, &out)
	}
}

func TestManualProjectsExpandInjectedHomeAndDeduplicateSymlinks(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	project := filepath.Join(home, "project")
	alias := filepath.Join(home, "alias")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(project, alias); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	projects, err := promptProjects(newPrompter(strings.NewReader("~/project\np\n~/alias\n\n"), &out), nil, nil, nil, home)
	canonical, _ := filepath.EvalSymlinks(project)
	if err != nil || len(projects) != 1 || projects[0].Root != canonical {
		t.Fatalf("projects=%+v err=%v", projects, err)
	}
}

// A project dropped on reconfigure and chosen again when setup asks for at
// least one keeps its activation time, so sessions already running in it
// still count. The review's projects edit goes through the same prompt.
func TestProjectChosenAgainAfterDroppingAllKeepsItsActivation(t *testing.T) {
	t.Parallel()
	root := gitRepo(t)
	activated := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	existing := []archive.ProjectActivation{{ProjectID: archive.ProjectID(root), Root: root, Included: true, ActivatedAt: activated}}
	var out bytes.Buffer
	projects, err := promptProjects(newPrompter(strings.NewReader("specific\n1\n\n1\n\n"), &out), existing, nil, nil)
	if err != nil || !reflect.DeepEqual(projects, existing) || !setupContainsText(out.String(), "choose at least one project") {
		t.Fatalf("projects=%+v err=%v\n%s", projects, err, &out)
	}
}

// In the list headed by the current repository, a number given twice in one
// answer, as in overlapping ranges, includes the project once rather than
// switching it back out.
func TestRepeatedNumberIncludesOnce(t *testing.T) {
	t.Parallel()
	current, one, two := gitRepo(t), gitRepo(t), gitRepo(t)
	result := []archive.ProjectActivation{{ProjectID: archive.ProjectID(current), Root: current, Included: true}}
	known := []backfill.KnownProject{{Root: one}, {Root: two}}
	var out bytes.Buffer
	projects, err := selectSetupProjects(newPrompter(strings.NewReader("specific\n2-3 3 2\n\n"), &out), result, nil, known, current, "", nil)
	if err != nil || includedProjects(projects) != 3 {
		t.Fatalf("projects=%+v err=%v\n%s", projects, err, &out)
	}
}

// A repository nested in the current one is part of it once the current one
// is configured, so its sessions count toward the current repository and
// it is not listed on its own.
func TestNestedProjectsFoldIntoTheCurrentRepository(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	known := []backfill.KnownProject{
		{Root: "/src/app/vendor/lib", Sessions: 2, LastUsed: now},
		{Root: "/src/other", Sessions: 1, LastUsed: now.Add(-time.Hour)},
		{Root: "/src/app", Sessions: 3, LastUsed: now.Add(-2 * time.Hour), Kind: backfill.ProjectKindRepository},
		{Root: "/src/application", Sessions: 4},
	}
	want := []backfill.KnownProject{
		{Root: "/src/app", Sessions: 5, LastUsed: now, Kind: backfill.ProjectKindRepository},
		{Root: "/src/other", Sessions: 1, LastUsed: now.Add(-time.Hour)},
		{Root: "/src/application", Sessions: 4},
	}
	var got []backfill.KnownProject
	for _, candidate := range setupProjectCandidates(nil, nil, known, "/src/app") {
		got = append(got, candidate.evidence)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

// With more projects included than the list shows, as after a large import,
// each answer reports a count rather than every path.
func TestSelectionWithManyProjectsPrintsACount(t *testing.T) {
	t.Parallel()
	roots := selectorRoots(t, maxKnownProjects+1)
	var result []archive.ProjectActivation
	for _, root := range roots {
		result = append(result, archive.ProjectActivation{ProjectID: archive.ProjectID(root.Root), Root: root.Root, Included: true})
	}
	known := []backfill.KnownProject{{Root: gitRepo(t)}}
	var out bytes.Buffer
	if _, err := selectSetupProjects(newPrompter(strings.NewReader("all\n"), &out), result, result, known, "", "", nil); err != nil {
		t.Fatal(err)
	}
	if !setupContainsText(out.String(), fmt.Sprintf("Found %d projects", maxKnownProjects+2)) || !setupContainsText(out.String(), "Showing 1–12") {
		t.Fatalf("output:\n%s", &out)
	}
}

// Compatible named answers must still leave resolved, human action receipts.
// Regression: 2026-10 setup review P2-R1-07.
func TestSetupReviewNamedAnswersHaveResolvedReceipts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		answer    string
		want      string
		blocked   bool
		installed bool
	}{{"yes", "Start archiving", false, false}, {"y", "Save changes", false, true}, {"n", "Cancel; keep draft", false, false}, {"c", "Check again", true, false}} {
		var out bytes.Buffer
		p := newPrompter(strings.NewReader(tc.answer+"\n"), &out)
		_, err := reviewAction(p, tc.installed, tc.blocked, false)
		p.close()
		must(t, err)
		if setupReceiptIndex(out.String(), tc.want) < 0 {
			t.Fatalf("alias %s receipt unresolved:\n%s", tc.answer, &out)
		}
	}
}
