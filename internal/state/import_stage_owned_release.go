package state

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

// RemoveOwnedImportStage releases only the untyped stage selected by reg.
// The caller has verified the complete publication. The staging lock keeps a
// cooperating writer from replacing that exact owner between decoding and unlink.
// Typed stages retain their separate readback and release authority.
func (s *Store) RemoveOwnedImportStage(reg archive.SessionRegistration) (err error) {
	ctx := s.durableContext()
	if !safeFileComponent(reg.ArchiveSessionID) || !readyDigestValid(reg.StagedImportSHA256) {
		return ErrDurableStorageRecovery
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	home, err := local.OpenRootedHome(s.home)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, ctx.Err(), home.Check(), home.Close()) }()
	unlock, err := local.RootedLockWait(home, "hooks.lock", time.Second)
	if err != nil {
		return errors.Join(err, ctx.Err())
	}
	defer unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	before, err := home.Root.Lstat("config.json")
	if err != nil {
		return err
	}
	cfg, found, release, err := config.LoadRootedOwned(ctx, home, s.readReservation())
	defer release()
	if err != nil {
		return err
	}
	if !found || !cfg.ImportStagingProtection || !cfg.DurableStorageProtection {
		return ErrDurableStorageRecovery
	}
	if _, err := home.Root.Lstat("setup-transaction.json"); !errors.Is(err, os.ErrNotExist) {
		return ErrDurableStorageRecovery
	}
	current, found, err := s.LoadRegistration(reg.ArchiveSessionID)
	if err != nil {
		return err
	}
	if !found || current.StagedImportSHA256 != reg.StagedImportSHA256 || !sameImportStageOwner(reg, current) {
		return ErrDurableStorageRecovery
	}
	check := func() error {
		after, err := home.Root.Lstat("config.json")
		if err != nil || !sameDurableStamp(before, after) {
			return errors.Join(ErrDurableStorageRecovery, err)
		}
		return errors.Join(ctx.Err(), home.Check())
	}
	return s.removeOwnedImportStageAt(ctx, home, reg, current, check)
}

func (s *Store) removeOwnedImportStageAt(ctx context.Context, home *local.RootedHome, reg, current archive.SessionRegistration, check func() error) (err error) {
	if err := check(); err != nil {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	relative := filepath.Dir(importStageRelative(reg.ArchiveSessionID))
	root, err := privateDirectory(home.Root, relative, false)
	if errors.Is(err, os.ErrNotExist) {
		return check()
	}
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	info, err := root.Lstat("stage.json")
	if errors.Is(err, os.ErrNotExist) {
		return check()
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxPendingHistoryBytes {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	file, err := root.OpenFile("stage.json", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	opened, err := file.Stat()
	if err != nil || !sameDurableStamp(info, opened) {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	reservation := s.readReservation()
	if !reservation.Reserve(3 * info.Size()) {
		return errStateBudget
	}
	defer reservation.Release(3 * info.Size())
	if err := s.validateOwnedImportStage(ctx, reg, current, file, info.Size(), reservation); err != nil {
		return err
	}
	if err := checkOwnedImportStage(ctx, home, root, relative, info, check); err != nil {
		return err
	}
	if err := root.Remove("stage.json"); err != nil {
		return err
	}
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	if err := errors.Join(dir.Sync(), dir.Close()); err != nil {
		return err
	}
	return removeEmptyImportDirectory(home, relative)
}

func (s *Store) validateOwnedImportStage(ctx context.Context, reg, current archive.SessionRegistration, reader io.Reader, size int64, reservation stateReservation) error {
	counted := &countingReader{r: exactPendingContextReader{ctx: ctx, reader: io.LimitReader(reader, size+1)}}
	stage, err := s.decodePreparedImportStage(ctx, reg.ArchiveSessionID, counted, size, reservation)
	if err != nil {
		return err
	}
	if stage.TypedSelection {
		return ErrReadySelectionFrozen
	}
	if err := errors.Join(stage.validateOwner(reg), stage.validateOwner(current)); err != nil {
		return err
	}
	if counted.n != size {
		return ErrDurableStorageRecovery
	}
	return ctx.Err()
}

func checkOwnedImportStage(ctx context.Context, home *local.RootedHome, root *os.Root, relative string, info os.FileInfo, check func() error) error {
	named, err := root.Lstat("stage.json")
	openedDirectory, e := root.Stat(".")
	namedDirectory, f := home.Root.Lstat(relative)
	if err != nil || e != nil || f != nil || !sameDurableStamp(info, named) || !os.SameFile(openedDirectory, namedDirectory) {
		return errors.Join(ErrDurableStorageRecovery, err, e, f)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := check(); err != nil {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	return nil
}
