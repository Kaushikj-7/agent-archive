package config

import (
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/local"
)

type durableStorageScope struct {
	home   *local.RootedHome
	path   string
	config os.FileInfo
	active atomic.Bool
}

// DurableStorageGuard binds a write to the current, durably protected
// configuration and held home. It expires when its lock scope ends.
type DurableStorageGuard struct{ scope *durableStorageScope }

var errRootedConfigObservationChanged = errors.New("rooted configuration changed while reading")

// CheckHome verifies that this live scope belongs to the named archive home.
func (g DurableStorageGuard) CheckHome(home string) error {
	if g.scope == nil || !g.scope.active.Load() {
		return errors.New("durable storage write guard expired")
	}
	absolute, err := filepath.Abs(home)
	if err != nil || absolute != g.scope.homePath() {
		return errors.New("durable storage write guard belongs to another home")
	}
	if err := g.scope.home.Check(); err != nil {
		return err
	}
	named, err := g.scope.home.Root.Lstat("config.json")
	if err != nil || !named.Mode().IsRegular() || !os.SameFile(g.scope.config, named) || g.scope.config.Size() != named.Size() || !g.scope.config.ModTime().Equal(named.ModTime()) {
		return errors.Join(errors.New("durable storage configuration changed while guarded"), err)
	}
	return nil
}

func (s *durableStorageScope) homePath() string { return s.path }

// RootedHome returns the held home only during the guarded callback. Callers
// borrow it and must not close it or retain it beyond the callback.
func (g DurableStorageGuard) RootedHome(home string) (*local.RootedHome, error) {
	if err := g.CheckHome(home); err != nil {
		return nil, err
	}
	return g.scope.home, nil
}

// WithDurableStorage saves the sticky writer fence before any new
// publication obligation. It holds hooks.lock, so nested callers pass its guard
// to private write helpers instead of reacquiring the lock.
func WithDurableStorage(home string, write func(DurableStorageGuard) error) (err error) {
	held, err := local.OpenRootedHome(home)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, held.Close()) }()
	// Reject unsupported/missing current config before creating even a lock file.
	// The protected config is loaded again under hooks; this preflight grants no witness.
	deadline := time.Now().Add(time.Second)
	_, present, err := loadDurableStoragePreflight(held, deadline)
	if err != nil || !present {
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
	configBefore, err := held.Root.Lstat("config.json")
	if err != nil {
		return errors.Join(errors.New("durable storage requires an existing configuration"), err)
	}
	cfg, found, err := LoadRooted(held)
	if err != nil || !found {
		return errors.Join(errors.New("durable storage requires an existing configuration"), err)
	}
	if err = rootedConfigUnchanged(held, configBefore); err != nil {
		return err
	}
	if !cfg.DurableStorageProtection {
		cfg.DurableStorageProtection = true
		if err = prepareDiscoveryConfig(&cfg); err != nil {
			return err
		}
		if err = held.Check(); err != nil {
			return err
		}
		if err = local.RootedWrite(held.Root, "config.json", cfg); err != nil {
			return err
		}
		configBefore, err = held.Root.Lstat("config.json")
		if err != nil {
			return err
		}
		verified, present, e := LoadRooted(held)
		if e != nil || !present || !verified.DurableStorageProtection || (verified.SchemaVersion < 7 || verified.SchemaVersion > 10) {
			return errors.Join(errors.New("durable storage protection was not persisted"), e)
		}
	}
	if err = held.Check(); err != nil {
		return err
	}
	if err = rootedConfigUnchanged(held, configBefore); err != nil {
		return err
	}
	absolute, e := filepath.Abs(home)
	if e != nil {
		return e
	}
	current, e := held.Root.Lstat("config.json")
	if e != nil || !current.Mode().IsRegular() {
		return errors.Join(errors.New("durable storage configuration changed before guard"), e)
	}
	scope := &durableStorageScope{home: held, path: absolute, config: current}
	scope.active.Store(true)
	defer scope.active.Store(false)
	return write(DurableStorageGuard{scope: scope})
}

func loadDurableStoragePreflight(home *local.RootedHome, deadline time.Time) (Config, bool, error) {
	var changed error
	for {
		if !time.Now().Before(deadline) {
			if changed != nil {
				return Config{}, false, changed
			}
			return Config{}, false, local.ErrBusy
		}
		cfg, found, err := LoadRooted(home)
		if !errors.Is(err, errRootedConfigObservationChanged) {
			return cfg, found, err
		}
		// Another writer may atomically install the floor before hooks is held.
		// Only that observation race can retry; every attempt validates the
		// current home and complete config before any lock allocation.
		changed = err
		remaining := time.Until(deadline)
		if remaining > 0 {
			time.Sleep(min(10*time.Millisecond, remaining))
		}
	}
}

// LoadRooted reads the current configuration through a caller-held archive home.
// It uses Load's decoder and validation without creating or changing a floor.
func LoadRooted(home *local.RootedHome) (cfg Config, found bool, err error) {
	if err = home.Check(); err != nil {
		return Config{}, false, err
	}
	before, err := home.Root.Lstat("config.json")
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, false, nil
	}
	if err != nil || !before.Mode().IsRegular() {
		return Config{}, false, errors.Join(errors.New("rooted configuration must be a regular file"), err)
	}
	raw, readErr := home.Root.ReadFile("config.json")
	if err := rootedConfigReadUnchanged(home, before, raw, readErr); err != nil {
		return Config{}, false, err
	}
	cfg, found, _, err = decodeLoadedConfig(raw, readErr, filepath.Join(home.Root.Name(), "config.json"), agentmeta.Builtins())
	return cfg, found, errors.Join(err, home.Check())
}

func rootedConfigReadUnchanged(home *local.RootedHome, before os.FileInfo, raw []byte, readErr error) error {
	if err := home.Check(); err != nil {
		return errors.Join(err, readErr)
	}
	after, err := home.Root.Lstat("config.json")
	if err != nil || !after.Mode().IsRegular() {
		return errors.Join(errors.New("rooted configuration unavailable after reading"), readErr, err)
	}
	if !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		if readErr != nil {
			return errors.Join(errors.New("rooted configuration read failed during change"), readErr)
		}
		// A changed observation never excuses an unsupported body we did read.
		// Reuse the complete decoder before making this race retryable.
		if _, _, _, err := decodeLoadedConfig(raw, nil, filepath.Join(home.Root.Name(), "config.json"), agentmeta.Builtins()); err != nil {
			return errors.Join(err, home.Check())
		}
		if err := home.Check(); err != nil {
			return err
		}
		return errRootedConfigObservationChanged
	}
	return nil
}

func rootedConfigUnchanged(home *local.RootedHome, before os.FileInfo) error {
	after, err := home.Root.Lstat("config.json")
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return errors.Join(errors.New("durable storage configuration changed before write guard"), err)
	}
	return home.Check()
}
