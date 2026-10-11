package collector

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/trace"
)

// ErrNoTranscript means a registration names no transcript file yet, or the
// file it names no longer exists, or, for a Cursor database chat, the chat
// or the database is gone.
var ErrNoTranscript = errors.New("the session's transcript is not available on this machine")

// ReadLocalBundle builds, in memory, the source bundle a collector pass would
// build for reg from its source as it is now: the same adapter filter,
// plus the hook evidence already published or pending for the session. It
// takes no lock and writes, registers, and uploads nothing, so it is safe
// beside a running collector and while collection is paused. The transcript
// is append-only and a torn final record is excluded exactly as a collector
// pass excludes it. A Cursor database chat is read from cursorDatabase
// (empty means the one under the user's home), through a snapshot of its
// own when Cursor is running, which is removed before it returns.
func ReadLocalBundle(ctx context.Context, home string, reg archive.SessionRegistration, capturedAt time.Time, cursorDatabase string, sources agentapi.SourcesLookup) (archive.SourceBundle, error) {
	defer trace.Start("read local transcript").End()
	store, closeRead := state.OpenReadOnly(home).WithReadBudget(ctx, nil)
	defer closeRead()
	if err := store.CheckDurableSessionRead(reg.ArchiveSessionID); err != nil {
		return archive.SourceBundle{}, err
	}
	published, publishedErr := store.LoadPublishedState(reg.ArchiveSessionID)
	if errors.Is(publishedErr, state.ErrDurableStorageRecovery) {
		return archive.SourceBundle{}, publishedErr
	}
	if reg.StagedImportSHA256 != "" && (publishedErr != nil || !published.Summary().Published) {
		stage, found, err := store.LoadImportStage(ctx, reg)
		if err != nil {
			return archive.SourceBundle{}, err
		}
		if !found {
			return archive.SourceBundle{}, state.ErrDurableStorageRecovery
		}
		adapter, err := sourceAdapter(sources, reg.Harness.Name)
		if err != nil {
			return archive.SourceBundle{}, err
		}
		return refilterBundleBounded(ctx, reg, adapter, stage.Bundle, agentapi.ReadLimits{FilteredBytes: DefaultMaxTranscriptBytes, RecordBytes: archive.MaxRecordBytes})
	}
	source, ok := newSourceReader(reg, Options{CursorDatabase: cursorDatabase, Sources: sources})
	if !ok {
		return archive.SourceBundle{}, ErrNoTranscript
	}
	adapter, err := sourceAdapter(sources, reg.Harness.Name)
	if err != nil {
		return archive.SourceBundle{}, err
	}
	filtered, _, err := source.Filter(ctx, adapter, DefaultMaxTranscriptBytes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && !agentapi.HasFailure(err, agentapi.Cleanup) {
			return archive.SourceBundle{}, ErrNoTranscript
		}
		return archive.SourceBundle{}, fmt.Errorf("filter transcript: %w", err)
	}
	var evidence []archive.SupplementalEvidence
	if publishedErr == nil {
		if cached, _, _, found := published.Cached(); found {
			evidence = cached.SupplementalEvidence
		}
	}
	if req, found, err := store.LoadRequest(reg.ArchiveSessionID); err == nil && found {
		evidence = mergeSupplementalEvidence(evidence, req.HookEvidence)
	}
	return archive.NewSourceBundle(reg, adapter, filtered, capturedAt, evidence)
}

// LastActivity is when reg's source last changed: its transcript's
// modification time, or a Cursor database chat's lastUpdatedAt. ok is false
// when that can't be read.
func LastActivity(ctx context.Context, reg archive.SessionRegistration, database string, sources agentapi.SourcesLookup) (time.Time, bool) {
	found := LastActivities(ctx, []archive.SessionRegistration{reg}, database, sources)
	at, ok := found[reg.ArchiveSessionID]
	return at, ok
}

