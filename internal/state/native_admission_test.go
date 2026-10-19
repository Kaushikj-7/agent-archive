package state

import (
	"context"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"strings"
	"testing"
	"time"
)

func TestPreparedNativeAdmissionZeroCloseIsSafeAndCannotCommit(t *testing.T) {
	t.Parallel()
	var prepared PreparedNativeAdmission
	if _, err := prepared.Filtered(); !errors.Is(err, agentapi.ErrClosed) {
		t.Fatalf("zero prepared output: %v", err)
	}
	s := newTestStore(t)
	if _, err := s.CommitNativeAdmission(t.Context(), &prepared); !errors.Is(err, agentapi.ErrClosed) {
		t.Fatalf("zero prepared commit: %v", err)
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPreparedNativeAdmissionStagesOutsideHooksAndHonorsCancellation(t *testing.T) {
	t.Parallel()
	fixture := newNativeAdmissionFixture(t)
	prepared, err := fixture.prepare(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if fixture.budget.Available() == agentapi.PassReadBudget {
		t.Fatal("prepared output/source was not retained")
	}
	ctx, cancel := context.WithCancel(t.Context())
	syncs := 0
	fixture.store.onIndexSync = func() {
		syncs++
		unlock, err := local.NamedLockWait(fixture.store.home, "hooks.lock", 0)
		if err != nil {
			t.Error("reservation fsync under hooks lock", err)
		} else {
			unlock()
		}
		cancel()
	}
	if _, err := fixture.store.CommitNativeAdmission(ctx, prepared); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled reservation committed: %v", err)
	}
	if syncs != 1 {
		t.Fatalf("unexpected staging count %d", syncs)
	}
	key := agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: fixture.candidate.Intent.NativeSessionID}
	if _, found, err := fixture.store.readQualifiedIndex(key); err != nil || found {
		t.Fatal("canceled stage reserved identity", found, err)
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	fixture.assertReleased(t)
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.CommitNativeAdmission(t.Context(), prepared); !errors.Is(err, agentapi.ErrClosed) {
		t.Fatal("closed preparation committed", err)
	}
}

func TestPreparedNativeAdmissionRegistrationSyncOutsideHooks(t *testing.T) {
	t.Parallel()
	fixture := newNativeAdmissionFixture(t)
	prepared, err := fixture.prepare(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	syncs := 0
	fixture.store.onIndexSync = func() {
		unlock, err := local.NamedLockWait(fixture.store.home, "hooks.lock", 0)
		if err != nil {
			t.Error("index fsync under hooks lock", err)
		} else {
			unlock()
		}
	}
	fixture.store.onWriteSync = func() {
		syncs++
		unlock, err := local.NamedLockWait(fixture.store.home, "hooks.lock", 0)
		if err != nil {
			t.Error("registration fsync under hooks lock", err)
		} else {
			unlock()
		}
	}
	owner, err := fixture.store.CommitNativeAdmission(t.Context(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	if syncs == 0 {
		t.Fatal("registration staging seam not exercised")
	}
	if _, err := fixture.store.CommitNativeAdmission(t.Context(), prepared); err != nil {
		t.Fatal("same preparation retry not idempotent", err)
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	fixture.assertReleased(t)
	current, found, err := fixture.store.LoadRegistration(owner.ArchiveSessionID)
	if err != nil || !found || current.NativeAdmission == nil || current.NativeBinding == nil {
		t.Fatal("incomplete committed owner", current, found, err)
	}
}

func TestPreparedNativeAdmissionKeepsOpaqueConfiguredProjectIdentity(t *testing.T) {
	t.Parallel()
	fixture := newNativeAdmissionFixture(t)
	cfg, found, err := config.Load(fixture.store.home)
	if err != nil || !found {
		t.Fatal(err)
	}
	next := cfg
	next.Archive.Projects = append([]archive.ProjectActivation(nil), cfg.Archive.Projects...)
	next.Archive.Projects[0].ProjectID = "existing-opaque-project"
	next, err = config.ReconcileMuseCapture(next, cfg, cfg.MuseCapture.Since.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Save(fixture.store.home, next); err != nil {
		t.Fatal(err)
	}
	proof := *fixture.candidate.Intent.NativeAdmission
	proof.PolicyFingerprint = next.MuseCapture.PolicyFingerprint
	proof.WindowGeneration = next.MuseCapture.Generation
	fixture.candidate.Intent.NativeAdmission = &proof
	fixture.candidate.ProjectID = next.Archive.Projects[0].ProjectID
	prepared, err := fixture.prepare(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	owner, err := fixture.store.CommitNativeAdmission(t.Context(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	if owner.ProjectID != fixture.candidate.ProjectID {
		t.Fatal("configured opaque identity was recomputed")
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	fixture.assertReleased(t)
	for _, id := range []string{"", strings.Repeat("x", 4097), "project\x00identity", "project\nidentity", string([]byte{0xff})} {
		candidate := fixture.candidate
		candidate.ProjectID = id
		if _, err := copyNativeAdmissionCandidate(candidate); err == nil {
			t.Fatal("unsupported supplemental identity copied")
		}
	}
}
