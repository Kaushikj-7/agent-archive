package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type liveLaneRejectionLayout string

const (
	liveLaneRejectNone  liveLaneRejectionLayout = "none"
	liveLaneRejectTop   liveLaneRejectionLayout = "top"
	liveLaneRejectChild liveLaneRejectionLayout = "child"
	liveLaneRejectMixed liveLaneRejectionLayout = "mixed"
	liveLaneRejectAll   liveLaneRejectionLayout = "all"
)

type liveLaneFixtureID string

const (
	liveLaneFixtureLive        liveLaneFixtureID = "live"
	liveLaneFixtureFirstImport liveLaneFixtureID = "i1"
)

func TestRunLiveLaneIgnoresRejectedRegistrations(t *testing.T) {
	t.Parallel()
	for _, rejected := range []liveLaneRejectionLayout{liveLaneRejectNone, liveLaneRejectTop, liveLaneRejectChild, liveLaneRejectMixed, liveLaneRejectAll} {
		t.Run(string(rejected), func(t *testing.T) {
			t.Parallel()
			dir, local := t.TempDir(), newTestStore(t)
			base := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
			cfg := config.Config{}
			save := func(id string, days int, imported, child, accepted bool) {
				t.Helper()
				reg := registration(t, writeTranscript(t, dir, id+".jsonl", codexTranscript))
				reg.ArchiveSessionID, reg.NativeSessionID = id, "native-"+id
				reg.Origin = archive.SessionOriginHook
				if imported {
					reg.Origin = archive.SessionOriginImport
				}
				if child {
					reg.ParentSessionID = "live"
				}
				reg.SessionStartedAt = base.Add(-time.Duration(days) * 24 * time.Hour)
				reg.DestinationID = cfg.DestinationID()
				if !accepted {
					reg.DestinationID = "previous-destination"
				}
				if cfg.AcceptSession(reg) != accepted {
					t.Fatalf("fixture acceptance for %s", id)
				}
				if err := local.SaveRegistration(reg); err != nil {
					t.Fatal(err)
				}
				if err := local.SaveRequest(id, "hook", base); err != nil {
					t.Fatal(err)
				}
			}
			for i, id := range []string{"i1", "i2", "i3"} {
				save(id, 5-i, true, false, rejected != liveLaneRejectAll)
			}
			save("live", 1, false, false, rejected != liveLaneRejectAll)
			var excluded []string
			if rejected == liveLaneRejectTop || rejected == liveLaneRejectMixed || rejected == liveLaneRejectAll {
				save("rejected-top", 0, false, false, false)
				excluded = append(excluded, "rejected-top")
			}
			if rejected == liveLaneRejectChild || rejected == liveLaneRejectMixed {
				save("rejected-child", 0, false, true, false)
				excluded = append(excluded, "rejected-child")
			}
			// Freeze exact surviving request tokens and raw registration/request bytes.
			preserved := map[string][]byte{}
			identities, err := local.LoadRegistrations()
			if err != nil {
				t.Fatal(err)
			}
			for _, reg := range identities {
				id := reg.ArchiveSessionID
				fixtureID := liveLaneFixtureID(id)
				if fixtureID != liveLaneFixtureLive && fixtureID != liveLaneFixtureFirstImport || rejected == liveLaneRejectAll {
					for _, kind := range []string{"requests", "registrations"} {
						path := filepath.Join(local.Home(), kind, id+".json")
						raw, err := os.ReadFile(path)
						if err != nil {
							t.Fatal(err)
						}
						preserved[path] = raw
					}
					req, found, err := local.LoadRequest(id)
					if err != nil || !found || req.Token == "" {
						t.Fatalf("missing initial token %s: %+v %v", id, req, err)
					}
				}
			}
			var order []string
			result, err := Run(context.Background(), local, storagetest.NewMemoryStore(), Options{
				Sources: testSources, Parsers: testParsers, MachineID: "m",
				Now: func() time.Time { return base.Add(time.Hour) }, AcceptSession: cfg.AcceptSession,
				Progress: func(p Progress) { order = append(order, p.ArchiveSessionID) },
				Stop:     func() bool { return len(order) >= 2 },
			})
			if err != nil {
				t.Fatal(err)
			}
			wantOrder, wantUnreached := []string{"live", "i1"}, map[string]int{"i2": 0, "i3": 1}
			if rejected == liveLaneRejectAll {
				wantOrder, wantUnreached = nil, nil
				excluded = append(excluded, "live", "i1", "i2", "i3")
			}
			if !slices.Equal(order, wantOrder) || result.Scanned != len(wantOrder) {
				t.Fatalf("processed %v (%d scanned), want %v", order, result.Scanned, wantOrder)
			}
			if !maps.Equal(result.Unreached, wantUnreached) || len(result.Errors) != 0 || len(result.Deferred) != 0 {
				t.Fatalf("unreached %v errors %v deferred %v, want %v with no errors or deferrals", result.Unreached, result.Errors, result.Deferred, wantUnreached)
			}
			status, err := local.LoadStatus()
			if err != nil || status.PendingCount != len(wantUnreached) || !maps.Equal(status.UnreachedSessions, wantUnreached) {
				t.Fatalf("status pending %d unreached %v error %v", status.PendingCount, status.UnreachedSessions, err)
			}
			afterIdentities, err := local.LoadRegistrations()
			if err != nil || len(afterIdentities) != len(identities) {
				t.Fatalf("registration census %d => %d: %v", len(identities), len(afterIdentities), err)
			}
			for i := range identities {
				if identities[i].ArchiveSessionID != afterIdentities[i].ArchiveSessionID {
					t.Fatal("registration identity changed")
				}
			}
			for path, before := range preserved {
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatalf("preserved state changed %s: %v", path, err)
				}
				if filepath.Base(filepath.Dir(path)) == "requests" {
					var beforeObject, afterObject map[string]any
					if json.Unmarshal(before, &beforeObject) != nil || json.Unmarshal(after, &afterObject) != nil || beforeObject["token"] != afterObject["token"] {
						t.Fatalf("request token changed %s", path)
					}
				}
			}
			for _, id := range append(excluded, "i2", "i3") {
				if _, found, err := local.LoadRequest(id); err != nil || !found {
					t.Fatalf("%s lost request: found %t error %v", id, found, err)
				}
				if _, found, err := local.LoadRegistration(id); err != nil || !found {
					t.Fatalf("%s lost registration: found %t error %v", id, found, err)
				}
			}
		})
	}
}
