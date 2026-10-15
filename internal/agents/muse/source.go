package muse

import (
	"context"
	"errors"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/sourceio"
)

// SourceProvider supports verified explicit local files only. Capture requires
// qualified admission and a durable binding, which this operation does not grant.
type SourceProvider struct{}

// Describe declares append-only file semantics without granting admission.
func (SourceProvider) Describe(ref agentapi.SourceRef) (agentapi.SourceSemantics, error) {
	return (sourceio.FileProvider{}).Describe(ref)
}

// OpenPass refuses capture before opening or filtering any native content.
func (SourceProvider) OpenPass(ctx context.Context, env agentapi.SourceEnvironment) (agentapi.SourcePass, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if env.RequireConfinedHistory || env.NativeRoot != nil || env.LegacyUnboundRegistration {
		return nil, agentapi.Wrap(agentapi.Unsafe, errors.Join(agentapi.ErrInvalidSourceBinding, errors.New("muse capture unavailable: qualified native source binding required; use handoff --file for local reading")))
	}
	return (sourceio.FileProvider{}).OpenPass(ctx, env)
}
