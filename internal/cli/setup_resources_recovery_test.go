package cli

import (
	"bytes"
	"context"
	"errors"
	"github.com/wangjohn/agent-archive/internal/cloudflare"
	"github.com/wangjohn/agent-archive/internal/cloudflare/cloudflaretest"
	"github.com/wangjohn/agent-archive/internal/local"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/issuance"
	"github.com/wangjohn/agent-archive/internal/setupresources"
	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

func recoveryReceipt(t *testing.T, home string, state setupresources.State) setupresources.Receipt {
	t.Helper()
	r, err := setupresources.Begin(home, setupresources.Receipt{Provider: setupresources.S3, Profile: "creator", Bucket: "agent-archive-example", Region: "us-east-1", CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if state != setupresources.RequestSent {
		if err = setupresources.Finish(home, r, setupresources.Created); err != nil {
			t.Fatal(err)
		}
		r.State = setupresources.Created
		if state != setupresources.Created {
			if err = setupresources.Finish(home, r, state); err != nil {
				t.Fatal(err)
			}
			r.State = state
		}
	}
	return r
}

func TestUncertainResourceRecoveryConnectsWithoutClaimingOwnership(t *testing.T) {
	// The golden specifies ASCII output independently of the inherited terminal.
	t.Setenv("TERM", "xterm-256color")
	home := t.TempDir()
	r := recoveryReceipt(t, home, setupresources.RequestSent)
	out := &guidedCommandOutput{caps: promptCapabilities{ASCII: true}}
	p := newPrompter(strings.NewReader("1\n"), out)
	saved, have, err := recoverSetupResources(p, home, setupDraft{}, false, config.Config{}, Env{})
	if err != nil || !have || saved.Config.Storage.Bucket != r.Bucket || saved.Step != 0 {
		t.Fatalf("saved=%+v have=%v err=%v", saved, have, err)
	}
	receipts, err := setupresources.List(home)
	if err != nil || receipts[0].State != setupresources.RequestSent {
		t.Fatalf("ownership changed: %+v %v", receipts, err)
	}
	golden.Check(t, "testdata/setup-resource-uncertain.txt", []byte(trimScreenLineEnds(out.String())))
	if !strings.Contains(out.String(), "Creation outcome unknown") {
		t.Fatal(out.String())
	}
	if err = new(prompter).refuseUncertainCreation(home, setupresources.Receipt{Provider: setupresources.S3, Bucket: r.Bucket, Profile: "rebound"}); err == nil {
		t.Fatal("ambiguous create allowed after profile change")
	}
}

func TestRecoveryLeavesActiveBucketAloneAcrossProfileChanges(t *testing.T) {
	home := t.TempDir()
	r := recoveryReceipt(t, home, setupresources.Created)
	active := config.Config{Storage: credentials.Config{Provider: "s3", Bucket: r.Bucket, AWSProfile: "different"}}
	var out bytes.Buffer
	_, _, err := recoverSetupResources(newPrompter(strings.NewReader(""), &out), home, setupDraft{}, false, active, Env{})
	if err != nil || out.Len() != 0 {
		t.Fatalf("active bucket offered for recovery: %v %s", err, out.String())
	}
}

func TestManualBucketCleanupRemainsPendingAcrossRestart(t *testing.T) {
	home := t.TempDir()
	r := recoveryReceipt(t, home, setupresources.Created)
	var out bytes.Buffer
	p := newPrompter(strings.NewReader("2\n"), &out)
	if err := setupResourceDetails(p, home, r, nil, setupDraft{}, false, config.Config{}, Env{}); err != nil {
		t.Fatal(err)
	}
	receipts, err := setupresources.List(home)
	if err != nil || receipts[0].State != setupresources.CleanupPending {
		t.Fatalf("pending cleanup lost: %+v %v", receipts, err)
	}
	if !strings.Contains(out.String(), "cannot verify ownership or deletion") {
		t.Fatal(out.String())
	}
}

func TestReferencedGuidedKeysAreNeverCleanupCandidates(t *testing.T) {
	s := issuance.Slot{Origin: issuance.Guided, State: issuance.CleanupPending, SecretRef: "opaque-key", SlotID: "slot"}
	if !cleanupGuidedEligible(s, setupDraft{}, false, config.Config{}) {
		t.Fatal("orphan withheld")
	}
	active := config.Config{Storage: credentials.Config{R2CredentialRef: s.SecretRef}}
	if cleanupGuidedEligible(s, setupDraft{}, false, active) {
		t.Fatal("active key eligible")
	}
	saved := setupDraft{StagedRefs: []string{s.SecretRef}}
	if cleanupGuidedEligible(s, saved, true, config.Config{}) {
		t.Fatal("draft key eligible")
	}
	s.State = issuance.Own
	if cleanupGuidedEligible(s, setupDraft{}, false, config.Config{}) {
		t.Fatal("own key eligible")
	}
}

func TestUnknownCreationRetryRequiresCurrentProcessContext(t *testing.T) {
	home := t.TempDir()
	r := recoveryReceipt(t, home, setupresources.RequestSent)
	p := newPrompter(strings.NewReader(""), &bytes.Buffer{})
	p.rememberResourceRequest(r.ID)
	if err := p.refuseUncertainCreation(home, r); err != nil {
		t.Fatalf("explicit same-process retry blocked: %v", err)
	}
	restarted := newPrompter(strings.NewReader(""), &bytes.Buffer{})
	if err := restarted.refuseUncertainCreation(home, r); err == nil {
		t.Fatal("restart forgot uncertain provider request")
	}
}

type protectedKeyOwner string

const (
	protectedKeyActive     protectedKeyOwner = "active"
	protectedKeyDraft      protectedKeyOwner = "draft"
	protectedKeyStaged     protectedKeyOwner = "staged"
	protectedKeySpare      protectedKeyOwner = "spare"
	protectedKeyUnreadable protectedKeyOwner = "unreadable"
	protectedKeyUnused     protectedKeyOwner = "unused"
)

func TestRecoveryCleanupProtectsReenteredProviderKeys(t *testing.T) {
	for _, owner := range []protectedKeyOwner{protectedKeyActive, protectedKeyDraft, protectedKeyStaged, protectedKeySpare, protectedKeyUnreadable, protectedKeyUnused} {
		for _, lost := range []bool{false, true} {
			t.Run(string(owner)+map[bool]string{false: "-known", true: "-lost"}[lost], func(t *testing.T) {
				env, home, cf, kc := dedicatedFixture(t)
				i := fixtureIssuer(t, env, home)
				slot, err := issuance.New(i.cfg.MachineID, i.cfg.DestinationID(), i.account, i.bucket, i.group, issuance.Guided, env.now())
				must(t, err)
				must(t, issuance.Save(home, slot))
				resource, err := cloudflare.BucketResource(i.account, i.bucket)
				must(t, err)
				token, err := i.api.CreateToken(context.Background(), i.account, cloudflare.TokenSpec{Name: slot.ProviderName, Policies: []cloudflare.Policy{{PermissionGroupIDs: []string{i.group}, Resources: map[string]string{resource: "*"}}}})
				must(t, err)
				derived := cloudflare.DeriveS3Credentials(token)
				key := credentials.R2Credentials{AccessKeyID: derived.AccessKeyID, SecretAccessKey: derived.SecretAccessKey}
				slot.State = issuance.CleanupPending
				slot.ProviderID = token.ID
				if lost {
					slot.ProviderID = ""
				}
				must(t, issuance.Save(home, slot))
				active, _, err := config.Load(home)
				must(t, err)
				switch owner {
				case protectedKeyActive:
					must(t, kc.Save(context.Background(), "reentered", key))
					active.Storage.R2CredentialRef = "reentered"
					must(t, config.Save(home, active))
				case protectedKeySpare:
					ref := "issued-" + strings.Repeat("f", 32)
					must(t, kc.Save(context.Background(), ref, key))
					active.SpareCredentialRefs = []string{ref}
					must(t, config.Save(home, active))
				case protectedKeyDraft, protectedKeyStaged:
					must(t, kc.Save(context.Background(), "reentered", key))
					draftConfig := active
					var stagedRefs []string
					if owner == protectedKeyDraft {
						draftConfig.Storage.R2CredentialRef = "reentered"
					} else {
						stagedRefs = []string{"reentered"}
					}
					must(t, local.Write(draftPath(home), setupDraft{Version: draftFormat, Config: draftConfig, Step: 1, StagedRefs: stagedRefs}))
				case protectedKeyUnused:
					// Leave the provider key absent from every protected credential.
				case protectedKeyUnreadable:
					env.Credentials = func() (credentials.CredentialStore, error) { return nil, errors.New("locked") }
				}
				env.LookupEnv = func(name string) (string, bool) { return bootstrapCanary, name == "CLOUDFLARE_API_TOKEN" }
				var out bytes.Buffer
				// The deletion gate reads protected credentials from current disk state.
				err = cleanupSetupKey(newPrompter(strings.NewReader("2\n"), &out), home, slot, env)
				must(t, err)
				slots, err := issuance.List(home)
				must(t, err)
				if owner == protectedKeyUnused {
					if cf.Calls(cloudflaretest.RouteDeleteToken) != 1 || slots[0].State != issuance.Deleted {
						t.Fatalf("unused key retained: %+v %s", slots, out.String())
					}
				} else if cf.Calls(cloudflaretest.RouteDeleteToken) != 0 || len(cf.Live()) != 1 || slots[0].State != issuance.CleanupPending {
					t.Fatalf("protected key revoked: %+v %s", slots, out.String())
				}
			})
		}
	}
}
