package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/discovery"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
)

func TestBackfillRegistrationSequenceKeepsOneBatchAcrossWindows(t *testing.T) {
	t.Parallel()
	f, remote := newImportFixture(t)
	root := filepath.Join(f.userHome, "agent-archive")
	for i := range 53 {
		id := fmt.Sprintf("paged-registration-%03d", i)
		// Equal starts cross the actual 50-parent registration window.
		f.write(t, filepath.Join(".claude", "projects", "paged", id+".jsonl"), fmt.Sprintf(`{"type":"user","sessionId":%q,"cwd":%q,"timestamp":%q,"message":{"role":"user","content":"synthetic window prompt"}}`+"\n", id, root, backfillNow.Add(-24*time.Hour).Format(time.RFC3339)))
	}
	var plan backfill.Plan
	f.env.observeBackfillPlan = func(p backfill.Plan) { plan = p }
	var owners []*discovery.CodexRolloutLookup
	f.env.observeBackfillLookup = func(owner *discovery.CodexRolloutLookup) { owners = append(owners, owner) }
	staged := map[string]bool{}
	independentStaged := 0
	f.env.backfillCheckpoint = func(step string) error {
		if step != "staged" {
			return nil
		}
		store, closeStore := state.OpenReadOnly(f.data).WithImportResultScope(t.Context())
		defer closeStore()
		for after := ""; ; {
			stages, next, err := store.ImportStages(t.Context(), after, 50)
			if err != nil {
				return err
			}
			for _, stage := range stages {
				reg := stage.Registration
				if staged[reg.ArchiveSessionID] {
					continue
				}
				staged[reg.ArchiveSessionID] = true
				if independentImportedSession(reg) {
					independentStaged++
				}
				// Publication and later registration steps must use frozen evidence.
				if err := os.Remove(reg.TranscriptPath); err != nil {
					return err
				}
			}
			if next == "" {
				break
			}
			after = next
		}
		return nil
	}
	out, errout, code := f.importRun(t, nil, false, "--yes", "--harness", "claude", "--project", root)
	if code != 0 {
		t.Fatalf("actual confirmed sequence: code=%d stderr=%s stdout=%s", code, errout, out)
	}
	want := len(plan.Imported())
	if want <= 50 || independentStaged != want {
		t.Fatal("fixture did not cross actual registration windows", want, independentStaged)
	}
	batches, err := backfill.LoadBatches(f.data)
	must(t, err)
	if len(batches) != 1 || batches[0].ID != firstImport || batches[0].CompletedAt == nil {
		t.Fatal("registration windows created separate or unfinished batches", batches)
	}
	batch := batches[0]
	defer batch.CloseStagedResults()
	cfg, _, err := config.Load(f.data)
	must(t, err)
	if !batch.Matches(plan.BatchFilters(), cfg.DestinationID()) || len(batch.Sessions) != want {
		t.Fatal("semantic selection/destination changed at a window", batch.Filters, len(batch.Sessions), want)
	}
	if len(staged) != len(batch.Sessions)+len(batch.Subagents) || len(batch.Subagents) != 2 {
		t.Fatal("selected dependent children lost at a window", len(staged), len(batch.Sessions), len(batch.Subagents))
	}
	store := state.OpenReadOnly(f.data)
	ids := append(append([]string(nil), batch.Sessions...), batch.Subagents...)
	for _, id := range ids {
		reg, found, err := store.LoadRegistration(id)
		must(t, err)
		if !found || !reg.InBatch(batch.ID) || reg.Origin != archive.SessionOriginImport || !reg.AdmittedAt.Equal(batch.StartedAt) {
			t.Fatal("window changed immutable import ownership", reg)
		}
		key, err := archive.MetadataObjectKey(reg.Harness.Name, id)
		must(t, err)
		metadata, err := reader.ReadMetadata(t.Context(), remote, key)
		must(t, err)
		source, err := remote.Get(t.Context(), metadata.SourceBundle.Key)
		must(t, err)
		bundle, err := archive.ReadSourceBundle(bytes.NewReader(source), archive.DecodeOptions{})
		must(t, err)
		if len(bundle.NativeRecords) == 0 || metadata.Origin != archive.SessionOriginImport || metadata.ImportedAt == nil || !metadata.ImportedAt.Equal(reg.AdmittedAt) {
			t.Fatal("remote readback lost selected provenance", id)
		}
		if owed, err := importOutstanding(store, cfg, reg); err != nil || owed.Pending() {
			t.Fatal("sequence reported success before durable publication", owed, err)
		}
	}
	if !strings.Contains(out, fmt.Sprintf("Registered %d sessions", want)) || len(owners) < 2 {
		t.Fatal("actual scalar registration path was not observed", out, len(owners))
	}
	for _, owner := range owners {
		if used, _ := owner.NativeReadBudget().Charged(); used != 0 {
			t.Fatal("registration lookup/state owner retained charges", used)
		}
	}
}

