package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/nativefacts"
)

const cursorScratchLimit int64 = 128 << 20
const cursorScratchControl int64 = 64 << 10

// cursorScratchReceipt is private control, never a capture or selection proof.
// Existing durable writers already refuse these two roots across a restart.
type cursorScratchReceipt struct {
	Version int    `json:"version"`
	Owner   string `json:"owner"`
	Key     string `json:"key"`
	Token   string `json:"token"`
	Bytes   int64  `json:"bytes"`
}

// CursorScratch retains a rooted, durably reserved private image workspace.
// Close may be retried after failed cleanup; its receipt remains until deletion
// has completed and the parent barrier has succeeded. Copies must not be made.
type CursorScratch struct {
	mu          sync.Mutex
	store       *Store
	ctx         context.Context
	home        *local.RootedHome
	receipt     cursorScratchReceipt
	unlock      func()
	reservation stateReservation
	closed      bool
	reserved    bool
	ready       bool
}

// NewCursorScratch prepares one bounded private owner without creating scratch.
// The store must have a live captured read budget. Only Reserve allocates its
// durable receipt, before any caller may create the actual image.
func (s *Store) NewCursorScratch(ctx context.Context, key string) (*CursorScratch, error) {
	if ctx == nil || s.resourceBudget == nil || !safeFileComponent(key) {
		return nil, ErrDurableStorageRecovery
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	owner := s.readReservation()
	if !owner.Reserve(cursorScratchControl) {
		return nil, errStateBudget
	}
	home, err := local.OpenRootedHome(s.home)
	if err != nil {
		owner.Release(cursorScratchControl)
		return nil, err
	}
	token, err := local.ID()
	if err != nil {
		owner.Release(cursorScratchControl)
		return nil, errors.Join(err, home.Close())
	}
	return &CursorScratch{store: s, ctx: ctx, home: home, reservation: owner, receipt: cursorScratchReceipt{Version: 1, Owner: "cursor-live-wal", Key: key, Token: token}}, nil
}

// Root names this owner's private scratch directory; the name grants no proof.
func (r *CursorScratch) Root() string { return filepath.Join(r.store.home, r.scratchPath()) }
func (r *CursorScratch) scratchPath() string {
	return filepath.Join(string(scratchDurableRoot), r.receipt.Token)
}
func (r *CursorScratch) receiptPath() string {
	return filepath.Join(string(reservationDurableRoot), r.receipt.Token+".json")
}

const cursorScratchLockName = "cursor-live-scratch.lock"

// Err checks the original lifetime and archive-home identity.
func (r *CursorScratch) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return os.ErrClosed
	}
	return errors.Join(r.ctx.Err(), r.home.Check())
}

// Reserve persists the full image capacity before creating its private directory.
// Current quota rules conservatively permit only one scratch obligation at once;
// other durable writers refuse while its receipt or scratch remains present.
func (r *CursorScratch) Reserve(n int64) (err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.reserved || n <= 0 || n > cursorScratchLimit {
		return ErrDurableStorageCapacity
	}
	if err = errors.Join(r.ctx.Err(), r.home.Check()); err != nil {
		return err
	}
	err = config.WithDurableStorage(r.store.home, func(g config.DurableStorageGuard) (err error) {
		q, err := r.store.openDurableQuota(g)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, q.Close()) }()
		u, err := q.usage()
		if err != nil {
			return err
		}
		// Physical data plus control atomic-write headroom are reserved prospectively.
		if n+2*cursorScratchControl > durableStorageQuota-u.charged {
			return ErrDurableStorageCapacity
		}
		unlock, err := local.RootedLockWait(r.home, cursorScratchLockName, time.Second)
		if err != nil {
			return err
		}
		r.unlock = unlock
		r.receipt.Bytes = n
		dir, err := privateDirectory(r.home.Root, string(reservationDurableRoot), true)
		if err != nil {
			return err
		}
		if err = dir.Close(); err != nil {
			return err
		}
		// A failed sync/rename may still have made a durable obligation visible.
		// Retain ownership and let Close inspect/remove only our exact control path.
		r.reserved = true
		if err := local.RootedWrite(r.home.Root, r.receiptPath(), r.receipt); err != nil {
			return err
		}
		r.ready = true
		return nil
	})
	return err
}

