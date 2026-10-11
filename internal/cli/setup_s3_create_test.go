package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/aws/smithy-go"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/setupresources"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// fakeCreator is a BucketCreator that answers from memory and records what it
// was asked, in order.
type fakeCreator struct {
	// create answers each CreateBucket in turn; the last answer repeats.
	create  []error
	block   []error
	deleted error
	privacy storage.PrivacyReport
	// reads, when set, answers each ReadBlockPublicAccess in turn (the last
	// repeats). Otherwise it answers as privacy says: all on when verified,
	// off when the report has read the settings, and refused when it has
	// not.
	reads []fakeRead
	calls []string
}

type fakeRead struct {
	allOn bool
	err   error
}

var errCreateDenied = &smithy.GenericAPIError{Code: "AccessDenied", Message: "synthetic"}

func (f *fakeCreator) next(answers *[]error) error {
	if len(*answers) == 0 {
		return nil
	}
	err := (*answers)[0]
	if len(*answers) > 1 {
		*answers = (*answers)[1:]
	}
	return err
}

func (f *fakeCreator) CreateBucket(_ context.Context, bucket, region string) error {
	f.calls = append(f.calls, "create "+bucket+" "+region)
	return f.next(&f.create)
}

func (f *fakeCreator) BlockPublicAccess(_ context.Context, bucket string) error {
	f.calls = append(f.calls, "block "+bucket)
	return f.next(&f.block)
}

func (f *fakeCreator) DeleteBucket(_ context.Context, bucket string) error {
	f.calls = append(f.calls, "delete "+bucket)
	return f.deleted
}

func (f *fakeCreator) InspectPrivacy(_ context.Context, bucket string) storage.PrivacyReport {
	f.calls = append(f.calls, "inspect "+bucket)
	return f.privacy
}

func (f *fakeCreator) ReadBlockPublicAccess(_ context.Context, bucket string) (bool, error) {
	f.calls = append(f.calls, "read "+bucket)
	if len(f.reads) > 0 {
		read := f.reads[0]
		if len(f.reads) > 1 {
			f.reads = f.reads[1:]
		}
		return read.allOn, read.err
	}
	switch {
	case f.privacy.State == "verified_private":
		return true, nil
	case containsString(f.privacy.Checks, "bucket_public_access_block"):
		return false, nil
	}
	return false, errCreateDenied
}

var verifiedPrivate = storage.PrivacyReport{State: "verified_private", Reason: "all_bucket_public_access_blocks_enabled"}

// sequentialNames makes newBucketName return agent-archive-1, -2, ... for
// the rest of the test. The test must not run in parallel.
func sequentialNames(t *testing.T) {
	t.Helper()
	old, n, delay := newBucketName, 0, bucketSettleDelay
	newBucketName = func() string {
		n++
		return "agent-archive-" + string(rune('0'+n))
	}
	bucketSettleDelay = 0
	t.Cleanup(func() { newBucketName, bucketSettleDelay = old, delay })
}

// createEnv is an Env whose one S3 profile, work, has credentials and the
// region profileRegion, that opens creator, and that lists no buckets unless
// finder says so. opened records each (profile, region) the creator was
// opened with.
func createEnv(t *testing.T, profileRegion string, creator BucketCreator, finder fakeBuckets, opened *[]string) Env {
	t.Helper()
	home := t.TempDir()
	return Env{
		Home:        func() (string, error) { return home, nil },
		AWSProfiles: func() ([]AWSProfile, error) { return []AWSProfile{{Name: "work", Region: profileRegion}}, nil },
		AWSBuckets:  finder.open,
		AWSBucketCreator: func(profile, region string) (BucketCreator, error) {
			if opened != nil {
				*opened = append(*opened, profile+" "+region)
			}
			return creator, nil
		},
	}
}

func runCreate(t *testing.T, env Env, cfg *credentials.Config, input string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	// The last question of a run that creates a bucket is which profile to
	// archive with; a blank answer keeps the profile that created it.
	err := promptS3Bucket(newPrompter(strings.NewReader(input+"\n"), &out), cfg, env, "", true)
	return out.String(), err
}

func TestCreateS3BucketInProfileRegionRecordsBucketAndOffersPolicy(t *testing.T) {
	sequentialNames(t)
	creator := &fakeCreator{privacy: verifiedPrivate}
	var opened []string
	var cfg credentials.Config
	out, err := runCreate(t, createEnv(t, "eu-west-2", creator, fakeBuckets{}, &opened), &cfg, "\n\nv\n")
	if err != nil {
		t.Fatalf("err=%v output:\n%s", err, out)
	}
	if cfg.Bucket != "agent-archive-1" || cfg.Region != "eu-west-2" || cfg.AWSProfile != "work" {
		t.Fatalf("cfg=%+v", cfg)
	}
	if want := []string{"work eu-west-2"}; strings.Join(opened, ",") != strings.Join(want, ",") {
		t.Fatalf("creator opened for %q, want %q", opened, want)
	}
	want := []string{"create agent-archive-1 eu-west-2", "block agent-archive-1", "read agent-archive-1"}
	if strings.Join(creator.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls %q, want %q", creator.calls, want)
	}
	for _, text := range []string{
		"Created bucket agent-archive-1 in eu-west-2.",
		"Checked: Block Public Access is on for all four settings.",
		`"Resource": "arn:aws:s3:::agent-archive-1/agent-archive/*"`,
		"separate IAM user or role",
		"This policy hasn't been tested against a real AWS bucket yet.",
	} {
		if !strings.Contains(out, text) {
			t.Errorf("output lacks %q:\n%s", text, out)
		}
	}
}

func TestCreateS3BucketInUSEast1AndOtherRegionsUseTheChosenRegion(t *testing.T) {
	for _, tc := range []struct {
		profileRegion string
		input         string
		want          string
	}{
		{"us-east-1", "\n\n", "create agent-archive-1 us-east-1"},
		{"us-east-1", "c\nregion\nap-southeast-2\n\n", "create agent-archive-1 ap-southeast-2"},
		{"", "us-west-1\n\n", "create agent-archive-1 us-west-1"},
	} {
		sequentialNames(t)
		creator := &fakeCreator{privacy: verifiedPrivate}
		var cfg credentials.Config
		out, err := runCreate(t, createEnv(t, tc.profileRegion, creator, fakeBuckets{}, nil), &cfg, "\n"+tc.input)
		if err != nil || len(creator.calls) == 0 || creator.calls[0] != tc.want {
			t.Errorf("profile region %q, input %q: calls %q err=%v, want %q\n%s", tc.profileRegion, tc.input, creator.calls, err, tc.want, out)
		}
	}
}

