package codex

import (
	"errors"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/codexmeta"
)

func TestHistoryRecordCapacityPressureRemainsRetryable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ref, _ := historyFile(t, dir, threadB, threadB, 0, map[string]any{"parent_thread_id": threadA}, strings.Repeat("safe text ", 2000))
	lookup := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &ref}}
	pass := historyPass(t, dir, lookup).(*relatedSourcePass)
	defer func() { _ = pass.Close() }()
	selection, err := pass.selectSource(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	ledger := pass.env.ReadBudget
	pressure := ledger.Available() - 6000
	if !ledger.Reserve(pressure) {
		t.Fatal("reserve")
	}
	_, err = pass.readHistorySelection(t.Context(), agentapi.ReadLimits{}, selection)
	if !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatalf("valid record under shared pressure must remain retryable: %v", err)
	}
	ledger.Release(pressure)
	snapshot, err := pass.Read(t.Context(), ref, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal("retry after pressure released", err)
	}
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCachedHistoryPrefixCapacityPressureRemainsRetryable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base, raw := historyFile(t, dir, threadA, threadA, 0, nil, strings.Repeat("safe text ", 2000))
	leaf, _ := historyFile(t, dir, threadB, threadB, 2, map[string]any{"forked_from_id": threadA, "forked_from_ordinal_exclusive": 2, "history_base": codexmeta.CodexHistoryPosition{RolloutID: threadA, EndOrdinal: 2, EndByteOffset: uint64(len(raw))}}, "own")
	lookup := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &leaf}, rollouts: map[string][]agentapi.SourceRef{threadA: {base}}}
	pass := historyPass(t, dir, lookup).(*relatedSourcePass)
	defer func() { _ = pass.Close() }()
	warm, err := pass.Read(t.Context(), leaf, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if err := warm.Close(); err != nil {
		t.Fatal(err)
	}
	if pass.files[base.Path].validated == nil {
		t.Fatal("prefix was not cached")
	}
	selection, err := pass.selectSource(t.Context(), leaf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pass.graph(t.Context(), selection.leaf, selection.set); err != nil {
		t.Fatal(err)
	}
	ledger := pass.env.ReadBudget
	pressure := ledger.Available() - 6000
	if !ledger.Reserve(pressure) {
		t.Fatal("reserve")
	}
	_, err = pass.readHistorySelection(t.Context(), agentapi.ReadLimits{}, selection)
	if !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatalf("cached valid prefix became a permanent record limit: %v", err)
	}
	_, err = pass.Read(t.Context(), leaf, agentapi.ReadLimits{RecordBytes: 8192})
	if !errors.Is(err, archive.ErrRecordTooLarge) || errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatalf("cached proof of configured oversize must remain permanent: %v", err)
	}
	ledger.Release(pressure)
	retry, err := pass.Read(t.Context(), leaf, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal("cached retry", err)
	}
	if err := retry.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestHistoryConfiguredRecordCeilingRemainsPermanent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ref, _ := historyFile(t, dir, threadB, threadB, 0, map[string]any{"parent_thread_id": threadA}, strings.Repeat("safe text ", 2000))
	pass := historyPass(t, dir, &historyLookup{thread: agentapi.CodexRolloutSet{Current: &ref}})
	defer func() { _ = pass.Close() }()
	_, err := pass.Read(t.Context(), ref, agentapi.ReadLimits{RecordBytes: 1024})
	if !errors.Is(err, archive.ErrRecordTooLarge) || errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatalf("configured record ceiling lost: %v", err)
	}
}

func TestHistoryScannerScratchPressureRemainsRetryable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ref, _ := historyFile(t, dir, threadB, threadB, 0, map[string]any{"parent_thread_id": threadA}, "own")
	pass := historyPass(t, dir, &historyLookup{thread: agentapi.CodexRolloutSet{Current: &ref}}).(*relatedSourcePass)
	defer func() { _ = pass.Close() }()
	selection, err := pass.selectSource(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	ledger := pass.env.ReadBudget
	pressure := ledger.Available()
	if !ledger.Reserve(pressure) {
		t.Fatal("reserve")
	}
	_, err = pass.readHistorySelection(t.Context(), agentapi.ReadLimits{}, selection)
	if !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatalf("scratch capacity refusal must remain pending: %v", err)
	}
	ledger.Release(pressure)
	snapshot, err := pass.Read(t.Context(), ref, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal("scratch retry", err)
	}
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
}
