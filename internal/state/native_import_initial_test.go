package state

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

type nativeInitialTestMode string

const (
	nativeInitialTestAbsentFiles nativeInitialTestMode = "absent-files"
	nativeInitialTestRemoved     nativeInitialTestMode = "removed"
	nativeInitialTestRevision    nativeInitialTestMode = "revision"
	nativeInitialTestDestination nativeInitialTestMode = "destination"
	nativeInitialTestProject     nativeInitialTestMode = "project"
	nativeInitialTestFlag        nativeInitialTestMode = "flag"
)

func restoreNativeImportBeforeRelease(t *testing.T, s *Store, reg archive.SessionRegistration, snapshot nativeImportReleaseSnapshot, acknowledged bool) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(s.Home(), importStageRelative(reg.ArchiveSessionID)), snapshot.stage, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.pendingPath(reg.ArchiveSessionID), snapshot.pending, 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []stagedControlName{stagedReleaseReceiptName, stagedReleaseProgressName} {
		if err := os.Remove(filepath.Join(s.Home(), stagedControl(reg.ArchiveSessionID, name))); err != nil {
			t.Fatal(err)
		}
	}
	if !acknowledged {
		if err := os.Remove(s.publishedPath(reg.ArchiveSessionID)); err != nil {
			t.Fatal(err)
		}
	}
}