func TestCreateS3BucketRetriesADefaultNameOnceThenAsksAboutTheNext(t *testing.T) {
	sequentialNames(t)
	taken := errors.Join(storage.ErrBucketNameTaken, errors.New("BucketAlreadyExists"))
	creator := &fakeCreator{create: []error{taken, taken, nil}, privacy: verifiedPrivate}
	var cfg credentials.Config
	// Profile, region, default name; then the bucket names agent-archive-1
	// (taken, retried once as agent-archive-2, taken), so setup offers to
	// pick an existing bucket instead, and the answer chooses another name,
	// my-archive-store, which is created.
	out, err := runCreate(t, createEnv(t, "us-east-1", creator, fakeBuckets{}, nil), &cfg, "\n\n\nretry\nc\nname\nmy-archive-store\n\n")
	if err != nil {
		t.Fatalf("err=%v output:\n%s", err, out)
	}
	want := []string{"create agent-archive-1 us-east-1", "create agent-archive-2 us-east-1", "create my-archive-store us-east-1", "block my-archive-store", "read my-archive-store"}
	if strings.Join(creator.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls %q, want %q\n%s", creator.calls, want, out)
	}
	if cfg.Bucket != "my-archive-store" {
		t.Fatalf("bucket %q", cfg.Bucket)
	}
	if !strings.Contains(out, "The name agent-archive-1 is taken; preparing another suggested name.") {
		t.Errorf("output does not say the default was replaced:\n%s", out)
	}
}

func TestCreateS3BucketAsksAgainWhenATypedNameIsTaken(t *testing.T) {
	sequentialNames(t)
	taken := errors.Join(storage.ErrBucketNameTaken, errors.New("BucketAlreadyExists"))
	creator := &fakeCreator{create: []error{taken, nil}, privacy: verifiedPrivate}
	var cfg credentials.Config
	out, err := runCreate(t, createEnv(t, "us-east-1", creator, fakeBuckets{}, nil), &cfg, "\nc\nname\nmine-alex\n\nretry\nc\nname\nmine-alex-2\n\n")
	if err != nil || cfg.Bucket != "mine-alex-2" {
		t.Fatalf("cfg=%+v err=%v\n%s", cfg, err, out)
	}
	if got := creator.calls[:2]; got[0] != "create mine-alex us-east-1" || got[1] != "create mine-alex-2 us-east-1" {
		t.Fatalf("a typed name must not be replaced on its own: %q", creator.calls)
	}
}

func TestCreateS3BucketRefusesNamesS3WouldRefuseBeforeAsking(t *testing.T) {
	sequentialNames(t)
	creator := &fakeCreator{privacy: verifiedPrivate}
	var cfg credentials.Config
	out, err := runCreate(t, createEnv(t, "us-east-1", creator, fakeBuckets{}, nil), &cfg, "\nc\nname\nMy_Bucket\nab\nok-bucket\n\n")
	if err != nil || cfg.Bucket != "ok-bucket" {
		t.Fatalf("cfg=%+v err=%v\n%s", cfg, err, out)
	}
	if creator.calls[0] != "create ok-bucket us-east-1" {
		t.Fatalf("an invalid name reached S3: %q", creator.calls)
	}
	for _, text := range []string{`"My_Bucket" can't be a bucket name`, `"ab" can't be a bucket name: it must be 3 to 63 characters long`} {
		if !strings.Contains(out, text) {
			t.Errorf("output lacks %q:\n%s", text, out)
		}
	}
}

func TestCreateS3BucketWithoutPermissionExplainsAndPicksAnExistingBucket(t *testing.T) {
	sequentialNames(t)
	creator := &fakeCreator{create: []error{errCreateDenied}}
	finder := fakeBuckets{names: []string{"photos", "team-archive"}, regions: map[string]string{"team-archive": "us-west-2"}}
	var cfg credentials.Config
	out, err := runCreate(t, createEnv(t, "us-east-1", creator, finder, nil), &cfg, "\n\nexisting\n2\n")
	if err != nil {
		t.Fatalf("err=%v output:\n%s", err, out)
	}
	if cfg.Bucket != "team-archive" || cfg.Region != "us-west-2" {
		t.Fatalf("cfg=%+v, want the existing bucket picked after the fallback", cfg)
	}
	for _, text := range []string{"isn't allowed to create buckets", "s3:CreateBucket", "s3:PutBucketPublicAccessBlock", "service control policy", "choose an existing bucket"} {
		if !strings.Contains(out, text) {
			t.Errorf("output lacks %q:\n%s", text, out)
		}
	}
	if strings.Join(creator.calls, ",") != "create agent-archive-1 us-east-1" {
		t.Fatalf("calls %q: nothing may follow a refused CreateBucket", creator.calls)
	}
	if strings.Contains(out, "arn:aws:s3") {
		t.Error("a policy was printed for a bucket that was not created")
	}
}

func TestCreateS3BucketProfileWithoutCredentialsFallsBackWithoutCallingS3(t *testing.T) {
	sequentialNames(t)
	creator := &fakeCreator{}
	env := createEnv(t, "us-east-1", creator, fakeBuckets{}, nil)
	env.AWSProfiles = func() ([]AWSProfile, error) { return []AWSProfile{{Name: "bare", NoCredentials: true}}, nil }
	var cfg credentials.Config
	out, err := runCreate(t, env, &cfg, "1\nexisting\ntyped\nus-east-1\n")
	if err != nil || cfg.Bucket != "typed" || len(creator.calls) != 0 {
		t.Fatalf("cfg=%+v calls=%q err=%v\n%s", cfg, creator.calls, err, out)
	}
	if !strings.Contains(out, "has no credentials configured. Choose another profile") {
		t.Errorf("output does not explain:\n%s", out)
	}
}

func TestCreateS3BucketOtherFailureIsExplainedWithoutQuotingS3(t *testing.T) {
	sequentialNames(t)
	for _, tc := range []struct {
		name string
		err  error
		want string
		not  string
	}{
		{
			"unrecognized error", &smithy.GenericAPIError{Code: "SomethingNew", Message: "secret-account-detail"},
			"S3 or STS returned an error setup doesn't recognize", "Try again in a moment",
		},
		{
			"account bucket limit", errors.Join(storage.ErrTooManyBuckets, &smithy.GenericAPIError{Code: "TooManyBuckets", Message: "secret-account-detail"}),
			"This AWS account has reached its limit on buckets.", "Try again in a moment",
		},
	} {
		creator := &fakeCreator{create: []error{tc.err}}
		var cfg credentials.Config
		out, err := runCreate(t, createEnv(t, "us-east-1", creator, fakeBuckets{names: []string{"x"}, regions: map[string]string{"x": "us-east-1"}}, nil), &cfg, "\n\nexisting\n1\n")
		if err != nil || cfg.Bucket != "x" {
			t.Fatalf("%s: cfg=%+v err=%v\n%s", tc.name, cfg, err, out)
		}
		if !strings.Contains(out, tc.want) || strings.Contains(out, tc.not) || strings.Contains(out, "secret-account-detail") {
			t.Errorf("%s: output:\n%s", tc.name, out)
		}
	}
}

