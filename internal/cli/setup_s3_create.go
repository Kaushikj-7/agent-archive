package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/aws/smithy-go"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/setupresources"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// Guided creation of an Amazon S3 bucket in the person's own AWS account
// (dev/proposals/implemented/portable-handoff-and-onboarding.md, Part 2b).
//
// It uses the AWS profile chosen for storage, at setup time. That profile
// needs s3:CreateBucket and s3:PutBucketPublicAccessBlock, which the runtime
// policy in docs/security/bucket-permissions.md deliberately lacks, so a
// profile that cannot create buckets falls back to picking an existing one.
// Setup never creates IAM users or keys, and does not set a lifecycle rule
// (see the note on bucket-permissions.md).

// storageChoiceS3New is the explicit interactive shortcut for S3 creation.
const storageChoiceS3New = "s3-new"

// createdS3Bucket is a bucket this setup run created, kept in memory for the
// run so that setup can offer it again instead of creating a second one,
// and say at the end that it is there.
type createdS3Bucket struct {
	receipt setupresources.Receipt
	home    string
	name    string
	region  string
	// profile created it; archiveProfile is the one chosen to archive with.
	profile        string
	archiveProfile string
	// secured is set once Block Public Access is on and read back.
	secured bool
}

// BucketCreator creates and secures a bucket in one AWS profile's account.
// It holds the profile's credentials only inside the SDK client.
type BucketCreator interface {
	// CreateBucket creates bucket in region; a name already in use is
	// storage.ErrBucketNameTaken and changes nothing.
	CreateBucket(ctx context.Context, bucket, region string) error
	// BlockPublicAccess turns on all four Block Public Access settings.
	BlockPublicAccess(ctx context.Context, bucket string) error
	// DeleteBucket deletes an empty bucket.
	DeleteBucket(ctx context.Context, bucket string) error
	// ReadBlockPublicAccess reads the bucket's Block Public Access settings
	// back: allOn says whether all four are on, and the error is why they
	// could not be read. (InspectPrivacy cannot say a refused read from an
	// empty one.)
	ReadBlockPublicAccess(ctx context.Context, bucket string) (allOn bool, err error)
	// InspectPrivacy reads the bucket's public access controls back.
	InspectPrivacy(ctx context.Context, bucket string) storage.PrivacyReport
}

func (e Env) awsBucketCreator(profile, region string) (BucketCreator, error) {
	if e.AWSBucketCreator != nil {
		return e.AWSBucketCreator(profile, region)
	}
	return openAWSBucketCreator(profile, region)
}

// openAWSBucketCreator is Env.AWSBucketCreator's default: a client for
// profile in region, the region the bucket is created in. It makes one
// attempt per call: a CreateBucket whose answer was lost and is retried
// would find its own bucket and read as a taken name. The package's tests
// replace it with one that fails, so a test that leaves Env.AWSBucketCreator
// unset never reaches AWS.
var openAWSBucketCreator = func(profile, region string) (BucketCreator, error) {
	ctx, cancel := context.WithTimeout(context.Background(), bucketDiscoveryTimeout)
	defer cancel()
	cfg, err := credentials.LoadAWSConfig(ctx, profile, region)
	if err != nil {
		return nil, err
	}
	return storage.NewBucketAdmin(storage.NewClient(cfg, "", true, 1), cfg), nil
}

// bucketCreateTimeout bounds each call that creates, secures or deletes a
// bucket.
const bucketCreateTimeout = 60 * time.Second

// promptS3Bucket asks for the S3 profile, then for a bucket: a new
// one when create is set, else an existing one.
func promptS3Bucket(p *prompter, cfg *credentials.Config, env Env, failedRegion string, create bool) error {
	if !create {
		return promptS3Location(p, cfg, env, failedRegion)
	}
	profileRegion, noCredentials, err := chooseS3Profile(p, cfg, env)
	if err != nil {
		return err
	}
	creationProfile := cfg.AWSProfile
	bucket, created, err := createS3Bucket(p, cfg, env, profileRegion, noCredentials)
	if err != nil {
		return err
	}
	if !created {
		if cfg.AWSProfile != creationProfile {
			// Creation customization and recovery can select another profile.
			// Never carry the old profile region or credential status into fallback.
			profileRegion, noCredentials = "", false
			if profiles, lookupErr := env.awsProfiles(); lookupErr == nil {
				for _, profile := range profiles {
					if profile.Name == cfg.AWSProfile {
						profileRegion, noCredentials = profile.Region, profile.NoCredentials
						if !validRegion(profileRegion) {
							profileRegion = ""
						}
						break
					}
				}
			}
		}
		return promptS3ExistingBucket(p, cfg, env, failedRegion, profileRegion, noCredentials)
	}
	// Two different choices: the profile that created the bucket (asked
	// first) and the one archiving will use (asked here). A bucket offered
	// again from earlier in the run was already through this question.
	if bucket.fresh {
		if err := chooseArchiveProfile(p, cfg, env, bucket); err != nil {
			return err
		}
	}
	cfg.Bucket, cfg.Region = bucket.name, bucket.region
	return nil
}

