package collector

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/destination"
	localfiles "github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestCatalogMetadataOnlySourceFailureRetainsExactJournalForFreshRetry(t *testing.T) {
	for _, damage := range []string{"missing", "different"} {
		t.Run(damage, func(t *testing.T) {
			local := newTestStore(t)
			cfg, _, err := config.Load(local.Home())
			if err != nil {
				t.Fatal(err)
			}
			cfg.Storage.ArchiveFormat = destination.FormatCatalogV4
			if err = config.Save(local.Home(), cfg); err != nil {
				t.Fatal(err)
			}
			path := writeTranscript(t, t.TempDir(), "codex.jsonl", codexTranscript+"\n")
			if err = local.SaveRegistration(registration(t, path)); err != nil {
				t.Fatal(err)
			}
			raw := &collectorCatalogStore{MemoryStore: storagetest.NewMemoryStore(), local: local, t: t, failed: true}
			remote, err := catalog.WrapConfigured(raw, cfg.Storage)
			if err != nil {
				t.Fatal(err)
			}
			guard, err := localfiles.LockCollectorGuard(local.Home())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { guard.Release() }()
			now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
			opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return now }, CollectorGuard: guard, CatalogDestination: config.DestinationID(cfg.Storage), Retry: storage.RetryPolicy{MaxAttempts: 1}}
			result, err := Run(t.Context(), local, remote, opts)
			if err != nil || len(result.Published) != 1 || len(result.Errors) != 0 {
				t.Fatal("initial valid publication", result, err)
			}
			key, err := archive.MetadataObjectKey("codex", "session-1")
			if err != nil {
				t.Fatal(err)
			}
			body, err := remote.Get(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			var metadata archive.Metadata
			if err = json.Unmarshal(body, &metadata); err != nil {
				t.Fatal(err)
			}
			metadata.Title = "exact metadata-only retry"
			body, err = json.Marshal(metadata)
			if err != nil {
				t.Fatal(err)
			}
			source, err := raw.Get(t.Context(), metadata.SourceBundle.Key)
			if err != nil {
				t.Fatal(err)
			}
			bundle, _, _, err := local.LoadLastPublished("session-1")
			if err != nil {
				t.Fatal(err)
			}
			bundle.SchemaVersion = 1 // Older ordinary source schema this build cannot reproduce.
			if _, err = archive.BuildCompressedSource(bundle); err == nil {
				t.Fatal("fixture bundle remained reproducible")
			}
			pending := state.PendingPublication{MetadataOnly: true, Bundle: bundle, SourceKey: metadata.SourceBundle.Key, SourceSHA256: metadata.SourceBundle.SHA256, SourceSize: metadata.SourceBundle.CompressedBytes, MetadataKey: key, MetadataBytes: body, ReadyAt: now}
			if err = local.SavePending("session-1", pending); err != nil {
				t.Fatal(err)
			}
			before, version, err := remote.Writer.Head(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			wantErr := storage.ErrNotFound
			if damage == "missing" {
				err = raw.Delete(t.Context(), pending.SourceKey)
			} else {
				wantErr = storage.ErrChecksumMismatch
				err = raw.Put(t.Context(), pending.SourceKey, []byte("different"))
			}
			if err != nil {
				t.Fatal(err)
			}
			raw.frozen = ""
			now = now.Add(time.Hour)
			result, err = Run(t.Context(), local, remote, opts)
			if err != nil || !errors.Is(result.Errors["session-1"], wantErr) {
				t.Fatal("recorded-source failure was not reached", result, err)
			}
			retained, found, err := local.LoadPending("session-1")
			if err != nil || !found || retained.Catalog == nil || retained.Catalog.Recovery == nil || !bytes.Equal(retained.MetadataBytes, body) {
				t.Fatal("admitted metadata-only journal erased", found, err)
			}
			after, nextVersion, err := remote.Writer.Head(t.Context())
			if err != nil || before.Identity != after.Identity || version != nextVersion {
				t.Fatal("source failure published a head", err)
			}
			seal, err := remote.Writer.Coordinator().Seal(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err = remote.Writer.Coordinator().HeldBarrier(seal).Hold(t.Context()); !errors.Is(err, catalog.ErrAdmissionClosed) {
				t.Fatal("unfinished exact owner lost protection", err)
			}
			// Restore verified bytes only in the isolated provider fixture.
			if err = raw.Put(t.Context(), pending.SourceKey, source); err != nil {
				t.Fatal(err)
			}
			guard.Release()
			local, err = openTestStore(local.Home())
			if err != nil {
				t.Fatal(err)
			}
			raw.local = local
			guard, err = localfiles.LockCollectorGuard(local.Home())
			if err != nil {
				t.Fatal(err)
			}
			opts.CollectorGuard = guard
			remote, err = catalog.WrapConfigured(raw, cfg.Storage)
			if err != nil {
				t.Fatal(err)
			}
			now = now.Add(time.Hour)
			result, err = Run(t.Context(), local, remote, opts)
			if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
				t.Fatal("fresh exact retry after verified source restoration", result, err)
			}
			if _, found, err = local.LoadPending("session-1"); err != nil || found {
				t.Fatal("completed journal not removed", err)
			}
			if err = remote.Writer.Coordinator().Release(t.Context(), seal); err != nil {
				t.Fatal("completed owner did not drain", err)
			}
			got, err := remote.Get(t.Context(), key)
			if err != nil || !bytes.Equal(got, body) {
				t.Fatal("retry changed frozen metadata", err)
			}
		})
	}
}
