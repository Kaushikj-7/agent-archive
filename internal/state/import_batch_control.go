package state

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

const importLocksDirectory = "import-locks"

// WithImportBatchLock serializes one batch's confirmation, undo marker and
// selecting rename. Callers must acquire this before any session request lock.
// It grants no source/registration authority and never takes the setup lock.
func WithImportBatchLock(ctx context.Context, home, id string, fn func(*local.RootedHome) error) (err error) {
	if !archive.ValidImportBatchID(id) {
		return archive.ErrUnsupportedNativeImport
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	held, err := local.OpenRootedHome(home)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, held.Close()) }()
	dir, err := privateDirectory(held.Root, importLocksDirectory, true)
	if err != nil {
		return err
	}
	if err := dir.Close(); err != nil {
		return err
	}
	name := filepath.Join(importLocksDirectory, id+".lock")
	unlock, err := local.RootedLockWait(held, name, time.Second)
	if err != nil {
		return err
	}
	defer unlock()
	info, err := held.Root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() != 0 {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	if err := errors.Join(ctx.Err(), held.Check()); err != nil {
		return err
	}
	if err := fn(held); err != nil {
		return err
	}
	return errors.Join(ctx.Err(), held.Check())
}

// ReplaceImportBatchControl prepares and syncs outside the batch lock. The
// supplied comparison observes current bounded control bytes under the lock,
// immediately before the existing rooted atomic writer's single rename.
func ReplaceImportBatchControl(ctx context.Context, home, id string, write func(io.Writer) error, compare func([]byte, bool) error) (err error) {
	if !archive.ValidImportBatchID(id) {
		return archive.ErrUnsupportedNativeImport
	}
	held, err := local.OpenRootedHome(home)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, held.Close()) }()
	dir, err := privateDirectory(held.Root, "imports", true)
	if err != nil {
		return err
	}
	if err := dir.Close(); err != nil {
		return err
	}
	name := filepath.Join("imports", id+".json")
	return local.RootedAtomicWriteChecked(held.Root, name, write, func(rename func() error) error {
		return WithImportBatchLock(ctx, home, id, func(locked *local.RootedHome) error {
			if err := held.Check(); err != nil {
				return err
			}
			raw, err := readImportBatchBytes(locked, id)
			found := !errors.Is(err, os.ErrNotExist)
			if err != nil && found {
				return err
			}
			if err := compare(raw, found); err != nil {
				return err
			}
			if err := errors.Join(ctx.Err(), held.Check(), locked.Check()); err != nil {
				return err
			}
			return rename()
		})
	})
}

func readImportBatchBytes(home *local.RootedHome, id string) (raw []byte, err error) {
	name := filepath.Join("imports", id+".json")
	info, err := home.Root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() < 0 || info.Size() > archive.MaxImportBatchControlBytes {
		return nil, ErrDurableStorageRecovery
	}
	f, err := home.Root.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) || !sameDurableStamp(info, opened) {
		return nil, errors.Join(ErrDurableStorageRecovery, err)
	}
	raw, err = io.ReadAll(io.LimitReader(f, archive.MaxImportBatchControlBytes+1))
	if err != nil || len(raw) != int(info.Size()) {
		return nil, errors.Join(ErrDurableStorageRecovery, err)
	}
	after, err := f.Stat()
	named, nameErr := home.Root.Lstat(name)
	if err != nil || nameErr != nil || !sameDurableStamp(info, after) || !sameDurableStamp(info, named) {
		return nil, errors.Join(ErrDurableStorageRecovery, err, nameErr)
	}
	return raw, home.Check()
}

// Synchronization files contain no authority or content. Validate each exact
// filename and zero-byte regular file, charging the shared scan entry ceiling.
func inspectImportLocks(ctx context.Context, home *local.RootedHome, remaining *int) (err error) {
	dir, err := privateDirectory(home.Root, importLocksDirectory, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, dir.Close()) }()
	f, err := dir.Open(".")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	for {
		entries, readErr := f.ReadDir(128)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		for _, entry := range entries {
			*remaining--
			if *remaining < 0 {
				return ErrDurableStorageRecovery
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			id, ok := strings.CutSuffix(entry.Name(), ".lock")
			info, err := dir.Lstat(entry.Name())
			if !ok || !archive.ValidImportBatchID(id) || err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() != 0 {
				return errors.Join(ErrDurableStorageRecovery, err)
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	return home.Check()
}
