package state

import (
	"bytes"
	"errors"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOwnedImportReleaseRechecksConcurrentCurrentOwner(t *testing.T) {
	t.Parallel()
	s, reg, _, pendingBefore := ownedImportReleaseCommittedFixture(t)
	stagePath := filepath.Join(s.Home(), importStageRelative(reg.ArchiveSessionID))
	before, err := os.ReadFile(stagePath)
	if err != nil {
		t.Fatal(err)
	}
	home, err := local.OpenRootedHome(s.Home())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := home.Close(); err != nil {
			t.Error(err)
		}
	}()
	unlock, err := local.RootedLockWait(home, "hooks.lock", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	changed := false
	check := func() error {
		if !changed {
			changed = true
			found, err := s.UpdateRegistration(reg.ArchiveSessionID, func(current *archive.SessionRegistration) error { current.ProjectID = "other-project"; return nil })
			if err != nil || !found {
				t.Fatal("normal registration update must remain possible", found, err)
			}
		}
		return home.Check()
	}
	// Model suspension after the public owner load, then resume its actual helper.
	releaseErr := s.removeOwnedImportStageAt(t.Context(), home, reg, reg, check, syncDurableParent)
	current, found, err := s.LoadRegistration(reg.ArchiveSessionID)
	if err != nil || !found || current.ProjectID == reg.ProjectID || current.StagedImportSHA256 != reg.StagedImportSHA256 {
		t.Fatal("new owner control", found, err)
	}
	after, err := os.ReadFile(stagePath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("concurrent owner change lost retained stage", err, "cleanup returned", releaseErr)
	}
	if !errors.Is(releaseErr, ErrDurableStorageRecovery) {
		t.Fatal("concurrent current-owner mismatch accepted", releaseErr)
	}
	if raw, err := os.ReadFile(s.pendingPath(reg.ArchiveSessionID)); err != nil || !bytes.Equal(raw, pendingBefore) {
		t.Fatal("refusal lost pending bytes", err)
	}
}

func TestOwnedImportReleaseAbsentRechecksConcurrentCurrentOwner(t *testing.T) {
	t.Parallel()
	for _, absent := range []string{"stage", "directory"} {
		t.Run(absent, func(t *testing.T) {
			t.Parallel()
			s, reg, _, pendingBefore := ownedImportReleaseCommittedFixture(t)
			path := filepath.Join(s.Home(), importStageRelative(reg.ArchiveSessionID))
			if absent == "directory" {
				if err := s.RemoveOwnedImportStage(reg); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			home, err := local.OpenRootedHome(s.Home())
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := home.Close(); err != nil {
					t.Error(err)
				}
			}()
			unlock, err := local.RootedLockWait(home, "hooks.lock", time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			changed := false
			check := func() error {
				if !changed {
					changed = true
					found, err := s.UpdateRegistration(reg.ArchiveSessionID, func(current *archive.SessionRegistration) error { current.ProjectID = "other-project"; return nil })
					if err != nil || !found {
						t.Fatal("normal owner update failed", found, err)
					}
				}
				return home.Check()
			}
			if err := s.removeOwnedImportStageAt(t.Context(), home, reg, reg, check, syncDurableParent); !errors.Is(err, ErrDurableStorageRecovery) {
				t.Fatal("absent cleanup skipped concurrent owner comparison", err)
			}
			if raw, err := os.ReadFile(s.pendingPath(reg.ArchiveSessionID)); err != nil || !bytes.Equal(raw, pendingBefore) {
				t.Fatal("owner refusal lost pending bytes", err)
			}
		})
	}
}

func TestOwnedImportReleaseSerializesOwnerThroughDurabilityBarrier(t *testing.T) {
	t.Parallel()
	s, reg, _, pendingBefore := ownedImportReleaseCommittedFixture(t)
	attempted := false
	if err := s.removeOwnedImportStageUsing(reg, func(held *os.Root) error {
		if !attempted {
			attempted = true
			_, err := s.UpdateRegistration(reg.ArchiveSessionID, func(current *archive.SessionRegistration) error {
				current.ProjectID = "other-project"
				return nil
			})
			if !errors.Is(err, local.ErrBusy) {
				t.Fatal("registration writer overtook cleanup durability barrier", err)
			}
		}
		return syncDurableParent(held)
	}); err != nil {
		t.Fatal(err)
	}
	if !attempted {
		t.Fatal("cleanup never reached its actual durability barrier")
	}
	if raw, err := os.ReadFile(s.pendingPath(reg.ArchiveSessionID)); err != nil || !bytes.Equal(raw, pendingBefore) {
		t.Fatal("stage release changed the exact selecting journal", err)
	}
	found, err := s.UpdateRegistration(reg.ArchiveSessionID, func(current *archive.SessionRegistration) error {
		current.ProjectID = "other-project"
		return nil
	})
	if err != nil || !found {
		t.Fatal("cleanup failed to release owner serialization", found, err)
	}
}
