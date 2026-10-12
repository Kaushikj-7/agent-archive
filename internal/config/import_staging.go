package config

import (
	"errors"
	"time"

	"github.com/wangjohn/agent-archive/internal/local"
)

// WithImportStaging persists the sticky compatibility fence before evidence is
// retained. Setup and collector callers already hold their operation locks.
func WithImportStaging(home string, write func(DurableStorageGuard) error) error {
	if _, found, err := Load(home); err != nil {
		return err
	} else if !found {
		return errors.New("import staging requires existing configuration")
	}
	unlock, err := local.NamedLockWait(home, "hooks.lock", time.Second)
	if err != nil {
		return err
	}
	cfg, found, err := Load(home)
	if err == nil && !found {
		err = errors.New("import staging requires existing configuration")
	}
	if err == nil && !cfg.ImportStagingProtection {
		cfg.ImportStagingProtection = true
		err = Save(home, cfg)
	}
	unlock()
	if err != nil {
		return err
	}
	return WithDurableStorage(home, write)
}
