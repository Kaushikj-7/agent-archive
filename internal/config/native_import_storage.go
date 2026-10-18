package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/wangjohn/agent-archive/internal/local"
)

// NativeImportStoragePreparation binds only an initial historical stage's
// preparation to already-sticky configuration. It cannot convert to an ordinary
// DurableStorageGuard, and does not imply that hooks.lock remains held.
type NativeImportStoragePreparation struct{ scope *nativeImportStorageScope }
type nativeImportStorageScope struct {
	home   *local.RootedHome
	path   string
	config os.FileInfo
	active atomic.Bool
}

// CheckHome verifies this callback lifetime, rooted identity and exact config.
func (p NativeImportStoragePreparation) CheckHome(home string) error {
	if p.scope == nil || !p.scope.active.Load() {
		return errors.New("native import storage preparation expired")
	}
	path, err := filepath.Abs(home)
	if err != nil || path != p.scope.path {
		return errors.Join(errors.New("native import storage preparation belongs to another home"), err)
	}
	if err := p.scope.home.Check(); err != nil {
		return err
	}
	current, err := p.scope.home.Root.Lstat("config.json")
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(current, p.scope.config) || current.Size() != p.scope.config.Size() || !current.ModTime().Equal(p.scope.config.ModTime()) {
		return errors.Join(errors.New("native import configuration changed while preparing storage"), err)
	}
	return nil
}

// RootedHome is borrowed only until the preparation callback returns.
func (p NativeImportStoragePreparation) RootedHome(home string) (*local.RootedHome, error) {
	if err := p.CheckHome(home); err != nil {
		return nil, err
	}
	return p.scope.home, nil
}

// WithNativeImportStorage checks existing writer fences under short hooks, then
// releases hooks before any source verification, encoding, file sync or callback.
// It grants storage preparation only; state must separately prove final admission.
func WithNativeImportStorage(ctx context.Context, home string, reserve func() ReadReservation, write func(NativeImportStoragePreparation) error) (err error) {
	if ctx == nil || reserve == nil || write == nil {
		return errors.New("native import storage requires context, reservation and callback")
	}
	held, err := local.OpenRootedHome(home)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, held.Close()) }()
	reads := &captureConfigReads{ctx: ctx, reserve: reserve}
	defer reads.close()
	eligible := func(cfg Config, found bool) bool {
		return found && cfg.NativeImportProtection && cfg.NativeAuthorityProtection && cfg.ImportStagingProtection && cfg.StagedSelectionProtection && cfg.DurableStorageProtection && cfg.PublicationCompositionProtection && cfg.SupportsStagedSelection() && cfg.Archive.Enabled && !cfg.Paused
	}
	cfg, found, err := reads.load(held)
	if err != nil || !eligible(cfg, found) {
		return errors.Join(errors.New("native import storage requires existing strong writer protection"), err)
	}
	var stamp os.FileInfo
	err = func() error {
		unlock, err := local.RootedLockWait(held, "hooks.lock", time.Second)
		if err != nil {
			return err
		}
		defer unlock()
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := held.Root.Lstat("setup-transaction.json"); !errors.Is(err, os.ErrNotExist) {
			return errors.Join(errors.New("setup pending before native import storage"), err)
		}
		before, err := held.Root.Lstat("config.json")
		if err != nil {
			return err
		}
		cfg, found, err = reads.load(held)
		if err != nil || !eligible(cfg, found) {
			return errors.Join(errors.New("native import storage requires existing strong writer protection"), err)
		}
		if err := rootedConfigUnchanged(held, before); err != nil {
			return err
		}
		stamp = before
		return held.Check()
	}()
	if err != nil {
		return err
	}
	absolute, err := filepath.Abs(home)
	if err != nil {
		return err
	}
	scope := &nativeImportStorageScope{home: held, path: absolute, config: stamp}
	scope.active.Store(true)
	defer scope.active.Store(false)
	prep := NativeImportStoragePreparation{scope: scope}
	if err := prep.CheckHome(home); err != nil {
		return err
	}
	return write(prep)
}
