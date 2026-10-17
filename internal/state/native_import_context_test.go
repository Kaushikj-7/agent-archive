package state

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

func TestDurableReadExplicitCancellationOnUnscopedAndWarmStore(t *testing.T) {
	t.Parallel()
	for _, absent := range []bool{false, true} {
		for _, warm := range []bool{false, true} {
			name := "present"
			if absent {
				name = "absent"
			}
			if warm {
				name += "-warm"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				s := newTestStore(t)
				if absent {
					s = OpenReadOnly(filepath.Join(t.TempDir(), "absent"))
				}
				// A live receiver context must not replace the cancelled invocation.
				budget := agentapi.NewNativeReadBudget(8 << 20)
				scoped, closeScope := s.WithReadBudget(t.Context(), budget)
				defer closeScope()
				if warm {
					if err := scoped.CheckDurableReadRoots(); err != nil {
						t.Fatal(err)
					}
				}
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				for _, store := range []*Store{s, scoped} {
					if err := store.CheckDurableReadRootsContext(ctx); !errors.Is(err, context.Canceled) {
						t.Fatal("explicit root cancellation lost", err)
					}
					if err := store.ResumeGenerationRecoveries(ctx); !errors.Is(err, context.Canceled) {
						t.Fatal("recovery ignored invocation cancellation", err)
					}
					if err := store.validateReadyOwner(ctx, PendingPublication{Selection: &ReadySelection{}}); !errors.Is(err, context.Canceled) {
						t.Fatal("ready owner ignored invocation cancellation", err)
					}
					if _, _, _, err := store.stagedContext(ctx, "missing-session"); !errors.Is(err, context.Canceled) {
						t.Fatal("stage owner ignored invocation cancellation", err)
					}
				}
				if budget.Available() != 8<<20 {
					t.Fatal("cancelled inspection retained data")
				}
				if err := scoped.CheckDurableReadRootsContext(t.Context()); err != nil {
					t.Fatal("cancelled invocation poisoned live receiver", err)
				}
				if absent {
					if _, err := os.Stat(s.Home()); !os.IsNotExist(err) {
						t.Fatal("cancelled absent-root recovery created state", err)
					}
				}
			})
		}
	}
}

func TestNativeImportExplicitCancellationPreservesTypedAndReleasedAuthority(t *testing.T) {
	t.Parallel()
	var snapshot nativeImportReleaseSnapshot
	s, reg, next := releasedNativeImportFixture(t, &snapshot)
	budget := agentapi.NewNativeReadBudget(128 << 20)
	read, closeRead := s.WithReadBudget(t.Context(), budget)
	defer func() {
		closeRead()
		if budget.Available() != 128<<20 {
			t.Error("authority scope leaked retained budget", budget.Available())
		}
	}()
	published, err := read.LoadPublishedStateContext(t.Context(), reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := published.ValidateNativeImportAuthority(t.Context(), reg); err != nil {
		t.Fatal("actual released authority", err)
	}
	assertCancelledNativeImportAuthority(t, published, reg)
	// Commit the real normally prepared retained metadata successor. Cancellation
	// must also reach the v5 selecting validation and its existing budget loan.
	if err := s.SavePending(reg.ArchiveSessionID, next); err != nil {
		t.Fatal(err)
	}
	if err := published.SaveReadyPublication(next, next.ReadyAt); err != nil {
		t.Fatal(err)
	}
	if published.state.PublicationVersion != 5 {
		t.Fatal("successor did not use v5")
	}
	assertCancelledNativeImportAuthority(t, published, reg)
	if err := published.ValidateNativeImportAuthority(t.Context(), reg); err != nil {
		t.Fatal("live v5 authority lost", err)
	}
	// Independently restore an actual interrupted initial typed ACK, including
	// the original selected stage and pending bytes, rather than invent a proof.
	restoreNativeImportBeforeRelease(t, s, reg, snapshot, false)
	initial, err := s.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := initial.SaveReadyPublication(snapshot.publication, snapshot.publication.ReadyAt); err != nil {
		t.Fatal(err)
	}
	initial, err = s.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	active, err := initial.NativeImportStageActive(t.Context(), reg)
	if err != nil || !active {
		t.Fatal("actual typed stage not active", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	before, err := os.ReadFile(s.publishedPath(reg.ArchiveSessionID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := initial.NativeImportStageActive(ctx, reg); !errors.Is(err, context.Canceled) {
		t.Fatal("typed authority lost explicit cancellation", err)
	}
	guard, err := local.LockCollectorGuard(s.Home())
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Release()
	if err := s.VerifyAndBeginStagedRelease(ctx, reg.ArchiveSessionID, stagedRemote(snapshot.publication), guard); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled release was not refused", err)
	}
	after, err := os.ReadFile(s.publishedPath(reg.ArchiveSessionID))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("cancelled release changed ACK", err)
	}
	if _, err := os.Stat(filepath.Join(s.Home(), importStageRelative(reg.ArchiveSessionID))); err != nil {
		t.Fatal("cancelled release removed stage", err)
	}
}

func assertCancelledNativeImportAuthority(t *testing.T, published *Published, reg archive.SessionRegistration) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	before, err := os.ReadFile(published.store.publishedPath(reg.ArchiveSessionID))
	if err != nil {
		t.Fatal(err)
	}
	if err := published.ValidateNativeImportAuthority(ctx, reg); !errors.Is(err, context.Canceled) {
		t.Fatal("released authority lost explicit cancellation", err)
	}
	if _, err := published.NativeImportStageActive(ctx, reg); !errors.Is(err, context.Canceled) {
		t.Fatal("stage classification lost explicit cancellation", err)
	}
	after, err := os.ReadFile(published.store.publishedPath(reg.ArchiveSessionID))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("cancelled authority validation changed selecting state", err)
	}
}
