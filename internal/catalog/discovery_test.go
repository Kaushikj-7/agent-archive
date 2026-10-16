package catalog

import (
	"bytes"
	"errors"
	"testing"

	"github.com/wangjohn/agent-archive/internal/storage"
)

func TestDiscoveryChargesCorruptMetadataBytes(t *testing.T) {
	t.Parallel()
	w, store := fixture(t)
	m := mutation(t, w, "synthetic")
	if _, err := w.Commit(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	activateFixture(t, w)
	snapshot, err := OpenSnapshot(t.Context(), store, nil)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := bytes.Repeat([]byte("x"), 100)
	if err = store.Put(t.Context(), m.Next.Metadata.Key, corrupt); err != nil {
		t.Fatal(err)
	}
	ctx, cost, err := DiscoveryBudget(t.Context(), 10, 150)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = snapshot.ReadMetadata(ctx, *m.Next); !errors.Is(err, storage.ErrChecksumMismatch) {
		t.Fatal(err)
	}
	if cost().Bytes != 100 {
		t.Fatalf("failed metadata bytes not charged: %+v", cost())
	}
	if _, err = snapshot.ReadMetadata(ctx, *m.Next); !errors.Is(err, ErrPickerBudget) {
		t.Fatalf("repeated corrupt metadata exceeded byte budget: %v", err)
	}
	if cost().Bytes > 150 {
		t.Fatalf("byte budget exceeded: %+v", cost())
	}
}

func TestDiscoveryChargesCorruptNodeBytes(t *testing.T) {
	t.Parallel()
	w, store := fixture(t)
	m := mutation(t, w, "synthetic")
	if _, err := w.Commit(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	activateFixture(t, w)
	snapshot, err := OpenSnapshot(t.Context(), store, nil)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := bytes.Repeat([]byte("x"), 100)
	if err = store.Put(t.Context(), snapshot.Root().Key, corrupt); err != nil {
		t.Fatal(err)
	}
	ctx, cost, err := DiscoveryBudget(t.Context(), 10, 150)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = snapshot.Find(ctx, m.SessionKey); !errors.Is(err, storage.ErrChecksumMismatch) {
		t.Fatal(err)
	}
	if cost().Bytes != 100 || cost().Nodes != 1 {
		t.Fatalf("failed node work not charged: %+v", cost())
	}
	if _, _, err = snapshot.ScanIdentities(ctx, "", 1); !errors.Is(err, ErrPickerBudget) {
		t.Fatalf("repeated corrupt node exceeded byte budget: %v", err)
	}
}