// newS3Bucket is the bucket createS3Bucket made or offered again.
type newS3Bucket struct {
	name   string
	region string
	// fresh is set for a bucket created just now, which still needs the
	// choice of the profile to archive with.
	fresh bool
}

// chooseArchiveProfile asks which profile archiving should use, after the
// bucket was created with another. The profile that created it is the
// default, and it can do far more than archiving needs. The answer is
// recorded in cfg.AWSProfile; the bucket and its region are not cfg's yet, so
// a change of profile cannot clear them.
func chooseArchiveProfile(p *prompter, cfg *credentials.Config, env Env, bucket newS3Bucket) error {
	terminal.Println(p.out, "Setup created bucket "+bucket.name+" in "+bucket.region+"; it is empty and will stay in your account if you stop now.")
	p.storageStage(storageDetails)
	terminal.Println(p.out, p.style.hang("", "Setup saves the creation profile unless you choose another now. For a narrower profile, view the archive permissions and attach that policy first."))
	for {
		choice, err := p.setupActions("Profile for archiving", "keep", nil, []actionOption{{"keep", "", "Use the selected profile"}, {"profile", "p", "Choose another archive profile"}, {"permissions", "v", "View archive permissions"}})
		if err != nil {
			return err
		}
		if choice == "permissions" {
			printRuntimePolicyAdvice(p, bucket.name, firstNonEmpty(cfg.Prefix, defaultPrefix))
			continue
		}
		if choice == "profile" {
			if _, _, err := chooseS3Profile(p, cfg, env); err != nil {
				return err
			}
		}
		setArchiveProfile(p, bucket.name, cfg.AWSProfile)
		return nil
	}
}

// createS3Bucket creates a bucket with the profile in cfg and returns its
// name and region; it writes neither to cfg. It returns false, having said
// why, only when the user explicitly chooses an existing bucket.
func createS3Bucket(p *prompter, cfg *credentials.Config, env Env, profileRegion string, noCredentials bool) (newS3Bucket, bool, error) {
	p.storageStage(storageDetails)
	if offered, used, err := offerCreatedBucket(p, cfg.AWSProfile); err != nil || used {
		return offered, used, err
	}
	name := newBucketName()
	region := firstNonEmpty(cfg.Region, profileRegion)
	explicitRegion := cfg.Region != ""
	automaticName, regenerated := true, false
	uncertain := map[string]bool{}
	for {
		p.storageStage(storageDetails)
		var creator BucketCreator
		var err error
		if noCredentials {
			terminal.Printf(p.out, "Profile %s has no credentials configured. Choose another profile or use an existing bucket.\n", cfg.AWSProfile)
		} else {
			if region == "" {
				region, err = promptRegion(p, "Region for the new bucket (for example us-east-1)", "")
				if err != nil {
					return newS3Bucket{}, false, err
				}
				explicitRegion = true
			}
			settings := s3CreationSettings{name: name, region: region, explicitRegion: explicitRegion, automaticName: automaticName, noCredentials: noCredentials}
			proceed, e := settings.confirm(p, cfg, env)
			name, region, explicitRegion, automaticName, noCredentials = settings.name, settings.region, settings.explicitRegion, settings.automaticName, settings.noCredentials
			if e != nil || !proceed {
				return newS3Bucket{}, false, e
			}
			if !noCredentials && region == "" {
				continue
			}
			creator = openS3CreationClient(p, cfg, env, region, name, noCredentials, uncertain[name])
		}
		if creator != nil {
			created, createErr, receiptErr := requestS3Bucket(p, env, creator, cfg.AWSProfile, name, region)
			if receiptErr != nil {
				return newS3Bucket{}, false, receiptErr
			}
			err = createErr
			if err == nil {
				return finishNewS3Bucket(p, creator, created)
			} else if errors.Is(err, storage.ErrBucketNameTaken) && automaticName && !regenerated {
				terminal.Printf(p.out, "The name %s is taken; preparing another suggested name.\n", name)
				name, regenerated = newBucketName(), true
				continue
			} else {
				if errors.Is(err, storage.ErrBucketNameTaken) || errors.Is(err, storage.ErrInvalidBucketName) {
					terminal.Printf(p.out, "S3 could not use the name %s. Customize the name or use an existing bucket.\n", name)
				} else {
					noteCreateFailure(p, cfg.AWSProfile, name, err)
				}
				if errors.Is(err, storage.ErrBucketMayExist) {
					uncertain[name] = true
				}
			}
		}
		def := string(s3RecoveryRetry)
		options := []option{{string(s3RecoveryRetry), "Review settings and retry"}, {string(s3RecoveryProfile), "Choose another creation profile"}}
		if noCredentials {
			def, options = string(s3RecoveryProfile), options[1:]
		}
		choice, e := p.setupActions("What next?", def, options, []actionOption{{"existing", "e", "Use an existing bucket"}, {"back", "b", "Back"}, {"stop", "q", "Stop setup"}})
		if e != nil {
			return newS3Bucket{}, false, e
		}
		switch s3RecoveryChoice(choice) {
		case s3RecoveryExisting:
			return newS3Bucket{}, false, nil
		case s3RecoveryBack:
			return newS3Bucket{}, false, errChooseStorageAgain
		case s3RecoveryStop:
			return newS3Bucket{}, false, errors.New("guided bucket creation stopped")
		case s3RecoveryRetry:
			continue
		case s3RecoveryProfile:
			profileRegion, noCredentials, err = chooseS3Profile(p, cfg, env)
			if err != nil {
				return newS3Bucket{}, false, err
			}
			if !explicitRegion {
				region = profileRegion
			}
		}
	}
}

