package state

import (
	"bufio"
	"errors"
	"io"
	"os"
	"syscall"

	"github.com/wangjohn/agent-archive/internal/config"
)

// deletionInactive preserves legacy registration/read behavior until deletion
// opts in. This refusal-only projection reads no session content. Any deletion
// namespace or sticky capability continues through the private rooted guard.
func (s *Store) deletionInactive() (inactive bool, err error) {
	ctx := s.durableContext()
	defer func() {
		err = errors.Join(err, ctx.Err())
		if err != nil {
			inactive = false
		}
	}()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	before, err := os.Stat(s.home)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil || !before.IsDir() {
		return false, errors.Join(ErrDurableStorageRecovery, err)
	}
	root, err := os.OpenRoot(s.home)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(before, opened) {
		return false, errors.Join(ErrDurableStorageRecovery, err)
	}
	if _, err := root.Lstat(deletionDirectory); !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	info, err := root.Lstat("config.json")
	if errors.Is(err, os.ErrNotExist) {
		return true, deletionGateUnchanged(root, s.home, before, nil)
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > durableStorageQuota {
		return false, errors.Join(ErrDurableStorageRecovery, err)
	}
	required, err := s.readDeletionCapability(root, info)
	if err != nil {
		return false, err
	}
	return !required, deletionGateUnchanged(root, s.home, before, info)
}

func deletionGateUnchanged(root *os.Root, home string, before, configInfo os.FileInfo) error {
	after, err := os.Stat(home)
	if err != nil || !os.SameFile(before, after) {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	if _, err := root.Lstat(deletionDirectory); !errors.Is(err, os.ErrNotExist) {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	after, err = root.Lstat("config.json")
	if configInfo == nil && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || configInfo == nil || !sameDurableStamp(configInfo, after) {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	return nil
}

func (s *Store) readDeletionCapability(root *os.Root, before os.FileInfo) (required bool, err error) {
	ctx := s.durableContext()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	f, err := root.OpenFile("config.json", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	opened, err := f.Stat()
	if err != nil || !sameDurableStamp(before, opened) {
		return false, errors.Join(ErrDurableStorageRecovery, err)
	}
	reservation := s.readReservation()
	buffer := min(max(before.Size(), 16), pendingFenceBuffer)
	// The additional fixed control allowance owns decoder maps and selector
	// bookkeeping independently of the streaming input buffer.
	scratch := buffer + pendingFenceBuffer
	if !reservation.Reserve(scratch) {
		return false, errStateBudget
	}
	defer reservation.Release(scratch)
	counted := &countingReader{r: exactPendingContextReader{ctx: ctx, reader: io.LimitReader(f, before.Size()+1)}}
	p := projector{in: bufio.NewReaderSize(counted, int(buffer)), reserve: reservation.Reserve, limit: int(durableControlBytes / 2), ctx: ctx.Err}
	defer func() { reservation.Release(p.charged) }()
	if err := p.object(jsonProjection{"schema_version": nil, "deletion_protection": nil}, true); err != nil {
		return false, err
	}
	if err := p.trailing(); err != nil {
		return false, err
	}
	after, err := f.Stat()
	if err != nil || !sameDurableStamp(before, after) || counted.n != before.Size() {
		return false, errors.Join(ErrDurableStorageRecovery, err)
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	n := 6 * int64(len(p.out))
	if !reservation.Reserve(n) {
		return false, errStateBudget
	}
	defer reservation.Release(n)
	required, err = config.DeletionGuardRequired(p.out)
	return required, errors.Join(err, ctx.Err())
}
