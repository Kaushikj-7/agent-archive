package state

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

// settledStageGenerationFixture publishes a genuine typed staged selection,
// releases it through the actual readback path, then blocks it as rewritten.
func settledStageGenerationFixture(t *testing.T) (*Store, archive.SessionRegistration, time.Time) {
	t.Helper()
	s, p, guard := stagedSelectionFixture(t, true)
	id := p.Bundle.ArchiveSessionID
	if err := s.VerifyAndBeginStagedRelease(t.Context(), id, stagedRemote(p), guard); err != nil {
		t.Fatal(err)
	}
	guard.Release()
	published, err := s.LoadPublishedState(id)
	if err != nil {
		t.Fatal(err)
	}
	if !published.TypedStagedPublished() {
		t.Fatal("fixture did not publish a typed staged selection")
	}
	if err := published.SaveBlocked(p.Bundle, p.ReadyAt, BlockedReasonTranscriptRewritten); err != nil {
		t.Fatal(err)
	}
	reg, found, err := s.LoadRegistration(id)
	if err != nil || !found || reg.StagedImportSHA256 == "" {
		t.Fatalf("fixture registration: %#v %v %v", reg, found, err)
	}
	// Establish the configuration floor recovery composes under, so refusal
	// inventories compare only effects of the recovery attempt itself.
	if err := config.WithPublicationComposition(s.Home(), func(config.PublicationCompositionGuard) error { return nil }); err != nil {
		t.Fatal(err)
	}
	return s, reg, p.ReadyAt.Add(time.Hour)
}

// stageClearingBuilder is the successor shape the collector builder renders.
func stageClearingBuilder(at time.Time) func(archive.SessionRegistration, string) (archive.SessionRegistration, PendingPublication, error) {
	return selectableGenerationBuilder(at, true)
}