// openS3CreationClient declines unsafe or incomplete settings before loading credentials.
func openS3CreationClient(p *prompter, cfg *credentials.Config, env Env, region, name string, noCredentials, uncertain bool) BucketCreator {
	if noCredentials || region == "" {
		return nil
	}
	if !standardAWSRegion(region) {
		terminal.Println(p.out, "Setup can only create buckets in the standard AWS regions. Customize the region or use an existing bucket.")
		return nil
	}
	if uncertain {
		terminal.Printf(p.out, "The bucket %s may have been created by an earlier request. Check the S3 console; customize the name before trying another creation.\n", name)
		return nil
	}
	release := p.suspendPrompts()
	creator, err := env.awsBucketCreator(cfg.AWSProfile, region)
	release()
	if err != nil {
		terminal.Printf(p.out, "Couldn't open profile %s (%s).\n", cfg.AWSProfile, discoveryReason(err))
	}
	return creator
}

// s3RecoveryChoice is an action after a bucket creation failure.
type s3RecoveryChoice string

const (
	s3RecoveryRetry    s3RecoveryChoice = "retry"
	s3RecoveryProfile  s3RecoveryChoice = "profile"
	s3RecoveryExisting s3RecoveryChoice = "existing"
	s3RecoveryBack     s3RecoveryChoice = "back"
	s3RecoveryStop     s3RecoveryChoice = "stop"
)

// s3SummaryChoice is an action on the creation summary.
type s3SummaryChoice string

const (
	s3SummaryCreate    s3SummaryChoice = "create"
	s3SummaryCustomize s3SummaryChoice = "customize"
	s3SummaryExisting  s3SummaryChoice = "existing"
	s3SummaryBack      s3SummaryChoice = "back"
)

// s3CustomizeChoice is a setting to edit before creation.
type s3CustomizeChoice string

const (
	s3CustomizeName    s3CustomizeChoice = "name"
	s3CustomizeRegion  s3CustomizeChoice = "region"
	s3CustomizeProfile s3CustomizeChoice = "profile"
	s3CustomizeBack    s3CustomizeChoice = "back"
)

// s3CreationSettings keeps customization separate from bucket mutations.
type s3CreationSettings struct {
	name           string
	region         string
	explicitRegion bool
	automaticName  bool
	noCredentials  bool
}

// confirm returns false only for the explicit existing-bucket route.
func (s *s3CreationSettings) confirm(p *prompter, cfg *credentials.Config, env Env) (bool, error) {
	for {
		p.storageStage(storageDetails)
		if !standardAWSRegion(s.region) {
			terminal.Println(p.out, "Setup can only create buckets in the standard AWS regions. Customize the region or use an existing bucket.")
		}
		terminal.Printf(p.out, "Profile: %s\nBucket: %s\nRegion: %s\nSetup will enable and check all four Block Public Access settings.\n", cfg.AWSProfile, s.name, s.region)
		choice, err := p.setupActions("Your archive storage", "create", nil, []actionOption{{"create", "", "Create"}, {"customize", "c", "Customize name, region or profile"}, {"existing", "e", "Use an existing bucket"}, {"back", "b", "Back"}})
		if err != nil {
			return false, err
		}
		switch s3SummaryChoice(choice) {
		case s3SummaryExisting:
			return false, nil
		case s3SummaryBack:
			return false, errChooseStorageAgain
		case s3SummaryCustomize:
			if err := s.customize(p, cfg, env); err != nil {
				return false, err
			}
			if s.noCredentials || s.region == "" {
				return true, nil
			}
		case s3SummaryCreate:
			if standardAWSRegion(s.region) {
				return true, nil
			}
		}
	}
}

