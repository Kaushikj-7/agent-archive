package catalog

import (
	"context"
	"errors"
	"fmt"
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