// OpenWorkspace opens only this owner's private directory after reservation.
// Callers close every returned root and SQLite reader before Close.
func (r *CursorScratch) OpenWorkspace() (_ *os.Root, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || !r.ready {
		return nil, ErrDurableStorageRecovery
	}
	if err = errors.Join(r.ctx.Err(), r.home.Check()); err != nil {
		return nil, err
	}
	return privateDirectory(r.home.Root, r.scratchPath(), true)
}

// Close removes scratch before its receipt, keeping accounting across failures.
// Cancellation never prevents owned cleanup. A failed cleanup remains retryable.
func (r *CursorScratch) Close() (err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	if err = r.home.Check(); err != nil {
		return err
	}
	if r.reserved {
		err = config.WithDurableStorage(r.store.home, func(g config.DurableStorageGuard) (err error) {
			q, err := r.store.openDurableQuota(g)
			if err != nil {
				return err
			}
			defer func() { err = errors.Join(err, q.Close()) }()
			return removeCursorScratch(r.home, r.receipt)
		})
		if err != nil {
			return err
		}
	}
	if r.unlock != nil {
		r.unlock()
		r.unlock = nil
	}
	r.closed = true
	r.reservation.Release(cursorScratchControl)
	return r.home.Close()
}

func removeCursorScratch(home *local.RootedHome, receipt cursorScratchReceipt) (err error) {
	if err := checkCursorScratchReceipt(home, receipt); err != nil {
		return err
	}

	scratch := filepath.Join(string(scratchDurableRoot), receipt.Token)
	if info, e := home.Root.Lstat(scratch); e == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return ErrDurableStorageRecovery
		}
		// Refuse unexpected children; never recursively follow unrecognized contents.
		dir, e := privateDirectory(home.Root, scratch, false)
		if e != nil {
			return e
		}
		defer func() { err = errors.Join(err, dir.Close()) }()
		f, e := dir.Open(".")
		if e != nil {
			return e
		}
		names, e := f.Readdirnames(2)
		e = errors.Join(e, f.Close())
		if e != nil && !errors.Is(e, io.EOF) {
			return e
		}
		if len(names) > 1 || len(names) == 1 && names[0] != "native.db" {
			return ErrDurableStorageRecovery
		}
		if len(names) == 1 {
			info, e := dir.Lstat("native.db")
			if e != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > receipt.Bytes {
				return errors.Join(ErrDurableStorageRecovery, e)
			}
			if e = dir.Remove("native.db"); e != nil {
				return e
			}
			if e = syncDurableParent(dir); e != nil {
				return e
			}
		}
		if e = home.Root.Remove(scratch); e != nil {
			return e
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	if dir, e := privateDirectory(home.Root, string(scratchDurableRoot), false); e == nil {
		e = errors.Join(syncDurableParent(dir), dir.Close())
		if e != nil {
			return e
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	path := filepath.Join(string(reservationDurableRoot), receipt.Token+".json")
	if e := home.Root.Remove(path); e != nil && !errors.Is(e, os.ErrNotExist) {
		return e
	}
	dir, e := privateDirectory(home.Root, string(reservationDurableRoot), false)
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	return errors.Join(syncDurableParent(dir), dir.Close(), home.Check())
}

// RecoverCursorScratch removes only validated abandoned live-image obligations.
// Active cross-process owners, unknown controls and unclaimed scratch stay put.
// Recovery is bounded and leaves existing stage/publication data unchanged.
func (s *Store) RecoverCursorScratch(ctx context.Context) (err error) {
	if ctx == nil || s.resourceBudget == nil {
		return ErrDurableStorageRecovery
	}
	owner := s.readReservation()
	if !owner.Reserve(cursorScratchControl) {
		return errStateBudget
	}
	defer owner.Release(cursorScratchControl)
	return config.WithDurableStorage(s.home, func(g config.DurableStorageGuard) (err error) {
		q, err := s.openDurableQuota(g)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, q.Close()) }()
		dir, err := privateDirectory(q.home.Root, string(reservationDurableRoot), false)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, dir.Close()) }()
		entries, err := dir.Open(".")
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, entries.Close()) }()
		for count := 0; count < 64; count++ {
			if err = ctx.Err(); err != nil {
				return err
			}
			names, e := entries.Readdirnames(1)
			if errors.Is(e, io.EOF) {
				return nil
			}
			if e != nil {
				return e
			}
			name := names[0]
			receipt, e := readCursorScratchReceipt(dir, name)
			if e != nil {
				return e
			}
			unlock, e := local.RootedLockWait(q.home, cursorScratchLockName, 0)
			if e != nil {
				return e
			}
			e = removeCursorScratch(q.home, receipt)
			unlock()
			if e != nil {
				return e
			}
		}
		return ErrDurableStorageRecovery
	})
}

