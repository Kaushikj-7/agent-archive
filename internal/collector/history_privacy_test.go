package collector

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/codex"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

func privacyJournal(t *testing.T) (*sessionScan, state.PendingPublication) {
	t.Helper()
	scan, _ := reconciliationFixture(t)
	if _, err := scan.run(scan.ctx); !errors.Is(err, archive.ErrHistoryMutationPending) {
		t.Fatal(err)
	}
	p, found, err := scan.local.LoadPending(scan.id())
	if err != nil || !found {
		t.Fatal(found, err)
	}
	for p.History.Preparing {
		if err := scan.advanceHistoryPreparation(&p); err != nil {
			t.Fatal(err)
		}
	}
	return scan, p
}

func TestRetainedSource3RefilterKeepsGraphOwnershipAndRawGaps(t *testing.T) {
	t.Parallel()
	scan, p := privacyJournal(t)
	b := p.Bundle
	// A copied prefix and active physical span keep separate ownership.
	message := func(text string) map[string]any {
		return map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}}}
	}
	quoted := " \n    <external_codex_apps_open_page>example</external_codex_apps_open_page>"
	b.NativeRecords = []map[string]any{
		{"type": "session_meta", "payload": map[string]any{"id": revisionB}}, message("inherited"),
		{"type": "session_meta", "payload": map[string]any{"id": scan.reg.NativeSessionID}}, message("<system-reminder>Injected</system-reminder><external_codex_apps_open_page>Context</external_codex_apps_open_page>" + quoted),
		{"type": "future_private_record", "secret": "synthetic-private-value"},
	}
	b.Ordinals = []uint64{0, 1, 2, 3, 4}
	last := len(b.NativeRecords) - 1
	dropped := b.Ordinals[last]
	own := uint64(3)
	b.History = &archive.SourceHistory{ActiveRolloutID: revisionC, ThreadID: scan.reg.NativeSessionID, OwnStart: &own, Spans: []archive.HistorySpan{
		{RolloutID: revisionB, ThreadID: revisionB, FirstRecord: 0, EndRecord: 2, StartOrdinal: 0, EndOrdinal: 2},
		{RolloutID: revisionC, ThreadID: scan.reg.NativeSessionID, FirstRecord: 2, EndRecord: 5, StartOrdinal: 2, EndOrdinal: 5},
	}}
	b.Capture.Gaps = append(b.Capture.Gaps, archive.CaptureGap{Code: "prior_gap", Detail: "original gap"})
	if err := b.ValidateHistory(); err != nil {
		t.Fatal(err)
	}
	originalSpans := slices.Clone(b.History.Spans)
	originalOrdinals := slices.Clone(b.Ordinals)
	out, err := refilterBundle(t, t.Context(), scan.reg, codex.Filter{}, b)
	if err != nil {
		t.Fatal(err)
	}
	if out.SchemaVersion != 3 || out.History.ActiveRolloutID != b.History.ActiveRolloutID || out.History.ThreadID != b.History.ThreadID || *out.History.OwnStart != own || !out.Capture.CapturedAt.Equal(b.Capture.CapturedAt) {
		t.Fatal("history/capture changed", out)
	}
	if slices.Contains(out.Ordinals, dropped) || !slices.Equal(out.Ordinals, originalOrdinals[:last]) || !slices.Contains(out.Capture.Gaps, b.Capture.Gaps[len(b.Capture.Gaps)-1]) {
		t.Fatal("raw gaps were flattened", out.Ordinals)
	}
	for i, span := range out.History.Spans {
		before := originalSpans[i]
		if span.RolloutID != before.RolloutID || span.ThreadID != before.ThreadID || span.StartOrdinal != before.StartOrdinal || span.EndOrdinal != before.EndOrdinal {
			t.Fatal("physical graph changed")
		}
	}
	ownPayload := out.NativeRecords[3]["payload"].(map[string]any)
	ownText := ownPayload["content"].([]any)[0].(map[string]any)["text"]
	if ownText != quoted {
		t.Fatalf("history refilter changed indented quote: %q want %q", ownText, quoted)
	}
	again, err := refilterBundle(t, t.Context(), scan.reg, codex.Filter{}, out)
	if err != nil {
		t.Fatal(err)
	}
	same, err := bundleEvidenceEqual(out, again)
	if err != nil || !same {
		t.Fatal("history refilter not stable", err)
	}
	if out.OwnRecord(0) {
		t.Fatal("inherited record became owned")
	}
	if !slices.Equal(b.Ordinals, originalOrdinals) || !slices.Equal(b.History.Spans, originalSpans) {
		t.Fatal("input graph mutated")
	}
	packed, err := archive.BuildCompressedSource(out)
	if err != nil {
		t.Fatal(err)
	}
	round, err := archive.ReadSourceBundle(bytes.NewReader(packed.Bytes), archive.DecodeOptions{})
	if err != nil || round.ValidateHistory() != nil {
		t.Fatal(err)
	}
	var safe bytes.Buffer
	if err := archive.EncodeSource(&safe, round); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(safe.Bytes(), []byte("synthetic-private-value")) {
		t.Fatal("dropped private record survived")
	}
}

func TestStricterHistoryPriorStagesAllOriginalsAndLeavesRequestsOwed(t *testing.T) {
	t.Parallel()
	scan, p := privacyJournal(t)
	p.Attempted = true
	if err := scan.local.SavePending(scan.id(), p); err != nil {
		t.Fatal(err)
	}
	before := append([]state.HistoryInput(nil), p.History.Inputs...)
	scan.opts.SkillEvidence = config.SkillEvidenceNone
	if _, err := scan.resumeStricterHistory(p); !errors.Is(err, archive.ErrHistoryMutationPending) {
		t.Fatal(err)
	}
	next, found, err := scan.local.LoadPublicationPending(scan.id())
	if err != nil || !found || next.Attempted || next.History.Preparing || len(next.History.Inputs) != len(before) {
		t.Fatal(next.History, err)
	}
	for i, input := range next.History.Inputs {
		assertOriginalHistoryInput(t, scan.local, scan.id(), input, before[i])
		if _, err := scan.local.ReadPendingSource(scan.id(), state.PendingSource{Reference: input.Reference, Name: input.Reference.SHA256 + ".gz"}); err != nil {
			t.Fatal(err)
		}
	}
	objects, err := scan.remote.List(t.Context(), "")
	if err != nil || len(objects) != 0 {
		t.Fatal("stricter preparation uploaded", err)
	}
	if outcome, err := scan.publishPending(scan.ctx, next); err != nil || outcome != outcomePublished {
		t.Fatal("prepared successor failed", err)
	}
	assertCompleteHistory(t, scan, next, scan.remote)
}

