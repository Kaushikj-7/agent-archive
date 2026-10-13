package config

import (
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/local"
)

// ConfigReadReservation owns encoded-equivalent configuration views, not a heap bound.
type ConfigReadReservation interface {
	Reserve(int64) bool
	Release(int64)
	Available() int64
}

// LoadRootedOwned reads and validates complete config under a captured owner.
// The returned close must follow all use of the decoded configuration.
func LoadRootedOwned(ctx context.Context, home *local.RootedHome, owner ConfigReadReservation) (cfg Config, found bool, closeConfig func(), err error) {
	closeConfig = func() {}
	if ctx == nil || home == nil || home.Root == nil {
		err = errors.New("owned configuration requires context and rooted home")
		return
	}
	if err = ctx.Err(); err != nil {
		return
	}
	if err = home.Check(); err != nil {
		return
	}
	before, e := home.Root.Lstat("config.json")
	if errors.Is(e, os.ErrNotExist) {
		return
	}
	if e != nil || !before.Mode().IsRegular() {
		err = errors.Join(errors.New("rooted configuration must be a regular file"), e)
		return
	}
	n := before.Size()
	const scratch int64 = 32 << 10
	if owner == nil || n < 0 || n > (math.MaxInt64-scratch)/3 || !owner.Reserve(3*n+scratch) {
		err = agentapi.Wrap(agentapi.Limit, agentapi.ErrReadBudget)
		return
	}
	held := 3*n + scratch
	keep := false
	defer func() {
		if !keep {
			owner.Release(held)
		}
	}()
	file, e := home.Root.Open("config.json")
	var raw []byte
	allocated := n
	readErr := e
	var closeErr error
	if e == nil {
		raw, allocated, readErr = readOwnedConfigBody(ctx, file, n, owner)
		closeErr = file.Close()
	}
	held += 3 * (allocated - n)
	if e = rootedConfigReadUnchanged(home, before, raw, readErr); e != nil {
		err = errors.Join(e, closeErr)
		return
	}
	cfg, found, _, err = decodeLoadedConfig(raw, readErr, filepath.Join(home.Root.Name(), "config.json"), agentmeta.Builtins())
	err = errors.Join(err, home.Check(), ctx.Err(), closeErr)
	if err != nil {
		return
	}
	retained := int64(len(raw))
	owner.Release(held - retained)
	var once sync.Once
	closeConfig = func() { once.Do(func() { owner.Release(retained) }) }
	keep = true
	return
}

func readOwnedConfigBody(ctx context.Context, file io.Reader, initial int64, owner ConfigReadReservation) (raw []byte, allocated int64, err error) {
	allocated = initial
	raw = make([]byte, int(initial))
	used := 0
	for {
		if err = ctx.Err(); err != nil {
			return raw[:used], allocated, err
		}
		if used == len(raw) {
			var probe [1]byte
			count, e := file.Read(probe[:])
			if count == 0 {
				if errors.Is(e, io.EOF) {
					return raw[:used], allocated, nil
				}
				if e != nil {
					return raw[:used], allocated, e
				}
				continue
			}
			growth := min(int64(4096), owner.Available()/3)
			if growth <= 0 || allocated > math.MaxInt64-growth || !owner.Reserve(3*growth) {
				return raw[:used], allocated, agentapi.Wrap(agentapi.Limit, agentapi.ErrReadBudget)
			}
			next := make([]byte, int(allocated+growth))
			copy(next, raw)
			raw = next
			allocated += growth
			raw[used] = probe[0]
			used++
			if e != nil && !errors.Is(e, io.EOF) {
				return raw[:used], allocated, e
			}
			continue
		}
		count, e := file.Read(raw[used:])
		used += count
		if errors.Is(e, io.EOF) {
			return raw[:used], allocated, nil
		}
		if e != nil {
			return raw[:used], allocated, e
		}
	}
}

type captureConfigReads struct {
	ctx     context.Context
	reserve func() ConfigReadReservation
	closes  []func()
}

func (r *captureConfigReads) load(home *local.RootedHome) (Config, bool, error) {
	cfg, found, closeCfg, err := LoadRootedOwned(r.ctx, home, r.reserve())
	if err == nil {
		r.closes = append(r.closes, closeCfg)
	}
	return cfg, found, err
}

