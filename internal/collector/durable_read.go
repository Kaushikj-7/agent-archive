package collector

import (
	"context"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
)

// Runtime passes initialize their shared ledger before these eligibility checks.
// Direct compatibility callers without a pass keep their explicit nil budget.
func checkDurableSessionRead(ctx context.Context, local *state.Store, id string, opts Options) error {
	var budget *agentapi.NativeReadBudget
	if opts.sourcePasses != nil {
		budget = opts.sourcePasses.env.ReadBudget
	}
	scratch, closeScratch := local.WithReadBudget(ctx, budget)
	defer closeScratch()
	return scratch.CheckDurableSessionRead(id)
}

func checkPublicationSessionRead(ctx context.Context, local *state.Store, reg archive.SessionRegistration, opts Options) error {
	var budget *agentapi.NativeReadBudget
	if opts.sourcePasses != nil {
		budget = opts.sourcePasses.env.ReadBudget
	}
	scratch, closeScratch := local.WithReadBudget(ctx, budget)
	defer closeScratch()
	scan := sessionScan{reg: reg}
	return scratch.CheckPublicationSessionRead(reg, opts.MachineID, scan.publicationAdmission())
}
