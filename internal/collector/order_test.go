package collector

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// Imported sessions with a pending request are processed first, oldest
// start first, then the rest in archive ID order. Progress reports each one; Stop ends
// the pass before the next session, leaving the rest pending.
func TestRunOrderProgressAndStop(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	local := newTestStore(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// IDs sort a..e; starts run the other way for the requested ones.
	starts := map[string]int{"a": 3, "b": 1, "c": 2, "d": 0, "e": 5}
	requested := map[string]bool{"a": true, "b": true, "c": true}
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		reg := registration(t, writeTranscript(t, dir, id+".jsonl", codexTranscript))
		reg.ArchiveSessionID, reg.NativeSessionID = id, "native-"+id
		// A backfill's queue: no live session claims the live lane.
		reg.Origin = archive.SessionOriginImport
		reg.SessionStartedAt = base.Add(-time.Duration(starts[id]) * 24 * time.Hour)
		if err := local.SaveRegistration(reg); err != nil {
			t.Fatal(err)
		}
		if requested[id] {
			if err := local.SaveRequest(id, "backfill", base); err != nil {
				t.Fatal(err)
			}
		}
	}
	now := base.Add(time.Hour)
	var order []string
	stopAfter := 0
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return now },
		Progress: func(p Progress) {
			order = append(order, fmt.Sprintf("%s:%t", p.ArchiveSessionID, p.Published))
		},
		Stop: func() bool { return stopAfter > 0 && len(order) >= stopAfter },
	}

	stopAfter = 2
	if _, err := Run(context.Background(), local, storagetest.NewMemoryStore(), opts); err != nil {
		t.Fatal(err)
	}
	if want := []string{"a:true", "c:true"}; !slices.Equal(order, want) {
		t.Fatalf("stopped pass processed %v, want %v", order, want)
	}
	status, _ := local.LoadStatus()
	if status.PendingCount != 1 {
		t.Fatalf("pending after the stop: %d, want b", status.PendingCount)
	}
	if want := map[string]int{"b": 0}; !maps.Equal(status.UnreachedSessions, want) {
		t.Fatalf("unreached after the stop: %v, want %v", status.UnreachedSessions, want)
	}
	if _, found, _ := local.LoadRequest("b"); !found {
		t.Fatal("b lost its request")
	}

	order, stopAfter = nil, 0
	local2 := newTestStore(t)
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		reg, _, _ := local.LoadRegistration(id)
		if err := local2.SaveRegistration(reg); err != nil {
			t.Fatal(err)
		}
		if requested[id] {
			if err := local2.SaveRequest(id, "backfill", base); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := Run(context.Background(), local2, storagetest.NewMemoryStore(), opts); err != nil {
		t.Fatal(err)
	}
	if want := []string{"a:true", "c:true", "b:true", "d:true", "e:true"}; !slices.Equal(order, want) {
		t.Fatalf("order %v, want %v", order, want)
	}
}

