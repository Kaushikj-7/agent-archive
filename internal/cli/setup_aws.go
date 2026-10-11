package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// AWSProfile contains only the information needed to offer a profile in setup.
// Discovery never retrieves credentials, runs credential_process, or logs in.
type AWSProfile struct {
	Name   string
	Region string
	// NoCredentials is true when the profile's settings name no credential
	// source (no access keys, credential_process, SSO, login session, or
	// role with a source to assume it from) or the SDK cannot load it.
	// Discovery checks only that such a setting is present.
	NoCredentials bool
}

func (e Env) awsProfiles() ([]AWSProfile, error) {
	if e.AWSProfiles != nil {
		return e.AWSProfiles()
	}
	home, err := e.userHomeDir()
	if err != nil {
		return nil, err
	}
	// The same files the collector's LaunchAgent is given (see
	// collectorEnvironment).
	return readAWSProfiles(awsFiles(home, e.lookupEnv))
}

func readAWSProfiles(configPath, credentialsPath string) ([]AWSProfile, error) {
	names := map[string]bool{}
	for i, path := range []string{configPath, credentialsPath} {
		f, err := os.Open(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("cannot read AWS profile settings")
		}
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			// This scan uses only section names. The SDK parses the values below, but
			// discovery keeps only the region and whether a credential setting is present.
			if !strings.HasPrefix(line, "[") {
				continue
			}
			end := strings.Index(line, "]")
			if end < 0 {
				continue
			}
			name := strings.TrimSpace(line[1:end])
			if i == 0 {
				if name != "default" && !strings.HasPrefix(name, "profile ") {
					continue
				}
				name = strings.TrimSpace(strings.TrimPrefix(name, "profile "))
			}
			if name != "" {
				names[name] = true
			}
		}
		scanErr := scanner.Err()
		_ = f.Close()
		if scanErr != nil {
			return nil, fmt.Errorf("cannot read AWS profile settings")
		}
	}
	profiles := make([]AWSProfile, 0, len(names))
	for name := range names {
		cfg, err := awsconfig.LoadSharedConfigProfile(context.Background(), name, func(o *awsconfig.LoadSharedConfigOptions) {
			o.ConfigFiles = []string{configPath}
			o.CredentialsFiles = []string{credentialsPath}
		})
		if err != nil {
			// The SDK cannot load this profile (a broken source_profile
			// chain, a missing sso-session section, conflicting credential
			// settings), so it cannot supply credentials either.
			profiles = append(profiles, AWSProfile{Name: name, NoCredentials: true})
			continue
		}
		profiles = append(profiles, AWSProfile{Name: name, Region: cfg.Region, NoCredentials: !hasCredentialSource(cfg)})
	}
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].Name < profiles[j].Name })
	return profiles, nil
}

// hasCredentialSource reports whether a profile names any way to get
// credentials. It looks only at which settings are present; nothing is
// retrieved or run. A role counts only with something to assume it from.
func hasCredentialSource(cfg awsconfig.SharedConfig) bool {
	role := cfg.RoleARN != "" && (cfg.SourceProfileName != "" || cfg.CredentialSource != "" || cfg.WebIdentityTokenFile != "")
	return cfg.Credentials.HasKeys() || cfg.CredentialProcess != "" || cfg.WebIdentityTokenFile != "" ||
		role || cfg.SSOSessionName != "" || cfg.SSOStartURL != "" || cfg.LoginSession != ""
}

// usableAWSProfiles names the discovered profiles that have a credential
// source.
func usableAWSProfiles(profiles []AWSProfile) []string {
	var names []string
	for _, profile := range profiles {
		if !profile.NoCredentials {
			names = append(names, profile.Name)
		}
	}
	return names
}

// defaultAWSProfile is the profile setup offers first: the saved one, then
// AWS_PROFILE, then "default" or the only profile, if it has a credential
// source.
func defaultAWSProfile(saved string, profiles []AWSProfile, env Env) string {
	if saved != "" {
		return saved
	}
	if name := lookupEnvTrimmed(env, "AWS_PROFILE"); name != "" {
		return name
	}
	usable := usableAWSProfiles(profiles)
	if containsString(usable, "default") {
		return "default"
	}
	if len(usable) == 1 {
		return usable[0]
	}
	return ""
}

