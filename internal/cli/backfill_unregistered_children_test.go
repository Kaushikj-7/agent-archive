package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// The temporary registration directory alone becomes read-only after staging.
// Existing parent publication and every retained child remain independently usable.
func TestBackfillUnregisteredChildrenRemainPendingUntilMaterialized(t *testing.T) {
	f, bucket := newImportFixture(t)
	store := state.OpenReadOnly(f.data)
	registrationDir := filepath.Join(f.data, "registrations")
	t.Cleanup(func() { must(t, os.Chmod(registrationDir, 0700)) })
	children := map[string]state.ImportStage{}
	armed := false
	f.env.backfillCheckpoint = func(step string) error {
		if step != "uploading" {
			return nil
		}
		candidates, issues, err := store.ScanSubagentCandidates()
		must(t, err)
		if len(issues) != 0 || len(candidates) != 2 {
			t.Fatal("missing actual selected child candidates", len(candidates), issues)
		}
		stages, _, err := store.ImportStages(t.Context(), "", 50)
		must(t, err)
		for _, stage := range stages {
			if stage.Registration.ParentSessionID != "" {
				full, found, err := store.LoadImportStage(t.Context(), stage.Registration)
				must(t, err)
				if !found {
					t.Fatal("selected child stage disappeared before fault")
				}
				children[stage.Registration.ArchiveSessionID] = full
			}
		}
		if len(children) != 2 {
			t.Fatal("missing exact durable child selections", len(children))
		}
		armed = true
		return os.Chmod(registrationDir, 0500)
	}
	args := []string{"--yes", "--harness", "claude", "--project", filepath.Join(f.userHome, "agent-archive"), "--since", "2026-09-21"}
	out, stderr, code := f.importRun(t, nil, false, args...)
	if !armed {
		t.Fatal("actual confirmed publication boundary was not reached", code, stderr, out)
	}
	if code != 1 || strings.Contains(out, "Uploaded ") {
		t.Error("unregistered retained children reported completed import", code, stderr, out)
	}
	checkUnregisteredChildrenRetained(t, f, store, bucket, children, stderr, out)

	must(t, os.Chmod(registrationDir, 0700))
	must(t, os.RemoveAll(filepath.Join(f.userHome, ".claude")))
	f.env.backfillCheckpoint = nil
	out, stderr, code = f.importRun(t, nil, false, args...)
	if code != 0 {
		t.Error("same-options native-deleted materialization retry failed", code, stderr, out)
	}
	checkMaterializedChildren(t, f, store, bucket, children)
}

func checkUnregisteredChildrenRetained(t *testing.T, f *backfillFixture, store *state.Store, bucket storage.ObjectStore, children map[string]state.ImportStage, stderr, out string) {
	t.Helper()
	parents, registeredChildren := importRegistrations(t, f.data, firstImport)
	if len(parents) != 1 || len(registeredChildren) != 0 {
		t.Fatal("registration write fault did not isolate the pre-registration boundary", len(parents), len(registeredChildren))
	}
	published, err := store.LoadPublishedState(parents[0].ArchiveSessionID)
	must(t, err)
	metadata, found, err := published.LastPublishedMetadata()
	must(t, err)
	if !found {
		t.Fatal("existing parent failed to publish under the isolated child write fault", stderr, out)
	}
	parentBundle, err := reader.LoadSource(t.Context(), bucket, metadata, reader.Limits{})
	must(t, err)
	if parentBundle.NativeSessionID != parents[0].NativeSessionID || !parentBundle.Capture.CapturedAt.Equal(parents[0].AdmittedAt) {
		t.Fatal("parent publication changed source identity or capture age")
	}
	candidates, issues, err := store.ScanSubagentCandidates()
	must(t, err)
	if len(issues) != 0 || len(candidates) != 2 {
		t.Fatal("failed materialization discarded retained child candidates", len(candidates), issues)
	}
	for _, candidate := range candidates {
		stage, expected := children[candidate.ArchiveSessionID]
		if !expected || candidate.StagedImportSHA256 != stage.SHA256 || candidate.ParentArchiveSessionID != parents[0].ArchiveSessionID {
			t.Fatal("failed materialization changed exact selected child ownership")
		}
		current, found, err := store.LoadImportStage(t.Context(), stage.Registration)
		must(t, err)
		if !found || current.SHA256 != stage.SHA256 {
			t.Fatal("failed materialization lost a selected child stage")
		}
	}
	status, err := store.LoadStatus()
	must(t, err)
	for id := range children {
		if status.SessionIssues[id] == "" {
			t.Fatal("the actual child registration write failure was not recorded", id)
		}
	}
	history, errout, historyCode := f.command(t, "backfill", "history")
	if historyCode != 0 || !strings.Contains(history, "2 subagent transcripts waiting") {
		t.Error("history omitted unregistered retained children", historyCode, errout, history)
	}
	t.Logf("actual parent publication/readback complete; child registrations=0 retained candidates=%d exact stages=%d; temporary registration directory write failures recorded for both children", len(candidates), len(children))
}

func checkMaterializedChildren(t *testing.T, f *backfillFixture, store *state.Store, bucket storage.ObjectStore, children map[string]state.ImportStage) {
	t.Helper()
	parents, registeredChildren := importRegistrations(t, f.data, firstImport)
	if len(parents) != 1 || len(registeredChildren) != 2 {
		t.Error("same-options retry omitted retained logical child owners", len(parents), len(registeredChildren))
	}
	for _, child := range registeredChildren {
		stage, expected := children[child.ArchiveSessionID]
		if !expected || child.StagedImportSHA256 != stage.SHA256 || !child.AdmittedAt.Equal(stage.Registration.AdmittedAt) {
			t.Fatal("materialization retry changed child identity or admission age")
		}
		published, err := store.LoadPublishedState(child.ArchiveSessionID)
		must(t, err)
		metadata, found, err := published.LastPublishedMetadata()
		must(t, err)
		if !found {
			t.Error("materialized child not published", child.ArchiveSessionID)
			continue
		}
		bundle, err := reader.LoadSource(t.Context(), bucket, metadata, reader.Limits{})
		must(t, err)
		if bundle.NativeSessionID != child.NativeSessionID || !bundle.Capture.CapturedAt.Equal(stage.Bundle.Capture.CapturedAt) {
			t.Fatal("retained child readback changed source identity or capture age")
		}
		var want, got bytes.Buffer
		must(t, archive.EncodeSource(&want, stage.Bundle))
		must(t, archive.EncodeSource(&got, bundle))
		if !bytes.Equal(want.Bytes(), got.Bytes()) {
			t.Fatal("retained child source bytes changed during materialization retry")
		}
		_, requested, err := store.LoadRequest(child.ArchiveSessionID)
		must(t, err)
		owed, err := store.Outstanding(child, requested)
		must(t, err)
		if owed.Pending() {
			t.Error("verified retained child still owes local settlement", owed)
		}
	}
}
