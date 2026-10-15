package collector

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// This Run-only witness uses fields present on the actual parent. The same
// assertions distinguish the new waiter lane from the parent's oldest-first
// execution, and exercise the nonnil acceptance slice separately from the census.
func TestLookupWaiterLaneWithAcceptedExecutionAndStop(t *testing.T) {
	for _, filtered := range []bool{false, true} {
		name := "nil predicate"
		if filtered {
			name = "accepted queue"
		}
		t.Run(name, func(t *testing.T) {
			local := newTestStore(t)
			dir := t.TempDir()
			now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
			cfg := config.Config{}
			originals := map[string][]byte{}
			tokens := map[string]string{}
			save := func(id string, age time.Duration, rejected, child bool) {
				t.Helper()
				reg := registration(t, writeTranscript(t, dir, id+".jsonl", codexTranscript))
				reg.ArchiveSessionID, reg.NativeSessionID = id, "native-"+id
				reg.Origin = archive.SessionOriginImport
				reg.SessionStartedAt, reg.RegisteredAt = now.Add(-age), now.Add(-age)
				reg.NativeChild = child
				if child {
					reg.ParentNativeSessionID = "absent-parent"
				}
				if rejected {
					reg.DestinationID = "previous-destination"
				}
				if cfg.AcceptSession(reg) == rejected {
					t.Fatalf("acceptance fixture %s", id)
				}
				if err := local.SaveRegistration(reg); err != nil {
					t.Fatal(err)
				}
				if err := local.SaveRequest(id, "stop", now.Add(-time.Hour)); err != nil {
					t.Fatal(err)
				}
				request, found, err := local.LoadRequest(id)
				if err != nil || !found || request.Token == "" {
					t.Fatalf("request fixture %s: %+v %v", id, request, err)
				}
				tokens[id] = request.Token
				for _, kind := range []string{"registrations", "requests"} {
					if kind == "requests" && !rejected {
						continue
					}
					path := filepath.Join(local.Home(), kind, id+".json")
					raw, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					originals[path] = raw
				}
			}
			save("heavy", 4*time.Hour, false, false)
			save("waiter", 3*time.Hour, false, false)
			if filtered {
				save("rejected-top", 5*time.Hour, true, false)
				save("rejected-child", 6*time.Hour, true, true)
			}
			since := now.Add(-2 * time.Hour)
			if err := local.SaveStatus(state.Status{LastScanAt: now.Add(-time.Hour), DeferredSince: map[string]time.Time{"heavy": since, "waiter": since}, DeferredSessions: map[string]string{"heavy": "lookup_allowance"}, UnreachedSessions: map[string]int{"waiter": 0}}); err != nil {
				t.Fatal(err)
			}
			var order []string
			var acceptSession func(archive.SessionRegistration) bool
			if filtered {
				acceptSession = cfg.AcceptSession
			}
			opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "m", AcceptSession: acceptSession, Now: func() time.Time { return now }, Progress: func(p Progress) { order = append(order, p.ArchiveSessionID) }, Stop: func() bool { return len(order) == 1 }}
			result, err := Run(context.Background(), local, storagetest.NewMemoryStore(), opts)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(order, []string{"waiter"}) || !slices.Equal(result.Published, []string{"waiter"}) || result.Scanned != 1 || len(result.Errors) != 0 || len(result.Deferred) != 0 {
				t.Fatalf("waiter lane execution: order=%v result=%+v", order, result)
			}
			status, err := local.LoadStatus()
			rank, present := result.Unreached["heavy"]
			if err != nil || len(result.Unreached) != 1 || !present || rank != 0 || status.PendingCount != 1 || !status.DeferredSince["heavy"].Equal(since) || len(status.DeferredSince) != 1 || len(status.UnreachedSessions) != 1 || status.UnreachedSessions["heavy"] != 0 {
				t.Fatalf("accepted stopped suffix: result=%+v status=%+v err=%v", result, status, err)
			}
			for id, token := range tokens {
				if id == "waiter" {
					continue
				}
				request, found, err := local.LoadRequest(id)
				if err != nil || !found || request.Token != token {
					t.Fatalf("remaining request %s: %+v %t %v", id, request, found, err)
				}
			}
			for path, want := range originals {
				got, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("census/request bytes changed at %s: %v", path, err)
				}
			}
			regs, err := local.LoadRegistrations()
			if err != nil || len(regs) != len(tokens) {
				t.Fatalf("full census count: %d want %d: %v", len(regs), len(tokens), err)
			}
		})
	}
}
