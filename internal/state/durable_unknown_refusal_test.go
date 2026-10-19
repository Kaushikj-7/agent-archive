package state

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

// An unavailable owner requires recovery even when other selected evidence
// consumes control headroom. Capacity-only callers must not drain or retry it.
func TestUnavailableStorageRefusalPreservesRecovery(t *testing.T) {
	fixture := func(t *testing.T) (*Store, string, []byte, []byte) {
		t.Helper()
		s := newTestStore(t)
		reg := registration(t)
		reg.Origin = archive.SessionOriginImport
		reg.ImportBatch = archive.NewImportBatch("synthetic-refusal")
		bundle := archive.SourceBundle{SchemaVersion: archive.SourceSchemaVersion, ArchiveSessionID: reg.ArchiveSessionID, NativeSessionID: reg.NativeSessionID, ProjectID: reg.ProjectID, Capture: archive.SourceCapture{Harness: reg.Harness, AdapterName: "test", AdapterVersion: "test-v1", FilterVersion: archive.FilterVersion, CapturedAt: reg.RegisteredAt}}
		digest, err := s.SaveImportSourceSet(t.Context(), reg, bundle, nil, config.SkillEvidenceNone)
		if err != nil {
			t.Fatal("small actual producer", err)
		}
		reg.StagedImportSHA256 = digest
		stage, found, err := s.LoadImportStage(t.Context(), reg)
		if err != nil || !found || stage.SHA256 != digest || !reflect.DeepEqual(stage.Bundle, bundle) {
			t.Fatal("small actual selected stage/digest", found, err)
		}
		stagePath := filepath.Join(s.home, importStageRelative(reg.ArchiveSessionID))
		wire, err := os.ReadFile(stagePath)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := os.ReadFile(filepath.Join(s.home, "config.json"))
		if err != nil {
			t.Fatal(err)
		}
		t.Log("small actual controlled stage positive control PASS; exact selected bundle/digest")
		return s, stagePath, wire, cfg
	}
	checkUnchanged := func(t *testing.T, s *Store, path string, stage, cfg []byte) {
		t.Helper()
		current, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(current, stage) {
			t.Fatal("refusal changed selected evidence", err)
		}
		current, err = os.ReadFile(filepath.Join(s.home, "config.json"))
		if err != nil || !bytes.Equal(current, cfg) {
			t.Fatal("refusal changed existing protection", err)
		}
		if _, err := os.Stat(filepath.Join(s.home, "sessions", "new-owner")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("refusal allocated a new source owner", err)
		}
	}
	for _, size := range []int64{1, durableStorageQuota} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			s, path, stage, cfg := fixture(t)
			unknown := filepath.Join(s.home, "temporary-reservations", "unknown")
			sparseDurable(t, unknown, size)
			before, err := os.Stat(unknown)
			if err != nil {
				t.Fatal(err)
			}
			data := []byte("synthetic refused source")
			_, writeErr := s.StagePendingSource("new-owner", durableRef(data), data)
			checkUnchanged(t, s, path, stage, cfg)
			after, err := os.Stat(unknown)
			if err != nil || !os.SameFile(before, after) || after.Size() != size || !after.ModTime().Equal(before.ModTime()) {
				t.Fatal("refusal changed unavailable evidence", err)
			}
			if !errors.Is(writeErr, ErrDurableStorageRecovery) || errors.Is(writeErr, ErrDurableStorageCapacity) {
				t.Fatalf("unknown storage masked recovery: %v", writeErr)
			}
		})
	}
	t.Run("known-capacity", func(t *testing.T) {
		s, path, stage, cfg := fixture(t)
		sparseDurable(t, filepath.Join(s.home, "pending", "full.json"), durableStorageQuota/2)
		data := []byte("synthetic capacity refusal")
		_, err := s.StagePendingSource("new-owner", durableRef(data), data)
		checkUnchanged(t, s, path, stage, cfg)
		if !errors.Is(err, ErrDurableStorageCapacity) || errors.Is(err, ErrDurableStorageRecovery) {
			t.Fatalf("known capacity lost its distinct refusal: %v", err)
		}
	})
}