func (s *s3CreationSettings) customize(p *prompter, cfg *credentials.Config, env Env) error {
	edit, err := p.setupActions("Customize storage", "name", []option{{"name", "Bucket name"}, {"region", "Region"}, {"profile", "Creation profile"}}, []actionOption{{"back", "b", "Back to summary"}})
	if err != nil {
		return err
	}
	switch s3CustomizeChoice(edit) {
	case s3CustomizeBack:
		return nil
	case s3CustomizeName:
		s.name, err = promptNewBucketName(p, s.name)
		s.automaticName = false
	case s3CustomizeRegion:
		s.region, err = promptRegion(p, "Region for the new bucket", s.region)
		s.explicitRegion = true
	case s3CustomizeProfile:
		var profileRegion string
		profileRegion, s.noCredentials, err = chooseS3Profile(p, cfg, env)
		if !s.explicitRegion {
			s.region = profileRegion
		}
	}
	return err
}

// offerCreatedBucket offers a bucket this run already created and secured
// with profile (or the one chosen to archive with) instead of creating
// another. used is set when it is taken, and bucket is then that bucket.
func offerCreatedBucket(p *prompter, profile string) (bucket newS3Bucket, used bool, err error) {
	for _, b := range p.createdBuckets {
		if !b.secured || (profile != b.profile && profile != b.archiveProfile) {
			continue
		}
		terminal.Printf(p.out, "Setup already created bucket %s in %s in this run; it is empty.\n", b.name, b.region)
		use, err := p.setupYesNo("Use it instead of creating another bucket?", true)
		if err != nil {
			return newS3Bucket{}, false, err
		}
		if use {
			return newS3Bucket{name: b.name, region: b.region}, true, nil
		}
	}
	return newS3Bucket{}, false, nil
}

func rememberSecuredBucket(p *prompter, name string) {
	for i := range p.createdBuckets {
		if p.createdBuckets[i].name == name {
			p.createdBuckets[i].secured = true
		}
	}
}

func setArchiveProfile(p *prompter, name, profile string) {
	for i := range p.createdBuckets {
		if p.createdBuckets[i].name == name {
			p.createdBuckets[i].archiveProfile = profile
		}
	}
}

func forgetCreatedBucket(p *prompter, name string) {
	kept := p.createdBuckets[:0]
	for _, b := range p.createdBuckets {
		if b.name != name {
			kept = append(kept, b)
		}
	}
	p.createdBuckets = kept
}

// savedStorageUses says whether the saved configuration (committed) or the
// saved setup draft (drafted) uses provider's bucket, so a note about a bucket
// setup created can say whether it is in use.
func savedStorageUses(home, provider, bucket string) (committed, drafted bool) {
	uses := func(cfg credentials.Config) bool { return cfg.Provider == provider && cfg.Bucket == bucket }
	if cfg, found, err := config.Load(home); err == nil && found {
		committed = uses(cfg.Storage)
	}
	if draft, found, problem, err := readDraft(home); err == nil && found && problem == "" {
		drafted = uses(draft.Config.Storage)
	}
	return committed, drafted
}

// noteUnusedCreatedBuckets says, once setup is over, which buckets it created
// that the saved configuration does not use, so none is left behind without
// the person knowing. A bucket that is in use is not mentioned.
func noteUnusedCreatedBuckets(p *prompter, home string) {
	if len(p.createdBuckets) == 0 {
		return
	}
	for _, b := range p.createdBuckets {
		active, drafted := savedStorageUses(home, credentials.ProviderS3, b.name)
		if active {
			continue
		}
		note := "Setup created bucket " + b.name + " in " + b.region + "; it is empty. Delete it in the S3 console if you don't want it."
		if drafted {
			note = "Setup created bucket " + b.name + " in " + b.region + "; it is empty. Your saved setup draft uses it, so running setup again will resume with it. To not use it, choose a different bucket there and delete this one in the S3 console."
		}
		if !b.secured {
			note += " Block Public Access is not on for it."
		}
		terminal.Println(p.out, note)
	}
}

// standardRegion matches the region names of the standard AWS partition: a
// continent or area, one direction, a number. Anything else (cn-*, us-gov-*,
// the isolated and sovereign partitions' names) is not matched, so a region
// this list has not been told about is declined, not guessed at.
var standardRegion = regexp.MustCompile(`^(us|eu|ap|sa|ca|me|af|il|mx)-[a-z]+-[0-9]{1,2}$`)

// standardAWSRegion reports whether region is one of the standard AWS
// partition's. ARNs, and so the runtime policy, differ by partition, and
// guided creation is only built and worded for the standard one.
func standardAWSRegion(region string) bool { return standardRegion.MatchString(region) }

// promptNewBucketName asks for a bucket name until it is one S3 accepts.
func promptNewBucketName(p *prompter, def string) (string, error) {
	return p.guidedText(promptModel{Question: "Name for the new bucket", Label: "Name", Default: def, Receipt: "Bucket", Validate: func(name string) error {
		if problem := bucketNameProblem(name); problem != "" {
			return fmt.Errorf("%q can't be a bucket name: %s", name, problem)
		}
		return nil
	}})
}

