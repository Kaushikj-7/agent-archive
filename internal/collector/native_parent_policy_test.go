package collector

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agents/codex"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
)

// Exercise completed, source-first repair retries as well as the interrupted
// preparation covered by the CLI. Originals include source2 and source3 and
// deliberately retain earlier-filter secrets until actual maintenance runs.
func TestNativeParentPolicySuccessorPreservesOriginalHeaderAuthority(t *testing.T) {
	for _, name := range []string{"provenance", "legacy_descriptor", "committed_legacy_descriptor"} {
		t.Run(name, func(t *testing.T) {
			scan, p := privacyJournal(t)
			var metadata archive.Metadata
			if err := json.Unmarshal(p.MetadataBytes, &metadata); err != nil {
				t.Fatal(err)
			}
			originalInputs := append([]state.HistoryInput(nil), p.History.Inputs...)
			originals := map[string]archive.SourceBundle{}
			metadata.NativeChild = true
			for i, input := range originalInputs {
				b, err := scan.loadHistoryInput(metadata, input)
				if err != nil {
					t.Fatal(err)
				}
				b.NativeChild = true
				b.Capture.FilterVersion = "14"
				if i == 1 {
					b.SchemaVersion, b.History, b.Ordinals = archive.SourceSchemaVersion, nil, nil
				}
				b.NativeRecords[len(b.NativeRecords)-1] = map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "visible DB_PASSWORD=" + plantedSecret}}}}
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
				originals[input.RevisionID] = b
				if input.RevisionID == metadata.History.CurrentRevision {
					metadata.SourceBundle, metadata.FilterVersion = ref, "14"
					p.Bundle = b
				} else {
					for j := range metadata.History.Preserved {
						r := &metadata.History.Preserved[j]
						if r.RevisionID == input.RevisionID {
							r.Source, r.FilterVersion, r.SourceSchemaVersion = ref, "14", b.SchemaVersion
						}
					}
				}
			}
			raw, err := json.Marshal(metadata)
			if err != nil {
				t.Fatal(err)
			}
			if err := scan.remote.Put(t.Context(), p.MetadataKey, raw); err != nil {
				t.Fatal(err)
			}
			if err := scan.published.SavePublication(p.Bundle, scan.now, metadata.SourceBundle, raw); err != nil {
				t.Fatal(err)
			}
			if err := scan.local.RemovePending(scan.id()); err != nil {
				t.Fatal(err)
			}
			scan.reg.NativeChild, scan.reg.ParentSessionID = true, "qualified-late-parent"
			scan.opts.SkillEvidence = config.SkillEvidenceBody
			if _, handled, err := scan.prepareRetainedHistoryWork(scan.ctx); !handled || !errors.Is(err, archive.ErrHistoryMutationPending) {
				t.Fatal("actual maintenance did not freeze", handled, err)
			}
			p, found, err := scan.local.LoadPending(scan.id())
			if err != nil || !found {
				t.Fatal(err)
			}
			for p.History.Preparing {
				if err := scan.advanceHistoryPreparation(&p); err != nil {
					t.Fatal(err)
				}
			}
			// Finished private work has not changed its exact remote predecessor.
			current, err := scan.remote.Get(t.Context(), p.MetadataKey)
			if err != nil || !bytes.Equal(current, raw) {
				t.Fatal("preparation changed remote authority", err)
			}
			if name == "committed_legacy_descriptor" {
				for _, stage := range p.History.Sources {
					data, err := scan.local.ReadPendingSource(scan.id(), stage)
					if err != nil {
						t.Fatal(err)
					}
					if err := scan.remote.Put(t.Context(), stage.Reference.Key, data); err != nil {
						t.Fatal(err)
					}
				}
				if err := scan.remote.Put(t.Context(), p.MetadataKey, p.MetadataBytes); err != nil {
					t.Fatal(err)
				}
				p.Attempted = true
			}
			if name != "provenance" {
				for i := range p.History.Inputs {
					p.History.Inputs[i].ParentSessionID = nil
				}
			}
			if err := scan.local.SavePending(scan.id(), p); err != nil {
				t.Fatal(err)
			}
			scan.local, err = state.Open(scan.local.Home())
			if err != nil {
				t.Fatal(err)
			}
			scan.published, err = scan.local.LoadPublishedState(scan.id())
			if err != nil {
				t.Fatal(err)
			}
			p, found, err = scan.local.LoadPending(scan.id())
			if err != nil || !found {
				t.Fatal(err)
			}
			scan.opts.SkillEvidence = config.SkillEvidenceNone
			if _, err := scan.resumeHistory(scan.ctx, p); !errors.Is(err, archive.ErrHistoryMutationPending) {
				t.Fatal("completed repair policy successor", err)
			}
			next, found, err := scan.local.LoadPending(scan.id())
			if err != nil || !found {
				t.Fatal(err)
			}
			for _, input := range next.History.Inputs {
				if input.ParentSessionID == nil || *input.ParentSessionID != "" {
					t.Fatal("lost original unresolved parent", input)
				}
				b, err := scan.loadHistoryInput(metadata, input)
				if err != nil {
					t.Fatal("original became unreadable", err)
				}
				original := originals[input.RevisionID]
				if !reflect.DeepEqual(b.NativeRecords, original.NativeRecords) || !reflect.DeepEqual(b.Ordinals, original.Ordinals) {
					t.Fatal("original input changed")
				}
			}
			if outcome, err := scan.publishPending(scan.ctx, next); err != nil || outcome != outcomePublished {
				t.Fatal("strict repair readback", outcome, err)
			}
			final, err := scan.remote.Get(t.Context(), next.MetadataKey)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(final, &metadata); err != nil {
				t.Fatal(err)
			}
			for id, original := range originals {
				b, err := reader.LoadRevision(t.Context(), scan.remote, metadata, id, reader.Limits{})
				if err != nil {
					t.Fatal(err)
				}
				encoded, err := json.Marshal(b)
				if err != nil {
					t.Fatal(err)
				}
				if bytes.Contains(encoded, []byte(plantedSecret)) || b.ParentSessionID != scan.reg.ParentSessionID || !b.Capture.CapturedAt.Equal(original.Capture.CapturedAt) || !reflect.DeepEqual(b.Ordinals, original.Ordinals) || !reflect.DeepEqual(b.History, original.History) {
					t.Fatal("retained policy/parent/age/raw ownership changed", id)
				}
			}
		})
	}
}

func TestNativeParentPolicyRefusesKnownConflictThroughLeasedPort(t *testing.T) {
	scan, p := privacyJournal(t)
	// These actual leased retained refilters previously bypassed the parent check
	// in refilterBundleBounded, which a stricter successor also uses indirectly.
	scan.reg.NativeChild, scan.reg.ParentSessionID = true, "conflicting-parent"
	p.Bundle.NativeChild, p.Bundle.ParentSessionID = true, "known-parent"
	before := snapshotMtimes(t, scan.local.Home())
	if _, err := scan.refilterRetained(codex.Filter{}, p.Bundle); err == nil {
		t.Fatal("leased retained port overwrote known parent")
	}
	if !reflect.DeepEqual(before, snapshotMtimes(t, scan.local.Home())) {
		t.Fatal("conflict mutated journal/stages")
	}
}