func TestCreateS3BucketRefusedRegionIsNotReportedAsAnotherRegionsBucket(t *testing.T) {
	sequentialNames(t)
	creator := &fakeCreator{create: []error{&smithy.GenericAPIError{Code: "IllegalLocationConstraintException", Message: "synthetic"}}}
	var cfg credentials.Config
	out, err := runCreate(t, createEnv(t, "us-east-1", creator, fakeBuckets{names: []string{"x"}, regions: map[string]string{"x": "us-east-1"}}, nil), &cfg, "\n\nexisting\n1\n")
	if err != nil || cfg.Bucket != "x" {
		t.Fatalf("cfg=%+v err=%v\n%s", cfg, err, out)
	}
	if !strings.Contains(out, "S3 didn't accept that region for a new bucket") || strings.Contains(out, "different AWS region") {
		t.Errorf("output:\n%s", out)
	}
}

func TestCreateS3BucketBlockPublicAccessFailureNeverContinues(t *testing.T) {
	for _, tc := range []struct {
		name      string
		answer    string
		wantErr   string
		wantCalls string
		wantCfg   string
	}{
		{"stop", "stop\n", "bucket agent-archive-1 was created but Block Public Access is not on", "create agent-archive-1 us-east-1,block agent-archive-1", ""},
		{"delete then an existing bucket", "delete\nagent-archive-1\n1\n", "", "create agent-archive-1 us-east-1,block agent-archive-1,delete agent-archive-1", "existing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sequentialNames(t)
			creator := &fakeCreator{block: []error{errCreateDenied}}
			finder := fakeBuckets{names: []string{"existing"}, regions: map[string]string{"existing": "us-east-1"}}
			var cfg credentials.Config
			out, err := runCreate(t, createEnv(t, "us-east-1", creator, finder, nil), &cfg, "\n\n"+tc.answer)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err=%v, want %q", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("err=%v\n%s", err, out)
			}
			if strings.Join(creator.calls, ",") != tc.wantCalls {
				t.Fatalf("calls %q, want %q", creator.calls, tc.wantCalls)
			}
			if cfg.Bucket == "agent-archive-1" || cfg.Bucket != tc.wantCfg {
				t.Fatalf("bucket %q: the half-secured bucket must not be chosen", cfg.Bucket)
			}
			for _, text := range []string{"Bucket agent-archive-1 was created, but setup couldn't turn on Block Public Access", "s3:PutBucketPublicAccessBlock", "Setup won't store sessions in it"} {
				if !strings.Contains(out, text) {
					t.Errorf("output lacks %q:\n%s", text, out)
				}
			}
			if strings.Contains(out, "arn:aws:s3") {
				t.Error("a policy was printed for a bucket that was not secured")
			}
		})
	}
}

func TestCreateS3BucketBlockPublicAccessRetrySucceeds(t *testing.T) {
	sequentialNames(t)
	creator := &fakeCreator{block: []error{errCreateDenied, nil}, privacy: verifiedPrivate}
	var cfg credentials.Config
	out, err := runCreate(t, createEnv(t, "us-east-1", creator, fakeBuckets{}, nil), &cfg, "\n\nretry\n")
	if err != nil || cfg.Bucket != "agent-archive-1" {
		t.Fatalf("cfg=%+v err=%v\n%s", cfg, err, out)
	}
	want := "create agent-archive-1 us-east-1,block agent-archive-1,block agent-archive-1,read agent-archive-1"
	if strings.Join(creator.calls, ",") != want {
		t.Fatalf("calls %q, want %q", creator.calls, want)
	}
}

func TestCreateS3BucketDeleteFailureNamesTheCommand(t *testing.T) {
	sequentialNames(t)
	creator := &fakeCreator{block: []error{errCreateDenied}, deleted: errCreateDenied}
	finder := fakeBuckets{names: []string{"existing"}, regions: map[string]string{"existing": "us-east-1"}}
	var cfg credentials.Config
	out, err := runCreate(t, createEnv(t, "us-east-1", creator, finder, nil), &cfg, "\n\ndelete\nagent-archive-1\n1\n")
	if err != nil || cfg.Bucket != "existing" {
		t.Fatalf("cfg=%+v err=%v\n%s", cfg, err, out)
	}
	if !strings.Contains(out, "aws s3api delete-bucket --bucket agent-archive-1 --profile work") {
		t.Errorf("output lacks the manual delete command:\n%s", out)
	}
}

func TestCreateS3BucketThatStillLooksPublicIsNotUsed(t *testing.T) {
	sequentialNames(t)
	creator := &fakeCreator{privacy: storage.PrivacyReport{State: "public_or_risky", Reason: "public_bucket_policy"}}
	var cfg credentials.Config
	out, err := runCreate(t, createEnv(t, "us-east-1", creator, fakeBuckets{}, nil), &cfg, "\n\nstop\n")
	if err == nil || cfg.Bucket != "" {
		t.Fatalf("cfg=%+v err=%v\n%s", cfg, err, out)
	}
	if !strings.Contains(out, "still looks public") {
		t.Errorf("output does not say so:\n%s", out)
	}
}

func TestCreateS3BucketUnreadableBlockPublicAccessWarnsAndContinues(t *testing.T) {
	sequentialNames(t)
	creator := &fakeCreator{privacy: storage.UnknownPrivacy("s3")}
	var cfg credentials.Config
	out, err := runCreate(t, createEnv(t, "us-east-1", creator, fakeBuckets{}, nil), &cfg, "\n\n")
	if err != nil || cfg.Bucket != "agent-archive-1" {
		t.Fatalf("cfg=%+v err=%v\n%s", cfg, err, out)
	}
	if !strings.Contains(out, "can't read it back to confirm") || strings.Contains(out, "Checked: Block Public Access is on") {
		t.Errorf("output:\n%s", out)
	}
}

func TestStorageMenuOffersS3CreationAndUsesTheS3Provider(t *testing.T) {
	sequentialNames(t)
	creator := &fakeCreator{privacy: verifiedPrivate}
	var out bytes.Buffer
	cfg, _, saveSecret, err := promptStorage(newPrompter(strings.NewReader("s3-new\n\n\n\n\n"), &out), credentials.Config{}, createEnv(t, "us-east-1", creator, fakeBuckets{}, nil), "")
	if err != nil || saveSecret {
		t.Fatalf("err=%v saveSecret=%v\n%s", err, saveSecret, &out)
	}
	if cfg.Provider != credentials.ProviderS3 || cfg.Bucket != "agent-archive-1" || cfg.Region != "us-east-1" || cfg.AWSProfile != "work" || cfg.Prefix != defaultPrefix {
		t.Fatalf("cfg=%+v", cfg)
	}
	if !strings.Contains(out.String(), "  2) Amazon S3") {
		t.Errorf("menu does not offer it:\n%s", &out)
	}
}

// The existing-bucket prompt has no special words: what is typed is a bucket
// name, even one as short as "new".
func TestEmptyBucketListPromptHasNoSpecialWords(t *testing.T) {
	sequentialNames(t)
	creator := &fakeCreator{}
	var out bytes.Buffer
	var cfg credentials.Config
	err := promptS3Location(newPrompter(strings.NewReader("\nnew\n"), &out), &cfg, createEnv(t, "eu-west-1", creator, fakeBuckets{regions: map[string]string{"new": "eu-west-1"}}, nil), "")
	if err != nil || cfg.Bucket != "new" || len(creator.calls) != 0 {
		t.Fatalf("cfg=%+v calls=%q err=%v\n%s", cfg, creator.calls, err, &out)
	}
	if !strings.Contains(out.String(), "choose Amazon S3 at the storage question and continue with creation") {
		t.Errorf("missing creation guidance:\n%s", &out)
	}

}

