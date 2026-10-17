package collector

import (
	"context"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"testing"
)

// Test output remains borrowed until test cleanup, using the actual shared transform.
func refilterBundle(t *testing.T, ctx context.Context, reg archive.SessionRegistration, adapter archive.Adapter, input archive.SourceBundle) (archive.SourceBundle, error) {
	t.Helper()
	return refilterBundleWithBudget(t, ctx, reg, adapter, input, agentapi.NewNativeReadBudget(128<<20))
}

func refilterBundleWithBudget(t *testing.T, ctx context.Context, reg archive.SessionRegistration, adapter archive.Adapter, input archive.SourceBundle, budget *agentapi.NativeReadBudget) (archive.SourceBundle, error) {
	t.Helper()
	if err := ctx.Err(); err != nil {
		return archive.SourceBundle{}, err
	}
	filter, ok := adapter.(agentapi.TranscriptFilter)
	if !ok {
		return archive.SourceBundle{}, errors.New("native refilter port required")
	}
	out, release, err := agentapi.RefilterRetainedSource(ctx, reg, filter, input, budget)
	if release != nil {
		t.Cleanup(release)
	}
	return out, err
}

// Native parent links may resolve after a retained revision was captured. The
// stable child owner still identifies that earlier evidence; a different known
// parent remains a conflict.
func retainedParentMatches(reg archive.SessionRegistration, bundle archive.SourceBundle) bool {
	return bundle.ParentSessionID == reg.ParentSessionID || nativeChildOwned(reg, bundle) && bundle.ParentSessionID == ""
}
