package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/discovery"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

func writeNativeImportJSON(out io.Writer, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return writeOutput(out, append(archive.DisplayJSON(raw), '\n'))
}

func nativeImportBatchFilters(opts backfillCommandOptions, projects []string) backfill.BatchFilters {
	return backfill.BatchFilters{Harnesses: []string{"muse"}, ProjectIDs: append([]string{}, projects...), Since: opts.filters.Since, Until: opts.filters.Until, SinceArg: opts.filters.SinceArg, UntilArg: opts.filters.UntilArg}
}

func planNativeImport(ctx context.Context, env Env, home string, cfg config.Config, opts backfillCommandOptions) (p nativeImportPreview, err error) {
	p.ChildCoverage, p.Destination = "unavailable", cfg.DestinationID()
	p.Rows = []nativeImportRow{}
	p.projects, err = nativeImportProjectFilters(env, cfg, opts)
	if err != nil {
		return p, err
	}
	p.SinceUTC, p.UntilUTC, err = nativeImportBounds(opts, env.now())
	if err != nil {
		return p, err
	}
	if err := planNativeImportResume(ctx, home, cfg, opts, &p); err != nil {
		return p, err
	}
	if p.resume != nil {
		return p, nil
	}
	p.budget = agentapi.NewNativeReadBudget(agentapi.PassReadBudget)
	// Existing confirmation bounds cover this retained bounded projection.
	if !p.budget.ReserveUnscoped(archive.MaxNativeImportBatchBytes) {
		return p, agentapi.ErrReadBudget
	}
	p.charge = archive.MaxNativeImportBatchBytes
	p.inventory, err = discovery.NewNativeImportInventory(ctx, cfg.NativeRoots, p.budget, builtin.ValidateNativeLocator)
	if err != nil {
		return p, errors.Join(err, p.close())
	}
	keep := false
	defer func() {
		if !keep {
			err = errors.Join(err, p.close())
		}
	}()
	candidates, err := p.inventory.Candidates(ctx)
	if err != nil {
		return p, err
	}
	p.Complete = candidates.Complete
	if len(candidates.IDs) > archive.MaxNativeImportBatchSelections {
		return p, errors.New("historical preview exceeds the existing 256-entry confirmation limit; select a narrower native store before retrying")
	}
	store, closeRead := state.OpenReadOnly(home).WithReadBudget(ctx, p.budget)
	defer closeRead()
	resolver := sourcefacts.NewProjectResolver()
	for _, id := range candidates.IDs {
		row := nativeImportRow{NativeID: id, Status: "unavailable", Reason: "bounded namespace coverage incomplete"}
		if p.Complete {
			row, err = inspectNativeImportRow(ctx, env, store, cfg, opts, &p, resolver, id)
			if err != nil {
				return p, err
			}
		}
		row.Ordinal = len(p.Rows) + 1
		p.Rows = append(p.Rows, row)
		if _, err := agentmeta.JSONWireBound(ctx, p.Rows, archive.MaxNativeImportBatchBytes); err != nil {
			return p, err
		}
	}
	keep = true
	return p, nil
}

func planNativeImportResume(ctx context.Context, home string, cfg config.Config, opts backfillCommandOptions, p *nativeImportPreview) error {
	batches, err := backfill.LoadBatches(home)
	if err != nil {
		return err
	}
	if len(batches) == 0 {
		return nil
	}
	last := batches[len(batches)-1]
	if last.NativeImport == nil || !last.Continues(nativeImportBatchFilters(opts, p.projects), cfg.DestinationID()) {
		return nil
	}
	if err := last.NativeImport.Validate(ctx); err != nil {
		return err
	}
	p.resume, p.Complete = &last, true
	p.SinceUTC, p.UntilUTC = last.NativeImport.Scope.SinceUTC, last.NativeImport.Scope.UntilExclusiveUTC
	for _, selected := range last.NativeImport.Scope.Selected {
		row := nativeImportRow{NativeID: selected.NativeID, ProjectID: selected.ProjectID, Status: "selected", Reason: "resume this exact previously confirmed scope", selection: selected}
		original, err := time.Parse(time.RFC3339Nano, selected.OriginalTUTC)
		row.OriginalT = &original
		if err != nil {
			return err
		}
		for _, project := range cfg.Archive.Projects {
			if project.ProjectID == row.ProjectID && project.Included {
				row.Project = project.Root
			}
		}
		for _, root := range cfg.NativeRoots {
			fingerprint, e := archive.NativeRootFingerprint(root)
			if e == nil && fingerprint == selected.RootFingerprint && root.Revision == selected.RootRevision {
				row.candidate = state.NativeImportCandidate{NativeID: row.NativeID, Root: root, ProjectRoot: row.Project, ProjectID: row.ProjectID}
			}
		}
		if row.Project == "" || row.candidate.Root.Agent != "muse" {
			return errors.New("confirmed root/project configuration changed; restore it or explicitly confirm a new scope")
		}
		row.Ordinal = len(p.Rows) + 1
		p.Rows = append(p.Rows, row)
	}
	return nil
}

