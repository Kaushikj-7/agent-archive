package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/destination"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
)

func journalFixture(t *testing.T) (*Store, *qualifiedStore, *state.Store, state.PendingPublication, *local.CollectorGuard, destination.Config) {
	t.Helper()
	w, provider := fixture(t)
	mutation := mutation(t, w, "journal")
	raw, err := w.readRef(t.Context(), mutation.Next.Metadata, 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	source := mutation.Next.Summary.SourceBundle
	bytes, err := provider.Get(t.Context(), source.Key)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	if err = os.Chmod(home, 0700); err != nil {
		t.Fatal(err)
	}
	localStore, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err = config.Save(home, config.Config{MachineID: "private"}); err != nil {
		t.Fatal(err)
	}
	guard, err := local.LockCollectorGuard(home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(guard.Release)
	cfg := destination.Config{Provider: "synthetic", Bucket: "private", Prefix: "catalog", ArchiveFormat: destination.FormatCatalogV4}
	store, err := WrapConfigured(provider, cfg)
	if err != nil {
		t.Fatal(err)
	}
	pending := state.PendingPublication{Catalog: &state.CatalogPublication{Protocol: 10, ID: mutation.ID}, SourceKey: source.Key, SourceSHA256: source.SHA256, SourceBytes: bytes, MetadataKey: mutation.SessionKey, MetadataBytes: raw}
	digest, err := localStore.CatalogJournalDigest("journal", pending)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := local.ID()
	if err != nil {
		t.Fatal(err)
	}
	origin, err := guard.Origin()
	if err != nil {
		t.Fatal(err)
	}
	pending.Catalog.Recovery = &local.CatalogJournal{Owner: owner, Origin: origin, Destination: config.DestinationID(cfg), SessionID: "journal", MutationID: mutation.ID, SHA256: digest}
	if err = localStore.SavePending("journal", pending); err != nil {
		t.Fatal(err)
	}
	if err = localStore.VerifyCatalogJournal("journal", pending, guard, config.DestinationID(cfg)); err != nil {
		t.Fatal(err)
	}
	return store, provider, localStore, pending, guard, cfg
}

func TestJournalAdmissionFreshStoreSealedResumeAndStaleInvocation(t *testing.T) {
	store, provider, localStore, pending, guard, cfg := journalFixture(t)
	j := *pending.Catalog.Recovery
	ctx, settled, err := store.BeginJournalPublication(t.Context(), j.MutationID, pending.MetadataBytes, j, guard)
	if err != nil || settled {
		t.Fatal("first admission", settled, err)
	}
	fresh, err := WrapConfigured(provider, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = fresh.BeginJournalPublication(t.Context(), j.MutationID, pending.MetadataBytes, j, guard); err == nil {
		t.Fatal("parallel fresh wrapper stole running invocation")
	}
	seal, err := store.Writer.Coordinator().Seal(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	store.EndPublicationAttempt(j.MutationID)
	guard.Release()
	guard, err = local.LockCollectorGuard(localStore.Home())
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Release()
	if err = localStore.VerifyCatalogJournal("journal", pending, guard, config.DestinationID(cfg)); err != nil {
		t.Fatal(err)
	}
	resumed, settled, err := fresh.BeginJournalPublication(t.Context(), j.MutationID, pending.MetadataBytes, j, guard)
	if err != nil || settled {
		t.Fatal("pre-seal exact owner did not resume", settled, err)
	}
	defer fresh.EndPublicationAttempt(j.MutationID)
	if err = store.Put(ctx, pending.SourceKey, pending.SourceBytes); !errors.Is(err, ErrAdmissionClosed) {
		t.Fatal("stale invocation still stages source", err)
	}
	if err = fresh.Put(resumed, pending.SourceKey, pending.SourceBytes); err != nil {
		t.Fatal(err)
	}
	if err = fresh.Publication(j.MutationID, pending.MetadataKey, "").Put(resumed, pending.MetadataKey, pending.MetadataBytes); err != nil {
		t.Fatal(err)
	}
	if err = fresh.CompletePublication(resumed, j.MutationID, pending.MetadataBytes); err != nil {
		t.Fatal(err)
	}
	state, _, err := fresh.Writer.Coordinator().read(t.Context())
	if err != nil || len(state.Owners) != 0 || len(state.Completed) != 1 {
		t.Fatal("completion did not retain exact receipt", err)
	}
	refs, release, err := fresh.Writer.Coordinator().HeldBarrier(seal).Hold(t.Context())
	if err != nil || len(refs) != 2 {
		t.Fatal("receipt sources/body missing from held inventory", err)
	}
	release()
	if err = guard.RecordJournalRemoval(j); err != nil {
		t.Fatal(err)
	}
	if _, err = guard.ProveJournalRemoval(j); err == nil {
		t.Fatal("receipt acknowledged before pending unlink")
	}
	if err = localStore.RemoveCatalogPending("journal", j, guard); err != nil {
		t.Fatal(err)
	}
	proof, err := guard.ProveJournalRemoval(j)
	if err != nil {
		t.Fatal(err)
	}
	if err = fresh.AcknowledgeJournalRemoval(t.Context(), proof, guard); err != nil {
		t.Fatal(err)
	}
	if err = fresh.AcknowledgeJournalRemoval(t.Context(), proof, guard); err != nil {
		t.Fatal("lost acknowledgment retry", err)
	}
}

func TestJournalAdmissionWrongBindingAndIncompleteFinalRefuse(t *testing.T) {
	store, provider, _, pending, guard, cfg := journalFixture(t)
	j := *pending.Catalog.Recovery
	for _, change := range []func(*local.CatalogJournal){
		func(j *local.CatalogJournal) { j.Destination = "different" },
		func(j *local.CatalogJournal) { j.Origin = j.SHA256 },
	} {
		bad := j
		change(&bad)
		if _, _, err := store.BeginJournalPublication(t.Context(), j.MutationID, pending.MetadataBytes, bad, guard); err == nil {
			t.Fatal("foreign journal admitted")
		}
	}
	unbound, err := Wrap(provider)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = unbound.BeginJournalPublication(t.Context(), j.MutationID, pending.MetadataBytes, j, guard); err == nil {
		t.Fatal("unbound destination recovered journal")
	}
	other := cfg
	other.Prefix = "different"
	wrong, err := WrapConfigured(provider, other)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = wrong.BeginJournalPublication(t.Context(), j.MutationID, pending.MetadataBytes, j, guard); err == nil {
		t.Fatal("changed configured destination admitted journal")
	}
	ctx, _, err := store.BeginJournalPublication(t.Context(), j.MutationID, pending.MetadataBytes, j, guard)
	if err != nil {
		t.Fatal(err)
	}
	defer store.EndPublicationAttempt(j.MutationID)
	if err = store.CompletePublication(ctx, j.MutationID, pending.MetadataBytes); err == nil {
		t.Fatal("uncommitted publication acknowledged")
	}
	if err = store.Writer.Coordinator().Complete(context.Background(), store.owners[j.MutationID], store.pending[j.MutationID]); !errors.Is(err, ErrAdmissionClosed) {
		t.Fatal("generic completion bypassed journal receipt", err)
	}
}

func TestJournalAdmissionVerifiesPersistedAuthorityBeforeCoordinatorWrite(t *testing.T) {
	for _, kind := range []journalAdmissionFault{journalMissing, journalChanged, journalRawMismatch, journalCopied} {
		t.Run(string(kind), func(t *testing.T) {
			_, raw, localStore, pending, guard, cfg := journalFixture(t)
			counted := &namespaceWriteStore{qualifiedStore: raw}
			remote, err := WrapConfigured(counted, cfg)
			if err != nil {
				t.Fatal(err)
			}
			j := *pending.Catalog.Recovery
			body := pending.MetadataBytes
			switch kind {
			case journalMissing:
				// Explicit private corruption fixture; public removal must refuse.
				if err = os.Remove(filepath.Join(localStore.Home(), "pending", j.SessionID+".json")); err != nil {
					t.Fatal(err)
				}
			case journalChanged:
				pending.MetadataBytes = append(append([]byte(nil), pending.MetadataBytes...), ' ')
				encoded, encodeErr := json.Marshal(pending)
				if encodeErr != nil {
					t.Fatal(encodeErr)
				}
				// Explicit private damage fixture bypasses the new public save fence.
				if err = os.WriteFile(filepath.Join(localStore.Home(), "pending", j.SessionID+".json"), encoded, 0600); err != nil {
					t.Fatal(err)
				}
			case journalRawMismatch:
				body = append(append([]byte(nil), body...), ' ')
			case journalCopied:
				home := t.TempDir()
				if err = os.Chmod(home, 0700); err != nil {
					t.Fatal(err)
				}
				copied, openErr := state.Open(home)
				if openErr != nil {
					t.Fatal(openErr)
				}
				if err = config.Save(home, config.Config{MachineID: "private"}); err != nil {
					t.Fatal(err)
				}
				principal, readErr := os.ReadFile(filepath.Join(localStore.Home(), "collector-principal.json"))
				if readErr != nil {
					t.Fatal(readErr)
				}
				if err = local.WriteBytes(filepath.Join(home, "collector-principal.json"), principal); err != nil {
					t.Fatal(err)
				}
				if err = copied.SavePending(j.SessionID, pending); err != nil {
					t.Fatal(err)
				}
				guard, err = local.LockCollectorGuard(home)
				if err != nil {
					t.Fatal(err)
				}
				defer guard.Release()
			}
			if _, _, err = remote.BeginJournalPublication(t.Context(), j.MutationID, body, j, guard); err == nil {
				t.Fatal("unverified persisted authority admitted")
			}
			if counted.writes != 0 {
				t.Fatal("invalid journal reached coordinator CAS", counted.writes)
			}
		})
	}
}

type journalAdmissionFault string

const (
	journalMissing     journalAdmissionFault = "missing"
	journalChanged     journalAdmissionFault = "changed"
	journalRawMismatch journalAdmissionFault = "raw-mismatch"
	journalCopied      journalAdmissionFault = "copied"
)

func TestCanceledGuardedCatalogSavePreservesExactJournal(t *testing.T) {
	_, _, localStore, pending, _, _ := journalFixture(t)
	path := filepath.Join(localStore.Home(), "pending", "journal.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	scoped, closeScoped := localStore.WithReadBudget(ctx, nil)
	defer closeScoped()
	pending.Attempted = true
	if err = scoped.SavePending("journal", pending); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled save accepted exact journal bookkeeping", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatal("canceled save changed exact recovery bytes", err)
	}
}