// defaultStorageProvider is the provider setup offers when none is saved:
// S3 when AWS_PROFILE is set or a discovered profile has a credential
// source, otherwise R2, which needs nothing installed. AWS_PROFILE counts
// even when discovery finds no credential source for it, because the
// profile question defaults to it too (see defaultAWSProfile): its
// credentials can come from somewhere discovery does not look, and setting
// it says the person uses AWS.
func defaultStorageProvider(env Env) string {
	if lookupEnvTrimmed(env, "AWS_PROFILE") != "" {
		return credentials.ProviderS3
	}
	if profiles, err := env.awsProfiles(); err == nil && len(usableAWSProfiles(profiles)) > 0 {
		return credentials.ProviderS3
	}
	return credentials.ProviderR2
}

// BucketFinder lists buckets and reads a bucket's region for one AWS
// profile. It holds the profile's credentials only inside the SDK client and
// never returns them.
type BucketFinder interface {
	Buckets(ctx context.Context) ([]string, error)
	Region(ctx context.Context, bucket string) (string, error)
}

func (e Env) awsBuckets(profile, region string) (BucketFinder, error) {
	if e.AWSBuckets != nil {
		return e.AWSBuckets(profile, region)
	}
	return openAWSBuckets(profile, region)
}

// openAWSBuckets is Env.AWSBuckets' default: a client for profile that asks
// S3 itself. The package's tests replace it with one that fails, so a test
// that leaves Env.AWSBuckets unset never reaches AWS.
var openAWSBuckets = func(profile, region string) (BucketFinder, error) {
	ctx, cancel := context.WithTimeout(context.Background(), bucketDiscoveryTimeout)
	defer cancel()
	cfg, err := loadBucketDiscoveryConfig(ctx, profile, region)
	if err != nil {
		return nil, err
	}
	return s3BucketFinder{storage.NewClient(cfg, "", true, 1)}, nil
}

