package cli

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/state/statetest"
)

// Regression: 2026-09 review M-20. Every command that counts pending
// sessions (status's pending count and its imports line, setup's
// destination guard, uninstall's warning, backfill's upload progress) asks
// state.Outstanding, so for any one session's state they agree. Before,
// each had its own definition: a published import with an update waiting
// for the upload interval was pending to setup but not to status's imports
// line.
func TestEveryPendingCountAgrees(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	bundle := func(reg archive.SessionRegistration) archive.SourceBundle {
		return pendingCountPublication(t, reg, now.Add(-time.Hour)).Bundle
	}
	pendingUpload := func(t *testing.T, store *state.Store, reg archive.SessionRegistration) {
		t.Helper()
		pending := pendingCountPublication(t, reg, now.Add(-time.Hour))
		pending.ReadyAt = now.Add(time.Hour)
		if err := store.SavePending(reg.ArchiveSessionID, pending); err != nil {
			t.Fatal(err)
		}
	}
	cache := func(t *testing.T, store *state.Store, reg archive.SessionRegistration, status state.CacheStatus) {
		t.Helper()
		if err := statetest.SavePublished(store, reg.ArchiveSessionID, bundle(reg), now.Add(-time.Hour), status); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name    string
		setup   func(t *testing.T, store *state.Store, reg archive.SessionRegistration)
		pending bool
	}{
		{"never captured", func(*testing.T, *state.Store, archive.SessionRegistration) {}, true},
		{"published", func(t *testing.T, s *state.Store, reg archive.SessionRegistration) {
			t.Helper()
			cache(t, s, reg, state.CacheStatusPublished)
		}, false},
		{"published with a rate-limited update", func(t *testing.T, s *state.Store, reg archive.SessionRegistration) {
			t.Helper()
			cache(t, s, reg, state.CacheStatusPublished)
			cache(t, s, reg, state.CacheStatusRateLimited)
			pendingUpload(t, s, reg)
		}, true},
		{"published with a rate-limited cache but no upload file", func(t *testing.T, s *state.Store, reg archive.SessionRegistration) {
			t.Helper()
			cache(t, s, reg, state.CacheStatusPublished)
			cache(t, s, reg, state.CacheStatusRateLimited)
		}, true},
		{"published with a request queued", func(t *testing.T, s *state.Store, reg archive.SessionRegistration) {
			t.Helper()
			cache(t, s, reg, state.CacheStatusPublished)
			if err := s.SaveRequest(reg.ArchiveSessionID, "stop", now); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"published with an interrupted scan", func(t *testing.T, s *state.Store, reg archive.SessionRegistration) {
			t.Helper()
			cache(t, s, reg, state.CacheStatusPublished)
			if err := s.SetScanPending(reg.ArchiveSessionID, true); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"published with an upload in flight", func(t *testing.T, s *state.Store, reg archive.SessionRegistration) {
			t.Helper()
			cache(t, s, reg, state.CacheStatusPublished)
			pendingUpload(t, s, reg)
		}, true},
		{"declined", func(t *testing.T, s *state.Store, reg archive.SessionRegistration) {
			t.Helper()
			cache(t, s, reg, state.CacheStatusDeclined)
		}, false},
		{"request queued in another destination", func(t *testing.T, s *state.Store, reg archive.SessionRegistration) {
			t.Helper()
			reg.DestinationID = "another-destination"
			if err := s.SaveRegistration(reg); err != nil {
				t.Fatal(err)
			}
			if err := s.SaveRequest(reg.ArchiveSessionID, "stop", now); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"blocked", func(t *testing.T, s *state.Store, reg archive.SessionRegistration) {
			t.Helper()
			if err := statetest.SaveBlocked(s, reg.ArchiveSessionID, bundle(reg), now.Add(-time.Hour), state.BlockedReasonTranscriptMissing); err != nil {
				t.Fatal(err)
			}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
			if err := os.Chmod(home, 0o700); err != nil {
				t.Fatal(err)
			}
			cfg := pairTestConfig(now, []string{"codex"}, project)
			if err := config.Save(home, cfg); err != nil {
				t.Fatal(err)
			}
			store, err := state.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			reg := saveImportedSession(t, store, now, "imported", project)
			tc.setup(t, store, reg)
			if reg, _, err = store.LoadRegistration(reg.ArchiveSessionID); err != nil {
				t.Fatal(err)
			}
			want := 0
			if tc.pending {
				want = 1
			}

			counts := map[string]int{}
			if n, err := pendingSessions(home, cfg); err != nil {
				t.Fatal(err)
			} else {
				counts["setup"] = n
			}
			n, unreadable := unpublishedSessions(home, cfg, true)
			if len(unreadable) > 0 {
				t.Fatal(unreadable)
			}
			counts["uninstall"] = n
			view, err := readStatus(pairStatusEnv(t, home, userHome, now, "codex"))
			if err != nil {
				t.Fatal(err)
			}
			counts["status pending"] = view.Collector.PendingCount
			counts["status imports"] = view.ImportedPending
			if p, err := importPending(store, cfg, reg); err != nil {
				t.Fatal(err)
			} else if p {
				counts["backfill upload"] = 1
			} else {
				counts["backfill upload"] = 0
			}
			for who, got := range counts {
				if got != want {
					t.Errorf("%s counts %d pending, want %d (all: %v)", who, got, want, counts)
				}
			}
		})
	}
}

// pendingCountPublication keeps the cache and upload facets independently valid.
func pendingCountPublication(t *testing.T, reg archive.SessionRegistration, at time.Time) state.PendingPublication {
	t.Helper()
	_, filter, ok := productionAgents.LookupSources(reg.Harness.Name)
	if !ok {
		t.Fatal("missing actual registered source codec")
	}
	bundle, err := archive.NewSourceBundle(reg, filter, archive.FilteredTranscript{Format: "synthetic-retained-jsonl", Records: [][]byte{[]byte(`{"type":"turn_context","model":"synthetic"}`)}}, at, nil)
	must(t, err)
	packed, err := archive.BuildCompressedSource(bundle)
	must(t, err)
	key, err := archive.SourceObjectKey(bundle, packed.SHA256)
	must(t, err)
	ref := archive.SourceReference{Key: key, SHA256: packed.SHA256, CompressedBytes: len(packed.Bytes)}
	metadata, err := archive.BuildMetadataWithAnalysis(bundle, archive.Analysis{}, nil, "synthetic-machine", reg.SessionStartedAt, at, ref, archive.ParserInfo{Name: reg.Harness.Name, Version: filter.Version()})
	must(t, err)
	body, err := json.Marshal(metadata)
	must(t, err)
	metadataKey, err := archive.MetadataObjectKey(reg.Harness.Name, reg.ArchiveSessionID)
	must(t, err)
	return state.PendingPublication{Bundle: bundle, SourceKey: key, MetadataKey: metadataKey, SourceSHA256: packed.SHA256, SourceBytes: packed.Bytes, MetadataBytes: body}
}
