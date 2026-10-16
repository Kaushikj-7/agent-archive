package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/storage"
)

type journalFaultPhase string

const (
	journalFaultAdmission  journalFaultPhase = "admission"
	journalFaultCompletion journalFaultPhase = "completion"
	journalFaultRemoval    journalFaultPhase = "removal"
)

var errJournalLostAck = errors.New("private fixture lost coordinator acknowledgment")

type journalFaultStore struct {
	*qualifiedStore
	phase journalFaultPhase
	lost  bool
	armed bool
}

func (s *journalFaultStore) PutConditional(ctx context.Context, key string, raw []byte, condition storage.PutCondition) (string, error) {
	etag, err := s.qualifiedStore.PutConditional(ctx, key, raw, condition)
	if err != nil || key != CoordinatorKey || s.lost || !s.armed {
		return etag, err
	}
	var state admissions
	if err = json.Unmarshal(raw, &state); err != nil {
		return etag, err
	}
	if (s.phase == journalFaultAdmission && len(state.Owners) != 0) || (s.phase == journalFaultCompletion && len(state.Completed) != 0) || (s.phase == journalFaultRemoval && len(state.Completed) == 0) {
		s.lost = true
		return etag, errJournalLostAck
	}
	return etag, nil
}

func TestJournalDurableAdmissionCompletionAndRemovalLostAcknowledgments(t *testing.T) {
	for _, phase := range []journalFaultPhase{journalFaultAdmission, journalFaultCompletion, journalFaultRemoval} {
		t.Run(string(phase), func(t *testing.T) {
			_, provider, localStore, pending, guard, cfg := journalFixture(t)
			fault := &journalFaultStore{qualifiedStore: provider, phase: phase, armed: phase != journalFaultRemoval}
			store, err := WrapConfigured(fault, cfg)
			if err != nil {
				t.Fatal(err)
			}
			j := *pending.Catalog.Recovery
			ctx, settled, err := store.BeginJournalPublication(t.Context(), j.MutationID, pending.MetadataBytes, j, guard)
			if phase == journalFaultAdmission {
				if !errors.Is(err, errJournalLostAck) {
					t.Fatal("admission fault was not exercised", err)
				}
				guard.Release()
				guard, err = local.LockCollectorGuard(localStore.Home())
				if err != nil {
					t.Fatal(err)
				}
				defer guard.Release()
				store, err = WrapConfigured(fault, cfg)
				if err != nil {
					t.Fatal(err)
				}
				ctx, settled, err = store.BeginJournalPublication(t.Context(), j.MutationID, pending.MetadataBytes, j, guard)
			}
			if err != nil || settled {
				t.Fatal("admission recovery", settled, err)
			}
			defer store.EndPublicationAttempt(j.MutationID)
			if err = store.Publication(j.MutationID, pending.MetadataKey, "").Put(ctx, pending.MetadataKey, pending.MetadataBytes); err != nil {
				t.Fatal(err)
			}
			err = store.CompletePublication(ctx, j.MutationID, pending.MetadataBytes)
			if phase == journalFaultCompletion {
				if !errors.Is(err, errJournalLostAck) {
					t.Fatal("completion fault was not exercised", err)
				}
				store.EndPublicationAttempt(j.MutationID)
				guard.Release()
				guard, err = local.LockCollectorGuard(localStore.Home())
				if err != nil {
					t.Fatal(err)
				}
				defer guard.Release()
				store, err = WrapConfigured(fault, cfg)
				if err != nil {
					t.Fatal(err)
				}
				ctx, settled, err = store.BeginJournalPublication(t.Context(), j.MutationID, pending.MetadataBytes, j, guard)
				if err != nil || !settled {
					t.Fatal("completed receipt was not recovered", settled, err)
				}
				defer store.EndPublicationAttempt(j.MutationID)
				if _, _, err = store.BeginJournalPublication(t.Context(), j.MutationID, pending.MetadataBytes, j, guard); err == nil {
					t.Fatal("parallel completion replay accepted")
				}
				if err = store.Put(ctx, pending.SourceKey, pending.SourceBytes); !errors.Is(err, ErrAdmissionClosed) {
					t.Fatal("completion replay gained source authority", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			// Both owner-removal→record and record→unlink crash windows retain
			// this exact persisted pending journal and completed remote receipt.
			if err = guard.RecordJournalRemoval(j); err != nil {
				t.Fatal(err)
			}
			if err = localStore.RemoveCatalogPending("journal", j, guard); err != nil {
				t.Fatal(err)
			}
			proof, err := guard.ProveJournalRemoval(j)
			if err != nil {
				t.Fatal(err)
			}
			fault.armed = true
			err = store.AcknowledgeJournalRemoval(t.Context(), proof, guard)
			if phase == journalFaultRemoval {
				if !errors.Is(err, errJournalLostAck) {
					t.Fatal("removal fault was not exercised", err)
				}
				store, err = WrapConfigured(fault, cfg)
				if err != nil {
					t.Fatal(err)
				}
				err = store.AcknowledgeJournalRemoval(t.Context(), proof, guard)
			}
			if err != nil || !fault.lost {
				t.Fatal("lost acknowledgment recovery", err)
			}
			if err = guard.RemoveJournalRemoval(j); err != nil {
				t.Fatal(err)
			}
			state, _, err := store.Writer.Coordinator().read(t.Context())
			if err != nil || len(state.Owners) != 0 || len(state.Completed) != 0 {
				t.Fatal("recovery did not settle exact lifecycle", err)
			}
			if phase == journalFaultCompletion {
				released := make(chan struct{})
				go func() { guard.Release(); close(released) }()
				select {
				case <-released:
					t.Fatal("completed replay failed to retain guard claim")
				case <-time.After(10 * time.Millisecond):
				}
				store.EndPublicationAttempt(j.MutationID)
				select {
				case <-released:
				case <-time.After(time.Second):
					t.Fatal("completed replay did not join at End")
				}
				if err = store.Put(ctx, pending.SourceKey, pending.SourceBytes); !errors.Is(err, ErrAdmissionClosed) {
					t.Fatal("stale completed replay retained write authority", err)
				}
			}
		})
	}
}