func inspectNativeImportRow(ctx context.Context, env Env, store *state.Store, cfg config.Config, opts backfillCommandOptions, p *nativeImportPreview, resolver *sourcefacts.ProjectResolver, id string) (row nativeImportRow, err error) {
	row = nativeImportRow{NativeID: id, Status: "refused"}
	var root archive.NativeRoot
	for _, candidate := range cfg.NativeRoots {
		if candidate.Agent == "muse" {
			root = candidate
		}
	}
	located, err := p.inventory.Locate(ctx, agentapi.LocatorRequest{Purpose: agentapi.LocatorHistoricalImport, NativeID: id, Root: root})
	if err != nil {
		row.Reason = "current native source/proof unavailable; retry this bounded preview"
		return row, nil
	}
	if located.Status != agentapi.LocatorFound || len(located.Candidates) != 1 {
		row.Reason = "native locator " + string(located.Status)
		return row, nil
	}
	row.ref, row.token = located.Candidates[0], located.Revision
	pass, snapshot, err := openNativeImportSource(ctx, env, root, row.ref, p.inventory, p.budget)
	if err != nil {
		row.Reason = "current native source/proof unavailable; retry this bounded preview"
		return row, nil
	}
	defer func() { err = errors.Join(err, snapshot.Close(), pass.Close()) }()
	evidence, ok := snapshot.(agentapi.SourceAdmissionEvidence)
	if !ok {
		return row, agentapi.ErrInvalidSourceBinding
	}
	facts, err := evidence.AdmissionEvidence(ctx, agentapi.SourceAdmission{NativeID: id})
	if err != nil {
		row.Reason = "current native source/proof unavailable; retry this bounded preview"
		return row, nil
	}
	binding := facts.Binding.Native
	if facts.Binding.Codex != nil || binding == nil || binding.Checkpoint == nil || binding.NativeID != id || binding.ParentID != "" || binding.RootID != "" && binding.RootID != id {
		row.Reason = "ordinary root authority unavailable; child authority is unavailable"
		return row, nil
	}
	original := binding.NativeCreatedAt
	row.OriginalT = &original
	day := row.OriginalT.In(env.now().Location()).Format("2006-01-02")
	if opts.filters.Since != "" && day < opts.filters.Since || opts.filters.Until != "" && day > opts.filters.Until {
		row.Status, row.Reason = "filtered", "outside exact original-start date bounds"
		return row, nil
	}
	physical, known := resolver.Resolve(binding.Cwd)
	if !known {
		row.Reason = "physical project/worktree unavailable; mapping overrides are unsupported"
		return row, nil
	}
	for _, project := range cfg.Archive.Projects {
		if project.Included && project.Root == physical.Root {
			row.Project, row.ProjectID = project.Root, project.ProjectID
			break
		}
	}
	if row.ProjectID == "" {
		row.Reason = "physical project is not currently configured and included"
		return row, nil
	}
	if len(p.projects) > 0 && !slices.Contains(p.projects, row.ProjectID) {
		row.Status, row.Reason = "filtered", "outside selected configured project IDs"
		return row, nil
	}
	if _, found, err := store.ArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: id}); err != nil {
		row.Reason = "existing ownership/index requires recovery"
		return row, nil
	} else if found {
		row.Status, row.Reason = "duplicate", "independent existing owner is preserved"
		return row, nil
	}
	if _, removed, err := store.Removal("muse", id); err != nil {
		return row, err
	} else if removed {
		row.Reason = "previous removal is preserved; include-removed is unavailable"
		return row, nil
	}
	fingerprint, err := archive.NativeRootFingerprint(root)
	if err != nil {
		return row, err
	}
	digest, err := archive.NativeImportBindingDigest(binding)
	if err != nil {
		return row, err
	}
	row.selection = archive.NativeImportBatchSelection{Kind: archive.NativeImportOrdinaryRoot, NativeID: id, OriginalTUTC: row.OriginalT.UTC().Format(time.RFC3339Nano), ProjectID: row.ProjectID, RootFingerprint: fingerprint, RootRevision: root.Revision, BindingSHA256: digest, Source: *binding.Checkpoint}
	row.candidate = state.NativeImportCandidate{NativeID: id, Root: root, ProjectRoot: row.Project, ProjectID: row.ProjectID}
	if err := p.inventory.Check(ctx, id, row.token); err != nil {
		row.Reason = "current native source/proof unavailable; retry this bounded preview"
		return row, nil
	}
	row.Status, row.Reason = "selected", "ordinary root; child coverage unavailable"
	return row, nil
}

func openNativeImportSource(ctx context.Context, env Env, root archive.NativeRoot, ref agentapi.SourceRef, lookup agentapi.NativeLocatorLookup, budget *agentapi.NativeReadBudget) (agentapi.SourcePass, agentapi.SourceSnapshot, error) {
	provider, _, ok := env.agentRegistry().LookupSources("muse")
	if !ok {
		return nil, nil, agentapi.ErrInvalidSourceBinding
	}
	pass, err := provider.OpenPass(ctx, agentapi.SourceEnvironment{NativeRoot: &root, NativeLocators: lookup, ReadBudget: budget, Files: transcriptio.RootOpener{Root: root.Canonical}, Policy: transcriptio.OpenPolicy{Root: root.Canonical, RejectSymlinks: true}})
	if err != nil {
		return nil, nil, err
	}
	snapshot, err := pass.Read(ctx, ref, agentapi.ReadLimits{RawBytes: collector.MaxRawTranscriptBytes("muse"), RecordBytes: archive.MaxRecordBytes, FilteredBytes: collector.DefaultMaxTranscriptBytes, ReadBudget: budget})
	if err != nil {
		return nil, nil, errors.Join(err, pass.Close())
	}
	return pass, snapshot, nil
}
