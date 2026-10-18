package cli

import (
	"context"
	"slices"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/discovery"
	"github.com/wangjohn/agent-archive/internal/state"
)

// passCodexRollouts owns one lookup for discovery and collector sources. Known
// native homes describe where facts may be read; creation permission continues
// to come independently from the actual original facts and current config.
func passCodexRollouts(ctx context.Context, store *state.Store, cfg config.Config, env Env) (*discovery.CodexRolloutLookup, []string, error) {
	homes, err := trustedCodexReadHomes(cfg, env)
	if err != nil {
		return nil, nil, err
	}
	lookup, err := discovery.NewCodexRolloutLookup(ctx, store, homes)
	return lookup, homes, err
}

func trustedCodexReadHomes(cfg config.Config, env Env) ([]string, error) {
	userHome, err := env.userHomeDir()
	if err != nil {
		return nil, err
	}
	homes := env.nativeSessionDirectories(userHome, cfg)["codex"]
	if cfg.Discovery != nil {
		for _, home := range cfg.Discovery.CodexHomes {
			if !slices.Contains(homes, home) {
				homes = append(homes, home)
			}
		}
	}
	return homes, nil
}

// foregroundCodexRollouts selects renewable work only for foreground planning.
// Confirmed admission and background consumers retain their existing policies.
func foregroundCodexRollouts(ctx context.Context, store *state.Store, cfg config.Config, env Env) (*discovery.CodexRolloutLookup, []string, error) {
	homes, err := trustedCodexReadHomes(cfg, env)
	if err != nil {
		return nil, nil, err
	}
	lookup, err := discovery.NewCodexRolloutLookupWithOptions(ctx, store, homes, discovery.CodexLookupOptions{Policy: discovery.ForegroundLookup, Now: env.nativeLookupNow})
	if err == nil && env.observeBackfillLookup != nil {
		env.observeBackfillLookup(lookup)
	}
	return lookup, homes, err
}
