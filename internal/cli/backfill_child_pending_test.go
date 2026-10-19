package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
)

type pendingChildrenFixture struct {
	f          *backfillFixture
	store      *state.Store
	cfg        config.Config
	regs       []archive.SessionRegistration
	batch      backfill.Batch
	candidates []state.SubagentCandidate
	args       []string
}

func newPendingChildrenFixture(t *testing.T) pendingChildrenFixture {
	t.Helper()
	f, _ := newImportFixture(t)
	args := []string{"--yes", "--harness", "claude", "--project", filepath.Join(f.userHome, "agent-archive"), "--since", "2026-09-21"}
	out, stderr, code := f.importRun(t, nil, false, append(args, "--background")...)
	if code != 0 {
		t.Fatal("prepare exact retained child cohort", code, stderr, out)
	}
	store := state.OpenReadOnly(f.data)
	regs, err := store.LoadRegistrations()
	must(t, err)
	candidates, err := store.LoadSubagentCandidates()
	must(t, err)
	if len(candidates) != 2 {
		t.Fatal("expected two actual staged children", len(candidates))
	}
	batch, _ := loadBatch(t, f.data)
	return pendingChildrenFixture{f, store, mustLoadConfig(t, f.data), regs, batch, candidates, args}
}

func TestPendingImportChildrenIgnoreUnrelatedAndHookCandidates(t *testing.T) {
	t.Parallel()
	fixture := newPendingChildrenFixture(t)
	var unrelated archive.SessionRegistration
	for _, reg := range fixture.regs {
		if !reg.InBatch(fixture.batch.ID) {
			unrelated = reg
		}
	}
	if unrelated.ArchiveSessionID == "" {
		t.Fatal("missing unrelated admitted owner")
	}
	hook := fixture.candidates[0]
	hook.ArchiveSessionID, hook.NativeSessionID, hook.Origin = "synthetic-hook-child", "synthetic-hook-native", archive.SessionOriginHook
	must(t, fixture.store.SaveSubagentCandidate(hook))
	other := fixture.candidates[0]
	other.ArchiveSessionID, other.NativeSessionID = "synthetic-other-child", "synthetic-other-native"
	other.ParentArchiveSessionID, other.ParentNativeSessionID = unrelated.ArchiveSessionID, unrelated.NativeSessionID
	must(t, fixture.store.SaveSubagentCandidate(other))
	fixture.batch.Subagents = append(fixture.batch.Subagents, fixture.batch.Subagents[0], hook.ArchiveSessionID, other.ArchiveSessionID)
	pending, blocked, err := pendingImportChildren(fixture.store, fixture.cfg, fixture.regs, fixture.batch)
	if err != nil || pending != 2 || blocked != 0 {
		t.Fatal("arbitrary candidates or duplicate IDs changed exact import count", pending, blocked, err)
	}
}

func TestPendingImportChildrenReadUnknownSelectedEvidenceWithoutMutation(t *testing.T) {
	t.Parallel()
	for _, selected := range []bool{false, true} {
		t.Run(map[bool]string{false: "unrelated", true: "selected"}[selected], func(t *testing.T) {
			t.Parallel()
			fixture := newPendingChildrenFixture(t)
			id := "synthetic-unrelated-unreadable"
			if selected {
				id = fixture.candidates[0].ArchiveSessionID
			}
			path := filepath.Join(fixture.f.data, "subagent-candidates", id+".json")
			must(t, os.WriteFile(path, []byte("{synthetic-unreadable"), 0600))
			before := snapshotTree(t, filepath.Join(fixture.f.data, "subagent-candidates"))
			history, stderr, code := fixture.f.command(t, "backfill", "history")
			if selected {
				if code != 1 || !strings.Contains(stderr, "read selected imported child") || strings.Contains(history, "uploaded;") {
					t.Error("unknown selected child became completed/absent", code, stderr, history)
				}
			} else if code != 0 || !strings.Contains(history, "2 subagent transcripts waiting") {
				t.Error("unknown unrelated candidate blocked selected import history", code, stderr, history)
			}
			if before != snapshotTree(t, filepath.Join(fixture.f.data, "subagent-candidates")) {
				t.Fatal("read-only history quarantined or changed candidate evidence")
			}
		})
	}
}

func TestPendingImportChildrenDeduplicateRegisteredCandidates(t *testing.T) {
	t.Parallel()
	fixture := newPendingChildrenFixture(t)
	out, stderr, code := fixture.f.importRun(t, nil, false, fixture.args...)
	if code != 0 {
		t.Fatal("materialize and publish existing exact children", code, stderr, out)
	}
	regs, err := fixture.store.LoadRegistrations()
	must(t, err)
	for _, candidate := range fixture.candidates {
		// A replay can leave the exact candidate after registration. Its
		// completed stage has already been released and must not be reread.
		must(t, fixture.store.SaveSubagentCandidate(candidate))
	}
	pending, blocked, err := pendingImportChildren(fixture.store, fixture.cfg, regs, fixture.batch)
	if err != nil || pending != 0 || blocked != 0 {
		t.Fatal("registered candidate was double-counted or reopened", pending, blocked, err)
	}
}

