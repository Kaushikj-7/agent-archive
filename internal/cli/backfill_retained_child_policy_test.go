package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

func TestBackfillRetainedChildrenSurviveTemporaryParentPolicyHold(t *testing.T) {
	checkRetainedChildrenPolicyHold(t, func(cfg *config.Config) {
		for i := range cfg.Archive.Projects {
			cfg.Archive.Projects[i].Included = false
		}
	})
}

func TestBackfillRetainedChildrenSurviveDisabledImportedHarness(t *testing.T) {
	checkRetainedChildrenPolicyHold(t, func(cfg *config.Config) {
		cfg.Harnesses = []string{"codex"}
		cfg.ImportedHarnesses = nil
	})
}

func checkRetainedChildrenPolicyHold(t *testing.T, hold func(*config.Config)) {
	t.Helper()
	fixture := prepareUnregisteredChildRetry(t)
	parents, children := importRegistrations(t, fixture.f.data, firstImport)
	if len(parents) != 1 || len(children) != 0 {
		t.Fatal("missing actual admitted-parent/unregistered-child boundary")
	}
	if _, found, err := fixture.store.LoadImportStage(t.Context(), parents[0]); err != nil || found {
		t.Fatal("parent selection was not settled before policy hold", found, err)
	}
	must(t, os.RemoveAll(filepath.Join(fixture.f.userHome, ".claude")))
	assertRetryNativeDeleted(t, fixture.f.userHome)
	beforeCandidates, beforeStages := map[string][]byte{}, map[string][]byte{}
	for id := range fixture.children {
		var err error
		beforeCandidates[id], err = os.ReadFile(filepath.Join(fixture.f.data, "subagent-candidates", id+".json"))
		must(t, err)
		beforeStages[id], err = os.ReadFile(filepath.Join(fixture.f.data, "sessions", id, "import-stage", "stage.json"))
		must(t, err)
	}
	beforeRemote := bucketSnapshot(t, fixture.bucket)
	allowed := mustLoadConfig(t, fixture.f.data)
	held := allowed
	held.Archive.Projects = append([]archive.ProjectActivation(nil), allowed.Archive.Projects...)
	hold(&held)
	if held.AcceptSession(parents[0]) || !allowed.AcceptSession(parents[0]) {
		t.Fatal("temporary policy control did not isolate publication permission")
	}
	must(t, config.Save(fixture.f.data, held))
	out, stderr, code := fixture.f.importRun(t, nil, false, fixture.args...)
	if code != 1 || strings.Contains(out, "Uploaded ") {
		t.Fatal("policy hold did not refuse completion", code, stderr, out)
	}
	if beforeRemote != bucketSnapshot(t, fixture.bucket) {
		t.Fatal("policy-held retained retry changed remote objects")
	}
	parents, children = importRegistrations(t, fixture.f.data, firstImport)
	if len(parents) != 1 || len(children) != 0 {
		t.Fatal("policy refusal changed admitted logical membership")
	}
	checkRetainedParentReadback(t, fixture, parents[0])
	for id, stage := range fixture.children {
		current, found, err := fixture.store.LoadImportStage(t.Context(), stage.Registration)
		must(t, err)
		after, err := os.ReadFile(filepath.Join(fixture.f.data, "sessions", id, "import-stage", "stage.json"))
		must(t, err)
		if !found || current.SHA256 != stage.SHA256 || !bytes.Equal(beforeStages[id], after) {
			t.Fatal("temporary policy refusal changed exact retained child evidence")
		}
	}
	t.Log("actual native-deleted policy hold: parent settled and remotely read back; two unregistered exact child stages retained; no remote changes or child admission")
	for id := range fixture.children {
		candidate, found, err := fixture.store.LoadSubagentCandidate(id)
		must(t, err)
		if !found {
			t.Error("temporary parent policy refusal discarded selected retained child continuation")
			continue
		}
		after, err := os.ReadFile(filepath.Join(fixture.f.data, "subagent-candidates", id+".json"))
		must(t, err)
		if candidate.StagedImportSHA256 != fixture.children[id].SHA256 || !bytes.Equal(beforeCandidates[id], after) {
			t.Fatal("policy hold rewrote exact child continuation")
		}
	}
	must(t, config.Save(fixture.f.data, allowed))
	out, stderr, code = fixture.f.importRun(t, nil, false, fixture.args...)
	if code != 0 {
		t.Error("restored parent policy could not resume native-deleted children", code, stderr, out)
	}
	checkMaterializedChildren(t, fixture.f, fixture.store, fixture.bucket, fixture.children)
}