// LastActivities preserves per-provider batching without choosing native readers in shared code.
func LastActivities(ctx context.Context, regs []archive.SessionRegistration, database string, sources agentapi.SourcesLookup) map[string]time.Time {
	span := trace.Start("local activity")
	span.Count("sessions", len(regs))
	defer span.End()
	out := map[string]time.Time{}
	if sources == nil {
		return out
	}
	groups := map[sourcePassKey][]archive.SessionRegistration{}
	for _, reg := range regs {
		key := sourcePassKey{name: reg.Harness.Name, discovery: reg.Origin == archive.SessionOriginDiscovery}
		if key.discovery {
			key.root = reg.DiscoveryRoot
		}
		groups[key] = append(groups[key], reg)
	}
	for key, group := range groups {
		e := sourceEnvironment(discoveryRegistration(group[0]), database)
		provider, _, ok := sources.LookupSources(key.name)
		if !ok {
			continue
		}
		refs := make([]agentapi.SourceRef, 0, len(group))
		for _, reg := range group {
			refs = append(refs, sourceRef(reg))
		}
		batch, ok := provider.(agentapi.ActivityProvider)
		if !ok {
			continue
		}
		found, err := batch.Activities(ctx, e, refs)
		if err != nil && ctx.Err() != nil {
			return out
		}
		for _, reg := range group {
			if at, ok := found[sourceRef(reg)]; ok && !at.IsZero() {
				out[reg.ArchiveSessionID] = at
			}
		}
	}
	return out
}

// FilterTranscriptFile filters one native transcript file that has no
// registration, for a harness named by the caller. startedAt stands in for
// the fresh-start proof a Cursor text transcript otherwise needs. A
// transcript over any of the collector's size limits is an error wrapping
// archive.ErrRecordTooLarge, as for FilterCursorChat. Nothing is registered,
// written, or uploaded.
func FilterTranscriptFile(harness, path string, startedAt time.Time, sources agentapi.SourcesLookup) (archive.FilteredTranscript, archive.Adapter, error) {
	return FilterSource(context.Background(), harness, agentapi.SourceRef{Path: path}, startedAt, sources)
}

// FilterSource filters an unregistered provider-owned native locator through the actual source port.
func FilterSource(ctx context.Context, harness string, ref agentapi.SourceRef, startedAt time.Time, sources agentapi.SourcesLookup) (archive.FilteredTranscript, archive.Adapter, error) {
	adapter, err := sourceAdapter(sources, harness)
	if err != nil {
		return archive.FilteredTranscript{}, nil, err
	}
	reg := archive.SessionRegistration{Harness: archive.Harness{Name: adapter.Name()}, TranscriptPath: ref.Path, SourceKind: ref.Kind, SourceKey: ref.Key, SessionStartedAt: startedAt}
	source, _ := newSourceReader(reg, Options{Sources: sources})
	out, _, err := source.Filter(ctx, adapter, DefaultMaxTranscriptBytes)
	if errors.Is(err, errRecordTooLarge) || errors.Is(err, errTranscriptTooLarge) {
		return archive.FilteredTranscript{}, nil, errors.Join(archive.ErrRecordTooLarge, err)
	}
	return out, adapter, err
}

// FilterTranscriptSnapshot filters the verified handle without reopening its path.
// The caller owns the handle and must close it, including on cancellation.
func FilterTranscriptSnapshot(ctx context.Context, snapshot agentapi.FileInput, harness string, startedAt time.Time, maxBytes int64, sources agentapi.SourcesLookup) (archive.FilteredTranscript, archive.Adapter, error) {
	adapter, err := sourceAdapter(sources, harness)
	if err != nil {
		return archive.FilteredTranscript{}, nil, err
	}
	reg := archive.SessionRegistration{Harness: archive.Harness{Name: adapter.Name()}, SessionStartedAt: startedAt}
	filtered, _, err := filterSnapshot(ctx, snapshot, adapter, reg, maxBytes)
	if errors.Is(err, errRecordTooLarge) || errors.Is(err, errTranscriptTooLarge) {
		return archive.FilteredTranscript{}, nil, fmt.Errorf("%w: %w", archive.ErrRecordTooLarge, err)
	}
	if err != nil {
		return archive.FilteredTranscript{}, nil, err
	}
	return filtered, adapter, nil
}

func sourceAdapter(sources agentapi.SourcesLookup, name string) (agentapi.TranscriptFilter, error) {
	if sources == nil {
		return nil, errors.New("source integrations are required")
	}
	_, f, ok := sources.LookupSources(name)
	if !ok {
		return nil, fmt.Errorf("source filter unavailable for %s", name)
	}
	return f, nil
}

