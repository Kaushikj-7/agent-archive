package cli

import (
	"bytes"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/cloudflare/cloudflaretest"
	"github.com/wangjohn/agent-archive/internal/credentials"
)

func TestStorageActionsAcceptOnlyUnambiguousChoices(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		input string
		want  string
	}{{"\n", "create"}, {"c\n", "customize"}, {"customize\n", "customize"}, {"e\n", "existing"}, {"b\n", "back"}, {"x\ne\n", "existing"}} {
		var out bytes.Buffer
		got, err := newPrompter(strings.NewReader(tc.input), &out).setupActions("Storage", "create", nil, []actionOption{{"create", "", "Create"}, {"customize", "c", "Customize"}, {"existing", "e", "Existing"}, {"back", "b", "Back"}})
		if err != nil || got != tc.want {
			t.Fatalf("input %q: %q %v", tc.input, got, err)
		}
	}
	var out bytes.Buffer
	_, err := newPrompter(strings.NewReader(""), &out).setupActions("Storage", "create", nil, []actionOption{{"create", "", "Create"}})
	if err == nil {
		t.Fatal("EOF must not accept the default")
	}
	p := newPrompter(strings.NewReader("e\nb\n"), &out)
	secret, err := p.secret("Secret: ")
	if err != nil || secret != "e" {
		t.Fatalf("secret intercepted: %q %v", secret, err)
	}
	name, err := p.setupStorageRequired("Name", "")
	if err != nil || name != "b" {
		t.Fatalf("name intercepted: %q %v", name, err)
	}
}

func TestStorageProviderNumbersAndModeAliases(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		input string
		want  string
	}{{"\n", "r2"}, {"2\n", "s3"}, {"r2-existing\n", "r2-existing"}, {"r2-\ns3\n", "s3"}} {
		var out bytes.Buffer
		got, err := newPrompter(strings.NewReader(tc.input), &out).guidedChoice(promptModel{Question: "Provider", Default: "r2", Primary: storageMenuOptions(), Aliases: []option{{"r2-existing", ""}, {"r2-create", ""}}})
		if err != nil || got != tc.want {
			t.Fatalf("%q: %q %v", tc.input, got, err)
		}
	}
}

func TestStorageR2CreationAvailableWithoutFlags(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	mode, err := storageIntroduction(newPrompter(strings.NewReader("\n"), &out), "r2")
	if err != nil || mode != "new" {
		t.Fatalf("%q %v", mode, err)
	}
	if !strings.Contains(out.String(), "Setup will create a private bucket") {
		t.Fatal(out.String())
	}
}

func TestGuidedR2DefaultProviderFlowSkipsCustomization(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	out := g.run(t, strings.Join([]string{"", "", "1", "", bootstrapCanary, "", ""}, "\n")+"\n", 0)
	if strings.Contains(out, "Bucket name [") || strings.Contains(out, "Customize storage location?") || strings.Count(out, "Your archive storage") != 1 {
		t.Fatalf("default path asks for settings:\n%s", out)
	}
	if !strings.Contains(out, "Archive-key permission lookup succeeded") || !strings.Contains(out, "Archive key saved") {
		t.Fatal(out)
	}
	g.savedConfig(t)
}

func TestGuidedR2SummaryCanReturnWithoutCreating(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	var out bytes.Buffer
	p := newPrompter(strings.NewReader(bootstrapCanary+"\ne\n"), &out)
	_, _, saved, err := createR2Bucket(p, g.env)
	if !errors.Is(err, errUseExistingStorage) || saved || g.cf.Calls(cloudflaretest.RouteCreateBucket) != 0 || len(g.apis) != 1 || !g.apis[0].discarded {
		t.Fatalf("fallback: %v saved=%v calls=%v\n%s", err, saved, routes(g.cf.Requests()), &out)
	}
}

func TestGuidedR2PermissionFailureOffersManualFallbackBeforeSettings(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	g.cf.Fail(cloudflaretest.RoutePermissionGroups, cloudflaretest.Failure{Status: http.StatusForbidden})
	var out bytes.Buffer
	_, _, _, err := createR2Bucket(newPrompter(strings.NewReader(bootstrapCanary+"\ne\n"), &out), g.env)
	if !errors.Is(err, errUseExistingStorage) || strings.Contains(out.String(), "Your archive storage") || g.cf.Calls(cloudflaretest.RouteCreateBucket) != 0 || !g.apis[0].discarded {
		t.Fatalf("early fallback: %v\n%s", err, &out)
	}
}

