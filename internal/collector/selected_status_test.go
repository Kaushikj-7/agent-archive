package collector

import (
	"bytes"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestSelectedPassCountsRejectedUnknownObligations(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	reg := registration(t, writeTranscript(t, t.TempDir(), "held.jsonl", codexTranscript))
	reg.ArchiveSessionID = "held"
	if err := store.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	requestPath := filepath.Join(store.Home(), "requests", "held.json")
	request := []byte(`{"archive_session_id":["unknown-shape"]}`)
	if err := os.WriteFile(requestPath, request, 0o600); err != nil {
		t.Fatal(err)
	}
	data := []byte("synthetic-frozen-orphan")
	ref := archive.SourceReference{SHA256: storage.SHA256Hex(data), Key: "synthetic", CompressedBytes: len(data)}
	stage, err := store.StagePendingSource("orphan", ref, data)
	if err != nil {
		t.Fatal(err)
	}
	remote := storagetest.NewMemoryStore()
	result, err := Run(t.Context(), store, remote, Options{
		MachineID: "synthetic", Sources: refuseNativeSources{t},
		AcceptSession: func(archive.SessionRegistration) bool { return false },
		SelectSession: func(archive.SessionRegistration) bool { return false },
	})
	defer result.CloseStagedResults()
	if err != nil || result.Scanned != 0 || len(result.Published) != 0 || len(result.Errors) != 2 || result.Errors["held"] == nil || !errors.Is(result.Errors["orphan"], state.ErrDurableStorageRecovery) {
		t.Fatal("unknown rejected obligations changed execution or diagnostics", result, err)
	}
	status, err := store.LoadStatus()
	if err != nil || status.PendingCount != 2 {
		t.Fatal("scoped census lost unknown request or durable orphan", status.PendingCount, err)
	}
	after, err := os.ReadFile(requestPath)
	if err != nil || !bytes.Equal(after, request) {
		t.Fatal("unknown request changed", err)
	}
	retained, err := store.ReadPendingSource("orphan", stage)
	if err != nil || !bytes.Equal(retained, data) {
		t.Fatal("orphan evidence changed", err)
	}
	objects, err := remote.List(t.Context(), "")
	if err != nil || len(objects) != 0 {
		t.Fatal("unknown obligations gained publication authority", objects, err)
	}
}

func TestSelectedPassPreservesUnselectedDeferralAndRetry(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	dir := t.TempDir()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	var regs []archive.SessionRegistration
	for _, id := range []string{"selected", "unrelated", "retrying"} {
		reg := registration(t, writeTranscript(t, dir, id+".jsonl", codexTranscript))
		reg.ArchiveSessionID, reg.NativeSessionID = id, "native-"+id
		reg.Origin = archive.SessionOriginImport
		if err := store.SaveRegistration(reg); err != nil {
			t.Fatal(err)
		}
		if err := store.SaveRequest(id, "synthetic-pending", now.Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
		regs = append(regs, reg)
	}
	clock := now.Add(-MaxDeferral - 30*time.Minute)
	next := clock.Add(MaxDeferral + time.Hour)
	previous := state.Status{
		LastScanAt: now.Add(-15 * time.Minute), PendingCount: 3,
		DeferredSince:     map[string]time.Time{"selected": clock, "unrelated": clock, "retrying": clock},
		DeferredSessions:  map[string]string{"selected": DeferredSourceChanged, "unrelated": DeferredSourceChanged, "retrying": DeferredLookupAllowance},
		NextRetryAt:       map[string]time.Time{"retrying": next},
		UnreachedSessions: map[string]int{"selected": 0, "unrelated": 1, "retrying": 2},
		SessionIssues:     map[string]string{"selected": "capture_failed", "unrelated": "storage_unavailable", "retrying": "capture_failed"},
		IssueCounts:       map[string]int{"capture_failed": 2, "storage_unavailable": 1},
		WaitingSubagents:  2, RunningSubagents: 1, PendingNativeLinks: 3,
		NativeLinkPendingReasons: map[string]int{"parent_registration_pending": 3},
	}
	if err := store.SaveStatus(previous); err != nil {
		t.Fatal(err)
	}
	remote := storagetest.NewMemoryStore()
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "synthetic", Now: func() time.Time { return now }, SelectSession: func(reg archive.SessionRegistration) bool { return reg.ArchiveSessionID == "selected" }}
	result, err := Run(t.Context(), store, remote, opts)
	defer result.CloseStagedResults()
	if err != nil || !slices.Equal(result.Published, []string{"selected"}) || len(result.Errors) != 0 {
		t.Fatal("selected source did not actually publish", result, err)
	}
	status, err := store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"unrelated", "retrying"} {
		if !status.DeferredSince[id].Equal(clock) || status.DeferredSessions[id] != previous.DeferredSessions[id] || status.UnreachedSessions[id] != previous.UnreachedSessions[id] || status.SessionIssues[id] != previous.SessionIssues[id] {
			t.Error("unselected owner's scheduling or diagnostic state changed", id, status)
		}
	}
	if !status.NextRetryAt["retrying"].Equal(next) || status.PendingCount != 2 || !maps.Equal(status.IssueCounts, map[string]int{"capture_failed": 1, "storage_unavailable": 1}) {
		t.Error("unselected retry or global owner counts changed", status)
	}
	if _, found := status.DeferredSince["selected"]; found || status.DeferredSessions["selected"] != "" || status.SessionIssues["selected"] != "" {
		t.Error("settled selected owner kept stale status", status)
	}
	if status.WaitingSubagents != previous.WaitingSubagents || status.RunningSubagents != previous.RunningSubagents || status.PendingNativeLinks != previous.PendingNativeLinks || !maps.Equal(status.NativeLinkPendingReasons, previous.NativeLinkPendingReasons) {
		t.Error("scoped work replaced unobserved whole-pass aggregates", status)
	}
	// A later ordinary pass must honor the original future retry, rather than
	// treating that unrelated owner as a fresh candidate after the scoped pass.
	opts.SelectSession = nil
	opts.Now = func() time.Time { return now.Add(time.Minute) }
	opts.Sources = failingReadSources{err: agentapi.Wrap(agentapi.Changed, errors.New("synthetic moving source"))}
	result, err = Run(t.Context(), store, remote, opts)
	defer result.CloseStagedResults()
	if err != nil || !result.RetryLater["retrying"].Equal(next) || slices.Contains(result.Published, "retrying") {
		t.Error("ordinary pass lost the unselected owner's retry hold", result, err)
	}
	if failure := result.Errors["unrelated"]; failure == nil || !strings.Contains(failure.Error(), DeferralExpiredProblem) {
		t.Error("ordinary source refusal lost the original 24-hour deferral clock", result)
	}
	_, requested, err := store.LoadRequest(regs[2].ArchiveSessionID)
	if err != nil || !requested {
		t.Fatal("unselected held request was acknowledged", requested, err)
	}
}

