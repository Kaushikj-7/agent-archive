package pairing

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestForeverPayload(t *testing.T) {
	p := testPayload()
	p.RetentionDays = 0
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	p.RetentionDays = -1
	if p.Validate() == nil {
		t.Fatal("negative retention accepted")
	}
}

// This opt-in compatibility probe executes a real pre-forever receiver against
// a valid encrypted v1 payload. The isolated release-test environment supplies
// the pinned old executable; ordinary unit tests do not depend on that artifact.
func TestOldReceiverRejectsForeverBeforeSideEffects(t *testing.T) {
	binary := os.Getenv("AGENT_ARCHIVE_OLD_CLI")
	if binary == "" {
		t.Skip("set AGENT_ARCHIVE_OLD_CLI to the pinned pre-forever executable")
	}
	for _, provider := range []string{"s3", "r2"} {
		t.Run(provider, func(t *testing.T) {
			p := testPayload()
			p.CreatedAt = time.Now().UTC()
			p.ExpiresAt = p.CreatedAt.Add(time.Hour)
			p.RetentionDays = 0
			if provider == "s3" {
				p.Storage = Storage{Provider: "s3", Bucket: "synthetic-retention-compatibility", Prefix: "synthetic/", AWSProfile: "synthetic", Region: "us-east-1"}
				p.AccessKeyID, p.SecretAccessKey = "", ""
				p.Kind, p.SlotID = "", ""
			}
			code, err := NewCode()
			if err != nil {
				t.Fatal(err)
			}
			bundle, err := Seal(p, code)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Open(bundle, code, time.Now()); err != nil {
				t.Fatalf("current receiver rejected fixture: %v", err)
			}
			root := t.TempDir()
			home, data := filepath.Join(root, "home"), filepath.Join(root, "data")
			if err := os.Mkdir(home, 0700); err != nil {
				t.Fatal(err)
			}
			input := filepath.Join(root, "bundle")
			if err := os.WriteFile(input, []byte(bundle), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, "setup", "--pair-file", input, "--yes")
			cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home, "AGENT_ARCHIVE_HOME=" + data, "AGENT_ARCHIVE_PAIRING_CODE=" + code}
			output, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 || !bytes.Contains(output, []byte("pairing settings are invalid or unsupported")) {
				t.Fatalf("old receiver did not fail closed: %v %s", err, output)
			}
			if _, err := os.Stat(data); !os.IsNotExist(err) {
				t.Fatalf("old receiver created data/config state: %v", err)
			}
			entries, err := os.ReadDir(home)
			if err != nil || len(entries) != 0 {
				t.Fatalf("old receiver mutated home/hooks/backend: %v %v", entries, err)
			}
			t.Logf("%s: old receiver exit1, invalid/unsupported payload; no data, config, hooks or backend files", provider)
		})
	}
}
