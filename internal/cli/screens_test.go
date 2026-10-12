package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/discovery"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

// The screen goldens record what setup and status print, whole, for the
// screens a user meets most. Each screen is recorded twice under
// testdata/screens/: NAME.txt as a pipe or NO_COLOR shows it, and
// NAME.color.txt as a color terminal shows it, with each escape character
// written \e so the codes read as text. The answers setup reads are echoed
// after their prompts, as a terminal would show them. Paths are fixed: the
// home folder is /Users/alex, the clock is 2026-09-25 12:00 UTC. A screen
// that ends in "no more input" stopped where its answers did.
//
// A change to what setup or status prints changes these files. Rewrite them
// and read the diff:
//
//	go test ./internal/cli -run Screens -update
//	git diff internal/cli/testdata/screens
func TestScreens(t *testing.T) {
	t.Parallel()
	for _, sc := range screens {
		for _, color := range []bool{false, true} {
			name := sc.name + ".txt"
			if color {
				name = sc.name + ".color.txt"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				f := newScreenFixture(t)
				if sc.arrange != nil {
					sc.arrange(t, f)
				}
				args := sc.args
				if args == nil {
					args = []string{"setup"}
				}
				got := f.run(t, args, sc.answers, color, sc.exit)
				golden.Check(t, filepath.Join("testdata", "screens", name), got)
			})
		}
	}
}

// screenNow is the screens' clock.
var screenNow = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

// screenHome is the home folder the screens show.
const screenHome = "/Users/alex"

type screen struct {
	name string
	// args default to setup.
	args    []string
	answers []string
	exit    int
	// arrange prepares the Mac and the data directory before the recorded
	// run.
	arrange func(*testing.T, *screenFixture)
}