// Regression: 2026-10 a live Codex session sat at position 373 of 419 in
// the pass order, behind hundreds of native children whose parents were
// not registered and older sessions, while a pass reached 25 to 80 before
// its time limit; status said "waiting for its first upload" and sync said
// nothing. A pass now starts with the newest live session, takes ready
// sessions before children waiting for a parent, and records every session
// it ran out of time for with how many were ahead of it.
func TestRunReachesLiveSessionBehindBacklogAndRecordsUnreached(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	local := newTestStore(t)
	base := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	save := func(id string, startedDaysAgo int, child bool) {
		t.Helper()
		reg := registration(t, writeTranscript(t, dir, id+".jsonl", codexTranscript))
		reg.ArchiveSessionID, reg.NativeSessionID = id, "native-"+id
		reg.Origin = archive.SessionOriginDiscovery
		reg.SessionStartedAt = base.Add(-time.Duration(startedDaysAgo) * 24 * time.Hour)
		if child {
			// Its parent never registered, so it cannot be linked yet.
			reg.NativeChild, reg.ParentNativeSessionID = true, "native-unregistered-parent"
		}
		if err := local.SaveRegistration(reg); err != nil {
			t.Fatal(err)
		}
		if err := local.SaveRequest(id, "discovered", base); err != nil {
			t.Fatal(err)
		}
	}
	// The children are the oldest of all; the older top-level sessions
	// next; the live session started today.
	for i := range 12 {
		save(fmt.Sprintf("child-%02d", i), 30+i, true)
	}
	for i := range 4 {
		save(fmt.Sprintf("older-%d", i), 5-i, false)
	}
	save("live", 0, false)

	var order []string
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return base.Add(time.Hour) },
		Progress: func(p Progress) { order = append(order, p.ArchiveSessionID) },
		Stop:     func() bool { return len(order) >= 2 },
	}
	result, err := Run(context.Background(), local, storagetest.NewMemoryStore(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"live", "older-0"}; !slices.Equal(order, want) {
		t.Fatalf("the stopped pass processed %v, want %v", order, want)
	}
	if len(result.Unreached) != 3+12 {
		t.Fatalf("unreached = %v, want the 3 older sessions and 12 children", result.Unreached)
	}
	for i := range 3 {
		if ahead := result.Unreached[fmt.Sprintf("older-%d", i+1)]; ahead != i {
			t.Fatalf("older-%d has %d ahead, want %d", i+1, ahead, i)
		}
	}
	for i := range 12 {
		// The oldest child is the first child in the queue, behind the
		// three older top-level sessions.
		id := fmt.Sprintf("child-%02d", i)
		if ahead, found := result.Unreached[id]; !found || ahead != 3+11-i {
			t.Fatalf("%s has %d ahead (recorded %t), want %d", id, ahead, found, 3+11-i)
		}
	}
	status, err := local.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(status.UnreachedSessions, result.Unreached) {
		t.Fatalf("status unreached_sessions = %v, want %v", status.UnreachedSessions, result.Unreached)
	}
	if len(result.Errors) != 0 || len(result.Deferred) != 0 {
		t.Fatalf("an unreached session is neither failed nor deferred: errors %v, deferred %v", result.Errors, result.Deferred)
	}
	if status.PendingCount != len(result.Unreached) {
		t.Fatalf("pending = %d, want the %d unreached sessions", status.PendingCount, len(result.Unreached))
	}

	// A pass that reaches every session clears the record.
	order = nil
	opts.Stop = nil
	if result, err = Run(context.Background(), local, storagetest.NewMemoryStore(), opts); err != nil {
		t.Fatal(err)
	}
	if len(result.Unreached) != 0 {
		t.Fatalf("a pass that reached every session left %v unreached", result.Unreached)
	}
	if status, err = local.LoadStatus(); err != nil || status.UnreachedSessions != nil {
		t.Fatalf("status unreached_sessions after a full pass = %v, %v", status.UnreachedSessions, err)
	}
}

// The pass order: requested sessions first; among them a native child
// waiting for its parent last; the first slot of every four to the newest
// session that was not imported; the rest oldest start first.
func TestOrderRequestsReservesLiveLane(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	reg := func(id string, startedDaysAgo int, origin archive.SessionOrigin, waitsForParent bool) archive.SessionRegistration {
		return archive.SessionRegistration{ArchiveSessionID: id, Origin: origin, NativeChild: waitsForParent, SessionStartedAt: base.Add(-time.Duration(startedDaysAgo) * 24 * time.Hour)}
	}
	for _, tc := range []struct {
		name string
		regs []archive.SessionRegistration
		want []string
	}{
		{"imports alone keep oldest first", []archive.SessionRegistration{
			reg("i1", 1, archive.SessionOriginImport, false), reg("i5", 5, archive.SessionOriginImport, false),
			reg("i3", 3, archive.SessionOriginImport, false), reg("i4", 4, archive.SessionOriginImport, false),
			reg("i2", 2, archive.SessionOriginImport, false),
		}, []string{"i5", "i4", "i3", "i2", "i1"}},
		{"one slot in four for the newest live session", []archive.SessionRegistration{
			reg("i9", 9, archive.SessionOriginImport, false), reg("i8", 8, archive.SessionOriginImport, false),
			reg("i7", 7, archive.SessionOriginImport, false), reg("i6", 6, archive.SessionOriginImport, false),
			reg("i5", 5, archive.SessionOriginImport, false), reg("i4", 4, archive.SessionOriginImport, false),
			reg("h1", 1, archive.SessionOriginHook, false), reg("d0", 0, archive.SessionOriginDiscovery, false),
		}, []string{"d0", "i9", "i8", "i7", "h1", "i6", "i5", "i4"}},
		{"children waiting for a parent last, unrequested after them", []archive.SessionRegistration{
			reg("unrequested", 20, archive.SessionOriginHook, false),
			reg("c10", 10, archive.SessionOriginDiscovery, true), reg("c11", 11, archive.SessionOriginDiscovery, true),
			reg("h2", 2, archive.SessionOriginHook, false), reg("h3", 3, archive.SessionOriginHook, false),
		}, []string{"h2", "h3", "c11", "c10", "unrequested"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			requests := map[string]state.Request{}
			for _, r := range tc.regs {
				if r.ArchiveSessionID != "unrequested" {
					requests[r.ArchiveSessionID] = state.Request{ArchiveSessionID: r.ArchiveSessionID, Token: "t"}
				}
			}
			regs := slices.Clone(tc.regs)
			orderRequests(regs, requests)
			got := make([]string, 0, len(regs))
			for _, r := range regs {
				got = append(got, r.ArchiveSessionID)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("order %v, want %v", got, tc.want)
			}
		})
	}
}
