package cli

import (
	"context"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/discovery"
)

func nativeLocatorFactory(ctx context.Context, pass collector.NativeLocatorPass) (agentapi.NativeLocatorLookup, func() error, error) {
	inventory, err := discovery.NewNativeInventory(ctx, pass.Store.Home(), pass.Roots, pass.Queries, pass.ReadBudget, builtin.ValidateNativeLocator)
	if err != nil {
		return nil, nil, err
	}
	return inventory, inventory.Close, nil
}