var screens = []screen{
	{
		name:    "setup-codex-all-projects-interactive",
		answers: []string{"", "all-projects", "done", "s3-existing", "work", "2", ""},
		arrange: func(t *testing.T, f *screenFixture) {
			t.Helper()
			f.withApps(t, "codex")
		},
	},
	{
		name: "setup-codex-all-projects-hook-only",
		args: []string{"setup", "--yes", "--apps", "codex", "--provider", "s3", "--bucket", "team-archive", "--aws-profile", "work", "--region", "us-east-1", "--codex-discovery", "off", "--codex-capture-scope", "all-projects"},
		arrange: func(t *testing.T, f *screenFixture) {
			t.Helper()
			f.withApps(t, "codex")
		},
	},
	{
		name: "status-codex-all-projects-mixed-formats",
		args: []string{"status"},
		arrange: func(t *testing.T, f *screenFixture) {
			t.Helper()
			f.withApps(t, "codex")
			setupYes(t, f.env, "", 0, "--yes", "--apps", "codex", "--provider", "s3", "--bucket", "team-archive", "--aws-profile", "work", "--region", "us-east-1", "--codex-discovery", "on", "--codex-capture-scope", "all-projects")
			must(t, local.Write(filepath.Join(f.home, "discovery-health.json"), struct {
				Version int              `json:"version"`
				Health  discovery.Health `json:"health"`
			}{Version: 1, Health: discovery.Health{Supported: true, LastAttempt: screenNow.Add(-time.Minute), Pending: true, Outcomes: map[string]int{"native_format": 2, "unsupported_producer": 1, "invalid_metadata": 1}}}))
		},
	},

	{
		// A first run on a Mac with all three apps, from inside a Git
		// repository, through to the next steps.
		name:    "setup-fresh-apps-git-cwd",
		answers: []string{"", "included-projects", "", "s3-existing", "work", "2", ""},
		arrange: func(t *testing.T, f *screenFixture) {
			t.Helper()
			f.withApps(t, "codex", "claude", "cursor")
			f.inWebApp(t)
		},
	},
	{
		// A first run on a Mac with none of the apps, outside any
		// repository, up to the storage question.
		name:    "setup-fresh-no-apps",
		answers: []string{"y", "n", "n", "included-projects", "~/src/web-app", ""},
		exit:    1,
		arrange: func(t *testing.T, f *screenFixture) {
			t.Helper()
			f.project(t, "src/web-app")
		},
	},
	{
		// Outside a repository, setup offers the projects the apps'
		// history mentions, up to the storage question.
		name:    "setup-recent-projects",
		answers: []string{"", "specific", "3", ""},
		exit:    1,
		arrange: func(t *testing.T, f *screenFixture) {
			t.Helper()
			f.withApps(t, "claude")
			writeClaudeSession(t, f.userHome, "one", f.project(t, "src/api"), screenNow.Add(-72*time.Hour))
			writeClaudeSession(t, f.userHome, "two", f.project(t, "src/web-app"), screenNow.Add(-time.Hour))
			writeClaudeSession(t, f.userHome, "three", f.project(t, "src/docs"), screenNow.Add(-40*24*time.Hour))
		},
	},
	{
		// Inside a repository, the recent-projects list starts with it,
		// included, and each project's session count.
		name:    "setup-recent-projects-git-cwd",
		answers: []string{"", "specific", "3", ""},
		exit:    1,
		arrange: func(t *testing.T, f *screenFixture) {
			t.Helper()
			f.withApps(t, "claude")
			f.inWebApp(t)
			writeClaudeSession(t, f.userHome, "one", f.project(t, "src/api"), screenNow.Add(-72*time.Hour))
			writeClaudeSession(t, f.userHome, "two", f.project(t, "src/web-app"), screenNow.Add(-time.Hour))
			writeClaudeSession(t, f.userHome, "three", f.project(t, "src/web-app"), screenNow.Add(-2*time.Hour))
			writeClaudeSession(t, f.userHome, "four", f.project(t, "src/docs"), screenNow.Add(-40*24*time.Hour))
		},
	},
	{
		// Leaving every project out asks again; a includes them all.
		name:    "setup-recent-projects-none-left",
		answers: []string{"", "specific", "1 2", "", "a", ""},
		exit:    1,
		arrange: func(t *testing.T, f *screenFixture) {
			t.Helper()
			f.withApps(t, "claude")
			f.inWebApp(t)
			writeClaudeSession(t, f.userHome, "one", f.project(t, "src/api"), screenNow.Add(-72*time.Hour))
		},
	},
	{
		// A setup left after its first step offers to continue.
		name:    "setup-resume-menu",
		answers: []string{"1", "s3-existing", "work", "2", "q"},
		arrange: func(t *testing.T, f *screenFixture) {
			t.Helper()
			f.withApps(t, "codex", "claude")
			f.inWebApp(t)
			f.setup(t, 1, "", "included-projects", "", "")
		},
	},
	{
		// With the AWS profile chosen, setup lists its buckets, pre-selects
		// the one named like agent-archive*, and uses that bucket's own
		// region rather than the profile's.
		name:    "setup-s3-bucket-list",
		answers: []string{"y", "n", "n", "included-projects", "", "s3", "e", "", "", "q"},
		arrange: func(t *testing.T, f *screenFixture) {
			t.Helper()
			f.inWebApp(t)
			f.env.AWSProfiles = func() ([]AWSProfile, error) {
				return []AWSProfile{{Name: "default", Region: "us-east-1"}, {Name: "personal", NoCredentials: true}}, nil
			}
			f.env.AWSBuckets = fakeBuckets{names: []string{"agent-archive-alex", "photos", "team-archive"}, regions: map[string]string{"agent-archive-alex": "eu-west-2"}}.open
		},
	},
	{
		// S3 refuses both lookups, so setup says why and asks for the
		// bucket and region, turning away a path typed as the region.
		name:    "setup-s3-bucket-typed",
		answers: []string{"y", "n", "n", "included-projects", "", "s3-existing", "work", "team-archive", "~/code/api", "us-east-1", "q"},
		arrange: func(t *testing.T, f *screenFixture) {
			t.Helper()
			f.inWebApp(t)
			f.env.AWSBuckets = fakeBuckets{listErr: errAccessDenied, regionErr: errAccessDenied}.open
		},
	},
	{
		// Claude Code's settings.json holds a comment, so setup stops
		// before its first question and says where and how to fix it.
		name: "setup-preflight-blocked",
		exit: 1,
		arrange: func(t *testing.T, f *screenFixture) {
			t.Helper()
			f.withApps(t, "codex", "claude")
			settings := filepath.Join(f.userHome, ".claude", "settings.json")
			must(t, os.MkdirAll(filepath.Dir(settings), 0o700))
			must(t, os.WriteFile(settings, []byte("{\n  // my model\n  \"model\": \"opus\"\n}\n"), 0o600))
		},
	},
	{
		// setup --yes makes the same checks first.
		name: "setup-yes-preflight",
		args: []string{"setup", "--yes", "--apps", "codex", "--codex-discovery", "on", "--codex-capture-scope", "included-projects", "--provider", "s3", "--bucket", "team-archive", "--aws-profile", "work", "--region", "us-east-1", "--project", "~/src/web-app"},
		arrange: func(t *testing.T, f *screenFixture) {
			t.Helper()
			f.withApps(t, "codex")
			f.project(t, "src/web-app")
		},
	},
	{
		// Setup on an installed Mac asks what to change; this run leaves
		// at the review.
		name:    "setup-reconfigure-menu",
		answers: []string{"3", "custom", "30", "q"},
		arrange: func(t *testing.T, f *screenFixture) {
			t.Helper()
			f.installed(t)
		},
	},
	{
		// Setup on an installed Mac can leave without changing anything.
		name:    "setup-reconfigure-exit",
		answers: []string{"5"},
		arrange: func(t *testing.T, f *screenFixture) {
			t.Helper()
			f.installed(t)
		},
	},
	{
		// Changing only storage on an installed Mac: its headings do not
		// count steps. This run leaves at the review.
		name:    "setup-reconfigure-storage",
		answers: []string{"2", "", "q"},
		arrange: func(t *testing.T, f *screenFixture) {
			t.Helper()
			f.installed(t)
		},
	},
	{
		// The storage check fails for want of AWS credentials: setup says
		// why, and how to fix it, and offers the fix first. This run stops
		// there.
		name:    "setup-storage-failure",
		answers: storageFailureAnswers,
		exit:    1,
		arrange: failUploads(&smithy.OperationError{ServiceID: "S3", OperationName: "PutObject", Err: &smithy.OperationError{ServiceID: "ec2imds", OperationName: "GetMetadata", Err: errors.New("dial tcp 169.254.169.254:80: connect: host is down")}}),
	},
	{
		// The same failure with --verbose: the storage error is printed
		// under the diagnosis, once.
		name:    "setup-storage-failure-verbose",
		args:    []string{"setup", "--verbose"},
		answers: storageFailureAnswers,
		exit:    1,
		arrange: failUploads(&smithy.OperationError{ServiceID: "S3", OperationName: "PutObject", Err: &smithy.GenericAPIError{Code: "AccessDenied", Message: "Access Denied"}}),
	},
	{
		// After a wrong-region failure that named no region, continuing
		// asks the storage questions again. S3 can't say where the bucket
		// is, so rather than check the region that failed again, setup
		// says so and asks for the region. The check passes; this run
		// leaves at the review.
		name:    "setup-storage-failure-continue",
		answers: []string{"1", "s3-existing", "", "", "eu-west-1", "q"},
		arrange: func(t *testing.T, f *screenFixture) {
			t.Helper()
			f.inWebApp(t)
			f.env.OpenStore = func(cfg config.Config) (storage.ObjectStore, error) {
				var err error
				if cfg.Storage.Region != "eu-west-1" {
					err = &smithyhttp.ResponseError{
						Response: &smithyhttp.Response{Response: &http.Response{StatusCode: http.StatusMovedPermanently, Header: http.Header{}}},
						Err:      &smithy.GenericAPIError{Code: "PermanentRedirect"},
					}
				}
				return putErrorStore{f.bucket, &err}, nil
			}
			f.setup(t, 1, storageFailureAnswers...)
			// S3 now refuses the region lookup, so the region is asked.
			f.env.AWSBuckets = fakeBuckets{names: []string{"photos", "team-archive"}, regionErr: errAccessDenied}.open
		},
	},
	{
		name:    "setup-storage-failure-access-denied",
		answers: storageFailureAnswers,
		exit:    1,
		arrange: failUploads(&smithy.GenericAPIError{Code: "AccessDenied"}),
	},
	{
		name:    "setup-storage-failure-no-such-bucket",
		answers: storageFailureAnswers,
		exit:    1,
		arrange: failUploads(&smithy.GenericAPIError{Code: "NoSuchBucket"}),
	},
	{
		name:    "setup-storage-failure-wrong-region",
		answers: storageFailureAnswers,
		exit:    1,
		arrange: failUploads(&smithyhttp.ResponseError{
			Response: &smithyhttp.Response{Response: &http.Response{StatusCode: http.StatusMovedPermanently, Header: http.Header{"X-Amz-Bucket-Region": {"eu-west-1"}}}},
			Err:      &smithy.GenericAPIError{Code: "PermanentRedirect"},
		}),
	},
	{
		name:    "setup-storage-failure-network",
		answers: storageFailureAnswers,
		exit:    1,
		arrange: failUploads(&smithyhttp.RequestSendError{Err: errors.New("dial tcp: lookup team-archive.s3.us-east-1.amazonaws.com: no such host")}),
	},
	{
		// A failure the provider did not answer, and Diagnose does not
		// recognize.
		name:    "setup-storage-failure-other",
		answers: storageFailureAnswers,
		exit:    1,
		arrange: failUploads(errors.New("incorrect region or folder")),
	},
	{
		// The object the check wrote read back changed.
		name:    "setup-storage-failure-read-back",
		answers: storageFailureAnswers,
		exit:    1,
		arrange: func(t *testing.T, f *screenFixture) {
			t.Helper()
			f.inWebApp(t)
			f.env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
				return changedReadStore{f.bucket}, nil
			}
		},
	},
	{
		// The review before a first setup commits, to a bucket that blocks
		// public access, cancelled there.
		name:    "setup-review-fresh",
		answers: []string{"y", "included-projects", "", "s3-existing", "work", "2", "q"},
		arrange: func(t *testing.T, f *screenFixture) {
			t.Helper()
			f.withApps(t, "codex", "claude")
			f.inWebApp(t)
			f.bucketPrivacy("verified_private", "all_bucket_public_access_blocks_enabled")
		},
	},
	{
		// The review before a first setup to Cloudflare R2, whose keys
		// cannot read public-access settings, cancelled there.
		name:    "setup-review-fresh-r2",
		answers: []string{"", "", "r2-existing", "0123456789abcdef0123456789abcdef", "team-archive", "ACCESSKEYID", "SECRET", "q"},
		arrange: func(t *testing.T, f *screenFixture) {
			t.Helper()
			f.withApps(t, "claude")
			f.inWebApp(t)
		},
	},
	{
		// The review when the bucket allows public access, cancelled
		// there.
		name:    "setup-review-public-bucket",
		answers: []string{"", "", "s3-existing", "work", "2", "q"},
		arrange: func(t *testing.T, f *screenFixture) {
			t.Helper()
			f.withApps(t, "claude")
			f.inWebApp(t)
			f.bucketPrivacy("public_or_risky", "public_bucket_policy")
		},
	},
	{
		// Reconfiguring from a shell whose CODEX_HOME differs from when
		// setup ran: the review warns that the hooks move, and stops there.
		name:    "setup-review-hooks-move",
		answers: []string{"3", "90-days", "q"},
		arrange: func(t *testing.T, f *screenFixture) {
			t.Helper()
			f.installed(t)
			elsewhere := filepath.Join(f.userHome, "codex-elsewhere")
			must(t, os.MkdirAll(elsewhere, 0o700))
			f.env.LookupEnv = func(k string) (string, bool) { return elsewhere, k == "CODEX_HOME" }
		},
	},
	{
		// The review of a reconfiguration that changes apps, projects and
		// retention, saved.
		name:    "setup-review-reconfigure-changes",
		answers: []string{"4", "y", "y", "p", "~/src/api", "", "", "edit", "retention", "custom", "30", ""},
		arrange: func(t *testing.T, f *screenFixture) {
			t.Helper()
			f.installed(t)
			f.withApps(t, "codex", "claude", "cursor")
			f.project(t, "src/api")
		},
	},
	{
		// What a committed first setup ends with.
		name:    "setup-next-steps",
		answers: []string{"y", "y", "y", "included-projects", "", "s3-existing", "work", "2", ""},
		arrange: func(t *testing.T, f *screenFixture) {
			t.Helper()
			f.inWebApp(t)
		},
	},
	{
		// A first setup whose project has past sessions imports those of
		// the last 7 days without asking, and names the older ones.
		name:    "setup-recent-import",
		answers: []string{"", "", "s3-existing", "work", "2", "", ""},
		arrange: func(t *testing.T, f *screenFixture) {
			t.Helper()
			f.withApps(t, "claude")
			f.inWebApp(t)
			f.pastSession(t, "one", "src/web-app", screenNow.Add(-72*time.Hour))
			f.pastSession(t, "two", "src/web-app", screenNow.Add(-2*time.Hour))
			f.pastSession(t, "old", "src/web-app", screenNow.Add(-9*24*time.Hour))
		},
	},
	{
		// setup --yes asks nothing: it points at backfill instead.
		name: "setup-yes-next-steps",
		args: []string{"setup", "--yes", "--provider", "s3", "--bucket", "team-archive", "--aws-profile", "work", "--region", "us-east-1", "--apps", "claude", "--project", "~/src/web-app"},
		arrange: func(t *testing.T, f *screenFixture) {
			t.Helper()
			f.project(t, "src/web-app")
			f.pastSession(t, "one", "src/web-app", screenNow.Add(-72*time.Hour))
		},
	},
	{
		// setup --yes refuses a public bucket, as the review's ✗ row
		// blocks interactive setup, before anything is committed.
		name: "setup-yes-public-bucket",
		args: []string{"setup", "--yes", "--provider", "s3", "--bucket", "team-archive", "--aws-profile", "work", "--region", "us-east-1", "--apps", "claude", "--project", "~/src/web-app"},
		exit: 1,
		arrange: func(t *testing.T, f *screenFixture) {
			t.Helper()
			f.project(t, "src/web-app")
			f.bucketPrivacy("public_or_risky", "public_bucket_policy")
		},
	},
	{
		// Status after a session was captured and published.
		name: "status-ready",
		args: []string{"status"},
		arrange: func(t *testing.T, f *screenFixture) {
			t.Helper()
			f.installed(t)
			f.published(t)
		},
	},
	{
		// Status before setup.
		name: "status-not-set-up",
		args: []string{"status"},
	},
	{
		// Status after setup, before the first session.
		name: "status-waiting",
		args: []string{"status"},
		arrange: func(t *testing.T, f *screenFixture) {
			t.Helper()
			f.installed(t)
		},
	},
	{
		// Status while collection is paused.
		name: "status-paused",
		args: []string{"status"},
		arrange: func(t *testing.T, f *screenFixture) {
			t.Helper()
			f.installed(t)
			f.published(t)
			f.env.Now = func() time.Time { return screenNow.Add(time.Minute) }
			var out bytes.Buffer
			if code := Run([]string{"pause"}, strings.NewReader(""), &out, &out, f.env); code != 0 {
				t.Fatalf("pause exit %d\n%s", code, &out)
			}
		},
	},
	{
		// Status when the last pass could not reach storage.
		name: "status-needs-attention",
		args: []string{"status"},
		arrange: func(t *testing.T, f *screenFixture) {
			t.Helper()
			f.installed(t)
			f.published(t)
			store, err := state.Open(f.home)
			must(t, err)
			status, err := store.LoadStatus()
			must(t, err)
			status.SetLastErrors("list registrations: AccessDenied: Access Denied")
			must(t, store.SaveStatus(status))
		},
	},
	{
		// status --verbose after a session was captured and published.
		name: "status-ready-verbose",
		args: []string{"status", "--verbose"},
		arrange: func(t *testing.T, f *screenFixture) {
			t.Helper()
			f.installed(t)
			f.published(t)
		},
	},
	{
		// status --verbose when the last pass could not reach storage.
		name: "status-needs-attention-verbose",
		args: []string{"status", "--verbose"},
		arrange: func(t *testing.T, f *screenFixture) {
			t.Helper()
			f.installed(t)
			f.published(t)
			store, err := state.Open(f.home)
			must(t, err)
			status, err := store.LoadStatus()
			must(t, err)
			status.SetLastErrors("list registrations: AccessDenied: Access Denied")
			must(t, store.SaveStatus(status))
		},
	},
}

