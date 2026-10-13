package codex

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

type inspectionSnapshotDecorator struct {
	agentapi.SourceSnapshot
	agentapi.VerifiedImportSnapshot
}
type baseInspectionFilter = Filter
type inspectionFilterDecorator struct{ baseInspectionFilter }

func (inspectionFilterDecorator) InspectFilteredImport(context.Context, agentapi.NativeInput, agentapi.FilterContext) (agentapi.ImportSummary, error) {
	return agentapi.ImportSummary{Conversation: true}, nil
}

func TestOwnedInspectionAttestsActualSnapshotAndFilter(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ref, _ := historyFile(t, dir, threadA, threadA, 0, nil, "synthetic safe text")
	pass, err := (SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pass.Close(); err != nil {
			t.Error(err)
		}
	})
	snap, err := pass.Read(t.Context(), ref, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	owned := snap.(agentapi.VerifiedImportSnapshot)
	if !owned.VerifiedInspectionFor(snap, Filter{}) {
		t.Fatal("actual operation declined")
	}
	wrapped := inspectionSnapshotDecorator{snap, owned}
	if wrapped.VerifiedInspectionFor(wrapped, Filter{}) || owned.VerifiedInspectionFor(snap, inspectionFilterDecorator{}) {
		t.Fatal("decorator promoted to verification authority")
	}
	if _, proof, err := owned.InspectVerifiedImport(t.Context(), inspectionFilterDecorator{}, agentapi.FilterContext{}); err == nil || proof.Version != 0 {
		t.Fatalf("unsupported filter receipt: %+v %v", proof, err)
	}
	if err := snap.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := owned.CapturedContentHint(t.Context()); !errors.Is(err, agentapi.ErrClosed) {
		t.Fatalf("closed hint: %v", err)
	}
	if _, proof, err := owned.InspectVerifiedImport(t.Context(), Filter{}, agentapi.FilterContext{}); !errors.Is(err, agentapi.ErrClosed) || proof.Version != 0 {
		t.Fatalf("closed operation: %+v %v", proof, err)
	}
}

func TestOwnedInspectionHintNeverAuthorizesChangedOrCancelledContent(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"before-hint", "after-hint", "after-inspection", "cancel", "append"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			ref, raw := historyFile(t, dir, threadA, threadA, 0, nil, "synthetic safe text")
			info, err := os.Stat(ref.Path)
			if err != nil {
				t.Fatal(err)
			}
			pass, err := (SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := pass.Close(); err != nil {
					t.Error(err)
				}
			})
			snap, err := pass.Read(t.Context(), ref, agentapi.ReadLimits{})
			if err != nil {
				t.Fatal(err)
			}
			owned := snap.(agentapi.VerifiedImportSnapshot)
			rewrite := func() {
				changed := bytes.Replace(raw, []byte("safe text"), []byte("evil text"), 1)
				if bytes.Equal(changed, raw) {
					t.Fatal("synthetic rewrite failed")
				}
				if err := os.WriteFile(ref.Path, changed, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(ref.Path, info.ModTime(), info.ModTime()); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "before-hint" {
				rewrite()
			}
			hint, err := owned.CapturedContentHint(t.Context())
			if err != nil || hint.Version == 0 {
				t.Fatalf("hint: %+v %v", hint, err)
			}
			if phase == "after-hint" {
				rewrite()
			}
			if phase == "after-inspection" {
				_, proof, err := owned.InspectVerifiedImport(t.Context(), Filter{}, agentapi.FilterContext{Filename: ref.Path})
				if err != nil || proof != hint {
					t.Fatalf("first receipt: %+v %v", proof, err)
				}
				rewrite()
			}
			ctx := t.Context()
			if phase == "cancel" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				defer cancel()
			}
			if phase == "append" {
				if err := os.WriteFile(ref.Path, append(raw, []byte(strings.Repeat("x", 10000))...), 0600); err != nil {
					t.Fatal(err)
				}
			}
			summary, proof, err := owned.InspectVerifiedImport(ctx, Filter{}, agentapi.FilterContext{Filename: ref.Path})
			if phase == "append" {
				if err != nil || proof != hint || !summary.Conversation {
					t.Fatalf("captured append boundary: %+v %v", proof, err)
				}
			} else if err == nil || proof.Version != 0 {
				t.Fatalf("hint granted stale authority: %+v %v", proof, err)
			}
		})
	}
}

