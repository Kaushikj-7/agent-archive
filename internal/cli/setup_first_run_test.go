package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
)

// testR2Account is a well-formed, made-up Cloudflare account ID.
const testR2Account = "0123456789abcdef0123456789abcdef"

// writeClaudeSession leaves a Claude Code transcript that ran in cwd, last
// changed at modified, where setup's project discovery finds it.
func writeClaudeSession(t *testing.T, userHome, id, cwd string, modified time.Time) {
	t.Helper()
	path := filepath.Join(userHome, ".claude", "projects", filepath.Base(cwd), id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, fmt.Appendf(nil, "{\"type\":\"user\",\"sessionId\":%q,\"cwd\":%q}\n", id, cwd), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
}

// gitRepo makes a resolved temporary folder that is a git repository.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Outside a repository, setup lists the projects the apps' history
// mentions, most recent first, and takes them by number; a path still works.
func TestSetupOffersProjectsFromAppHistory(t *testing.T) {
	t.Parallel()
	home, userHome := t.TempDir(), t.TempDir()
	older, newer, typed := gitRepo(t), gitRepo(t), gitRepo(t)
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	writeClaudeSession(t, userHome, "one", older, now.Add(-72*time.Hour))
	writeClaudeSession(t, userHome, "two", newer, now.Add(-time.Hour))
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), now)
	env.BackfillTempDirs = []string{}
	input := strings.Join([]string{"n", "y", "n", "specific", "9", "p", typed, "", "s3-existing", "b", "profile", "us-east-1", "y"}, "\n") + "\n"
	output := setupRun(t, env, input, 0)
	first, second := strings.Index(output, "1) "+newer), strings.Index(output, "2) "+older)
	if first < 0 || second < first || !setupContainsText(output, "today") || !setupContainsText(output, "3 days ago") || !setupContainsText(output, "enter project numbers") {
		t.Fatalf("projects not offered newest first:\n%s", output)
	}
	cfg, _, _ := config.Load(home)
	var roots []string
	for _, project := range cfg.Archive.Projects {
		roots = append(roots, project.Root)
	}
	if strings.Join(roots, ",") != strings.Join([]string{newer, older, typed}, ",") {
		t.Fatalf("projects %v", roots)
	}
}

// Inside a repository, setup lists it already included, followed by the
// projects the apps' history mentions with their session counts, so another
// project is one number away and a blank line keeps just the repository.
func TestSetupListsRecentProjectsAfterTheCurrentRepository(t *testing.T) {
	t.Parallel()
	for _, add := range []bool{false, true} {
		t.Run(strconv.FormatBool(add), func(t *testing.T) {
			t.Parallel()
			home, userHome, current, other := t.TempDir(), t.TempDir(), gitRepo(t), gitRepo(t)
			now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
			writeClaudeSession(t, userHome, "one", other, now.Add(-time.Hour))
			writeClaudeSession(t, userHome, "two", other, now.Add(-2*time.Hour))
			env := setupTestEnv(t, home, userHome, newFakeKeychain(), now)
			env.BackfillTempDirs = []string{}
			env.WorkingDir = func() (string, error) { return current, nil }
			answers := []string{"n", "y", "n", "specific", "2", ""}
			if add {
				answers = []string{"n", "y", "n", ""}
			}
			input := strings.Join(append(answers, "s3-existing", "b", "profile", "us-east-1", "y"), "\n") + "\n"
			output := setupRun(t, env, input, 0)
			cfg, _, _ := config.Load(home)
			want := 1
			if add {
				want = 2
			}
			if !setupContainsText(output, "1) "+current) || !setupContainsText(output, "2) "+other) || !setupContainsText(output, "2 sessions · today") ||
				setupContainsText(output, "Add another project?") || includedProjects(cfg.Archive.Projects) != want || cfg.Archive.Projects[0].Root != current {
				t.Fatalf("projects %+v\n%s", cfg.Archive.Projects, output)
			}
		})
	}
}

// Pasting the bucket URL Cloudflare shows fills in the account and the
// bucket, so the bucket is not asked for; a bad answer says what to paste.
func TestSetupTakesAccountAndBucketFromTheR2BucketURL(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	kc := newFakeKeychain()
	env := setupTestEnv(t, home, t.TempDir(), kc, time.Now())
	input := strings.Join([]string{"y", "n", "n", "included-projects", project, "", "r2-existing",
		"https://" + testR2Account + ".r2.cloudflarestorage.com/my-bucket/folder",
		"https://" + testR2Account + ".r2.cloudflarestorage.com/my-bucket",
		"ACCESS", "secret-value", "y"}, "\n") + "\n"
	output := setupRun(t, env, input, 0)
	if setupContainsText(output, "Bucket name") || !setupContainsText(output, "Bucket: my-bucket") || !setupContainsText(output, "only the bucket") {
		t.Fatalf("unexpected prompts:\n%s", output)
	}
	cfg, _, _ := config.Load(home)
	if cfg.Storage.Bucket != "my-bucket" || cfg.Storage.R2AccountID != testR2Account || cfg.Storage.R2Endpoint != "https://"+testR2Account+".r2.cloudflarestorage.com" {
		t.Fatalf("storage %+v", cfg.Storage)
	}
}

