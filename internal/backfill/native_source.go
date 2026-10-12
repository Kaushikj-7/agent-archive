package backfill

import (
	"context"
	"errors"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

// filterImportSource validates native ownership on the same snapshot it filters.
// Historical admission belongs to the import; ancestry supplies no permission.
func filterImportSource(ctx context.Context, env Environment, w *work, started time.Time) (out archive.FilteredTranscript, release func(), err error) {
	release = func() {}
	ref := agentapi.SourceRef{Kind: w.c.SourceKind, Path: w.t.path, Key: w.c.SourceKey}
	if ref.Kind == "" {
		ref.Kind = archive.SourceKindFile
	}
	if string(w.t.harness) != "codex" || !w.c.NativeChild && !w.c.RelatedHistory || env.CodexRollouts == nil {
		out, _, err = collector.FilterSource(ctx, string(w.t.harness), ref, started, env.Sources)
		return out, release, err
	}
	provider, filter, ok := env.Sources.LookupSources(string(w.t.harness))
	if !ok {
		return out, release, errors.New("native source capability unavailable")
	}
	root := w.c.NativeHome
	var budget *agentapi.NativeReadBudget
	if shared, ok := env.CodexRollouts.(agentapi.CodexRolloutResourceBudget); ok {
		budget = shared.NativeReadBudget()
	}
	environment := agentapi.SourceEnvironment{ReadBudget: budget, CodexRollouts: env.CodexRollouts, RequireConfinedHistory: true, Files: sourcefacts.RootOpener{Root: root}, Policy: transcriptio.OpenPolicy{Root: root, RejectSymlinks: true}}
	pass, err := provider.OpenPass(ctx, environment)
	if err != nil {
		return out, release, err
	}
	defer func() { err = errors.Join(err, pass.Close()) }()
	limits := agentapi.ReadLimits{RawBytes: collector.MaxRawTranscriptBytes(string(w.t.harness)), RecordBytes: archive.MaxRecordBytes, FilteredBytes: collector.DefaultMaxTranscriptBytes}
	snapshot, err := pass.Read(ctx, ref, limits)
	if err != nil {
		return out, release, err
	}
	defer func() { err = errors.Join(err, snapshot.Close()) }()
	admission, ok := snapshot.(agentapi.SourceAdmissionEvidence)
	if !ok {
		return out, release, agentapi.Wrap(agentapi.Unavailable, errors.New("native admission evidence unavailable"))
	}
	evidence, err := admission.AdmissionEvidence(ctx, agentapi.SourceAdmission{NativeID: w.c.NativeSessionID, Cwd: w.t.cwd})
	if err != nil {
		return out, release, err
	}
	// Children and supported forks require their first own native task.
	// Ordinary historical and paginated imports retain their compatibility.
	if evidence.Binding.RequiresOwnTask() && !evidence.Task.ValidNativeCreation(evidence.Binding.NativeCreatedAt) {
		return out, release, agentapi.Wrap(agentapi.Unavailable, errors.New("native own task unavailable"))
	}
	if leased, ok := filter.(agentapi.LeasedTranscriptFilter); ok && leased.LeasedFilterFor(filter) {
		out, release, err = leased.FilterLeased(ctx, snapshot.Input(), agentapi.FilterContext{Filename: ref.Path, StartedAt: started, Limits: limits}, environment.ReadBudget)
	} else {
		out, err = filter.Filter(ctx, snapshot.Input(), agentapi.FilterContext{Filename: ref.Path, StartedAt: started, Limits: limits})
	}
	if err != nil {
		return out, release, err
	}
	binding := evidence.Binding
	if binding.FirstNativeTaskAt.IsZero() && evidence.Task.Native {
		binding.FirstNativeTaskAt, binding.FirstNativeTaskID = evidence.Task.StartedAt, evidence.Task.TurnID
	}
	w.c.CodexBinding = &binding
	w.c.NativeChild, w.c.ParentNativeID, w.c.RootNativeID = binding.Child, binding.ParentID, binding.RootID
	w.c.NativeHome = binding.Home
	w.t.metaStart = binding.NativeCreatedAt
	return out, release, nil
}
