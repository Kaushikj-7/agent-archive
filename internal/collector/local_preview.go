package collector

import (
	"context"
	"errors"
	"slices"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/trace"
)

// ErrPreviewUnavailable means a session's labels cannot be previewed from
// bounded windows of its transcript; ReadLocalBundle reads them whole.
var ErrPreviewUnavailable = errors.New("the session's transcript cannot be previewed")

// PreviewLocalTranscript previews reg's labels from the filtered head and
// tail windows of its transcript as it is now. The transcript is opened by
// the reader ReadLocalBundle uses, with its confinement and admission checks,
// behind the same durable read check, and every record is filtered before
// any fact is taken from it. Like ReadLocalBundle, it takes no lock and
// writes nothing.
//
// It is ErrPreviewUnavailable for a session a preview cannot stand in for:
// one not read from a single transcript file (a Cursor database chat, Codex
// related history), a subagent or child session, whose full read names it
// from its metadata or spawn record, a native or staged import, a historical project, a
// harness with no record previewer, a session whose retained or pending
// evidence carries a session label, and a transcript shorter than minBytes,
// which a full read handles about as quickly.
func PreviewLocalTranscript(ctx context.Context, home string, reg archive.SessionRegistration, cursorDatabase string, sources agentapi.SourcesLookup, previews agentapi.PreviewsLookup, minBytes int64, limits PreviewLimits) (TranscriptPreview, error) {
	defer trace.Start("preview local transcript").End()
	if reg.NativeImport != nil || reg.HistoricalProject != nil || reg.StagedImportSHA256 != "" || !reg.ReadsTranscriptFile() || reg.IsChild() || reg.SubagentID != "" || previews == nil {
		return TranscriptPreview{}, ErrPreviewUnavailable
	}
	previewer, ok := previews.LookupPreview(reg.Harness.Name)
	if !ok {
		return TranscriptPreview{}, ErrPreviewUnavailable
	}
	store, closeRead := state.OpenReadOnly(home).WithReadBudget(ctx, nil)
	defer closeRead()
	if err := store.CheckDurableSessionRead(reg.ArchiveSessionID); err != nil {
		return TranscriptPreview{}, err
	}
	// A session label (a Codex thread name) reaches the full read as
	// evidence, not from the transcript, so such a session is read whole.
	var evidence []archive.SupplementalEvidence
	published, err := store.LoadPublishedState(reg.ArchiveSessionID)
	if errors.Is(err, state.ErrDurableStorageRecovery) {
		return TranscriptPreview{}, err
	}
	if err == nil {
		if cached, _, _, found := published.Cached(); found {
			evidence = cached.SupplementalEvidence
		}
	}
	if req, found, err := store.LoadRequest(reg.ArchiveSessionID); err == nil && found {
		evidence = append(evidence, req.HookEvidence...)
	}
	if slices.ContainsFunc(evidence, func(e archive.SupplementalEvidence) bool { return e.Kind == archive.EvidenceKindSessionLabels }) {
		return TranscriptPreview{}, ErrPreviewUnavailable
	}
	source, ok := newSourceReader(reg, Options{CursorDatabase: cursorDatabase, Sources: sources})
	reader, provider := source.(providerReader)
	if !ok || !provider || reader.nativeRoot != nil {
		return TranscriptPreview{}, ErrPreviewUnavailable
	}
	reader.preview = true
	return reader.previewTranscript(ctx, previewer, minBytes, limits, reg.NativeSessionID)
}

// previewTranscript opens the source as filter does, with the same read
// limits and admission checks, and previews its windows instead of filtering
// all of it.
func (r providerReader) previewTranscript(ctx context.Context, previewer agentapi.RecordPreviewer, minBytes int64, limits PreviewLimits, nativeID string) (out TranscriptPreview, err error) {
	ctx, cancel := r.validationContext(ctx)
	defer cancel()
	provider, filter, err := r.binding()
	if err != nil {
		return out, err
	}
	p, closePass, err := r.pass(ctx, provider, filter.Name())
	if err != nil {
		return out, err
	}
	defer func() { err = errors.Join(err, closePass()) }()
	snap, err := p.Read(ctx, r.ref, agentapi.ReadLimits{RawBytes: maxRawBytes(r.harness, DefaultMaxTranscriptBytes), RecordBytes: recordLimit, SubagentMetadata: r.subagentMetadata})
	if err != nil {
		return out, translateSourceError(err)
	}
	defer func() { err = errors.Join(err, snap.Close()) }()
	if err = r.validateObservation(provider, snap.Observation()); err != nil {
		return out, err
	}
	if _, err = r.snapshotBinding(ctx, snap); err != nil {
		return out, err
	}
	file := snap.Input().File
	if file == nil || file.Length() < minBytes {
		return out, ErrPreviewUnavailable
	}
	return PreviewTranscript(ctx, file, previewer, limits, nativeID)
}
