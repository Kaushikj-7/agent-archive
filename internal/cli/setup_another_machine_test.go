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
	"github.com/wangjohn/agent-archive/internal/credentials"
)

// The line for another machine names the R2 key only by its environment
// variables, never by value, whether setup asked for it or read it.
func TestAnotherMachineCommandNeverCarriesTheR2Secret(t *testing.T) {
	t.Parallel()
	const secret, keyID = "private-secret-value", "PRIVATEKEYID"
	check := func(t *testing.T, out string) {
		t.Helper()
		if strings.Contains(out, secret) || strings.Contains(out, keyID) {
			t.Fatalf("output carries the key:\n%s", out)
		}
		want := "To set up another machine with this storage, set " + envR2AccessKeyID + " and\n" + envR2SecretAccessKey + " there, then run:\n  agent-archive setup --yes --provider r2 --bucket test-bucket --r2-account " + testR2Account + " --apps codex --codex-discovery on --codex-capture-scope included-projects --prefix agent-archive/ --retention-days 90 --no-require-skill-use --skill-evidence metadata --skills --project "
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
	t.Run("interactive", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now())
		input := strings.Join([]string{"y", "n", "n", "included-projects", t.TempDir(), "", "r2-existing", testR2Account, "test-bucket", keyID, secret, "y", "details"}, "\n") + "\n"
		check(t, setupRun(t, env, input, 0))
	})
	t.Run("yes", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		env := withEnvironment(setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now()), map[string]string{envR2AccessKeyID: keyID, envR2SecretAccessKey: secret})
		var out bytes.Buffer
		args := []string{"setup", "--yes", "--provider", "r2", "--r2-account", testR2Account, "--bucket", "test-bucket", "--apps", "codex", "--codex-discovery", "on", "--codex-capture-scope", "included-projects", "--project", t.TempDir()}
		if code := Run(args, strings.NewReader(""), &out, &out, env); code != 0 {
			t.Fatalf("exit %d\n%s", code, &out)
		}
		if strings.Contains(out.String(), secret) || strings.Contains(out.String(), keyID) || strings.Contains(out.String(), "To set up another machine with this storage") || !strings.Contains(out.String(), "Setup complete") {
			t.Fatalf("unsafe or expanded completion:\n%s", &out)
		}
		cfg, _, err := config.Load(home)
		must(t, err)
		out.Reset()
		printAnotherMachine(newPrompter(strings.NewReader(""), &out), cfg, "")
		check(t, out.String())
	})
}

// The command for another machine writes projects in the home folder from ~, and
// quotes what the shell would split.
func TestAnotherMachineCommand(t *testing.T) {
	t.Parallel()
	cfg := config.Config{RetentionDays: 90,
		Storage:   credentials.Config{Provider: credentials.ProviderS3, Bucket: "team-archive", AWSProfile: "work", Region: "us-east-1"},
		Harnesses: []string{"codex", "claude"},
		Archive: archive.Config{Projects: []archive.ProjectActivation{
			{Root: "/Users/alex/src/web app", Included: true},
			{Root: "/Users/alex/src/api", Included: false},
			{Root: "/Volumes/work/it's", Included: true},
			{Root: "/Users/alex", Included: true},
		}},
	}
	got := anotherMachineCommand(cfg, "/Users/alex")
	want := `agent-archive setup --yes --provider s3 --bucket team-archive --aws-profile work --region us-east-1 --apps codex,claude --codex-discovery off --codex-capture-scope included-projects --prefix agent-archive/ --retention-days 90 --no-require-skill-use --skill-evidence body --skills --project-scope-file - <<'AGENT_ARCHIVE_PROJECT_SCOPE'
[{"path":"~/src/web app","included":true},{"path":"~/src/api","included":false},{"path":"/Volumes/work/it's","included":true},{"path":"~","included":true}]
AGENT_ARCHIVE_PROJECT_SCOPE`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

// A project in the home folder is written from ~ even when the home folder's
// path runs through a symlink, as project roots are saved resolved.
func TestAnotherMachineCommandResolvesTheHomeFolder(t *testing.T) {
	t.Parallel()
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "home")
	must(t, os.Symlink(target, link))
	resolved, err := filepath.EvalSymlinks(target)
	must(t, err)
	cfg := config.Config{RetentionDays: 90,
		Storage:   credentials.Config{Provider: credentials.ProviderS3, Bucket: "b", AWSProfile: "p"},
		Harnesses: []string{"claude"},
		Archive:   archive.Config{Projects: []archive.ProjectActivation{{Root: filepath.Join(resolved, "src", "app"), Included: true}}},
	}
	if got := anotherMachineCommand(cfg, link); !strings.HasSuffix(got, " --project ~/src/app") {
		t.Fatalf("got %s", got)
	}
}

// The command for another machine names an R2 bucket on a custom endpoint by
// that endpoint, and says how to set a folder inside the bucket, which
// setup --yes cannot.
func TestAnotherMachineCommandCustomEndpointAndFolder(t *testing.T) {
	t.Parallel()
	endpoint := "https://" + testR2Account + ".eu.r2.cloudflarestorage.com"
	cfg := config.Config{RetentionDays: 90,
		Storage: credentials.Config{
			Provider: credentials.ProviderR2, Bucket: "b", Prefix: "team/",
			R2Endpoint: endpoint, R2CredentialRef: "ref-123",
		},
		Harnesses: []string{"claude"},
		Archive:   archive.Config{Projects: []archive.ProjectActivation{{Root: "/Users/alex/src/app", Included: true}}},
	}
	var out bytes.Buffer
	printAnotherMachine(newPrompter(strings.NewReader(""), &out), cfg, "/Users/alex")
	got := out.String()
	want := "  agent-archive setup --yes --provider r2 --bucket b --r2-account " + endpoint + " --apps claude --prefix team/ --retention-days 90 --no-require-skill-use --skill-evidence body --skills --project ~/src/app\n"
	if !strings.HasSuffix(got, want) || strings.Contains(got, "ref-123") {
		t.Fatalf("got:\n%s", got)
	}
	if loc, err := credentials.ParseR2Location(endpoint); err != nil || loc.Endpoint != endpoint {
		t.Fatalf("the endpoint does not read back: %+v %v", loc, err)
	}
}

func TestAnotherMachineCommandCarriesCapturePolicies(t *testing.T) {
	t.Parallel()
	cfg := config.Config{
		Storage:       credentials.Config{Provider: credentials.ProviderS3, Bucket: "b", Prefix: "private/", AWSProfile: "p"},
		RetentionDays: 14, RequireSkillUse: true, SkillEvidence: config.SkillEvidenceNone, NoSkills: true,
	}
	got := anotherMachineCommand(cfg, "")
	want := "--prefix private/ --retention-days 14 --require-skill-use --skill-evidence none --no-skills"
	if !strings.Contains(got, want) {
		t.Fatalf("capture policy missing: %s", got)
	}
}