func TestExistingR2DestinationChangeDoesNotReuseStoredKey(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	cfg := credentials.Config{Provider: "r2", R2AccountID: testR2Account, Bucket: "old", R2CredentialRef: "old-ref"}
	input := testR2Account + "\nnew\nNEWKEY\nNEWSECRET\n"
	cfg, secret, save, err := promptExistingR2(newPrompter(strings.NewReader(input), &out), cfg, Env{})
	if err != nil || cfg.R2CredentialRef != "" || !save || secret.AccessKeyID != "NEWKEY" || strings.Contains(out.String(), "Keep stored R2 credentials") {
		t.Fatalf("changed destination reused credentials: %+v %v\n%s", cfg, err, &out)
	}
}

func TestCreateS3BackAndFallbackNeverCreate(t *testing.T) {
	for _, choice := range []string{"back", "existing"} {
		sequentialNames(t)
		creator := &fakeCreator{privacy: verifiedPrivate}
		var cfg credentials.Config
		var out bytes.Buffer
		_, made, err := createS3Bucket(newPrompter(strings.NewReader(choice+"\n"), &out), &cfg, createEnv(t, "us-east-1", creator, fakeBuckets{}, nil), "us-east-1", false)
		if made || len(creator.calls) != 0 || (choice == "back" && !errors.Is(err, errChooseStorageAgain)) || (choice == "existing" && err != nil) {
			t.Fatalf("%s made=%v err=%v calls=%v", choice, made, err, creator.calls)
		}
	}
}

func TestS3CustomizationTracksProfileRegionUntilExplicitlyChanged(t *testing.T) {
	t.Parallel()
	for _, explicit := range []bool{false, true} {
		cfg := credentials.Config{AWSProfile: "old"}
		settings := s3CreationSettings{name: "archive", region: "us-east-1", explicitRegion: explicit}
		env := Env{AWSProfiles: func() ([]AWSProfile, error) {
			return []AWSProfile{{Name: "new", Region: "eu-west-2"}}, nil
		}}
		var out bytes.Buffer
		if err := settings.customize(newPrompter(strings.NewReader("profile\nnew\n"), &out), &cfg, env); err != nil {
			t.Fatal(err)
		}
		want := "eu-west-2"
		if explicit {
			want = "us-east-1"
		}
		if cfg.AWSProfile != "new" || settings.region != want {
			t.Fatalf("explicit=%v: %+v %+v", explicit, cfg, settings)
		}
	}
}

func TestS3CustomizationRequiresConfirmationBeforeOpeningCreator(t *testing.T) {
	sequentialNames(t)
	creator := &fakeCreator{privacy: verifiedPrivate}
	var opened []string
	env := createEnv(t, "us-east-1", creator, fakeBuckets{}, &opened)
	cfg := credentials.Config{AWSProfile: "work"}
	var out bytes.Buffer
	_, _, err := createS3Bucket(newPrompter(strings.NewReader("c\nname\nmy-archive\n"), &out), &cfg, env, "us-east-1", false)
	if err == nil || len(opened) != 0 || len(creator.calls) != 0 || strings.Count(out.String(), "Your archive storage") != 2 {
		t.Fatalf("opened=%v calls=%v err=%v\n%s", opened, creator.calls, err, &out)
	}
}

func TestSavedStorageKeepSkipsCreationAndProviderQuestions(t *testing.T) {
	t.Parallel()
	cfg := credentials.Config{Provider: "s3", Bucket: "saved", AWSProfile: "work", Region: "us-east-1"}
	var out bytes.Buffer
	got, _, _, err := promptStorage(newPrompter(strings.NewReader("\n"), &out), cfg, Env{}, "")
	if err != nil || got != cfg || strings.Contains(out.String(), "Where should your archive live?") {
		t.Fatalf("cfg=%+v err=%v\n%s", got, err, &out)
	}
}

