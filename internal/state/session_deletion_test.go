package state

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
)

func TestDeletionOwnerAndTemporaryControlsRemainActionable(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	reg := registration(t)
	if err := s.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	j, err := s.PrepareSessionDeletion(reg, RemovalReasonRetention, nil, time.Now(), "")
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(filepath.Join(s.home, deletionPath(reg.ArchiveSessionID)))
	if err != nil {
		t.Fatal(err)
	}
	other := reg
	other.DestinationID = "different-destination"
	if _, found, err := s.LoadSessionDeletion(other); !found || !errors.Is(err, ErrDurableStorageRecovery) {
		t.Fatal("foreign deletion owner accepted", found, err)
	}
	if _, err = s.AdvanceSessionDeletion(SessionDeletion{Registration: reg}, DeletionDeleting); !errors.Is(err, ErrDurableStorageRecovery) {
		t.Fatal("unbound phase advance accepted", err)
	}
	path := filepath.Join(s.home, deletionDirectory, reg.ArchiveSessionID, ".pending-interrupted")
	if err = os.WriteFile(path, []byte("retained incomplete intent"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.LoadSessionDeletion(reg); !found || !errors.Is(err, ErrDurableStorageRecovery) {
		t.Fatal("temporary control invisible", found, err)
	}
	if _, err = s.Outstanding(reg, false); !errors.Is(err, ErrDurableStorageRecovery) {
		t.Fatal("status omitted corrupt deletion obligation", err)
	}
	after, err := os.ReadFile(filepath.Join(s.home, deletionPath(reg.ArchiveSessionID)))
	if err != nil || !bytes.Equal(original, after) {
		t.Fatal("refusal changed original intent", err)
	}
	if j.Phase != DeletionPrepared {
		t.Fatal("fixture phase")
	}
}

func TestDeletionQuotaUsesExistingAggregatePool(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	reg := registration(t)
	if err := config.WithDurableStorage(s.home, func(config.DurableStorageGuard) error { return nil }); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.home, "pending", "saturated.json")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate(durableStorageQuota / 2); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PrepareSessionDeletion(reg, RemovalReasonRetention, nil, time.Now(), ""); !errors.Is(err, ErrDurableStorageCapacity) {
		t.Fatal("deletion bypassed aggregate cap", err)
	}
	if _, err = os.Stat(filepath.Join(s.home, deletionPath(reg.ArchiveSessionID))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("saturated write selected intent", err)
	}
}

func TestPinnedForgetRefusesDirectoryReplacementWithoutUnlinkingEitherOwner(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	reg := registration(t)
	if err := s.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	path := s.registrationPath(reg.ArchiveSessionID)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	records, err := s.pinDeletionRecords([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = records.close() }()
	old := filepath.Join(s.home, "registrations-original")
	if err = os.Rename(filepath.Dir(path), old); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	newer := []byte("unrelated replacement")
	if err = os.WriteFile(path, newer, 0600); err != nil {
		t.Fatal(err)
	}
	if err = records.remove(path); !errors.Is(err, ErrDurableStorageRecovery) {
		t.Fatal("replaced directory accepted", err)
	}
	for target, want := range map[string][]byte{path: newer, filepath.Join(old, filepath.Base(path)): before} {
		got, err := os.ReadFile(target)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatal("refusal removed another owner", target, err)
		}
	}
}
