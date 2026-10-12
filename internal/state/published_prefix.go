package state

import (
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/wangjohn/agent-archive/internal/local"
)

// publishedPrefix requires current configuration before a cheap prefix can
// supply authority. Future publication codecs put their version first; this
// reader does not inspect arbitrary later body fields after a healthy manual
// configuration downgrade with that mandatory header removed.
func (s *Store) publishedPrefix(id string) (summary PublishedSummary, found, head bool, info os.FileInfo, err error) {
	reservation := s.resourceBudget.Reservation()
	home, err := local.OpenRootedHome(s.home)
	if errors.Is(err, os.ErrNotExist) {
		return summary, false, false, nil, nil
	}
	if err != nil {
		return summary, true, false, nil, errors.Join(ErrDurableStorageRecovery, err)
	}
	defer func() {
		err = errors.Join(err, home.Close())
		if err != nil {
			summary, found, head = PublishedSummary{}, true, false
			err = errors.Join(ErrDurableStorageRecovery, err)
		}
	}()
	path := filepath.Join("published", id+".json")
	info, err = home.Root.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return summary, false, false, nil, nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return summary, true, false, info, errors.Join(ErrDurableStorageRecovery, err)
	}
	found = true
	cfgBefore, err := home.Root.Lstat("config.json")
	if err != nil {
		return summary, true, false, info, errors.Join(ErrDurableStorageRecovery, err)
	}
	_, present, err := s.loadDurableInspectionConfig(home)
	if err != nil || !present {
		return summary, true, false, info, errors.Join(ErrDurableStorageRecovery, err)
	}
	if err := s.durableContext().Err(); err != nil {
		return summary, true, false, info, err
	}
	f, err := home.Root.Open(path)
	if err != nil {
		return summary, true, false, info, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	opened, err := f.Stat()
	if err != nil || !sameDurableStamp(info, opened) {
		return summary, true, false, info, errors.Join(ErrDurableStorageRecovery, err)
	}
	if s.resourceBudget != nil {
		if !reservation.Reserve(16 << 10) {
			return summary, true, false, info, errStateBudget
		}
		defer reservation.Release(16 << 10)
	}
	summary, head, err = readLeadingSummary(io.LimitReader(f, 8192))
	if errors.Is(err, ErrDurableStorageRecovery) {
		return summary, true, false, info, err
	}
	// Malformed or older prefixes defer to the existing closed full decoder.
	err = nil
	named, fileErr := home.Root.Lstat(path)
	cfgAfter, cfgErr := home.Root.Lstat("config.json")
	if fileErr != nil || cfgErr != nil || !sameDurableStamp(info, named) || !sameDurableStamp(cfgBefore, cfgAfter) {
		return PublishedSummary{}, true, false, info, errors.Join(ErrDurableStorageRecovery, fileErr, cfgErr)
	}
	err = errors.Join(home.Check(), s.durableContext().Err())
	return summary, true, head, info, err
}