// Setup ends with one line per selected app on what it needs, then how to
// check progress.
func TestSetupNextStepsNameEachApp(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now())
	output := setupRun(t, env, s3SetupInput("b", "us-east-1", "profile", true, true, true, t.TempDir()), 0)
	for _, want := range []string{
		"Codex: run /hooks and approve the archive hooks; they capture sessions that start after that (or after /clear).",
		"Claude Code: nothing to approve; hooks capture sessions that start from now on (or after /clear).",
		"Cursor: nothing to approve; hooks capture Agent chats that start from now on.\n",
		"Check progress with agent-archive status.",
	} {
		if !setupContainsText(output, want) {
			t.Fatalf("missing %q:\n%s", want, output)
		}
	}
}

// R2 object keys can never read public-access settings, so the review
// reminds rather than warns; status keeps reporting not_verified.
func TestSetupReviewRemindsR2UsersToCheckPublicAccess(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now())
	output := setupRun(t, env, r2SetupInput(t.TempDir(), "secret-value"), 0)
	if setupContainsText(output, "public-access settings could not be read") || !setupContainsText(output, "! Bucket privacy unknown · check public access in the Cloudflare dashboard") {
		t.Fatalf("unexpected privacy note:\n%s", output)
	}
	cfg, _, _ := config.Load(home)
	if cfg.BucketPrivacy == nil || cfg.BucketPrivacy.State != "not_verified" || cfg.BucketPrivacy.Reason != r2PrivacyUnreadable {
		t.Fatalf("privacy evidence %+v", cfg.BucketPrivacy)
	}
}

// The Sessions row shows only when it is not the default, or when a
// reconfiguration changes it back to the default.
func TestSetupReviewShowsSessionsOnlyWhenNotTheDefault(t *testing.T) {
	t.Parallel()
	cfg := config.Config{Harnesses: []string{"codex"}, RetentionDays: 90}
	skills := cfg
	skills.RequireSkillUse = true
	for _, tc := range []struct {
		name          string
		next          config.Config
		current       config.Config
		reconfiguring bool
		want          string
	}{
		{"default", cfg, config.Config{}, false, ""},
		{"skills only", skills, config.Config{}, false, "Only new sessions that use skills"},
		{"back to all", cfg, skills, true, "All new sessions, with or without skills"},
	} {
		var out strings.Builder
		showSetupReview(newPrompter(strings.NewReader(""), &out), tc.next, setupReview{existing: tc.current, reconfiguring: tc.reconfiguring})
		if got := out.String(); tc.want == "" && setupContainsText(got, "Sessions") || tc.want != "" && !setupContainsText(got, tc.want) {
			t.Errorf("%s: review:\n%s", tc.name, got)
		}
	}
}

// A number or range beyond the list is refused without being expanded, so a
// typo such as 1-999999999 costs nothing.
func TestParseNumbersBoundsRangesBeforeExpanding(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		answer      string
		numbers     []int
		ok          bool
		inRangeWant bool
	}{
		{"1 3", []int{1, 3}, true, true},
		{"2-4,1", []int{2, 3, 4, 1}, true, true},
		{"1-999999999", nil, true, false},
		{"0", nil, true, false},
		{"~/src", nil, false, false},
		{"3-1", nil, false, false},
	} {
		numbers, ok, inRange := parseNumbers(tc.answer, 5)
		if ok != tc.ok || inRange != tc.inRangeWant || tc.inRangeWant && !slices.Equal(numbers, tc.numbers) {
			t.Errorf("parseNumbers(%q) = %v, %v, %v", tc.answer, numbers, ok, inRange)
		}
	}
}

// After a plain uninstall the configuration stays with archiving disabled;
// with no arguments, agent-archive says it is not set up.
func TestNoArgsSaysNotSetUpAfterUninstall(t *testing.T) {
	t.Parallel()
	_, _, env := installedFixture(t, newFakeKeychain(), s3SetupInput("b", "us-east-1", "p", true, false, false, t.TempDir()))
	if code := Run([]string{"uninstall", "--yes"}, nil, io.Discard, io.Discard, env); code != 0 {
		t.Fatalf("uninstall: exit %d", code)
	}
	var out strings.Builder
	if code := Run(nil, nil, &out, nil, env); code != 0 || !strings.HasPrefix(out.String(), "Not set up yet") {
		t.Fatalf("exit %d\n%s", code, &out)
	}
}