// noteCreateFailure says in plain words why the bucket called name could not
// be created, and offers explicit recovery choices. It never quotes
// S3's error. When the answer was unclear the bucket may exist, so it says
// where to look and offers nothing that deletes.
func noteCreateFailure(p *prompter, profile, name string, err error) {
	const fallback = " Review the settings and retry, or choose an existing bucket."
	d := storage.Diagnose(err)
	switch {
	case storage.IsCredentialsCheck(err):
		// Not a refusal to create: the check that the credentials work failed.
		if d.Cause == storage.CauseNoCredentials {
			terminal.Println(p.out, "Couldn't check the credentials of profile "+profile+" with AWS. "+d.Explanation)
			terminal.Println(p.out, strings.ReplaceAll(d.Fix, "<profile>", profile)+fallback)
		} else {
			terminal.Println(p.out, "Couldn't check the credentials of profile "+profile+" with AWS ("+discoveryReason(err)+"). Check that they are current, then run setup again."+fallback)
		}
	case errors.Is(err, storage.ErrBucketMayExist):
		terminal.Printf(p.out, "S3 didn't answer clearly, so the bucket %s may have been created. Check the S3 console for it, and delete it there if you don't want it; setup won't touch it.%s\n", name, fallback)
	case errors.Is(err, storage.ErrTooManyBuckets):
		terminal.Println(p.out, "This AWS account has reached its limit on buckets. Delete one you don't need, or ask AWS to raise the limit."+fallback)
	case d.Cause == storage.CauseAccessDenied:
		terminal.Printf(p.out, "Profile %s isn't allowed to create buckets. This step needs a profile with s3:CreateBucket and s3:PutBucketPublicAccessBlock (and s3:DeleteBucket to undo a failed attempt);\n", profile)
		terminal.Println(p.out, "an organization policy (a service control policy or permissions boundary) can also forbid it. Use a profile that may create buckets, for this step only."+fallback)
	case d.Cause == storage.CauseWrongRegion:
		// For CreateBucket this is S3 refusing the region, not a bucket in
		// another one.
		terminal.Println(p.out, "S3 didn't accept that region for a new bucket. Check its name, and that your account has the region enabled."+fallback)
	case d.Cause == storage.CauseNoCredentials || d.Cause == storage.CauseNetwork:
		terminal.Println(p.out, "Couldn't create the bucket. "+d.Explanation)
		terminal.Println(p.out, strings.ReplaceAll(d.Fix, "<profile>", profile)+fallback)
	default:
		terminal.Println(p.out, "Couldn't create the bucket: S3 or STS returned an error setup doesn't recognize. Check the region and your AWS account, then run setup again."+fallback)
	}
}

// secureChoice is what the person chose after Block Public Access could not
// be turned on for the new bucket.
type secureChoice string

const (
	secureRetry  secureChoice = "retry"
	secureDelete secureChoice = "delete"
	secureStop   secureChoice = "stop"
)

// secureNewBucket turns on Block Public Access for a bucket this run just
// created and reads it back. Until that works the bucket is not used, so
// nothing is uploaded to it: setup offers to try again, delete the (empty)
// bucket, or stop, and never carries on. It returns false, with a nil
// error, when the bucket was deleted and setup asks for an existing one.
func secureNewBucket(p *prompter, creator BucketCreator, bucket, profile string) (secured bool, err error) {
	for {
		p.storageStage(storageVerification)
		problem := blockPublicAccess(p, creator, bucket)
		if problem == "" {
			return true, nil
		}
		terminal.Printf(p.out, "Bucket %s was created, but %s\n", bucket, problem)
		terminal.Println(p.out, "Setup won't store sessions in it until Block Public Access is on.")
		choice, err := askSecureChoice(p, creator, bucket, profile)
		if err != nil {
			return false, err
		}
		switch choice {
		case secureDelete:
			return false, nil
		case secureStop:
			return false, fmt.Errorf("bucket %s was created but Block Public Access is not on; turn it on in the S3 console, or delete the bucket, then run setup again", bucket)
		case secureRetry:
		}
	}
}