// storageFailureAnswers set up Codex in ~/src/web-app with S3 storage, and
// stop at the storage check's failure menu.
var storageFailureAnswers = []string{"y", "n", "n", "included-projects", "", "s3-existing", "work", "2", "q"}

// failUploads makes the bucket refuse every upload with err.
func failUploads(err error) func(*testing.T, *screenFixture) {
	return func(t *testing.T, f *screenFixture) {
		t.Helper()
		f.inWebApp(t)
		f.env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
			return putErrorStore{f.bucket, &err}, nil
		}
	}
}

// bucketPrivacy makes the bucket report its public-access settings as
// state, for reason.
func (f *screenFixture) bucketPrivacy(state, reason string) {
	f.env.OpenStore = func(cfg config.Config) (storage.ObjectStore, error) {
		report := storage.UnknownPrivacy(cfg.Storage.Provider)
		report.State, report.Reason = state, reason
		return privacyReportStore{ObjectStore: f.bucket, report: report}, nil
	}
}

// privacyReportStore reports fixed public-access settings.
type privacyReportStore struct {
	storage.ObjectStore
	report storage.PrivacyReport
}

func (s privacyReportStore) InspectPrivacy(context.Context) storage.PrivacyReport { return s.report }

// changedReadStore reads back other bytes than were written.
type changedReadStore struct{ storage.ObjectStore }

