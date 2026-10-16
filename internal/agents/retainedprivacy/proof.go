// Package retainedprivacy lends evidence of fixed retained-only privacy work.
// It grants no permission to observe native files or databases.
package retainedprivacy

import (
	"context"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/retainedprivacy/internal/sealed"
)

// Proof is minted only by the fixed production retained filter.
type Proof = sealed.Proof

// Request owns immutable expectations and retained input, never transform authority.
type Request = sealed.Request

// Policy names actual filter and supplemental-evidence policy.
type Policy = sealed.Policy

// Facts bind both immutable input and transformed output.
type Facts = sealed.Facts

// Expectation describes the actual host operation without granting proof authority.
type Expectation = sealed.Expectation

// FreezeRequestCaptured snapshots an existing scoped owner without borrowing
// later ambient credit. It grants only fixed retained transformation access.
func FreezeRequestCaptured(ctx context.Context, expected Expectation, original []byte, budget *agentapi.NativeReadBudget, owner *agentapi.NativeReadReservation) (*Request, error) {
	return sealed.FreezeRequestCaptured(ctx, expected, original, budget, owner)
}