func (r *captureConfigReads) close() {
	for i := len(r.closes) - 1; i >= 0; i-- {
		r.closes[i]()
	}
	r.closes = nil
}

func loadOwnedSettledPreflight(r *captureConfigReads, home *local.RootedHome, deadline time.Time) (Config, bool, error) {
	var changed error
	for {
		if err := r.ctx.Err(); err != nil {
			return Config{}, false, err
		}
		if !time.Now().Before(deadline) {
			if changed != nil {
				return Config{}, false, changed
			}
			return Config{}, false, local.ErrBusy
		}
		cfg, found, err := r.load(home)
		if !errors.Is(err, errRootedConfigObservationChanged) {
			return cfg, found, err
		}
		changed = err
		timer := time.NewTimer(min(10*time.Millisecond, time.Until(deadline)))
		select {
		case <-r.ctx.Done():
			timer.Stop()
			return Config{}, false, r.ctx.Err()
		case <-timer.C:
		}
	}
}

// WithImportStagingOwned guards only already qualified settled Cursor config.
// It never promotes an unsupported floor; default staging remains unchanged.
func WithImportStagingOwned(ctx context.Context, home string, reserve func() ConfigReadReservation, write func(DurableStorageGuard) error) (err error) {
	if ctx == nil || reserve == nil || write == nil {
		return errors.New("owned import guard requires context, reservation and writer")
	}
	held, err := local.OpenRootedHome(home)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, held.Close()) }()
	reads := &captureConfigReads{ctx: ctx, reserve: reserve}
	defer reads.close()
	cfg, found, err := reads.load(held)
	if err != nil || !found || !cfg.SupportsSettledCursorSelection() {
		return errors.Join(errors.New("settled capture requires protected import configuration"), err)
	}
	unlock, err := local.RootedLockWait(held, "hooks.lock", time.Second)
	if err != nil {
		return err
	}
	cfg, found, err = reads.load(held)
	unlock()
	if err != nil || !found || !cfg.SupportsSettledCursorSelection() {
		return errors.Join(errors.New("settled capture requires protected import configuration"), err)
	}
	return withDurableStorageOwned(reads, held, home, write)
}

func withDurableStorageOwned(reads *captureConfigReads, held *local.RootedHome, home string, write func(DurableStorageGuard) error) (err error) {
	deadline := time.Now().Add(time.Second)
	cfg, present, err := loadOwnedSettledPreflight(reads, held, deadline)
	if err != nil || !present || !cfg.SupportsSettledCursorSelection() {
		return errors.Join(errors.New("durable storage requires an existing supported configuration"), err)
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return local.ErrBusy
	}
	unlock, err := local.RootedLockWait(held, "hooks.lock", remaining)
	if err != nil {
		return err
	}
	defer unlock()
	if _, e := held.Root.Lstat("setup-transaction.json"); !errors.Is(e, os.ErrNotExist) {
		return errors.New("setup pending before durable storage protection")
	}
	before, err := held.Root.Lstat("config.json")
	if err != nil {
		return errors.Join(errors.New("durable storage requires an existing configuration"), err)
	}
	cfg, found, err := reads.load(held)
	if err != nil || !found || !cfg.SupportsSettledCursorSelection() {
		return errors.Join(errors.New("durable storage requires an existing configuration"), err)
	}
	if err = rootedConfigUnchanged(held, before); err != nil {
		return err
	}
	// Qualified floor14 already requires both sticky fences. Promotion is refused.
	if err = reads.ctx.Err(); err != nil {
		return err
	}
	absolute, err := filepath.Abs(home)
	if err != nil {
		return err
	}
	current, err := held.Root.Lstat("config.json")
	if err != nil || !current.Mode().IsRegular() {
		return errors.Join(errors.New("durable storage configuration changed before guard"), err)
	}
	scope := &durableStorageScope{home: held, path: absolute, config: current}
	scope.active.Store(true)
	defer scope.active.Store(false)
	return write(DurableStorageGuard{scope: scope})
}
