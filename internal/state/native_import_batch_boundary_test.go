package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

func mutateNativeImportBatch(t *testing.T, s *Store, reg archive.SessionRegistration, mode string) {
	t.Helper()
	path := filepath.Join(s.Home(), "imports", reg.ImportBatch.Recorded()+".json")
	if mode == "missing" {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var control archive.NativeImportBatchControl
	if err := archive.DecodeNativeImportBatchControl(raw, &control); err != nil {
		t.Fatal(err)
	}
	switch mode {
	case "undone":
		at := reg.AdmittedAt.Add(time.Hour)
		control.UndoneAt = &at
	case "instance":
		control.NativeImport.Instance = strings.Repeat("f", 32)
	case "scope":
		control.NativeImport.Scope.Selected[0].Source.Boundary++
		control.NativeImport.ScopeSHA256, err = archive.NativeImportBatchScopeDigest(t.Context(), control.NativeImport.Scope)
		if err != nil {
			t.Fatal(err)
		}
	case "future":
		raw = bytes.Replace(raw, []byte("muse-historical-import-v1"), []byte("muse-historical-import-v2"), 1)
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		return
	default:
		t.Fatal("unknown batch mutation", mode)
	}
	if err := WithImportBatchLock(t.Context(), s.Home(), reg.ImportBatch.Recorded(), func(held *local.RootedHome) error {
		return local.RootedAtomicWriteChecked(held.Root, filepath.Join("imports", reg.ImportBatch.Recorded()+".json"), func(w io.Writer) error { return json.NewEncoder(w).Encode(control) }, func(rename func() error) error { return rename() })
	}); err != nil {
		t.Fatal(err)
	}
}

func TestNativeImportBatchInitialGuardAndExactACKCleanup(t *testing.T) {
	for _, acknowledged := range []bool{false, true} {
		for _, mode := range []string{"missing", "undone", "instance", "scope", "future"} {
			name := mode
			if acknowledged {
				name += "-ack"
			}
			t.Run(name, func(t *testing.T) {
				var snapshot nativeImportReleaseSnapshot
				s, reg, _ := releasedNativeImportFixture(t, &snapshot)
				restoreNativeImportBeforeRelease(t, s, reg, snapshot, acknowledged)
				mutateNativeImportBatch(t, s, reg, mode)
				beforeStage, err := os.ReadFile(filepath.Join(s.Home(), importStageRelative(reg.ArchiveSessionID)))
				if err != nil {
					t.Fatal(err)
				}
				beforePending, err := os.ReadFile(s.pendingPath(reg.ArchiveSessionID))
				if err != nil {
					t.Fatal(err)
				}
				validation := s.validateStagedSelection(t.Context(), snapshot.publication)
				if (validation == nil) != acknowledged {
					t.Fatal("batch guard/ACK distinction", validation)
				}
				candidate := snapshot.publication
				candidate.StagedSelection, candidate.CursorSelection, candidate.Selection = nil, nil, nil
				candidate.JournalVersion = 0
				if _, err := s.FreezeStagedSelection(t.Context(), reg.ArchiveSessionID, candidate, ReadyPredecessorAbsent, nil); err == nil {
					t.Fatal("factory borrowed existing ACK or replaced confirmation")
				}
				stage, found, err := s.LoadImportStage(t.Context(), reg)
				if err != nil || !found {
					t.Fatal(err)
				}
				// Admission recovery receives a bounded descriptor, with source bundles
				// intentionally released. It must reopen actual selected evidence.
				stage.Bundle = archive.SourceBundle{}
				resume := s.resumePreparedImport(t.Context(), stage)
				if (resume == nil) != acknowledged {
					t.Fatal("resume bypassed creating batch or required it after ACK", resume)
				}
				if acknowledged {
					guard, err := local.LockCollectorGuard(s.Home())
					if err != nil {
						t.Fatal(err)
					}
					defer guard.Release()
					if err := s.VerifyAndBeginStagedRelease(t.Context(), reg.ArchiveSessionID, stagedRemote(snapshot.publication), guard); err != nil {
						t.Fatal("exact ACK cleanup required creating batch", err)
					}
					return
				}
				afterStage, err := os.ReadFile(filepath.Join(s.Home(), importStageRelative(reg.ArchiveSessionID)))
				if err != nil || !bytes.Equal(beforeStage, afterStage) {
					t.Fatal("refusal changed stage", err)
				}
				afterPending, err := os.ReadFile(s.pendingPath(reg.ArchiveSessionID))
				if err != nil || !bytes.Equal(beforePending, afterPending) {
					t.Fatal("refusal changed pending", err)
				}
			})
		}
	}
}

func TestNativeImportBatchFinalPendingAndSelectingCAS(t *testing.T) {
	for _, selecting := range []bool{false, true} {
		for _, mode := range []string{"valid", "undone", "owner", "request", "config", "candidate"} {
			name := mode
			if selecting {
				name += "-selecting"
			}
			t.Run(name, func(t *testing.T) {
				var snapshot nativeImportReleaseSnapshot
				s, reg, _ := releasedNativeImportFixture(t, &snapshot)
				restoreNativeImportBeforeRelease(t, s, reg, snapshot, false)
				pending := snapshot.publication
				var next *publishedState
				target := filepath.Join("pending", reg.ArchiveSessionID+".json")
				var value any = publicationWire(pending)
				if selecting {
					published, err := s.LoadPublishedStateContext(t.Context(), reg.ArchiveSessionID)
					if err != nil {
						t.Fatal(err)
					}
					source := pending.SourceReference()
					candidate, err := published.nextBudgeted(pending.Bundle, pending.ReadyAt, CacheStatusPublished, "", pending.MetadataBytes, nil, &source)
					if err != nil {
						t.Fatal(err)
					}
					proof := *pending.StagedProof()
					candidate.LastPublished.setStagedProof(&proof)
					candidate.PublicationVersion = 3
					summary, err := s.summaryBudgeted(candidate)
					if err != nil {
						t.Fatal(err)
					}
					candidate.Summary = &summary
					next = &candidate
					value = candidate
					target = filepath.Join("published", reg.ArchiveSessionID+".json")
				}
				check, err := s.nativeImportStagedCommit(t.Context(), reg.ArchiveSessionID, &pending, next)
				if err != nil || check == nil {
					t.Fatal("initial exact control", err)
				}
				held, err := local.OpenRootedHome(s.Home())
				if err != nil {
					t.Fatal(err)
				}
				defer held.Close()
				before, readErr := os.ReadFile(filepath.Join(s.Home(), target))
				if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
					t.Fatal(readErr)
				}
				err = local.RootedAtomicWriteWitnessed(t.Context(), held.Root, target, func(w io.Writer) error { return json.NewEncoder(w).Encode(value) }, make([]byte, 4096), func(witness *local.AtomicWriteWitness, rename func() error) error {
					switch mode {
					case "undone":
						mutateNativeImportBatch(t, s, reg, "undone")
					case "owner":
						reg.RegisteredAt = reg.RegisteredAt.Add(time.Second)
						if err := local.Write(s.registrationPath(reg.ArchiveSessionID), reg); err != nil {
							t.Fatal(err)
						}
					case "request":
						if err := s.SaveRequest(reg.ArchiveSessionID, "later-request", reg.AdmittedAt.Add(time.Second)); err != nil {
							t.Fatal(err)
						}
					case "config":
						changeNativeImportInitialConfig(t, s, reg, "project")
					case "candidate":
						// Change only mutable wire bookkeeping after encoding. A fully valid
						// proof does not authorize bytes from a different candidate snapshot.
						pending.Attempted = !pending.Attempted
						if next != nil {
							next.Status = CacheStatusRateLimited
						}
					}
					return check(witness, rename)
				})
				if (err == nil) != (mode == "valid") {
					t.Fatal("final CAS accepted changed authority or refused valid own temp", err)
				}
				if mode != "valid" {
					after, err := os.ReadFile(filepath.Join(s.Home(), target))
					if errors.Is(readErr, os.ErrNotExist) {
						if !errors.Is(err, os.ErrNotExist) {
							t.Fatal("refusal created selecting state", err)
						}
					} else if err != nil || !bytes.Equal(before, after) {
						t.Fatal("refusal changed pending", err)
					}
				}
			})
		}
	}
}