func TestStricterHistoryUnknownRemoteKeepsAttemptedDescriptor(t *testing.T) {
	t.Parallel()
	scan, p := privacyJournal(t)
	p.Attempted = true
	if err := scan.local.SavePending(scan.id(), p); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(scan.local.Home(), "pending", scan.id()+".json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := scan.remote.Put(t.Context(), p.MetadataKey, []byte(`{"schema_version":99}`)); err != nil {
		t.Fatal(err)
	}
	scan.opts.SkillEvidence = config.SkillEvidenceNone
	if _, err := scan.resumeStricterHistory(p); !errors.Is(err, errHistoryMetadataConflict) {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("uncertain retry changed", err)
	}
}

func TestStricterHistoryCommittedQueuesSuccessorWithoutReupload(t *testing.T) {
	t.Parallel()
	scan, p := privacyJournal(t)
	// A source-first remote commit occurred just before the process stopped.
	for _, stage := range p.History.Sources {
		raw, err := scan.local.ReadPendingSource(scan.id(), stage)
		if err != nil {
			t.Fatal(err)
		}
		if err := scan.remote.Put(t.Context(), stage.Reference.Key, raw); err != nil {
			t.Fatal(err)
		}
	}
	if err := scan.remote.Put(t.Context(), p.MetadataKey, p.MetadataBytes); err != nil {
		t.Fatal(err)
	}
	p.Attempted = true
	if err := scan.local.SavePending(scan.id(), p); err != nil {
		t.Fatal(err)
	}
	objectsBefore, err := scan.remote.List(t.Context(), "sessions/")
	if err != nil {
		t.Fatal(err)
	}
	counted := &privacyPutStore{ObjectStore: scan.remote}
	scan.remote = counted
	scan.opts.SkillEvidence = config.SkillEvidenceNone
	if _, err := scan.resumeStricterHistory(p); !errors.Is(err, archive.ErrHistoryMutationPending) {
		t.Fatal(err)
	}
	for _, key := range counted.puts {
		if key == p.MetadataKey || strings.Contains(key, "/source.") {
			t.Fatal("broader publication reuploaded", key)
		}
	}
	next, found, err := scan.local.LoadPending(scan.id())
	if err != nil || !found || next.History.ExpectedMetadataSHA256 != metadataSHA(p.MetadataBytes) || next.History.MaintenanceOwed || next.Attempted {
		t.Fatal(next.History, err)
	}
	raw, err := scan.remote.Get(t.Context(), p.MetadataKey)
	if err != nil || !bytes.Equal(raw, p.MetadataBytes) {
		t.Fatal("committed broader metadata reuploaded", err)
	}
	objectsAfter, err := scan.remote.List(t.Context(), "sessions/")
	if err != nil || !slices.Equal(objectsBefore, objectsAfter) {
		t.Fatal("committed source bytes reuploaded", err)
	}
	acknowledged, found, err := scan.published.LastPublishedMetadata()
	if err != nil || !found || acknowledged.SourceBundle != p.SourceReference() {
		t.Fatal("exact commit not acknowledged", err)
	}
}

