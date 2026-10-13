package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"os"
	"testing"

	"github.com/wangjohn/agent-archive/internal/destination"
)

type pickerCutover struct{}

func (pickerCutover) VerifyCatalogCutover(_ context.Context, source, target destination.Config) (CutoverProof, error) {
	return CutoverProof{ID: "synthetic-scoped-credential-proof", Source: destinationIdentity(source), Destination: destinationIdentity(target), Protocol: 11}, nil
}
func pickerDestination() destination.Config {
	return destination.Config{Provider: "s3", Bucket: "synthetic", ArchiveFormat: destination.FormatCatalogV4}
}

func completePickerUpgrade(t *testing.T, w *Writer, s *qualifiedStore) *PickerUpgrade {
	t.Helper()
	u, err := OpenPickerUpgrade(t.Context(), s, pickerDestination(), pickerCutover{}, "")
	if err != nil {
		t.Fatal(err)
	}
	for {
		done, e := u.Step(t.Context(), 2)
		if e != nil {
			t.Fatal(e)
		}
		if done {
			break
		}
	}
	if err = u.Activate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = u.FinishActivation(t.Context()); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestPickerUpgradeResumesPreservesIdentityAndReceiptAndEnablesCommit(t *testing.T) {
	t.Parallel()
	w, s := fixture(t)
	var inputs []CatalogMutation
	for i := range 7 {
		m := mutation(t, w, fmt.Sprintf("upgrade-%d", i))
		if _, err := w.Commit(t.Context(), m); err != nil {
			t.Fatal(err)
		}
		inputs = append(inputs, m)
	}
	deleted := inputs[0]
	_, rev, err := w.Find(t.Context(), deleted.SessionKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.Commit(t.Context(), CatalogMutation{ID: "delete-before-upgrade", SessionKey: deleted.SessionKey, ExpectedRevision: rev}); err != nil {
		t.Fatal(err)
	}
	source, _, err := w.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	u, err := OpenPickerUpgrade(t.Context(), s, pickerDestination(), pickerCutover{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if done, e := u.Step(t.Context(), 2); e != nil || done {
		t.Fatalf("page %v %v", done, e)
	}
	if _, e := w.Commit(t.Context(), inputs[1]); !errors.Is(e, ErrAdmissionClosed) {
		t.Fatal(e)
	}
	if e := w.Coordinator().Active(t.Context()); e == nil {
		t.Fatal("candidate was reader-active")
	}
	owner := u.State.Owner
	u, err = OpenPickerUpgrade(t.Context(), s, pickerDestination(), pickerCutover{}, owner)
	if err != nil {
		t.Fatal(err)
	}
	for {
		done, e := u.Step(t.Context(), 2)
		if e != nil {
			t.Fatal(e)
		}
		if done {
			break
		}
	}
	if u.State.Copied != 6 {
		t.Fatal(u.State.Copied)
	}
	if err = u.Activate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, e := w.PutImmutable(t.Context(), KindMetadata, []byte(`{}`)); !errors.Is(e, ErrAdmissionClosed) {
		t.Fatal(e)
	}
	u, err = OpenPickerUpgrade(t.Context(), s, pickerDestination(), pickerCutover{}, owner)
	if err != nil {
		t.Fatal(err)
	}
	if err = u.Activate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = u.FinishActivation(t.Context()); err != nil {
		t.Fatal(err)
	}
	head, _, err := w.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if head.Protocol != 11 || head.Schema != 5 || !head.Picker.valid() || head.Receipts != source.Receipts {
		t.Fatal(head)
	}
	entry, _, err := w.Find(t.Context(), inputs[1].SessionKey)
	if err != nil || entry.Picker == nil {
		t.Fatal(entry, err)
	}
	if _, err = w.Commit(t.Context(), inputs[1]); err != nil {
		t.Fatal("old exact receipt changed", err)
	}
	if _, err = w.Commit(t.Context(), mutation(t, w, "after-upgrade")); err != nil {
		t.Fatal(err)
	}
	if err = w.Coordinator().Active(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = u.FinishActivation(t.Context()); err != nil {
		t.Fatal("completed activation replay after new publication", err)
	}
}

func TestPickerUpgradeFreshCatalogCompleteBeforeAvailability(t *testing.T) {
	t.Parallel()
	w, s := fixture(t)
	completePickerUpgrade(t, w, s)
	h, _, err := w.Head(t.Context())
	if err != nil || h.Protocol != 11 {
		t.Fatal(h, err)
	}
	if _, err = w.Commit(t.Context(), mutation(t, w, "fresh-scoped")); err != nil {
		t.Fatal(err)
	}
}

func TestPickerUpgradeRetainsUnknownPendingOwnerAndBlocksGC(t *testing.T) {
	t.Parallel()
	w, s := fixture(t)
	if err := w.Coordinator().Admit(t.Context(), "foreign-frozen-owner", "exact-frozen-digest", nil); err != nil {
		t.Fatal(err)
	}
	u, err := OpenPickerUpgrade(t.Context(), s, pickerDestination(), pickerCutover{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = u.Step(t.Context(), 1); !errors.Is(err, ErrAdmissionClosed) {
		t.Fatal(err)
	}
	if _, _, err = w.Coordinator().HeldBarrier(u.State.Owner).Hold(t.Context()); !errors.Is(err, ErrAdmissionClosed) {
		t.Fatal(err)
	}
	state, _, err := w.Coordinator().read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = w.Coordinator().RecoverUnleasedSeal(t.Context(), u.State.Owner, state.Generation); err == nil {
		t.Fatal("released upgrade seal")
	}
	if len(state.Owners) != 1 {
		t.Fatal("forgot unknown owner")
	}
	if err = w.Coordinator().Complete(t.Context(), "foreign-frozen-owner", "wrong"); !errors.Is(err, ErrMutationReuse) {
		t.Fatal(err)
	}
	if err = w.Coordinator().Complete(t.Context(), "foreign-frozen-owner", "exact-frozen-digest"); err != nil {
		t.Fatal(err)
	}
	if _, err = u.Step(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
}

func TestScopedLegacyMigrationConstructsCompleteIndexesBeforeActivation(t *testing.T) {
	t.Parallel()
	source, target, src, dst, _ := migrationFixture(t, 9)
	m, err := OpenScopedMigration(t.Context(), source, target, src, dst, pickerCutover{})
	if err != nil {
		t.Fatal(err)
	}
	for {
		done, e := m.Step(t.Context())
		if e != nil {
			t.Fatal(e)
		}
		if done {
			break
		}
	}
	if _, err = OpenSnapshot(t.Context(), target, nil); err == nil {
		t.Fatal("candidate was available")
	}
	if err = m.Activate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = m.FinishActivation(t.Context()); err != nil {
		t.Fatal(err)
	}
	w, err := New(target)
	if err != nil {
		t.Fatal(err)
	}
	h, _, err := w.Head(t.Context())
	if err != nil || h.Protocol != 11 {
		t.Fatal(h, err)
	}
	if _, err = w.Commit(t.Context(), mutation(t, w, "scoped-after-legacy")); err != nil {
		t.Fatal(err)
	}
}

func TestPickerUpgradeRollbackKeepsStrongerFenceAndRefusesNewPublications(t *testing.T) {
	t.Parallel()
	for _, publish := range []bool{false, true} {
		t.Run(fmt.Sprint(publish), func(t *testing.T) {
			t.Parallel()
			w, s := fixture(t)
			u := completePickerUpgrade(t, w, s)
			if publish {
				if _, err := w.Commit(t.Context(), mutation(t, w, "newer")); err != nil {
					t.Fatal(err)
				}
			}
			err := u.Rollback(t.Context())
			if publish {
				if err == nil {
					t.Fatal("forgot newer publication")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			h, _, err := w.Head(t.Context())
			if err != nil || h.Protocol != 11 || !h.Picker.valid() {
				t.Fatal(h, err)
			}
			if _, err = w.PutImmutable(t.Context(), KindMetadata, []byte(`{}`)); !errors.Is(err, ErrAdmissionClosed) {
				t.Fatal(err)
			}
			if err = w.Coordinator().Active(t.Context()); err != nil {
				t.Fatal("original indexed data unavailable", err)
			}
		})
	}
}

func TestScopedCommitLostResponseRetainsExactReceipts(t *testing.T) {
	t.Parallel()
	for _, unknown := range []bool{false, true} {
		t.Run(fmt.Sprint(unknown), func(t *testing.T) {
			t.Parallel()
			w, base := fixture(t)
			completePickerUpgrade(t, w, base)
			s := &lostStore{qualifiedStore: base, failRead: unknown}
			w, err := New(s)
			if err != nil {
				t.Fatal(err)
			}
			input := mutation(t, w, "scoped-lost-response")
			revision, err := w.Commit(t.Context(), input)
			if unknown {
				if !errors.Is(err, ErrCommitUnknown) {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			s.mu.Lock()
			s.failRead = false
			s.mu.Unlock()
			replay, err := w.Commit(t.Context(), input)
			if err != nil || replay == "" || (revision != "" && revision != replay) {
				t.Fatal(replay, err)
			}
			input.ExpectedRevision = "different-frozen-input"
			if _, err = w.Commit(t.Context(), input); !errors.Is(err, ErrMutationReuse) {
				t.Fatal(err)
			}
		})
	}
}

func TestScopedStorePublicationCASRaceAndConflictKeepCompleteUniverse(t *testing.T) {
	t.Parallel()
	w, base := fixture(t)
	completePickerUpgrade(t, w, base)
	delayed := &pausedCASStore{qualifiedStore: base, reached: make(chan struct{}), resume: make(chan struct{})}
	store, err := Wrap(delayed)
	if err != nil {
		t.Fatal(err)
	}
	first := mutation(t, store.Writer, "scoped-publication")
	raw, err := json.Marshal(first.Next.Summary)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := store.BeginPublication(t.Context(), first.ID, raw)
	if err != nil {
		t.Fatal(err)
	}
	defer store.EndPublicationAttempt(first.ID)
	delayed.mu.Lock()
	delayed.armed = true
	delayed.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- store.Publication(first.ID, first.SessionKey, "").Put(ctx, first.SessionKey, raw) }()
	<-delayed.reached
	unrelated := mutation(t, w, "unrelated-scoped-race")
	if _, err = w.Commit(t.Context(), unrelated); err != nil {
		t.Fatal(err)
	}
	close(delayed.resume)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if err = store.CompletePublication(ctx, first.ID, raw); err != nil {
		t.Fatal(err)
	}
	if _, err = w.Commit(t.Context(), first); err != nil {
		t.Fatal("identical publication digest changed", err)
	}
	_, revision, err := w.Find(t.Context(), first.SessionKey)
	if err != nil {
		t.Fatal(err)
	}
	first.ID = "new-conflicting-invocation"
	first.ExpectedRevision = "stale-revision"
	if _, err = w.Commit(t.Context(), first); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	first.ExpectedRevision = revision
	if _, err = w.Commit(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	h, etag, err := w.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	verifier := &PickerUpgrade{writer: w, State: PickerUpgradeState{Source: h, SourceETag: etag, Candidate: h, Copied: 2, Memberships: 24}}
	if err = verifier.verify(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestPickerUpgradeDrainsFrozenOldJournalAndPreservesCleanupReceipt(t *testing.T) {
	t.Parallel()
	store, provider, localStore, pending, guard, cfg := journalFixture(t)
	j := *pending.Catalog.Recovery
	_, settled, err := store.BeginJournalPublication(t.Context(), j.MutationID, pending.MetadataBytes, j, guard)
	if err != nil || settled {
		t.Fatal(settled, err)
	}
	var ctx context.Context
	store.EndPublicationAttempt(j.MutationID)
	u, err := OpenPickerUpgrade(t.Context(), provider, cfg, pickerCutover{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = u.Step(t.Context(), 1); !errors.Is(err, ErrAdmissionClosed) {
		t.Fatal(err)
	}
	// Transfer only the persisted original journal, then complete its old digest.
	ctx, settled, err = store.BeginJournalPublication(t.Context(), j.MutationID, pending.MetadataBytes, j, guard)
	if err != nil || settled {
		t.Fatal(settled, err)
	}
	if err = store.Put(ctx, pending.SourceKey, pending.SourceBytes); err != nil {
		t.Fatal(err)
	}
	if err = store.Publication(j.MutationID, pending.MetadataKey, "").Put(ctx, pending.MetadataKey, pending.MetadataBytes); err != nil {
		t.Fatal(err)
	}
	if err = store.CompletePublication(ctx, j.MutationID, pending.MetadataBytes); err != nil {
		t.Fatal(err)
	}
	store.EndPublicationAttempt(j.MutationID)
	for {
		done, e := u.Step(t.Context(), 1)
		if e != nil {
			t.Fatal(e)
		}
		if done {
			break
		}
	}
	if err = u.Activate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = u.FinishActivation(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, settled, err = store.BeginJournalPublication(t.Context(), j.MutationID, pending.MetadataBytes, j, guard)
	if err != nil || !settled {
		t.Fatal(settled, err)
	}
	defer store.EndPublicationAttempt(j.MutationID)
	if err = store.Put(ctx, pending.SourceKey, pending.SourceBytes); !errors.Is(err, ErrAdmissionClosed) {
		t.Fatal("old completed journal authorized new writes", err)
	}
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
	if err = store.AcknowledgeJournalRemoval(t.Context(), proof, guard); err != nil {
		t.Fatal(err)
	}
}

func TestScopedJournalUsesDistinctFrozenProtocol(t *testing.T) {
	t.Parallel()
	store, provider, _, pending, _, cfg := journalFixture(t)
	u, err := OpenPickerUpgrade(t.Context(), provider, cfg, pickerCutover{}, "")
	if err != nil {
		t.Fatal(err)
	}
	for {
		done, e := u.Step(t.Context(), 2)
		if e != nil {
			t.Fatal(e)
		}
		if done {
			break
		}
	}
	if err = u.Activate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = u.FinishActivation(t.Context()); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	if err = os.Chmod(home, 0700); err != nil {
		t.Fatal(err)
	}
	localStore, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err = config.Save(home, config.Config{MachineID: "synthetic"}); err != nil {
		t.Fatal(err)
	}
	guard, err := local.LockCollectorGuard(home)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Release()
	origin, err := guard.Origin()
	if err != nil {
		t.Fatal(err)
	}
	pending.Catalog.Protocol = 11
	pending.Catalog.Recovery.Protocol = 11
	pending.Catalog.Recovery.Origin = origin
	digest, err := localStore.CatalogJournalDigest("journal", pending)
	if err != nil {
		t.Fatal(err)
	}
	pending.Catalog.Recovery.SHA256 = digest
	if err = localStore.SavePending("journal", pending); err != nil {
		t.Fatal(err)
	}
	j := *pending.Catalog.Recovery
	ctx, settled, err := store.BeginJournalPublication(t.Context(), j.MutationID, pending.MetadataBytes, j, guard)
	if err != nil || settled {
		t.Fatal(settled, err)
	}
	defer store.EndPublicationAttempt(j.MutationID)
	if err = store.Put(ctx, pending.SourceKey, pending.SourceBytes); err != nil {
		t.Fatal(err)
	}
	if err = store.Publication(j.MutationID, pending.MetadataKey, "").Put(ctx, pending.MetadataKey, pending.MetadataBytes); err != nil {
		t.Fatal(err)
	}
	if err = store.CompletePublication(ctx, j.MutationID, pending.MetadataBytes); err != nil {
		t.Fatal(err)
	}
}

type boundedPickerDestination struct{ *qualifiedStore }

func (*boundedPickerDestination) List(context.Context, string) ([]storage.Object, error) {
	return nil, errors.New("unbounded construction inventory forbidden")
}

func TestScopedMigrationUsesBoundedDestinationInventory(t *testing.T) {
	t.Parallel()
	source, target, src, dst, _ := migrationFixture(t, 3)
	destination := &boundedPickerDestination{target}
	migration, err := OpenScopedMigration(t.Context(), source, destination, src, dst, pickerCutover{})
	if err != nil {
		t.Fatal(err)
	}
	for {
		done, e := migration.Step(t.Context())
		if e != nil {
			t.Fatal(e)
		}
		if done {
			break
		}
	}
	if err = migration.Activate(t.Context()); err != nil {
		t.Fatal(err)
	}
}
