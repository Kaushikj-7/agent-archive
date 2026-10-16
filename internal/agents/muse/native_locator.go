package muse

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

// ValidateNativeLocator checks provisional strict startup through an injected
// counted confined opener. It returns no facts, consent, binding or currentness
// authority. The selected reader independently repeats admission and full Filter.
func ValidateNativeLocator(ctx context.Context, env agentapi.SourceEnvironment, ref agentapi.SourceRef, nativeID string) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	root := env.NativeRoot
	if root == nil || root.Validate() != nil || root.Agent != "muse" || env.Files == nil || env.ReadBudget == nil || env.Policy.Root != root.Canonical || !env.Policy.RejectSymlinks || ref.Key != nativeID || !ordinaryRootLocator(root.Canonical, ref.Path) || filepath.Base(filepath.Dir(ref.Path)) != nativeID {
		return agentapi.ErrInvalidSourceBinding
	}
	file, err := transcriptio.Open(env.Files, ref.Path, env.Policy)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	owner := &sourcePass{env: env}
	snapshot := &sourceSnapshot{owner: owner, ref: ref, file: file}
	facts, err := snapshot.startupFacts(ctx)
	if err != nil {
		return err
	}
	if facts.NativeID != nativeID {
		return agentapi.ErrInvalidSourceBinding
	}
	return file.Check()
}
