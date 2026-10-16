package cli

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/discovery"
	"github.com/wangjohn/agent-archive/internal/inspectioncache"
	"github.com/wangjohn/agent-archive/internal/state"
)

// buildBackfillPlan owns the same trusted lookup used by continuous capture.
func buildBackfillPlan(ctx context.Context, env Env, home, userHome string, cfg config.Config, filters backfill.Filters, progress func(backfill.PlanningProgress)) (plan backfill.Plan, err error) {
	return buildBackfillPlanWithLookup(ctx, env, home, userHome, cfg, filters, progress, passCodexRollouts)
}

// buildBackfillPlanWithLookup keeps preview resource ownership around every exit.
func buildBackfillPlanWithLookup(ctx context.Context, env Env, home, userHome string, cfg config.Config, filters backfill.Filters, progress func(backfill.PlanningProgress), openLookup func(context.Context, *state.Store, config.Config, Env) (*discovery.CodexRolloutLookup, []string, error)) (plan backfill.Plan, err error) {
	lookup, _, err := openLookup(ctx, state.OpenReadOnly(home), cfg, env)
	if err != nil {
		return plan, err
	}
	defer func() { err = errors.Join(err, lookup.CloseReadOnly()) }()
	native := env.backfillEnvironment(userHome, cfg)
	cache := inspectioncache.New(filepath.Join(home, "backfill-inspection-cache"))
	writer := inspectioncache.NewWriter(ctx, cache)
	defer writer.Close()
	native.InspectionCache, native.ObserveInspection = cache, writer.Observe
	native.Progress = progress
	native.CodexRollouts = lookup
	native.PrepareCodexProof = func(ctx context.Context, ids []string) error {
		if len(ids) == 0 {
			return nil
		}
		for _, id := range ids {
			_, lookupErr := lookup.Thread(ctx, id)
			if lookupErr != nil && (agentapi.HasFailure(lookupErr, agentapi.Cleanup) || ctx.Err() != nil) {
				return lookupErr
			}
		}
		for {
			health, err := lookup.ObserveReadOnly(ctx)
			if err != nil {
				if agentapi.HasFailure(err, agentapi.Limit) {
					return nil
				}
				return err
			}
			if !health.Pending {
				return nil
			}
		}
	}
	// SourcePass and rollout lookup ownership is serial. Other callers can retain
	// the ordinary parallel import inspector when they do not supply native lookup.
	native.Workers = 1
	return backfill.BuildPlan(ctx, native, newArchiveState(home, cfg), cfg, filters)
}