func (changedReadStore) Get(context.Context, string) ([]byte, error) {
	return []byte("changed"), nil
}

// screenFixture is one screen's Mac: a home folder at root/Users/alex
// (shown as /Users/alex), its data directory in ~/.agent-archive, and an
// in-memory bucket.
type screenFixture struct {
	root     string
	userHome string
	home     string
	env      Env
	bucket   storage.ObjectStore
}

func newScreenFixture(t *testing.T) *screenFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	userHome := filepath.Join(root, "Users", "alex")
	home := filepath.Join(userHome, ".agent-archive")
	must(t, os.MkdirAll(home, 0o700))
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), screenNow)
	account := filepath.Join(root, "Users", "account")
	must(t, os.MkdirAll(account, 0o700))
	env.AccountHome = func() (string, error) { return account, nil }
	executable := filepath.Join(root, "opt", "homebrew", "bin", "agent-archive")
	must(t, os.MkdirAll(filepath.Dir(executable), 0o755))
	must(t, os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	env.Executable = func() (string, error) { return executable, nil }
	// The temporary folder is under root too, so normalize hides it if a
	// screen ever prints it.
	tempDir := filepath.Join(root, "tmp")
	must(t, os.MkdirAll(tempDir, 0o700))
	env.TempDir = func() string { return tempDir }
	// Backfill and setup's project list skip that folder alone, not the test
	// run's temporary folder that holds the fixture's repositories.
	env.BackfillTempDirs = []string{tempDir}
	f := &screenFixture{root: root, userHome: userHome, home: home, env: env, bucket: storagetest.NewMemoryStore()}
	f.env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return f.bucket, nil }
	// The profile setup is given can list two buckets; team-archive is in
	// us-east-1.
	f.env.AWSBuckets = fakeBuckets{names: []string{"photos", "team-archive"}, regions: map[string]string{"team-archive": "us-east-1"}}.open
	f.env.IsTerminal = func(stream any) bool {
		switch stream.(type) {
		case *strings.Reader, *echoAnswers:
			return true
		}
		return false
	}
	return f
}