func TestOwnedInspectionRetainsLimitsAndLateSourceChecks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ref, _ := historyFile(t, dir, threadA, threadA, 0, nil, strings.Repeat("synthetic safe text ", 1000))
	pass, err := (SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pass.Close(); err != nil {
			t.Error(err)
		}
	})
	snap, err := pass.Read(t.Context(), ref, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	owned := snap.(agentapi.VerifiedImportSnapshot)
	for _, limits := range []agentapi.ReadLimits{{RecordBytes: 512}, {FilteredBytes: 128}, {Records: 1}} {
		c := agentapi.FilterContext{Filename: ref.Path, Limits: limits}
		_, want := (Filter{}).InspectFilteredImport(t.Context(), snap.Input(), c)
		_, proof, got := owned.InspectVerifiedImport(t.Context(), Filter{}, c)
		if (got == nil) != (want == nil) || (got != nil && got.Error() != want.Error()) {
			t.Fatalf("limit %+v: %v/%v", limits, got, want)
		}
		if got != nil && proof.Version != 0 {
			t.Fatal("refused inspection emitted proof")
		}
	}
	if err := os.Chmod(ref.Path, 0640); err != nil {
		t.Fatal(err)
	}
	if _, proof, err := owned.InspectVerifiedImport(t.Context(), Filter{}, agentapi.FilterContext{Filename: ref.Path}); err == nil || proof.Version != 0 {
		t.Fatalf("late mode change: %+v %v", proof, err)
	}
}

func TestOrdinaryHashScratchRemainsChargedUntilClose(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ref, _ := historyFile(t, dir, threadA, threadA, 0, nil, strings.Repeat("synthetic safe text ", 10000))
	budget := agentapi.NewNativeReadBudget(agentapi.PassReadBudget)
	pass, err := (SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{ReadBudget: budget})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := pass.Read(t.Context(), ref, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	ordinary := snap.(*ordinarySnapshot)
	if int64(cap(ordinary.hashScratch)) > ordinary.scratchCharge || ordinary.scratchCharge != ordinaryScratch {
		t.Fatalf("scratch ownership=%d/%d", cap(ordinary.hashScratch), ordinary.scratchCharge)
	}
	charged, _ := budget.Charged()
	for range 3 {
		if err := ordinary.VerifyContent(t.Context()); err != nil {
			t.Fatal(err)
		}
		if after, _ := budget.Charged(); after != charged {
			t.Fatalf("verification changed ownership: %d/%d", charged, after)
		}
	}
	if err := snap.Close(); err != nil {
		t.Fatal(err)
	}
	if ordinary.hashScratch != nil || ordinary.scratchCharge != 0 {
		t.Fatal("closed snapshot retained hashing scratch")
	}
	if err := ordinary.VerifyContent(t.Context()); !errors.Is(err, agentapi.ErrClosed) {
		t.Fatalf("closed verifier: %v", err)
	}
	if err := pass.Close(); err != nil {
		t.Fatal(err)
	}
	if after, _ := budget.Charged(); after != 0 {
		t.Fatalf("pass retained %d bytes", after)
	}
}

type inspectionMutationFiles struct {
	transcriptio.OS
	before func([]byte, int64)
}
type inspectionMutationFile struct {
	transcriptio.File
	before func([]byte, int64)
}

func (f inspectionMutationFiles) OpenRegular(path string) (transcriptio.File, error) {
	file, err := f.OS.OpenRegular(path)
	if err != nil {
		return nil, err
	}
	return inspectionMutationFile{file, f.before}, nil
}
func (f inspectionMutationFile) ReadAt(p []byte, off int64) (int, error) {
	f.before(p, off)
	return f.File.ReadAt(p, off)
}

func TestOwnedInspectionRejectsRewriteDuringFilterAndFinalVerification(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"filter", "verification"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			ref, raw := historyFile(t, dir, threadA, threadA, 0, nil, strings.Repeat("synthetic safe text ", 12000))
			info, err := os.Stat(ref.Path)
			if err != nil {
				t.Fatal(err)
			}
			armed := false
			starts := 0
			files := inspectionMutationFiles{before: func(p []byte, off int64) {
				if !armed || off != 0 || len(p) < 64<<10 {
					return
				}
				starts++
				target := 1
				if phase == "verification" {
					target = 2
				}
				if starts != target {
					return
				}
				changed := bytes.ReplaceAll(raw, []byte("safe text"), []byte("evil text"))
				if err := os.WriteFile(ref.Path, changed, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(ref.Path, info.ModTime(), info.ModTime()); err != nil {
					t.Fatal(err)
				}
			}}
			pass, err := (SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{Files: files})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := pass.Close(); err != nil {
					t.Error(err)
				}
			})
			snap, err := pass.Read(t.Context(), ref, agentapi.ReadLimits{})
			if err != nil {
				t.Fatal(err)
			}
			owned := snap.(agentapi.VerifiedImportSnapshot)
			if _, err := owned.CapturedContentHint(t.Context()); err != nil {
				t.Fatal(err)
			}
			armed = true
			_, proof, err := owned.InspectVerifiedImport(t.Context(), Filter{}, agentapi.FilterContext{Filename: ref.Path})
			if err == nil || proof.Version != 0 || !agentapi.HasFailure(err, agentapi.Changed) {
				t.Fatalf("%s receipt: %+v %v", phase, proof, err)
			}
		})
	}
}
