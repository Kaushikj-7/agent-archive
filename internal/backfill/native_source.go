package backfill

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/inspectioncache"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"github.com/wangjohn/agent-archive/internal/trace"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

// filterImportSource validates native ownership on the same snapshot it filters.
// Historical admission belongs to the import; ancestry supplies no permission.
func filterImportSource(ctx context.Context, env Environment, w *work, started time.Time, inspect bool) (out archive.FilteredTranscript, summary *agentapi.ImportSummary, release func(), err error) {
	release = func() {}
	ref := agentapi.SourceRef{Kind: w.c.SourceKind, Path: w.t.path, Key: w.c.SourceKey}
	if ref.Kind == "" {
		ref.Kind = archive.SourceKindFile
	}
	if string(w.t.harness) != "codex" || !w.c.NativeChild && !w.c.RelatedHistory || env.CodexRollouts == nil {
		if inspect {
			out, summary, w.inspectionObservation, err = collector.InspectSourceCached(ctx, string(w.t.harness), ref, started, env.Sources, ordinaryInspectionCache(env, w))
		} else {
			trace.Add(trace.AdapterInvocations, 1)
			out, _, err = collector.FilterSource(ctx, string(w.t.harness), ref, started, env.Sources)
		}
		return out, summary, release, err
	}
	trace.Add(trace.BackfillNativeInspectionAttempts, 1)
	provider, filter, ok := env.Sources.LookupSources(string(w.t.harness))
	if !ok {
		return out, summary, release, errors.New("native source capability unavailable")
	}
	root := w.c.NativeHome
	// Discovery may spell a trusted home through a platform alias, while the
	// shared rollout lookup supplies canonical locators. Rebase only the home;
	// descendant symlinks remain visible to the confined source opener.
	relative, err := filepath.Rel(root, ref.Path)
	if err != nil {
		return out, summary, release, err
	}
	root = env.resolved(root)
	ref.Path = filepath.Join(root, relative)
	var budget *agentapi.NativeReadBudget
	if shared, ok := env.CodexRollouts.(agentapi.CodexRolloutResourceBudget); ok {
		budget = shared.NativeReadBudget()
	}
	var sourceAllowed func(string) bool
	if w.res.historical != nil {
		sourceAllowed = func(cwd string) bool {
			if env.HistoricalSourceAllowed != nil && !env.HistoricalSourceAllowed(cwd) {
				return false
			}
			for _, path := range uniquePaths(filepath.Clean(cwd), env.resolved(cwd)) {
				if !slices.Contains(w.res.historical.SourceCwds, path) {
					w.res.historical.SourceCwds = append(w.res.historical.SourceCwds, path)
				}
			}
			return len(w.res.historical.SourceCwds) <= 2*archive.MaxHistorySpans
		}
	}
	environment := agentapi.SourceEnvironment{HistorySourceAllowed: sourceAllowed, ReadBudget: budget, CodexRollouts: env.CodexRollouts, RequireConfinedHistory: true, Files: sourcefacts.RootOpener{Root: root}, Policy: transcriptio.OpenPolicy{Root: root, RejectSymlinks: true}}
	pass, err := provider.OpenPass(ctx, environment)
	if err != nil {
		return out, summary, release, err
	}
	defer func() { err = errors.Join(err, pass.Close()) }()
	limits := agentapi.ReadLimits{RawBytes: collector.MaxRawTranscriptBytes(string(w.t.harness)), RecordBytes: archive.MaxRecordBytes, FilteredBytes: collector.DefaultMaxTranscriptBytes}
	snapshot, err := pass.Read(ctx, ref, limits)
	if err != nil {
		return out, summary, release, err
	}
	defer func() { err = errors.Join(err, snapshot.Close()) }()
	admission, ok := snapshot.(agentapi.SourceAdmissionEvidence)
	if !ok {
		return out, summary, release, agentapi.WithCause(agentapi.Wrap(agentapi.Unavailable, errors.New("native admission evidence unavailable")), agentapi.CauseNativeAdmissionEvidenceUnavailable)
	}
	evidence, err := admission.AdmissionEvidence(ctx, agentapi.SourceAdmission{NativeID: w.c.NativeSessionID, Cwd: w.t.cwd})
	if err != nil {
		return out, summary, release, err
	}
	// Children and supported forks require their first own native task.
	// Ordinary historical and paginated imports retain their compatibility.
	if err := evidence.Binding.ValidateFor("codex"); err != nil {
		return out, summary, release, err
	}
	if evidence.Binding.Codex == nil {
		return out, summary, release, agentapi.ErrInvalidSourceBinding
	}
	if evidence.Binding.Codex.RequiresOwnTask() && !evidence.Task.ValidNativeCreation(evidence.Binding.Codex.NativeCreatedAt) {
		cause := ownTaskRefusalCause(evidence.Task, evidence.Binding.Codex.NativeCreatedAt)
		return out, summary, release, agentapi.WithCause(agentapi.Wrap(agentapi.Unavailable, errors.New("native own task unavailable")), cause)
	}
	trace.Add(trace.AdapterInvocations, 1)
	if leased, ok := filter.(agentapi.LeasedTranscriptFilter); ok && leased.LeasedFilterFor(filter) {
		out, release, err = leased.FilterLeased(ctx, snapshot.Input(), agentapi.FilterContext{Filename: ref.Path, StartedAt: started, Limits: limits}, environment.ReadBudget)
	} else {
		out, err = filter.Filter(ctx, snapshot.Input(), agentapi.FilterContext{Filename: ref.Path, StartedAt: started, Limits: limits})
	}
	if err != nil {
		return out, summary, release, err
	}
	binding := *evidence.Binding.Codex
	if binding.FirstNativeTaskAt.IsZero() && evidence.Task.Native {
		binding.FirstNativeTaskAt, binding.FirstNativeTaskID = evidence.Task.StartedAt, evidence.Task.TurnID
	}
	w.c.CodexBinding = &binding
	w.c.NativeChild, w.c.ParentNativeID, w.c.RootNativeID = binding.Child, binding.ParentID, binding.RootID
	w.c.NativeHome = binding.Home
	w.t.metaStart = binding.NativeCreatedAt
	return out, summary, release, nil
}

// ownTaskRefusalCause classifies an already rejected first own task.
func ownTaskRefusalCause(task agentapi.OwnTaskFacts, createdAt time.Time) agentapi.SourceCause {
	cause := agentapi.CauseOwnTaskMissing
	if task.Seen {
		cause = agentapi.CauseOwnTaskInvalid
		if task.Refusal != "" {
			cause = task.Refusal
		}
		if task.Native && !task.LocalExecution {
			cause = agentapi.CauseOwnTaskExecutionUnsupported
		}
		if task.Native && task.LocalExecution && !task.StartedAt.IsZero() && !createdAt.IsZero() {
			cause = agentapi.CauseOwnTaskBeforeCreation
		}
	}
	return cause
}

func ordinaryInspectionCache(env Environment, w *work) *inspectioncache.Cache {
	if w.c.NativeChild || w.c.RelatedHistory {
		return nil
	}
	return env.InspectionCache
}
