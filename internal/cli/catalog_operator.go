package cli

import (
	"context"
	"errors"
	"flag"
	"io"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/destination"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

type catalogOperatorAction string

const (
	catalogProbe       catalogOperatorAction = "probe"
	catalogCollect     catalogOperatorAction = "collect"
	catalogRecover     catalogOperatorAction = "recover"
	catalogRecoverSeal catalogOperatorAction = "recover-seal"
)

// runCatalogOperator is the explicit provider-evidence and fenced maintenance
// entry point. It never migrates, activates, or qualifies a destination.
func runCatalogOperator(args []string, stdout, stderr io.Writer, env Env) int {
	if len(args) < 1 {
		terminal.Println(stderr, "agent-archive: _catalog: choose probe, collect, recover, or recover-seal")
		return 2
	}
	action, options := catalogOperatorAction(args[0]), args[1:]
	if action != catalogProbe && action != catalogCollect && action != catalogRecover && action != catalogRecoverSeal {
		terminal.Println(stderr, "agent-archive: _catalog: unknown action")
		return 2
	}
	fs := flag.NewFlagSet("_catalog "+string(action), flag.ContinueOnError)
	fs.SetOutput(stderr)
	var bucket, prefix, owner string
	var isolated bool
	var generation uint64
	switch action {
	case catalogProbe:
		fs.StringVar(&bucket, "bucket", "", "explicit isolated target bucket")
		fs.StringVar(&prefix, "prefix", "", "explicit .catalog-qualification/<name>/ target prefix")
		fs.BoolVar(&isolated, "isolated", false, "confirm this target is reserved for synthetic qualification observations")
	case catalogRecoverSeal:
		fs.Uint64Var(&generation, "generation", 0, "exact observed coordinator seal generation")
		fs.StringVar(&owner, "owner", "", "exact observed unleased seal owner")
	case catalogRecover:
		fs.StringVar(&owner, "owner", "", "exact observed GC lease owner")
	case catalogCollect:
		// Collection accepts no command options.
	}
	if err := fs.Parse(options); err != nil || fs.NArg() != 0 {
		return 2
	}
	if (action == catalogRecover || action == catalogRecoverSeal) && owner == "" {
		return catalogOperatorError(stderr, errors.New("exact observed GC lease owner is required"))
	}
	if action == catalogRecoverSeal && generation == 0 {
		return catalogOperatorError(stderr, errors.New("exact observed seal generation is required"))
	}
	home, err := env.readHome()
	if err != nil {
		return catalogOperatorError(stderr, err)
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		return catalogOperatorError(stderr, err)
	}
	if !found {
		return catalogOperatorError(stderr, errNotSetUp)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if action == catalogProbe {
		return runCatalogProbe(ctx, cfg, bucket, prefix, isolated, stdout, stderr, env)
	}
	if cfg.Storage.EffectiveArchiveFormat() != destination.FormatCatalogV4 {
		return catalogOperatorError(stderr, errors.New("catalog maintenance requires a catalog-v4 destination"))
	}
	remote, err := env.openStoreContext(ctx, cfg)
	if err != nil {
		return catalogOperatorError(stderr, err)
	}
	adapter, ok := remote.(*catalog.Store)
	if !ok {
		return catalogOperatorError(stderr, errors.New("catalog maintenance requires qualified catalog authority"))
	}
	if action == catalogRecoverSeal {
		if err = adapter.Writer.Coordinator().RecoverUnleasedSeal(ctx, owner, generation); err != nil {
			return catalogOperatorError(stderr, err)
		}
		terminal.Println(stdout, "Exact drained unleased seal recovered.")
		return 0
	}
	var barrier catalog.Barrier
	if action == catalogRecover {
		barrier, err = adapter.CatalogRecoveryBarrier(ctx, owner)
	} else {
		barrier, err = adapter.CatalogBarrier(ctx)
	}
	if err != nil {
		return catalogOperatorError(stderr, err)
	}
	if action == catalogRecover {
		err = adapter.Writer.RecoverGC(ctx, barrier, owner)
	} else {
		err = adapter.Writer.Collect(ctx, barrier)
	}
	if err != nil {
		return catalogOperatorError(stderr, err)
	}
	terminal.Println(stdout, "Catalog maintenance completed under the destination barrier.")
	return 0
}

func catalogProbeTarget(cfg config.Config, bucket, prefix string, isolated bool) (config.Config, error) {
	// Require a canonical, dedicated namespace rather than normalizing an
	// ambiguous request into authority over a different target.
	if !isolated || bucket == "" || strings.TrimSpace(bucket) != bucket || !strings.HasPrefix(prefix, ".catalog-qualification/") || !strings.HasSuffix(prefix, "/") {
		return cfg, errors.New("probe requires --isolated and explicit --bucket and --prefix .catalog-qualification/<name>/")
	}
	name := strings.TrimSuffix(strings.TrimPrefix(prefix, ".catalog-qualification/"), "/")
	if strings.Trim(name, ".") == "" || strings.ContainsAny(name, "/\\ \t\r\n") {
		return cfg, errors.New("qualification prefix must have one nonempty safe name")
	}
	activePrefix := strings.Trim(cfg.Storage.Prefix, "/")
	if activePrefix != "" {
		activePrefix += "/"
	}
	if bucket == cfg.Storage.Bucket && (strings.HasPrefix(prefix, activePrefix) || strings.HasPrefix(activePrefix, prefix)) {
		return cfg, errors.New("qualification target overlaps the configured archive destination")
	}
	cfg.Storage.Bucket = bucket
	cfg.Storage.Prefix = prefix
	cfg.Storage.ArchiveFormat = destination.FormatLegacy
	return cfg, nil
}

func catalogOperatorError(stderr io.Writer, err error) int {
	terminal.Printf(stderr, "agent-archive: _catalog: %v\n", err)
	return 1
}

func runCatalogProbe(ctx context.Context, cfg config.Config, bucket, prefix string, isolated bool, stdout, stderr io.Writer, env Env) int {
	target, err := catalogProbeTarget(cfg, bucket, prefix, isolated)
	if err != nil {
		return catalogOperatorError(stderr, err)
	}
	remote, err := env.openStoreContext(ctx, target)
	if err != nil {
		return catalogOperatorError(stderr, err)
	}
	if err = storage.ProbeConditionalSemantics(ctx, remote); err != nil {
		return catalogOperatorError(stderr, err)
	}
	terminal.Println(stdout, "Atomic conditional-write observation passed; provider clock and release qualification remain unproven. No destination was activated.")
	return 0
}