func TestBucketNameProblem(t *testing.T) {
	t.Parallel()
	for name, wantProblem := range map[string]bool{
		"agent-archive-3f9a1c2e": false,
		"abc":                    false,
		"my.bucket-1":            false,
		strings.Repeat("a", 63):  false,
		"ab":                     true,
		strings.Repeat("a", 64):  true,
		"Upper":                  true,
		"under_score":            true,
		"-leading":               true,
		"trailing-":              true,
		"has space":              true,
		"a..b":                   true,
		".dot":                   true,
		"192.168.5.4":            true,
		"xn--bucket":             true,
		"sthree-bucket":          true,
		"amzn-s3-demo-bucket":    true,
		"bucket-s3alias":         true,
		"bucket--ol-s3":          true,
		"bucket.mrap":            true,
		"bucket--x-s3":           true,
		"bucket--table-s3":       true,
		"my-bucket-an":           true,
		"an":                     true,
		"my-bucket-and":          false,
	} {
		if got := bucketNameProblem(name) != ""; got != wantProblem {
			t.Errorf("bucketNameProblem(%q) = %q, want a problem: %v", name, bucketNameProblem(name), wantProblem)
		}
	}
}

func TestDefaultBucketNameIsValidAndDiffersEachTime(t *testing.T) {
	t.Parallel()
	a, b := newBucketName(), newBucketName()
	if a == b || !strings.HasPrefix(a, "agent-archive-") || bucketNameProblem(a) != "" || len(a) != len("agent-archive-")+8 {
		t.Fatalf("names %q and %q", a, b)
	}
}

// documentedRuntimePolicy is the first JSON block under "Amazon S3" in
// docs/security/bucket-permissions.md.
func documentedRuntimePolicy(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("../../docs/security/bucket-permissions.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(data)
	_, afterHeading, found := strings.Cut(doc, "\n## Amazon S3\n")
	if !found {
		t.Fatal("bucket-permissions.md has no Amazon S3 section")
	}
	_, afterFence, found := strings.Cut(afterHeading, "```json\n")
	if !found {
		t.Fatal("the Amazon S3 section has no json block")
	}
	block, _, found := strings.Cut(afterFence, "\n```")
	if !found {
		t.Fatal("the json block is not closed")
	}
	return block
}

func TestRuntimePolicyMatchesDocument(t *testing.T) {
	t.Parallel()
	if got := documentedRuntimePolicy(t); got != runtimePolicyTemplate {
		t.Fatalf("the policy setup prints has drifted from docs/security/bucket-permissions.md.\ndocument:\n%s\n\nsetup:\n%s", got, runtimePolicyTemplate)
	}
}

func TestRuntimePolicyNamesTheBucketAndPrefix(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		bucket      string
		prefix      string
		wantObjects string
		wantList    string
	}{
		{"agent-archive-3f9a1c2e", "agent-archive/", "arn:aws:s3:::agent-archive-3f9a1c2e/agent-archive/*", `["agent-archive/*"]`},
		{"work-logs", "team/notes", "arn:aws:s3:::work-logs/team/notes/*", `["team/notes/*"]`},
	} {
		policy := runtimePolicy(tc.bucket, tc.prefix)
		var decoded map[string]any
		if err := json.Unmarshal([]byte(policy), &decoded); err != nil {
			t.Fatalf("policy is not JSON: %v\n%s", err, policy)
		}
		if strings.Contains(policy, "my-archive-bucket") {
			t.Errorf("the placeholder bucket is left in:\n%s", policy)
		}
		for _, want := range []string{tc.wantObjects, tc.wantList, `"Resource": "arn:aws:s3:::` + tc.bucket + `"`} {
			if !strings.Contains(policy, want) {
				t.Errorf("policy for %s/%s lacks %q:\n%s", tc.bucket, tc.prefix, want, policy)
			}
		}
		// Creating a bucket is exactly what the runtime policy must not allow.
		for _, forbidden := range []string{"s3:CreateBucket", "s3:PutBucketPublicAccessBlock", "s3:DeleteBucket"} {
			if strings.Contains(policy, forbidden) {
				t.Errorf("the runtime policy grants %s", forbidden)
			}
		}
	}
}

// Through the whole of setup: a bucket created for the person passes the
// same storage check as any other, and the review shows it as the storage.
func TestSetupThroughStorageCheckWithACreatedS3Bucket(t *testing.T) {
	sequentialNames(t)
	f := newScreenFixture(t)
	f.withApps(t, "claude")
	f.inWebApp(t)
	creator := &fakeCreator{privacy: verifiedPrivate}
	f.env.AWSProfiles = func() ([]AWSProfile, error) { return []AWSProfile{{Name: "default", Region: "us-west-2"}}, nil }
	f.env.AWSBucketCreator = func(profile, region string) (BucketCreator, error) {
		if profile != "default" || region != "us-west-2" {
			t.Errorf("creator opened for %q in %q", profile, region)
		}
		return creator, nil
	}
	// The storage question's third choice creates the bucket; the profile,
	// region and name questions take their defaults; then the review is
	// cancelled.
	out := f.runSetup(t, strings.Join([]string{"", "", storageMenuNumber(t, storageChoiceS3New), "", "", "v", "", "q"}, "\n")+"\n")
	for _, text := range []string{
		"Created bucket agent-archive-1 in us-west-2.",
		"Checked: Block Public Access is on for all four settings.",
		"✓ Connected to your storage.",
		"s3://agent-archive-1/agent-archive/",
		`"Resource": "arn:aws:s3:::agent-archive-1/agent-archive/*"`,
		// The review is cancelled, so the bucket is not in use, but the saved
		// draft names it: setup says that.
		"Setup created bucket agent-archive-1 in us-west-2; it is empty. Your saved setup draft uses it, so running setup again will resume with it.",
	} {
		if !strings.Contains(out, text) {
			t.Errorf("output lacks %q:\n%s", text, out)
		}
	}
}

func TestCreateS3BucketOffersAnExitWhenEveryNameReadsAsTaken(t *testing.T) {
	sequentialNames(t)
	taken := errors.Join(storage.ErrBucketNameTaken, errors.New("HEAD 403"))
	creator := &fakeCreator{create: []error{taken}}
	finder := fakeBuckets{names: []string{"existing"}, regions: map[string]string{"existing": "us-east-1"}}
	var cfg credentials.Config
	// Default taken, retried once and taken, then two typed names taken,
	// each time offered the exit; the last answer takes it.
	out, err := runCreate(t, createEnv(t, "us-east-1", creator, finder, nil), &cfg, "\n\n\nretry\nc\nname\nfirst\n\nretry\nc\nname\nsecond\n\nexisting\n1\n")
	if err != nil || cfg.Bucket != "existing" {
		t.Fatalf("cfg=%+v err=%v\n%s", cfg, err, out)
	}
	if n := strings.Count(out, "Use an existing bucket"); n < 3 {
		t.Errorf("the exit was offered %d times, want with every answer from the second on:\n%s", n, out)
	}
	for _, call := range creator.calls {
		if strings.HasPrefix(call, "block") || strings.HasPrefix(call, "delete") {
			t.Errorf("nothing may follow a name that was never created: %q", creator.calls)
		}
	}
}

