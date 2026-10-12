package cli

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/state"
)

func TestBackfillDryRunReportsPendingIndexWithoutChangingState(t *testing.T) {
	f, _ := newImportFixture(t)
	store, err := state.Open(f.data)
	must(t, err)
	must(t, store.MarkSessionIndexRecoveryNeeded())
	before := snapshotTree(t, f.data)
	var out, errOut bytes.Buffer
	code := Run([]string{"backfill", "--dry-run", "--json", "--harness", "codex"}, nil, &out, &errOut, f.env)
	if code != 0 {
		t.Fatalf("dry-run aborted: %d %s", code, errOut.String())
	}
	var plan struct {
		Diagnostics []backfill.DiagnosticSummary `json:"diagnostics"`
		Skipped     map[string]int               `json:"skipped"`
		Projects    []struct {
			Sessions map[string]int `json:"sessions"`
		} `json:"projects"`
	}
	must(t, json.Unmarshal(out.Bytes(), &plan))
	if plan.Skipped[string(backfill.SkipIndexRecovery)] != 1 {
		t.Fatalf("lost pending index: %s", out.String())
	}
	found := false
	for _, d := range plan.Diagnostics {
		if d.Skip == backfill.SkipIndexRecovery && d.Detail == backfill.DetailSessionIndexRecovery && d.Action == backfill.ActionRetry && d.Count == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing recovery diagnostic: %s", out.String())
	}
	for _, p := range plan.Projects {
		if p.Sessions["codex"] != 0 {
			t.Fatal("unresolved identity selected for import")
		}
	}
	if snapshotTree(t, f.data) != before {
		t.Fatal("dry-run wrote index recovery state")
	}
	out.Reset()
	errOut.Reset()
	code = Run([]string{"backfill", "--yes", "--background", "--harness", "codex"}, nil, &out, &errOut, f.env)
	if code != 0 {
		t.Fatalf("pending import failed: %d %s", code, errOut.String())
	}
	if snapshotTree(t, f.data) != before {
		t.Fatal("pending identity was admitted or modified")
	}
	// The collector's ordinary recovery restores eligibility; the preview has not
	// installed a sticky refusal or fabricated an absence entry.
	complete, err := store.RecoverSessionIndexScheduled(t.Context(), state.SessionIndexRecoverySlice)
	must(t, err)
	if !complete {
		t.Fatal("small recovery did not finish")
	}
	out.Reset()
	errOut.Reset()
	code = Run([]string{"backfill", "--dry-run", "--json", "--harness", "codex"}, nil, &out, &errOut, f.env)
	if code != 0 {
		t.Fatalf("recovered preview failed: %d %s", code, errOut.String())
	}
	plan.Projects = nil
	plan.Skipped = nil
	plan.Diagnostics = nil
	must(t, json.Unmarshal(out.Bytes(), &plan))
	if plan.Skipped[string(backfill.SkipIndexRecovery)] != 0 {
		t.Fatal("recovered identity stayed pending")
	}
	eligible := 0
	for _, p := range plan.Projects {
		eligible += p.Sessions["codex"]
	}
	if eligible != 1 {
		t.Fatalf("recovered source not eligible: %s", out.String())
	}
}
