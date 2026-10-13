package cli

import (
	"context"
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/local"
	"io"
	"os"

	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/destination"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

func runPickerUpgrade(stdout, stderr io.Writer, env Env, rollback bool) int {
	fail := func(err error) int { terminal.Printf(stderr, "agent-archive: migrate: %v\n", err); return 1 }
	home, err := env.readHome()
	if err != nil {
		return fail(err)
	}
	if setupjournal.TransactionPending(home) {
		return fail(fmt.Errorf("%s", recoveryPending(home)))
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		return fail(err)
	}
	if !found {
		return fail(errNotSetUp)
	}
	if cfg.Storage.EffectiveArchiveFormat() != destination.FormatCatalogV4 {
		return fail(errors.New("--scoped-picker requires an existing catalog-v4 destination"))
	}
	store, err := env.openStore(cfg)
	if err != nil {
		return fail(err)
	}
	authority, ok := store.(catalog.CutoverAuthority)
	if !ok {
		return fail(errors.New("reviewed protocol 11 credential cutover authority is unavailable"))
	}
	ctx := context.Background()
	checkpoint, err := catalog.PickerUpgradeCheckpoint(ctx, store)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return fail(err)
	}
	owner := checkpoint.Owner
	unlock, err := lockCollector(home, "scoped catalog upgrade", env.now())
	if err != nil {
		return fail(err)
	}
	defer unlock()
	if err = ensurePickerPendingDrained(home); err != nil {
		return fail(err)
	}
	upgrade, err := catalog.OpenPickerUpgrade(ctx, store, cfg.Storage, authority, owner)
	if err != nil {
		return fail(err)
	}
	if rollback {
		if err = upgrade.Rollback(ctx); err != nil {
			return fail(err)
		}
		cfg.ScopedPickerProtection = true
		cfg.Paused = true
		if err = config.Save(home, cfg); err != nil {
			return fail(err)
		}
		terminal.Println(stdout, "Retained the original fully indexed catalog under its permanent scoped writer fence. Publication remains sealed and collection is paused.")
		return 0
	}
	for {
		done, e := upgrade.Step(ctx, 64)
		if e != nil {
			return fail(e)
		}
		if done {
			break
		}
	}
	if err = upgrade.Activate(ctx); err != nil {
		return fail(err)
	}
	cfg.ScopedPickerProtection = true
	if err = config.Save(home, cfg); err != nil {
		return fail(err)
	}
	if err = upgrade.FinishActivation(ctx); err != nil {
		return fail(err)
	}
	terminal.Printf(stdout, "Activated complete scoped picker indexes (%d sessions).\n", upgrade.State.Copied)
	return 0
}

func ensurePickerPendingDrained(home string) (err error) {
	root, err := local.OpenRootedHome(home)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	for _, name := range []string{"pending", "publication-evidence"} {
		directory, e := root.Root.Open(name)
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			return e
		}
		names, readErr := directory.Readdirnames(1)
		closeErr := directory.Close()
		if len(names) > 0 {
			return fmt.Errorf("unresolved local %s remains; settle its exact original publication or recovery route before scoped upgrade", name)
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return root.Check()
}
