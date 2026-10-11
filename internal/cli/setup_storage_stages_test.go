package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/aws/smithy-go"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/storage"
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

func TestGuidedR2ConnectivityReportsTheManagementPrivacyEvidence(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	out := g.run(t, g.happy(), 0)
	connected := strings.Index(out, "Connected to your storage.")
	if connected < 0 || !strings.Contains(out[connected:], "Bucket privacy: r2.dev off and no enabled custom domains at the last check.") || strings.Contains(out[connected:], "Bucket privacy not verified") {
		t.Fatalf("connectivity discarded the management privacy result:\n%s", out)
	}
}

func TestUnreadableStoredR2BackPreservesChangedDestination(t *testing.T) {
	t.Parallel()
	initial := credentials.Config{Provider: credentials.ProviderR2, R2AccountID: strings.Repeat("a", 32), Bucket: "old-bucket", R2CredentialRef: "unreadable"}
	env := Env{Credentials: func() (credentials.CredentialStore, error) { return newFakeKeychain(), nil }}
	var out bytes.Buffer
	input := "\n" + strings.Repeat("b", 32) + "\nnew-bucket\n:back\nr2-existing\n\n\nSYNTHETIC-ID\nSYNTHETIC-SECRET\n"
	got, _, _, err := promptStorage(newPrompter(strings.NewReader(input), &out), initial, env, "")
	if err != nil || got.R2AccountID != strings.Repeat("b", 32) || got.Bucket != "new-bucket" || got.R2CredentialRef != "" {
		t.Fatalf("got=%+v err=%v output:\n%s", got, err, &out)
	}
}

func TestWrongRegionCorrectionRestoresStorageStage(t *testing.T) {
	t.Parallel()
	g := newScreenFixture(t)
	draft := setupDraft{Config: config.Config{Storage: credentials.Config{Provider: credentials.ProviderS3, Bucket: "synthetic", AWSProfile: "work", Region: "us-east-1"}}}
	var out bytes.Buffer
	p := newPrompter(strings.NewReader("fix\neu-west-1\n"), &out)
	p.storageStage(storageVerification)
	_, err := recoverSetupStorageFailure(p, &draft, func() error { return nil }, g.home+"/draft", "", g.env, nil, &smithy.GenericAPIError{Code: "PermanentRedirect"}, false)
	if err != nil {
		t.Fatal(err)
	}
	ask := strings.Index(out.String(), "? Bucket region")
	if ask < 0 || strings.LastIndex(out.String()[:ask], "Storage · Storage") < strings.LastIndex(out.String()[:ask], "Storage · Verification") {
		t.Fatalf("region edited under the wrong stage:\n%s", &out)
	}
}

func TestS3CreationProfileRetryRestoresStorageBeforeRegion(t *testing.T) {
	sequentialNames(t)
	creator := &fakeCreator{create: []error{errCreateDenied}}
	env := createEnv(t, "us-east-1", creator, fakeBuckets{}, nil)
	env.AWSProfiles = func() ([]AWSProfile, error) {
		return []AWSProfile{{Name: "work", Region: "us-east-1"}, {Name: "other"}}, nil
	}
	cfg := credentials.Config{AWSProfile: "work"}
	var out bytes.Buffer
	_, made, err := createS3Bucket(newPrompter(strings.NewReader("\nprofile\nother\nus-west-2\ne\n"), &out), &cfg, env, "us-east-1", false)
	if err != nil || made {
		t.Fatalf("made=%v err=%v output:\n%s", made, err, &out)
	}
	ask := strings.Index(out.String(), "? Region for the new bucket")
	if ask < 0 || strings.LastIndex(out.String()[:ask], "Storage · Storage") < strings.LastIndex(out.String()[:ask], "Storage · Account") {
		t.Fatalf("region edited under Account:\n%s", &out)
	}
}

func TestS3CreationPrivacyCheckUsesVerificationStage(t *testing.T) {
	sequentialNames(t)
	cfg := credentials.Config{AWSProfile: "work"}
	var out bytes.Buffer
	_, made, err := createS3Bucket(newPrompter(strings.NewReader("\n"), &out), &cfg, createEnv(t, "us-east-1", &fakeCreator{privacy: verifiedPrivate}, fakeBuckets{}, nil), "us-east-1", false)
	if err != nil || !made {
		t.Fatalf("made=%v err=%v output:\n%s", made, err, &out)
	}
	checked := strings.Index(out.String(), "Checked: Block Public Access")
	if checked < 0 || strings.LastIndex(out.String()[:checked], "Storage · Verification") < strings.LastIndex(out.String()[:checked], "Storage · Storage") {
		t.Fatalf("privacy checked under Storage:\n%s", &out)
	}
}

func TestGuidedR2KeyRetryRestoresStorageBeforeMinting(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	open := g.env.OpenStore
	calls := 0
	g.env.OpenStore = func(cfg config.Config) (storage.ObjectStore, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("synthetic storage unavailable")
		}
		return open(cfg)
	}
	out := g.run(t, guidedAnswers(bootstrapCanary, "", "retry", ""), 0)
	mint := strings.LastIndex(out, "Creating the bucket's key")
	if mint < 0 || strings.LastIndex(out[:mint], "Storage · Storage") < strings.LastIndex(out[:mint], "Storage · Verification") {
		t.Fatalf("archive key minted under Verification:\n%s", out)
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
	if !errors.Is(err, errChooseStorageAgain) || cfg.Bucket != initial.Bucket || cfg.R2AccountID != initial.R2AccountID {
		t.Fatalf("cfg=%+v err=%v output=%s", cfg, err, out.String())
	}
}

func TestExistingR2BackAfterChangingAccountDropsOldCredentialReference(t *testing.T) {
	var out bytes.Buffer
	initial := credentials.Config{Provider: credentials.ProviderR2, R2AccountID: strings.Repeat("a", 32), Bucket: "synthetic-bucket", R2CredentialRef: "old-key"}
	p := newPrompter(strings.NewReader(strings.Repeat("b", 32)+"\n:back\n"), &out)
	cfg, _, _, err := promptExistingR2(p, initial, Env{})
	if !errors.Is(err, errChooseStorageAgain) || cfg.R2CredentialRef != "" {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
}
