package cli

import (
	"context"
	"os"
	"path/filepath"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/search"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
)

type searchChangeSink struct {
	home      string
	namespace string
}

func (s searchChangeSink) Hint(ctx context.Context, batch SessionChangeBatch) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// Nonblocking lifecycle admission: capture never waits for a worker or setup.
	release, err := local.NamedLock(s.home, "search-worker.lock")
	if err != nil {
		return nil
	}
	defer release()
	if setupjournal.TransactionPending(s.home) {
		return nil
	}
	cfg, found, err := config.Load(s.home)
	if err != nil || !found || !cfg.Archive.Enabled || !cfg.SearchIndexing || cfg.Paused || cfg.DestinationID() != batch.DestinationID {
		return nil
	}
	namespace, dir := contentSearchLocation(s.home, cfg)
	if namespace != s.namespace {
		return nil
	}
	identities := make([]search.SessionIdentity, 0, min(len(batch.Sessions), sessionChangeLimit))
	for _, candidate := range batch.Sessions[:min(len(batch.Sessions), sessionChangeLimit)] {
		identities = append(identities, search.SessionIdentity{Harness: candidate.Harness, SessionID: candidate.ArchiveSessionID})
	}
	return search.RecordSessionHints(ctx, dir, namespace, identities)
}

// searchHintDiagnostic emits one fixed, content-free line to a capped local log.
// No arbitrary sink error text or session identity is written to diagnostics.
func searchHintDiagnostic(ctx context.Context, home string) {
	if ctx.Err() != nil {
		return
	}
	path := filepath.Join(home, "search-error.log")
	info, err := os.Lstat(path)
	if err == nil && (!info.Mode().IsRegular() || info.Size() >= errorLogKeepBytes) {
		return
	}
	if err != nil && !os.IsNotExist(err) {
		return
	}
	// The lifecycle lock protects this write from teardown and never blocks.
	release, err := local.NamedLock(home, "search-worker.lock")
	if err != nil {
		return
	}
	defer release()
	if ctx.Err() != nil || setupjournal.TransactionPending(home) {
		return
	}
	cfg, found, err := config.Load(home)
	if err != nil || !found || !cfg.SearchIndexing || !cfg.Archive.Enabled || cfg.Paused {
		return
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return
	}
	_, _ = f.WriteString("search: publication hints unavailable; periodic reconciliation will retry\n")
	_ = f.Close()
}

func (e Env) changeConsumers(home, namespace string) []sessionChangeConsumer {
	if e.sessionChangeConsumers != nil {
		return e.sessionChangeConsumers
	}
	return []sessionChangeConsumer{{sink: searchChangeSink{home: home, namespace: namespace}, diagnostic: func(ctx context.Context, _ error) { searchHintDiagnostic(ctx, home) }}}
}
