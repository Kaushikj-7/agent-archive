package config

import (
	"crypto/sha256"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
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
func WithDurableStorage(home string, write func(DurableStorageGuard) error) error {
	return withPublicationStorage(home, false, write)
}

// PublicationCompositionGuard requires the durably saved composition floor.
// Its storage witness borrows the same held root and lock scope.
type PublicationCompositionGuard struct{ storage DurableStorageGuard }

// Storage derives the subordinate witness without acquiring another lock.
func (g PublicationCompositionGuard) Storage(home string) (DurableStorageGuard, error) {
	if err := g.storage.CheckHome(home); err != nil {
		return DurableStorageGuard{}, err
	}
	return g.storage, nil
}

// WithPublicationComposition persists source-set protocol5 protection before allocation.
func WithPublicationComposition(home string, write func(PublicationCompositionGuard) error) error {
	return withPublicationStorage(home, true, func(g DurableStorageGuard) error { return write(PublicationCompositionGuard{storage: g}) })
}

func withPublicationStorage(home string, composition bool, write func(DurableStorageGuard) error) error {
	return withPublicationStorageClosing(home, composition, write, func(held *local.RootedHome) error { return held.Close() })
}

func withPublicationStorageClosing(home string, composition bool, write func(DurableStorageGuard) error, closeHome func(*local.RootedHome) error) (err error) {
	held, err := local.OpenRootedHome(home)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, closeHome(held)) }()
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
	configBefore, err = ensurePublicationStorageProtection(held, cfg, composition, configBefore)
	if err != nil {
		return err
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
func LoadRooted(home *local.RootedHome) (Config, bool, error) {
	cfg, found, _, err := LoadRootedObserved(home)
	return cfg, found, err
}

// LoadRootedObserved returns the digest of the same supported bytes decoded by
// LoadRooted. It neither creates a protection floor nor supplies write authority.
func LoadRootedObserved(home *local.RootedHome) (cfg Config, found bool, digest [32]byte, err error) {
	return LoadRootedObservedBounded(home, math.MaxInt64)
}

// LoadRootedObservedBounded limits the exact observation before allocating or
// decoding it. A caller can reserve for an earlier file size without allowing a
// larger replacement to consume memory outside that reservation.
func LoadRootedObservedBounded(home *local.RootedHome, maxBytes int64) (cfg Config, found bool, digest [32]byte, err error) {
	if err = home.Check(); err != nil {
		return Config{}, false, digest, err
	}
	before, err := home.Root.Lstat("config.json")
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, false, digest, nil
	}
	if err != nil || !before.Mode().IsRegular() {
		return Config{}, false, digest, errors.Join(errors.New("rooted configuration must be a regular file"), err)
	}
	if before.Size() < 0 || before.Size() > maxBytes {
		return Config{}, false, digest, errors.New("rooted configuration exceeds the reserved read limit")
	}
	raw, readErr := readRootedConfigObserved(home, before)
	if err := rootedConfigReadUnchanged(home, before, raw, readErr); err != nil {
		return Config{}, false, digest, err
	}
	cfg, found, _, err = decodeLoadedConfig(raw, readErr, filepath.Join(home.Root.Name(), "config.json"), agentmeta.Builtins())
	err = errors.Join(err, home.Check())
	if err == nil {
		digest = sha256.Sum256(raw)
	}
	return cfg, found, digest, err
}

func readRootedConfigObserved(home *local.RootedHome, before os.FileInfo) (raw []byte, err error) {
	f, err := home.Root.OpenFile("config.json", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() {
		if e := f.Close(); e != nil {
			raw = nil
			err = errors.Join(err, e)
		}
	}()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || opened.Size() < 0 || opened.Size() > math.MaxInt-1 || !os.SameFile(before, opened) || before.Size() != opened.Size() || !before.ModTime().Equal(opened.ModTime()) {
		return nil, errors.Join(errRootedConfigObservationChanged, err)
	}
	raw = make([]byte, opened.Size())
	if _, err = io.ReadFull(f, raw); err != nil {
		return nil, err
	}
	var extra [1]byte
	n, e := f.Read(extra[:])
	if n != 0 || !errors.Is(e, io.EOF) {
		return nil, errors.Join(errRootedConfigObservationChanged, e)
	}
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil, errors.Join(errRootedConfigObservationChanged, err)
	}
	return raw, nil
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

func ensurePublicationStorageProtection(held *local.RootedHome, cfg Config, composition bool, configBefore os.FileInfo) (os.FileInfo, error) {
	if !cfg.DurableStorageProtection || (composition && !cfg.SourceSetPublicationProtection) {
		if composition {
			cfg.SourceSetPublicationProtection = true
		}
		cfg.DurableStorageProtection = true
		if err := prepareDiscoveryConfig(&cfg); err != nil {
			return nil, err
		}
		if err := held.Check(); err != nil {
			return nil, err
		}
		if err := local.RootedWrite(held.Root, "config.json", cfg); err != nil {
			return nil, err
		}
		var err error
		configBefore, err = held.Root.Lstat("config.json")
		if err != nil {
			return nil, err
		}
		verified, present, e := LoadRooted(held)
		if e != nil || !present || !verified.DurableStorageProtection || (verified.SchemaVersion < 7 || verified.SchemaVersion > 17 || verified.SchemaVersion == 14) || (composition && !verified.SourceSetPublicationProtection) {
			return nil, errors.Join(errors.New("durable storage protection was not persisted"), e)
		}
	}
	return configBefore, nil
}