// project makes a Git repository at rel under the home folder.
func (f *screenFixture) project(t *testing.T, rel string) string {
	t.Helper()
	dir := filepath.Join(f.userHome, rel)
	must(t, os.MkdirAll(filepath.Join(dir, ".git"), 0o700))
	return dir
}

// inWebApp makes the Git repository ~/src/web-app and runs the recorded
// command from inside it.
func (f *screenFixture) inWebApp(t *testing.T) {
	t.Helper()
	dir := f.project(t, "src/web-app")
	f.env.WorkingDir = func() (string, error) { return dir, nil }
}

// withApps makes setup find these apps installed, at fixed versions.
func (f *screenFixture) withApps(t *testing.T, apps ...string) {
	t.Helper()
	versions := map[string]string{"codex": "0.121.0", "claude": "2.1.90", "cursor": "3.21.13"}
	for _, app := range apps {
		if versions[app] == "" {
			t.Fatalf("withApps: no fixed version for app %q", app)
		}
	}
	f.env.DetectHarnesses = func(string) []string { return apps }
	f.env.DiscoverApplications = func(string) map[string]applicationDiscovery {
		found := map[string]applicationDiscovery{}
		for _, app := range apps {
			found[app] = applicationDiscovery{Installed: true, Version: versions[app], VersionSource: "test", VersionKind: versionKindCLI, VersionState: "known", ObservedAt: screenNow}
		}
		return found
	}
}

