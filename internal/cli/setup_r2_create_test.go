package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/aws/smithy-go"

	"github.com/wangjohn/agent-archive/internal/cloudflare"
	"github.com/wangjohn/agent-archive/internal/cloudflare/cloudflaretest"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/issuance"
	"github.com/wangjohn/agent-archive/internal/setupresources"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// bootstrapCanary stands for the pasted Cloudflare API token. It must never
// show up anywhere but the request that carries it to the fake.
const bootstrapCanary = "CANARY-cf-bootstrap-token-7f3a91"

// trackedAPI is a cloudflare.API that notes when it was discarded.
type trackedAPI struct {
	cloudflare.API
	discarded bool
	// createToken, when set, replaces CreateToken.
	createToken func(context.Context, string, cloudflare.TokenSpec) (cloudflare.Token, error)
	// deleteToken, when set, replaces DeleteToken; next does the real one.
	deleteToken func(ctx context.Context, account, id string, next func() error) error
}

func (a *trackedAPI) DeleteToken(ctx context.Context, account, id string) error {
	if a.deleteToken != nil {
		return a.deleteToken(ctx, account, id, func() error { return a.API.DeleteToken(ctx, account, id) })
	}
	return a.API.DeleteToken(ctx, account, id)
}

func (a *trackedAPI) CreateToken(ctx context.Context, account string, spec cloudflare.TokenSpec) (cloudflare.Token, error) {
	if a.createToken != nil {
		return a.createToken(ctx, account, spec)
	}
	return a.API.CreateToken(ctx, account, spec)
}

func (a *trackedAPI) Discard() {
	a.discarded = true
	a.API.Discard()
}

// guidedR2Fixture is a first setup in ~/src/web-app with a fake Cloudflare.
type guidedR2Fixture struct {
	*screenFixture
	cf       *cloudflaretest.Server
	keychain *fakeKeychain
	apis     []*trackedAPI
	pauses   int
	// createToken and deleteToken are set on every client the fixture makes.
	createToken func(context.Context, string, cloudflare.TokenSpec) (cloudflare.Token, error)
	deleteToken func(ctx context.Context, account, id string, next func() error) error
}

func newGuidedR2Fixture(t *testing.T) *guidedR2Fixture {
	t.Helper()
	f := newScreenFixture(t)
	f.withApps(t, "claude")
	f.inWebApp(t)
	g := &guidedR2Fixture{screenFixture: f, cf: cloudflaretest.New(t, bootstrapCanary), keychain: newFakeKeychain()}
	g.setEnv(nil)
	f.env.Credentials = func() (credentials.CredentialStore, error) { return g.keychain, nil }
	f.env.Pause = func(time.Duration) { g.pauses++ }
	f.env.Cloudflare = func(token string) cloudflare.API {
		api := &trackedAPI{createToken: g.createToken, deleteToken: g.deleteToken, API: cloudflare.New(token, cloudflare.Options{
			BaseURL: g.cf.URL + "/client/v4",
			Sleep:   func(context.Context, time.Duration) error { return nil },
		})}
		g.apis = append(g.apis, api)
		return api
	}
	return g
}

// setEnv is the process environment setup sees.
func (g *guidedR2Fixture) setEnv(extra map[string]string) {
	g.env.LookupEnv = func(key string) (string, bool) {
		value, ok := extra[key]
		return value, ok
	}
}

// answers is what a person types: the combined apps-and-project question,
// the storage menu's guided choice, then the rest.
func guidedAnswers(rest ...string) string {
	return strings.Join(append([]string{"", "", "r2-create"}, rest...), "\n") + "\n"
}

// Each answer of the flow that follows the token: the bucket name (default),
// no data location, go ahead, and start archiving.
var (
	askToken     = []string{bootstrapCanary}
	acceptedRest = []string{"", ""}
)

func (g *guidedR2Fixture) happy() string {
	return guidedAnswers(append(append([]string{}, askToken...), acceptedRest...)...)
}

func (g *guidedR2Fixture) run(t *testing.T, input string, want int) string {
	t.Helper()
	var out bytes.Buffer
	if code := Run([]string{"setup"}, strings.NewReader(input), &out, &out, g.env); code != want {
		t.Fatalf("setup exit %d, want %d\n%s", code, want, &out)
	}
	return out.String()
}

func (g *guidedR2Fixture) savedConfig(t *testing.T) config.Config {
	t.Helper()
	cfg, found, err := config.Load(g.home)
	if err != nil || !found {
		t.Fatalf("no saved configuration: %v", err)
	}
	return cfg
}

