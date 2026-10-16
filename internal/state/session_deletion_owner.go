package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/wangjohn/agent-archive/internal/local"
	"os"
	"path/filepath"
)

// SessionDeletionPresent is a refusal-only guard. Any named work, including
// corrupt or interrupted work, defers relationship mutation without decoding
// selecting metadata or granting deletion/restoration authority.
func (s *Store) SessionDeletionPresent(id string) (present bool, err error) {
	if unsafeDurableSessionID(id) {
		return true, ErrDurableStorageRecovery
	}
	home, err := local.OpenRootedHome(s.home)
	if err != nil {
		return true, err
	}
	defer func() { err = errors.Join(err, home.Close()) }()
	return rootHasEntries(home.Root, filepath.Join(deletionDirectory, id))
}

// Registration transition checks need only canonical control ownership. Full
// replay independently validates immutable metadata and every source; hooks
// must not decode those potentially large inputs merely to refuse relocation.
func (s *Store) loadDeletionControl(id string) (j SessionDeletion, found bool, err error) {
	if unsafeDurableSessionID(id) {
		return j, true, ErrDurableStorageRecovery
	}
	home, err := local.OpenRootedHome(s.home)
	if err != nil {
		return j, true, err
	}
	defer func() { err = errors.Join(err, home.Close()) }()
	dir, err := privateDirectory(home.Root, filepath.Join(deletionDirectory, id), false)
	if errors.Is(err, os.ErrNotExist) {
		return j, false, nil
	}
	if err != nil {
		return j, true, err
	}
	defer func() { err = errors.Join(err, dir.Close()) }()
	raw, err := s.readDeletionObject(home, dir, "intent.json", deletionWireLimit)
	if errors.Is(err, os.ErrNotExist) {
		present, e := rootHasEntries(home.Root, filepath.Join(deletionDirectory, id))
		if e != nil || present {
			return j, true, errors.Join(ErrDurableStorageRecovery, e)
		}
		return j, false, nil
	}
	if err != nil {
		return j, true, err
	}
	reservation := s.readReservation()
	if !reservation.Reserve(2 * int64(len(raw))) {
		return j, true, errStateBudget
	}
	defer reservation.Release(2 * int64(len(raw)))
	if err := s.unmarshalOwned(raw, &j); err != nil {
		return j, true, errors.Join(ErrDurableStorageRecovery, err)
	}
	if err := j.validateControl(id); err != nil {
		return j, true, err
	}
	canonical, err := json.Marshal(j)
	if err != nil || !bytes.Equal(raw, append(canonical, '\n')) {
		return j, true, ErrDurableStorageRecovery
	}
	if err := validateDeletionEntries(dir, j); err != nil {
		return j, true, err
	}
	return j, true, home.Check()
}