func TestCreateS3BucketWithAnUnclearAnswerSaysToLookAndNeverOffersDelete(t *testing.T) {
	sequentialNames(t)
	creator := &fakeCreator{create: []error{errors.Join(storage.ErrBucketMayExist, errors.New("timeout"))}}
	finder := fakeBuckets{names: []string{"x"}, regions: map[string]string{"x": "us-east-1"}}
	var cfg credentials.Config
	out, err := runCreate(t, createEnv(t, "us-east-1", creator, finder, nil), &cfg, "\n\nexisting\n1\n")
	if err != nil || cfg.Bucket != "x" {
		t.Fatalf("cfg=%+v err=%v\n%s", cfg, err, out)
	}
	if !strings.Contains(out, "the bucket agent-archive-1 may have been created. Check the S3 console for it") {
		t.Errorf("output does not say to look:\n%s", out)
	}
	if strings.Contains(out, "Delete the empty bucket") || strings.Join(creator.calls, ",") != "create agent-archive-1 us-east-1" {
		t.Errorf("calls %q; output:\n%s", creator.calls, out)
	}
}

func TestCreateS3BucketAsksAgainWhenS3RefusesTheName(t *testing.T) {
	sequentialNames(t)
	creator := &fakeCreator{create: []error{errors.Join(storage.ErrInvalidBucketName, errors.New("InvalidBucketName")), nil}, privacy: verifiedPrivate}
	var cfg credentials.Config
	out, err := runCreate(t, createEnv(t, "us-east-1", creator, fakeBuckets{}, nil), &cfg, "\nc\nname\nweird-name\n\nretry\nc\nname\nother-name\n\n")
	if err != nil || cfg.Bucket != "other-name" {
		t.Fatalf("cfg=%+v err=%v\n%s", cfg, err, out)
	}
	if !strings.Contains(out, "S3 could not use the name weird-name.") {
		t.Errorf("output:\n%s", out)
	}
}

func TestCreateS3BucketOpensTheCreatorInTheChosenRegionNotTheProfilesOwn(t *testing.T) {
	sequentialNames(t)
	creator := &fakeCreator{privacy: verifiedPrivate}
	var opened []string
	var cfg credentials.Config
	out, err := runCreate(t, createEnv(t, "us-east-1", creator, fakeBuckets{}, &opened), &cfg, "\nc\nregion\neu-west-3\n\n")
	if err != nil || cfg.Region != "eu-west-3" {
		t.Fatalf("cfg=%+v err=%v\n%s", cfg, err, out)
	}
	if strings.Join(opened, ",") != "work eu-west-3" || creator.calls[0] != "create agent-archive-1 eu-west-3" {
		t.Fatalf("opened %q, calls %q: the profile's region was used for a bucket created elsewhere", opened, creator.calls)
	}
}

func TestCreateS3BucketDeclinesRegionsOutsideTheStandardOnes(t *testing.T) {
	sequentialNames(t)
	for _, region := range []string{"cn-north-1", "cn-northwest-1", "us-gov-west-1", "us-iso-east-1", "us-isob-east-1", "eusc-de-east-1"} {
		creator := &fakeCreator{}
		var opened []string
		var cfg credentials.Config
		out, err := runCreate(t, createEnv(t, "us-east-1", creator, fakeBuckets{}, &opened), &cfg, "\nc\nregion\n"+region+"\nexisting\ntyped\nus-east-1\n")
		if err != nil || cfg.Bucket != "typed" || len(opened) != 0 || len(creator.calls) != 0 {
			t.Fatalf("%s: cfg=%+v opened=%q calls=%q err=%v\n%s", region, cfg, opened, creator.calls, err, out)
		}
		if !strings.Contains(out, "can only create buckets in the standard AWS regions") {
			t.Errorf("%s: output:\n%s", region, out)
		}
	}
}

func TestStandardAWSRegion(t *testing.T) {
	t.Parallel()
	for region, want := range map[string]bool{
		"us-east-1": true, "us-west-2": true, "eu-west-2": true, "eu-central-1": true, "ap-southeast-4": true,
		"sa-east-1": true, "ca-central-1": true, "me-south-1": true, "af-south-1": true, "il-central-1": true, "mx-central-1": true,
		"cn-north-1": false, "cn-northwest-1": false, "us-gov-west-1": false, "us-gov-east-1": false,
		"us-iso-east-1": false, "us-isob-east-1": false, "eu-isoe-west-1": false, "eusc-de-east-1": false,
		"": false, "us-east": false, "mars-east-1": false,
	} {
		if got := standardAWSRegion(region); got != want {
			t.Errorf("standardAWSRegion(%q) = %v, want %v", region, got, want)
		}
	}
}

func TestCreateS3BucketDeletesOnlyTheNameItCreatedAndOnlyWhenTyped(t *testing.T) {
	sequentialNames(t)
	taken := errors.Join(storage.ErrBucketNameTaken, errors.New("BucketAlreadyExists"))
	creator := &fakeCreator{create: []error{taken, nil}, block: []error{errCreateDenied}}
	finder := fakeBuckets{names: []string{"existing"}, regions: map[string]string{"existing": "us-east-1"}}
	var cfg credentials.Config
	// The default agent-archive-1 is taken, agent-archive-2 is created; Block
	// Public Access is refused. Deleting first names the wrong bucket, so
	// nothing is deleted and the menu returns; then the right one is typed.
	out, err := runCreate(t, createEnv(t, "us-east-1", creator, finder, nil), &cfg, "\n\n\ndelete\nagent-archive-1\ndelete\nagent-archive-2\n1\n")
	if err != nil || cfg.Bucket != "existing" {
		t.Fatalf("cfg=%+v err=%v\n%s", cfg, err, out)
	}
	want := "create agent-archive-1 us-east-1,create agent-archive-2 us-east-1,block agent-archive-2,delete agent-archive-2"
	if strings.Join(creator.calls, ",") != want {
		t.Fatalf("calls %q, want %q", creator.calls, want)
	}
	if !strings.Contains(out, `Not deleted: "agent-archive-1" isn't agent-archive-2.`) {
		t.Errorf("output does not refuse the wrong name:\n%s", out)
	}
}

