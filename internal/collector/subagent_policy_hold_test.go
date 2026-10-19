package collector

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
)

type retainedChildStageCondition string

const (
	retainedChildStageValid   retainedChildStageCondition = "valid"
	retainedChildStageMissing retainedChildStageCondition = "missing"
	retainedChildStageCorrupt retainedChildStageCondition = "corrupt"
)

func TestImportedChildPolicyHoldRequiresExactRetainedStage(t *testing.T) {
	for _, condition := range []retainedChildStageCondition{retainedChildStageValid, retainedChildStageMissing, retainedChildStageCorrupt} {
		t.Run(string(condition), func(t *testing.T) {
			store, parent, child, opts := stagedUnadmittedGroupFixture(t)
			if err := store.ResumeAllImportAdmissions(t.Context()); err != nil {
				t.Fatal(err)
			}
			candidate, found, err := store.LoadSubagentCandidate(child.ArchiveSessionID)
			if err != nil || !found || candidate.StagedImportSHA256 != child.StagedImportSHA256 {
				t.Fatal("actual prepared child continuation missing", found, err)
			}
			for _, path := range []string{parent.TranscriptPath, child.TranscriptPath} {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(store.Home(), "sessions", child.ArchiveSessionID, "import-stage", "stage.json")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch condition {
			case retainedChildStageMissing:
				err = os.Remove(path)
			case retainedChildStageCorrupt:
				before = []byte("{synthetic-invalid-stage")
				err = os.WriteFile(path, before, 0600)
			case retainedChildStageValid:
			}
			if err != nil {
				t.Fatal(err)
			}
			opts.AcceptSession = func(archive.SessionRegistration) bool { return false }
			err = materializeSubagentCandidate(t.Context(), store, candidate, opts, parent.AdmittedAt)
			if condition == retainedChildStageValid {
				if !errors.Is(err, state.ErrReadySelectionFrozen) {
					t.Fatal("validated retained child was not held by policy", err)
				}
			} else if err == nil || errors.Is(err, state.ErrReadySelectionFrozen) {
				t.Fatal("missing or invalid evidence became a validated policy hold", err)
			}
			current, found, loadErr := store.LoadSubagentCandidate(child.ArchiveSessionID)
			if loadErr != nil || !found || current != candidate {
				t.Fatal("refused child continuation changed", found, loadErr)
			}
			if _, found, err := store.LoadRegistration(child.ArchiveSessionID); err != nil || found {
				t.Fatal("refused child was registered", found, err)
			}
			if condition != retainedChildStageMissing {
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatal("refused retained evidence changed", err)
				}
			}
		})
	}
}

func TestPolicyRejectedHookChildKeepsPermanentRefusal(t *testing.T) {
	store, at, path := subagentGraceFixture(t, archive.SessionOriginHook)
	writeChildTranscript(t, path)
	candidate, found, err := store.LoadSubagentCandidate("child")
	if err != nil || !found {
		t.Fatal("actual hook candidate missing", found, err)
	}
	err = materializeSubagentCandidate(t.Context(), store, candidate, Options{Sources: refuseNativeSources{t}, AcceptSession: func(archive.SessionRegistration) bool { return false }}, at)
	var rejected subagentRejectedError
	if !errors.As(err, &rejected) || rejected.code != "subagent_parent_ownership_unavailable" {
		t.Fatal("ordinary hook policy refusal changed", err)
	}
	if _, found, err := store.LoadSubagentCandidate("child"); err != nil || found {
		t.Fatal("permanently refused hook candidate retained", found, err)
	}
	if _, found, err := store.LoadRegistration("child"); err != nil || found {
		t.Fatal("policy-refused hook child registered", found, err)
	}
}