// loadBucketDiscoveryConfig loads profile the way the storage check does.
// Listing works from any region, so us-east-1 is used only when neither
// region nor the SDK's own sources (AWS_REGION, the profile) name one;
// forcing it would send GovCloud or China credentials to the wrong
// partition.
func loadBucketDiscoveryConfig(ctx context.Context, profile, region string) (aws.Config, error) {
	cfg, err := credentials.LoadAWSConfig(ctx, profile, region)
	if err != nil {
		return aws.Config{}, err
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	return cfg, nil
}

// bucketDiscoveryTimeout bounds each discovery call, so a profile whose
// credentials never arrive leaves setup asking for the name instead of
// waiting.
const bucketDiscoveryTimeout = 20 * time.Second

type s3BucketFinder struct{ client *s3.Client }

func (f s3BucketFinder) Buckets(ctx context.Context) ([]string, error) {
	return storage.ListBucketNames(ctx, f.client)
}

func (f s3BucketFinder) Region(ctx context.Context, bucket string) (string, error) {
	return storage.BucketRegion(ctx, f.client, bucket)
}

// regionPattern matches AWS region names such as us-east-1,
// us-gov-west-1 and ap-southeast-4.
var regionPattern = regexp.MustCompile(`^[a-z]{2,4}(-[a-z]+)+-[0-9]{1,2}$`)

// validRegion reports whether region is shaped like an AWS region name.
func validRegion(region string) bool {
	return regionPattern.MatchString(region)
}

// promptRegion asks for a bucket region until the answer is shaped like
// one, so a path or a typo is never saved as the region.
func promptRegion(p *prompter, label, def string) (string, error) {
	return p.guidedText(promptModel{Question: label, Label: "Region", Default: def, Receipt: label, Validate: func(region string) error {
		if !validRegion(region) {
			return fmt.Errorf("%q isn't an AWS region. Enter one like us-east-1 or eu-west-2", region)
		}
		return nil
	}})
}

// promptS3Location asks for the AWS profile, then the bucket, then settles
// the region. With the profile chosen, setup lists its buckets to pick from
// and reads the chosen bucket's own region, so a bucket outside the
// profile's region works. When S3 refuses either lookup, setup says why in
// one line and asks instead. A region the last storage check failed for
// (failedRegion) is asked for again rather than kept.
func promptS3Location(p *prompter, cfg *credentials.Config, env Env, failedRegion string) error {
	profileRegion, noCredentials, err := chooseS3Profile(p, cfg, env)
	if err != nil {
		return err
	}
	return promptS3ExistingBucket(p, cfg, env, failedRegion, profileRegion, noCredentials)
}

// chooseS3Profile asks for the AWS profile and records it in cfg. It returns
// the profile's own region, when discovery found a valid one, and whether
// the profile names no credential source.
func chooseS3Profile(p *prompter, cfg *credentials.Config, env Env) (profileRegion string, noCredentials bool, err error) {
	p.storageStage(storageAccount)
	profiles, err := env.awsProfiles()
	if err != nil {
		terminal.Println(p.out, "Could not read AWS profiles automatically. Enter an existing profile name below.")
	}
	def := defaultAWSProfile(cfg.AWSProfile, profiles, env)
	var profile string
	if len(profiles) > 0 {
		profile, err = pickAWSProfile(p, profiles, def)
	} else {
		profile, err = p.setupStorageRequired("AWS profile", def)
	}
	if err != nil {
		return "", false, err
	}
	if profile != cfg.AWSProfile {
		cfg.Region = ""
	}
	cfg.AWSProfile = profile
	// A region saved before setup checked its shape is dropped here, before
	// it can break the discovery client.
	if !validRegion(cfg.Region) {
		cfg.Region = ""
	}
	for _, candidate := range profiles {
		if candidate.Name == profile {
			profileRegion, noCredentials = candidate.Region, candidate.NoCredentials
			break
		}
	}
	if !validRegion(profileRegion) {
		profileRegion = ""
	}
	return profileRegion, noCredentials, nil
}

// promptS3ExistingBucket asks which existing bucket to use, for the profile
// chooseS3Profile recorded in cfg, then settles the region as
// promptS3Location describes.
func promptS3ExistingBucket(p *prompter, cfg *credentials.Config, env Env, failedRegion, profileRegion string, noCredentials bool) error {
	p.storageStage(storageDetails)
	profile := cfg.AWSProfile
	var err error
	var finder BucketFinder
	if noCredentials {
		terminal.Printf(p.out, "Profile %s has no credentials configured, so type the bucket name.\n", profile)
	} else if finder, err = openSetupBuckets(p, env, profile, firstNonEmpty(cfg.Region, profileRegion)); err != nil {
		finder = nil
		noteListFailure(p, profile, err)
	}
	bucket, listErr, err := promptBucket(p, finder, profile, cfg.Bucket)
	if err != nil {
		return err
	}
	cfg.Bucket = bucket
	// Without working credentials or a network, the region lookup would run
	// the credential chain again only to fail the same way.
	if cause := storage.Diagnose(listErr).Cause; listErr != nil && (cause == storage.CauseNoCredentials || cause == storage.CauseNetwork) {
		finder = nil
	}

	if finder != nil {
		release := p.suspendPrompts()
		region, err := bucketRegion(finder, cfg.Bucket)
		release()
		if err == nil && region == failedRegion {
			cfg.Region, err = askFailedRegion(p, region)
			return err
		}
		if err == nil {
			cfg.Region = region
			terminal.Printf(p.out, "Bucket %s is in %s; using that region.\n", cfg.Bucket, region)
			return nil
		}
		terminal.Printf(p.out, "Couldn't look up the region of bucket %s (%s).\n", cfg.Bucket, discoveryReason(err))
	}
	if cfg.Region == "" {
		cfg.Region = profileRegion
	}
	if cfg.Region == "" {
		cfg.Region, err = promptRegion(p, "Bucket region (for example us-east-1)", "")
		return err
	}
	if cfg.Region == failedRegion {
		cfg.Region, err = askFailedRegion(p, cfg.Region)
		return err
	}
	terminal.Printf(p.out, "Using region %s. You can change it at the final review.\n", cfg.Region)
	return nil
}

// promptBucket offers the buckets finder lists by number, or asks for the
// name when there is no finder, the listing fails, or it is empty. The
// saved bucket is the default, else the first named like agent-archive*.
// listErr is why the listing failed, if it did.
func promptBucket(p *prompter, finder BucketFinder, profile, saved string) (bucket string, listErr, err error) {
	if finder == nil {
		bucket, err = p.setupStorageRequired("Bucket name", saved)
		return bucket, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), bucketDiscoveryTimeout)
	release := p.suspendPrompts()
	names, listErr := finder.Buckets(ctx)
	release()
	cancel()
	if listErr != nil {
		noteListFailure(p, profile, listErr)
		bucket, err = p.setupStorageRequired("Bucket name", saved)
		return bucket, listErr, err
	}
	if len(names) == 0 {
		terminal.Printf(p.out, "Profile %s can't see any buckets. Type the bucket name; to create one instead, choose Amazon S3 at the storage question and continue with creation.\n", profile)
		bucket, err = p.setupStorageRequired("Bucket name", saved)
		return bucket, nil, err
	}
	def := saved
	if def == "" {
		for _, name := range names {
			if strings.HasPrefix(name, "agent-archive") {
				def = name
				break
			}
		}
	}
	bucket, err = pickByNumber(p, "Which bucket should sessions be stored in?", names, nil, def, "bucket name", maxListedBuckets)
	return bucket, nil, err
}