// askSecureChoice asks what to do about a bucket whose Block Public Access
// is not on, and carries out a deletion itself. S3 gives a successful
// CreateBucket no sign that the bucket was created just now rather than
// already being the account's (in us-east-1 it answers both alike), and two
// runs that pick the same name at the same moment can both succeed, so
// deleting is never done on a single keypress: the person types the
// bucket's name to confirm, and S3 refuses to delete one that has objects.
func askSecureChoice(p *prompter, creator BucketCreator, bucket, profile string) (secureChoice, error) {
	for {
		answer, err := p.setupMenu("What now?", string(secureRetry),
			option{string(secureRetry), "Try again"},
			option{string(secureDelete), "Delete the empty bucket and pick an existing one"},
			option{string(secureStop), "Stop setup and leave the bucket as it is"})
		if err != nil {
			return secureStop, err
		}
		if secureChoice(answer) != secureDelete {
			return secureChoice(answer), nil
		}
		terminal.Println(p.out, "Only delete it if setup just created it: a bucket you already owned under this name isn't yours to delete here.")
		typed, err := p.guidedText(promptModel{Question: "Type the bucket name " + bucket + " to delete it, or press Enter to keep it", Label: "Answer", ResolveReceipt: func(value string) string {
			if value == bucket {
				return "Deletion confirmed for bucket " + bucket
			}
			return "Deletion not confirmed; keeping bucket " + bucket
		}})
		if err != nil {
			return secureStop, err
		}
		if typed == bucket {
			deleteNewBucket(p, creator, bucket, profile)
			return secureDelete, nil
		}
		if typed == "" {
			terminal.Println(p.out, "Not deleted.")
		} else {
			terminal.Printf(p.out, "Not deleted: %q isn't %s.\n", typed, bucket)
		}
	}
}

// Block Public Access is read back a few times, since a bucket that was
// just created can briefly answer "no such bucket" or nothing to the calls
// that follow it.
const bucketSettleAttempts = 3

// bucketSettleDelay is the wait between those attempts. A variable only so
// tests do not wait.
var bucketSettleDelay = time.Second

// settle calls try until it reports it is finished, at most
// bucketSettleAttempts times, bucketSettleDelay apart.
func settle(try func() (finished bool)) {
	for attempt := range bucketSettleAttempts {
		if attempt > 0 {
			time.Sleep(bucketSettleDelay)
		}
		if try() {
			return
		}
	}
}

// blockPublicAccess turns Block Public Access on for bucket and reads it
// back, and returns why not when it could not, or "" on success. A
// read-back the profile may not make (no s3:GetBucketPublicAccessBlock) is a
// warning, not a failure: setup's storage check reads it again with the
// profile it saves. A read-back that shows a setting off is a failure.
func blockPublicAccess(p *prompter, creator BucketCreator, bucket string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*bucketCreateTimeout)
	defer cancel()
	var err error
	settle(func() bool {
		err = creator.BlockPublicAccess(ctx, bucket)
		return err == nil || storage.Diagnose(err).Cause != storage.CauseNoSuchBucket
	})
	if err != nil {
		if storage.Diagnose(err).Cause == storage.CauseAccessDenied {
			return "setup couldn't turn on Block Public Access: the profile needs s3:PutBucketPublicAccessBlock, and an organization policy can also forbid it."
		}
		return "setup couldn't turn on Block Public Access (" + discoveryReason(err) + ")."
	}
	// The settings are read once, by the call that can tell a refused read from
	// an empty one, and retried only for the latter. With all four on the
	// bucket is verified private (that is InspectPrivacy's own rule, so it is
	// not asked again); otherwise InspectPrivacy looks for a public policy or
	// ACL, which is a failure.
	var allOn bool
	var readErr error
	settle(func() bool {
		allOn, readErr = creator.ReadBlockPublicAccess(ctx, bucket)
		return readErr == nil || storage.Diagnose(readErr).Cause == storage.CauseAccessDenied
	})
	if readErr == nil && !allOn {
		return "Block Public Access reads back with a setting still off."
	}
	if readErr != nil {
		if report := creator.InspectPrivacy(ctx, bucket); report.State == "public_or_risky" {
			return "it still looks public after Block Public Access was turned on (" + privacyReasonText(report.Reason) + ")."
		}
	}
	switch {
	case readErr == nil:
		terminal.Println(p.out, "  "+p.style.okMark()+" Checked: Block Public Access is on for all four settings.")
	case storage.Diagnose(readErr).Cause == storage.CauseAccessDenied:
		p.warn("Block Public Access was turned on, but this profile can't read it back to confirm (that needs s3:GetBucketPublicAccessBlock). Setup checks again at the review.")
	default:
		p.warn("Block Public Access was turned on, but setup couldn't read it back to confirm (" + discoveryReason(readErr) + "). Setup checks again at the review.")
	}
	return ""
}

