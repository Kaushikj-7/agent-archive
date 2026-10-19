package capture

import (
	"context"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"time"
)

// Legacy test names keep the historical cases exercising the same current
// implementation without production dead wrappers.
func ReplayAdmissionIntents(home string, now time.Time, lookups ...agentapi.DecodersLookup) error {
	return ReplayAdmissionIntentsWithin(context.Background(), home, now, lookups...)
}

func replayAdmissionIntents(home string, now time.Time, lookup agentapi.DecodersLookup, after func(effectName) error) error {
	return replayAdmissionIntentsWithin(context.Background(), home, now, lookup, after)
}
