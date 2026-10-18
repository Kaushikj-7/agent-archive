package discovery

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/local"
)

// captureHomeAliases retains only aliases supplied by the trusted-home owner.
// Ordinary physical homes need no additional retained mapping. The existing
// metadata root fence binds each spelling to its constructor-approved identity.
func (l *CodexRolloutLookup) captureHomeAliases(ctx context.Context) error {
	var captureErr error
	l.roots = observeApprovedRoots(l.homes, func(home, root string) {
		if captureErr != nil || home == root || filepath.Clean(home) != home || len(home) > 4096 {
			return
		}
		if err := ctx.Err(); err != nil {
			captureErr = err
			return
		}
		if l.homeAliases == nil {
			// Reserve fixed backing slots before allocation. Each retained path
			// and FileInfo is charged separately before its mapping is appended.
			charge := int64(len(l.homes)) * metadataTypeSize[metadataRoot]()
			if charge > metadataBytes || !l.readBudget.ReserveUnscoped(charge) {
				captureErr = agentapi.Wrap(agentapi.Limit, agentapi.ErrReadBudget)
				return
			}
			l.homeAliasCharge = charge
			l.homeAliases = make([]metadataRoot, 0, len(l.homes))
		}
		charge := int64(len(root)) + 512
		if l.homeAliasCharge+charge > metadataBytes || !l.readBudget.ReserveUnscoped(charge) {
			captureErr = agentapi.Wrap(agentapi.Limit, agentapi.ErrReadBudget)
			return
		}
		l.homeAliasCharge += charge
		l.homeAliases = append(l.homeAliases, metadataRoot{home: home, path: root})
	})
	if len(l.roots) > 1024 {
		return agentapi.Wrap(agentapi.Limit, errors.New("native home limit"))
	}
	if captureErr != nil {
		return captureErr
	}
	for i := range l.homeAliases {
		home := &l.homeAliases[i]
		if err := nativeUnit(ctx, func(context.Context) error {
			var err error
			home.info, err = os.Lstat(home.path)
			return err
		}); err != nil {
			if nativeInterrupted(err) {
				return err
			}
			return agentapi.Wrap(agentapi.Unavailable, err)
		}
		if !home.info.IsDir() {
			return agentapi.Wrap(agentapi.Changed, errors.New("native home identity changed"))
		}
	}
	return l.checkHomeAliases(ctx)
}

// checkHomeAliases runs even for cached selections: neither a cached locator nor
// an unchanged SQLite row proves that an original home spelling still agrees.
func (l *CodexRolloutLookup) checkHomeAliases(ctx context.Context) error {
	for _, home := range l.homeAliases {
		if err := nativeUnit(ctx, func(context.Context) error {
			if !home.current() {
				return agentapi.Wrap(agentapi.Changed, errors.New("native home identity changed"))
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// currentHomePath substitutes only an already-fenced trusted home prefix.
// Descendant spellings remain intact for the existing confined source opener.
func (l *CodexRolloutLookup) currentHomePath(root, path string) (string, bool) {
	if !filepath.IsAbs(path) || len(path) > 4096 || filepath.Clean(path) != path {
		return "", false
	}
	if l.homeFor(path) == root {
		return path, true
	}
	for _, home := range l.homeAliases {
		if home.path != root || !local.PathWithin(path, filepath.Join(home.home, "sessions")) && !local.PathWithin(path, filepath.Join(home.home, "archived_sessions")) {
			continue
		}
		relative, err := filepath.Rel(home.home, path)
		if err != nil {
			return "", false
		}
		path = filepath.Join(root, relative)
		return path, l.homeFor(path) == root
	}
	return "", false
}
