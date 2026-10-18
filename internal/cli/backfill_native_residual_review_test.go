package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/backfill"
)

type reviewJSONField string

const (
	reviewJSONFieldNativeResiduals reviewJSONField = "NativeResiduals"
	reviewJSONFieldNativeSnake     reviewJSONField = "native_residuals"
	reviewJSONFieldRecoveryReview  reviewJSONField = "RecoveryReview"
	reviewJSONFieldRecoverySnake   reviewJSONField = "recovery_review"
)

func TestRecoveryReviewUsesFinalNativeResidualsAfterExactMapping(t *testing.T) {
	// The existing synthetic Git fixture isolates process HOME and Git settings.
	f := newRecoveryReviewFixture(t)
	projects := f.cfg.Archive.Projects
	f.cfg.Archive.Projects = nil
	f.save(t)
	f.codex(t, "11111111-1111-4111-8111-111111111111", f.missing, `,"forked_from_id":"33333333-3333-4333-8333-333333333333","forked_from_ordinal_exclusive":2`)
	f.codex(t, "22222222-2222-4222-8222-222222222222", f.missing, `,"forked_from_id":"33333333-3333-4333-8333-333333333333","forked_from_ordinal_exclusive":1`)
	before := snapshotTree(t, f.home)
	output := filepath.Join(t.TempDir(), "unselected.json")
	out, errout, code := f.command(t, "skip\n", "backfill", "review-projects", "--harness", "codex", "--output", output)
	if code != 0 || !strings.Contains(out, "Native history/own-task evidence unresolved or unchecked: 2") || strings.Contains(out, "Native residual:") {
		t.Fatal("uninspected sources gained native residual evidence", code, out, errout)
	}
	if snapshotTree(t, f.home) != before {
		t.Fatal("private unchecked review changed archive state")
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Fatal("skipped review wrote an assignment", err)
	}
	t.Log("actual private review left project-blocked native evidence unchecked")
	f.cfg.Archive.Projects = projects
	f.save(t)
	before = snapshotTree(t, f.home)
	var observed backfill.Plan
	f.env.reviewBackfillProjects = true
	f.env.observeBackfillPlan = func(plan backfill.Plan) { observed = plan }
	out, errout, code = f.command(t, "", "backfill", "--dry-run", "--json", "--harness", "codex", "--map-project", f.missing+"="+f.target)
	if code != 0 || observed.RecoveryReview == nil || len(observed.RecoveryReview.Groups) != 1 {
		t.Fatal("exact mapping did not reach native qualification", code, out, errout)
	}
	group := observed.RecoveryReview.Groups[0]
	if group.Candidates != 2 || group.PrimaryReasons[backfill.SkipRelatedHistory] != 2 || group.DiagnosticDetails[backfill.DetailOwnTaskMissing] != 2 || group.NativeResiduals[backfill.DetailNoOwnRecordsInSnapshot] != 1 || group.NativeResiduals[backfill.DetailOwnRecordsWithoutTask] != 1 {
		t.Fatal("private review lost finalized native observations", group)
	}
	var rendered bytes.Buffer
	renderRecoveryGroup(&rendered, group)
	for _, want := range []string{"Native residual: no_own_records_in_snapshot = 1", "Native residual: own_records_without_task = 1"} {
		if !strings.Contains(rendered.String(), want) {
			t.Fatal("private review omitted native residual", rendered.String())
		}
	}
	var structured struct {
		Diagnostics []backfill.DiagnosticSummary `json:"diagnostics"`
	}
	must(t, json.Unmarshal([]byte(out), &structured))
	if len(structured.Diagnostics) != 2 || structured.Diagnostics[0].Count != 1 || structured.Diagnostics[1].Count != 1 {
		t.Fatal("ordinary summary merged distinct native residuals", out)
	}
	var document any
	must(t, json.Unmarshal([]byte(out), &document))
	var checkPrivateKeys func(any)
	checkPrivateKeys = func(value any) {
		switch value := value.(type) {
		case map[string]any:
			for key, child := range value {
				switch reviewJSONField(key) {
				case reviewJSONFieldNativeResiduals, reviewJSONFieldNativeSnake, reviewJSONFieldRecoveryReview, reviewJSONFieldRecoverySnake:
					t.Errorf("ordinary JSON exposed private field %q", key)
				}
				checkPrivateKeys(child)
			}
		case []any:
			for _, child := range value {
				checkPrivateKeys(child)
			}
		}
	}
	checkPrivateKeys(document)
	after := snapshotTree(t, f.home)
	if after != before {
		// Snapshot entries contain relative names, type/mode, size, mtime and
		// digest, never file contents. Report only changed entries.
		for _, snapshot := range []struct {
			phase string
			value string
			other string
		}{{"before", before, after}, {"after", after, before}} {
			for line := range strings.SplitSeq(snapshot.value, "\n") {
				if line != "" && !strings.Contains("\n"+snapshot.other+"\n", "\n"+line+"\n") {
					t.Logf("changed archive entry %s: %s", snapshot.phase, line)
				}
			}
		}
		t.Fatal("native residual preview changed archive state")
	}
	t.Log("actual qualified native residuals stayed private; exact mapped preview preserved archive state")
}
