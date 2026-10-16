package muse

import (
	"context"
	"errors"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/sourceio"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

// SourceProvider supports verified local files only. Capture requires
// qualified admission and a durable binding, which this operation does not grant.
type SourceProvider struct{}

// Describe declares append-only file semantics without granting admission.
func (SourceProvider) Describe(ref agentapi.SourceRef) (agentapi.SourceSemantics, error) {
	return (sourceio.FileProvider{}).Describe(ref)
}

// OpenPass refuses capture before opening or filtering any native content.
func (SourceProvider) OpenPass(ctx context.Context, env agentapi.SourceEnvironment) (agentapi.SourcePass, error) {
	if err := localSourceEnvironment(ctx, env); err != nil {
		return nil, err
	}
	if env.Files == nil {
		env.Files = transcriptio.OS{}
	}
	return &sourcePass{env: env, live: map[*sourceSnapshot]bool{}}, nil
}

// OpenPreviewPass uses confined file snapshots without hashing uninspected bytes.
// It supplies no content proof and preserves the ordinary capture refusal.
func (SourceProvider) OpenPreviewPass(ctx context.Context, env agentapi.SourceEnvironment) (agentapi.SourcePass, error) {
	if err := localSourceEnvironment(ctx, env); err != nil {
		return nil, err
	}
	return (sourceio.FileProvider{}).OpenPass(ctx, env)
}

func localSourceEnvironment(ctx context.Context, env agentapi.SourceEnvironment) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if env.RequireConfinedHistory || env.NativeRoot != nil || env.LegacyUnboundRegistration {
		return agentapi.Wrap(agentapi.Unsafe, errors.Join(agentapi.ErrInvalidSourceBinding, errors.New("muse capture unavailable: qualified native source binding required; use handoff --file for local reading")))
	}
	return nil
}
