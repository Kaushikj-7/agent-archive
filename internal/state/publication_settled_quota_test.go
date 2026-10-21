package state

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/config"
)

// padPublished appends whitespace to a published state file so it stands in
// for a large cached bundle, returning the file's new size.
func padPublished(t *testing.T, s *Store, id string, size int64) int64 {
	t.Helper()
	raw, err := os.ReadFile(s.publishedPath(id))
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(raw)) < size {
		raw = append(raw, bytes.Repeat([]byte(" "), int(size)-len(raw))...)
	}
	if err = os.WriteFile(s.publishedPath(id), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return int64(len(raw))
}

// fillDurableQuota leaves free bytes of the durable quota uncharged, given
// that nothing else in s's home is charged.
func fillDurableQuota(t *testing.T, s *Store, free int64) {
	t.Helper()
	sparseDurable(t, filepath.Join(s.home, "pending", "occupied.json"), (durableStorageQuota-free)/2)
}

// A history of settled publications is published cache: it must not use up
// the room the next session's publication needs, as it did when each settled
// record was charged twice its size.
func TestSettledPublicationsLeaveRoomForTheNextPublication(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	const free = 4 << 20
	var settled int64
	for i := range 3 {
		id := fmt.Sprintf("settled-%d", i)
		saveAccountingFixture(t, s, accountingFixture(t, id, false, false), false)
		settled += padPublished(t, s, id, 2<<20)
	}
	fillDurableQuota(t, s, free)
	if 2*settled <= free {
		t.Fatalf("settled records of %d bytes would fit even when charged twice; the fixture proves nothing", settled)
	}

	next := accountingFixture(t, "next", false, false)
	next.History = &PendingHistory{Version: 1}
	next, err := PreparePublicationV2(next, PublicationPredecessor{State: PredecessorAbsent}, "destination", "admission", "policy", PublicationCapture)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SavePending(next.Bundle.ArchiveSessionID, next); err != nil {
		t.Fatal("next publication refused beside settled records:", err)
	}
}

// Rewriting settled published state needs room only for its transient copy,
// not for the old file beside it nor a double charge of the new one.
func TestSettledPublicationRewriteNeedsOnlyItsCopy(t *testing.T) {
	t.Parallel()
	s, pending := settledPayloadFixture(t)
	id := pending.Bundle.ArchiveSessionID
	published, err := s.LoadPublishedState(id)
	if err != nil {
		t.Fatal(err)
	}
	if err = published.SaveCommittedPublication(pending, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = s.RemovePending(id); err != nil {
		t.Fatal(err)
	}
	const free = 4 << 20
	if size := padPublished(t, s, id, 5<<20); size <= free {
		t.Fatal("padded record fits beside its copy; the fixture proves nothing", size)
	}
	fillDurableQuota(t, s, free)
	if u, _ := publishedUsage(t, s, id, 0); u.charged != durableStorageQuota-free {
		t.Fatalf("census charged %d; want %d", u.charged, durableStorageQuota-free)
	}

	published, err = s.LoadPublishedState(id)
	if err != nil {
		t.Fatal(err)
	}
	if err = published.ClampAgeFrom(pending.Bundle.Capture.CapturedAt.Add(-time.Hour)); err != nil {
		t.Fatal("settled rewrite refused:", err)
	}
	if u, _ := publishedUsage(t, s, id, 0); u.charged != durableStorageQuota-free {
		t.Fatalf("rewritten settled state charged; census %d, want %d", u.charged, durableStorageQuota-free)
	}
}

// A rewrite refuses when even the transient copy does not fit.
func TestSettledPublicationRewriteStillNeedsRoomForItsCopy(t *testing.T) {
	t.Parallel()
	s, pending := settledPayloadFixture(t)
	id := pending.Bundle.ArchiveSessionID
	published, err := s.LoadPublishedState(id)
	if err != nil {
		t.Fatal(err)
	}
	if err = published.SaveCommittedPublication(pending, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = s.RemovePending(id); err != nil {
		t.Fatal(err)
	}
	fillDurableQuota(t, s, 2)
	err = published.ClampAgeFrom(pending.Bundle.Capture.CapturedAt.Add(-time.Hour))
	if !errors.Is(err, ErrDurableStorageCapacity) {
		t.Fatal("rewrite admitted without room for its copy:", err)
	}
}

// The accounting fact saved after a settled write is reused in place of a
// read, so it must charge exactly what a fresh classification of the saved
// file charges, including when minting settled privacy changed the state.
func TestSavedAccountingFactChargesLikeAFreshClassification(t *testing.T) {
	t.Parallel()
	for _, purpose := range []PublicationPurpose{PublicationCapture, PublicationPrivacyRewrite} {
		t.Run(string(purpose), func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			p := accountingFixture(t, "session", false, false)
			p.History = &PendingHistory{Version: 1}
			p, err := PreparePublicationV2(p, PublicationPredecessor{State: PredecessorAbsent}, "destination", "admission", "policy", purpose)
			if err != nil {
				t.Fatal(err)
			}
			budget := agentapi.NewNativeReadBudget(128 << 20)
			accounting, closeAccounting := s.WithPublicationAccounting(t.Context(), budget)
			defer closeAccounting()
			scoped, end := accounting.WithReadBudget(t.Context(), budget)
			defer end()
			published, err := scoped.LoadPublishedState(p.Bundle.ArchiveSessionID)
			if err != nil {
				t.Fatal(err)
			}
			if err = published.SaveCommittedPublication(p, p.Bundle.Capture.CapturedAt); err != nil {
				t.Fatal(err)
			}
			if minted := published.state.SettledPrivacy != nil; minted != (purpose == PublicationPrivacyRewrite) {
				t.Fatalf("settled privacy minted %v for %s", minted, purpose)
			}
			if err = config.WithDurableStorage(s.home, func(g config.DurableStorageGuard) (err error) {
				q, e := scoped.openDurableQuota(g)
				if e != nil {
					return e
				}
				defer func() { err = errors.Join(err, q.Close()) }()
				path := filepath.Join("published", p.Bundle.ArchiveSessionID+".json")
				info, e := q.home.Root.Lstat(path)
				if e != nil {
					return e
				}
				mode, charged, reused, e := q.accountingClassification(path, info)
				if e != nil {
					return e
				}
				if !reused {
					t.Fatal("saved accounting fact was not reused")
				}
				freshMode, freshCharged, _, e := q.store.readPublishedClassification(t.Context(), q.home, path, info)
				if e != nil {
					return e
				}
				if mode != freshMode || charged != freshCharged {
					t.Fatalf("saved fact classifies mode %d charge %d; fresh read %d charge %d", mode, charged, freshMode, freshCharged)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// An interrupted published write's temporary holds the durable quota in
// recovery until it is swept, whatever room was reserved for it, so a settled
// rewrite need not reserve its copy at the temporary's double census rate.
func TestLeftoverPublishedTemporaryHoldsQuotaInRecovery(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	sparseDurable(t, filepath.Join(s.home, "published", ".pending-0123456789abcdef0123456789abcdef"), 1)
	if err := config.WithPublicationComposition(s.home, func(c config.PublicationCompositionGuard) error {
		g, e := c.Storage(s.home)
		if e != nil {
			return e
		}
		q, e := s.openDurableQuota(g)
		if e != nil {
			return e
		}
		defer func() {
			if err := q.Close(); err != nil {
				t.Error(err)
			}
		}()
		if _, e = q.usageContext(t.Context()); !errors.Is(e, ErrDurableStorageRecovery) {
			t.Fatal("leftover temporary admitted by census:", e)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
