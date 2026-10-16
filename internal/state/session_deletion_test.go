package state

import (
	"bytes"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/local"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
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

func TestDeletionStalePhaseCannotReplaceCurrentIntent(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	reg := registration(t)
	j, err := s.PrepareSessionDeletion(reg, RemovalReasonRetention, nil, time.Now(), "")
	if err != nil {
		t.Fatal(err)
	}
	current, err := s.AdvanceSessionDeletion(j, DeletionDeleting)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(s.home, deletionPath(reg.ArchiveSessionID)))
	if err != nil {
		t.Fatal(err)
	}
	j.Phase = DeletionAbsent
	if err := s.saveSessionDeletion(j, nil); !errors.Is(err, ErrDurableStorageRecovery) {
		t.Fatalf("stale phase replaced %s: %v", current.Phase, err)
	}
	after, err := os.ReadFile(filepath.Join(s.home, deletionPath(reg.ArchiveSessionID)))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("stale transition changed current intent", err)
	}
}

func TestDeletionReadRefusesSameInodeGrowthBeforeAllocation(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	path := filepath.Join(s.home, "growing-deletion")
	if err := os.WriteFile(path, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	home, err := local.OpenRootedHome(s.home)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = home.Close() }()
	budget := agentapi.NewNativeReadBudget(256)
	scoped, closeScope := s.WithReadBudget(t.Context(), budget)
	defer closeScope()
	scoped.deletionFileOpen = func(root *os.Root, name string) (*os.File, error) {
		f, err := root.Open(name)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 64<<10), 0600); err != nil {
			_ = f.Close()
			return nil, err
		}
		return f, nil
	}
	closeFailure := errors.New("checked close")
	var offset int64
	scoped.publicationFileClose = func(f *os.File) error {
		offset, _ = f.Seek(0, io.SeekCurrent)
		return errors.Join(f.Close(), closeFailure)
	}
	if _, err := scoped.readDeletionObject(home, home.Root, "growing-deletion", 1<<20); !errors.Is(err, ErrDurableStorageRecovery) || !errors.Is(err, closeFailure) {
		t.Fatal("growth or close failure was lost", err)
	}
	if offset != 0 {
		t.Fatalf("read %d unreserved bytes after same-inode growth", offset)
	}
	r := budget.Reservation()
	if !r.Reserve(256) {
		t.Fatal("refused read retained its reservation")
	}
	r.Release(256)
}

func TestForgetPropagatesFinalDirectoryBarrierFailure(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	reg := registration(t)
	if err := s.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("final directory barrier")
	s.deletionRecordsSync = func(root *os.Root) error { return errors.Join(syncDurableParent(root), failure) }
	if err := s.ForgetSession(reg.ArchiveSessionID, agentmeta.SessionKey{Agent: agentmeta.ID(reg.Harness.Name), NativeID: reg.NativeSessionID}); !errors.Is(err, failure) {
		t.Fatal("final durability failure was discarded", err)
	}
	if _, found, err := s.LoadRegistration(reg.ArchiveSessionID); err != nil || found {
		t.Fatal("fixture did not reach final barrier", found, err)
	}
}

func TestDeletionOwnerFreezesSourceAndRelationshipAuthority(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	reg := registration(t)
	if _, err := s.PrepareSessionDeletion(reg, RemovalReasonRetention, nil, time.Now(), ""); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*archive.SessionRegistration){
		"parent":         func(r *archive.SessionRegistration) { r.ParentSessionID = "changed-parent" },
		"native-child":   func(r *archive.SessionRegistration) { r.NativeChild = !r.NativeChild },
		"transcript":     func(r *archive.SessionRegistration) { r.TranscriptPath += "-relocated" },
		"discovery-root": func(r *archive.SessionRegistration) { r.DiscoveryRoot = "/different-approved-root" },
		"native-root":    func(r *archive.SessionRegistration) { r.NativeRootSessionID = "other-root" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := reg
			mutate(&changed)
			if _, found, err := s.LoadSessionDeletion(changed); !found || !errors.Is(err, ErrDurableStorageRecovery) {
				t.Fatal("changed authority accepted", found, err)
			}
		})
	}
	observed := reg
	observed.HookObservedAt = time.Now()
	observed.LastHeadSeenAt = new(time.Time)
	if _, found, err := s.LoadSessionDeletion(observed); err != nil || !found {
		t.Fatal("hook observation retargeted owner", found, err)
	}
}