func validCursorScratchReceipt(name string, r cursorScratchReceipt) bool {
	return r.Version == 1 && r.Owner == "cursor-live-wal" && safeFileComponent(r.Key) && safeFileComponent(r.Token) && name == r.Token+".json" && r.Bytes > 0 && r.Bytes <= cursorScratchLimit
}

func decodeCursorScratchReceipt(raw []byte) (receipt cursorScratchReceipt, ok bool) {
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return receipt, false
	}
	seen := map[string]bool{}
	for d.More() {
		token, err = d.Token()
		if err != nil {
			return receipt, false
		}
		key, valid := token.(string)
		if !valid || seen[key] {
			return receipt, false
		}
		seen[key] = true
		var target any
		switch key {
		case "version":
			target = &receipt.Version
		case "owner":
			target = &receipt.Owner
		case "key":
			target = &receipt.Key
		case "token":
			target = &receipt.Token
		case "bytes":
			target = &receipt.Bytes
		default:
			return receipt, false
		}
		if d.Decode(target) != nil {
			return receipt, false
		}
	}
	token, err = d.Token()
	if err != nil || token != json.Delim('}') || len(seen) != 5 {
		return receipt, false
	}
	_, err = d.Token()
	return receipt, errors.Is(err, io.EOF)
}

func readCursorScratchReceipt(dir *os.Root, name string) (receipt cursorScratchReceipt, err error) {
	before, err := dir.Lstat(name)
	if err != nil {
		return receipt, err
	}
	if !before.Mode().IsRegular() || before.Size() < 0 || before.Size() > 4096 {
		return receipt, ErrDurableStorageRecovery
	}
	f, err := dir.Open(name)
	if err != nil {
		return receipt, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	held, err := f.Stat()
	if err != nil || !os.SameFile(before, held) {
		return receipt, errors.Join(ErrDurableStorageRecovery, err)
	}
	raw, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return receipt, err
	}
	after, err := dir.Lstat(name)
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || int64(len(raw)) != before.Size() {
		return receipt, errors.Join(ErrDurableStorageRecovery, err)
	}
	receipt, valid := decodeCursorScratchReceipt(raw)
	if !valid || !validCursorScratchReceipt(name, receipt) {
		return receipt, ErrDurableStorageRecovery
	}
	return receipt, nil
}

func checkCursorScratchReceipt(home *local.RootedHome, receipt cursorScratchReceipt) error {
	receiptDirectory, e := privateDirectory(home.Root, string(reservationDurableRoot), false)
	if e == nil {
		observed, readErr := readCursorScratchReceipt(receiptDirectory, receipt.Token+".json")
		closeErr := receiptDirectory.Close()
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			return errors.Join(readErr, closeErr)
		}
		if readErr == nil && observed != receipt {
			return errors.Join(ErrDurableStorageRecovery, closeErr)
		}
		if closeErr != nil {
			return closeErr
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}

	return nil
}

// NewLiveCaptureRequest binds only a real durable owner to the original frozen
// request's captured lease. Its private callbacks cannot be retargeted by callers.
func (s *Store) NewLiveCaptureRequest(original *nativefacts.CaptureRequest) (_ *nativefacts.LiveCaptureRequest, err error) {
	if original == nil || !original.MatchesBudget(s.resourceBudget) {
		return nil, ErrReadySelectionFrozen
	}
	captured, err := original.Borrow()
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			captured.Close()
		}
	}()
	reg, err := captured.Registration()
	if err != nil {
		return nil, err
	}
	scoped, closeScoped := s.WithReadBudget(captured.Context(), captured.Budget())
	defer closeScoped()
	scoped.resourceReservation = func() stateReservation { return captured.Reservation() }
	scratch, err := scoped.NewCursorScratch(captured.Context(), reg.ArchiveSessionID)
	if err != nil {
		return nil, err
	}
	bound, err := nativefacts.BindLiveCaptureRequest(original, captured, scratch.Reserve, scratch.OpenWorkspace, scratch.Close, scratch.Err)
	if err != nil {
		return nil, errors.Join(err, scratch.Close())
	}
	keep = true
	return bound, nil
}