// ReadImportSourceSelection freezes the selected source set and its binding from the same
// source handle, so admission can freeze ownership without a second native read.
func ReadImportSourceSelection(ctx context.Context, reg archive.SessionRegistration, at time.Time, opts Options) (archive.SourceBundle, []archive.SourceBundle, *archive.CodexSourceBinding, error) {
	var binding *archive.CodexSourceBinding
	bundle, preserved, err := readImportSourceSet(ctx, reg, at, opts, &binding)
	return bundle, preserved, binding, err
}

func readImportSourceSet(ctx context.Context, reg archive.SessionRegistration, at time.Time, opts Options, selected **archive.CodexSourceBinding) (archive.SourceBundle, []archive.SourceBundle, error) {
	adapter, err := sourceAdapter(opts.Sources, reg.Harness.Name)
	if err != nil {
		return archive.SourceBundle{}, nil, err
	}
	source, ok := newSourceReader(reg, opts)
	if !ok {
		return archive.SourceBundle{}, nil, ErrNoTranscript
	}
	filtered, observed, err := source.Filter(ctx, adapter, state.MaxImportStageBytes)
	if err != nil {
		return archive.SourceBundle{}, nil, err
	}
	if err := validateImportSelection(ctx, reg, filtered, opts.Sources); err != nil {
		return archive.SourceBundle{}, nil, err
	}
	if selected != nil {
		*selected = observed.binding
	}
	bundle, err := archive.NewSourceBundle(reg, adapter, filtered, at, nil)
	if err != nil {
		return bundle, nil, err
	}
	if reg.Harness.Name != archive.HarnessCodex || observed.binding == nil || (bundle.History == nil && opts.CodexRollouts == nil) {
		return bundle, nil, nil
	}
	provider, ok := source.(providerReader)
	if !ok {
		return bundle, nil, errors.New("import revision reader unavailable")
	}
	preserved := []archive.SourceBundle{}
	remaining := state.MaxImportStageBytes - int64(bundle.Capture.Boundary.RetainedBytes)
	seen := map[string]bool{revisionID(bundle): true}
	err = provider.revisions(ctx, adapter, observed, state.MaxImportStageBytes, bundle.History != nil, func(part archive.FilteredTranscript, binding *archive.CodexSourceBinding) error {
		owner := reg
		owner.CodexBinding = binding
		prior, err := archive.NewSourceBundle(owner, adapter, part, at, nil)
		if err != nil {
			return err
		}
		id := revisionID(prior)
		if seen[id] {
			return nil
		}
		seen[id] = true
		n := int64(prior.Capture.Boundary.RetainedBytes)
		if n > remaining {
			return agentapi.ErrReadBudget
		}
		remaining -= n
		preserved = append(preserved, prior)
		return nil
	})
	return bundle, preserved, err
}

// Reuse each integration's planning identity and conversation rules against the
// fresh snapshot. A stable descriptor alone does not prove that a replaced path
// still belongs to the native session selected in the confirmed plan.
func validateImportSelection(ctx context.Context, reg archive.SessionRegistration, filtered archive.FilteredTranscript, sources agentapi.SourcesLookup) error {
	imports, ok := sources.(agentapi.ImportsLookup)
	if !ok {
		return errors.New("import inspection capability unavailable")
	}
	inspector, ok := imports.LookupImport(reg.Harness.Name)
	if !ok {
		return errors.New("import inspection unavailable for selected harness")
	}
	inspection, err := inspector.InspectImport(ctx, agentapi.ImportInspectionRequest{
		Session: agentapi.NativeSession{Agent: agentmeta.ID(reg.Harness.Name), NativeID: reg.NativeSessionID},
		Source:  sourceRef(reg), Header: agentapi.NativeHeader{NativeID: reg.NativeSessionID, StartedAt: reg.SessionStartedAt}, Filtered: filtered,
	})
	if err != nil {
		return err
	}
	if inspection.IdentityMismatch || filtered.LocalIdentity.ID != "" && filtered.LocalIdentity.ID != reg.NativeSessionID {
		return errors.New("selected import source native identity changed; replan the import")
	}
	if !inspection.Conversation {
		return errors.New("selected import source no longer contains a conversation; replan the import")
	}
	return nil
}
