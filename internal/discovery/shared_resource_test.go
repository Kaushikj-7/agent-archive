package discovery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/codex"
	"github.com/wangjohn/agent-archive/internal/local"
)

func TestSourceAndLiveIndexShareChargeBeforeCopyAndRelease(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	db := hintDatabase(t, root, true)
	addHint(t, db, "thread", "rollout", time.Now())
	mainPath := filepath.Join(root, "state_5.sqlite")
	main, err := os.OpenFile(mainPath, os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := main.Truncate(96 << 20); err != nil {
		t.Fatal(err)
	}
	if err := main.Close(); err != nil {
		t.Fatal(err)
	}
	sourceDir := filepath.Join(root, "sessions")
	if err := os.MkdirAll(sourceDir, 0700); err != nil {
		t.Fatal(err)
	}
	id := "11111111-1111-4111-8111-111111111111"
	path := filepath.Join(sourceDir, "rollout-2026-10-01T12-00-00-"+id+".jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(file, "{\"type\":\"session_meta\",\"payload\":{\"id\":%q,\"cwd\":%q,\"timestamp\":\"2026-10-01T12:00:00Z\",\"source\":\"cli\",\"cli_version\":\"1.0.0\",\"originator\":\"codex_cli_rs\"}}\n", id, root); err != nil {
		t.Fatal(err)
	}
	row := make([]byte, 32<<10)
	for i := range len(row) - 1 {
		row[i] = ' '
	}
	row[len(row)-1] = '\n'
	for range 1025 {
		if _, err := file.Write(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	ledger := agentapi.NewNativeReadBudget(currentSnapshotLimit)
	// A restored local catalog remains an input owner while the real native
	// source and private index compete for the same logical data allowance.
	catalogPath := filepath.Join(root, "synthetic-catalog.json")
	if err := local.WriteCompact(catalogPath, catalog{Health: Health{Errors: []string{strings.Repeat("synthetic", 1<<20)}}}); err != nil {
		t.Fatal(err)
	}
	owner := &CodexRolloutLookup{readBudget: ledger}
	var restored catalog
	if err := owner.readCatalog(t.Context(), catalogPath, &restored); err != nil {
		t.Fatal(err)
	}
	catalogCharge := owner.catalogCharge + owner.restoredCharge
	source, err := (codex.SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{ReadBudget: ledger})
	if err != nil {
		t.Fatal(err)
	}
	captured, err := source.Read(t.Context(), agentapi.SourceRef{Path: path, Key: id}, agentapi.ReadLimits{RawBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	// The 32 MiB rollout is read through its descriptor: the source holds
	// its header and scratch, not the file's length.
	sourceCharge, _ := ledger.Charged()
	if sourceCharge-catalogCharge <= 0 || sourceCharge-catalogCharge > 1<<20 {
		t.Fatalf("native source charge is not what it holds: %d", sourceCharge-catalogCharge)
	}
	// Another holder (a session's record buffers and filtered output) takes
	// the rest the index copy would need.
	held := int64(32 << 20)
	if !ledger.Reserve(held) {
		t.Fatal("reserve held output")
	}
	before, _ := ledger.Charged()
	indexCopy, err := snapshotCurrentIndexBudget(t.Context(), root, nil, ledger)
	if indexCopy != nil {
		_ = indexCopy.close()
		t.Fatal("allocated an index indexCopy over shared source budget")
	}
	if agentapi.Failure(err) != agentapi.Limit {
		t.Fatal(err)
	}
	used, peak := ledger.Charged()
	if used != before || peak > currentSnapshotLimit {
		t.Fatalf("failed indexCopy leaked/exceeded shared charge: %d %d", used, peak)
	}
	ledger.Release(held)
	if err := captured.Close(); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	if used, _ := ledger.Charged(); used != catalogCharge {
		t.Fatalf("source pass charge leaked: %d", used)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if used, _ := ledger.Charged(); used != 0 {
		t.Fatalf("catalog charge leaked: %d", used)
	}
	ctx, cancel := context.WithCancel(t.Context())
	_, err = snapshotCurrentIndexBudget(ctx, root, func(stage string) {
		if stage == "generation" {
			cancel()
		}
	}, ledger)
	if err == nil {
		t.Fatal("cancelled indexCopy accepted")
	}
	if used, _ := ledger.Charged(); used != 0 {
		t.Fatalf("cancelled indexCopy charge leaked: %d", used)
	}
	indexCopy, err = snapshotCurrentIndexBudget(t.Context(), root, nil, ledger)
	if err != nil {
		t.Fatal(err)
	}
	if err := indexCopy.close(); err != nil {
		t.Fatal(err)
	}
	if used, peak := ledger.Charged(); used != 0 || peak > currentSnapshotLimit {
		t.Fatalf("successful indexCopy charge leaked/exceeded: %d %d", used, peak)
	}
}

func TestCurrentHeaderRequiresSharedScratchBeforeProbe(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	id := writeRollout(t, root, cfg.Archive.Projects[0].Root, at, 1, "sessions")
	path := filepath.Join(root, "sessions", "rollout-2026-10-01T12-00-00-"+id+".jsonl")
	lookup, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lookup.Close() }()
	held := lookup.readBudget.Available()
	if !lookup.readBudget.Reserve(held) {
		t.Fatal("reserve")
	}
	_, err = lookup.inspect(t.Context(), path, id)
	if agentapi.Failure(err) != agentapi.Limit || lookup.probes != 0 {
		t.Fatal("header read without reserved scratch", err, lookup.probes)
	}
	lookup.readBudget.Release(held)
	if _, err := lookup.inspect(t.Context(), path, id); err != nil {
		t.Fatal(err)
	}
	if used, _ := lookup.readBudget.Charged(); used != lookup.observationBytes || used == 0 {
		t.Fatal("header scratch not released to retained fact charge", used, lookup.observationBytes)
	}
	if err := lookup.Close(); err != nil {
		t.Fatal(err)
	}
	if used, _ := lookup.readBudget.Charged(); used != 0 {
		t.Fatal("fact charge leaked on close", used)
	}
}

func TestDiscoveryHeaderCannotReadWithExhaustedSharedScratch(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	writeRollout(t, root, cfg.Archive.Projects[0].Root, at, 1, "sessions")
	lookup, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lookup.Close() }()
	held := lookup.readBudget.Available()
	if !lookup.readBudget.Reserve(held) {
		t.Fatal("reserve")
	}
	health, err := run(t.Context(), store, cfg, Options{Now: func() time.Time { return at }, Rollouts: lookup}, syntheticSupport)
	lookup.readBudget.Release(held)
	if err != nil && !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatal(err)
	}
	if health.Probes != 0 || !health.Pending {
		t.Fatal("discovery read without scratch", health)
	}
}

func TestLookupRevisionDigestRefusesSharedPressureAndRetries(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	id := writeRollout(t, root, cfg.Archive.Projects[0].Root, at, 1, "sessions")
	lookup, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lookup.Close() }()
	if _, err := lookup.Thread(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	for pass := 0; lookup.coverage.Phase != coverageComplete && pass < 4; pass++ {
		if _, err := runWithAdapters(t.Context(), store, cfg, Options{Now: func() time.Time { return at }, Rollouts: lookup, inventoryOnly: true}, []SourceAdapter{codexAdapter{supported: syntheticSupport}}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := lookup.Thread(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Complete {
		t.Fatal("requested coverage did not converge")
	}
	// A complete observed attempt can still be owed to its consumer after
	// restart. A resource refusal must not make it eligible for rotation.
	request := lookup.coverage.Requests[id]
	request.DeliveredEpoch = 0
	lookup.coverage.Requests[id] = request
	owned, _ := lookup.readBudget.Charged()
	pressure := lookup.readBudget.Available()
	if !lookup.readBudget.Reserve(pressure) {
		t.Fatal("pressure")
	}
	if _, err := lookup.Thread(t.Context(), id); !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatal("cached thread digest bypassed shared pressure", err)
	}
	if err := lookup.Check(t.Context(), id, first.Revision); !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatal("comparison digest bypassed shared pressure", err)
	}
	if request := lookup.coverage.Requests[id]; request.DeliveredEpoch != 0 {
		t.Fatal("refusal delivered an unseen requested proof", request)
	}
	lookup.readBudget.Release(pressure)
	if used, _ := lookup.readBudget.Charged(); used != owned {
		t.Fatal("refused digest leaked", used, owned)
	}
	next, err := lookup.Thread(t.Context(), id)
	if err != nil || next.Revision != first.Revision {
		t.Fatal("retry changed source facts", next, err)
	}
	if err := lookup.Check(t.Context(), id, first.Revision); err != nil {
		t.Fatal(err)
	}
	if used, _ := lookup.readBudget.Charged(); used != owned {
		t.Fatal("successful digest leaked", used, owned)
	}
}