func TestCreateS3BucketBlankConfirmationKeepsTheBucket(t *testing.T) {
	sequentialNames(t)
	creator := &fakeCreator{block: []error{errCreateDenied}}
	var cfg credentials.Config
	out, err := runCreate(t, createEnv(t, "us-east-1", creator, fakeBuckets{}, nil), &cfg, "\n\ndelete\n\nstop\n")
	if err == nil || strings.Contains(strings.Join(creator.calls, ","), "delete") {
		t.Fatalf("err=%v calls=%q\n%s", err, creator.calls, out)
	}
	if !strings.Contains(out, "Not deleted.\n") || strings.Contains(out, `Not deleted: ""`) {
		t.Errorf("a blank answer should read plainly:\n%s", out)
	}
	if !strings.Contains(out, "Only delete it if setup just created it: a bucket you already owned under this name isn't yours to delete here.") {
		t.Errorf("the delete prompt lacks its warning:\n%s", out)
	}
}

func TestCreateS3BucketRetriesABucketThatIsNotThereYet(t *testing.T) {
	sequentialNames(t)
	noSuchBucket := &smithy.GenericAPIError{Code: "NoSuchBucket", Message: "synthetic"}
	creator := &fakeCreator{
		block:   []error{noSuchBucket, noSuchBucket, nil},
		reads:   []fakeRead{{err: noSuchBucket}, {allOn: true}},
		privacy: verifiedPrivate,
	}
	var cfg credentials.Config
	out, err := runCreate(t, createEnv(t, "us-east-1", creator, fakeBuckets{}, nil), &cfg, "\n\n")
	if err != nil || cfg.Bucket != "agent-archive-1" {
		t.Fatalf("cfg=%+v err=%v\n%s", cfg, err, out)
	}
	want := "create agent-archive-1 us-east-1,block agent-archive-1,block agent-archive-1,block agent-archive-1,read agent-archive-1,read agent-archive-1"
	if strings.Join(creator.calls, ",") != want {
		t.Fatalf("calls %q, want %q", creator.calls, want)
	}
	if strings.Contains(out, "What now?") || !strings.Contains(out, "Checked: Block Public Access is on") {
		t.Errorf("a brief delay must not reach the person:\n%s", out)
	}
}

func TestCreateS3BucketGivesUpAfterAFewAttempts(t *testing.T) {
	sequentialNames(t)
	noSuchBucket := &smithy.GenericAPIError{Code: "NoSuchBucket", Message: "synthetic"}
	creator := &fakeCreator{block: []error{noSuchBucket}}
	var cfg credentials.Config
	out, err := runCreate(t, createEnv(t, "us-east-1", creator, fakeBuckets{}, nil), &cfg, "\n\nstop\n")
	if err == nil {
		t.Fatalf("err=nil\n%s", out)
	}
	blocks := 0
	for _, call := range creator.calls {
		if strings.HasPrefix(call, "block") {
			blocks++
		}
	}
	if blocks != bucketSettleAttempts {
		t.Fatalf("%d attempts, want %d: %q", blocks, bucketSettleAttempts, creator.calls)
	}
}

func TestCreateS3BucketWithASettingStillOffIsAFailureNotAWarning(t *testing.T) {
	sequentialNames(t)
	partial := storage.PrivacyReport{State: "not_verified", Reason: "public_access_controls_not_fully_verified", Checks: []string{"bucket_public_access_block"}}
	creator := &fakeCreator{privacy: partial}
	var cfg credentials.Config
	out, err := runCreate(t, createEnv(t, "us-east-1", creator, fakeBuckets{}, nil), &cfg, "\n\nstop\n")
	if err == nil || cfg.Bucket != "" {
		t.Fatalf("cfg=%+v err=%v\n%s", cfg, err, out)
	}
	if !strings.Contains(out, "Block Public Access reads back with a setting still off.") || strings.Contains(out, "can't read it back") {
		t.Errorf("output:\n%s", out)
	}
	if n := strings.Count(strings.Join(creator.calls, ","), "read "); n != 1 {
		t.Errorf("a read-back that shows the settings is not retried; read %d times", n)
	}
}

func TestCreateS3BucketDoesNotSpendAllAttemptsWhenTheReadBackIsRefused(t *testing.T) {
	sequentialNames(t)
	creator := &fakeCreator{reads: []fakeRead{{err: errCreateDenied}}}
	var cfg credentials.Config
	out, err := runCreate(t, createEnv(t, "us-east-1", creator, fakeBuckets{}, nil), &cfg, "\n\n")
	if err != nil || cfg.Bucket != "agent-archive-1" {
		t.Fatalf("cfg=%+v err=%v\n%s", cfg, err, out)
	}
	if n := strings.Count(strings.Join(creator.calls, ","), "read "); n != 1 {
		t.Errorf("a refused read-back was tried %d times, want once: %q", n, creator.calls)
	}
	if !strings.Contains(out, "this profile can't read it back to confirm (that needs s3:GetBucketPublicAccessBlock)") {
		t.Errorf("output:\n%s", out)
	}
}

func TestCreateS3BucketRetriesAnEmptyReadBackAndThenWarnsWithoutBlamingPermissions(t *testing.T) {
	sequentialNames(t)
	creator := &fakeCreator{reads: []fakeRead{{err: errors.New("no answer")}}}
	var cfg credentials.Config
	out, err := runCreate(t, createEnv(t, "us-east-1", creator, fakeBuckets{}, nil), &cfg, "\n\n")
	if err != nil || cfg.Bucket != "agent-archive-1" {
		t.Fatalf("cfg=%+v err=%v\n%s", cfg, err, out)
	}
	if n := strings.Count(strings.Join(creator.calls, ","), "read "); n != bucketSettleAttempts {
		t.Errorf("read %d times, want %d: %q", n, bucketSettleAttempts, creator.calls)
	}
	if !strings.Contains(out, "setup couldn't read it back to confirm (the lookup failed)") || strings.Contains(out, "that needs s3:GetBucketPublicAccessBlock") {
		t.Errorf("output:\n%s", out)
	}
}

func TestCreateS3BucketAsksWhichProfileArchivingShouldUse(t *testing.T) {
	sequentialNames(t)
	creator := &fakeCreator{privacy: verifiedPrivate}
	env := createEnv(t, "us-east-1", creator, fakeBuckets{}, nil)
	env.AWSProfiles = func() ([]AWSProfile, error) {
		return []AWSProfile{{Name: "admin", Region: "us-east-1"}, {Name: "archive", Region: "eu-west-1"}}, nil
	}
	var cfg credentials.Config
	// Create with admin; then archive with archive, whose own region is not
	// the bucket's.
	out, err := runCreate(t, env, &cfg, "1\n\nv\nprofile\n2\n")
	if err != nil {
		t.Fatalf("err=%v\n%s", err, out)
	}
	if cfg.AWSProfile != "archive" || cfg.Bucket != "agent-archive-1" || cfg.Region != "us-east-1" {
		t.Fatalf("cfg=%+v, want the narrower profile with the created bucket and its region", cfg)
	}
	policyAt := strings.Index(out, `"Sid": "ArchiveObjects"`)
	reminderAt := strings.Index(out, "Setup created bucket agent-archive-1 in us-east-1; it is empty and will stay in your account if you stop now.")
	askAt := strings.Index(out, "Setup saves the creation profile unless you choose another now.")
	if reminderAt < 0 || askAt < reminderAt || policyAt < askAt {
		t.Errorf("the bucket and default profile are explained before optional permissions:\n%s", out)
	}
}

