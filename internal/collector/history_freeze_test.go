package collector

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
)

func TestRunStagesCompleteRevisionJournalAndResumesWithoutNativeReads(t *testing.T) {
	scan, lookup := reconciliationFixture(t)
	if err := scan.local.SaveRequest(scan.id(), "stop", scan.now); err != nil {
		t.Fatal(err)
	}
	opts := scan.opts
	opts.RepoKey = func(string) string { return "" }
	for pass := range 3 {
		if pass > 0 {
			reopened, err := openTestStore(scan.local.Home())
			if err != nil {
				t.Fatal(err)
			}
			scan.local = reopened
		}
		result, err := Run(t.Context(), scan.local, scan.remote, opts)
		if pass == 2 {
			if err != nil || len(result.Published) != 1 || len(result.Errors) != 0 {
				t.Fatal("final publication failed", result, err)
			}
			if _, found, err := scan.local.LoadPending(scan.id()); err != nil || found {
				t.Fatal("final journal not cleaned", err)
			}
			if _, found, err := scan.local.LoadRequest(scan.id()); err != nil || !found {
				t.Fatal("newer request lost", err)
			}
			return
		}
		if err != nil || len(result.Published) != 0 || len(result.Errors) == 0 {
			t.Fatal("preparation settled early", result, err)
		}
		pending, found, err := scan.local.LoadPending(scan.id())
		if err != nil || !found {
			t.Fatal("journal missing", found, err)
		}
		if pending.Attempted || pending.History == nil || len(pending.History.Sources) != 3 || len(pending.History.Inputs) != 3 {
			t.Fatal("incomplete freeze", pending.History)
		}
		var metadata archive.Metadata
		if err := json.Unmarshal(pending.MetadataBytes, &metadata); err != nil {
			t.Fatal(err)
		}
		if len(metadata.History.Preserved) != 2 || metadata.History.CurrentRevision != revisionC {
			t.Fatal(metadata.History)
		}
		for _, input := range pending.History.Inputs {
			if !input.CapturedAt.Equal(scan.now) || input.FilterVersion != archive.FilterVersion || input.SourceSchemaVersion != archive.HistorySourceSchemaVersion {
				t.Fatal("capture/provenance changed", input)
			}
		}
		for _, stage := range pending.History.Sources {
			raw, err := scan.local.ReadPendingSource(scan.id(), stage)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := decodeHistoryStage(t.Context(), metadata, pending, stage.Reference, raw); err != nil {
				t.Fatal(err)
			}
		}
		if pass == 0 {
			// All native evidence disappears, lookup contradicts its earlier result,
			// and a newer token arrives. The frozen observation remains authoritative.
			for _, refs := range lookup.refs {
				for _, ref := range refs {
					if err := os.Remove(ref.Path); err != nil {
						t.Fatal(err)
					}
				}
			}
			lookup.changed = true
			if err := scan.local.SaveRequest(scan.id(), "stop", scan.now.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
		}
		request, found, err := scan.local.LoadRequest(scan.id())
		if err != nil || !found || pass > 0 && request.Token == pending.RequestToken {
			t.Fatal("newer request lost", err)
		}
		if pass >= 2 && (pending.History.Preparing || !pending.History.PreparedAt.Equal(scan.now)) {
			t.Fatal("preparation not frozen", pending.History)
		}
	}
	objects, err := scan.remote.List(t.Context(), "")
	if err != nil || len(objects) != 0 {
		t.Fatal("preparation uploaded", err)
	}
}

func TestHistoryPolicyMismatchReplacesDescriptorButKeepsOriginalEvidence(t *testing.T) {
	for _, attempted := range []bool{false, true} {
		t.Run(map[bool]string{false: "preparing", true: "attempted"}[attempted], func(t *testing.T) {
			scan, _ := reconciliationFixture(t)
			read, ok, err := scan.read()
			if err != nil || !ok {
				t.Fatal(err)
			}
			candidate, _, err := scan.build(read)
			if err != nil {
				t.Fatal(err)
			}
			scan.revisions, err = scan.reconcileRevisions(read, candidate)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := scan.publish(scan.ctx, read, candidate); !errors.Is(err, archive.ErrHistoryMutationPending) {
				t.Fatal(err)
			}
			p, found, err := scan.local.LoadPublicationPending(scan.id())
			if err != nil || !found {
				t.Fatal(err)
			}
			if attempted {
				for p.History.Preparing {
					if err := scan.advanceHistoryPreparation(&p); err != nil {
						t.Fatal(err)
					}
				}
				p.Attempted = true
				if err := scan.local.SavePending(scan.id(), p); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(scan.local.Home(), "pending", scan.id()+".json")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			scan.opts.SkillEvidence = config.SkillEvidenceNone
			if _, handled, err := scan.resume(scan.ctx); err == nil || !handled {
				t.Fatal("policy bypass", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || bytes.Equal(before, after) {
				t.Fatal("stricter successor was not persisted", err)
			}
			next, found, err := scan.local.LoadPublicationPending(scan.id())
			if err != nil || !found || next.Attempted || next.History.Preparing || len(p.History.Inputs) != len(next.History.Inputs) {
				t.Fatal("original evidence/provenance discarded", err)
			}
			for i, input := range p.History.Inputs {
				assertOriginalHistoryInput(t, scan.local, scan.id(), next.History.Inputs[i], input)
			}
			for _, stage := range p.History.Sources {
				if _, err := scan.local.ReadPendingSource(scan.id(), stage); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestJournalSaveFailureRetainsVisibleSourceOnlyRecovery(t *testing.T) {
	scan, _ := reconciliationFixture(t)
	journal := filepath.Join(scan.local.Home(), "pending", scan.id()+".json")
	if err := os.MkdirAll(journal, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := scan.run(scan.ctx); err == nil {
		t.Fatal("journal failure ignored")
	}
	dir := filepath.Join(scan.local.Home(), "sessions", scan.id(), "pending-sources")
	// A damaged existing journal is refused before staging. To exercise the
	// stage-before-journal point, construct from already reconciled evidence.
	if err := os.Remove(journal); err != nil {
		t.Fatal(err)
	}
	read, ok, err := scan.read()
	if err != nil || !ok {
		t.Fatal(err)
	}
	candidate, _, err := scan.build(read)
	if err != nil {
		t.Fatal(err)
	}
	scan.revisions, err = scan.reconcileRevisions(read, candidate)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := renderPublication(scan.ctx, scan.resolveParser(), scan.parserVersion(), candidate, scan.reg, scan.now, scan.opts, scan.priorRepoKey)
	if err != nil {
		t.Fatal(err)
	}
	pending := state.PendingPublication{Bundle: candidate, SkillEvidence: string(scan.opts.skillEvidence()), SourceKey: rendered.source.Key, SourceSHA256: rendered.source.SHA256, SourceBytes: rendered.sourceBytes, MetadataKey: rendered.metadataKey, MetadataBytes: rendered.metadata}
	if err := scan.freezeRevisionPublication(&pending); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(journal, 0700); err != nil {
		t.Fatal(err)
	}
	if err := scan.local.SavePending(scan.id(), pending); err == nil {
		t.Fatal("journal save unexpectedly succeeded")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 3 {
		t.Fatal("stages missing before journal", len(entries), err)
	}
	if err := os.Remove(journal); err != nil {
		t.Fatal(err)
	}
	if err := scan.local.SweepPendingSources(scan.id()); !errors.Is(err, state.ErrDurableStorageRecovery) {
		t.Fatal("source-only sweep did not refuse", err)
	}
	if _, found, err := scan.local.LoadPending(scan.id()); !found || !errors.Is(err, state.ErrDurableStorageRecovery) {
		t.Fatalf("source-only recovery lost: %v %v", found, err)
	}
	if present, err := scan.local.HasPending(scan.id()); !present || err != nil {
		t.Fatalf("source-only visibility: %v %v", present, err)
	}
	for _, stage := range pending.History.Sources {
		if _, err := scan.local.ReadPendingSource(scan.id(), stage); err != nil {
			t.Fatal("retained bytes lost", err)
		}
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("abandoned stages lost", err)
	}
}

func TestMissingFrozenInputCannotUseChangedNativeBytes(t *testing.T) {
	scan, _ := reconciliationFixture(t)
	if _, err := scan.run(scan.ctx); !errors.Is(err, archive.ErrHistoryMutationPending) {
		t.Fatal(err)
	}
	p, found, err := scan.local.LoadPending(scan.id())
	if err != nil || !found {
		t.Fatal(err)
	}
	input := p.History.Inputs[p.History.PrivacyCursor]
	path := filepath.Join(scan.local.Home(), "sessions", scan.id(), "pending-sources", input.Reference.SHA256+".gz")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	before := bytes.Clone(p.MetadataBytes)
	if _, _, err := scan.resume(scan.ctx); err == nil {
		t.Fatal("missing frozen input regenerated")
	}
	after, found, err := scan.local.LoadPending(scan.id())
	if err != nil || !found || after.History.PrivacyCursor != p.History.PrivacyCursor || !bytes.Equal(before, after.MetadataBytes) {
		t.Fatal("descriptor changed after frozen evidence loss", err)
	}
	if err := scan.local.SweepPendingSources(scan.id()); err != nil {
		t.Fatal(err)
	}
	for _, stage := range p.History.Sources {
		if stage.Reference == input.Reference {
			continue
		}
		if _, err := scan.local.ReadPendingSource(scan.id(), stage); err != nil {
			t.Fatal("live stage swept", err)
		}
	}
}

func TestFrozenDescriptorUsesExactAcknowledgedPredecessorAndRetainedTimes(t *testing.T) {
	scan, lookup := reconciliationFixture(t)
	a := lookup.refs[revisionThread][0]
	current := lookup.set.Current
	lookup.set.Current = &a
	read, ok, err := scan.read()
	if err != nil || !ok {
		t.Fatal(err)
	}
	prior, _, err := scan.build(read)
	if err != nil {
		t.Fatal(err)
	}
	at := scan.now.Add(-time.Hour)
	prior.Capture.CapturedAt = at
	rendered, err := renderPublication(scan.ctx, scan.resolveParser(), scan.parserVersion(), prior, scan.reg, at, scan.opts, scan.priorRepoKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := scan.remote.Put(scan.ctx, rendered.source.Key, rendered.sourceBytes); err != nil {
		t.Fatal(err)
	}
	if err := scan.remote.Put(scan.ctx, rendered.metadataKey, rendered.metadata); err != nil {
		t.Fatal(err)
	}
	if err := scan.published.SavePublication(prior, at, rendered.source, rendered.metadata); err != nil {
		t.Fatal(err)
	}
	lookup.set.Current = current
	read, ok, err = scan.read()
	if err != nil || !ok {
		t.Fatal(err)
	}
	candidate, _, err := scan.build(read)
	if err != nil {
		t.Fatal(err)
	}
	scan.revisions, err = scan.reconcileRevisions(read, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scan.publish(scan.ctx, read, candidate); !errors.Is(err, archive.ErrHistoryMutationPending) {
		t.Fatal(err)
	}
	p, found, err := scan.local.LoadPending(scan.id())
	if err != nil || !found {
		t.Fatal(err)
	}
	if p.History.ExpectedMetadataSHA256 != metadataSHA(rendered.metadata) {
		t.Fatal("predecessor SHA changed")
	}
	var metadata archive.Metadata
	if err := json.Unmarshal(p.MetadataBytes, &metadata); err != nil {
		t.Fatal(err)
	}
	foundPrior := false
	for _, revision := range metadata.History.Preserved {
		if revision.RevisionID == revisionThread {
			foundPrior = true
			if !revision.CapturedAt.Equal(at) || revision.Source != rendered.source {
				t.Fatal("acknowledged evidence regenerated", revision)
			}
		}
	}
	if !foundPrior || len(p.History.Retired) != 0 {
		t.Fatal("retained predecessor retired", p.History)
	}
}

func TestSamePhysicalJournalGuardPreservesAgeAndRefusesContradiction(t *testing.T) {
	scan, _ := reconciliationFixture(t)
	read, ok, err := scan.read()
	if err != nil || !ok {
		t.Fatal(err)
	}
	candidate, _, err := scan.build(read)
	if err != nil {
		t.Fatal(err)
	}
	prior := candidate
	prior.Capture.CapturedAt = scan.now.Add(-time.Hour)
	if err := scan.published.Save(prior, prior.Capture.CapturedAt, state.CacheStatusRateLimited); err != nil {
		t.Fatal(err)
	}
	if err := scan.guardRevisionCandidate(read, &candidate); err != nil || !candidate.Capture.CapturedAt.Equal(prior.Capture.CapturedAt) {
		t.Fatal("age changed without meaningful evidence", err)
	}
	raw, err := json.Marshal(candidate)
	if err != nil {
		t.Fatal(err)
	}
	var changed archive.SourceBundle
	if err := json.Unmarshal(raw, &changed); err != nil {
		t.Fatal(err)
	}
	last := len(changed.NativeRecords) - 1
	changed.NativeRecords[last] = map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "contradictory owned evidence"}}}}
	if err := scan.guardRevisionCandidate(read, &changed); err == nil {
		t.Fatal("same-physical contradiction journaled")
	}
}
