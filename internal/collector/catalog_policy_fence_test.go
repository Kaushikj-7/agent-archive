package collector

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/destination"
	localfiles "github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestRunCatalogPolicyChangePreservesAdmittedOrdinaryJournal(t *testing.T) {
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
	provider := &collectorCatalogStore{MemoryStore: storagetest.NewMemoryStore(), local: local, t: t}
	remote, err := catalog.WrapConfigured(provider, cfg.Storage)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := localfiles.LockCollectorGuard(local.Home())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { guard.Release() }()
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC) }, CollectorGuard: guard, CatalogDestination: config.DestinationID(cfg.Storage)}
	result, err := Run(t.Context(), local, remote, opts)
	if err != nil || len(result.Errors) == 0 {
		t.Fatal("admitted failed publication not reached", result, err)
	}
	pending, found, err := local.LoadPending("session-1")
	if err != nil || !found || pending.Catalog == nil || pending.Catalog.Recovery == nil {
		t.Fatal(found, err)
	}
	journalPath := filepath.Join(local.Home(), "pending", "session-1.json")
	before, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	seal, err := remote.Writer.Coordinator().Seal(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	guard.Release()
	local, err = openTestStore(local.Home())
	if err != nil {
		t.Fatal(err)
	}
	provider.local = local
	guard, err = localfiles.LockCollectorGuard(local.Home())
	if err != nil {
		t.Fatal(err)
	}
	opts.CollectorGuard, opts.SkillEvidence = guard, config.SkillEvidenceNone
	remote, err = catalog.WrapConfigured(provider, cfg.Storage)
	if err != nil {
		t.Fatal(err)
	}
	result, err = Run(t.Context(), local, remote, opts)
	if err != nil || !errors.Is(result.Errors["session-1"], state.ErrCatalogJournalFrozen) {
		t.Fatal("policy replacement did not fence", result, err)
	}
	after, err := os.ReadFile(journalPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("policy changed exact journal", err)
	}
	if _, _, err = remote.Writer.Coordinator().HeldBarrier(seal).Hold(t.Context()); !errors.Is(err, catalog.ErrAdmissionClosed) {
		t.Fatal("old owner disappeared", err)
	}
}

func TestCatalogStricterHistoryPreservesCommittedAndUncommittedJournal(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "uncommitted", true: "committed"}[committed], func(t *testing.T) {
			scan, pending := privacyJournal(t)
			cfg, _, err := config.Load(scan.local.Home())
			if err != nil {
				t.Fatal(err)
			}
			cfg.Storage.ArchiveFormat = destination.FormatCatalogV4
			if err = config.Save(scan.local.Home(), cfg); err != nil {
				t.Fatal(err)
			}
			provider := &collectorCatalogStore{MemoryStore: storagetest.NewMemoryStore(), local: scan.local, t: t, failed: true}
			remote, err := catalog.WrapConfigured(provider, cfg.Storage)
			if err != nil {
				t.Fatal(err)
			}
			guard, err := localfiles.LockCollectorGuard(scan.local.Home())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { guard.Release() }()
			scan.remote, scan.opts.CollectorGuard, scan.opts.CatalogDestination = remote, guard, config.DestinationID(cfg.Storage)
			id, revision, err := remote.FreezeCatalogMutation(t.Context(), pending.MetadataKey)
			if err != nil {
				t.Fatal(err)
			}
			pending.Catalog = &state.CatalogPublication{Protocol: 10, ID: id, ExpectedRevision: revision}
			if err = scan.local.SavePending(scan.id(), pending); err != nil {
				t.Fatal(err)
			}
			pending, err = scan.prepareCatalogJournal(pending)
			if err != nil {
				t.Fatal(err)
			}
			j := *pending.Catalog.Recovery
			admitted, settled, err := remote.BeginJournalPublication(t.Context(), id, pending.MetadataBytes, j, guard)
			if err != nil || settled {
				t.Fatal(settled, err)
			}
			for _, stage := range pending.History.Sources {
				body, err := scan.local.ReadPendingSource(scan.id(), stage)
				if err != nil {
					t.Fatal(err)
				}
				if err = remote.Put(admitted, stage.Reference.Key, body); err != nil {
					t.Fatal(err)
				}
			}
			if committed {
				if err = remote.Publication(id, pending.MetadataKey, revision).Put(admitted, pending.MetadataKey, pending.MetadataBytes); err != nil {
					t.Fatal(err)
				}
				body, err := remote.Get(t.Context(), pending.MetadataKey)
				if err != nil || !bytes.Equal(body, pending.MetadataBytes) {
					t.Fatal("actual committed fixture", err)
				}
			}
			remote.EndPublicationAttempt(id)
			seal, err := remote.Writer.Coordinator().Seal(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(scan.local.Home(), "pending", scan.id()+".json")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			guard.Release()
			scan.local, err = openTestStore(scan.local.Home())
			if err != nil {
				t.Fatal(err)
			}
			provider.local = scan.local
			guard, err = localfiles.LockCollectorGuard(scan.local.Home())
			if err != nil {
				t.Fatal(err)
			}
			remote, err = catalog.WrapConfigured(provider, cfg.Storage)
			if err != nil {
				t.Fatal(err)
			}
			scan.remote, scan.opts.CollectorGuard, scan.opts.SkillEvidence = remote, guard, config.SkillEvidenceNone
			if _, err = scan.resumeHistory(pending); !errors.Is(err, state.ErrCatalogJournalFrozen) {
				t.Fatal("stricter policy did not fence", err)
			}
			if _, err = scan.resumeStagedPolicy(pending); !errors.Is(err, state.ErrCatalogJournalFrozen) {
				t.Fatal("staged policy changed frozen catalog authority", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("stricter policy changed exact journal", err)
			}
			if _, _, err = remote.Writer.Coordinator().HeldBarrier(seal).Hold(t.Context()); !errors.Is(err, catalog.ErrAdmissionClosed) {
				t.Fatal("history owner disappeared", err)
			}
		})
	}
}