// pastSession writes a Claude Code session with a conversation, started at
// start in the project at rel under the home folder. Its file is padded to a
// fixed size, so the sizes an import prints do not depend on the temporary
// folder's path.
func (f *screenFixture) pastSession(t *testing.T, id, rel string, start time.Time) {
	t.Helper()
	cwd := f.project(t, rel)
	records := fmt.Sprintf(`{"type":"user","uuid":"a","sessionId":%q,"cwd":%q,"timestamp":%q,"message":{"role":"user","content":"please check it"}}
{"type":"assistant","uuid":"b","sessionId":%q,"timestamp":%q,"message":{"role":"assistant","content":[{"type":"text","text":"Checked."}]}}
`, id, cwd, start.Format(time.RFC3339), id, start.Add(time.Minute).Format(time.RFC3339))
	const size = 2048
	pad := `{"pad":"` + strings.Repeat("x", size-len(records)-len(`{"pad":"",`)) + `",`
	content := strings.Replace(records, "{", pad, 1)
	path := filepath.Join(f.userHome, ".claude", "projects", strings.ReplaceAll(rel, "/", "-"), id+".jsonl")
	must(t, os.MkdirAll(filepath.Dir(path), 0o700))
	must(t, os.WriteFile(path, []byte(content), 0o600))
	written := start.Add(time.Hour)
	must(t, os.Chtimes(path, written, written))
}

