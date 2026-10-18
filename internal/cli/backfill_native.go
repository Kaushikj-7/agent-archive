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
	"github.com/wangjohn/agent-archive/internal/trace"
)

// buildBackfillPlan owns the same trusted lookup used by continuous capture.
func buildBackfillPlan(ctx context.Context, env Env, home, userHome string, cfg config.Config, filters backfill.Filters, progress func(backfill.PlanningProgress)) (plan backfill.Plan, err error) {
	return buildBackfillPlanWithLookup(ctx, env, home, userHome, cfg, filters, progress, foregroundCodexRollouts)
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
	native.InspectionCache = cache
	if !env.reviewBackfillProjects {
		writer := inspectioncache.NewWriter(ctx, cache)
		defer writer.Close()
		native.ObserveInspection = writer.Observe
	}
	native.Progress = progress
	native.ReviewProjects = env.reviewBackfillProjects
	native.CodexRollouts = lookup
	var incomplete *backfill.Diagnostic
	native.PrepareCodexProof = func(ctx context.Context, ids []string) error {
		if len(ids) == 0 {
			return nil
		}
		span := trace.Start("backfill native preparation")
		defer span.End()
		seen := map[string]bool{}
		for _, id := range ids {
			if seen[id] {
				continue
			}
			seen[id] = true
			trace.Add(trace.BackfillNativePreparationAttempts, 1)
			_, lookupErr := lookup.Thread(ctx, id)
			if lookupErr != nil && (agentapi.HasFailure(lookupErr, agentapi.Cleanup) || errors.Is(lookupErr, context.DeadlineExceeded) || lookupIOTimeout(lookupErr) || ctx.Err() != nil) {
				return lookupErr
			}
		}
		for {
			health, err := lookup.ObserveReadOnly(ctx)
			if err != nil {
				if ctx.Err() == nil && lookupNoProgressOnly(err) {
					// Inventory is observation, not per-source authority. Stop once;
					// each source must still prove its own current native history.
					incomplete = &backfill.Diagnostic{Detail: backfill.DetailLookupNoProgress, Action: backfill.ActionRetry}
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
	plan, err = backfill.BuildPlan(ctx, native, newArchiveState(home, cfg), cfg, filters)
	if err == nil {
		plan.NativeLookupIncomplete = incomplete
	}
	return plan, err
}

// lookupNoProgressOnly permits no secondary failure to hide behind a no-progress
// cause. In particular joined cleanup, cancellation, capacity and I/O stay fatal.
func lookupNoProgressOnly(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if classified, ok := err.(agentapi.Classified); ok {
		kind, cause := classified.Classification()
		var nested agentapi.Classified
		return kind == agentapi.Unavailable && cause == agentapi.CauseLookupNoProgress && !errors.As(errors.Unwrap(err), &nested)
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !lookupNoProgressOnly(child) {
				return false
			}
		}
		return true
	}
	if single, ok := err.(interface{ Unwrap() error }); ok {
		return lookupNoProgressOnly(single.Unwrap())
	}
	return false
}

func lookupIOTimeout(err error) bool {
	var source *agentapi.SourceError
	return errors.As(err, &source) && source.Cause == agentapi.CauseLookupIOTimeout
}