func TestPendingImportChildPolicyUsesFrozenChildRegistration(t *testing.T) {
	t.Parallel()
	fixture := newPendingChildrenFixture(t)
	candidate := fixture.candidates[0]
	var parent archive.SessionRegistration
	for _, reg := range fixture.regs {
		if reg.ArchiveSessionID == candidate.ParentArchiveSessionID {
			parent = reg
		}
	}
	// Exercise the accounting helper's policy projection independently: the
	// admitted-parent anchor cannot substitute its own location for the child.
	parent.ProjectRoot = filepath.Join(fixture.f.userHome, "synthetic-other-project")
	parent.ProjectID = archive.ProjectID(parent.ProjectRoot)
	fixture.cfg.Archive.Projects = []archive.ProjectActivation{
		{Root: parent.ProjectRoot, ProjectID: parent.ProjectID, Included: true},
		{Root: candidate.ProjectRoot, ProjectID: candidate.ProjectID, Included: false},
	}
	if !fixture.cfg.AcceptSession(parent) {
		t.Fatal("parent policy control was not accepted")
	}
	allowed, err := pendingImportChildPolicy(fixture.store, fixture.cfg, parent, candidate, fixture.batch.ID)
	if err != nil || allowed {
		t.Fatal("parent policy replaced exact frozen child policy", allowed, err)
	}
}

// A candidate is operational bookkeeping; quarantine cannot erase the first
// retained child stage from the selected batch's completion obligations.
func TestPendingImportChildrenRetainStageAfterCandidateDisappears(t *testing.T) {
	t.Parallel()
	for _, mutation := range []string{"missing", "quarantined"} {
		for _, retained := range []bool{true, false} {
			name := mutation + map[bool]string{true: "/retained", false: "/removed"}[retained]
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				fixture := newPendingChildrenFixture(t)
				writer, err := state.Open(fixture.f.data)
				must(t, err)
				candidate := fixture.candidates[0]
				stage := selectedChildStage(t, fixture, candidate)
				stagePath := filepath.Join(fixture.f.data, "sessions", candidate.ArchiveSessionID, "import-stage", "stage.json")
				before, err := os.ReadFile(stagePath)
				must(t, err)
				if !retained {
					must(t, writer.RemoveImportStage(candidate.ArchiveSessionID))
				}
				makeSelectedCandidateAbsent(t, writer, fixture.f.data, candidate.ArchiveSessionID, mutation)
				current, found, err := fixture.store.LoadImportStage(t.Context(), stage.Registration)
				must(t, err)
				if found != retained || found && current.SHA256 != stage.SHA256 {
					t.Fatal("candidate removal changed retained-stage control")
				}
				if retained {
					after, err := os.ReadFile(stagePath)
					must(t, err)
					if !bytes.Equal(before, after) {
						t.Fatal("candidate removal rewrote retained stage bytes")
					}
				}
				other := fixture.candidates[1]
				actual, otherFound, err := fixture.store.LoadSubagentCandidate(other.ArchiveSessionID)
				must(t, err)
				if !otherFound || actual.StagedImportSHA256 != other.StagedImportSHA256 {
					t.Fatal("other selected candidate was changed by the control")
				}
				selectedChildStage(t, fixture, other)
				t.Logf("actual selected candidate %s; exact retained stage=%t; other selected candidate and stage intact", mutation, retained)
				pending, blocked, err := pendingImportChildren(fixture.store, fixture.cfg, fixture.regs, fixture.batch)
				if retained {
					if err == nil && (pending != 2 || blocked != 0) {
						t.Error("retained selected child disappeared from completion accounting", pending, blocked, err)
					}
				} else if err != nil || pending != 1 || blocked != 0 {
					t.Error("fully removed child stage remained a completion obligation", pending, blocked, err)
				}
			})
		}
	}
}

func selectedChildStage(t *testing.T, fixture pendingChildrenFixture, candidate state.SubagentCandidate) state.ImportStage {
	t.Helper()
	stages, _, err := fixture.store.ImportStages(t.Context(), "", 50)
	must(t, err)
	for _, stage := range stages {
		if stage.Registration.ArchiveSessionID == candidate.ArchiveSessionID {
			full, found, err := fixture.store.LoadImportStage(t.Context(), stage.Registration)
			must(t, err)
			if !found || full.SHA256 != candidate.StagedImportSHA256 || !full.Registration.InBatch(fixture.batch.ID) {
				t.Fatal("missing exact selected child stage")
			}
			return full
		}
	}
	t.Fatal("selected child stage descriptor absent")
	return state.ImportStage{}
}

func makeSelectedCandidateAbsent(t *testing.T, writer *state.Store, home, id, mutation string) {
	t.Helper()
	if mutation == "missing" {
		must(t, writer.RemoveSubagentCandidate(id))
	} else {
		path := filepath.Join(home, "subagent-candidates", id+".json")
		broken := []byte("{synthetic-unreadable")
		must(t, os.WriteFile(path, broken, 0600))
		candidates, issues, err := writer.ScanSubagentCandidates()
		must(t, err)
		if len(candidates) != 1 || len(issues) != 1 || !errors.Is(issues[id], state.ErrQuarantined) {
			t.Fatal("actual writable candidate scan did not isolate selected decode failure")
		}
		quarantined := writer.QuarantinedFiles()
		if len(quarantined) != 1 {
			t.Fatal("selected candidate quarantine evidence absent")
		}
		actual, err := os.ReadFile(filepath.Join(home, quarantined[0]))
		must(t, err)
		if !bytes.Equal(actual, broken) {
			t.Fatal("candidate quarantine changed original decode-failure bytes")
		}
	}
	_, found, err := writer.LoadSubagentCandidate(id)
	must(t, err)
	if found {
		t.Fatal("selected candidate was not actually absent")
	}
}
