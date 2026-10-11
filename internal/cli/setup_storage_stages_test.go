package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/credentials"
)

func TestStorageStagesFollowOperationIncludingReturn(t *testing.T) {
	var out bytes.Buffer
	p := newPrompter(strings.NewReader(""), &out)
	for _, stage := range []storageStage{storageAccount, storageDetails, storageVerification, storageDetails} {
		p.storageStage(stage)
	}
	text := out.String()
	if strings.Count(text, "Storage · Storage") != 2 || strings.Count(text, "Account → Storage → Verification") != 4 {
		t.Fatalf("stage output: %s", text)
	}
}

func TestArchivePermissionsAreOptionalAndReturnToProfileChoice(t *testing.T) {
	for _, tc := range []struct {
		input  string
		policy bool
	}{{"\n", false}, {"v\nv\n\n", true}} {
		var out bytes.Buffer
		p := newPrompter(strings.NewReader(tc.input), &out)
		cfg := credentials.Config{AWSProfile: "creation"}
		if err := chooseArchiveProfile(p, &cfg, Env{}, newS3Bucket{name: "synthetic-bucket", region: "us-east-1"}); err != nil {
			t.Fatal(err)
		}
		if cfg.AWSProfile != "creation" || strings.Contains(out.String(), `"Version": "2012-10-17"`) != tc.policy {
			t.Fatalf("profile=%s, output=%s", cfg.AWSProfile, out.String())
		}
		if !strings.Contains(out.String(), "View archive permissions") {
			t.Fatal(out.String())
		}
	}
}

func TestExistingR2BackPreservesBucketAnswer(t *testing.T) {
	var out bytes.Buffer
	initial := credentials.Config{Provider: credentials.ProviderR2, R2AccountID: strings.Repeat("a", 32), Bucket: "synthetic-bucket"}
	p := newPrompter(strings.NewReader("\n:back\n"), &out)
	cfg, _, _, err := promptExistingR2(p, initial, Env{})
	if err != errChooseStorageAgain || cfg.Bucket != initial.Bucket || cfg.R2AccountID != initial.R2AccountID {
		t.Fatalf("cfg=%+v err=%v output=%s", cfg, err, out.String())
	}
}

func TestExistingR2BackAfterChangingAccountDropsOldCredentialReference(t *testing.T) {
	var out bytes.Buffer
	initial := credentials.Config{Provider: credentials.ProviderR2, R2AccountID: strings.Repeat("a", 32), Bucket: "synthetic-bucket", R2CredentialRef: "old-key"}
	p := newPrompter(strings.NewReader(strings.Repeat("b", 32)+"\n:back\n"), &out)
	cfg, _, _, err := promptExistingR2(p, initial, Env{})
	if err != errChooseStorageAgain || cfg.R2CredentialRef != "" {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
}