func TestBackfillRegistrationUsesLookupLedger(t *testing.T) {
	t.Parallel()
	f, remote := newImportFixture(t)
	committed := false
	var owner *discovery.CodexRolloutLookup
	var external int64
	var prepared state.ImportStage
	f.env.observeBackfillLookup = func(current *discovery.CodexRolloutLookup) {
		if committed {
			owner = current
		}
	}
	f.env.backfillCheckpoint = func(step string) error {
		committed = committed || step == "committed"
		if step != "staged" {
			return nil
		}
		if owner == nil || external != 0 {
			t.Fatal("single-parent staged boundary was not exact", owner != nil, external)
		}
		stages, _, err := state.OpenReadOnly(f.data).ImportStages(t.Context(), "", 50)
		if err != nil || len(stages) != 1 {
			t.Fatal("single selected preparation missing", len(stages), err)
		}
		prepared = stages[0]
		external = owner.NativeReadBudget().Available()
		if !owner.NativeReadBudget().ReserveUnscoped(external) {
			t.Fatal("synthetic shared-owner pressure refused")
		}
		return nil
	}
	root := filepath.Join(f.userHome, "agent-archive")
	args := []string{"--yes", "--harness", "claude", "--project", root, "--since", "2026-09-19", "--until", "2026-09-19"}
	_, _, code := f.importRun(t, nil, false, append(args, "--background")...)
	if owner == nil || prepared.SHA256 == "" || external == 0 || code != 1 {
		t.Fatal("actual staged registration boundary was not exercised", owner != nil, prepared.SHA256 != "", external, code)
	}
	parents, children := importRegistrations(t, f.data, firstImport)
	requests, err := state.OpenReadOnly(f.data).LoadRequests()
	must(t, err)
	if len(parents)+len(children)+len(requests) != 0 {
		t.Fatal("shared-ledger refusal admitted or queued unreported work", len(parents), len(children), len(requests))
	}
	before, _ := loadBatch(t, f.data)
	defer before.CloseStagedResults()
	if before.CompletedAt != nil {
		t.Fatal("unacknowledged prepared parent completed the batch")
	}
	if used, _ := owner.NativeReadBudget().Charged(); used != external {
		t.Fatal("close consumed another retained owner", used, external)
	}
	owner.NativeReadBudget().ReleaseUnscoped(external)
	if used, _ := owner.NativeReadBudget().Charged(); used != 0 {
		t.Fatal("shared owner leaked", used)
	}
	store := state.OpenReadOnly(f.data)
	stage, found, err := store.LoadImportStage(t.Context(), prepared.Registration)
	must(t, err)
	if !found || stage.SHA256 != prepared.SHA256 {
		t.Fatal("resource refusal lost exact retained preparation", found)
	}
	must(t, os.Remove(prepared.Registration.TranscriptPath))
	f.env.backfillCheckpoint, f.env.observeBackfillLookup = nil, nil
	out, errout, code := f.importRun(t, nil, false, args...)
	if code != 0 {
		t.Fatal("funded same-batch retry failed", code, errout, out)
	}
	after, _ := loadBatch(t, f.data)
	defer after.CloseStagedResults()
	if after.ID != before.ID || !after.StartedAt.Equal(before.StartedAt) || after.CompletedAt == nil || len(after.Sessions) != 1 || after.Sessions[0] != prepared.Registration.ArchiveSessionID {
		t.Fatal("funded retry changed exact batch/owner", after)
	}
	reg, found, err := store.LoadRegistration(after.Sessions[0])
	must(t, err)
	if !found || reg.StagedImportSHA256 != prepared.SHA256 || !reg.AdmittedAt.Equal(prepared.Registration.AdmittedAt) || !reg.InBatch(before.ID) {
		t.Fatal("funded retry reinterpreted provenance", reg)
	}
	key, err := archive.MetadataObjectKey(reg.Harness.Name, reg.ArchiveSessionID)
	must(t, err)
	metadata, err := reader.ReadMetadata(t.Context(), remote, key)
	must(t, err)
	raw, err := remote.Get(t.Context(), metadata.SourceBundle.Key)
	must(t, err)
	bundle, err := archive.ReadSourceBundle(bytes.NewReader(raw), archive.DecodeOptions{})
	must(t, err)
	if bundle.NativeSessionID != reg.NativeSessionID || metadata.ImportedAt == nil || !metadata.ImportedAt.Equal(reg.AdmittedAt) || len(bundle.NativeRecords) == 0 {
		t.Fatal("funded retry lacked exact native-free remote readback")
	}
}