func (g *guidedR2Fixture) notSaved(t *testing.T) {
	t.Helper()
	if _, found, err := config.Load(g.home); err != nil || found {
		t.Fatalf("a configuration was saved: %v %v", found, err)
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// assertNothingHolds fails if any secret shows in output or in any file the
// run left under the test's Mac: the data directory (configuration, setup
// draft, journal, state), hook files, and the LaunchAgent.
func (g *guidedR2Fixture) assertNothingHolds(t *testing.T, output string, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if setupContainsText(output, secret) {
			t.Errorf("output holds %q", secret)
		}
	}
	var paths []string
	err := filepath.WalkDir(g.root, func(path string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			paths = append(paths, path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	files := 0
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			// A file that went away, or is not ours to read, holds nothing
			// of ours.
			continue
		}
		files++
		for _, secret := range secrets {
			if bytes.Contains(data, []byte(secret)) {
				t.Errorf("%s holds %q", path, secret)
			}
		}
	}
	if files == 0 {
		t.Fatal("the search found no files, so it proved nothing")
	}
}

var defaultBucketName = regexp.MustCompile(`^agent-archive-[0-9a-f]{8}$`)

func routes(reqs []cloudflaretest.Request) []string {
	var names []string
	for _, r := range reqs {
		names = append(names, string(r.Route))
	}
	return names
}

// The whole flow: the permission lookup, the bucket, a token limited to it, the key derived from the
// token and stored as a pasted key would be, and the
// public-access reads, in that order, with setup finishing on the stored key.
func TestGuidedR2CreatesBucketAndScopedKey(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	out := g.run(t, g.happy(), 0)
	home, homeErr := g.env.readHome()
	if homeErr != nil {
		t.Fatal(homeErr)
	}
	receipts, receiptErr := setupresources.List(home)
	if receiptErr != nil || len(receipts) != 1 || receipts[0].State != setupresources.Created {
		t.Fatalf("bucket receipt missing: %v %v", receipts, receiptErr)
	}
	if !setupContainsText(out, "Using Cloudflare account: Test account ("+cloudflaretest.AccountID+")") {
		t.Fatalf("account was not selected automatically:\n%s", out)
	}
	want := []string{
		string(cloudflaretest.RouteAccounts), string(cloudflaretest.RoutePermissionGroups), string(cloudflaretest.RouteCreateBucket),
		string(cloudflaretest.RouteCreateToken), string(cloudflaretest.RouteManagedDomain), string(cloudflaretest.RouteCustomDomains),
		string(cloudflaretest.RouteCreateToken), string(cloudflaretest.RouteCreateToken),
	}
	if got := routes(g.cf.Requests()); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("calls %v, want %v", got, want)
	}
	cfg := g.savedConfig(t)
	if cfg.MachineAssignment == nil || cfg.MachineAssignment.Kind != config.MachineAssignmentR2Own || cfg.MachineAssignment.IssuerID != cfg.MachineID || cfg.MachineAssignment.SlotID == "" || len(cfg.SpareCredentialRefs) != 2 {
		t.Fatal("guided assignment/spare index missing")
	}
	if cfg.BucketPrivacy == nil || cfg.BucketPrivacy.State != "verified_private" || cfg.BucketPrivacy.Reason != "r2_public_domains_disabled" ||
		!slices.Equal(cfg.BucketPrivacy.Checks, []string{"r2_dev_domain", "custom_domains"}) ||
		cfg.BucketPrivacy.ConfigurationID != privacyConfigurationID(cfg) || cfg.BucketPrivacy.CheckedAt == nil ||
		!setupContainsText(out, "✓ Bucket is private") {
		t.Fatalf("guided privacy evidence %+v; output:\n%s", cfg.BucketPrivacy, out)
	}
	if cfg.Storage.Provider != credentials.ProviderR2 || !defaultBucketName.MatchString(cfg.Storage.Bucket) ||
		cfg.Storage.R2AccountID != cloudflaretest.AccountID || cfg.Storage.R2Endpoint != "https://"+cloudflaretest.AccountID+".r2.cloudflarestorage.com" {
		t.Fatalf("storage %+v", cfg.Storage)
	}
	tokens := g.cf.Tokens()
	if len(tokens) != 3 || len(g.cf.Live()) != 3 {
		t.Fatalf("tokens %+v", tokens)
	}
	issued := tokens[0]
	// The stored key is the token's ID and the hash of its value.
	stored, err := g.keychain.Load(context.Background(), cfg.Storage.R2CredentialRef)
	if err != nil || stored.AccessKeyID != issued.ID || stored.SecretAccessKey != sha256Hex(issued.Value) {
		t.Fatalf("stored key %+v (%v), want ID %s", stored, err, issued.ID)
	}
	if len(g.keychain.items) != 3 {
		t.Fatalf("the Keychain holds %d items, want the own key and two spares", len(g.keychain.items))
	}
	// The token has one allow policy, on this bucket only, with the group ID
	// Cloudflare listed, and no expiry.
	body, _ := json.Marshal(issued.Body)
	resource := "com.cloudflare.edge.r2.bucket." + cloudflaretest.AccountID + "_default_" + cfg.Storage.Bucket
	if !setupContainsText(string(body), `"`+resource+`":"*"`) || !setupContainsText(string(body), `"id":"aaaa0000000000000000000000000002"`) || setupContainsText(string(body), "expires_on") {
		t.Fatalf("token request %s", body)
	}
	if len(issued.Name) != 118 || !setupContainsText(out, `named "`+issued.Name+`"`) {
		t.Fatalf("token name %q; output:\n%s", issued.Name, out)
	}
	for _, api := range g.apis {
		if !api.discarded {
			t.Fatal("the bootstrap token was not discarded")
		}
	}
	for _, want := range []string{"Bucket: " + cfg.Storage.Bucket, "Location: automatic", "Created bucket " + cfg.Storage.Bucket, "r2.dev public access: off (checked at setup).", "Custom domains: none enabled (checked at setup).", "Archive key saved.", "not saved anywhere", "Connected to your storage."} {
		if !setupContainsText(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// The instructions come before anything is asked, name the permissions, and
// link directly to Cloudflare's account API tokens page.
func TestGuidedR2GroupsTokenInstructionsAfterTheQuestion(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	out := g.run(t, g.happy(), 0)
	if !setupContainsText(out, "Setup needs a Cloudflare API token to create the bucket and key, and won't save it.") || setupContainsText(out, "This is experimental") {
		t.Fatalf("token introduction is not concise:\n%s", out)
	}
	link := strings.Index(out, "Get your token: "+cloudflare.TokenDashboardURL)
	create := strings.Index(out, "Choose Create Token and use the custom token form")
	intro := strings.Index(out, "Account > Workers R2 Storage > Edit")
	ask := strings.Index(out, "Cloudflare API token (hidden")
	if ask < 0 || link < ask || create < link || intro < create || !setupContainsText(out, "Account > Account API Tokens > Edit") {
		t.Fatalf("instructions:\n%s", out)
	}
	answer := strings.Index(out[ask:], "Credential:")
	if answer < 0 {
		t.Fatalf("missing credential answer cursor\n%s", out)
	}
	for _, want := range []string{
		"Manage account > Account API tokens",
		"Name it agent-archive setup",
		"Account > Workers R2 Storage > Edit",
		"Account > Account API Tokens > Edit",
		"Limit access to this account only",
		"Copy the API token value and paste it below",
		"not the S3 Access Key ID or Secret Access Key",
		"If you see Create Account API token and Object Read & Write",
		"return to Manage account: that is the R2-specific form",
		"choose existing R2 storage and follow the manual steps: " + bucketDocURL,
	} {
		if !setupContainsText(out[ask:ask+answer], want) {
			t.Errorf("instructions before answer cursor lack %q:\n%s", want, out)
		}
	}
}

// The bootstrap token, and the value of the token setup made, are in no file
// the run wrote, no output, and no setup draft, even when setup stops midway.
func TestGuidedR2NeverPersistsTheBootstrapToken(t *testing.T) {
	t.Parallel()
	t.Run("finished setup", func(t *testing.T) {
		t.Parallel()
		g := newGuidedR2Fixture(t)
		out := g.run(t, g.happy(), 0)
		issued := g.cf.Tokens()[0]
		g.assertNothingHolds(t, out, bootstrapCanary, issued.Value, "Bearer")
	})
	t.Run("stopped with a saved draft", func(t *testing.T) {
		t.Parallel()
		g := newGuidedR2Fixture(t)
		// A check that keeps failing leaves the setup draft on disk.
		g.env.OpenStore = failingOpener(invalidKey)
		input := guidedAnswers(append(append([]string{}, askToken...), "", "stop")...)
		out := g.run(t, input, 1)
		if _, err := os.Stat(draftPath(g.home)); err != nil {
			t.Fatalf("no setup draft was saved, so the search would prove nothing about it: %v", err)
		}
		g.assertNothingHolds(t, out, bootstrapCanary, g.cf.Tokens()[0].Value)
	})
	t.Run("token from the environment", func(t *testing.T) {
		t.Parallel()
		g := newGuidedR2Fixture(t)
		g.setEnv(map[string]string{"CLOUDFLARE_API_TOKEN": bootstrapCanary})
		out := g.run(t, guidedAnswers(acceptedRest...), 0)
		g.assertNothingHolds(t, out, bootstrapCanary, g.cf.Tokens()[0].Value)
		if !setupContainsText(out, "Using the API token in CLOUDFLARE_API_TOKEN.") {
			t.Fatalf("no note about the environment token:\n%s", out)
		}
		if !setupContainsText(out, "Setup did not save the Cloudflare API token from CLOUDFLARE_API_TOKEN") || setupContainsText(out, "you pasted") || setupContainsText(out, "delete it in the dashboard") {
			t.Fatalf("wrong wording for a token that came from the environment:\n%s", out)
		}
	})
}

func invalidKey() error {
	return &smithy.OperationError{ServiceID: "S3", OperationName: "ListObjectsV2", Err: &smithy.GenericAPIError{Code: "InvalidAccessKeyId", Message: "The access key does not exist."}}
}

// failingOpener opens a store whose first listing fails as fail says, for as
// long as fail returns an error.
func failingOpener(fail func() error) func(config.Config) (storage.ObjectStore, error) {
	return func(config.Config) (storage.ObjectStore, error) {
		return &guidedStore{fail: fail}, nil
	}
}

// guidedStore is a bucket whose listing can be made to fail. Nothing else
// works on it: a test that gets past the listing has failed.
type guidedStore struct {
	storage.ObjectStore
	fail func() error
}

func (s *guidedStore) ListPage(context.Context, string, string, int32) (storage.ObjectPage, error) {
	return storage.ObjectPage{}, s.fail()
}

func TestGuidedR2TokenAndAccountFromTheEnvironment(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	g.setEnv(map[string]string{"CLOUDFLARE_API_TOKEN": "  " + bootstrapCanary + "\n", "CLOUDFLARE_ACCOUNT_ID": strings.ToUpper(cloudflaretest.AccountID)})
	out := g.run(t, guidedAnswers(acceptedRest...), 0)
	if g.cf.Calls(cloudflaretest.RouteAccounts) != 0 || !setupContainsText(out, "Using the account ID in CLOUDFLARE_ACCOUNT_ID.") {
		t.Fatalf("accounts listed anyway:\n%s", out)
	}
	if setupContainsText(out, "Cloudflare API token (hidden") {
		t.Fatalf("asked for a token that the environment holds:\n%s", out)
	}
	if cfg := g.savedConfig(t); cfg.Storage.R2AccountID != cloudflaretest.AccountID {
		t.Fatalf("account %q", cfg.Storage.R2AccountID)
	}
}

// The token read from CLOUDFLARE_API_TOKEN is removed from setup's own
// environment (not CLOUDFLARE_ACCOUNT_ID, which is no secret), so what the
// message says is true.
func TestGuidedR2RemovesTheTokenVariableFromSetupsEnvironment(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	g.setEnv(map[string]string{"CLOUDFLARE_API_TOKEN": bootstrapCanary, "CLOUDFLARE_ACCOUNT_ID": cloudflaretest.AccountID})
	var unset []string
	g.env.UnsetEnv = func(key string) error { unset = append(unset, key); return nil }
	out := g.run(t, guidedAnswers(acceptedRest...), 0)
	if !slices.Equal(unset, []string{"CLOUDFLARE_API_TOKEN"}) || !setupContainsText(out, "it also removed the variable from setup's own environment") {
		t.Fatalf("unset %v\n%s", unset, out)
	}
	// A token typed in is not in the environment, so nothing is removed.
	typed := newGuidedR2Fixture(t)
	typed.env.UnsetEnv = func(key string) error { t.Errorf("removed %s", key); return nil }
	if out := typed.run(t, typed.happy(), 0); setupContainsText(out, "removed the variable") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestGuidedR2RefusesWhenTheTokenVariableCannotBeRemoved(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	g.setEnv(map[string]string{"CLOUDFLARE_API_TOKEN": bootstrapCanary})
	g.env.UnsetEnv = func(string) error { return errors.New("denied " + bootstrapCanary) }
	out := g.run(t, guidedAnswers(acceptedRest...), 1)
	if !setupContainsText(out, "could not remove CLOUDFLARE_API_TOKEN") || setupContainsText(out, bootstrapCanary) || len(g.cf.Requests()) != 0 {
		t.Fatalf("management operation after failed token removal: %s", out)
	}
}

// By default the variable really leaves the process environment, and with it
// the environment a program setup starts would inherit. This test changes the
// real environment, so it does not run in parallel.
func TestGuidedR2TokenVariableLeavesTheProcessEnvironment(t *testing.T) {
	t.Setenv("CLOUDFLARE_API_TOKEN", bootstrapCanary)
	var out bytes.Buffer
	p := newPrompter(strings.NewReader(""), &out)
	token, fromEnv, removed, err := askBootstrapToken(p, Env{})
	if err != nil || token != bootstrapCanary || !fromEnv || !removed {
		t.Fatalf("token %q fromEnv %v removed %v err %v", token, fromEnv, removed, err)
	}
	if _, set := os.LookupEnv("CLOUDFLARE_API_TOKEN"); set {
		t.Fatal("the variable is still set")
	}
	// What a program started now inherits is os.Environ.
	for _, kv := range os.Environ() {
		if setupContainsText(kv, bootstrapCanary) || strings.HasPrefix(kv, "CLOUDFLARE_API_TOKEN=") {
			t.Fatalf("a child would inherit %q", kv)
		}
	}
}

func TestGuidedR2IgnoresAMalformedAccountIDInTheEnvironment(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	g.setEnv(map[string]string{"CLOUDFLARE_ACCOUNT_ID": "not-an-account"})
	out := g.run(t, g.happy(), 0)
	if !setupContainsText(out, "CLOUDFLARE_ACCOUNT_ID isn't a Cloudflare account ID") || g.cf.Calls(cloudflaretest.RouteAccounts) != 1 {
		t.Fatalf("output:\n%s", out)
	}
}

// A blank token goes back to the storage menu, and nothing is created.
func TestGuidedR2BlankTokenReturnsToTheMenu(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	out := g.run(t, guidedAnswers("", "s3-existing", "work", "2", ""), 0)
	if n := strings.Count(out, "Where should your archive live?"); n != 2 {
		t.Fatalf("menu shown %d times:\n%s", n, out)
	}
	if len(g.cf.Requests()) != 0 || len(g.apis) != 0 {
		t.Fatalf("Cloudflare was called: %v", routes(g.cf.Requests()))
	}
	if cfg := g.savedConfig(t); cfg.Storage.Provider != credentials.ProviderS3 {
		t.Fatalf("storage %+v", cfg.Storage)
	}
}

// A rejected token offers a way back to the storage menu.
func TestGuidedR2RejectedTokenReturnsToTheMenu(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	out := g.run(t, guidedAnswers("some-other-token", "other", "s3-existing", "work", "2", ""), 0)
	if !setupContainsText(out, "Cloudflare didn't accept the API token") || g.cf.Calls(cloudflaretest.RouteCreateBucket) != 0 {
		t.Fatalf("output:\n%s", out)
	}
	if !g.apis[0].discarded {
		t.Fatal("the rejected token was not discarded")
	}
	g.assertNothingHolds(t, out, "some-other-token")
}

func TestGuidedR2CanReplaceRejectedEnvironmentToken(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	const rejected = "CANARY-rejected-env-token"
	g.setEnv(map[string]string{"CLOUDFLARE_API_TOKEN": rejected})
	input := guidedAnswers(append([]string{"token", bootstrapCanary}, acceptedRest...)...)
	out := g.run(t, input, 0)
	if len(g.apis) != 2 || !g.apis[0].discarded || !g.apis[1].discarded {
		t.Fatalf("replacement did not discard both clients: %+v", g.apis)
	}
	checked := strings.Index(out, "Archive-key permission lookup succeeded")
	bucket := strings.Index(out, "Bucket:")
	if checked < 0 || bucket < checked || !setupContainsText(out, "Paste a different token") {
		t.Fatalf("replacement was not checked before bucket settings:\n%s", out)
	}
	if setupContainsText(out, "Setup did not save the Cloudflare API token from CLOUDFLARE_API_TOKEN") {
		t.Fatalf("replacement described as environment token:\n%s", out)
	}
	g.savedConfig(t)
	g.assertNothingHolds(t, out, rejected, bootstrapCanary)
}

func TestGuidedR2RejectsMalformedReplacementBeforeCreatingClient(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	g.cf.Fail(cloudflaretest.RoutePermissionGroups, cloudflaretest.Failure{Status: http.StatusForbidden, Times: 1})
	const malformed = "CANARY:invalid-bearer"
	out := g.run(t, guidedAnswers(bootstrapCanary, "token", malformed, "", "stop"), 1)
	if len(g.apis) != 1 || !setupContainsText(out, "Credential was not accepted. Try again.") {
		t.Fatalf("malformed replacement reached client creation: %d clients\n%s", len(g.apis), out)
	}
	if !g.apis[0].discarded || g.cf.Calls(cloudflaretest.RouteCreateBucket) != 0 || g.cf.Calls(cloudflaretest.RouteCreateToken) != 0 {
		t.Fatal("malformed replacement retained a client or created provider resources")
	}
	g.assertNothingHolds(t, out, malformed, bootstrapCanary)
}

func TestGuidedR2UsesAndPreservesConfiguredTokenCommand(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	command := []string{"synthetic-token-source", "read", "synthetic-reference"}
	must(t, config.Save(g.home, config.Config{CloudflareTokenCommand: command}))
	calls := 0
	g.env.RunTokenCommand = func(ctx context.Context, args, environment []string) (string, error) {
		calls++
		if !slices.Equal(args, command) {
			t.Fatal("guided setup did not use the committed source")
		}
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 20*time.Second {
			t.Fatal("token source has no bounded deadline")
		}
		return bootstrapCanary, nil
	}
	out := g.run(t, guidedAnswers(acceptedRest...), 0)
	if calls != 1 || !slices.Equal(g.savedConfig(t).CloudflareTokenCommand, command) {
		t.Fatal("guided setup lost or failed to consume the committed source")
	}
	g.assertNothingHolds(t, out, bootstrapCanary)
}

func TestGuidedR2AsksForTheAccountWhenItCannotBeFound(t *testing.T) {
	t.Parallel()
	t.Run("listing refused", func(t *testing.T) {
		t.Parallel()
		g := newGuidedR2Fixture(t)
		g.cf.Fail(cloudflaretest.RouteAccounts, cloudflaretest.Failure{Status: http.StatusForbidden, Message: "not allowed"})
		input := guidedAnswers(append(append([]string{}, askToken...), append([]string{"short", cloudflaretest.AccountID}, acceptedRest...)...)...)
		out := g.run(t, input, 0)
		for _, want := range []string{"Couldn't list your Cloudflare accounts", "isn't allowed to list accounts", "that isn't a Cloudflare account ID", "Cloudflare said: not allowed"} {
			if !setupContainsText(out, want) {
				t.Errorf("output lacks %q:\n%s", want, out)
			}
		}
		if g.savedConfig(t).Storage.R2AccountID != cloudflaretest.AccountID {
			t.Fatal("wrong account")
		}
	})
	t.Run("several accounts", func(t *testing.T) {
		t.Parallel()
		g := newGuidedR2Fixture(t)
		other := "fedcba9876543210fedcba9876543210"
		g.cf.Accounts = append(g.cf.Accounts, cloudflaretest.Account{ID: other, Name: "Other account"})
		input := guidedAnswers(append(append([]string{}, askToken...), append([]string{other}, acceptedRest...)...)...)
		out := g.run(t, input, 0)
		if !setupContainsText(out, "more than one Cloudflare account") || !setupContainsText(out, "Other account") {
			t.Fatalf("accounts not listed:\n%s", out)
		}
		if g.savedConfig(t).Storage.R2AccountID != other {
			t.Fatal("wrong account")
		}
	})
	t.Run("no accounts", func(t *testing.T) {
		t.Parallel()
		g := newGuidedR2Fixture(t)
		g.cf.Accounts = nil
		input := guidedAnswers(append(append([]string{}, askToken...), "0123456789abcdef0123456789abcdef", "", "stop")...)
		if out := g.run(t, input, 1); !setupContainsText(out, "Cloudflare doesn't know that account") {
			t.Fatalf("output:\n%s", out)
		}
	})
}

func TestGuidedR2ValidatesTheBucketName(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	input := guidedAnswers(append(append([]string{}, askToken...), "customize", "Bad_Name", "ab", "-lead", "My-Archive-Bkt", "n", "", "")...)
	out := g.run(t, input, 0)
	if strings.Count(out, "a bucket name") < 3 {
		t.Fatalf("names not refused:\n%s", out)
	}
	if got := g.savedConfig(t).Storage.Bucket; got != "my-archive-bkt" {
		t.Fatalf("bucket %q", got)
	}
	if _, ok := g.cf.Buckets["my-archive-bkt"]; !ok || len(g.cf.Buckets) != 1 {
		t.Fatalf("buckets %v", g.cf.Buckets)
	}
}

// A collision on the name setup generated is retried once with a new one.
func TestGuidedR2ReplacesACollidingDefaultName(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	g.cf.Fail(cloudflaretest.RouteCreateBucket, cloudflaretest.Failure{Status: http.StatusConflict, Code: 10004, Message: "Bucket name already exists.", Times: 1})
	out := g.run(t, g.happy()+"\n", 0)
	if !setupContainsText(out, "is taken; preparing") || g.cf.Calls(cloudflaretest.RouteCreateBucket) != 2 {
		t.Fatalf("output:\n%s", out)
	}
	var names []string
	for _, r := range g.cf.Requests() {
		if r.Route == cloudflaretest.RouteCreateBucket {
			var body struct {
				Name string `json:"name"`
			}
			must(t, json.Unmarshal([]byte(r.Body), &body))
			names = append(names, body.Name)
		}
	}
	if names[0] == names[1] || !defaultBucketName.MatchString(names[1]) || g.savedConfig(t).Storage.Bucket != names[1] || strings.Count(out, "Your archive storage") != 2 || !setupContainsText(out, "Bucket: "+names[1]) {
		t.Fatalf("names %v", names)
	}
}

// A collision on a name the person chose is asked about, never replaced.
func TestGuidedR2AsksAgainWhenAChosenNameIsTaken(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	g.cf.Buckets["mine-bucket"] = ""
	input := guidedAnswers(append(append([]string{}, askToken...), "customize", "mine-bucket", "n", "", "mine-two", "", "")...)
	out := g.run(t, input, 0)
	if !setupContainsText(out, "The name mine-bucket is taken in your Cloudflare account.") || g.cf.Calls(cloudflaretest.RouteCreateBucket) != 2 {
		t.Fatalf("output:\n%s", out)
	}
	if got := g.savedConfig(t).Storage.Bucket; got != "mine-two" {
		t.Fatalf("bucket %q", got)
	}
}

// A jurisdiction reaches the header, the token's resource string, and the
// endpoint setup saves.
func TestGuidedR2JurisdictionAndLocationHint(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	input := guidedAnswers(append(append([]string{}, askToken...), "customize", "eu-archive", "y", "mars", "EU", "nowhere", "weur", "", "")...)
	out := g.run(t, input, 0)
	if !setupContainsText(out, "can't be changed later") {
		t.Fatalf("no warning about permanence:\n%s", out)
	}
	for _, r := range g.cf.Requests() {
		// Account-level calls carry no jurisdiction.
		accountLevel := slices.Contains([]cloudflaretest.Route{cloudflaretest.RouteAccounts, cloudflaretest.RoutePermissionGroups, cloudflaretest.RouteCreateToken, cloudflaretest.RouteDeleteToken}, r.Route)
		if !accountLevel && r.Jurisdiction != "eu" {
			t.Errorf("%s sent jurisdiction %q", r.Route, r.Jurisdiction)
		}
	}
	created := ""
	for _, r := range g.cf.Requests() {
		if r.Route == cloudflaretest.RouteCreateBucket {
			created = r.Body
		}
	}
	if !setupContainsText(created, `"locationHint":"weur"`) {
		t.Fatalf("create body %s", created)
	}
	body, _ := json.Marshal(g.cf.Tokens()[0].Body)
	if !setupContainsText(string(body), "com.cloudflare.edge.r2.bucket."+cloudflaretest.AccountID+"_eu_eu-archive") {
		t.Fatalf("token request %s", body)
	}
	cfg := g.savedConfig(t)
	if cfg.Storage.R2Endpoint != "https://"+cloudflaretest.AccountID+".eu.r2.cloudflarestorage.com" || cfg.Storage.R2AccountID != "" {
		t.Fatalf("storage %+v", cfg.Storage)
	}
	// The saved settings read back as a pasted jurisdiction URL does.
	loc, err := credentials.ParseR2Location(cfg.Storage.R2Endpoint)
	if err != nil || loc.Endpoint != cfg.Storage.R2Endpoint {
		t.Fatalf("location %+v (%v)", loc, err)
	}
}

// Declining the final question creates nothing.
func TestGuidedR2DecliningTheGoAheadCreatesNothing(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	out := g.run(t, guidedAnswers(append(append([]string{}, askToken...), "back", "s3-existing", "work", "2", "")...), 0)
	if g.cf.Calls(cloudflaretest.RouteCreateBucket) != 0 || len(g.cf.Tokens()) != 0 {
		t.Fatalf("something was created:\n%s", out)
	}
}

// publicRest is the answers after the token when the bucket reads as public:
// the bucket name (default), no data location, go ahead, then what to do about
// public access, and start archiving.
func publicRest(whatNow ...string) []string {
	return append(append([]string{""}, whatNow...), "")
}

// A bucket that reads as public stops setup at a menu whose default is another
// storage option. Continuing anyway stores the key as before.
func TestGuidedR2ContinuesAnywayWhenToldTo(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	g.cf.ManagedEnabled = true
	g.cf.CustomDomains = []string{"files.example.com"}
	out := g.run(t, guidedAnswers(append(append([]string{}, askToken...), publicRest("continue")...)...), 0)
	for _, want := range []string{"public r2.dev URL is ON", "anyone with the link can read", "files.example.com", "What now?", "Continue anyway (the bucket is publicly readable)", "Connected to your storage."} {
		if !setupContainsText(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if setupContainsText(out, "off (checked at setup)") || setupContainsText(out, "Custom domains: none") {
		t.Fatalf("claims access is off:\n%s", out)
	}
	if len(g.cf.Live()) != 3 || g.savedConfig(t).Storage.Bucket == "" {
		t.Fatalf("live %d\n%s", len(g.cf.Live()), out)
	}
	if report := g.savedConfig(t).BucketPrivacy; report == nil || report.State != "public_or_risky" || report.Reason != "r2_public_access_enabled" || !setupContainsText(out, "! Bucket is public") {
		t.Fatalf("public privacy evidence %+v; output:\n%s", report, out)
	}
}

// Enter at that menu, or choosing another storage option, revokes the key's
// token (made and checked but never stored), reports the empty bucket, drops
// the bootstrap token, and returns to the storage question. An input that ends
// there does the same revoke before it ends setup.
func TestGuidedR2PublicBucketAnotherStorageRevokesTheKey(t *testing.T) {
	t.Parallel()
	for name, answer := range map[string][]string{"default": {""}, "chosen": {"other"}, "number": {"2"}, "input ends": nil} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			g := newGuidedR2Fixture(t)
			g.cf.ManagedEnabled = true
			rest := append([]string{""}, answer...)
			// After "another storage option" the storage question is asked
			// again and the input ends there.
			want := 1
			out := g.run(t, guidedAnswers(append(append([]string{}, askToken...), rest...)...), want)
			if len(g.cf.Tokens()) != 1 || len(g.cf.Live()) != 0 || g.cf.Calls(cloudflaretest.RouteDeleteToken) != 1 {
				t.Fatalf("tokens %d, live %d\n%s", len(g.cf.Tokens()), len(g.cf.Live()), out)
			}
			for _, want := range []string{"What now?", "Revoked the key that wasn't used.", "was created and is empty"} {
				if !setupContainsText(out, want) {
					t.Errorf("output lacks %q:\n%s", want, out)
				}
			}
			if name != "input ends" && strings.Count(out, "Where should your archive live?") != 2 {
				t.Errorf("the storage question was not asked again:\n%s", out)
			}
			if setupContainsText(out, "not saved anywhere") || setupContainsText(out, "Connected to your storage.") {
				t.Errorf("went on with a public bucket:\n%s", out)
			}
			if len(g.apis) != 1 || !g.apis[0].discarded {
				t.Error("the bootstrap token was not discarded")
			}
			g.notSaved(t)
			g.assertNothingHolds(t, out, bootstrapCanary)
		})
	}
}

// flipReader delivers one answer per Read and, just before it delivers
// before, runs flip: someone changes the bucket in the dashboard while setup
// waits.
type flipReader struct {
	lines  []string
	before string
	flip   func()
}

func (r *flipReader) Read(p []byte) (int, error) {
	if len(r.lines) == 0 {
		return 0, io.EOF
	}
	if r.lines[0] == r.before && r.flip != nil {
		r.flip()
		r.flip = nil
	}
	n := copy(p, r.lines[0]+"\n")
	r.lines = r.lines[1:]
	return n, nil
}

// "Check again" reads both domain endpoints again: once the r2.dev URL is
// turned off in the dashboard, setup goes on without asking again.
func TestGuidedR2CheckAgainSeesTheDashboardChange(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	g.cf.ManagedEnabled = true
	answers := strings.Split(strings.TrimSuffix("no\n"+guidedAnswers(append(append([]string{}, askToken...), publicRest("again")...)...), "\n"), "\n")
	g.env.IsTerminal = func(any) bool { return true }
	in := &flipReader{lines: answers, before: "again", flip: func() { g.cf.SetPublicAccess(false, nil) }}
	var out bytes.Buffer
	if code := Run([]string{"setup"}, in, &out, &out, g.env); code != 0 {
		t.Fatalf("setup exit %d\n%s", code, &out)
	}
	text := out.String()
	if strings.Count(text, "What now?") != 1 || !setupContainsText(text, "public r2.dev URL is ON") || !setupContainsText(text, "r2.dev public access: off (checked at setup).") {
		t.Fatalf("output:\n%s", text)
	}
	if g.cf.Calls(cloudflaretest.RouteManagedDomain) != 2 || g.cf.Calls(cloudflaretest.RouteCustomDomains) != 2 {
		t.Fatalf("reads: managed %d, custom %d", g.cf.Calls(cloudflaretest.RouteManagedDomain), g.cf.Calls(cloudflaretest.RouteCustomDomains))
	}
	if report := g.savedConfig(t).BucketPrivacy; report == nil || report.State != "verified_private" || report.Reason != "r2_public_domains_disabled" || !setupContainsText(text, "✓ Bucket is private") {
		t.Fatalf("rechecked privacy evidence %+v; output:\n%s", report, text)
	}
}

// A read that failed does not hide the other's positive: the stop still
// happens, and the failed read is still only a warning.
func TestGuidedR2AFailedReadDoesNotHideAPublicAnswer(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	g.cf.ManagedEnabled = true
	g.cf.Fail(cloudflaretest.RouteCustomDomains, cloudflaretest.Failure{Status: http.StatusForbidden})
	out := g.run(t, guidedAnswers(append(append([]string{}, askToken...), publicRest("continue")...)...), 0)
	for _, want := range []string{"public r2.dev URL is ON", "What now?", "Couldn't check whether the bucket has custom domains"} {
		if !setupContainsText(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	// Custom domains alone, with the r2.dev read refused, stop it too.
	h := newGuidedR2Fixture(t)
	h.cf.CustomDomains = []string{"files.example.com"}
	h.cf.Fail(cloudflaretest.RouteManagedDomain, cloudflaretest.Failure{Status: http.StatusForbidden})
	out = h.run(t, guidedAnswers(append(append([]string{}, askToken...), publicRest("continue")...)...), 0)
	if !setupContainsText(out, "What now?") || !setupContainsText(out, "files.example.com") {
		t.Fatalf("output:\n%s", out)
	}
}

// When the reads are refused, setup says it did not check, and claims
// nothing.
func TestGuidedR2PublicAccessReadsRefused(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	g.cf.Fail(cloudflaretest.RouteManagedDomain, cloudflaretest.Failure{Status: http.StatusForbidden})
	g.cf.Fail(cloudflaretest.RouteCustomDomains, cloudflaretest.Failure{Status: http.StatusForbidden})
	out := g.run(t, g.happy(), 0)
	for _, want := range []string{"Couldn't check whether the bucket's public r2.dev URL is on", "Couldn't check whether the bucket has custom domains"} {
		if !setupContainsText(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if setupContainsText(out, "(checked at setup)") {
		t.Fatalf("claims a check that did not happen:\n%s", out)
	}
	if report := g.savedConfig(t).BucketPrivacy; report == nil || report.State != "not_verified" || report.Reason != "r2_public_access_not_fully_checked" || !setupContainsText(out, "Bucket privacy unknown") {
		t.Fatalf("unreadable privacy evidence %+v; output:\n%s", report, out)
	}
}

// The failure menu after each step that can fail: what to fix, the bucket kept
// and reported, and no key made.
func TestGuidedR2StepFailures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		route   cloudflaretest.Route
		failure cloudflaretest.Failure
		want    []string
		bucket  bool
		mutate  func(*cloudflaretest.Server)
	}{
		{"bucket forbidden", cloudflaretest.RouteCreateBucket, cloudflaretest.Failure{Status: http.StatusForbidden, Message: "no scope"},
			[]string{"Couldn't create the bucket", "needs the " + cloudflare.PermissionR2Write + " permission", "Cloudflare said: no scope"}, false, nil},
		{"bucket rate limited", cloudflaretest.RouteCreateBucket, cloudflaretest.Failure{Status: http.StatusTooManyRequests, RetryAfter: "600"},
			[]string{"limiting this token's requests; it asked for a wait of 600 seconds"}, false, nil},
		{"bucket server error", cloudflaretest.RouteCreateBucket, cloudflaretest.Failure{Status: http.StatusInternalServerError},
			[]string{"Cloudflare had a problem (HTTP 500)"}, false, nil},
		{"bucket account not found", cloudflaretest.RouteCreateBucket, cloudflaretest.Failure{Status: http.StatusNotFound},
			[]string{"Cloudflare doesn't know that account or bucket"}, false, nil},
		{"permission group lookup forbidden", cloudflaretest.RoutePermissionGroups, cloudflaretest.Failure{Status: http.StatusForbidden},
			[]string{"Couldn't look up the permission", cloudflare.PermissionTokensWrite, "subset of their own permissions", "No bucket or key has been created", "Account > Account API Tokens > Edit"}, false, nil},
		{"permission group missing", "", cloudflaretest.Failure{},
			[]string{"Couldn't find the permission for the bucket's key", "is listed", "No bucket or key has been created"}, false, func(s *cloudflaretest.Server) { s.Groups = nil }},
		{"permission group not selectable", "", cloudflaretest.Failure{},
			[]string{"may not grant it", "No bucket or key has been created"}, false, func(s *cloudflaretest.Server) {
				s.Groups = []cloudflaretest.Group{{ID: "aaaa0000000000000000000000000002", Name: cloudflare.PermissionBucketItemWrite}}
			}},
		{"token forbidden", cloudflaretest.RouteCreateToken, cloudflaretest.Failure{Status: http.StatusForbidden, Message: "Unauthorized to access requested resource"},
			[]string{"Couldn't create the key", cloudflare.PermissionTokensWrite, "subset of their own permissions", "administrator"}, true, nil},
		{"token conflict", cloudflaretest.RouteCreateToken, cloudflaretest.Failure{Status: http.StatusConflict},
			[]string{"Couldn't create the key", "refused the request (HTTP 409)"}, true, nil},
		{"token server error", cloudflaretest.RouteCreateToken, cloudflaretest.Failure{Status: http.StatusBadGateway},
			[]string{"Cloudflare had a problem (HTTP 502)", "If Cloudflare did create it, revoke the token named"}, true, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := newGuidedR2Fixture(t)
			if tc.route != "" {
				g.cf.Fail(tc.route, tc.failure)
			}
			if tc.mutate != nil {
				tc.mutate(g.cf)
			}
			input := guidedAnswers(append(append([]string{}, askToken...), "", "stop")...)
			if strings.HasPrefix(tc.name, "permission group") {
				input = guidedAnswers(append(append([]string{}, askToken...), "stop")...)
			}
			out := g.run(t, input, 1)
			for _, want := range tc.want {
				if !setupContainsText(out, want) {
					t.Errorf("output lacks %q:\n%s", want, out)
				}
			}
			if left := setupContainsText(out, "was created and is empty"); left != tc.bucket {
				t.Errorf("bucket reported left behind = %v, want %v:\n%s", left, tc.bucket, out)
			}
			if strings.HasPrefix(tc.name, "permission group") && (g.cf.Calls(cloudflaretest.RouteCreateBucket) != 0 || g.cf.Calls(cloudflaretest.RouteCreateToken) != 0 || setupContainsText(out, "Your archive storage") || setupContainsText(out, "Bucket:")) {
				t.Errorf("permission failure reached creation or confirmation:\n%s", out)
			}
			if len(g.cf.Live()) != 0 || len(g.keychain.items) != 0 {
				t.Errorf("a key was made: %d tokens, %d stored", len(g.cf.Live()), len(g.keychain.items))
			}
			g.notSaved(t)
			for _, api := range g.apis {
				if !api.discarded {
					t.Error("the bootstrap token was not discarded")
				}
			}
			g.assertNothingHolds(t, out, bootstrapCanary)
		})
	}
}

// The token's name is printed before it is created, so a crash leaves a
// recognizable orphan: here creation fails, and the name printed is the name
// that was sent.
func TestGuidedR2PrintsTheTokenNameBeforeCreatingIt(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	g.cf.Fail(cloudflaretest.RouteCreateToken, cloudflaretest.Failure{Status: http.StatusForbidden})
	out := g.run(t, guidedAnswers(append(append([]string{}, askToken...), "", "stop")...), 1)
	var sent struct {
		Name string `json:"name"`
	}
	for _, r := range g.cf.Requests() {
		if r.Route == cloudflaretest.RouteCreateToken {
			must(t, json.Unmarshal([]byte(r.Body), &sent))
		}
	}
	if sent.Name == "" || !setupContainsText(out, `an API token named "`+sent.Name+`"`) {
		t.Fatalf("name sent %q; output:\n%s", sent.Name, out)
	}
	if strings.Index(out, "revoke that token in the dashboard") > strings.Index(out, "Couldn't create the key") {
		t.Fatalf("the recovery text came after the failure:\n%s", out)
	}
}

func TestGuidedR2RetriesPermissionLookupBeforeCreatingAnything(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	g.cf.Fail(cloudflaretest.RoutePermissionGroups, cloudflaretest.Failure{Status: http.StatusForbidden, Times: 1})
	out := g.run(t, guidedAnswers(append(append([]string{}, askToken...), "retry", "", "")...), 0)
	want := []string{
		string(cloudflaretest.RouteAccounts), string(cloudflaretest.RoutePermissionGroups), string(cloudflaretest.RoutePermissionGroups),
		string(cloudflaretest.RouteCreateBucket), string(cloudflaretest.RouteCreateToken),
		string(cloudflaretest.RouteManagedDomain), string(cloudflaretest.RouteCustomDomains),
		string(cloudflaretest.RouteCreateToken), string(cloudflaretest.RouteCreateToken),
	}
	if got := routes(g.cf.Requests()); !slices.Equal(got, want) {
		t.Fatalf("calls %v, want %v", got, want)
	}
	failed := strings.Index(out, "No bucket or key has been created.")
	confirm := strings.Index(out, "Your archive storage")
	if failed < 0 || confirm < failed || strings.Count(out, "Your archive storage") != 1 || setupContainsText(out, "Try again with the same bucket") {
		t.Fatalf("lookup retry did not precede creation confirmation:\n%s", out)
	}
	cfg := g.savedConfig(t)
	if len(cfg.SpareCredentialRefs) != 2 {
		t.Fatal("guided retry did not retain two spare references")
	}
}

// A retry after a failed token step reuses the bucket, and the permission
// group looked up once.
func TestGuidedR2RetryReusesTheBucket(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	g.cf.Fail(cloudflaretest.RouteCreateToken, cloudflaretest.Failure{Status: http.StatusForbidden, Times: 1})
	out := g.run(t, guidedAnswers(append(append([]string{}, askToken...), "", "retry", "")...), 0)
	if g.cf.Calls(cloudflaretest.RouteCreateBucket) != 1 || g.cf.Calls(cloudflaretest.RoutePermissionGroups) != 1 || g.cf.Calls(cloudflaretest.RouteCreateToken) != 4 {
		t.Fatalf("calls %v", routes(g.cf.Requests()))
	}
	if !setupContainsText(out, "Try again with the same bucket") || len(g.cf.Live()) != 3 {
		t.Fatalf("output:\n%s", out)
	}
	g.savedConfig(t)
}

// After a failed bucket step, a retry creates it.
func TestGuidedR2RetryAfterAFailedBucketStep(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	g.cf.Fail(cloudflaretest.RouteCreateBucket, cloudflaretest.Failure{Status: http.StatusInternalServerError, Times: 1})
	out := g.run(t, guidedAnswers(append(append([]string{}, askToken...), "", "retry", "")...), 0)
	if setupContainsText(out, "with the same bucket") || g.cf.Calls(cloudflaretest.RouteCreateBucket) != 2 {
		t.Fatalf("output:\n%s", out)
	}
	g.savedConfig(t)
}

// Choosing another option after a failure ends guided creation, reports the
// bucket left behind, and setup carries on with the other option.
func TestGuidedR2FailureThenAnotherOption(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	g.cf.Fail(cloudflaretest.RouteCreateToken, cloudflaretest.Failure{Status: http.StatusForbidden})
	out := g.run(t, guidedAnswers(append(append([]string{}, askToken...), "", "other", "s3-existing", "work", "2", "")...), 0)
	if !setupContainsText(out, "was created and is empty. It stays in your Cloudflare account") {
		t.Fatalf("bucket not reported:\n%s", out)
	}
	if cfg := g.savedConfig(t); cfg.Storage.Provider != credentials.ProviderS3 {
		t.Fatalf("storage %+v", cfg.Storage)
	}
}

// A key that never passes the storage check is revoked, the check is
// repeated while Cloudflare may still be starting to honor the key, and
// nothing is stored.
func TestGuidedR2RevokesAKeyThatFailsVerification(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	var listings atomic.Int32
	g.env.OpenStore = failingOpener(func() error { listings.Add(1); return invalidKey() })
	out := g.run(t, guidedAnswers(append(append([]string{}, askToken...), "", "stop")...), 1)
	if n := listings.Load(); n != 5 || g.pauses != 4 || r2VerifyAttempts != 5 {
		t.Fatalf("%d checks and %d pauses, want 5 and 4", n, g.pauses)
	}
	for _, want := range []string{"The new key didn't pass the storage check.", "Can't sign in to Cloudflare R2.", "Revoked the key that wasn't used.", "Waiting for the key to activate (up to ~15s)"} {
		if !setupContainsText(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if len(g.cf.Tokens()) != 1 || len(g.cf.Live()) != 0 || g.cf.Calls(cloudflaretest.RouteDeleteToken) != 1 {
		t.Fatalf("tokens %d, live %d", len(g.cf.Tokens()), len(g.cf.Live()))
	}
	if len(g.keychain.items) != 0 {
		t.Fatalf("a failed key was stored: %v", g.keychain.items)
	}
	// Setup ends without the public-access reads: the bucket is unused.
	if g.cf.Calls(cloudflaretest.RouteManagedDomain) != 0 {
		t.Fatalf("calls %v", routes(g.cf.Requests()))
	}
	g.notSaved(t)
}

// Trying again after a failed check makes a fresh key for the same bucket.
func TestGuidedR2RetryAfterFailedVerification(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	var opened atomic.Int32
	g.env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		if int(opened.Add(1)) <= r2VerifyAttempts {
			return &guidedStore{fail: invalidKey}, nil
		}
		return g.bucket, nil
	}
	out := g.run(t, guidedAnswers(append(append([]string{}, askToken...), "", "retry", "")...), 0)
	tokens := g.cf.Tokens()
	if len(tokens) != 4 || len(g.cf.Live()) != 3 || g.cf.Live()[0].ID != tokens[1].ID || g.cf.Calls(cloudflaretest.RouteCreateBucket) != 1 {
		t.Fatalf("tokens %+v, calls %v\n%s", tokens, routes(g.cf.Requests()), out)
	}
	if tokens[0].Name == tokens[1].Name {
		t.Fatalf("the retry reused a token name: %q", tokens[0].Name)
	}
	cfg := g.savedConfig(t)
	stored, err := g.keychain.Load(context.Background(), cfg.Storage.R2CredentialRef)
	if err != nil || stored.AccessKeyID != tokens[1].ID {
		t.Fatalf("stored %+v (%v), want the second token", stored, err)
	}
}

// A refused revoke names the token to remove by hand.
func TestGuidedR2FailedRevokeNamesTheToken(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	g.env.OpenStore = failingOpener(invalidKey)
	g.cf.Fail(cloudflaretest.RouteDeleteToken, cloudflaretest.Failure{Status: http.StatusForbidden})
	out := g.run(t, guidedAnswers(append(append([]string{}, askToken...), "", "stop")...), 1)
	name := g.cf.Tokens()[0].Name
	if !setupContainsText(out, "Couldn't revoke the key that wasn't used.") || !setupContainsText(out, `Revoke the API token named "`+name+`"`) {
		t.Fatalf("output:\n%s", out)
	}
}

// Only failures that can be Cloudflare catching up are waited out.
func TestGuidedR2DoesNotWaitOutOtherFailures(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	var listings atomic.Int32
	g.env.OpenStore = failingOpener(func() error { listings.Add(1); return errors.New("connection reset") })
	out := g.run(t, guidedAnswers(append(append([]string{}, askToken...), "", "stop")...), 1)
	if listings.Load() != 1 || g.pauses != 0 {
		t.Fatalf("%d checks, %d pauses", listings.Load(), g.pauses)
	}
	if !setupContainsText(out, "Revoked the key that wasn't used.") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestGuidedR2RateLimitIsWaitedOutThenSucceeds(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	g.cf.Fail(cloudflaretest.RouteCreateBucket, cloudflaretest.Failure{Status: http.StatusTooManyRequests, RetryAfter: "5", Times: 2})
	out := g.run(t, g.happy(), 0)
	if g.cf.Calls(cloudflaretest.RouteCreateBucket) != 3 || setupContainsText(out, "limiting this token") {
		t.Fatalf("output:\n%s", out)
	}
}

// Ending the flow any way but success still discards the token.
func TestGuidedR2DiscardsTheTokenWhenTheFlowEnds(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	g.run(t, g.happy(), 0)
	if len(g.apis) != 1 || !g.apis[0].discarded {
		t.Fatalf("clients %d, discarded %v", len(g.apis), g.apis[0].discarded)
	}
	if _, err := g.apis[0].Accounts(context.Background()); err == nil {
		t.Fatal("the discarded client still works")
	}
}

// The menu offers the guided choice, and its number works.
func TestGuidedR2IsInTheStorageMenu(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	input := strings.Join(append([]string{"", "", r2MenuNumber(t, guidedR2Choice)}, append(append([]string{}, askToken...), acceptedRest...)...), "\n") + "\n"
	out := g.run(t, input, 0)
	if !setupContainsText(out, "  1) Cloudflare R2") {
		t.Fatalf("menu:\n%s", out)
	}
	g.savedConfig(t)
}

// setup --yes never reaches Cloudflare: the guided path is interactive only.
func TestGuidedR2IsNotPartOfSetupYes(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	g.env.Cloudflare = func(string) cloudflare.API {
		t.Error("setup --yes reached Cloudflare")
		return nil
	}
	var out bytes.Buffer
	args := []string{"setup", "--yes", "--provider", "s3", "--bucket", "b", "--aws-profile", "p", "--region", "us-east-1", "--apps", "claude", "--project", filepath.Join(g.userHome, "src", "web-app")}
	if code := Run(args, strings.NewReader(""), &out, &out, g.env); code != 0 {
		t.Fatalf("setup --yes exit %d\n%s", code, &out)
	}
}

func TestExplainCloudflare(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want []string
	}{
		{"unauthorized", &cloudflare.Error{Status: 401}, []string{"didn't accept the API token"}},
		{"forbidden with step text", &cloudflare.Error{Status: 403}, []string{"STEP TEXT"}},
		{"not found", &cloudflare.Error{Status: 404}, []string{"doesn't know that account or bucket"}},
		{"bucket name conflict", &cloudflare.Error{Status: 409, Codes: []int{10073}}, []string{"already exists"}},
		{"other conflict", &cloudflare.Error{Status: 409, Messages: []string{"something else"}}, []string{"refused the request (HTTP 409)", "Cloudflare said: something else"}},
		{"rate limited without a wait", &cloudflare.Error{Status: 429}, []string{"limiting this token's requests. Try again in a few minutes."}},
		{"rate limited", &cloudflare.Error{Status: 429, RetryAfter: 90 * time.Second}, []string{"asked for a wait of 90 seconds"}},
		{"server", &cloudflare.Error{Status: 503}, []string{"HTTP 503"}},
		{"other", &cloudflare.Error{Status: 418}, []string{"refused the request (HTTP 418)"}},
		{"no response", &cloudflare.Error{Err: errors.New("dial tcp: refused")}, []string{"Couldn't reach Cloudflare", "dial tcp: refused"}},
		{"message", &cloudflare.Error{Status: 500, Messages: []string{"boom"}}, []string{"Cloudflare said: boom"}},
		{"long message", &cloudflare.Error{Status: 500, Messages: []string{strings.Repeat("x", 500)}}, []string{strings.Repeat("x", 200) + "…"}},
		{"another error", errors.New("plain"), []string{"couldn't complete the request: plain"}},
	}
	for _, tc := range cases {
		text := explainCloudflare(tc.err, "STEP TEXT")
		for _, want := range tc.want {
			if !setupContainsText(text, want) {
				t.Errorf("%s: %q lacks %q", tc.name, text, want)
			}
		}
		if setupContainsText(text, strings.Repeat("x", 201)) {
			t.Errorf("%s: message not cut", tc.name)
		}
	}
	// Cutting a long message never splits a character.
	wide := explainCloudflare(&cloudflare.Error{Status: 500, Messages: []string{strings.Repeat("é", 300)}}, "")
	if !utf8.ValidString(wide) || !setupContainsText(wide, strings.Repeat("é", 200)+"…") || setupContainsText(wide, strings.Repeat("é", 201)) {
		t.Errorf("wide message cut badly: %q", wide)
	}
	if text := explainCloudflare(&cloudflare.Error{Status: http.StatusOK, Err: errors.New("bad json")}, ""); !setupContainsText(text, "couldn't read the answer") {
		t.Errorf("unreadable answer: %q", text)
	}
	if text := explainCloudflare(&cloudflare.Error{Status: 403}, ""); !setupContainsText(text, "doesn't have the permission") {
		t.Errorf("empty 403 text: %q", text)
	}
}

func TestParseR2AccountID(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"0123456789abcdef0123456789abcdef", " 0123456789ABCDEF0123456789ABCDEF ", "https://0123456789abcdef0123456789abcdef.r2.cloudflarestorage.com"} {
		if got, err := parseR2AccountID(ok); err != nil || got != "0123456789abcdef0123456789abcdef" {
			t.Errorf("%q: %q, %v", ok, got, err)
		}
	}
	for _, bad := range []string{"", "abc", "0123456789abcdef0123456789abcdeg", "https://0123456789abcdef0123456789abcdef.r2.cloudflarestorage.com/bucket", "https://example.com"} {
		if got, err := parseR2AccountID(bad); err == nil {
			t.Errorf("%q accepted as %q", bad, got)
		}
	}
}

func menuKeys(options []option) string {
	var keys []string
	for _, o := range options {
		keys = append(keys, o.Key)
	}
	return strings.Join(keys, ",")
}

func TestStorageMenuHasTwoProviders(t *testing.T) {
	t.Parallel()
	for range 2 {
		if got := menuKeys(storageMenuOptions()); got != "r2,s3" {
			t.Fatalf("providers %s", got)
		}
	}
}

// A bucket and key setup made and did not commit are reported when setup ends
// however it ends: here the review is cancelled, which keeps the draft.
// A setup that commits them says nothing.
func TestGuidedR2ReportsWhatItLeftWhenSetupEndsWithoutUsingIt(t *testing.T) {
	t.Parallel()
	t.Run("review cancelled, draft uses them", func(t *testing.T) {
		t.Parallel()
		g := newGuidedR2Fixture(t)
		out := g.run(t, guidedAnswers(append(append([]string{}, askToken...), "", "no")...), 0)
		tokens := g.cf.Tokens()
		if len(tokens) != 3 || len(g.cf.Live()) != 3 {
			t.Fatalf("tokens %+v\n%s", tokens, out)
		}
		bucket := ""
		for name := range g.cf.Buckets {
			bucket = name
		}
		want := "Setup created the bucket " + bucket + ` (it is empty) and an API token named "` + tokens[0].Name + `" for it. Your saved setup draft uses them, so running setup again will resume with them.`
		if !setupContainsText(out, want) || strings.Count(out, "Setup created the bucket") != 1 {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
		g.notSaved(t)
		draft, found, problem, err := readDraft(g.home)
		if err != nil || !found || problem != "" || draft.Config.BucketPrivacy == nil || draft.Config.BucketPrivacy.Reason != "r2_public_domains_disabled" {
			t.Fatalf("saved guided privacy: found %v, problem %q, report %+v, err %v", found, problem, draft.Config.BucketPrivacy, err)
		}
		resumed := g.run(t, "continue\n\n", 0)
		if report := g.savedConfig(t).BucketPrivacy; report == nil || report.State != "verified_private" || !setupContainsText(resumed, "✓ Bucket is private") {
			t.Fatalf("guided privacy after resume %+v; output:\n%s", report, resumed)
		}
	})
	t.Run("committed", func(t *testing.T) {
		t.Parallel()
		g := newGuidedR2Fixture(t)
		out := g.run(t, g.happy(), 0)
		if setupContainsText(out, "Setup created the bucket") {
			t.Fatalf("reports a bucket that is in use:\n%s", out)
		}
		g.savedConfig(t)
	})
}

// Two guided runs in one setup (the first one's key staged, then "Edit a
// setting" > storage, and a second bucket made): committing the second names
// the first's bucket and token, and only those.
func TestGuidedR2ReportsEveryPairTheCommittedConfigDoesNotUse(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	input := guidedAnswers(append(append(append(append([]string{}, askToken...), "", "edit", "storage", "change", guidedR2Choice), askToken...), acceptedRest...)...)
	out := g.run(t, input, 0)
	tokens := g.cf.Tokens()
	if len(tokens) != 6 || len(g.cf.Buckets) != 2 {
		t.Fatalf("tokens %+v, buckets %v\n%s", tokens, g.cf.Buckets, out)
	}
	used := g.savedConfig(t).Storage.Bucket
	for bucket := range g.cf.Buckets {
		mentioned := setupContainsText(out, "Setup created the bucket "+bucket+" (it is empty)")
		if mentioned == (bucket == used) {
			t.Errorf("bucket %s (committed %s): mentioned %v\n%s", bucket, used, mentioned, out)
		}
		if bucket != used {
			var name string
			for _, token := range tokens {
				raw, _ := json.Marshal(token.Body)
				if setupContainsText(string(raw), "_default_"+bucket) {
					name = token.Name
					break
				}
			}
			if name == "" || !setupContainsText(out, `an API token named "`+name+`" for it. Neither is used`) {
				t.Errorf("the token of %s is not named (%q):\n%s", bucket, name, out)
			}
		}
	}
}

// A failure to reach Cloudflare is never reported as a problem with the token:
// no "didn't accept", no missing permission.
func TestGuidedR2NetworkFailuresAreNotBlamedOnTheToken(t *testing.T) {
	t.Parallel()
	down := &cloudflare.Error{Op: "any", Err: errors.New("dial tcp: connection refused")}
	refuse := func(t *testing.T, out string) {
		t.Helper()
		if strings.Count(out, "Couldn't reach Cloudflare") == 0 {
			t.Errorf("no network message:\n%s", out)
		}
		for _, bad := range []string{"didn't accept the API token", "isn't allowed", "The token needs", "doesn't have the permission"} {
			if setupContainsText(out, bad) {
				t.Errorf("blames the token (%q):\n%s", bad, out)
			}
		}
	}
	t.Run("listing accounts", func(t *testing.T) {
		t.Parallel()
		g := newGuidedR2Fixture(t)
		g.env.Cloudflare = func(token string) cloudflare.API {
			return &networkDownAPI{API: cloudflare.New(token, cloudflare.Options{BaseURL: g.cf.URL + "/client/v4"}), accounts: down}
		}
		input := guidedAnswers(append(append([]string{}, askToken...), append([]string{cloudflaretest.AccountID}, acceptedRest...)...)...)
		out := g.run(t, input, 0)
		refuse(t, out)
		if !setupContainsText(out, "setup needs the account ID") {
			t.Errorf("does not ask for the account ID:\n%s", out)
		}
	})
	t.Run("permission group", func(t *testing.T) {
		t.Parallel()
		g := newGuidedR2Fixture(t)
		g.env.Cloudflare = func(token string) cloudflare.API {
			return &networkDownAPI{API: cloudflare.New(token, cloudflare.Options{BaseURL: g.cf.URL + "/client/v4"}), groups: down}
		}
		out := g.run(t, guidedAnswers(append(append([]string{}, askToken...), "stop")...), 1)
		refuse(t, out)
	})
	t.Run("public access reads", func(t *testing.T) {
		t.Parallel()
		g := newGuidedR2Fixture(t)
		g.env.Cloudflare = func(token string) cloudflare.API {
			return &networkDownAPI{API: cloudflare.New(token, cloudflare.Options{BaseURL: g.cf.URL + "/client/v4"}), domains: down}
		}
		out := g.run(t, g.happy(), 0)
		refuse(t, out)
		if setupContainsText(out, "(checked at setup)") {
			t.Errorf("claims a check that did not happen:\n%s", out)
		}
	})
}

// networkDownAPI fails the calls it is given an error for.
type networkDownAPI struct {
	cloudflare.API
	accounts error
	groups   error
	domains  error
}

func (a *networkDownAPI) Accounts(ctx context.Context) ([]cloudflare.Account, error) {
	if a.accounts != nil {
		return nil, a.accounts
	}
	return a.API.Accounts(ctx)
}

func (a *networkDownAPI) PermissionGroups(ctx context.Context, account, name string) ([]cloudflare.PermissionGroup, error) {
	if a.groups != nil {
		return nil, a.groups
	}
	return a.API.PermissionGroups(ctx, account, name)
}

func (a *networkDownAPI) ManagedDomain(ctx context.Context, account string, bucket cloudflare.BucketRef) (cloudflare.ManagedDomain, error) {
	if a.domains != nil {
		return cloudflare.ManagedDomain{}, a.domains
	}
	return a.API.ManagedDomain(ctx, account, bucket)
}

func (a *networkDownAPI) CustomDomains(ctx context.Context, account string, bucket cloudflare.BucketRef) ([]cloudflare.CustomDomain, error) {
	if a.domains != nil {
		return nil, a.domains
	}
	return a.API.CustomDomains(ctx, account, bucket)
}

// A successful answer with nothing in it is not "off" or "none": setup says it
// could not read the answer and claims no check.
func TestGuidedR2EmptyPublicAccessAnswersAreNotReportedAsOff(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	g.cf.Fail(cloudflaretest.RouteManagedDomain, cloudflaretest.Failure{Status: http.StatusOK, RawBody: `{"success":true,"result":{}}`})
	g.cf.Fail(cloudflaretest.RouteCustomDomains, cloudflaretest.Failure{Status: http.StatusOK, RawBody: `{"success":true}`})
	out := g.run(t, g.happy(), 0)
	if strings.Count(out, "setup couldn't read the answer") != 2 || setupContainsText(out, "(checked at setup)") {
		t.Fatalf("output:\n%s", out)
	}
}

// Names Cloudflare sends are made printable: a terminal escape or a line
// break in an account name never reaches the screen.
func TestGuidedR2PrintsCloudflareNamesSafely(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	g.cf.Accounts = []cloudflaretest.Account{{ID: cloudflaretest.AccountID, Name: "Evil\x1b[2J\nname"}}
	g.cf.ManagedEnabled = true
	g.cf.CustomDomains = []string{"a.example.com\x1b[31m"}
	out := g.run(t, guidedAnswers(append(append([]string{}, askToken...), publicRest("continue")...)...), 0)
	if setupContainsText(out, "\x1b") || !setupContainsText(out, "Evil [2J name") || !setupContainsText(out, "a.example.com [31m") {
		t.Fatalf("output:\n%q", out)
	}
}

// r2MenuNumber is the number of the entry key in the menu setup shows when
// the experimental R2 switch is on, as a person types it.
func r2MenuNumber(t *testing.T, key string) string {
	t.Helper()
	return key
}

// The pasted token is dropped and its "not saved" line printed only once the
// key is staged, which is after the key passed its check and before the
// ordinary storage check.
func TestGuidedR2SaysTheTokenIsDroppedOnlyAfterStaging(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	out := g.run(t, g.happy(), 0)
	minted := strings.Index(out, "Created a key for")
	dropped := strings.Index(out, "The Cloudflare API token you pasted is not saved anywhere.")
	connected := strings.Index(out, "Connected to your storage.")
	if minted < 0 || dropped < minted || connected < dropped {
		t.Fatalf("order: key %d, dropped %d, connected %d\n%s", minted, dropped, connected, out)
	}
	flow := out[strings.Index(out, "Setup can create"):strings.Index(out, "Checking your storage connection")]
	if setupContainsText(flow, "private") {
		t.Fatalf("the word private is a claim guided creation can't back:\n%s", flow)
	}
}

// saveFailsKeychain refuses to store anything.
type saveFailsKeychain struct{ *fakeKeychain }

func (saveFailsKeychain) Save(context.Context, string, credentials.R2Credentials) error {
	return errors.New("the Keychain said no")
}

// A key that passed its check but could not be staged is useless, so its
// token is revoked, the person is told what is left, and the "not saved" line
// is not printed.
func TestGuidedR2RevokesTheTokenWhenStagingFails(t *testing.T) {
	t.Parallel()
	cases := map[string]func(g *guidedR2Fixture){
		"Keychain save fails": func(g *guidedR2Fixture) {
			g.env.Credentials = func() (credentials.CredentialStore, error) { return saveFailsKeychain{g.keychain}, nil }
		},
		"Keychain unavailable": func(g *guidedR2Fixture) {
			g.env.Credentials = func() (credentials.CredentialStore, error) { return nil, errors.New("no keychain here") }
		},
	}
	for name, arrange := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			g := newGuidedR2Fixture(t)
			arrange(g)
			out := g.run(t, g.happy(), 1)
			if len(g.cf.Tokens()) != 1 || len(g.cf.Live()) != 0 || g.cf.Calls(cloudflaretest.RouteDeleteToken) != 1 {
				t.Fatalf("tokens %d, live %d\n%s", len(g.cf.Tokens()), len(g.cf.Live()), out)
			}
			for _, want := range []string{"Setup couldn't store the new key", "Revoked the key that wasn't used.", "was created and is empty"} {
				if !setupContainsText(out, want) {
					t.Errorf("output lacks %q:\n%s", want, out)
				}
			}
			if setupContainsText(out, "not saved anywhere") || setupContainsText(out, "Setup did not save") {
				t.Errorf("says the token is dropped though staging failed:\n%s", out)
			}
			if !g.apis[0].discarded {
				t.Error("the bootstrap token was not discarded")
			}
			g.notSaved(t)
			g.assertNothingHolds(t, out, bootstrapCanary)
		})
	}
}

func TestGuidedR2FailedRollbackNamesTheToken(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	g.env.Credentials = func() (credentials.CredentialStore, error) { return saveFailsKeychain{g.keychain}, nil }
	g.cf.Fail(cloudflaretest.RouteDeleteToken, cloudflaretest.Failure{Status: http.StatusForbidden})
	out := g.run(t, g.happy(), 1)
	name := g.cf.Tokens()[0].Name
	if !setupContainsText(out, "Couldn't revoke the key that wasn't used.") || !setupContainsText(out, `Revoke the API token named "`+name+`"`) || len(g.cf.Live()) != 1 {
		t.Fatalf("output:\n%s", out)
	}
}

// When the ordinary storage check fails on the stored key, the recovery text
// says what guided creation left in Cloudflare, and how to clean it up.
func TestGuidedR2StorageCheckFailureNamesTheBucketAndToken(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	var opened atomic.Int32
	g.env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		// The flow's own check passes; the ordinary one, after staging, fails.
		if opened.Add(1) == 1 {
			return g.bucket, nil
		}
		return &guidedStore{fail: invalidKey}, nil
	}
	out := g.run(t, guidedAnswers(append(append([]string{}, askToken...), "", "cancel")...), 1)
	tokens := g.cf.Tokens()
	if len(tokens) != 2 || len(g.cf.Live()) != 2 {
		t.Fatalf("tokens %+v\n%s", tokens, out)
	}
	bucket := ""
	for name := range g.cf.Buckets {
		bucket = name
	}
	for _, want := range []string{
		"Setup created the bucket " + bucket + ` and an API token named "` + tokens[0].Name + `" for it; both are still in your Cloudflare account.`,
		"delete the bucket and revoke that token in the dashboard",
	} {
		if !setupContainsText(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "not saved anywhere") > strings.Index(out, "Setup created the bucket") {
		t.Errorf("the token was reported dropped after the failure text:\n%s", out)
	}
	// Said once: the note at the end of setup does not repeat it.
	if n := strings.Count(out, "Setup created the bucket"); n != 1 {
		t.Errorf("the leftovers were reported %d times:\n%s", n, out)
	}
	g.assertNothingHolds(t, out, bootstrapCanary)
}

// ctxStore is a bucket whose listing announces itself and then waits for the
// request to be canceled.
type ctxStore struct {
	storage.ObjectStore
	onList func()
}

func (s *ctxStore) ListPage(ctx context.Context, _, _ string, _ int32) (storage.ObjectPage, error) {
	s.onList()
	<-ctx.Done()
	return storage.ObjectPage{}, ctx.Err()
}

// An interrupt while the new key is being checked stops the flow, revokes the
// key's token, and exits with the signal's status.
func TestGuidedR2InterruptDuringTheCheckRevokesTheToken(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	caught := make(chan os.Signal, 1)
	g.env.Interrupts = func() (<-chan os.Signal, func()) { return caught, func() {} }
	g.env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		return &ctxStore{onList: func() { caught <- syscall.SIGINT }}, nil
	}
	var out bytes.Buffer
	code := Run([]string{"setup"}, strings.NewReader(guidedAnswers(append(append([]string{}, askToken...), "")...)), &out, &out, g.env)
	if code != 128+int(syscall.SIGINT) {
		t.Fatalf("exit %d\n%s", code, &out)
	}
	if len(g.cf.Tokens()) != 1 || len(g.cf.Live()) != 0 {
		t.Fatalf("tokens %d, live %d\n%s", len(g.cf.Tokens()), len(g.cf.Live()), &out)
	}
	text := out.String()
	for _, want := range []string{"Stopped.", `an API token named "` + g.cf.Tokens()[0].Name + `"`, "Revoked the key that wasn't used.", "was created and is empty"} {
		if !setupContainsText(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
	if setupContainsText(text, "What next?") {
		t.Errorf("asked what next after an interrupt:\n%s", text)
	}
	if !g.apis[0].discarded {
		t.Error("the bootstrap token was not discarded")
	}
	g.notSaved(t)
}

// An interrupt while the token is being created cannot tell whether it was:
// the name is printed with what to do, and the exit status is the signal's.
func TestGuidedR2InterruptDuringTokenCreationNamesTheToken(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	caught := make(chan os.Signal, 1)
	g.env.Interrupts = func() (<-chan os.Signal, func()) { return caught, func() {} }
	g.createToken = func(ctx context.Context, _ string, spec cloudflare.TokenSpec) (cloudflare.Token, error) {
		caught <- syscall.SIGINT
		<-ctx.Done()
		return cloudflare.Token{}, &cloudflare.Error{Op: "create API token", Err: ctx.Err()}
	}
	var out bytes.Buffer
	code := Run([]string{"setup"}, strings.NewReader(guidedAnswers(append(append([]string{}, askToken...), "")...)), &out, &out, g.env)
	if code != 128+int(syscall.SIGINT) {
		t.Fatalf("exit %d\n%s", code, &out)
	}
	text := out.String()
	if !setupContainsText(text, "If Cloudflare did create it, revoke the token named") || !setupContainsText(text, "Stopped.") {
		t.Fatalf("output:\n%s", text)
	}
	if g.cf.Calls(cloudflaretest.RouteDeleteToken) != 0 {
		t.Fatal("revoked a token that was never seen")
	}
}

// A bucket whose creation got no answer may exist: when the retry finds the
// name taken, setup says so and asks for another name, and never quietly
// picks one, or uses the bucket.
func TestGuidedR2LostBucketAnswerIsNotSilentlyReplaced(t *testing.T) {
	t.Parallel()
	for name, chosen := range map[string]string{"chosen name": "keep-name", "generated name": ""} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			g := newGuidedR2Fixture(t)
			g.cf.Fail(cloudflaretest.RouteCreateBucket, cloudflaretest.Failure{Status: http.StatusBadGateway, AfterWork: true, Times: 1})
			input := guidedAnswers(append(append([]string{}, askToken...), "customize", chosen, "n", "", "retry", "second-name", "", "")...)
			out := g.run(t, input+"\n", 0)
			for _, want := range []string{"Cloudflare may have made the bucket", "may have been created by the earlier request, which got no answer", "Another bucket name"} {
				if !setupContainsText(out, want) {
					t.Errorf("output lacks %q:\n%s", want, out)
				}
			}
			if setupContainsText(out, "is taken; preparing") {
				t.Errorf("replaced the name quietly:\n%s", out)
			}
			if got := g.savedConfig(t).Storage.Bucket; got != "second-name" || len(g.cf.Buckets) != 2 {
				t.Errorf("bucket %q, buckets %v", got, g.cf.Buckets)
			}
		})
	}
}

// A token whose creation got an answer setup cannot read may exist, so the
// hint names it however the answer failed.
func TestGuidedR2UnreadableTokenAnswerNamesTheToken(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	g.cf.Fail(cloudflaretest.RouteCreateToken, cloudflaretest.Failure{Status: http.StatusOK, RawBody: "<html>ok</html>", AfterWork: true, Times: 1})
	out := g.run(t, guidedAnswers(append(append([]string{}, askToken...), "", "stop")...), 1)
	name := g.cf.Tokens()[0].Name
	for _, want := range []string{"Cloudflare answered, but setup couldn't read the answer.", `If Cloudflare did create it, revoke the token named "` + name + `"`} {
		if !setupContainsText(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// A token answer with an ID and no value means the token exists: it is
// revoked by its ID; a 404 there is reported as Cloudflare saying the token
// does not exist, with what to check.
func TestGuidedR2TokenAnswerWithoutAValueIsRevokedByID(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	g.cf.Fail(cloudflaretest.RouteCreateToken, cloudflaretest.Failure{Status: http.StatusOK, RawBody: `{"success":true,"result":{"id":"abc123"}}`, Times: 1})
	out := g.run(t, guidedAnswers(append(append([]string{}, askToken...), "", "stop")...), 1)
	var deleted []string
	for _, r := range g.cf.Requests() {
		if r.Route == cloudflaretest.RouteDeleteToken {
			deleted = append(deleted, r.Path)
		}
	}
	if len(deleted) != 1 || !strings.HasSuffix(deleted[0], "/tokens/abc123") || !setupContainsText(out, "Cloudflare says that token doesn't exist.") || !setupContainsText(out, `named "`) {
		t.Fatalf("deleted %v\n%s", deleted, out)
	}
	// An invalid returned identifier cannot overwrite the durable creation
	// intent, even though best-effort provider cleanup still runs.
	home, err := g.env.readHome()
	if err != nil {
		t.Fatal(err)
	}
	slots, err := issuance.List(home)
	if err != nil || len(slots) != 1 || slots[0].State != issuance.CreationIntent {
		t.Fatalf("lost creation evidence: %v %v", slots, err)
	}

}

// Guided creation works through the explicit shortcut without environment switches.
func TestGuidedR2AvailableWithoutFlags(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	g.env.LookupEnv = func(string) (string, bool) { return "", false }
	g.run(t, g.happy(), 0)
	if len(g.cf.Tokens()) != 3 {
		t.Fatal("guided creation did not issue its archive key and spares")
	}
	g.savedConfig(t)
}

func TestGuidedR2DoesNotAddNumberedProviderChoices(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	out := g.run(t, g.happy(), 0)
	if !strings.Contains(out, "  1) Cloudflare R2 (default)\n  2) Amazon S3\n\n  [h] Setup instructions") {
		t.Fatalf("menu:\n%s", out)
	}
}

// signalStub stands in for Env.Interrupts and counts how many signal
// handlers are installed and removed.
type signalStub struct {
	ch     chan os.Signal
	starts atomic.Int32
	stops  atomic.Int32
}

func newSignalStub() *signalStub { return &signalStub{ch: make(chan os.Signal, 4)} }

func (s *signalStub) interrupts() (<-chan os.Signal, func()) {
	s.starts.Add(1)
	return s.ch, func() { s.stops.Add(1) }
}

func (s *signalStub) active() int32 { return s.starts.Load() - s.stops.Load() }

// probeReader gives setup one answer per Read, and tells onRead each time
// setup is waiting for input, as a terminal would block.
type probeReader struct {
	lines  []string
	onRead func()
}

func (r *probeReader) Read(p []byte) (int, error) {
	r.onRead()
	if len(r.lines) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.lines[0]+"\n")
	r.lines = r.lines[1:]
	return n, nil
}

// No signal handler is installed while setup waits for the person: at every
// prompt Ctrl-C keeps its ordinary effect, including "Another bucket name".
// Every handler that was installed is removed again.
func TestGuidedR2AsksForANewNameOutsideTheSignalHandler(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	g.cf.Buckets["taken-name"] = ""
	sig := newSignalStub()
	g.env.Interrupts = sig.interrupts
	g.env.IsTerminal = func(any) bool { return true }
	var reads, activeAtRead atomic.Int32
	answers := append(append([]string{"no", "", "", "r2-create"}, askToken...), "customize", "taken-name", "n", "", "other-name", "", "")
	in := &probeReader{lines: answers, onRead: func() {
		reads.Add(1)
		activeAtRead.Add(sig.active())
	}}
	var out bytes.Buffer
	if code := Run([]string{"setup"}, in, &out, &out, g.env); code != 0 {
		t.Fatalf("exit %d\n%s", code, &out)
	}
	if !setupContainsText(out.String(), "Another bucket name") {
		t.Fatalf("no second name asked for:\n%s", &out)
	}
	if reads.Load() < 5 || activeAtRead.Load() != 0 {
		t.Fatalf("%d reads, %d of them with a signal handler installed", reads.Load(), activeAtRead.Load())
	}
	if sig.starts.Load() < 2 || sig.stops.Load() != sig.starts.Load() {
		t.Fatalf("%d handlers installed, %d removed", sig.starts.Load(), sig.stops.Load())
	}
	if got := g.savedConfig(t).Storage.Bucket; got != "other-name" {
		t.Fatalf("bucket %q", got)
	}
}

// A signal while the revoke request is out is answered, not ignored: setup
// finishes the revoke first.
func TestGuidedR2SignalDuringRevokeIsAnswered(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	sig := newSignalStub()
	g.env.Interrupts = sig.interrupts
	g.env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		return &ctxStore{onList: func() { sig.ch <- syscall.SIGINT }}, nil
	}
	out := &syncBuffer{}
	g.deleteToken = func(_ context.Context, _, _ string, next func() error) error {
		sig.ch <- syscall.SIGINT
		for deadline := time.Now().Add(5 * time.Second); !setupContainsText(out.String(), "Still revoking"); {
			if time.Now().After(deadline) {
				t.Error("the second signal was not answered")
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		return next()
	}
	code := Run([]string{"setup"}, strings.NewReader(guidedAnswers(append(append([]string{}, askToken...), "")...)), out, out, g.env)
	if code != 128+int(syscall.SIGINT) {
		t.Fatalf("exit %d\n%s", code, out)
	}
	text := out.String()
	if len(g.cf.Live()) != 0 || !setupContainsText(text, "Revoking the key that wasn't used") || !setupContainsText(text, "Still revoking the key's token; please wait.") || !setupContainsText(text, "Revoked the key that wasn't used.") {
		t.Fatalf("live %d\n%s", len(g.cf.Live()), text)
	}
}

// matchOption takes an exact key first (whatever its case), else the one key
// an answer begins, and an answer that begins several keys, as "r" does with
// guided R2 creation on and "s" does with guided S3 creation, is ambiguous:
// the person types the whole key or the number.
func TestMatchOptionExactThenUniquePrefixThenAmbiguous(t *testing.T) {
	t.Parallel()
	options := []option{{"r2", "R2"}, {"s3", "S3"}, {"s3-new", "Create S3"}, {"r2-create", "Create R2"}, {"help", "Help"}}
	for answer, want := range map[string]string{"r2": "r2", "R2": "r2", "s3": "s3", "s3-new": "s3-new", "r2-c": "r2-create", "r2-create": "r2-create", "h": "help", "HELP": "help"} {
		if got, ok := matchOption(answer, options); !ok || got != want {
			t.Errorf("%q chose %q (%v), want %q", answer, got, ok, want)
		}
	}
	for _, answer := range []string{"", "r", "s", "x", "r3", "r2-x"} {
		if got, ok := matchOption(answer, options); ok {
			t.Errorf("%q chose %q", answer, got)
		}
	}
}

// stageStorageSecret journals the reference before it writes the Keychain,
// and lists it for cleanup.
func TestStageStorageSecretJournalsBeforeTheKeychain(t *testing.T) {
	t.Parallel()
	var order []string
	var journaled []string
	keychain := &hookKeychain{fakeKeychain: newFakeKeychain()}
	env := testEnv(t, t.TempDir(), time.Now())
	env.Credentials = func() (credentials.CredentialStore, error) { return keychain, nil }
	draft := &setupDraft{Version: draftFormat}
	save := func() error {
		order = append(order, "save")
		journaled = append([]string(nil), draft.StagedRefs...)
		return nil
	}
	var atKeychain []string
	keychain.onSave = func(ref string) {
		order = append(order, "keychain")
		atKeychain = append([]string(nil), journaled...)
		if !slices.Contains(atKeychain, ref) {
			t.Errorf("the draft on disk lists %v, not %s, when the Keychain is written", atKeychain, ref)
		}
	}
	cfg := credentials.Config{Provider: credentials.ProviderR2, Bucket: "b"}
	secret := credentials.R2Credentials{AccessKeyID: "id", SecretAccessKey: "secret"}
	if err := stageStorageSecret(draft, save, env, &cfg, secret); err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "save,keychain" {
		t.Fatalf("order %v", order)
	}
	if !strings.HasPrefix(cfg.R2CredentialRef, "setup-") || draft.CredentialRef != cfg.R2CredentialRef || !slices.Equal(draft.StagedRefs, []string{cfg.R2CredentialRef}) || draft.Config.Storage.R2CredentialRef != cfg.R2CredentialRef {
		t.Fatalf("cfg %+v, draft ref %q, staged %v", cfg, draft.CredentialRef, draft.StagedRefs)
	}
	if got, err := keychain.Load(context.Background(), cfg.R2CredentialRef); err != nil || got != secret {
		t.Fatalf("stored %+v (%v)", got, err)
	}
}

func TestStageStorageSecretFailures(t *testing.T) {
	t.Parallel()
	secret := credentials.R2Credentials{AccessKeyID: "id", SecretAccessKey: "secret"}
	t.Run("journal fails", func(t *testing.T) {
		t.Parallel()
		keychain := &hookKeychain{fakeKeychain: newFakeKeychain(), onSave: func(string) { t.Error("the Keychain was written after the journal failed") }}
		env := testEnv(t, t.TempDir(), time.Now())
		env.Credentials = func() (credentials.CredentialStore, error) { return keychain, nil }
		cfg := credentials.Config{Provider: credentials.ProviderR2}
		err := stageStorageSecret(&setupDraft{}, func() error { return errors.New("disk full") }, env, &cfg, secret)
		if err == nil || !setupContainsText(err.Error(), "disk full") {
			t.Fatalf("error %v", err)
		}
	})
	t.Run("Keychain fails", func(t *testing.T) {
		t.Parallel()
		env := testEnv(t, t.TempDir(), time.Now())
		env.Credentials = func() (credentials.CredentialStore, error) { return saveFailsKeychain{newFakeKeychain()}, nil }
		cfg := credentials.Config{Provider: credentials.ProviderR2}
		draft := &setupDraft{}
		err := stageStorageSecret(draft, func() error { return nil }, env, &cfg, secret)
		if err == nil || !setupContainsText(err.Error(), "save staged credential") || len(draft.StagedRefs) != 1 {
			t.Fatalf("error %v, staged %v", err, draft.StagedRefs)
		}
	})
	t.Run("Keychain unavailable", func(t *testing.T) {
		t.Parallel()
		env := testEnv(t, t.TempDir(), time.Now())
		env.Credentials = func() (credentials.CredentialStore, error) { return nil, errors.New("none") }
		cfg := credentials.Config{Provider: credentials.ProviderR2}
		if err := stageStorageSecret(&setupDraft{}, func() error { return nil }, env, &cfg, secret); err == nil || !setupContainsText(err.Error(), "open Keychain") {
			t.Fatalf("error %v", err)
		}
	})
}

// hookKeychain tells onSave each key it is asked to store.
type hookKeychain struct {
	*fakeKeychain
	onSave func(ref string)
}

func (k *hookKeychain) Save(ctx context.Context, ref string, value credentials.R2Credentials) error {
	if k.onSave != nil {
		k.onSave(ref)
	}
	return k.fakeKeychain.Save(ctx, ref, value)
}

// When the last save of the storage step fails, after the key is staged, the
// new token is revoked: the key is not in use, so nothing may be left live.
func TestGuidedR2RevokesTheTokenWhenTheDraftSaveFailsAfterStaging(t *testing.T) {
	t.Parallel()
	g := newGuidedR2Fixture(t)
	var out bytes.Buffer
	p := newPrompter(strings.NewReader(strings.Join(append(append([]string{guidedR2Choice}, askToken...), ""), "\n")+"\n"), &out)
	draft := setupDraft{Version: draftFormat, Step: 1, Config: config.Config{Harnesses: []string{"claude"}}}
	saves := 0
	save := func() error {
		saves++
		if saves == 2 {
			// The first save journals the staged key; the second records that
			// storage is done.
			return errors.New("disk full")
		}
		return nil
	}
	var verified credentials.Config
	_, err := advanceSetupDraft(p, &draft, save, draftPath(g.home), g.home, g.userHome, g.env, nil, &verified, false)
	if err == nil || !setupContainsText(err.Error(), "disk full") {
		t.Fatalf("error %v\n%s", err, &out)
	}
	if len(g.cf.Tokens()) != 1 || len(g.cf.Live()) != 0 || g.cf.Calls(cloudflaretest.RouteDeleteToken) != 1 {
		t.Fatalf("tokens %d, live %d\n%s", len(g.cf.Tokens()), len(g.cf.Live()), &out)
	}
	if !setupContainsText(out.String(), "Setup couldn't store the new key") || setupContainsText(out.String(), "not saved anywhere") || !g.apis[0].discarded {
		t.Fatalf("output:\n%s", &out)
	}
}