func TestMixedHistoryPreparationRestartsAndReadsOriginalReplacedStage(t *testing.T) {
	t.Parallel()
	scan, p := privacyJournal(t)
	var m archive.Metadata
	if err := json.Unmarshal(p.MetadataBytes, &m); err != nil {
		t.Fatal(err)
	}
	// One ordinary source2 alternative alongside active and preserved source3.
	input := &p.History.Inputs[1]
	b, err := scan.loadHistoryInput(m, *input)
	if err != nil {
		t.Fatal(err)
	}
	b.SchemaVersion, b.History, b.Ordinals = archive.SourceSchemaVersion, nil, nil
	b.Capture.FilterVersion = "14"
	packed, err := archive.BuildCompressedSource(b)
	if err != nil {
		t.Fatal(err)
	}
	key, err := archive.SourceObjectKey(b, packed.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	ref := archive.SourceReference{Key: key, SHA256: packed.SHA256, CompressedBytes: len(packed.Bytes)}
	stage, err := scan.local.StagePendingSource(scan.id(), ref, packed.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	input.Reference, input.FilterVersion, input.SourceSchemaVersion = ref, "14", 2
	original := *input
	for i := range m.History.Preserved {
		r := &m.History.Preserved[i]
		if r.RevisionID == input.RevisionID {
			r.Source, r.FilterVersion, r.SourceSchemaVersion = ref, "14", 2
		}
	}
	p.MetadataBytes, err = json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	p.History.Sources = append(p.History.Sources, stage)
	// The old source3 stage is not final after replacing its pointer.
	refs, err := m.SourceReferences()
	if err != nil {
		t.Fatal(err)
	}
	stages := p.History.Sources[:0]
	for _, candidate := range p.History.Sources {
		if slices.Contains(refs, candidate.Reference) {
			stages = append(stages, candidate)
		}
	}
	p.History.Sources = stages
	p = freshLegacyHistoryOriginal(t, scan, p)
	p.History.Preparing, p.History.PrivacyCursor, p.History.PreparedAt = true, 0, time.Time{}
	for range len(p.History.Inputs) {
		if err := scan.local.SavePending(scan.id(), p); err != nil {
			t.Fatal(err)
		}
		scan.local, err = openTestStore(scan.local.Home())
		if err != nil {
			t.Fatal(err)
		}
		p, _, err = scan.local.LoadPublicationPending(scan.id())
		if err != nil {
			t.Fatal(err)
		}
		if err := scan.advanceHistoryPreparation(&p); err != nil {
			t.Fatal(err)
		}
	}
	if p.History.Preparing {
		t.Fatal("all-ref preparation incomplete")
	}
	if err := json.Unmarshal(p.MetadataBytes, &m); err != nil {
		t.Fatal(err)
	}
	if _, err := scan.loadHistoryInput(m, original); err != nil {
		t.Fatal("original removed from Sources became unreadable", err)
	}
	for _, stage := range p.History.Sources {
		raw, err := scan.local.ReadPendingSource(scan.id(), stage)
		if err != nil {
			t.Fatal(err)
		}
		b, err := decodeHistoryStage(t.Context(), m, p, stage.Reference, raw)
		if err != nil {
			t.Fatal(err)
		}
		if b.Capture.FilterVersion != archive.FilterVersion {
			t.Fatal("partial privacy claim")
		}
	}
	if p.Bundle.SchemaVersion != archive.HistorySourceSchemaVersion {
		t.Fatal("active flattened")
	}
	for _, r := range m.History.Preserved {
		if r.RevisionID == original.RevisionID {
			if r.SourceSchemaVersion != 2 || !r.CapturedAt.Equal(original.CapturedAt) {
				t.Fatal("mixed provenance/capture changed")
			}
			raw, err := scan.local.ReadPendingSource(scan.id(), state.PendingSource{Reference: r.Source, Name: r.Source.SHA256 + ".gz"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := reader.DecodeRevisionSource(t.Context(), m, r.RevisionID, raw, reader.Limits{}); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestRunAcknowledgedAllRefPrivacyPublishesTogetherWithoutNativeReads(t *testing.T) {
	for _, covered := range []bool{false, true} {
		t.Run(map[bool]string{false: "live-newer-owed", true: "frozen-covered"}[covered], func(t *testing.T) { runAcknowledgedPrivacyHook(t, covered) })
	}
}

func runAcknowledgedPrivacyHook(t *testing.T, covered bool) {
	t.Helper()
	scan, p := privacyJournal(t)
	var m archive.Metadata
	if err := json.Unmarshal(p.MetadataBytes, &m); err != nil {
		t.Fatal(err)
	}
	for _, input := range p.History.Inputs {
		b, err := scan.loadHistoryInput(m, input)
		if err != nil {
			t.Fatal(err)
		}
		b.Capture.FilterVersion = "14"
		for _, record := range b.NativeRecords {
			record["api_key"] = "sk-abcdefghijklmnopqrstuv"
		}
		packed, err := archive.BuildCompressedSource(b)
		if err != nil {
			t.Fatal(err)
		}
		key, err := archive.SourceObjectKey(b, packed.SHA256)
		if err != nil {
			t.Fatal(err)
		}
		ref := archive.SourceReference{Key: key, SHA256: packed.SHA256, CompressedBytes: len(packed.Bytes)}
		if err := scan.remote.Put(t.Context(), key, packed.Bytes); err != nil {
			t.Fatal(err)
		}
		if input.RevisionID == m.History.CurrentRevision {
			m.SourceBundle, m.FilterVersion = ref, "14"
			p.Bundle = b
		} else {
			for i := range m.History.Preserved {
				r := &m.History.Preserved[i]
				if r.RevisionID == input.RevisionID {
					r.Source, r.FilterVersion = ref, "14"
				}
			}
		}
	}
	m.Title = "sk-abcdefghijklmnopqrstuv"
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := scan.remote.Put(t.Context(), p.MetadataKey, raw); err != nil {
		t.Fatal(err)
	}
	if err := scan.published.SavePublication(p.Bundle, scan.now, m.SourceBundle, raw); err != nil {
		t.Fatal(err)
	}
	if err := scan.local.RemovePending(scan.id()); err != nil {
		t.Fatal(err)
	}
	if covered {
		if err := scan.published.SaveBlocked(p.Bundle, scan.now, state.BlockedReasonTranscriptRewritten); err != nil {
			t.Fatal(err)
		}
		opts := scan.opts
		opts.RepoKey = func(string) string { return "" }
		builder, closePreview, err := PrepareGenerationRecovery(t.Context(), scan.reg, scan.now.Add(time.Hour), opts)
		if err != nil {
			t.Fatal(err)
		}
		_, err = scan.local.BeginGenerationRecovery(scan.id(), scan.now.Add(time.Hour), builder)
		closePreview()
		if err != nil {
			t.Fatal(err)
		}
		scan.reg, _, err = scan.local.LoadRegistration(scan.id())
		if err != nil || !scan.reg.CaptureFrozen {
			t.Fatal("actual frozen owner missing", err)
		}
		scan.opts.AcceptSession = func(reg archive.SessionRegistration) bool { return reg.ArchiveSessionID == scan.id() }
	}
	if err := scan.local.SaveRequest(scan.id(), "feedback", scan.now, archive.SupplementalEvidence{Kind: archive.EvidenceKindExplicitFeedback, Provenance: "synthetic-owned-hook", ObservedAt: scan.now, Payload: map[string]any{"text": "combined frozen observation sk-abcdefghijklmnopqrstuv"}}); err != nil {
		t.Fatal(err)
	}
	request, _, err := scan.local.LoadRequest(scan.id())
	if err != nil {
		t.Fatal(err)
	}
	// A real preparation route must survive the disappearance of all native files.
	if err := os.RemoveAll(scan.reg.ProjectRoot); err != nil {
		t.Fatal(err)
	}
	scan.opts.RepoKey = func(string) string { t.Fatal("retained preparation consulted current Git"); return "" }
	for pass := range len(p.History.Inputs) {
		scan.local, err = openTestStore(scan.local.Home())
		if err != nil {
			t.Fatal(err)
		}
		result, err := Run(t.Context(), scan.local, scan.remote, scan.opts)
		if pass == len(p.History.Inputs)-1 {
			if err != nil || len(result.Published) != 1 || len(result.Errors) != 0 {
				t.Fatal("complete maintenance did not publish", result, err)
			}
			current, found, err := scan.local.LoadRequest(scan.id())
			if covered {
				if err != nil || found {
					t.Fatal("exact frozen covered token was not completed", err)
				}
			} else {
				if err != nil || !found || current.Token != request.Token {
					t.Fatal("live native request lost", err)
				}
				requestBody, err := json.Marshal(current.HookEvidence)
				if err != nil || !bytes.Contains(requestBody, []byte("combined frozen observation sk-abcdefghijklmnopqrstuv")) || !bytes.Contains(requestBody, []byte("newer observation must stay owed")) {
					t.Fatal("exact live owed observations lost", err)
				}
			}
			final, err := scan.remote.Get(t.Context(), p.MetadataKey)
			if err != nil || bytes.Equal(final, raw) {
				t.Fatal("maintenance sidecar missing", err)
			}
			if err := json.Unmarshal(final, &m); err != nil {
				t.Fatal(err)
			}
			refs, err := m.SourceReferences()
			if err != nil {
				t.Fatal(err)
			}
			for _, ref := range refs {
				data, err := scan.remote.Get(t.Context(), ref.Key)
				if err != nil {
					t.Fatal(err)
				}
				b, err := decodeHistoryStage(t.Context(), m, p, ref, data)
				encoded, encodeErr := json.Marshal(b.NativeRecords)
				if encodeErr != nil || bytes.Contains(encoded, []byte("sk-abcdefghijklmnopqrstuv")) {
					t.Fatal("stricter source retained secret marker", encodeErr)
				}
				if err != nil || b.Capture.FilterVersion != archive.FilterVersion || !b.Capture.CapturedAt.Equal(scan.now) {
					t.Fatal("partial refilter or changed capture", err)
				}
			}
			currentSource, err := scan.local.LoadPublishedState(scan.id())
			if err != nil {
				t.Fatal(err)
			}
			selected, _, _ := currentSource.LastPublished()
			selectedRaw, err := json.Marshal(selected)
			if err != nil || !bytes.Contains(selectedRaw, []byte("combined frozen observation")) || bytes.Contains(selectedRaw, []byte("newer observation must stay owed")) || bytes.Contains(selectedRaw, []byte("sk-abcdefghijklmnopqrstuv")) {
				t.Fatal("combined owned selection changed after restart", err)
			}
			privateRaw, err := os.ReadFile(publishedPath(scan.local, scan.id()))
			if err != nil {
				t.Fatal(err)
			}
			var private struct {
				Settled     json.RawMessage `json:"settled_privacy"`
				Preparation *struct {
					OriginMetadata          []byte `json:"origin_metadata"`
					PrivacyPreviousMetadata []byte `json:"privacy_previous_metadata"`
					Migration               *struct {
						PreviousMetadata []byte `json:"previous_metadata"`
					} `json:"migration"`
				} `json:"preparation"`
			}
			if err = json.Unmarshal(privateRaw, &private); err != nil {
				t.Fatal(err)
			}
			if private.Preparation != nil || len(private.Settled) == 0 {
				t.Fatal("successful privacy selection did not prune full private preparation")
			}
			if private.Preparation != nil {
				if bytes.Contains(private.Preparation.OriginMetadata, []byte("sk-abcdefghijklmnopqrstuv")) || bytes.Contains(private.Preparation.PrivacyPreviousMetadata, []byte("sk-abcdefghijklmnopqrstuv")) || private.Preparation.Migration != nil && bytes.Contains(private.Preparation.Migration.PreviousMetadata, []byte("sk-abcdefghijklmnopqrstuv")) {
					t.Fatal("successful privacy successor retained private prior metadata secret")
				}
			}
			if _, found, err := scan.local.LoadPending(scan.id()); err != nil || found {
				t.Fatal("maintenance cleanup incomplete", err)
			}
			if covered {
				assertPrivateTreeHasNoSecret(t, scan.local.Home(), "sk-abcdefghijklmnopqrstuv")
			} else {
				assertPrivateTreeHasNoSecret(t, scan.local.Home(), "sk-abcdefghijklmnopqrstuv", filepath.Join(scan.local.Home(), "requests", scan.id()+".json"))
			}
			return
		}
		if err != nil || len(result.Published) != 0 || len(result.Errors) == 0 {
			t.Fatal("partial maintenance escaped preparation", result, err)
		}
		current, found, err := scan.local.LoadRequest(scan.id())
		if err != nil || !found || current.Token != request.Token {
			t.Fatal("preparation acknowledged", err)
		}
		if pass == 0 {
			frozen, found, err := scan.local.LoadPublicationPending(scan.id())
			if err != nil || !found || frozen.Preparation == nil {
				t.Fatal("combined original not frozen", err)
			}
			hooks := 0
			for _, input := range frozen.Preparation.Inputs {
				if input.HookObservations != nil {
					hooks++
					if input.Selection.Role != state.PublicationCurrent || !bytes.Contains(input.HookObservations.Body, []byte("combined frozen observation")) {
						t.Fatal("wrong frozen hook owner")
					}
				}
			}
			if hooks != 1 {
				t.Fatal("owned observations missing from actual preparation", hooks)
			}
			if !covered {
				if err := scan.local.SaveRequest(scan.id(), "newer", scan.now.Add(time.Hour), archive.SupplementalEvidence{Kind: archive.EvidenceKindExplicitFeedback, Provenance: "synthetic-newer-hook", ObservedAt: scan.now.Add(time.Hour), Payload: map[string]any{"text": "newer observation must stay owed"}}); err != nil {
					t.Fatal(err)
				}
				request, _, err = scan.local.LoadRequest(scan.id())
				if err != nil {
					t.Fatal(err)
				}
			}
		}
		remote, err := scan.remote.Get(t.Context(), p.MetadataKey)
		if err != nil || !bytes.Equal(remote, raw) {
			t.Fatal("partial privacy sidecar published", err)
		}
	}
	t.Fatal("all-reference preparation did not converge")
}

type privacyPutStore struct {
	storage.ObjectStore
	puts []string
}

func (s *privacyPutStore) Put(ctx context.Context, key string, data []byte) error {
	s.puts = append(s.puts, key)
	return s.ObjectStore.Put(ctx, key, data)
}

func (s *privacyPutStore) GetLimited(ctx context.Context, key string, limit int64) ([]byte, error) {
	return s.ObjectStore.(storage.LimitedGetter).GetLimited(ctx, key, limit)
}

func (s *privacyPutStore) GetLimitedVersioned(ctx context.Context, key string, limit int64) ([]byte, string, error) {
	getter, ok := s.ObjectStore.(storage.LimitedVersionedGetter)
	if !ok {
		return nil, "", storage.ErrVersionedReadUnavailable
	}
	return getter.GetLimitedVersioned(ctx, key, limit)
}

func TestCommittedHistoryMaintenanceObligationSurvivesMissingOriginalRestart(t *testing.T) {
	t.Parallel()
	scan, p := privacyJournal(t)
	var m archive.Metadata
	if err := json.Unmarshal(p.MetadataBytes, &m); err != nil {
		t.Fatal(err)
	}
	// A previous privacy slice replaced an original, leaving it in Inputs only.
	original := p.History.Inputs[1]
	b, err := scan.loadHistoryInput(m, original)
	if err != nil {
		t.Fatal(err)
	}
	b.Capture.FilterVersion = "14"
	packed, err := archive.BuildCompressedSource(b)
	if err != nil {
		t.Fatal(err)
	}
	key, err := archive.SourceObjectKey(b, packed.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	original.Reference = archive.SourceReference{Key: key, SHA256: packed.SHA256, CompressedBytes: len(packed.Bytes)}
	original.FilterVersion = "14"
	p.History.Inputs[1] = original
	// These original bytes were private and never uploaded. The native source
	// cannot recover them, even though the exact complete final was committed.
	for _, stage := range p.History.Sources {
		raw, err := scan.local.ReadPendingSource(scan.id(), stage)
		if err != nil {
			t.Fatal(err)
		}
		if err := scan.remote.Put(t.Context(), stage.Reference.Key, raw); err != nil {
			t.Fatal(err)
		}
	}
	if err := scan.remote.Put(t.Context(), p.MetadataKey, p.MetadataBytes); err != nil {
		t.Fatal(err)
	}
	p.Attempted = true
	if err := scan.local.SavePending(scan.id(), p); err != nil {
		t.Fatal(err)
	}
	if err := scan.local.SaveRequest(scan.id(), "stop", scan.now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	newer, _, err := scan.local.LoadRequest(scan.id())
	if err != nil {
		t.Fatal(err)
	}
	scan.opts.SkillEvidence = config.SkillEvidenceNone
	if _, err := scan.resumeStricterHistory(p); err == nil {
		t.Fatal("missing original was invented")
	}
	p, found, err := scan.local.LoadPublicationPending(scan.id())
	if err != nil || !found || !p.History.MaintenanceOwed || !p.Attempted {
		t.Fatal("durable obligation lost", p.History, err)
	}
	scan.local, err = openTestStore(scan.local.Home())
	if err != nil {
		t.Fatal(err)
	}
	scan.published, err = scan.local.LoadPublishedState(scan.id())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scan.local.StagePendingSource(scan.id(), original.Reference, packed.Bytes); err != nil {
		t.Fatal(err)
	}
	if _, err := scan.resumeHistory(scan.ctx, p); !errors.Is(err, archive.ErrHistoryMutationPending) {
		t.Fatal(err)
	}
	next, found, err := scan.local.LoadPublicationPending(scan.id())
	if err != nil || !found || next.History.MaintenanceOwed || next.Attempted {
		t.Fatal("restart lost original provenance", err)
	}
	assertOriginalHistoryInput(t, scan.local, scan.id(), next.History.Inputs[1], original)
	request, found, err := scan.local.LoadRequest(scan.id())
	if err != nil || !found || request.Token != newer.Token {
		t.Fatal("newer request acknowledged", err)
	}
}

func TestHistoryMetadataRefreshPreservesManifestAndCaptures(t *testing.T) {
	t.Parallel()
	scan, p := privacyJournal(t)
	var prior archive.Metadata
	if err := json.Unmarshal(p.MetadataBytes, &prior); err != nil {
		t.Fatal(err)
	}
	prior.Parser.Version = "previous-parser"
	oldDigest, err := prior.SourceSetDigest()
	if err != nil {
		t.Fatal(err)
	}
	raw, changed, err := scan.refreshedMetadata(lastPublication{bundle: p.Bundle, metadata: prior}, prior.SourceBundle)
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	var next archive.Metadata
	if err := json.Unmarshal(raw, &next); err != nil {
		t.Fatal(err)
	}
	digest, err := next.SourceSetDigest()
	if err != nil || digest != oldDigest || !next.CapturedAt.Equal(prior.CapturedAt) || !slices.Equal(next.History.Preserved, prior.History.Preserved) {
		t.Fatal("parser refresh lost manifest or manufactured capture age", err)
	}
	if next.Parser.Version == prior.Parser.Version {
		t.Fatal("parser not updated")
	}
}

func TestRunCommittedStrongerHistoryAcknowledgesExactAndQueuesWithoutNative(t *testing.T) {
	scan, p := privacyJournal(t)
	for _, stage := range p.History.Sources {
		raw, err := scan.local.ReadPendingSource(scan.id(), stage)
		if err != nil {
			t.Fatal(err)
		}
		if err := scan.remote.Put(t.Context(), stage.Reference.Key, raw); err != nil {
			t.Fatal(err)
		}
	}
	if err := scan.remote.Put(t.Context(), p.MetadataKey, p.MetadataBytes); err != nil {
		t.Fatal(err)
	}
	p.Attempted = true
	if err := scan.local.SavePending(scan.id(), p); err != nil {
		t.Fatal(err)
	}
	if err := scan.local.SaveRequest(scan.id(), "stop", scan.now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	newer, _, err := scan.local.LoadRequest(scan.id())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(scan.reg.ProjectRoot); err != nil {
		t.Fatal(err)
	}
	counted := &privacyPutStore{ObjectStore: scan.remote}
	scan.opts.SkillEvidence = config.SkillEvidenceNone
	result, err := Run(t.Context(), scan.local, counted, scan.opts)
	if err != nil || len(result.Errors) == 0 {
		t.Fatal("stronger successor escaped publication fence", result, err)
	}
	for _, key := range counted.puts {
		if key == p.MetadataKey || strings.Contains(key, "/source.") {
			t.Fatal("broader frozen bytes uploaded", key)
		}
	}
	next, found, err := scan.local.LoadPending(scan.id())
	if err != nil || !found || next.Attempted || next.History.ExpectedMetadataSHA256 != metadataSHA(p.MetadataBytes) {
		t.Fatal("successor absent", err)
	}
	published, err := scan.local.LoadPublishedState(scan.id())
	if err != nil {
		t.Fatal(err)
	}
	m, found, err := published.LastPublishedMetadata()
	if err != nil || !found || m.SourceBundle != p.SourceReference() {
		t.Fatal("exact committed authority not acknowledged", err)
	}
	request, found, err := scan.local.LoadRequest(scan.id())
	if err != nil || !found || request.Token != newer.Token {
		t.Fatal("newer token lost", err)
	}
}

func TestPreparedHistoryDerivationUsesFrozenNextTimeAfterRestart(t *testing.T) {
	scan, p := privacyJournal(t)
	defer scan.releaseRetained()
	var metadata archive.Metadata
	if err := json.Unmarshal(p.MetadataBytes, &metadata); err != nil {
		t.Fatal(err)
	}
	frozen := metadata.MetadataDerivedAt
	if frozen.IsZero() {
		t.Fatal("fixture has no frozen derivation time")
	}
	if err := scan.derivePreparedHistory(&p, metadata); err != nil {
		t.Fatal(err)
	}
	first := bytes.Clone(p.MetadataBytes)
	scan.now = scan.now.Add(72 * time.Hour)
	if err := os.RemoveAll(scan.reg.ProjectRoot); err != nil {
		t.Fatal(err)
	}
	if err := scan.derivePreparedHistory(&p, metadata); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, p.MetadataBytes) {
		t.Fatal("resumed wall clock changed selecting metadata")
	}
	var resumed archive.Metadata
	if err := json.Unmarshal(p.MetadataBytes, &resumed); err != nil || !resumed.MetadataDerivedAt.Equal(frozen) {
		t.Fatal("frozen NEXT derivation time lost", err)
	}
	metadata.MetadataDerivedAt = time.Time{}
	if err := scan.derivePreparedHistory(&p, metadata); err == nil {
		t.Fatal("missing frozen time manufactured a new authority")
	}
}

func TestPublicationPrivacyFactoryOwnsExactTransformAndLease(t *testing.T) {
	scan, p := privacyJournal(t)
	defer scan.releaseRetained()
	var origin archive.Metadata
	if err := json.Unmarshal(p.MetadataBytes, &origin); err != nil {
		t.Fatal(err)
	}
	original := p.Bundle
	original.Capture.FilterVersion = "14"
	for _, record := range original.NativeRecords {
		record["api_key"] = "sk-abcdefghijklmnopqrstuv"
	}
	packed, err := archive.BuildCompressedSource(original)
	if err != nil {
		t.Fatal(err)
	}
	key, err := archive.SourceObjectKey(original, packed.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	ref := archive.SourceReference{Key: key, SHA256: packed.SHA256, CompressedBytes: len(packed.Bytes)}
	origin.SourceBundle, origin.FilterVersion = ref, "14"
	body, err := json.Marshal(origin)
	if err != nil {
		t.Fatal(err)
	}

	adapter, err := sourceAdapter(scan.opts.Sources, scan.reg.Harness.Name)
	if err != nil {
		t.Fatal(err)
	}
	oldPolicy := state.PublicationPolicy{FilterVersion: "14", AdapterVersion: original.Capture.AdapterVersion, SkillEvidence: config.SkillEvidenceBody}
	nextPolicy := state.PublicationPolicy{FilterVersion: archive.FilterVersion, AdapterVersion: adapter.Version(), SkillEvidence: config.SkillEvidenceNone}
	frozen := state.PublicationContext{DestinationID: scan.reg.DestinationID, AdmissionContext: scan.publicationAdmission()}
	budget := agentapi.NewNativeReadBudget(128 << 20)
	reservation := budget.Reservation()
	if !reservation.Reserve(int64(len(packed.Bytes))) {
		t.Fatal("fixture source loan")
	}
	hooks := []archive.SupplementalEvidence{{Kind: archive.EvidenceKindExplicitFeedback, Provenance: "synthetic-hook", ObservedAt: scan.now, Payload: map[string]any{"text": "combined hook sk-abcdefghijklmnopqrstuv"}}}
	hookBody, err := json.Marshal(hooks)
	if err != nil {
		t.Fatal(err)
	}
	if !reservation.Reserve(int64(len(hookBody))) {
		t.Fatal("observation fixture loan")
	}
	defer reservation.Release(int64(len(hookBody)))
	candidate := state.PendingPublication{Bundle: original, MetadataBytes: body, MetadataKey: p.MetadataKey, SourceKey: ref.Key, SourceSHA256: ref.SHA256, SourceBytes: packed.Bytes, SkillEvidence: "body", History: &state.PendingHistory{Version: 1, Preparing: true}}
	if err := state.FreezePublicationHookObservations(t.Context(), &candidate, hookBody, frozen.DestinationID, frozen.AdmissionContext, nextPolicy.Context(), budget); err != nil {
		t.Fatal(err)
	}
	candidate, err = state.PreparePublicationV2(candidate, state.PublicationPredecessor{State: state.PredecessorAbsent}, frozen.DestinationID, frozen.AdmissionContext, nextPolicy.Context(), state.PublicationPrivacyRewrite)
	if err != nil {
		t.Fatal(err)
	}
	input := state.PreparationInput{HookObservations: candidate.Preparation.Inputs[0].HookObservations, Reference: ref, Selection: state.PublicationSelection{Role: state.PublicationCurrent, RevisionID: origin.History.CurrentRevision, CapturedAt: original.Capture.CapturedAt, SourceSchemaVersion: original.SchemaVersion}, FilterVersion: "14", AdapterVersion: original.Capture.AdapterVersion, SkillPolicy: "body"}
	before := budget.Available()
	out, encoded, next, proof, release, err := state.RefilterPublicationInput(t.Context(), scan.reg, adapter, origin, body, input, 0, packed.Bytes, frozen, oldPolicy, nextPolicy, config.SkillEvidenceBody, budget)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(out)
	if err != nil || bytes.Contains(raw, []byte("sk-abcdefghijklmnopqrstuv")) {
		t.Fatal("actual filter failed", err)
	}
	if next.SHA256 != encoded.SHA256 || next.CompressedBytes != len(encoded.Bytes) || next == ref {
		t.Fatal("canonical output not bound")
	}
	receiptRelease, recordErr := p.RecordPrivacyOutput(proof)
	if recordErr != nil {
		err = recordErr
		t.Fatal(err)
	}
	defer receiptRelease()
	if budget.Available() >= before {
		t.Fatal("factory output uncharged")
	}
	release()
	release()
	if budget.Available() >= before {
		t.Fatal("persisted receipt ownership ended with factory output")
	}
	receiptRelease()
	receiptRelease()
	if budget.Available() != before {
		t.Fatal("factory lease not released exactly once", budget.Available(), before)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, _, _, _, err = state.RefilterPublicationInput(canceled, scan.reg, adapter, origin, body, input, 0, packed.Bytes, frozen, oldPolicy, nextPolicy, config.SkillEvidenceBody, budget); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation ignored", err)
	}
	if _, _, _, _, _, err = state.RefilterPublicationInput(t.Context(), scan.reg, adapter, origin, body, input, 0, packed.Bytes, frozen, oldPolicy, nextPolicy, config.SkillEvidenceBody, agentapi.NewNativeReadBudget(32<<10)); !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatal("lease pressure ignored", err)
	}
	corrupted := bytes.Clone(packed.Bytes)
	corrupted[0] ^= 1
	if _, _, _, _, _, err = state.RefilterPublicationInput(t.Context(), scan.reg, adapter, origin, body, input, 0, corrupted, frozen, oldPolicy, nextPolicy, config.SkillEvidenceBody, budget); err == nil {
		t.Fatal("foreign original bytes accepted")
	}
	for _, mutate := range []func(*state.PublicationHookObservations){
		func(h *state.PublicationHookObservations) { h.Body = bytes.Clone(h.Body); h.Body[0] ^= 1 },
		func(h *state.PublicationHookObservations) { h.Facts.RevisionID = "foreign" },
		func(h *state.PublicationHookObservations) { h.Facts.CapturedAt = h.Facts.CapturedAt.Add(time.Second) },
		func(h *state.PublicationHookObservations) { h.Facts.AdmissionContext = "foreign" },
	} {
		wrong := input
		h := *input.HookObservations
		mutate(&h)
		wrong.HookObservations = &h
		available := budget.Available()
		if _, _, _, _, _, err := state.RefilterPublicationInput(t.Context(), scan.reg, adapter, origin, body, wrong, 0, packed.Bytes, frozen, oldPolicy, nextPolicy, config.SkillEvidenceBody, budget); !errors.Is(err, state.ErrDurableStorageRecovery) {
			t.Fatal("wrong frozen observation accepted", err)
		}
		if budget.Available() != available {
			t.Fatal("rejected observation leaked loan")
		}
	}
	reservation.Release(int64(len(packed.Bytes)))
}

func TestPublicationPrivacySettlementMintBudgetRefusalRetainsFullPending(t *testing.T) {
	scan, original := privacyJournal(t)
	defer scan.releaseRetained()
	scan.opts.SkillEvidence = config.SkillEvidenceNone
	p, err := scan.stricterHistorySuccessor(original, false)
	if err != nil || p.ValidatePublication() != nil || p.Commit == nil || p.Commit.Purpose != state.PublicationPrivacyRewrite {
		t.Fatal("actual ready privacy transaction missing", err)
	}
	if err = scan.local.SavePending(scan.id(), p); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(scan.local.Home(), "pending", scan.id()+".json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The same validation loan succeeds first; mint then needs independently
	// owned projection facts while that loan is still held. Leave one byte
	// less than that overlap, rather than merely failing the entry decoder.
	metadataBytes := len(p.MetadataBytes) + len(p.Preparation.OriginMetadata) + len(p.Preparation.PrivacyPreviousMetadata)
	if p.Preparation.Migration != nil {
		metadataBytes += len(p.Preparation.Migration.PreviousMetadata)
	}
	facts := len(p.Sources) + len(p.Progress.Outputs) + len(p.Preparation.Inputs)
	validationLoan := int64(8*metadataBytes + (facts+1)*(16<<10))
	var origin archive.Metadata
	if err := json.Unmarshal(p.Preparation.OriginMetadata, &origin); err != nil {
		t.Fatal(err)
	}
	identityBytes := int64(len(origin.SessionID) + len(origin.NativeSessionID) + len(origin.ProjectID) + len(origin.MachineID) + len(origin.Harness.Name) + len(origin.Harness.Version) + len(origin.Harness.Mode) + len(origin.Origin) + len(origin.StartedAtSource) + len(origin.PreviousGenerationID))
	if identityBytes == 0 {
		t.Fatal("identity string fixture missing")
	}
	projectionFacts := int64(len(p.Preparation.Inputs)+1)*(16<<10) + identityBytes
	for _, canceled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		budget := agentapi.NewNativeReadBudget(validationLoan + projectionFacts - 1)
		local, closeScope := scan.local.WithReadBudget(ctx, budget)
		published, err := local.LoadPublishedState(scan.id())
		if err != nil {
			t.Fatal(err)
		}
		if canceled {
			cancel()
		}
		err = published.SaveCommittedPublication(p, scan.now)
		if canceled && !errors.Is(err, context.Canceled) || !canceled && !errors.Is(err, agentapi.ErrReadBudget) {
			t.Fatal("mint failed with wrong refusal", canceled, err)
		}
		if published.Found() {
			t.Fatal("failed mint partially selected publication")
		}
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("full pending changed on failed mint", err)
		}
		if _, err := os.Stat(publishedPath(scan.local, scan.id())); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("failed mint installed published control", err)
		}
		closeScope()
		cancel()
		if used, _ := budget.Charged(); used != 0 {
			t.Fatal("mint ownership leaked", used)
		}
	}
	budget := agentapi.NewNativeReadBudget(256 << 20)
	local, closeScope := scan.local.WithReadBudget(t.Context(), budget)
	published, err := local.LoadPublishedState(scan.id())
	if err != nil {
		t.Fatal(err)
	}
	if err := published.SaveCommittedPublication(p, scan.now); err != nil {
		t.Fatal(err)
	}
	used, _ := budget.Charged()
	if used < projectionFacts || used >= validationLoan+projectionFacts {
		t.Fatal("mint did not retain fact ownership after releasing decode loan", used, projectionFacts, validationLoan)
	}
	closeScope()
	closeScope()
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("settled fact ownership did not release once", used)
	}

}

// Inspect real disposable private files, including base64 metadata/inline bodies
// and gzip payloads. These fixtures are tiny; refuse a surprise large fixture.
func assertPrivateTreeHasNoSecret(t *testing.T, home, secret string, verifiedOwedRequest ...string) {
	t.Helper()
	var inspectingPath string
	var inspect func([]byte, int)
	inspect = func(raw []byte, depth int) {
		if depth > 8 || len(raw) > 4<<20 {
			t.Fatal("unexpected private fixture expansion")
		}
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatal("obsolete sensitive private bytes survived full cleanup", inspectingPath, "decoded depth", depth)
		}
		if len(raw) > 2 && raw[0] == 0x1f && raw[1] == 0x8b {
			r, err := gzip.NewReader(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(io.LimitReader(r, (4<<20)+1))
			_ = r.Close()
			if err != nil {
				t.Fatal(err)
			}
			inspect(body, depth+1)
			return
		}
		var v any
		if json.Unmarshal(raw, &v) != nil {
			return
		}
		var walk func(any)
		walk = func(v any) {
			switch x := v.(type) {
			case string:
				if b, err := base64.StdEncoding.DecodeString(x); err == nil && len(b) > 0 {
					inspect(b, depth+1)
				}
			case []any:
				for _, item := range x {
					walk(item)
				}
			case map[string]any:
				for _, item := range x {
					walk(item)
				}
			}
		}
		walk(v)
	}
	if err := filepath.WalkDir(home, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if slices.Contains(verifiedOwedRequest, path) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() > 4<<20 {
			t.Fatal("unexpected private fixture file", path)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		inspectingPath = path
		inspect(raw, 0)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Unknown original header facts can become explicit only from the same retained
// checksum-backed source; all other original reference, schema, age and policy facts stay exact.
func assertOriginalHistoryInput(t *testing.T, store *state.Store, id string, got, original state.HistoryInput) {
	t.Helper()
	raw, err := store.ReadPendingSource(id, state.PendingSource{Reference: original.Reference, Name: original.Reference.SHA256 + ".gz"})
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := archive.ReadSourceBundle(bytes.NewReader(raw), archive.DecodeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if original.ParentSessionID == nil {
		original.ParentSessionID = &bundle.ParentSessionID
	}
	if original.NativeChild == nil {
		original.NativeChild = &bundle.NativeChild
	}
	if !reflect.DeepEqual(got, original) {
		t.Fatal("original evidence/provenance changed", got, original)
	}
}

func TestCommittedHistoryAcknowledgesNarrowerBaselineAfterSkillPolicyLoosens(t *testing.T) {
	for _, frozen := range []config.SkillEvidence{config.SkillEvidenceNone, config.SkillEvidenceMetadata} {
		t.Run(string(frozen), func(t *testing.T) {
			scan, _ := reconciliationFixture(t)
			scan.opts.SkillEvidence = frozen
			if _, err := scan.run(scan.ctx); !errors.Is(err, archive.ErrHistoryMutationPending) {
				t.Fatal(err)
			}
			p, found, err := scan.local.LoadPending(scan.id())
			if err != nil || !found {
				t.Fatal(found, err)
			}
			for p.History.Preparing {
				if err := scan.advanceHistoryPreparation(&p); err != nil {
					t.Fatal(err)
				}
			}
			for _, stage := range p.History.Sources {
				raw, err := scan.local.ReadPendingSource(scan.id(), stage)
				if err != nil {
					t.Fatal(err)
				}
				if err := scan.remote.Put(t.Context(), stage.Reference.Key, raw); err != nil {
					t.Fatal(err)
				}
			}
			if err := scan.remote.Put(t.Context(), p.MetadataKey, p.MetadataBytes); err != nil {
				t.Fatal(err)
			}
			p.Attempted = true
			if err := scan.local.SavePending(scan.id(), p); err != nil {
				t.Fatal(err)
			}
			counted := &privacyPutStore{ObjectStore: scan.remote}
			scan.remote = counted
			scan.opts.SkillEvidence = config.SkillEvidenceBody
			if _, err := scan.resumeStricterHistory(p); !errors.Is(err, archive.ErrHistoryMutationPending) {
				t.Fatal("narrower committed baseline could not be acknowledged", err)
			}
			acknowledged, found, err := scan.published.LastPublishedMetadata()
			if err != nil || !found || acknowledged.SourceBundle != p.SourceReference() {
				t.Fatal("exact narrower baseline was not acknowledged", found, err)
			}
			for _, key := range counted.puts {
				if key == p.MetadataKey || strings.Contains(key, "/source.") {
					t.Fatal("acknowledgement rewrote remote source or metadata", key)
				}
			}
			next, found, err := scan.local.LoadPublicationPending(scan.id())
			if err != nil || !found || next.Preparation == nil {
				t.Fatal("retained successor was not frozen", found, err)
			}
			for _, input := range next.Preparation.Inputs {
				if input.SkillPolicy != string(frozen) {
					t.Fatal("successor lost the original evidence ceiling", input.SkillPolicy)
				}
			}
		})
	}
}
