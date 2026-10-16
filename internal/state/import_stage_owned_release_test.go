package state

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

func ownedImportReleaseFixture(t *testing.T, content string) (*Store, archive.SessionRegistration, archive.SourceBundle) {
	t.Helper()
	s := newTestStore(t)
	reg := registration(t)
	reg.Origin = archive.SessionOriginImport
	reg.ImportBatch = archive.NewImportBatch("synthetic-owned-release")
	bundle := archive.SourceBundle{SchemaVersion: archive.SourceSchemaVersion, ArchiveSessionID: reg.ArchiveSessionID, NativeSessionID: reg.NativeSessionID, ProjectID: reg.ProjectID, Capture: archive.SourceCapture{Harness: reg.Harness, AdapterName: "test", CapturedAt: reg.RegisteredAt}, NativeRecords: []map[string]any{{"synthetic": content}}}
	var err error
	reg.StagedImportSHA256, err = s.SaveImportStage(t.Context(), reg, bundle, config.SkillEvidenceNone)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	return s, reg, bundle
}

func TestOwnedImportStageReleaseRefusesForeignReplacement(t *testing.T) {
	t.Parallel()
	s, reg, _ := ownedImportReleaseFixture(t, "original selection")
	other, foreign, _ := ownedImportReleaseFixture(t, "different selection")
	if foreign.StagedImportSHA256 == reg.StagedImportSHA256 {
		t.Fatal("replacement did not change actual selection")
	}
	if _, found, err := other.LoadImportStage(t.Context(), foreign); err != nil || !found {
		t.Fatal("replacement must be a valid independent stage", found, err)
	}
	raw, err := os.ReadFile(filepath.Join(other.Home(), importStageRelative(foreign.ArchiveSessionID)))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.Home(), importStageRelative(reg.ArchiveSessionID))
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveOwnedImportStage(reg); !errors.Is(err, ErrDurableStorageRecovery) {
		t.Fatal("foreign selected stage removed", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, raw) {
		t.Fatal("refusal changed replacement bytes", err)
	}
}

func TestOwnedImportStageReleaseRequiresCurrentRegistration(t *testing.T) {
	t.Parallel()
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "exact-owner", true: "changed-owner"}[changed], func(t *testing.T) {
			t.Parallel()
			s, reg, _ := ownedImportReleaseFixture(t, "original selection")
			if changed {
				// Simulate a replaced registration; SaveRegistration itself already
				// refuses changing an admitted stage marker through its normal port.
				current := reg
				current.StagedImportSHA256 = strings.Repeat("a", 64)
				if err := local.Write(s.registrationPath(reg.ArchiveSessionID), current); err != nil {
					t.Fatal(err)
				}
			}
			err := s.RemoveOwnedImportStage(reg)
			if changed {
				if !errors.Is(err, ErrDurableStorageRecovery) {
					t.Fatal("changed registration authorized cleanup", err)
				}
			} else if err != nil {
				t.Fatal(err)
			} else if err := s.RemoveOwnedImportStage(reg); err != nil {
				t.Fatal("matching owner could not retry after stage unlink", err)
			}
			_, statErr := os.Stat(filepath.Join(s.Home(), importStageRelative(reg.ArchiveSessionID)))
			if changed && statErr != nil || !changed && !errors.Is(statErr, os.ErrNotExist) {
				t.Fatal("stage removal disagrees with owner", statErr)
			}
		})
	}
}

func TestOwnedImportStageAbsentRetryRequiresMatchingCurrentOwner(t *testing.T) {
	t.Parallel()
	s, reg, _ := ownedImportReleaseFixture(t, "original selection")
	if err := s.RemoveOwnedImportStage(reg); err != nil {
		t.Fatal(err)
	}
	current := reg
	current.StagedImportSHA256 = strings.Repeat("a", 64)
	if err := local.Write(s.registrationPath(reg.ArchiveSessionID), current); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveOwnedImportStage(reg); !errors.Is(err, ErrDurableStorageRecovery) {
		t.Fatal("absent stage bypassed the changed current owner", err)
	}
}

func TestOwnedImportStageReleasePreservesTypedStage(t *testing.T) {
	t.Parallel()
	s, pending, _ := stagedSelectionFixture(t, false)
	reg, found, err := s.LoadRegistration(pending.Bundle.ArchiveSessionID)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	if err := s.RemoveOwnedImportStage(reg); !errors.Is(err, ErrReadySelectionFrozen) {
		t.Fatal("typed stage bypassed readback release", err)
	}
	if _, found, err := s.LoadImportStage(t.Context(), reg); err != nil || !found {
		t.Fatal("typed stage lost", found, err)
	}
}

func TestOwnedImportStageReleasePreservesEvidenceOnRefusal(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"cancel", "budget", "replaced-root"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			s, reg, _ := ownedImportReleaseFixture(t, strings.Repeat("synthetic", 8192))
			path := filepath.Join(s.Home(), importStageRelative(reg.ArchiveSessionID))
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "cancel":
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				scoped, closeScope := s.WithReadBudget(ctx, nil)
				defer closeScope()
				if err := scoped.RemoveOwnedImportStage(reg); !errors.Is(err, context.Canceled) {
					t.Fatal("lost captured cancellation", err)
				}
			case "budget":
				scoped, closeScope := s.WithReadBudget(t.Context(), agentapi.NewNativeReadBudget(128<<10))
				defer closeScope()
				if err := scoped.RemoveOwnedImportStage(reg); !errors.Is(err, agentapi.ErrReadBudget) {
					t.Fatal("cleanup escaped shared read budget", err)
				}
			case "replaced-root":
				home, err := local.OpenRootedHome(s.Home())
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := home.Close(); err != nil {
						t.Error(err)
					}
				}()
				moved := filepath.Join(t.TempDir(), "held-home")
				if err := os.Rename(s.Home(), moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(s.Home(), 0700); err != nil {
					t.Fatal(err)
				}
				if err := s.removeOwnedImportStageAt(t.Context(), home, reg, reg, home.Check); !errors.Is(err, ErrDurableStorageRecovery) {
					t.Fatal("cleanup followed a replaced named home")
				}
				path = filepath.Join(moved, importStageRelative(reg.ArchiveSessionID))
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, raw) {
				t.Fatal("refused cleanup lost original stage", err)
			}
		})
	}
}
