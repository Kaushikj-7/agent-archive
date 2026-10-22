package collector

import (
	"context"
	"errors"

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
// related history), a subagent or child session (its full read names it
// from its metadata or spawn record), a native or staged import, a
// historical project, a harness with no record previewer, a session whose
// retained or pending evidence carries a session label its previewer cannot
// name it from (or the full read would refuse), and a transcript
// short enough for the windows to span with a record the preview left unread
// (one over the record limit, or an unfinished last record).
func PreviewLocalTranscript(ctx context.Context, home string, reg archive.SessionRegistration, cursorDatabase string, sources agentapi.SourcesLookup, previews agentapi.PreviewsLookup, limits PreviewLimits) (TranscriptPreview, error) {
	// A leaf: the handoff picker runs several of these at once.
	defer trace.StartLeaf("preview local transcript").End()
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
	// evidence, not from the transcript. The preview takes the same label
	// from the same evidence, filtered as the full read filters it, and
	// names the session from it as the harness's parser would.
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
		evidence = mergeSupplementalEvidence(evidence, req.HookEvidence)
	}
	label, labeled, err := archive.SessionLabelFor(reg, evidence)
	namer, names := previewer.(agentapi.SessionLabelNamer)
	if err != nil || (labeled && !names) {
		return TranscriptPreview{}, ErrPreviewUnavailable
	}
	source, ok := newSourceReader(reg, Options{CursorDatabase: cursorDatabase, Sources: sources})
	reader, provider := source.(providerReader)
	if !ok || !provider || reader.nativeRoot != nil {
		return TranscriptPreview{}, ErrPreviewUnavailable
	}
	reader.preview = true
	preview, err := reader.previewTranscript(ctx, previewer, limits, reg.NativeSessionID)
	if err == nil && labeled {
		preview.Name = archive.CollapseSessionTitle(namer.SessionLabelName(label, preview.Name))
	}
	return preview, err
}

// previewTranscript opens the source as filter does, with the same read
// limits and admission checks, and previews its windows instead of filtering
// all of it.
func (r providerReader) previewTranscript(ctx context.Context, previewer agentapi.RecordPreviewer, limits PreviewLimits, nativeID string) (out TranscriptPreview, err error) {
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
	if file == nil {
		return out, ErrPreviewUnavailable
	}
	spanned := file.Length() <= limits.HeadBytes+limits.TailBytes
	if spanned {
		// One head window over the whole transcript reads the bytes both
		// windows would, without leaving a record across their boundary
		// unread, and says whether it read every record.
		limits.HeadBytes += limits.TailBytes
	}
	preview, err := PreviewTranscript(ctx, file, previewer, limits, nativeID)
	if err == nil && spanned && !preview.NameComplete {
		// A record over the record limit, or an unfinished last one, was
		// left unread. Read the transcript whole, which is as cheap at this
		// size, rather than miss what that record holds.
		return TranscriptPreview{}, ErrPreviewUnavailable
	}
	return preview, err
}
