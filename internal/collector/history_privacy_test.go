package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

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
	out, err := refilterBundle(t.Context(), scan.reg, codex.Filter{}, b)
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
	again, err := refilterBundle(t.Context(), scan.reg, codex.Filter{}, out)
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
	next, found, err := scan.local.LoadPending(scan.id())
	if err != nil || !found || next.Attempted || next.History.Preparing || len(next.History.Inputs) != len(before) {
		t.Fatal(next.History, err)
	}
	for i, input := range next.History.Inputs {
		if !reflect.DeepEqual(input, before[i]) {
			t.Fatal("original evidence/provenance lost")
		}
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
	p.History.Preparing, p.History.PrivacyCursor, p.History.PreparedAt = true, 0, time.Time{}
	for range len(p.History.Inputs) {
		if err := scan.local.SavePending(scan.id(), p); err != nil {
			t.Fatal(err)
		}
		scan.local, err = openTestStore(scan.local.Home())
		if err != nil {
			t.Fatal(err)
		}
		p, _, err = scan.local.LoadPending(scan.id())
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
	if err := scan.local.SaveRequest(scan.id(), "stop", scan.now); err != nil {
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
			if err != nil || !found || current.Token != request.Token {
				t.Fatal("live native request lost", err)
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
				if err != nil || b.Capture.FilterVersion != archive.FilterVersion || !b.Capture.CapturedAt.Equal(scan.now) {
					t.Fatal("partial refilter or changed capture", err)
				}
			}
			if _, found, err := scan.local.LoadPending(scan.id()); err != nil || found {
				t.Fatal("maintenance cleanup incomplete", err)
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
	p, found, err := scan.local.LoadPending(scan.id())
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
	next, found, err := scan.local.LoadPending(scan.id())
	if err != nil || !found || next.History.MaintenanceOwed || next.Attempted || !reflect.DeepEqual(next.History.Inputs[1], original) {
		t.Fatal("restart lost original provenance", err)
	}
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