func TestSelectedRetryAttemptKeepsItsNewFutureSchedule(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	r := newSessionRunner{t: t, local: newTestStore(t), dir: t.TempDir(), start: now.Add(-7 * 24 * time.Hour)}
	reg := r.save(1, false)
	id := reg.ArchiveSessionID
	clock := now.Add(-MaxDeferral - 4*time.Hour)
	wholeScan := now.Add(-6 * time.Hour)
	if err := r.local.SaveStatus(state.Status{LastScanAt: wholeScan, DeferredSince: map[string]time.Time{id: clock}, NextRetryAt: map[string]time.Time{id: now.Add(-3 * time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	remote := storagetest.NewMemoryStore()
	opts := Options{Sources: failingReadSources{err: lookupAllowanceSpent()}, Parsers: testParsers, MachineID: "synthetic", SelectSession: func(archive.SessionRegistration) bool { return true }}
	run := func(at time.Time) Result {
		t.Helper()
		opts.Now = func() time.Time { return at }
		result, err := Run(t.Context(), r.local, remote, opts)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(result.CloseStagedResults)
		return result
	}
	result := run(now)
	if result.Errors[id] == nil {
		t.Fatal("selected due retry never attempted the failing source", result)
	}
	status, err := r.local.LoadStatus()
	if err != nil || !status.NextRetryAt[id].Equal(now.Add(3*time.Hour)) || !status.LastScanAt.Equal(wholeScan) {
		t.Fatal("selected attempt lost its actual next schedule or changed whole-pass clock", status, err)
	}
	for _, scoped := range []bool{true, false} {
		if !scoped {
			opts.SelectSession = nil
		}
		result := run(now.Add(time.Hour))
		if len(result.Errors) != 0 || !result.RetryLater[id].Equal(now.Add(3*time.Hour)) {
			t.Fatal("subsequent pass attempted a retry before its newly saved time", scoped, result)
		}
	}
}

func TestSelectedPassPreservesUntouchedWaiterNewcomerOrder(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	r := newSessionRunner{t: t, local: newTestStore(t), dir: t.TempDir(), start: now.Add(-7 * 24 * time.Hour)}
	selected, established, newcomer := r.save(1, false), r.save(2, false), r.save(3, false)
	wholeScan := now.Add(-time.Hour)
	if err := r.local.SaveStatus(state.Status{
		LastScanAt:        wholeScan,
		DeferredSince:     map[string]time.Time{established.ArchiveSessionID: now.Add(-2 * time.Hour), newcomer.ArchiveSessionID: wholeScan},
		UnreachedSessions: map[string]int{newcomer.ArchiveSessionID: 0, established.ArchiveSessionID: 1},
	}); err != nil {
		t.Fatal(err)
	}
	remote := storagetest.NewMemoryStore()
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "synthetic", Now: func() time.Time { return now }, SelectSession: func(reg archive.SessionRegistration) bool { return reg.ArchiveSessionID == selected.ArchiveSessionID }}
	result, err := Run(t.Context(), r.local, remote, opts)
	defer result.CloseStagedResults()
	if err != nil || !slices.Equal(result.Published, []string{selected.ArchiveSessionID}) {
		t.Fatal("intervening scoped publication did not run", result, err)
	}
	var order []string
	opts.SelectSession = nil
	opts.Now = func() time.Time { return now.Add(time.Minute) }
	opts.Progress = func(progress Progress) { order = append(order, progress.ArchiveSessionID) }
	opts.Stop = func() bool { return len(order) == 1 }
	result, err = Run(t.Context(), r.local, remote, opts)
	defer result.CloseStagedResults()
	if err != nil || !slices.Equal(order, []string{established.ArchiveSessionID}) || !slices.Equal(result.Published, order) {
		t.Fatal("scoped pass let an untouched newcomer jump the established waiter", order, result, err)
	}
}