func TestCreateS3BucketKeepsTheCreationProfileOnABlankAnswer(t *testing.T) {
	sequentialNames(t)
	creator := &fakeCreator{privacy: verifiedPrivate}
	var cfg credentials.Config
	if out, err := runCreate(t, createEnv(t, "us-east-1", creator, fakeBuckets{}, nil), &cfg, "\n\n"); err != nil || cfg.AWSProfile != "work" {
		t.Fatalf("cfg=%+v err=%v\n%s", cfg, err, out)
	}
}

func TestCreatedS3BucketShowsPolicyOnlyWhenRequested(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  int
	}{
		{"default", "\n\n", 0},
		{"view permissions", "\n\nv\n", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sequentialNames(t)
			creator := &fakeCreator{privacy: verifiedPrivate}
			var cfg credentials.Config
			out, err := runCreate(t, createEnv(t, "us-east-1", creator, fakeBuckets{}, nil), &cfg, tc.input)
			if err != nil || strings.Count(out, `"Sid": "ArchiveObjects"`) != tc.want {
				t.Fatalf("policy count want %d, err=%v\n%s", tc.want, err, out)
			}
		})
	}
}

// A bucket created earlier in the same run is offered, not duplicated.
func TestCreateS3BucketOffersTheBucketThisRunAlreadyCreated(t *testing.T) {
	sequentialNames(t)
	creator := &fakeCreator{privacy: verifiedPrivate}
	env := createEnv(t, "us-east-1", creator, fakeBuckets{}, nil)
	var out bytes.Buffer
	// First run: profile, region, name, archive profile. Second: profile,
	// then the offer, taken with its default.
	p := newPrompter(strings.NewReader("\n\n\n\n\n"), &out)
	var first credentials.Config
	if err := promptS3Bucket(p, &first, env, "", true); err != nil {
		t.Fatalf("first: %v\n%s", err, &out)
	}
	var second credentials.Config
	if err := promptS3Bucket(p, &second, env, "", true); err != nil {
		t.Fatalf("second: %v\n%s", err, &out)
	}
	if second.Bucket != "agent-archive-1" || second.Region != "us-east-1" {
		t.Fatalf("second=%+v, want the bucket created the first time", second)
	}
	creates := 0
	for _, call := range creator.calls {
		if strings.HasPrefix(call, "create ") {
			creates++
		}
	}
	if creates != 1 || !strings.Contains(out.String(), "Setup already created bucket agent-archive-1 in us-east-1 in this run; it is empty.") {
		t.Fatalf("creates=%d\n%s", creates, &out)
	}
}

func TestCreateS3BucketDeclinedOfferCreatesAnother(t *testing.T) {
	sequentialNames(t)
	creator := &fakeCreator{privacy: verifiedPrivate}
	env := createEnv(t, "us-east-1", creator, fakeBuckets{}, nil)
	var out bytes.Buffer
	p := newPrompter(strings.NewReader("\n\n\n"+"\nn\n\n\n"), &out)
	var cfg credentials.Config
	if err := promptS3Bucket(p, &cfg, env, "", true); err != nil {
		t.Fatal(err)
	}
	if err := promptS3Bucket(p, &cfg, env, "", true); err != nil || cfg.Bucket != "agent-archive-2" {
		t.Fatalf("cfg=%+v err=%v\n%s", cfg, err, &out)
	}
}

func TestDeletedBucketIsNoLongerRemembered(t *testing.T) {
	sequentialNames(t)
	creator := &fakeCreator{block: []error{errCreateDenied}}
	finder := fakeBuckets{names: []string{"existing"}, regions: map[string]string{"existing": "us-east-1"}}
	var out bytes.Buffer
	p := newPrompter(strings.NewReader("\n\ndelete\nagent-archive-1\n1\n"), &out)
	var cfg credentials.Config
	if err := promptS3Bucket(p, &cfg, createEnv(t, "us-east-1", creator, finder, nil), "", true); err != nil {
		t.Fatalf("err=%v\n%s", err, &out)
	}
	if len(p.createdBuckets) != 0 {
		t.Fatalf("a deleted bucket is still remembered: %+v", p.createdBuckets)
	}
}

func TestNoteUnusedCreatedBuckets(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	cfg := config.Config{Storage: credentials.Config{Provider: credentials.ProviderS3, Bucket: "in-use"}}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	p := newPrompter(strings.NewReader(""), &out)
	p.createdBuckets = []createdS3Bucket{
		{name: "in-use", region: "us-east-1", secured: true},
		{name: "left-over", region: "eu-west-1", secured: true},
		{name: "half-made", region: "us-west-2"},
	}
	noteUnusedCreatedBuckets(p, home)
	got := out.String()
	if strings.Contains(got, "in-use") {
		t.Errorf("a bucket in use was mentioned:\n%s", got)
	}
	for _, want := range []string{
		"Setup created bucket left-over in eu-west-1; it is empty. Delete it in the S3 console if you don't want it.",
		"Setup created bucket half-made in us-west-2; it is empty. Delete it in the S3 console if you don't want it. Block Public Access is not on for it.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
}

func TestRuntimePolicyIsNotPrintedForAPrefixWithSpecialCharacters(t *testing.T) {
	t.Parallel()
	for _, prefix := range []string{`a"b/`, "a\nb/", "team/*/", "team/?x", "a b/", "$x/", "a\\b/", "/", "", "é/"} {
		var out bytes.Buffer
		printRuntimePolicyAdvice(newPrompter(strings.NewReader(""), &out), "agent-archive-1", prefix)
		got := out.String()
		if strings.Contains(got, "ArchiveObjects") || strings.Contains(got, "arn:aws:s3:::") {
			t.Errorf("prefix %q: a policy was printed:\n%s", prefix, got)
		}
		if !strings.Contains(got, "has characters setup won't put into a policy") || !strings.Contains(got, "bucket-permissions.md") {
			t.Errorf("prefix %q: no note:\n%s", prefix, got)
		}
	}
	for _, prefix := range []string{"agent-archive/", "team/notes", "a.b_c-d/e"} {
		var out bytes.Buffer
		printRuntimePolicyAdvice(newPrompter(strings.NewReader(""), &out), "agent-archive-1", prefix)
		if !strings.Contains(out.String(), "ArchiveObjects") {
			t.Errorf("prefix %q: no policy:\n%s", prefix, &out)
		}
	}
}

func TestCreateS3BucketCredentialsCheckFailureIsNotABucketPermissionProblem(t *testing.T) {
	sequentialNames(t)
	check := storage.NewCredentialsCheckError(&smithy.GenericAPIError{Code: "AccessDenied", Message: "synthetic"})
	creator := &fakeCreator{create: []error{check}}
	finder := fakeBuckets{names: []string{"x"}, regions: map[string]string{"x": "us-east-1"}}
	var cfg credentials.Config
	out, err := runCreate(t, createEnv(t, "us-east-1", creator, finder, nil), &cfg, "\n\nexisting\n1\n")
	if err != nil || cfg.Bucket != "x" {
		t.Fatalf("cfg=%+v err=%v\n%s", cfg, err, out)
	}
	if !strings.Contains(out, "Couldn't check the credentials of profile work with AWS (access denied).") || strings.Contains(out, "isn't allowed to create buckets") {
		t.Errorf("output:\n%s", out)
	}
}

func TestNoteUnusedCreatedBucketsSaysWhenTheSavedDraftUsesIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		draft bool
		want  string
		not   string
	}{
		{"draft names the bucket", true, "Your saved setup draft uses it, so running setup again will resume with it. To not use it, choose a different bucket there and delete this one in the S3 console.", "Delete it in the S3 console if you don't want it."},
		{"no draft", false, "Setup created bucket left-over in eu-west-1; it is empty. Delete it in the S3 console if you don't want it.", "saved setup draft"},
	} {
		home := t.TempDir()
		if tc.draft {
			draft := setupDraft{Version: draftFormat, Step: 2, Config: config.Config{Storage: credentials.Config{Provider: credentials.ProviderS3, Bucket: "left-over"}}}
			if err := local.Write(draftPath(home), draft); err != nil {
				t.Fatal(err)
			}
		}
		var out bytes.Buffer
		p := newPrompter(strings.NewReader(""), &out)
		p.createdBuckets = []createdS3Bucket{{name: "left-over", region: "eu-west-1", secured: true}}
		noteUnusedCreatedBuckets(p, home)
		if !strings.Contains(out.String(), tc.want) || strings.Contains(out.String(), tc.not) {
			t.Errorf("%s: output:\n%s", tc.name, &out)
		}
	}
}

