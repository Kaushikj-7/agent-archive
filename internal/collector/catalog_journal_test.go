package collector

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/destination"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestRunFrozenHistoryCatalogJournalResumesFreshStoreAfterSeal(t *testing.T) {
	scan, _ := reconciliationFixture(t)
	cfg, _, err := config.Load(scan.local.Home())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Storage.ArchiveFormat = destination.FormatCatalogV4
	if err = config.Save(scan.local.Home(), cfg); err != nil {
		t.Fatal(err)
	}
	if err = scan.local.SaveRequest(scan.id(), "stop", scan.now); err != nil {
		t.Fatal(err)
	}
	raw := &collectorCatalogStore{MemoryStore: storagetest.NewMemoryStore(), local: scan.local, t: t}
	opts := scan.opts
	opts.RepoKey = func(string) string { return "" }
	opts.CatalogDestination = config.DestinationID(cfg.Storage)
	var frozen []byte
	seal := ""
	for pass := range 8 {
		reopened, err := openTestStore(scan.local.Home())
		if err != nil {
			t.Fatal(err)
		}
		scan.local, raw.local = reopened, reopened
		guard, err := local.LockCollectorGuard(reopened.Home())
		if err != nil {
			t.Fatal(err)
		}
		opts.CollectorGuard = guard
		remote, err := catalog.WrapConfigured(raw, cfg.Storage)
		if err != nil {
			guard.Release()
			t.Fatal(err)
		}
		result, runErr := Run(t.Context(), reopened, remote, opts)
		guard.Release()
		if runErr != nil {
			t.Fatal(runErr)
		}
		if len(result.Published) == 1 && len(result.Errors) == 0 {
			if !raw.failed || len(frozen) == 0 || pass == 0 {
				t.Fatal("fixture did not exercise an admitted failed publication")
			}
			body, err := remote.Get(t.Context(), scanMetadataKey(t, scan))
			if err != nil || !bytes.Equal(body, frozen) {
				t.Fatal("restart replaced frozen final metadata", err)
			}
			var m archive.Metadata
			if err = json.Unmarshal(body, &m); err != nil {
				t.Fatal(err)
			}
			refs, err := m.SourceReferences()
			if err != nil || len(refs) != 3 || m.History == nil || len(m.History.Preserved) != 2 {
				t.Fatal("history reference-set oracle", err)
			}
			for _, ref := range refs {
				data, err := raw.Get(t.Context(), ref.Key)
				if err != nil || !storage.VerifySHA256(data, ref.SHA256) {
					t.Fatal("resumed history lost source authority", err)
				}
			}
			if err = remote.Writer.Coordinator().Release(t.Context(), seal); err != nil {
				t.Fatal("history owner did not drain", err)
			}
			if _, found, err := reopened.LoadPending(scan.id()); err != nil || found {
				t.Fatal("history acknowledgment retained pending journal", err)
			}
			return
		}
		if len(result.Errors) == 0 {
			t.Fatal("preparation/admission failure disappeared")
		}
		if raw.failed && seal == "" {
			pending, found, err := reopened.LoadPending(scan.id())
			if err != nil || !found || pending.Catalog == nil || pending.Catalog.Recovery == nil {
				t.Fatal("history crash lost durable owner proof", err)
			}
			frozen = append([]byte(nil), pending.MetadataBytes...)
			seal, err = remote.Writer.Coordinator().Seal(t.Context())
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Fatal("bounded frozen history retries did not complete")
}

func scanMetadataKey(t *testing.T, scan *sessionScan) string {
	t.Helper()
	key, err := archive.MetadataObjectKey(scan.reg.Harness.Name, scan.id())
	if err != nil {
		t.Fatal(err)
	}
	return key
}
