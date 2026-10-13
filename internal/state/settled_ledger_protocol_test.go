package state

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
)

type settledLedgerArm struct {
	store      *Store
	reg        archive.SessionRegistration
	a          *agentapi.NativeReadBudget
	b          *agentapi.NativeReadBudget
	wrongSave  func() error
	rightSave  func() error
	closeOwned func() error
}

// Both arms use this exact observer/assertion source; only API fixture wiring differs.
func TestSettledCaptureLedgerAdmissionProtocol(t *testing.T) {
	t.Run("adequate-positive-first", func(t *testing.T) {
		arm := newSettledLedgerArm(t)
		if err := arm.rightSave(); err != nil {
			t.Fatal("real adequate original ledger setup refused", err)
		}
		if err := arm.closeOwned(); err != nil {
			t.Fatal(err)
		}
		if arm.a.Available() != 128<<20 || arm.b.Available() != 128<<20 {
			t.Fatal("adequate original owner leaked")
		}
	})
	t.Run("genuine-foreign-firstsave-no-loss", func(t *testing.T) {
		arm := newSettledLedgerArm(t)
		before := settledProtocolInventory(t, arm.store.Home())
		availableA, availableB := arm.a.Available(), arm.b.Available()
		if err := arm.wrongSave(); err == nil {
			t.Fatal("genuine capture on foreign ledger accepted")
		}
		after := settledProtocolInventory(t, arm.store.Home())
		if len(before) != len(after) {
			t.Fatal("foreign refusal created or removed authority")
		}
		for path, raw := range before {
			if !bytes.Equal(raw, after[path]) {
				t.Fatalf("foreign refusal changed %s", path)
			}
		}
		if arm.a.Available() != availableA || arm.b.Available() != availableB {
			t.Fatal("foreign refusal changed ledger charges")
		}
		if err := arm.rightSave(); err != nil {
			t.Fatal("same exact proof refused through original adequate ledger", err)
		}
		if err := arm.closeOwned(); err != nil {
			t.Fatal(err)
		}
		if arm.a.Available() != 128<<20 || arm.b.Available() != 128<<20 {
			t.Fatal("exact owner release failed")
		}
	})
}

func settledProtocolInventory(t *testing.T, home string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.WalkDir(home, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || strings.HasSuffix(path, ".lock") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(home, path)
		if err != nil {
			return err
		}
		out[relative] = raw
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
