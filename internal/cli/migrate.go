package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/destination"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

func runMigrateCommand(args []string, stdout, stderr io.Writer, env Env) int {
	fs := env.newCommandFlags("migrate", stderr)
	format := fs.String("format", "", "destination format (catalog-v4)")
	prefix := fs.String("prefix", "", "isolated destination prefix")
	bucket := fs.String("bucket", "", "isolated destination bucket")
	rollback := fs.Bool("rollback", false, "return to retained read-only legacy source, only if catalog has not changed")
	if !fs.parseFlagsOnly(args) {
		return 2
	}
	if *format != string(destination.FormatCatalogV4) || (*prefix == "" && *bucket == "") {
		terminal.Println(stderr, "agent-archive: migrate: --format catalog-v4 and an isolated --prefix or --bucket are required")
		return 2
	}
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
	targetConfig := cfg
	targetConfig.Storage.ArchiveFormat = destination.FormatCatalogV4
	if *prefix != "" {
		targetConfig.Storage.Prefix = *prefix
	}
	if *bucket != "" {
		targetConfig.Storage.Bucket = *bucket
	}
	ctx := context.Background()
	// Provider qualification and cutover authority are checked before acquiring
	// local locks, creating migration objects or changing any configuration.
	target, err := env.openStore(targetConfig)
	if err != nil {
		return fail(err)
	}
	authority, ok := target.(catalog.CutoverAuthority)
	if !ok {
		return fail(fmt.Errorf("reviewed credential cutover authority is unavailable; old write grants must be revoked and source kept read-only"))
	}
	sourceConfig := cfg
	if cfg.Storage.EffectiveArchiveFormat() == destination.FormatCatalogV4 {
		checkpoint, e := catalog.MigrationCheckpoint(ctx, target)
		if e != nil {
			return fail(e)
		}
		sourceConfig.Storage = checkpoint.Source
	}
	source, err := env.openStore(sourceConfig)
	if err != nil {
		return fail(err)
	}
	unlock, err := lockCollector(home, "migrate", env.now())
	if err != nil {
		return fail(err)
	}
	defer unlock()
	migration, err := catalog.OpenMigration(ctx, source, target, sourceConfig.Storage, targetConfig.Storage, authority)
	if err != nil {
		return fail(err)
	}
	if *rollback {
		if err = migration.Rollback(ctx); err != nil {
			return fail(err)
		}
		cfg.Storage = migration.State.Source
		cfg.Paused = true
		cfg.SchemaVersion = config.SchemaVersion
		if err = config.Save(home, cfg); err != nil {
			return fail(err)
		}
		terminal.Println(stdout, "Rolled back to the retained read-only source. Collection remains paused.")
		return 0
	}
	for {
		done, e := migration.Step(ctx)
		if e != nil {
			return fail(e)
		}
		if done {
			break
		}
	}
	if err = migration.Activate(ctx); err != nil {
		return fail(err)
	}
	cfg.Storage = targetConfig.Storage
	if err = config.Save(home, cfg); err != nil {
		return fail(err)
	}
	if err = migration.FinishActivation(ctx); err != nil {
		return fail(err)
	}
	terminal.Printf(stdout, "Activated catalog-v4 after exhaustive source and history verification (%d sessions). The original destination remains read-only.\n", migration.State.Copied)
	return 0
}