func TestFailedDraftDefaultsToConnectingItsSavedBucket(t *testing.T) {
	t.Parallel()
	cfg := credentials.Config{Provider: "s3", Bucket: "saved", AWSProfile: "work", Region: "us-east-1"}
	env := Env{AWSProfiles: func() ([]AWSProfile, error) { return []AWSProfile{{Name: "work", Region: "us-east-1"}}, nil }, AWSBuckets: fakeBuckets{names: []string{"saved"}, regions: map[string]string{"saved": "us-east-1"}}.open}
	var out bytes.Buffer
	got, _, _, err := promptStorage(newPrompter(strings.NewReader("\n\n\n"), &out), cfg, env, "", false)
	if err != nil || got.Bucket != "saved" || strings.Contains(out.String(), "Your archive storage") {
		t.Fatalf("cfg=%+v err=%v\n%s", got, err, &out)
	}
}

func TestS3ExistingFallbackUsesTheChangedCreationProfile(t *testing.T) {
	for _, missingCredentials := range []bool{false, true} {
		sequentialNames(t)
		creator := &fakeCreator{privacy: verifiedPrivate}
		env := createEnv(t, "us-east-1", creator, fakeBuckets{regionErr: errAccessDenied, listErr: errAccessDenied}, nil)
		env.AWSProfiles = func() ([]AWSProfile, error) {
			return []AWSProfile{{Name: "east", Region: "us-east-1", NoCredentials: missingCredentials}, {Name: "west", Region: "us-west-2"}}, nil
		}
		input := "east\nc\nprofile\nwest\ne\nsaved\n"
		if missingCredentials {
			input = "east\nprofile\nwest\ne\nsaved\n"
		}
		cfg := credentials.Config{}
		out, err := runCreate(t, env, &cfg, input)
		if err != nil || cfg.AWSProfile != "west" || cfg.Bucket != "saved" || cfg.Region != "us-west-2" || len(creator.calls) != 0 || strings.Contains(out, "Couldn't use profile west: it has no credentials") {
			t.Fatalf("missing=%v cfg=%+v err=%v\n%s", missingCredentials, cfg, err, out)
		}
	}
}

func TestS3ExistingFallbackPromptsForMalformedChangedProfileRegion(t *testing.T) {
	sequentialNames(t)
	creator := &fakeCreator{privacy: verifiedPrivate}
	env := createEnv(t, "us-east-1", creator, fakeBuckets{regionErr: errAccessDenied, listErr: errAccessDenied}, nil)
	env.AWSProfiles = func() ([]AWSProfile, error) {
		return []AWSProfile{{Name: "east", Region: "us-east-1"}, {Name: "west", Region: "malformed"}}, nil
	}
	cfg := credentials.Config{}
	out, err := runCreate(t, env, &cfg, "east\nc\nprofile\nwest\nus-west-2\ne\nsaved\nus-west-1\n")
	if err != nil || cfg.Region != "us-west-1" || !strings.Contains(out, "Bucket region") || len(creator.calls) != 0 {
		t.Fatalf("cfg=%+v err=%v\n%s", cfg, err, out)
	}
}

func TestGuidedR2RegeneratedNameRequiresAnotherConfirmation(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	g.cf.Fail(cloudflaretest.RouteCreateBucket, cloudflaretest.Failure{Status: http.StatusConflict, Code: 10073, Message: "Bucket name already exists.", Times: 1})
	var out bytes.Buffer
	_, _, _, err := createR2Bucket(newPrompter(strings.NewReader(bootstrapCanary+"\n\n"), &out), g.env)
	if err == nil || g.cf.Calls(cloudflaretest.RouteCreateBucket) != 1 || g.cf.Calls(cloudflaretest.RouteCreateToken) != 0 || strings.Count(out.String(), "Your archive storage") != 2 {
		t.Fatalf("err=%v requests=%v\n%s", err, routes(g.cf.Requests()), &out)
	}
}

func TestGuidedR2GeneratesOnlyOneReplacementName(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	g.cf.Fail(cloudflaretest.RouteCreateBucket, cloudflaretest.Failure{Status: http.StatusConflict, Code: 10073, Message: "Bucket name already exists.", Times: 2})
	var out bytes.Buffer
	_, _, _, err := createR2Bucket(newPrompter(strings.NewReader(bootstrapCanary+"\n\n\n"), &out), g.env)
	if err == nil || g.cf.Calls(cloudflaretest.RouteCreateBucket) != 2 || strings.Count(out.String(), "is taken; preparing") != 1 || !strings.Contains(out.String(), "Another bucket name") {
		t.Fatalf("err=%v requests=%v\n%s", err, routes(g.cf.Requests()), &out)
	}
}

