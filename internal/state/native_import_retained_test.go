package state

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agents/muse"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

type nativeImportReleaseSnapshot struct {
	stage       []byte
	pending     []byte
	publication PendingPublication
}

type nativeRetainedTestMode string

const (
	nativeRetainedTestValid           nativeRetainedTestMode = "valid"
	nativeRetainedTestMissing         nativeRetainedTestMode = "missing"
	nativeRetainedTestFuture          nativeRetainedTestMode = "future"
	nativeRetainedTestTampered        nativeRetainedTestMode = "tampered"
	nativeRetainedTestPredecessor     nativeRetainedTestMode = "predecessor"
	nativeRetainedTestOwner           nativeRetainedTestMode = "owner"
	nativeRetainedTestOrdinary        nativeRetainedTestMode = "ordinary"
	nativeRetainedTestMetadata        nativeRetainedTestMode = "metadata"
	nativeRetainedTestProgressMissing nativeRetainedTestMode = "progress-missing"
	nativeRetainedTestProgressFuture  nativeRetainedTestMode = "progress-future"
	nativeRetainedTestStage           nativeRetainedTestMode = "stage"
	nativeRetainedTestCheckpoint      nativeRetainedTestMode = "checkpoint"
)

// This starts with an actual committed live owner and explicitly arranges an
// already-owned historical receipt. It does not exercise historical admission.
func releasedNativeImportFixture(t *testing.T, snapshots ...*nativeImportReleaseSnapshot) (*Store, archive.SessionRegistration, PendingPublication) {
	t.Helper()
	f := newNativeAdmissionFixture(t)
	prepared, err := f.prepare(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := prepared.Close(); err != nil {
			t.Error(err)
		}
		f.assertReleased(t)
	}()
	reg, err := f.store.CommitNativeAdmission(t.Context(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	reg = historicalNativeRegistrationFromOwner(t, f, reg)
	cfg, _, err := config.Load(f.store.Home())
	if err != nil {
		t.Fatal(err)
	}
	cfg.NativeImportProtection, cfg.DurableStorageProtection = true, true
	cfg.PublicationCompositionProtection, cfg.ImportStagingProtection, cfg.StagedSelectionProtection = true, true, true
	if err := config.Save(f.store.Home(), cfg); err != nil {
		t.Fatal(err)
	}
	at := reg.AdmittedAt
	bundle, err := archive.NewSourceBundle(reg, muse.Filter{}, prepared.filtered, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := local.Write(f.store.registrationPath(reg.ArchiveSessionID), reg); err != nil {
		t.Fatal(err)
	}
	reg.StagedImportSHA256, err = f.store.SaveImportStage(t.Context(), reg, bundle, cfg.EffectiveSkillEvidence())
	if err != nil {
		t.Fatal(err)
	}
	if err := local.Write(f.store.registrationPath(reg.ArchiveSessionID), reg); err != nil {
		t.Fatal(err)
	}
	source, err := archive.BuildCompressedSource(bundle)
	if err != nil {
		t.Fatal(err)
	}
	sourceKey, err := archive.SourceObjectKey(bundle, source.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	metadataKey, err := archive.MetadataObjectKey("muse", reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	p := PendingPublication{Bundle: bundle, SourceBytes: source.Bytes, SourceSHA256: source.SHA256, SourceKey: sourceKey, MetadataKey: metadataKey, ReadyAt: at, Attempted: true, SkillEvidence: string(cfg.EffectiveSkillEvidence())}
	m, err := archive.BuildMetadataWithAnalysis(bundle, archive.Analysis{}, nil, "synthetic", at, at, p.SourceReference(), archive.ParserInfo{Name: "muse", Version: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	p.MetadataBytes, err = json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	p, err = f.store.FreezeStagedSelection(t.Context(), reg.ArchiveSessionID, p, ReadyPredecessorAbsent, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.SavePending(reg.ArchiveSessionID, p); err != nil {
		t.Fatal(err)
	}
	published, err := f.store.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := published.SaveReadyPublication(p, at); err != nil {
		t.Fatal(err)
	}
	guard, err := local.LockCollectorGuard(f.store.Home())
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Release()
	for _, snapshot := range snapshots {
		snapshot.stage, err = os.ReadFile(filepath.Join(f.store.Home(), importStageRelative(reg.ArchiveSessionID)))
		if err != nil {
			t.Fatal(err)
		}
		snapshot.pending, err = os.ReadFile(f.store.pendingPath(reg.ArchiveSessionID))
		if err != nil {
			t.Fatal(err)
		}
		snapshot.publication = p
	}
	if err := f.store.VerifyAndBeginStagedRelease(t.Context(), reg.ArchiveSessionID, stagedRemote(p), guard); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.path); err != nil {
		t.Fatal(err)
	}
	if err := f.store.NativeImportRetainedAuthority(t.Context(), reg); err != nil {
		t.Fatal(err)
	}
	var nextMetadata archive.Metadata
	if err := json.Unmarshal(p.MetadataBytes, &nextMetadata); err != nil {
		t.Fatal(err)
	}
	nextMetadata.Parser.Version = "successor"
	metadataBytes, err := json.Marshal(nextMetadata)
	if err != nil {
		t.Fatal(err)
	}
	next := PendingPublication{Bundle: bundle, SourceBytes: source.Bytes, SourceSHA256: source.SHA256, SourceKey: p.SourceKey, MetadataKey: p.MetadataKey, ReadyAt: at.Add(time.Hour), Attempted: true, SkillEvidence: p.SkillEvidence, MetadataOnly: true, MetadataBytes: metadataBytes}
	admission, err := archive.NativeImportAdmissionContext(reg)
	if err != nil {
		t.Fatal(err)
	}
	policy := readySHA([]byte(archive.FilterVersion + "\x00" + bundle.Capture.AdapterVersion + "\x00" + string(cfg.EffectiveSkillEvidence())))
	next, err = PreparePublicationV2(next, published.PublicationPredecessor(), reg.DestinationID, admission, policy, PublicationMetadata)
	if err != nil {
		t.Fatal(err)
	}
	return f.store, reg, next
}

func TestNativeImportMissingPendingRequiresExactReleasedAuthority(t *testing.T) {
	for _, mode := range []nativeRetainedTestMode{nativeRetainedTestValid, nativeRetainedTestMissing, nativeRetainedTestFuture, nativeRetainedTestTampered, nativeRetainedTestPredecessor, nativeRetainedTestOwner, nativeRetainedTestOrdinary, nativeRetainedTestMetadata, nativeRetainedTestProgressMissing, nativeRetainedTestProgressFuture, nativeRetainedTestStage, nativeRetainedTestCheckpoint} {
		t.Run(string(mode), func(t *testing.T) {
			s, reg, next := releasedNativeImportFixture(t)
			path := filepath.Join(s.Home(), stagedControl(reg.ArchiveSessionID, stagedReleaseReceiptName))
			switch mode {
			case nativeRetainedTestMissing:
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case nativeRetainedTestFuture, nativeRetainedTestTampered:
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if mode == nativeRetainedTestFuture {
					raw = bytes.Replace(raw, []byte(`"version":3`), []byte(`"version":99`), 1)
				} else {
					raw = append(raw[:len(raw)-2], []byte(`,"future":true}`)...)
				}
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			case nativeRetainedTestPredecessor:
				next.Preparation.PredecessorSHA256 = strings.Repeat("a", 64)
			case nativeRetainedTestOwner, nativeRetainedTestOrdinary:
				if mode == nativeRetainedTestOrdinary {
					reg.NativeImport = nil
				} else {
					reg.RegisteredAt = reg.RegisteredAt.Add(time.Second)
				}
				if err := local.Write(s.registrationPath(reg.ArchiveSessionID), reg); err != nil {
					t.Fatal(err)
				}
			case nativeRetainedTestProgressMissing:
				if err := os.Remove(filepath.Join(s.Home(), stagedControl(reg.ArchiveSessionID, stagedReleaseProgressName))); err != nil {
					t.Fatal(err)
				}
			case nativeRetainedTestProgressFuture:
				if err := os.WriteFile(filepath.Join(s.Home(), stagedControl(reg.ArchiveSessionID, stagedReleaseProgressName)), []byte(`{"version":99}`), 0600); err != nil {
					t.Fatal(err)
				}
			case nativeRetainedTestStage, nativeRetainedTestCheckpoint:
				if mode == nativeRetainedTestStage {
					reg.StagedImportSHA256 = strings.Repeat("a", 64)
				} else {
					reg.NativeBinding.Checkpoint.SHA256 = strings.Repeat("f", 64)
				}
				if err := local.Write(s.registrationPath(reg.ArchiveSessionID), reg); err != nil {
					t.Fatal(err)
				}
			case nativeRetainedTestMetadata:
				next.MetadataBytes = append(next.MetadataBytes, ' ')
			}
			before, err := os.ReadFile(s.registrationPath(reg.ArchiveSessionID))
			if err != nil {
				t.Fatal(err)
			}
			err = s.SavePending(reg.ArchiveSessionID, next)
			if mode == nativeRetainedTestValid {
				if err != nil {
					t.Fatal("valid released successor", err)
				}
				return
			}
			if err == nil {
				t.Fatal("unsupported successor accepted")
			}
			if _, err := os.Stat(s.pendingPath(reg.ArchiveSessionID)); !os.IsNotExist(err) {
				t.Fatal("refused successor wrote pending", err)
			}
			after, err := os.ReadFile(s.registrationPath(reg.ArchiveSessionID))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("refusal changed owner", err)
			}
		})
	}
}

func TestNativeImportReleasedDetachIgnoresRemovedRootAndPreservesIndependentRequest(t *testing.T) {
	s, reg, next := releasedNativeImportFixture(t)
	original, err := s.nativeImportReleasedSelection(t.Context(), reg)
	if err != nil {
		t.Fatal(err)
	}
	// Arrange only the supported closed membership transition; production undo
	// authority remains a separate port, not this fixture's local write.
	reg.NativeImport.Membership.State = archive.NativeImportDetached
	if err := local.Write(s.registrationPath(reg.ArchiveSessionID), reg); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := config.Load(s.Home())
	if err != nil || len(cfg.NativeRoots) == 0 {
		t.Fatal("root removal fixture", err)
	}
	previous := cfg
	cfg.Harnesses = []string{"codex"}
	cfg.ImportedHarnesses = []string{"muse"}
	cfg.NativeRoots = nil
	cfg, err = config.ReconcileMuseCapture(cfg, previous, reg.AdmittedAt.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Save(s.Home(), cfg); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveRequest(reg.ArchiveSessionID, "independent", reg.AdmittedAt.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	req, found, err := s.LoadRequest(reg.ArchiveSessionID)
	if err != nil || !found {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.NativeImportRetainedAuthority(t.Context(), reg); err != nil {
			t.Fatal("detached retained authority", err)
		}
		if active, err := s.NativeImportStageActive(t.Context(), reg); err != nil || active {
			t.Fatal("released stage reactivated", active, err)
		}
	}
	detached, err := s.nativeImportReleasedSelection(t.Context(), reg)
	if err != nil || !sameStagedSelection(original, detached) {
		t.Fatal("detachment changed original proof", err)
	}
	if err := s.SavePending(reg.ArchiveSessionID, next); err != nil {
		t.Fatal("detached exact pending successor", err)
	}
	current, found, err := s.LoadRequest(reg.ArchiveSessionID)
	if err != nil || !found || current.Token != req.Token {
		t.Fatal("released checks consumed independent request", err)
	}
}

func TestNativeImportInterruptedReleaseRestartsFromActualReceipt(t *testing.T) {
	for _, phase := range []stagedReleasePhase{stagedReleaseVerified, stagedReleaseLocalComplete} {
		for _, mismatch := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/mismatch=%v", phase, mismatch), func(t *testing.T) {
				var snapshot nativeImportReleaseSnapshot
				s, reg, _ := releasedNativeImportFixture(t, &snapshot)
				id := reg.ArchiveSessionID
				var receipt stagedReleaseReceipt
				if err := s.readStagedControl(id, stagedReleaseReceiptName, &receipt); err != nil {
					t.Fatal(err)
				}
				var progress stagedReleaseProgress
				if err := s.readStagedControl(id, stagedReleaseProgressName, &progress); err != nil {
					t.Fatal(err)
				}
				progress.Phase = phase
				if mismatch {
					progress.ReceiptSHA256 = strings.Repeat("f", 64)
				}
				progressPath := filepath.Join(s.Home(), stagedControl(id, stagedReleaseProgressName))
				if err := local.Write(progressPath, progress); err != nil {
					t.Fatal(err)
				}
				stagePath := filepath.Join(s.Home(), importStageRelative(id))
				if phase == stagedReleaseVerified {
					if err := os.WriteFile(stagePath, snapshot.stage, 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(s.pendingPath(id), snapshot.pending, 0600); err != nil {
						t.Fatal(err)
					}
				}
				reopened, err := Open(s.Home())
				if err != nil {
					t.Fatal(err)
				}
				active, routeErr := reopened.NativeImportStageActive(t.Context(), reg)
				if mismatch {
					if routeErr == nil {
						t.Fatal("mismatched release progress selected routing")
					}
				} else if routeErr != nil || !active {
					t.Fatal("unfinished release lost active routing", active, routeErr)
				}
				guard, err := local.LockCollectorGuard(s.Home())
				if err != nil {
					t.Fatal(err)
				}
				defer guard.Release()
				paths := []string{progressPath, filepath.Join(s.Home(), stagedControl(id, stagedReleaseReceiptName)), s.registrationPath(id), s.publishedPath(id)}
				if phase == stagedReleaseVerified {
					paths = append(paths, stagePath, s.pendingPath(id))
				}
				before := map[string][]byte{}
				for _, path := range paths {
					raw, e := os.ReadFile(path)
					if e != nil {
						t.Fatal(e)
					}
					before[path] = raw
				}
				err = reopened.VerifyAndBeginStagedRelease(t.Context(), id, stagedRemote(snapshot.publication), guard)
				if mismatch {
					if err == nil {
						t.Fatal("mismatched progress released evidence")
					}
					for path, raw := range before {
						after, e := os.ReadFile(path)
						if e != nil || !bytes.Equal(raw, after) {
							t.Fatal("mismatched release changed evidence", path, e)
						}
					}
					return
				}
				if err != nil {
					t.Fatal("exact interrupted release restart", err)
				}
				if active, err := reopened.NativeImportStageActive(t.Context(), reg); err != nil || active {
					t.Fatal("finished restart remained active", active, err)
				}
				original, err := reopened.nativeImportReleasedSelection(t.Context(), reg)
				if err != nil || !sameStagedSelection(original, receipt.Selection) {
					t.Fatal("restart changed original selection", err)
				}
				for _, path := range []string{stagePath, s.pendingPath(id)} {
					if _, e := os.Stat(path); !os.IsNotExist(e) {
						t.Fatal("released payload remains", path, e)
					}
				}
			})
		}
	}
}

func TestNativeImportStageActivityKeepsEarlyReturns(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		published *Published
	}{{"nil", nil}, {"without-store", &Published{}}} {
		for _, staged := range []bool{false, true} {
			t.Run(test.name+"/"+map[bool]string{false: "absent-stage", true: "ordinary-stage"}[staged], func(t *testing.T) {
				t.Parallel()
				var reg archive.SessionRegistration
				if staged {
					reg.StagedImportSHA256 = strings.Repeat("a", 64)
				}
				active, err := test.published.NativeImportStageActive(reg)
				if err != nil || active != staged {
					t.Fatal("legacy early branch required a store", active, err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				active, err = test.published.nativeImportStageActive(ctx, reg)
				if err != nil || active != staged {
					t.Fatal("context delegation changed an early branch", active, err)
				}
			})
		}
	}
}