func stagedGenerationInventory(t *testing.T, home string) map[string]string {
	t.Helper()
	inventory := map[string]string{}
	err := filepath.WalkDir(home, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(home, path)
		if err != nil {
			return err
		}
		if strings.HasSuffix(rel, ".lock") {
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			inventory[rel] = "symlink:" + target
			return err
		}
		if entry.IsDir() {
			inventory[rel] = "dir"
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		inventory[rel] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return inventory
}

// assertSettledStageJournal checks the durable successor intent: the stage
// link stays with the predecessor and every other admission field is intact.
func assertSettledStageJournal(t *testing.T, s *Store, original archive.SessionRegistration) generationRecovery {
	t.Helper()
	journal, found, err := readJSON[generationRecovery](s, s.generationRecoveryPath(original.ArchiveSessionID))
	if err != nil || !found || journal.Version != 2 || journal.Registration == nil {
		t.Fatalf("journal: %#v %v %v", journal, found, err)
	}
	successor := *journal.Registration
	if successor.StagedImportSHA256 != "" {
		t.Fatalf("successor inherited settled stage link %q", successor.StagedImportSHA256)
	}
	preserved := successor
	preserved.ArchiveSessionID = original.ArchiveSessionID
	preserved.PreviousGenerationID = original.PreviousGenerationID
	preserved.StagedImportSHA256 = original.StagedImportSHA256
	if !reflect.DeepEqual(preserved, original) {
		t.Fatalf("successor changed admission or provenance:\n%#v\n%#v", preserved, original)
	}
	old, found, err := s.LoadRegistration(original.ArchiveSessionID)
	if err != nil || !found || old.StagedImportSHA256 != original.StagedImportSHA256 {
		t.Fatalf("predecessor lost its stage authority: %#v %v %v", old, found, err)
	}
	return journal
}

func TestGenerationRecoveryClearsSettledStageOnlyInSuccessor(t *testing.T) {
	t.Parallel()
	s, reg, at := settledStageGenerationFixture(t)
	if _, err := s.BeginGenerationRecovery(reg.ArchiveSessionID, at, generationBuilder(at)); err == nil {
		t.Fatal("successor journal carried the settled stage link")
	}
	if _, found, err := s.GenerationSuccessor(reg.ArchiveSessionID); err != nil || found {
		t.Fatalf("refused successor left a journal: %v %v", found, err)
	}
	interruptStageGenerationAtJournal(t, s, reg, at)
	journal := assertSettledStageJournal(t, s, reg)
	if err := s.validateGenerationRecoveryReplay(t.Context(), reg.ArchiveSessionID, journal); err != nil {
		t.Fatalf("settled successor refused on replay: %v", err)
	}
}

// Each refusal says whether sync can settle the stage: an unfinished release
// is owed, a link no release can settle is unsupported, and unreadable or
// inconsistent controls keep their own cause.
func TestGenerationRecoveryRefusesUnsettledStageBeforeBuilding(t *testing.T) {
	t.Parallel()
	owed := func(err error) bool { return errors.Is(err, ErrGenerationStageUnsettled) }
	unsupported := func(err error) bool { return errors.Is(err, ErrGenerationStageUnsupported) }
	ownCause := func(err error) bool {
		return err != nil && !errors.Is(err, ErrGenerationStageUnsettled) && !errors.Is(err, ErrGenerationStageUnsupported)
	}
	for name, tc := range map[string]struct {
		unsettle func(*testing.T, *Store, archive.SessionRegistration)
		want     func(error) bool
	}{
		"verified-only progress": {func(t *testing.T, s *Store, reg archive.SessionRegistration) {
			t.Helper()
			setStagedReleasePhase(t, s, reg.ArchiveSessionID, stagedReleaseVerified)
		}, owed},
		"local-complete progress": {func(t *testing.T, s *Store, reg archive.SessionRegistration) {
			t.Helper()
			setStagedReleasePhase(t, s, reg.ArchiveSessionID, stagedReleaseLocalComplete)
		}, owed},
		"release not begun": {func(t *testing.T, s *Store, reg archive.SessionRegistration) {
			t.Helper()
			for _, name := range []stagedControlName{stagedReleaseReceiptName, stagedReleaseProgressName} {
				if err := os.Remove(filepath.Join(s.Home(), stagedControl(reg.ArchiveSessionID, name))); err != nil {
					t.Fatal(err)
				}
			}
		}, owed},
		"corrupt progress": {func(t *testing.T, s *Store, reg archive.SessionRegistration) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(s.Home(), stagedControl(reg.ArchiveSessionID, stagedReleaseProgressName)), []byte(`{"version":3}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}, func(err error) bool { return ownCause(err) && errors.Is(err, ErrDurableStorageRecovery) }},
		"receipt for another selection": {func(t *testing.T, s *Store, reg archive.SessionRegistration) {
			t.Helper()
			var progress stagedReleaseProgress
			if err := s.readStagedControl(reg.ArchiveSessionID, stagedReleaseProgressName, &progress); err != nil {
				t.Fatal(err)
			}
			progress.ReceiptSHA256 = strings.Repeat("d", 64)
			if err := s.writeStagedControl(t.Context(), reg.ArchiveSessionID, stagedReleaseProgressName, progress); err != nil {
				t.Fatal(err)
			}
		}, func(err error) bool { return ownCause(err) && errors.Is(err, ErrDurableStorageRecovery) }},
		// A rewritten registration fails its own integrity check first.
		"mismatched link": {func(t *testing.T, s *Store, reg archive.SessionRegistration) {
			t.Helper()
			reg.StagedImportSHA256 = strings.Repeat("b", 64)
			if err := local.Write(s.registrationPath(reg.ArchiveSessionID), reg); err != nil {
				t.Fatal(err)
			}
		}, ownCause},
		"foreign root": {func(t *testing.T, s *Store, reg archive.SessionRegistration) {
			t.Helper()
			foreignStageRoot(t, s, reg.ArchiveSessionID)
		}, ownCause},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, reg, at := settledStageGenerationFixture(t)
			tc.unsettle(t, s, reg)
			if err := assertStageRecoveryRefused(t, s, reg.ArchiveSessionID, at); !tc.want(err) {
				t.Fatalf("refusal misclassified: %v", err)
			}
		})
	}
	t.Run("orphan link without typed proof", func(t *testing.T) {
		t.Parallel()
		s, reg, at := generationFixture(t)
		reg.StagedImportSHA256 = strings.Repeat("c", 64)
		if err := local.Write(s.registrationPath(reg.ArchiveSessionID), reg); err != nil {
			t.Fatal(err)
		}
		if err := config.WithPublicationComposition(s.Home(), func(config.PublicationCompositionGuard) error { return nil }); err != nil {
			t.Fatal(err)
		}
		if err := assertStageRecoveryRefused(t, s, reg.ArchiveSessionID, at); !unsupported(err) {
			t.Fatalf("untyped link not reported as unsupported: %v", err)
		}
	})
}

func assertStageRecoveryRefused(t *testing.T, s *Store, id string, at time.Time) error {
	t.Helper()
	before := stagedGenerationInventory(t, s.Home())
	built := false
	build := stageClearingBuilder(at)
	_, err := s.BeginGenerationRecovery(id, at, func(reg archive.SessionRegistration, next string) (archive.SessionRegistration, PendingPublication, error) {
		built = true
		return build(reg, next)
	})
	if err == nil || built {
		t.Fatalf("unsettled stage reached the successor builder: built=%v err=%v", built, err)
	}
	if after := stagedGenerationInventory(t, s.Home()); !reflect.DeepEqual(before, after) {
		t.Fatalf("refused recovery changed durable state:\n%v\n%v", before, after)
	}
	return err
}

func setStagedReleasePhase(t *testing.T, s *Store, id string, phase stagedReleasePhase) {
	t.Helper()
	var progress stagedReleaseProgress
	if err := s.readStagedControl(id, stagedReleaseProgressName, &progress); err != nil {
		t.Fatal(err)
	}
	progress.Phase = phase
	if err := s.writeStagedControl(t.Context(), id, stagedReleaseProgressName, progress); err != nil {
		t.Fatal(err)
	}
}

// foreignStageRoot replaces the stage control directory with a symlink to an
// exact copy elsewhere; durable reads must refuse the redirected root.
func foreignStageRoot(t *testing.T, s *Store, id string) {
	t.Helper()
	dir := filepath.Join(s.Home(), filepath.Dir(stagedControl(id, stagedReleaseReceiptName)))
	foreign := filepath.Join(t.TempDir(), "import-stage")
	if err := os.Rename(dir, foreign); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(foreign, dir); err != nil {
		t.Fatal(err)
	}
}

func interruptStageGenerationAtJournal(t *testing.T, s *Store, reg archive.SessionRegistration, at time.Time) string {
	t.Helper()
	stop := errors.New("stop after journal")
	s.onIndexStep = func(step string) error {
		if step == "generation-journal" {
			return stop
		}
		return nil
	}
	if _, err := s.BeginGenerationRecovery(reg.ArchiveSessionID, at, stageClearingBuilder(at)); !errors.Is(err, stop) {
		t.Fatal(err)
	}
	s.onIndexStep = nil
	next, found, err := s.GenerationSuccessor(reg.ArchiveSessionID)
	if err != nil || !found {
		t.Fatalf("journal missing: %v %v", found, err)
	}
	return next
}

func TestGenerationRecoveryReplayKeepsChangedStageAuthorityOwed(t *testing.T) {
	t.Parallel()
	for name, change := range map[string]struct {
		apply   func(*testing.T, *Store, archive.SessionRegistration)
		restore func(*testing.T, *Store, archive.SessionRegistration)
	}{
		"progress regressed": {
			apply: func(t *testing.T, s *Store, reg archive.SessionRegistration) {
				t.Helper()
				setStagedReleasePhase(t, s, reg.ArchiveSessionID, stagedReleaseLocalComplete)
			},
			restore: func(t *testing.T, s *Store, reg archive.SessionRegistration) {
				t.Helper()
				setStagedReleasePhase(t, s, reg.ArchiveSessionID, stagedReleaseReleased)
			},
		},
		"foreign root": {
			apply: func(t *testing.T, s *Store, reg archive.SessionRegistration) {
				t.Helper()
				foreignStageRoot(t, s, reg.ArchiveSessionID)
			},
			restore: func(t *testing.T, s *Store, reg archive.SessionRegistration) {
				t.Helper()
				dir := filepath.Join(s.Home(), filepath.Dir(stagedControl(reg.ArchiveSessionID, stagedReleaseReceiptName)))
				foreign, err := os.Readlink(dir)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(dir); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(foreign, dir); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, reg, at := settledStageGenerationFixture(t)
			next := interruptStageGenerationAtJournal(t, s, reg, at)
			change.apply(t, s, reg)
			if err := s.ResumeGenerationRecoveries(t.Context()); !errors.Is(err, ErrSessionIndexRecoveryRequired) {
				t.Fatalf("replayed changed stage authority: %v", err)
			}
			old, _, err := s.LoadRegistration(reg.ArchiveSessionID)
			if err != nil || old.CaptureFrozen || old.StagedImportSHA256 != reg.StagedImportSHA256 {
				t.Fatalf("refused replay changed predecessor: %#v %v", old, err)
			}
			if _, found, err := s.LoadRegistration(next); err != nil || found {
				t.Fatalf("refused replay registered successor: %v %v", found, err)
			}
			// Restored authority is still owed, not lost.
			change.restore(t, s, reg)
			if err := s.validateGenerationRecoveryReplay(t.Context(), reg.ArchiveSessionID, assertSettledStageJournal(t, s, reg)); err != nil {
				t.Fatalf("restored stage authority refused: %v", err)
			}
		})
	}
}

func TestGenerationRecoveryStageReplayCancellationAndNewerToken(t *testing.T) {
	t.Parallel()
	s, reg, at := settledStageGenerationFixture(t)
	interruptStageGenerationAtJournal(t, s, reg, at)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.ResumeGenerationRecoveries(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled replay: %v", err)
	}
	journal := assertSettledStageJournal(t, s, reg)
	if err := s.validateGenerationRecoveryReplay(canceled, reg.ArchiveSessionID, journal); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled stage validation: %v", err)
	}
	if old, _, err := s.LoadRegistration(reg.ArchiveSessionID); err != nil || old.CaptureFrozen {
		t.Fatalf("canceled replay froze predecessor: %v", err)
	}
	// Feedback can name the predecessor directly while hooks are gated. Its
	// newer token is the predecessor's own retained work, not stage authority.
	if err := s.SaveRequest(reg.ArchiveSessionID, "feedback", at); err != nil {
		t.Fatal(err)
	}
	if err := s.validateGenerationRecoveryReplay(t.Context(), reg.ArchiveSessionID, journal); err != nil {
		t.Fatalf("newer predecessor token blocked settled replay: %v", err)
	}
}

// Journals written before successors cleared stage links keep their original
// replay semantics, which never consulted release controls.
func TestGenerationRecoveryReplaysEarlierJournalKeepingStageLink(t *testing.T) {
	t.Parallel()
	s, reg, at := settledStageGenerationFixture(t)
	interruptStageGenerationAtJournal(t, s, reg, at)
	journal := assertSettledStageJournal(t, s, reg)
	journal.Registration.StagedImportSHA256 = reg.StagedImportSHA256
	setStagedReleasePhase(t, s, reg.ArchiveSessionID, stagedReleaseLocalComplete)
	if err := s.validateGenerationRecoveryReplay(t.Context(), reg.ArchiveSessionID, journal); err != nil {
		t.Fatalf("earlier journal refused: %v", err)
	}
}

// A cleared stage link relies on version 2's exact prior-selection digests.
func TestGenerationRecoveryRefusesClearedStageInVersionOneJournal(t *testing.T) {
	t.Parallel()
	s, reg, at := settledStageGenerationFixture(t)
	interruptStageGenerationAtJournal(t, s, reg, at)
	journal := assertSettledStageJournal(t, s, reg)
	journal.Version, journal.PriorMetadataSHA256, journal.PriorSourceSetSHA256 = 1, "", ""
	if err := s.validateGenerationRecoveryReplay(t.Context(), reg.ArchiveSessionID, journal); !errors.Is(err, ErrSessionIndexRecoveryRequired) {
		t.Fatalf("version 1 journal cleared a stage link: %v", err)
	}
	journal.Registration.StagedImportSHA256 = reg.StagedImportSHA256
	if err := s.validateGenerationRecoveryReplay(t.Context(), reg.ArchiveSessionID, journal); err != nil {
		t.Fatalf("version 1 journal keeping the link refused: %v", err)
	}
}