// deleteNewBucket deletes the empty bucket this run created. When S3 refuses,
// it says how to delete the bucket by hand; setup goes on to an existing
// bucket either way.
func deleteNewBucket(p *prompter, creator BucketCreator, bucket, profile string) {
	ctx, cancel := context.WithTimeout(context.Background(), bucketCreateTimeout)
	defer cancel()
	// Record the cleanup request before sending it: a crash or lost reply
	// must leave the bucket eligible for later cleanup, never marked usable.
	for _, created := range p.createdBuckets {
		if created.name == bucket && created.profile == profile && created.receipt.ID != "" {
			if err := setupresources.Finish(created.home, created.receipt, setupresources.CleanupPending); err != nil {
				p.warn("Bucket was not deleted because its cleanup intent could not be saved.")
				return
			}
		}
	}
	if err := creator.DeleteBucket(ctx, bucket); err != nil {
		terminal.Printf(p.out, "Couldn't delete bucket %s (%s). It is empty; delete it yourself with:\n", bucket, discoveryReason(err))
		terminal.Printf(p.out, "  aws s3api delete-bucket --bucket %s --profile %s\n", bucket, profile)
		return
	}
	for _, created := range p.createdBuckets {
		if created.name == bucket && created.profile == profile && created.receipt.ID != "" {
			if err := setupresources.Finish(created.home, created.receipt, setupresources.Deleted); err != nil {
				p.warn("Bucket deleted, but its recovery receipt could not be updated. Check the account before acting on that receipt.")
			}
		}
	}
	forgetCreatedBucket(p, bucket)
	terminal.Printf(p.out, "Deleted bucket %s.\n", bucket)
}

// newBucketName is a default bucket name: agent-archive- and eight random
// hex digits, since bucket names are shared by everyone on AWS. Tests replace
// it.
var newBucketName = func() string {
	var random [4]byte
	if _, err := rand.Read(random[:]); err != nil {
		// crypto/rand failing is extraordinarily unlikely; the clock still
		// makes a name that a person can change or S3 can refuse.
		return fmt.Sprintf("agent-archive-%x", time.Now().UnixNano()&0xffffffff)
	}
	return "agent-archive-" + hex.EncodeToString(random[:])
}

var (
	bucketNameShape = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*[a-z0-9]$`)
	bucketNameIPv4  = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$`)
)

// reservedBucketPrefixes and reservedBucketSuffixes are what S3 refuses at
// the start and end of a general purpose bucket name.
var (
	reservedBucketPrefixes = []string{"xn--", "sthree-", "amzn-s3-demo-"}
	reservedBucketSuffixes = []string{"-s3alias", "--ol-s3", ".mrap", "--x-s3", "--table-s3", "-an"}
)

// bucketNameProblem says in a few words why name is not an S3 bucket name,
// or returns "" when it is one. The rules are S3's for general purpose
// buckets: 3 to 63 lowercase letters, digits, hyphens and dots, starting and
// ending with a letter or digit, no adjacent dots, not shaped like an IP
// address, and none of the reserved prefixes and suffixes.
func bucketNameProblem(name string) string {
	switch {
	case len(name) < 3 || len(name) > 63:
		return "it must be 3 to 63 characters long"
	case !bucketNameShape.MatchString(name):
		return "use only lowercase letters, digits, hyphens and dots, starting and ending with a letter or digit"
	case strings.Contains(name, ".."):
		return "it can't have two dots in a row"
	case bucketNameIPv4.MatchString(name):
		return "it can't look like an IP address"
	}
	for _, prefix := range reservedBucketPrefixes {
		if strings.HasPrefix(name, prefix) {
			return "S3 reserves names starting with " + prefix
		}
	}
	for _, suffix := range reservedBucketSuffixes {
		if strings.HasSuffix(name, suffix) {
			return "S3 reserves names ending with " + suffix
		}
	}
	return ""
}

// runtimePolicyTemplate is the runtime policy of
// docs/security/bucket-permissions.md, for the placeholder bucket
// my-archive-bucket and prefix agent-archive. The document is what people
// read and copy; TestRuntimePolicyMatchesDocument fails when this text and
// the document's differ.
const runtimePolicyTemplate = `{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "ArchiveObjects",
      "Effect": "Allow",
      "Action": ["s3:PutObject", "s3:GetObject", "s3:DeleteObject"],
      "Resource": "arn:aws:s3:::my-archive-bucket/agent-archive/*"
    },
    {
      "Sid": "ListArchivePrefix",
      "Effect": "Allow",
      "Action": "s3:ListBucket",
      "Resource": "arn:aws:s3:::my-archive-bucket",
      "Condition": {"StringLike": {"s3:prefix": ["agent-archive/*"]}}
    },
    {
      "Sid": "InspectBucketPrivacyReadOnly",
      "Effect": "Allow",
      "Action": ["s3:GetBucketPublicAccessBlock", "s3:GetBucketPolicyStatus", "s3:GetBucketAcl"],
      "Resource": "arn:aws:s3:::my-archive-bucket"
    }
  ]
}`

// runtimePolicy is the runtime policy for bucket and prefix.
func runtimePolicy(bucket, prefix string) string {
	return strings.NewReplacer(
		"my-archive-bucket", bucket,
		"agent-archive/*", strings.Trim(prefix, "/")+"/*",
	).Replace(runtimePolicyTemplate)
}