// setup runs setup unrecorded with these answers.
func (f *screenFixture) setup(t *testing.T, exit int, answers ...string) {
	t.Helper()
	input := strings.Join(answers, "\n") + "\n"
	var out bytes.Buffer
	if code := Run([]string{"setup"}, strings.NewReader(input), &out, &out, f.env); code != exit {
		t.Fatalf("setup exit %d want %d\n%s", code, exit, &out)
	}
}

// installed sets the Mac up for Codex in ~/src/web-app, storing in S3.
func (f *screenFixture) installed(t *testing.T) {
	t.Helper()
	project := f.project(t, "src/web-app")
	f.setup(t, 0, "y", "n", "n", "included-projects", project, "", "s3-existing", "work", "2", "")
}

// published captures and publishes one Codex session in ~/src/web-app.
func (f *screenFixture) published(t *testing.T) {
	t.Helper()
	project := f.project(t, "src/web-app")
	path := writeCodexTranscript(t, project)
	must(t, handleTestHookEvent(f.home, "codex", map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native", "cwd": project, "transcript_path": path}, screenNow))
	result, err := runOnePass(f.env, false)
	if err != nil || len(result.Published) != 1 {
		t.Fatalf("publish: %+v %v", result, err)
	}
}

// run runs args with the answers, recording stdout and stderr together, and
// returns the screen as its golden file holds it.
func (f *screenFixture) run(t *testing.T, args, answers []string, color bool, exit int) []byte {
	t.Helper()
	out := screenOutput{color: color}
	in := &echoAnswers{answers: answers, echo: &out}
	code := Run(args, in, &out, &out, f.env)
	screen := f.normalize(out.String())
	if color {
		screen = strings.ReplaceAll(screen, "\x1b", `\e`)
	}
	if code != exit {
		t.Fatalf("%s: exit %d want %d\n%s", strings.Join(args, " "), code, exit, screen)
	}
	if in.next < len(answers) {
		t.Fatalf("%s: %d answer(s) left unread\n%s", strings.Join(args, " "), len(answers)-in.next, screen)
	}
	header := fmt.Sprintf("$ agent-archive %s\n", strings.Join(args, " "))
	footer := fmt.Sprintf("[exit %d]\n", code)
	return []byte(header + screen + footer)
}

// setupKeyPattern matches the random name of the object setup's storage
// check writes.
var setupKeyPattern = regexp.MustCompile(`\.setup-test/[0-9a-f]+\.json`)

// normalize replaces what changes from run to run: the temporary folders
// and the storage check's object name.
func (f *screenFixture) normalize(s string) string {
	s = strings.ReplaceAll(s, f.userHome, screenHome)
	s = strings.ReplaceAll(s, f.root, "")
	return trimScreenLineEnds(setupKeyPattern.ReplaceAllString(s, ".setup-test/KEY.json"))
}

// screenOutput is stdout and stderr together, a color terminal or not.
type screenOutput struct {
	bytes.Buffer
	color bool
}

func (o *screenOutput) colorTerminal() bool { return o.color }

// Screen transcripts model static terminal history with explicit dimensions.
func (o *screenOutput) promptCapabilities() promptCapabilities {
	return promptCapabilities{Color: o.color, InputTerminal: true, OutputTerminal: true, SharedTerminal: true, Width: 80, Height: 24}
}

// echoAnswers hands setup one answer per read and echoes it to the screen,
// as a terminal shows what the user typed after the prompt.
type echoAnswers struct {
	answers []string
	next    int
	echo    *screenOutput
	hidden  bool
}

func (e *echoAnswers) Read(p []byte) (int, error) {
	if e.next == len(e.answers) {
		return 0, io.EOF
	}
	line := e.answers[e.next] + "\n"
	if len(p) < len(line) {
		return 0, errors.New("screen answer longer than the read buffer")
	}
	e.next++
	if !e.hidden {
		e.echo.WriteString(line)
	}
	return copy(p, line), nil
}

func (e *echoAnswers) hidePromptEcho() func() { e.hidden = true; return func() { e.hidden = false } }

// trimScreenLineEnds records visible cells rather than cursor padding after a
// hidden or redirected answer. Keep the live cursor's trailing space in output.
func trimScreenLineEnds(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t")
	}
	return strings.Join(lines, "\n")
}
