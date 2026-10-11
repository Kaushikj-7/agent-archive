package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/pairing"
	"github.com/wangjohn/agent-archive/internal/scheduler"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestPairingTransferPaneMatchesFixture(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	var out bytes.Buffer
	printPairingTransfer(&out, "/source/Downloads/synthetic.txt", false, pairingLedger{Name: "laptop", ExpiresAt: now.Add(15 * time.Minute)}, now)
	want, err := os.ReadFile("testdata/pairing-transfer.txt")
	must(t, err)
	if out.String() != string(want) {
		t.Fatalf("pairing transfer pane changed:\n%s", &out)
	}
}

func TestPairingFailureNeverClaimsCommittedSuccessAndCanRetry(t *testing.T) {
	type pairingFailure string
	const (
		wrongCode      pairingFailure = "wrong-code"
		expiredBundle  pairingFailure = "expired"
		storageFailure pairingFailure = "storage"
		commitFailure  pairingFailure = "commit"
	)
	for _, failure := range []pairingFailure{wrongCode, expiredBundle, storageFailure, commitFailure} {
		t.Run(string(failure), func(t *testing.T) {
			now := time.Now().UTC()
			payload := pairing.Payload{Version: 1, PairingID: strings.Repeat("1", 32), RecipientID: strings.Repeat("2", 32), IssuerID: strings.Repeat("3", 32), IssuerName: "studio", Name: "laptop", CreatedAt: now, ExpiresAt: now.Add(15 * time.Minute), Storage: pairing.Storage{Provider: "s3", Bucket: "synthetic", AWSProfile: "archive", Region: "us-east-1"}, Apps: []string{"codex"}, RetentionDays: 90, SkillEvidence: "metadata"}
			code := "aardvark-abandoned-abbreviate-abdomen-abhorrence-abiding"
			bundle, err := pairing.Seal(payload, code)
			must(t, err)
			home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
			env := setupTestEnv(t, home, userHome, newFakeKeychain(), now)
			env.DetectHarnesses = func(string) []string { return []string{"codex"} }
			env.AWSProfiles = func() ([]AWSProfile, error) { return []AWSProfile{{Name: "archive", Region: "us-east-1"}}, nil }
			entered := code
			env.LookupEnv = func(k string) (string, bool) { return entered, k == "AGENT_ARCHIVE_PAIRING_CODE" }
			store := storagetest.NewMemoryStore()
			env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return store, nil }
			switch failure {
			case wrongCode:
				entered = "yo-yo-abandoned-abbreviate-abdomen-abhorrence-abiding"
			case expiredBundle:
				env.Now = func() time.Time { return now.Add(time.Hour) }
			case storageFailure:
				env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return nil, errors.New("offline") }
			case commitFailure:
				fakeSched(env).beforeLoad = func(scheduler.Ref) error { return errors.New("interrupted load") }
			}
			args := []string{"setup", "--pair-file", "-", "--yes", "--codex-discovery", "on", "--codex-capture-scope", "included-projects", "--project", project}
			var out bytes.Buffer
			if exit := Run(args, strings.NewReader(bundle), &out, &out, env); exit != 1 {
				t.Fatalf("failure exit %d: %s", exit, &out)
			}
			if strings.Contains(out.String(), "Paired successfully") {
				t.Fatal("failed pairing claimed success")
			}
			if _, found, err := config.Load(home); err != nil || found {
				t.Fatalf("failed pairing committed: %t %v", found, err)
			}
			if failure == wrongCode || failure == expiredBundle {
				if !strings.Contains(out.String(), "Expiry does not revoke storage access") {
					t.Fatalf("missing recovery guidance: %s", &out)
				}
			}
			entered = code
			env.Now = func() time.Time { return now }
			env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return store, nil }
			fakeSched(env).beforeLoad = nil
			out.Reset()
			if exit := Run(args, strings.NewReader(bundle), &out, &out, env); exit != 0 {
				t.Fatalf("retry %d: %s", exit, &out)
			}
			last := -1
			for _, stage := range []string{"Pairing · Review settings", "Pairing · Check storage", "Pairing · Save configuration", "Paired successfully"} {
				at := strings.Index(out.String(), stage)
				if at <= last {
					t.Fatalf("missing or out of order %s: %s", stage, &out)
				}
				last = at
			}
		})
	}
}

func TestPairingS3CancellationPreservesLocalProfilesAndBundle(t *testing.T) {
	source, home, _ := pairingSourceFixture(t)
	source.IsTerminal = func(any) bool { return true }
	source.Interrupts = noInterrupts
	path := filepath.Join(t.TempDir(), "pairing.txt")
	var out bytes.Buffer
	if exit := Run([]string{"machines", "add", "--name", "laptop", "--file", path}, strings.NewReader("\n\ncancel\n"), &out, &out, source); exit != 0 {
		t.Fatalf("cancel %d: %s", exit, &out)
	}
	if !strings.Contains(out.String(), "no AWS credential was issued") || strings.Contains(out.String(), "Shared R2 access") {
		t.Fatalf("incorrect cancellation: %s", &out)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("cancellation removed transferred bundle")
	}
	ledgers, err := readPairingLedgers(home)
	must(t, err)
	if len(ledgers) != 1 || ledgers[0].State != pairingCancelled {
		t.Fatal("cancellation not recorded")
	}
}

func TestPairingReceiverCancellationLeavesCaptureUnchanged(t *testing.T) {
	now := time.Now().UTC()
	payload := pairing.Payload{Version: 1, PairingID: strings.Repeat("1", 32), RecipientID: strings.Repeat("2", 32), IssuerID: strings.Repeat("3", 32), IssuerName: "studio", Name: "laptop", CreatedAt: now, ExpiresAt: now.Add(15 * time.Minute), Storage: pairing.Storage{Provider: "s3", Bucket: "synthetic", AWSProfile: "archive", Region: "us-east-1"}, Apps: []string{"codex"}, RetentionDays: 90, SkillEvidence: "metadata"}
	code := "aardvark-abandoned-abbreviate-abdomen-abhorrence-abiding"
	bundle, err := pairing.Seal(payload, code)
	must(t, err)
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	path := filepath.Join(t.TempDir(), "pairing.txt")
	must(t, os.WriteFile(path, []byte(bundle), 0600))
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), now)
	env.DetectHarnesses = func(string) []string { return []string{"codex"} }
	env.AWSProfiles = func() ([]AWSProfile, error) { return []AWSProfile{{Name: "archive", Region: "us-east-1"}}, nil }
	env.PairingCode = func() (string, error) { return code, nil }
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		t.Fatal("cancelled review checked storage")
		return nil, nil
	}
	var out bytes.Buffer
	args := []string{"setup", "--pair-file", path, "--codex-discovery", "on", "--codex-capture-scope", "included-projects", "--project", project}
	if exit := Run(args, strings.NewReader("cancel\n"), &out, &out, env); exit != 1 {
		t.Fatalf("cancel %d: %s", exit, &out)
	}
	if !strings.Contains(out.String(), "Issued storage access") || strings.Contains(out.String(), "Paired successfully") {
		t.Fatalf("incorrect receiver cancellation: %s", &out)
	}
	if _, found, err := config.Load(home); found || err != nil {
		t.Fatalf("cancel committed: %t %v", found, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("cancel removed bundle")
	}
}