// receiptCheckingCreator checks the crash boundary from inside the provider call.
type receiptCheckingCreator struct {
	*fakeCreator
	t    *testing.T
	home string
}

func (c *receiptCheckingCreator) CreateBucket(ctx context.Context, bucket, region string) error {
	receipts, err := setupresources.List(c.home)
	if err != nil || len(receipts) != 1 || receipts[0].State != setupresources.RequestSent || receipts[0].Bucket != bucket {
		c.t.Fatalf("provider called without durable intent: %v %v", receipts, err)
	}
	return c.fakeCreator.CreateBucket(ctx, bucket, region)
}

func TestS3PersistsIntentBeforeProviderAndSecurityAfter(t *testing.T) {
	sequentialNames(t)
	home := t.TempDir()
	creator := &receiptCheckingCreator{fakeCreator: &fakeCreator{privacy: verifiedPrivate}, t: t, home: home}
	env := createEnv(t, "eu-west-2", creator, fakeBuckets{}, nil)
	env.Home = func() (string, error) { return home, nil }
	var cfg credentials.Config
	if out, err := runCreate(t, env, &cfg, "\n\n"); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	receipts, err := setupresources.List(home)
	if err != nil || len(receipts) != 1 || receipts[0].State != setupresources.Secured {
		t.Fatalf("security outcome not durable: %v %v", receipts, err)
	}
}

func (c *receiptCheckingCreator) DeleteBucket(ctx context.Context, bucket string) error {
	receipts, err := setupresources.List(c.home)
	if err != nil || len(receipts) != 1 || receipts[0].State != setupresources.CleanupPending || receipts[0].Bucket != bucket {
		c.t.Fatalf("provider delete called without durable cleanup intent: %v %v", receipts, err)
	}
	return c.fakeCreator.DeleteBucket(ctx, bucket)
}

func TestS3DeletionPersistsCleanupBeforeProvider(t *testing.T) {
	for _, deleteErr := range []error{nil, errors.New("lost delete answer")} {
		t.Run(fmt.Sprint(deleteErr), func(t *testing.T) {
			sequentialNames(t)
			home := t.TempDir()
			creator := &receiptCheckingCreator{fakeCreator: &fakeCreator{block: []error{errCreateDenied}, deleted: deleteErr}, t: t, home: home}
			env := createEnv(t, "us-east-1", creator, fakeBuckets{names: []string{"saved"}, regions: map[string]string{"saved": "us-east-1"}}, nil)
			env.Home = func() (string, error) { return home, nil }
			var cfg credentials.Config
			if out, err := runCreate(t, env, &cfg, "\n\ndelete\nagent-archive-1\n1\n"); err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			receipts, err := setupresources.List(home)
			want := setupresources.Deleted
			if deleteErr != nil {
				want = setupresources.CleanupPending
			}
			if err != nil || len(receipts) != 1 || receipts[0].State != want {
				t.Fatalf("%v %v want %s", receipts, err, want)
			}
		})
	}
}

func TestS3DeletionStopsWhenCleanupIntentCannotBeSaved(t *testing.T) {
	home := t.TempDir()
	r, err := setupresources.Begin(home, setupresources.Receipt{Provider: setupresources.S3, Profile: "work", Bucket: "archive"})
	if err != nil {
		t.Fatal(err)
	}
	if err = setupresources.Finish(home, r, setupresources.Created); err != nil {
		t.Fatal(err)
	}
	r.Bucket = "other" // Mismatched immutable binding makes the save fail.
	var out bytes.Buffer
	p := newPrompter(strings.NewReader(""), &out)
	p.createdBuckets = []createdS3Bucket{{name: "archive", profile: "work", home: home, receipt: r}}
	creator := &fakeCreator{}
	deleteNewBucket(p, creator, "archive", "work")
	if len(creator.calls) != 0 {
		t.Fatalf("provider called after failed intent: %v", creator.calls)
	}
	receipts, err := setupresources.List(home)
	if err != nil || receipts[0].State != setupresources.Created {
		t.Fatalf("%v %v", receipts, err)
	}
}

func TestS3CreationReceiptsDistinguishRefusalFromMissingAnswers(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure error
		want    setupresources.State
	}{
		{"client refusal", &smithy.GenericAPIError{Code: "AccessDenied", Fault: smithy.FaultClient}, setupresources.Refused},
		{"account limit", storage.ErrTooManyBuckets, setupresources.Refused},
		{"lost response", storage.ErrBucketMayExist, setupresources.RequestSent},
		{"server error", &smithy.GenericAPIError{Code: "InternalError", Fault: smithy.FaultServer}, setupresources.RequestSent},
		{"unknown error", errors.New("unknown response"), setupresources.RequestSent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sequentialNames(t)
			creator := &fakeCreator{create: []error{tc.failure}}
			env := createEnv(t, "us-east-1", creator, fakeBuckets{names: []string{"saved"}, regions: map[string]string{"saved": "us-east-1"}}, nil)
			var cfg credentials.Config
			if out, err := runCreate(t, env, &cfg, "\n\nexisting\n1\n"); err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			home, err := env.readHome()
			if err != nil {
				t.Fatal(err)
			}
			receipts, err := setupresources.List(home)
			if err != nil || len(receipts) != 1 || receipts[0].State != tc.want {
				t.Fatalf("%v %v want %s", receipts, err, tc.want)
			}
		})
	}
}
