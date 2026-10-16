package state

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestPublicationNativeTargetLoanAndSelectingReplay(t *testing.T) {
	p := publicationFixture(t, publicationThread, time.Now())
	p.History = &PendingHistory{Version: 1}
	reg := archive.SessionRegistration{ArchiveSessionID: p.Bundle.ArchiveSessionID, NativeSessionID: p.Bundle.NativeSessionID, ProjectID: p.Bundle.ProjectID, Harness: p.Bundle.Capture.Harness, NativeChild: true}
	budget := agentapi.NewNativeReadBudget(128 << 10)
	release, err := FreezePublicationNativeTarget(t.Context(), &p, reg, budget)
	if err != nil {
		t.Fatal(err)
	}
	if used, _ := budget.Charged(); used == 0 {
		t.Fatal("frozen target has no owned loan")
	}
	p, err = PreparePublicationV2(p, PublicationPredecessor{State: PredecessorAbsent}, "destination", "admission", "policy", PublicationCapture)
	if err != nil {
		t.Fatal(err)
	}
	selecting := publishedState{Status: CacheStatusPublished, PublishedAt: time.Now(), Sources: []archive.SourceReference{p.Sources[0].Reference}, PublicationVersion: 5, Bundle: p.Bundle, MetadataBytes: p.MetadataBytes, Commit: p.Commit, Payloads: p.Sources, Preparation: p.Preparation}
	summary := selecting.summary()
	selecting.Summary = &summary
	if err = selecting.validateSelectingPublished(); err != nil {
		t.Fatal("real selecting producer", err)
	}
	// Rebinding the preparation digest cannot make a different target correspond
	// to the exact metadata and selected source owner.
	target := *p.Preparation.NativeTarget
	target.ParentSessionID = "foreign-parent"
	authority := *p.Preparation
	authority.NativeTarget = &target
	authority.SHA256 = preparationSHA(authority)
	commit := *p.Commit
	commit.PreparationSHA256 = authority.SHA256
	selecting.Preparation = &authority
	selecting.Commit = &commit
	if err = selecting.validateSelectingPublished(); err == nil {
		t.Fatal("foreign rebound target accepted")
	}
	target.ParentSessionID = ""
	target.NativeChild = true
	authority.SHA256 = preparationSHA(authority)
	commit.PreparationSHA256 = authority.SHA256
	if err = selecting.validateSelectingPublished(); err == nil {
		t.Fatal("rebound marker accepted")
	}
	release()
	release()
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("target loan leaked", used)
	}
}

func TestPublicationNativeTargetPressureAndCancelKeepOriginal(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		p := publicationFixture(t, publicationThread, time.Now())
		p.History = &PendingHistory{Version: 1}
		reg := archive.SessionRegistration{ArchiveSessionID: p.Bundle.ArchiveSessionID, NativeSessionID: p.Bundle.NativeSessionID, ProjectID: p.Bundle.ProjectID, Harness: p.Bundle.Capture.Harness, NativeChild: true}
		capacity := int64(1024)
		if cancelled {
			capacity = 128 << 10
		}
		budget := agentapi.NewNativeReadBudget(capacity)
		ctx, cancel := context.WithCancel(t.Context())
		if cancelled {
			cancel()
		}
		release, err := FreezePublicationNativeTarget(ctx, &p, reg, budget)
		cancel()
		if err == nil || release != nil || p.nativeTarget != nil {
			t.Fatal("failed freeze mutated authority", err)
		}
		if cancelled && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if used, _ := budget.Charged(); used != 0 {
			t.Fatal("failed freeze leaked", used)
		}
	}
}
