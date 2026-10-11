package cli

import (
	"bytes"
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
	r, err := setupresources.Begin(home, setupresources.Receipt{Provider: "s3", Profile: "creator", Bucket: "agent-archive-example", Region: "us-east-1", CreatedAt: time.Now()})
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
	home := t.TempDir()
	r := recoveryReceipt(t, home, setupresources.RequestSent)
	var out bytes.Buffer
	p := newPrompter(strings.NewReader("1\n"), &out)
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
	if err = refuseUncertainCreation(home, setupresources.Receipt{Provider: "s3", Bucket: r.Bucket, Profile: "rebound"}); err == nil {
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