func changeNativeImportInitialConfig(t *testing.T, s *Store, reg archive.SessionRegistration, mode nativeInitialTestMode) {
	t.Helper()
	cfg, _, err := config.Load(s.Home())
	if err != nil {
		t.Fatal(err)
	}
	previous := cfg
	if mode == nativeInitialTestAbsentFiles {
		if len(cfg.NativeRoots) != 1 {
			t.Fatal("configured root fixture")
		}
		if err := os.RemoveAll(cfg.NativeRoots[0].Canonical); err != nil {
			t.Fatal(err)
		}
		return
	}
	switch mode {
	case nativeInitialTestRemoved:
		cfg.Harnesses, cfg.ImportedHarnesses, cfg.NativeRoots = []string{"codex"}, []string{"muse"}, nil
	case nativeInitialTestRevision:
		cfg.NativeRoots = append([]archive.NativeRoot(nil), cfg.NativeRoots...)
		cfg.NativeRoots[0].Revision = "two"
	case nativeInitialTestDestination:
		cfg.Storage.Bucket += "-changed"
	case nativeInitialTestProject:
		cfg.Archive.Projects = append([]archive.ProjectActivation(nil), cfg.Archive.Projects...)
		cfg.Archive.Projects[0].Included = false
	case nativeInitialTestFlag:
		// Actual setup cannot remove this sticky flag. Model a stale/out-of-band
		// writer to prove the consuming API independently fails closed.
		path := filepath.Join(s.Home(), "config.json")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		changed := bytes.Replace(raw, []byte(`"native_import_protection": true`), []byte(`"native_import_protection": false`), 1)
		if bytes.Equal(changed, raw) {
			changed = bytes.Replace(raw, []byte(`"native_import_protection":true`), []byte(`"native_import_protection":false`), 1)
		}
		if bytes.Equal(changed, raw) {
			t.Fatal("inert flag fixture")
		}
		if err := os.WriteFile(path, changed, 0600); err != nil {
			t.Fatal(err)
		}
		return
	default:
		t.Fatal("unknown fixture mode", mode)
	}
	cfg, err = config.ReconcileMuseCapture(cfg, previous, reg.AdmittedAt.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Save(s.Home(), cfg); err != nil {
		t.Fatal(err)
	}
}

func TestNativeImportInitialStageUsesCurrentConfiguredRootUntilExactACK(t *testing.T) {
	for _, acknowledged := range []bool{false, true} {
		for _, mode := range []nativeInitialTestMode{nativeInitialTestAbsentFiles, nativeInitialTestRemoved, nativeInitialTestRevision, nativeInitialTestDestination, nativeInitialTestProject, nativeInitialTestFlag} {
			t.Run(fmt.Sprintf("ack=%v/%s", acknowledged, mode), func(t *testing.T) {
				var snapshot nativeImportReleaseSnapshot
				s, reg, _ := releasedNativeImportFixture(t, &snapshot)
				restoreNativeImportBeforeRelease(t, s, reg, snapshot, acknowledged)
				changeNativeImportInitialConfig(t, s, reg, mode)
				stagePath := filepath.Join(s.Home(), importStageRelative(reg.ArchiveSessionID))
				beforeStage, err := os.ReadFile(stagePath)
				if err != nil {
					t.Fatal(err)
				}
				beforePending, err := os.ReadFile(s.pendingPath(reg.ArchiveSessionID))
				if err != nil {
					t.Fatal(err)
				}
				validationErr := s.validateStagedSelection(t.Context(), snapshot.publication)
				allow := mode == nativeInitialTestAbsentFiles || acknowledged && (mode == nativeInitialTestRemoved || mode == nativeInitialTestRevision)
				if allow && validationErr != nil {
					t.Fatal("exact staged source refused", validationErr)
				}
				if !allow && validationErr == nil {
					t.Fatal("changed initial policy authorized source")
				}
				candidate := snapshot.publication
				candidate.StagedSelection, candidate.CursorSelection, candidate.Selection = nil, nil, nil
				candidate.JournalVersion = 0
				_, freezeErr := s.FreezeStagedSelection(t.Context(), reg.ArchiveSessionID, candidate, ReadyPredecessorAbsent, nil)
				if mode == nativeInitialTestAbsentFiles {
					if freezeErr != nil {
						t.Fatal("unchanged configured root required native filesystem", freezeErr)
					}
				} else if freezeErr == nil {
					t.Fatal("new factory selection borrowed existing ACK")
				}
				if acknowledged && allow {
					guard, err := local.LockCollectorGuard(s.Home())
					if err != nil {
						t.Fatal(err)
					}
					defer guard.Release()
					if err := s.VerifyAndBeginStagedRelease(t.Context(), reg.ArchiveSessionID, stagedRemote(snapshot.publication), guard); err != nil {
						t.Fatal("exact ACK cleanup required original root", err)
					}
					return
				}
				afterStage, err := os.ReadFile(stagePath)
				if err != nil || !bytes.Equal(beforeStage, afterStage) {
					t.Fatal("validation changed stage", err)
				}
				afterPending, err := os.ReadFile(s.pendingPath(reg.ArchiveSessionID))
				if err != nil || !bytes.Equal(beforePending, afterPending) {
					t.Fatal("validation changed pending", err)
				}
			})
		}
	}
}

func TestNativeImportInitialStageNeverBorrowsDifferentACK(t *testing.T) {
	var snapshot nativeImportReleaseSnapshot
	s, reg, _ := releasedNativeImportFixture(t, &snapshot)
	restoreNativeImportBeforeRelease(t, s, reg, snapshot, true)
	changeNativeImportInitialConfig(t, s, reg, "removed")
	// Change the candidate's source/body/proof consistently through an actual
	// factory under the old config is forbidden above. A different stored ACK
	// also cannot excuse the original candidate: even a marker-only rewrite
	// that recomputes its proof digest must retain full owner/source checks.
	var selected publishedState
	raw, err := os.ReadFile(s.publishedPath(reg.ArchiveSessionID))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &selected); err != nil {
		t.Fatal(err)
	}
	proof := selected.LastPublished.StagedProof()
	proof.Core.Predecessor = ReadyPredecessorUnknown
	proof.Core.PredecessorSHA256 = ""
	copyProof := *proof
	copyProof.SHA256 = ""
	proof.SHA256, err = s.readyDigest(t.Context(), copyProof)
	if err != nil {
		t.Fatal(err)
	}
	if err := local.Write(s.publishedPath(reg.ArchiveSessionID), selected); err != nil {
		t.Fatal(err)
	}
	if current, err := s.LoadPublishedStateContext(t.Context(), reg.ArchiveSessionID); err != nil || current.state.PublicationVersion != 3 {
		t.Fatal("different ACK fixture is not a valid current typed selection", err)
	}
	if err := s.validateStagedSelection(t.Context(), snapshot.publication); err == nil {
		t.Fatal("different selecting ACK excused removed initial root")
	}
}
