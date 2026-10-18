package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/config"
)

func TestOutstandingIncludesStagedImportAfterPendingUnlink(t *testing.T) {
	s, p, guard := stagedSelectionFixture(t, true)
	id := p.Bundle.ArchiveSessionID
	reg, found, err := s.LoadRegistration(id)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	remote := stagedRemote(p)
	if err := s.verifyStagedRemote(t.Context(), remote, p); err != nil {
		t.Fatal(err)
	}
	receipt := stagedReleaseReceipt{Version: p.StagedProof().Version, Selection: *p.StagedProof()}
	receipt.SHA256, err = s.readyDigest(t.Context(), receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.writeStagedControl(t.Context(), id, stagedReleaseReceiptName, receipt); err != nil {
		t.Fatal(err)
	}
	progress := stagedReleaseProgress{Version: receipt.Version, ReceiptSHA256: receipt.SHA256, Phase: stagedReleaseLocalComplete}
	// Execute the existing verified release's exact writes up to its durable
	// pending-unlink boundary, leaving only the selected stage to retire.
	err = config.WithDurableStorage(s.Home(), func(g config.DurableStorageGuard) error {
		if err := s.removeCoveredStagedRequest(id); err != nil {
			return err
		}
		if err := s.writeDurableGuard(t.Context(), g, stagedControl(id, stagedReleaseProgressName), progress); err != nil {
			return err
		}
		return s.removeExactStagedPending(t.Context(), g, id, p)
	})
	if err != nil {
		t.Fatal(err)
	}
	owed, err := OpenReadOnly(s.Home()).Outstanding(reg, false)
	if err != nil || !owed.StagedImport || !owed.Published || !owed.Pending() || !owed.Owed() || owed.Requested || owed.Upload || owed.Rechecking() || owed.DefersExpiry() {
		t.Fatal("unfinished first-stage settlement was lost or changed expiry", owed, err)
	}
	if err := OpenReadOnly(s.Home()).VerifyAndBeginStagedRelease(t.Context(), id, remote, guard); err != nil {
		t.Fatal(err)
	}
	owed, err = OpenReadOnly(s.Home()).Outstanding(reg, false)
	if err != nil || owed.StagedImport || owed.Pending() {
		t.Fatal("settled imported owner remained pending", owed, err)
	}
}

func TestOutstandingStagedImportRejectsAliases(t *testing.T) {
	for _, directory := range []bool{false, true} {
		t.Run(map[bool]string{false: "payload", true: "directory"}[directory], func(t *testing.T) {
			s, p, _ := stagedSelectionFixture(t, true)
			reg, found, err := s.LoadRegistration(p.Bundle.ArchiveSessionID)
			if err != nil || !found {
				t.Fatal(found, err)
			}
			path := filepath.Join(s.Home(), "sessions", reg.ArchiveSessionID, "import-stage")
			if !directory {
				path = filepath.Join(path, "stage.json")
			}
			if err := os.Rename(path, path+"-original"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(path+"-original", path); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Outstanding(reg, true); !errors.Is(err, ErrDurableStorageRecovery) {
				t.Fatal("unsafe stage became absent progress", err)
			}
		})
	}
}