// policyPrefix is what setup will put into a policy as the folder: letters,
// digits and . _ - / only. Nothing that is special in an IAM policy or in
// JSON (a quote, *, ?, $, whitespace, a control character) is substituted into
// the template.
var policyPrefix = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// policyPrefixOK reports whether prefix, without its slashes at the ends, is
// something setup will put into a policy.
func policyPrefixOK(prefix string) bool {
	trimmed := strings.Trim(prefix, "/")
	return trimmed != "" && policyPrefix.MatchString(trimmed)
}

// printRuntimePolicyAdvice prints the least-privilege policy for the new
// bucket and recommends a separate runtime profile, since the profile used
// to create the bucket is usually much broader than archiving needs and is
// what setup saves.
func printRuntimePolicyAdvice(p *prompter, bucket, prefix string) {
	terminal.Println(p.out, "")
	terminal.Println(p.out, p.style.bold("Recommended: archive with a narrower profile"))
	if !policyPrefixOK(prefix) {
		terminal.Println(p.out, p.style.hang("", "The folder name "+fmt.Sprintf("%q", prefix)+" has characters setup won't put into a policy (only letters, digits and . _ - / are used), so it doesn't print one. Write the policy yourself from the bucket permissions guide, for bucket "+bucket+":"))
		terminal.Println(p.out, "https://github.com/wangjohn/agent-archive/blob/main/docs/security/bucket-permissions.md")
		terminal.Println(p.out, "")
		return
	}
	terminal.Println(p.out, p.style.hang("", "The profile you just used can do far more than archiving needs. Attach this policy to a separate IAM user or role, and save its access key as its own AWS profile:"))
	terminal.Println(p.out, "")
	terminal.Println(p.out, runtimePolicy(bucket, prefix))
	terminal.Println(p.out, "")
	terminal.Println(p.out, p.style.dim("This policy hasn't been tested against a real AWS bucket yet. If the storage check fails once you switch to the new profile, see the note in the bucket permissions guide."))
	terminal.Println(p.out, "More: https://github.com/wangjohn/agent-archive/blob/main/docs/security/bucket-permissions.md")
	terminal.Println(p.out, "")
}

// s3CreationRefused distinguishes affirmative refusals from missing answers.
func s3CreationRefused(err error) bool {
	if errors.Is(err, storage.ErrBucketMayExist) {
		return false
	}
	if errors.Is(err, storage.ErrBucketNameTaken) || errors.Is(err, storage.ErrInvalidBucketName) || errors.Is(err, storage.ErrTooManyBuckets) {
		return true
	}
	var apiErr smithy.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorFault() == smithy.FaultClient
}

// requestS3Bucket keeps persistence failures distinct from provider refusals,
// which the interactive flow may offer to retry.
func requestS3Bucket(p *prompter, env Env, creator BucketCreator, profile, name, region string) (created createdS3Bucket, providerErr, receiptErr error) {
	home, err := env.readHome()
	if err != nil {
		return created, nil, err
	}
	if err = p.refuseUncertainCreation(home, setupresources.Receipt{Provider: setupresources.S3, Profile: profile, Bucket: name, Region: region, CreatedAt: env.now()}); err != nil {
		return created, nil, err
	}
	receipt, err := setupresources.Begin(home, setupresources.Receipt{Provider: setupresources.S3, Profile: profile, Bucket: name, Region: region, CreatedAt: env.now()})
	if err != nil {
		return created, nil, err
	}
	p.rememberResourceRequest(receipt.ID)
	created = createdS3Bucket{name: name, region: region, profile: profile, receipt: receipt, home: home}
	ctx, cancel := context.WithTimeout(context.Background(), bucketCreateTimeout)
	providerErr = creator.CreateBucket(ctx, name, region)
	cancel()
	if providerErr == nil {
		receiptErr = setupresources.Finish(home, receipt, setupresources.Created)
	} else if s3CreationRefused(providerErr) {
		receiptErr = setupresources.Finish(home, receipt, setupresources.Refused)
	}
	return created, providerErr, receiptErr
}

// finishNewS3Bucket secures only a confirmed creation and preserves its outcome.
func finishNewS3Bucket(p *prompter, creator BucketCreator, created createdS3Bucket) (newS3Bucket, bool, error) {
	terminal.Printf(p.out, "Created bucket %s in %s.\n", created.name, created.region)
	p.createdBuckets = append(p.createdBuckets, created)
	secured, err := secureNewBucket(p, creator, created.name, created.profile)
	if err != nil {
		return newS3Bucket{}, false, err
	}
	if !secured {
		// The delete action explicitly chose existing storage.
		return newS3Bucket{}, false, nil
	}
	if err = setupresources.Finish(created.home, created.receipt, setupresources.Secured); err != nil {
		return newS3Bucket{}, false, err
	}
	rememberSecuredBucket(p, created.name)
	return newS3Bucket{name: created.name, region: created.region, fresh: true}, true, nil
}