// noteListFailure says in one line that profile's buckets couldn't be
// listed, and why.
func noteListFailure(p *prompter, profile string, err error) {
	terminal.Printf(p.out, "Couldn't list buckets for profile %s (%s). Type the bucket name.\n", profile, discoveryReason(err))
}

// bucketRegion reads bucket's region. A region S3 names while refusing
// the lookup (a wrong_region failure) is used too.
func bucketRegion(finder BucketFinder, bucket string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), bucketDiscoveryTimeout)
	defer cancel()
	region, err := finder.Region(ctx, bucket)
	if err != nil {
		if d := storage.Diagnose(err); d.Cause == storage.CauseWrongRegion && validRegion(d.Region) {
			return d.Region, nil
		}
		return "", err
	}
	if !validRegion(region) {
		return "", errUnexpectedRegion
	}
	return region, nil
}

var errUnexpectedRegion = errors.New("the answer from S3 is not a region name")

// discoveryReason says in a few words why a bucket lookup failed. Like
// storage.Diagnose, it never quotes the error, which can hold whatever a
// credential_process printed.
func discoveryReason(err error) string {
	if errors.Is(err, errUnexpectedRegion) {
		return "S3 didn't name a region"
	}
	switch storage.Diagnose(err).Cause {
	case storage.CauseAccessDenied:
		return "access denied"
	case storage.CauseNoCredentials:
		return "no working credentials"
	case storage.CauseNoSuchBucket:
		return "no such bucket"
	case storage.CauseNetwork:
		return "S3 couldn't be reached"
	case storage.CauseWrongRegion:
		return "the bucket is in another region"
	case storage.CauseOther:
		// This includes local failures, such as an unusable profile, where
		// S3 was never asked.
		return "the lookup failed"
	default:
		return "the lookup failed"
	}
}

// pickAWSProfile lists the discovered profiles by number, marking those with
// no credential source. A profile that discovery missed can still be typed
// by name.
func pickAWSProfile(p *prompter, profiles []AWSProfile, def string) (string, error) {
	names := make([]string, len(profiles))
	notes := make([]string, len(profiles))
	for i, profile := range profiles {
		names[i] = profile.Name
		if profile.NoCredentials {
			notes[i] = p.style.dim("(no credentials configured)")
		}
	}
	return pickByNumber(p, "Which AWS profile has access to the bucket?", names, notes, def, "profile name", 0)
}

// maxListedBuckets caps how many buckets setup lists; the rest can be typed.
const maxListedBuckets = 20

// pickByNumber lists names by number under question, each followed by its
// note if any, and returns the one chosen by number or the name typed. The
// default is def's number when listed, else def itself. A positive limit
// caps how many are listed; the rest can be typed.
func pickByNumber(p *prompter, question string, names, notes []string, def, kind string, limit int) (string, error) {
	listed := names
	if limit > 0 && len(listed) > limit {
		listed = listed[:limit]
	}
	helpers := []string{}
	defaultAnswer := def
	for i, name := range listed {
		note := ""
		if i < len(notes) && notes[i] != "" {
			note = " · " + notes[i]
		}
		if name == def {
			note += " (default)"
		}
		helpers = append(helpers, fmt.Sprintf("%d) %s%s", i+1, name, note))
		if name == def {
			defaultAnswer = strconv.Itoa(i + 1)
		}
	}
	if more := len(names) - len(listed); more > 0 {
		helpers = append(helpers, fmt.Sprintf("%d more available by name", more))
	}
	helpers = append(helpers, "Enter a number or "+kind+".", "[:back] Back to storage options")
	resolve := func(answer string) string {
		if n, e := strconv.Atoi(answer); e == nil && n >= 1 && n <= len(listed) {
			return listed[n-1]
		}
		return answer
	}
	answer, err := p.guidedText(promptModel{Question: question, Helpers: helpers, Default: defaultAnswer, Label: "Choose", ResolveReceipt: resolve, Validate: func(value string) error {
		if value == "" {
			return fmt.Errorf("this value is required")
		}
		return nil
	}})
	if err != nil {
		return "", err
	}
	if answer == ":back" {
		return "", errChooseStorageAgain
	}
	return resolve(answer), nil
}

func openSetupBuckets(p *prompter, env Env, profile, region string) (BucketFinder, error) {
	release := p.suspendPrompts()
	defer release()
	return env.awsBuckets(profile, region)
}
