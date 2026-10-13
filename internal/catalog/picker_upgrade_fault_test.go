package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/wangjohn/agent-archive/internal/storage"
)

type interruptedPickerStore struct {
	*qualifiedStore
	phase   string
	blocked bool
	fired   bool
}

func (s *interruptedPickerStore) PutConditional(ctx context.Context, key string, raw []byte, condition storage.PutCondition) (string, error) {
	etag, err := s.qualifiedStore.PutConditional(ctx, key, raw, condition)
	if err != nil || s.fired {
		return etag, err
	}
	if key == CoordinatorKey {
		var state admissions
		if err = json.Unmarshal(raw, &state); err != nil {
			return "", err
		}
		if state.Upgrade != nil && state.Upgrade.Phase == s.phase {
			s.fired = true
			s.blocked = true
			return "", errors.New("synthetic upgrade interruption")
		}
	}
	return etag, nil
}
func (s *interruptedPickerStore) GetCatalogVersion(ctx context.Context, key string, limit int64) ([]byte, storage.CatalogObjectVersion, error) {
	if s.blocked {
		return nil, storage.CatalogObjectVersion{}, errors.New("synthetic upgrade readback unavailable")
	}
	return s.qualifiedStore.GetCatalogVersion(ctx, key, limit)
}

func TestPickerUpgradeResumesEveryDurableCoordinatorPhase(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"draining", "building", "verified", "activating", "active"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			w, base := fixture(t)
			input := mutation(t, w, "fault-scoped")
			if _, err := w.Commit(t.Context(), input); err != nil {
				t.Fatal(err)
			}
			store := &interruptedPickerStore{qualifiedStore: base, phase: phase}
			u, err := OpenPickerUpgrade(t.Context(), store, pickerDestination(), pickerCutover{}, "")
			if err == nil {
				for {
					done, e := u.Step(t.Context(), 1)
					err = e
					if err != nil || done {
						break
					}
				}
			}
			if err == nil {
				err = u.Activate(t.Context())
			}
			if err == nil || !store.fired {
				t.Fatal("interruption was not exercised", err)
			}
			store.blocked = false
			checkpoint, err := PickerUpgradeCheckpoint(t.Context(), store)
			if err != nil {
				t.Fatal(err)
			}
			u, err = OpenPickerUpgrade(t.Context(), store, pickerDestination(), pickerCutover{}, checkpoint.Owner)
			if err != nil {
				t.Fatal(err)
			}
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
			reader, err := New(store)
			if err != nil {
				t.Fatal(err)
			}
			entry, _, err := reader.Find(t.Context(), input.SessionKey)
			if err != nil || entry == nil || entry.Picker == nil {
				t.Fatal(entry, err)
			}
		})
	}
}

func TestPickerUpgradeActivationHeadLostResponseResumesExactCandidate(t *testing.T) {
	t.Parallel()
	w, base := fixture(t)
	input := mutation(t, w, "head-loss")
	if _, err := w.Commit(t.Context(), input); err != nil {
		t.Fatal(err)
	}
	store := &lostStore{qualifiedStore: base}
	u, err := OpenPickerUpgrade(t.Context(), store, pickerDestination(), pickerCutover{}, "")
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
	if err = u.Activate(t.Context()); !errors.Is(err, ErrCommitUnknown) {
		t.Fatal(err)
	}
	resumed, err := OpenPickerUpgrade(t.Context(), store, pickerDestination(), pickerCutover{}, u.State.Owner)
	if err != nil {
		t.Fatal(err)
	}
	if err = resumed.Activate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = resumed.FinishActivation(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestPickerUpgradeRejectsLegacyNamespaceAndCorruptedSource(t *testing.T) {
	t.Parallel()
	t.Run("legacy", func(t *testing.T) {
		t.Parallel()
		_, s := fixture(t)
		if err := s.Put(t.Context(), "sessions/claude/legacy/metadata.json", []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenPickerUpgrade(t.Context(), s, pickerDestination(), pickerCutover{}, ""); err == nil {
			t.Fatal("claimed an incomplete legacy namespace")
		}
	})
	t.Run("corrupt", func(t *testing.T) {
		t.Parallel()
		w, s := fixture(t)
		input := mutation(t, w, "corrupt")
		if _, err := w.Commit(t.Context(), input); err != nil {
			t.Fatal(err)
		}
		if err := s.Put(t.Context(), input.Next.Metadata.Key, []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
		u, err := OpenPickerUpgrade(t.Context(), s, pickerDestination(), pickerCutover{}, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = u.Step(t.Context(), 2); !errors.Is(err, storage.ErrChecksumMismatch) {
			t.Fatal(err)
		}
		h, _, err := w.Head(t.Context())
		if err != nil || h.Protocol != 10 {
			t.Fatal(h, err)
		}
	})
}

func TestPickerUpgradeCheckpointRejectsUnknownAndMissingFields(t *testing.T) {
	t.Parallel()
	_, store := fixture(t)
	u, err := OpenPickerUpgrade(t.Context(), store, pickerDestination(), pickerCutover{}, "")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(u.State)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"source_etag", "candidate", "copied", "memberships"} {
		value := fields[field]
		delete(fields, field)
		probe, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		original := u.State
		if err = json.Unmarshal(probe, &original); err == nil || original.Owner != u.State.Owner {
			t.Fatal("accepted incomplete checkpoint", field, err)
		}
		fields[field] = value
	}
	fields["unknown_protocol"] = json.RawMessage(`12`)
	probe, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	var decoded PickerUpgradeState
	if err = json.Unmarshal(probe, &decoded); err == nil {
		t.Fatal("accepted foreign checkpoint authority")
	}
}
