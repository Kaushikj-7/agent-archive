package state

import (
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/wangjohn/agent-archive/internal/local"
)

// Hold each owning directory and regular-file identity before local forget.
// A corrupt later directory must refuse before an earlier record is unlinked.
type deletionRecords struct {
	root       *os.Root
	held       *local.RootedHome
	home       string
	dirs       map[string]*os.Root
	files      map[string]os.FileInfo
	syncParent func(*os.Root) error
}

func (s *Store) pinDeletionRecords(paths []string) (_ *deletionRecords, err error) {
	held, err := local.OpenRootedHome(s.home)
	if err != nil {
		return nil, err
	}
	root := held.Root
	r := &deletionRecords{root: root, held: held, home: s.home, dirs: map[string]*os.Root{".": root}, files: map[string]os.FileInfo{}}
	r.syncParent = syncDurableParent
	if s.deletionRecordsSync != nil {
		r.syncParent = s.deletionRecordsSync
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, r.close())
		}
	}()
	for _, path := range paths {
		rel, e := filepath.Rel(s.home, path)
		if e != nil || rel == "." || !local.PathWithin(path, s.home) {
			return nil, ErrDurableStorageRecovery
		}
		directory := filepath.Dir(rel)
		dir, e := r.directory(directory)
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			return nil, e
		}
		info, e := dir.Lstat(filepath.Base(rel))
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			return nil, e
		}
		if !info.Mode().IsRegular() {
			return nil, ErrDurableStorageRecovery
		}
		r.files[rel] = info
	}
	return r, nil
}

func (r *deletionRecords) directory(path string) (*os.Root, error) {
	if dir := r.dirs[path]; dir != nil {
		return dir, nil
	}
	parent, err := r.directory(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	dir, err := privateDirectory(parent, filepath.Base(path), false)
	if err != nil {
		return nil, err
	}
	r.dirs[path] = dir
	return dir, nil
}

func (r *deletionRecords) currentHome() error {
	if err := r.held.Check(); err != nil {
		return err
	}
	for path, dir := range r.dirs {
		if path == "." {
			continue
		}
		parent := r.dirs[filepath.Dir(path)]
		named, err := parent.Lstat(filepath.Base(path))
		if err != nil {
			return err
		}
		opened, err := dir.Stat(".")
		if err != nil || !named.IsDir() || named.Mode()&os.ModeSymlink != 0 || !os.SameFile(named, opened) {
			return ErrDurableStorageRecovery
		}
	}
	return nil
}

func (r *deletionRecords) remove(path string) error {
	if err := r.currentHome(); err != nil {
		return err
	}
	rel, err := filepath.Rel(r.home, path)
	if err != nil {
		return err
	}
	dir, err := r.directory(filepath.Dir(rel))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	name := filepath.Base(rel)
	info, err := dir.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	before := r.files[rel]
	if before == nil || !info.Mode().IsRegular() || !os.SameFile(before, info) || before.Size() != info.Size() || !before.ModTime().Equal(info.ModTime()) {
		return ErrDurableStorageRecovery
	}
	return dir.Remove(name)
}

func (r *deletionRecords) removeEmptySession(id string) error {
	dir, err := r.directory(filepath.Join("sessions", id))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	file, err := dir.Open(".")
	if err != nil {
		return err
	}
	entries, err := file.ReadDir(1)
	if errors.Is(err, io.EOF) {
		err = nil
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return nil
	}
	parent := r.dirs["sessions"]
	info, err := parent.Lstat(id)
	if err != nil {
		return err
	}
	held, err := dir.Stat(".")
	if err != nil {
		return err
	}
	if !info.IsDir() || !os.SameFile(info, held) {
		return ErrDurableStorageRecovery
	}
	return parent.Remove(id)
}

func (r *deletionRecords) close() (err error) {
	for path, dir := range r.dirs {
		err = errors.Join(err, r.syncParent(dir))
		if path != "." {
			err = errors.Join(err, dir.Close())
		}
	}
	err = errors.Join(err, r.held.Check(), r.held.Close())
	return err
}