func TestGuidedR2AutomaticPlacementClearsPreviousCustomization(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	input := guidedAnswers(append(append([]string{}, askToken...), "customize", "my-archive", "y", "eu", "weur", "customize", "my-archive", "n", "", "")...)
	out := g.run(t, input, 0)
	for _, request := range g.cf.Requests() {
		if request.Route == cloudflaretest.RouteCreateBucket && (request.Jurisdiction != "" || strings.Contains(request.Body, "locationHint")) {
			t.Fatalf("creation retained location: %+v\n%s", request, out)
		}
	}
	if cfg := g.savedConfig(t); cfg.Storage.R2AccountID != cloudflaretest.AccountID || strings.Contains(cfg.Storage.R2Endpoint, ".eu.") || !strings.Contains(out, "Location: automatic") {
		t.Fatalf("saved custom endpoint: %+v\n%s", cfg.Storage, out)
	}
}

func TestCreateS3DeletionGoesDirectlyToExistingBucketSelection(t *testing.T) {
	sequentialNames(t)
	creator := &fakeCreator{block: []error{errCreateDenied}}
	finder := fakeBuckets{names: []string{"saved"}, regions: map[string]string{"saved": "us-east-1"}}
	var cfg credentials.Config
	out, err := runCreate(t, createEnv(t, "us-east-1", creator, finder, nil), &cfg, "\n\ndelete\nagent-archive-1\n1\n")
	if err != nil || cfg.Bucket != "saved" || strings.Join(creator.calls, ",") != "create agent-archive-1 us-east-1,block agent-archive-1,delete agent-archive-1" || strings.Contains(out, "Review settings and retry") {
		t.Fatalf("cfg=%+v err=%v calls=%v\n%s", cfg, err, creator.calls, out)
	}
}

func TestCreateS3MissingCredentialsDefaultsToChoosingAProfile(t *testing.T) {
	sequentialNames(t)
	creator := &fakeCreator{}
	env := createEnv(t, "us-east-1", creator, fakeBuckets{}, nil)
	env.AWSProfiles = func() ([]AWSProfile, error) {
		return []AWSProfile{{Name: "bare", NoCredentials: true}, {Name: "ready", Region: "us-west-2"}}, nil
	}
	var cfg credentials.Config
	out, err := runCreate(t, env, &cfg, "bare\n\nready\ne\nsaved\n")
	if err != nil || cfg.AWSProfile != "ready" || cfg.Region != "us-west-2" || len(creator.calls) != 0 || strings.Contains(out, "Review settings and retry") || strings.Count(out, "Profile bare has no credentials") != 1 {
		t.Fatalf("cfg=%+v err=%v calls=%v\n%s", cfg, err, creator.calls, out)
	}
}

func TestStorageCredentialReceiptsHideValuesAndPreserveNavigation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		input string
		want  string
		back  bool
	}{
		{"\nSYNTHETIC-PRIVATE-ID\nnext\n", "SYNTHETIC-PRIVATE-ID", false},
		{"back\nnext\n", "back", false},
		{":back\nnext\n", "", true},
	} {
		var out bytes.Buffer
		p := newPrompter(strings.NewReader(tc.input), &out)
		got, err := p.setupStorageField("Access key ID (hidden)", "", true)
		if got != tc.want || errors.Is(err, errChooseStorageAgain) != tc.back || (!tc.back && err != nil) {
			t.Fatalf("credential %q: %q %v", tc.input, got, err)
		}
		next, err := p.in.ReadString('\n')
		p.close()
		if err != nil || next != "next\n" {
			t.Fatalf("buffered follow-up %q %v", next, err)
		}
		if strings.Contains(out.String(), "SYNTHETIC-PRIVATE-ID") || setupReceiptIndex(out.String(), "Access key ID (hidden) back") >= 0 {
			t.Fatalf("credential value in receipt:\n%s", &out)
		}
		if strings.HasPrefix(tc.input, "\n") && (!strings.Contains(out.String(), "Credential was not accepted. Try again.") || strings.Count(out.String(), "? Access key ID (hidden)") != 2) {
			t.Fatalf("missing required retry:\n%s", &out)
		}
	}
	var out bytes.Buffer
	p := newPrompter(strings.NewReader(""), &out)
	_, err := p.setupStorageField("Access key ID (hidden)", "", true)
	p.close()
	if err == nil {
		t.Fatal("EOF accepted a credential")
	}
}
