package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestRunHistoryStageCreationFailureReopensWithoutPublication(t *testing.T) {
	scan, lookup := reconciliationFixture(t)
	remote := &historyCrashStore{MemoryStore: scan.remote.(*storagetest.MemoryStore)}
	parent := filepath.Join(scan.local.Home(), "sessions", scan.id())
	if err := os.MkdirAll(parent, 0700); err != nil {
		t.Fatal(err)
	}
	restore := historyReadOnlyDirectory(t, parent)
	if err := scan.local.SaveRequest(scan.id(), "stop", scan.now); err != nil {
		t.Fatal(err)
	}
	request, _, err := scan.local.LoadRequest(scan.id())
	if err != nil {
		t.Fatal(err)
	}
	result, err := Run(t.Context(), scan.local, remote, scan.opts)
	if err != nil || !errors.Is(result.Errors[scan.id()], os.ErrPermission) || !strings.Contains(result.Errors[scan.id()].Error(), "pending-sources") || len(result.Published) != 0 || remote.puts != 0 {
		t.Fatal("stage failure escaped its durable boundary", result, err)
	}
	if _, found, err := scan.local.LoadPending(scan.id()); err != nil || found {
		t.Fatal("failed stage created an authoritative journal", err)
	}
	assertHistoryRequest(t, scan, request.Token)
	restore()
	scan.local, err = openTestStore(scan.local.Home())
	if err != nil {
		t.Fatal(err)
	}
	result, err = Run(t.Context(), scan.local, remote, scan.opts)
	if err != nil || !errors.Is(result.Errors[scan.id()], archive.ErrHistoryMutationPending) || remote.puts != 0 {
		t.Fatal("reopened stage retry did not freeze privately", result, err)
	}
	pending, found, err := scan.local.LoadPending(scan.id())
	if err != nil || !found || pending.Attempted || pending.History == nil || len(pending.History.Inputs) != 3 {
		t.Fatal("stage retry lost its complete frozen set", err)
	}
	for _, input := range pending.History.Inputs {
		if !input.CapturedAt.Equal(scan.now) {
			t.Fatal("stage retry changed capture provenance", input)
		}
	}
	lookup.changed = true
	if err := os.RemoveAll(scan.reg.ProjectRoot); err != nil {
		t.Fatal(err)
	}
	if err := scan.local.SaveRequest(scan.id(), "newer", scan.now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	newer, _, err := scan.local.LoadRequest(scan.id())
	if err != nil {
		t.Fatal(err)
	}
	final := convergeHistoryLocalRetry(t, scan, remote)
	if !bytes.Equal(final.SourceBytes, pending.SourceBytes) || !final.Bundle.Capture.CapturedAt.Equal(pending.Bundle.Capture.CapturedAt) {
		t.Fatal("frozen stage retry rebuilt newer native evidence")
	}
	assertHistoryRequest(t, scan, newer.Token)
}

func TestRunHistoryPreparationCheckpointFailureKeepsPriorDescriptor(t *testing.T) {
	scan, pending := privacyJournal(t)
	defer scan.releaseRetained()
	var metadata archive.Metadata
	if err := json.Unmarshal(pending.MetadataBytes, &metadata); err != nil {
		t.Fatal(err)
	}
	input := &pending.History.Inputs[1]
	bundle, err := scan.loadHistoryInput(metadata, *input)
	if err != nil {
		t.Fatal(err)
	}
	bundle.Capture.FilterVersion = "14"
	bundle.NativeRecords[len(bundle.NativeRecords)-1]["api_key"] = "sk-abcdefghijklmnopqrstuv"
	packed, err := archive.BuildCompressedSource(bundle)
	if err != nil {
		t.Fatal(err)
	}
	key, err := archive.SourceObjectKey(bundle, packed.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	old := input.Reference
	input.Reference = archive.SourceReference{Key: key, SHA256: packed.SHA256, CompressedBytes: len(packed.Bytes)}
	input.FilterVersion = "14"
	stage, err := scan.local.StagePendingSource(scan.id(), input.Reference, packed.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	for i := range pending.History.Sources {
		if pending.History.Sources[i].Reference == old {
			pending.History.Sources[i] = stage
		}
	}
	for i := range metadata.History.Preserved {
		if metadata.History.Preserved[i].RevisionID == input.RevisionID {
			metadata.History.Preserved[i].Source = input.Reference
			metadata.History.Preserved[i].FilterVersion = "14"
		}
	}
	pending.MetadataBytes, err = json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	// This is an imported original legacy preparing descriptor, assembled
	// before the protocol2 freeze. Never mutate an existing selecting seal.
	currentBytes, err := scan.readRetainedInputBytes(metadata.SourceBundle)
	if err != nil {
		t.Fatal(err)
	}
	pending = state.PendingPublication{Bundle: pending.Bundle, SourceKey: metadata.SourceBundle.Key, SourceSHA256: metadata.SourceBundle.SHA256, SourceBytes: currentBytes, MetadataKey: pending.MetadataKey, MetadataBytes: pending.MetadataBytes, ReadyAt: pending.ReadyAt, SkillEvidence: pending.SkillEvidence, History: &state.PendingHistory{Version: 1, Preparing: true, Inputs: pending.History.Inputs, FilterVersion: archive.FilterVersion, AdapterVersion: pending.Bundle.Capture.AdapterVersion}}
	// A fresh disposable root represents the legacy producer's original
	// descriptor. The prior fixture's protocol2 crash links are not its proof.
	originalLocal := scan.local
	fresh := newTestStore(t)
	cfg, _, err := config.Load(originalLocal.Home())
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Save(fresh.Home(), cfg); err != nil {
		t.Fatal(err)
	}
	if err := fresh.SaveRegistration(scan.reg); err != nil {
		t.Fatal(err)
	}
	for _, originalInput := range pending.History.Inputs {
		raw, err := originalLocal.ReadPendingSource(scan.id(), state.PendingSource{Reference: originalInput.Reference, Name: originalInput.Reference.SHA256 + ".gz"})
		if err != nil {
			t.Fatal(err)
		}
		stage, err := fresh.StagePendingSource(scan.id(), originalInput.Reference, raw)
		if err != nil {
			t.Fatal(err)
		}
		pending.History.Sources = append(pending.History.Sources, stage)
	}
	scan.local = fresh
	scan.published, err = fresh.LoadPublishedState(scan.id())
	if err != nil {
		t.Fatal(err)
	}
	if err := scan.local.SavePending(scan.id(), pending); err != nil {
		t.Fatal(err)
	}
	if err := scan.sealPending(&pending); err != nil {
		t.Fatal(err)
	}
	if err := scan.advanceHistoryPreparation(&pending); err != nil {
		t.Fatal(err)
	}
	if pending.History.PrivacyCursor != 1 {
		t.Fatal("actual first checkpoint missing", pending.History)
	}
	// Only owning replay may interpret the retained original. Native preview,
	// naming and generic readers keep their evidence fence.
	budget := agentapi.NewNativeReadBudget(128 << 20)
	scoped, closeScope := scan.local.WithReadBudget(t.Context(), budget)
	available := budget.Available()
	if err := scoped.CheckDurableSessionRead(scan.id()); !errors.Is(err, state.ErrDurableStorageRecovery) {
		t.Fatal("generic evidence fence weakened", err)
	}
	if err := scoped.CheckPublicationSessionRead(scan.reg, scan.opts.MachineID, scan.publicationAdmission()); err != nil {
		t.Fatal("owning replay unavailable", err)
	}
	wrong := scan.reg
	wrong.NativeSessionID = "foreign"
	if err := scoped.CheckPublicationSessionRead(wrong, scan.opts.MachineID, scan.publicationAdmission()); !errors.Is(err, state.ErrDurableStorageRecovery) {
		t.Fatal("foreign replay owner accepted", err)
	}
	if err := scoped.CheckPublicationSessionRead(scan.reg, scan.opts.MachineID, "foreign"); !errors.Is(err, state.ErrDurableStorageRecovery) {
		t.Fatal("foreign replay context accepted", err)
	}
	if budget.Available() != available {
		t.Fatal("read-only guard leaked loans")
	}
	closeScope()
	extra := filepath.Join(scan.local.Home(), "publication-evidence", scan.id(), "unknown")
	if err := os.WriteFile(extra, []byte("unknown"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := scan.local.CheckPublicationSessionRead(scan.reg, scan.opts.MachineID, scan.publicationAdmission()); !errors.Is(err, state.ErrDurableStorageRecovery) {
		t.Fatal("extra evidence admitted", err)
	}
	if err := os.Remove(extra); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	canceledLocal, closeCanceled := scan.local.WithReadBudget(canceled, budget)
	if err := canceledLocal.CheckPublicationSessionRead(scan.reg, scan.opts.MachineID, scan.publicationAdmission()); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled owning read accepted", err)
	}
	closeCanceled()
	pressured, closePressure := scan.local.WithReadBudget(t.Context(), agentapi.NewNativeReadBudget(16<<10))
	if err := pressured.CheckPublicationSessionRead(scan.reg, scan.opts.MachineID, scan.publicationAdmission()); !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatal("owning read bypassed ledger", err)
	}
	closePressure()
	pending.History.PreparedAt = time.Time{}
	pending.Attempted = false
	remote := &historyCrashStore{MemoryStore: scan.remote.(*storagetest.MemoryStore)}
	before, newer := persistHistoryLocalRetry(t, scan, &pending)
	stages := filepath.Join(scan.local.Home(), "sessions", scan.id(), "pending-sources")
	entriesBefore, err := os.ReadDir(stages)
	if err != nil {
		t.Fatal(err)
	}
	restore := historyReadOnlyDirectory(t, filepath.Join(scan.local.Home(), "pending"))
	result := runHistoryRetry(t, scan, remote)
	assertHistoryCheckpointRefusal(t, scan, remote, result, before, newer)
	entriesAfter, err := os.ReadDir(stages)
	if err != nil || len(entriesAfter) != len(entriesBefore)+1 {
		t.Fatal("checkpoint failed before preparing its output stage", len(entriesAfter), err)
	}
	// The new output is durable but cannot replace the frozen original until
	// the cursor descriptor commits. Retrying must use these exact safe bytes.
	known := map[string]bool{}
	for _, entry := range entriesBefore {
		known[entry.Name()] = true
	}
	var output []byte
	var outputName string
	for _, entry := range entriesAfter {
		if !known[entry.Name()] {
			outputName = entry.Name()
			output, err = os.ReadFile(filepath.Join(stages, outputName))
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	safe, err := archive.ReadSourceBundle(bytes.NewReader(output), archive.DecodeOptions{})
	if err != nil || !safe.Capture.CapturedAt.Equal(input.CapturedAt) || safe.Capture.FilterVersion != archive.FilterVersion {
		t.Fatal("prepared output changed frozen provenance", err)
	}
	encoded, err := json.Marshal(safe)
	if err != nil || bytes.Contains(encoded, []byte("sk-abcdefghijklmnopqrstuv")) {
		t.Fatal("prepared output retained the old secret", err)
	}
	for _, original := range pending.History.Inputs {
		if _, err := scan.local.ReadPendingSource(scan.id(), state.PendingSource{Reference: original.Reference, Name: original.Reference.SHA256 + ".gz"}); err != nil {
			t.Fatal("checkpoint failure lost a live original", err)
		}
	}
	restore()
	final := convergeHistoryLocalRetry(t, scan, remote)
	var finalMetadata archive.Metadata
	if err := json.Unmarshal(final.MetadataBytes, &finalMetadata); err != nil {
		t.Fatal(err)
	}
	matched := false
	for _, revision := range finalMetadata.History.Preserved {
		if revision.RevisionID == input.RevisionID {
			matched = true
			data, err := remote.Get(t.Context(), revision.Source.Key)
			if err != nil || revision.Source.SHA256+".gz" != outputName || !bytes.Equal(data, output) || !revision.CapturedAt.Equal(input.CapturedAt) {
				t.Fatal("retry changed prepared revision bytes or capture", err)
			}
		}
	}
	if !matched {
		t.Fatal("prepared revision disappeared from the final manifest")
	}
	assertHistoryRequest(t, scan, newer)
}

func TestRunHistoryAttemptedMarkFailurePrecedesEveryUpload(t *testing.T) {
	scan, pending := privacyJournal(t)
	defer scan.releaseRetained()
	pending.Attempted = false
	remote := &historyCrashStore{MemoryStore: scan.remote.(*storagetest.MemoryStore)}
	before, newer := persistHistoryLocalRetry(t, scan, &pending)
	restore := historyReadOnlyDirectory(t, filepath.Join(scan.local.Home(), "pending"))
	result := runHistoryRetry(t, scan, remote)
	assertHistoryCheckpointRefusal(t, scan, remote, result, before, newer)
	if !strings.Contains(result.Errors[scan.id()].Error(), "mark pending publication attempted") {
		t.Fatal("failure did not reach the attempted mark", result.Errors)
	}
	restore()
	final := convergeHistoryLocalRetry(t, scan, remote)
	if !bytes.Equal(final.SourceBytes, pending.SourceBytes) || !bytes.Equal(final.MetadataBytes, pending.MetadataBytes) || !final.Bundle.Capture.CapturedAt.Equal(pending.Bundle.Capture.CapturedAt) {
		t.Fatal("attempted-mark retry changed frozen publication")
	}
	assertHistoryRequest(t, scan, newer)
}

func historyReadOnlyDirectory(t *testing.T, path string) func() {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("filesystem permission obstruction requires a non-root test user")
	}
	if err := os.Chmod(path, 0500); err != nil {
		t.Fatal(err)
	}
	restore := func() {
		if err := os.Chmod(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(restore)
	return restore
}

func persistHistoryLocalRetry(t *testing.T, scan *sessionScan, pending *state.PendingPublication) ([]byte, string) {
	t.Helper()
	if err := scan.local.SaveRequest(scan.id(), "covered", scan.now); err != nil {
		t.Fatal(err)
	}
	covered, _, err := scan.local.LoadRequest(scan.id())
	if err != nil {
		t.Fatal(err)
	}
	pending.RequestToken = covered.Token
	if err := scan.local.SavePending(scan.id(), *pending); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(scan.local.Home(), "pending", scan.id()+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := scan.local.SaveRequest(scan.id(), "newer", scan.now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	newer, _, err := scan.local.LoadRequest(scan.id())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(scan.reg.ProjectRoot); err != nil {
		t.Fatal(err)
	}
	return before, newer.Token
}

func assertHistoryRequest(t *testing.T, scan *sessionScan, token string) {
	t.Helper()
	request, found, err := scan.local.LoadRequest(scan.id())
	if err != nil || !found || request.Token != token {
		t.Fatal("local failure/retry acknowledged newer work", err)
	}
}

func assertHistoryCheckpointRefusal(t *testing.T, scan *sessionScan, remote *historyCrashStore, result Result, before []byte, newer string) {
	t.Helper()
	if !errors.Is(result.Errors[scan.id()], os.ErrPermission) || len(result.Published) != 0 || remote.puts != 0 {
		t.Fatal("local checkpoint refusal uploaded or acknowledged", result)
	}
	after, err := os.ReadFile(filepath.Join(scan.local.Home(), "pending", scan.id()+".json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed checkpoint changed durable descriptor", err)
	}
	published, err := scan.local.LoadPublishedState(scan.id())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, acknowledged := published.LastPublished(); acknowledged {
		t.Fatal("failed checkpoint acknowledged an unpublished source set")
	}
	assertHistoryRequest(t, scan, newer)
}

func convergeHistoryLocalRetry(t *testing.T, scan *sessionScan, remote *historyCrashStore) state.PendingPublication {
	t.Helper()
	var final state.PendingPublication
	for range 8 {
		pending, found, err := scan.local.LoadPublicationPending(scan.id())
		if err != nil || !found {
			t.Fatal("frozen retry descriptor lost", err)
		}
		result := runHistoryRetry(t, scan, remote)
		if len(result.Published) == 1 {
			if len(result.Errors) != 0 {
				t.Fatal("completed retry retained a failure", result.Errors)
			}
			final = pending
			// The final preparation slice may derive new metadata in this pass.
			raw, err := remote.Get(t.Context(), pending.MetadataKey)
			if err != nil {
				t.Fatal(err)
			}
			final.MetadataBytes = raw
			var metadata archive.Metadata
			if err := json.Unmarshal(raw, &metadata); err != nil {
				t.Fatal(err)
			}
			for _, input := range pending.History.Inputs {
				at := metadata.CapturedAt
				if input.RevisionID != metadata.History.CurrentRevision {
					at = time.Time{}
					for _, revision := range metadata.History.Preserved {
						if revision.RevisionID == input.RevisionID {
							at = revision.CapturedAt
						}
					}
				}
				if !at.Equal(input.CapturedAt) {
					t.Fatal("final source set lost frozen capture provenance", input)
				}
			}
			assertCompleteHistory(t, scan, final, remote)
			return final
		}
		if !errors.Is(result.Errors[scan.id()], archive.ErrHistoryMutationPending) {
			t.Fatal("frozen retry did not advance", result)
		}
	}
	t.Fatal("frozen local retry did not converge")
	return final
}

// freshLegacyHistoryOriginal imports a synthetic complete original descriptor
// before its first seal, preserving every verified original stage and age.
func freshLegacyHistoryOriginal(t *testing.T, scan *sessionScan, p state.PendingPublication) state.PendingPublication {
	t.Helper()
	var m archive.Metadata
	if err := json.Unmarshal(p.MetadataBytes, &m); err != nil {
		t.Fatal(err)
	}
	current, err := scan.readRetainedInputBytes(m.SourceBundle)
	if err != nil {
		t.Fatal(err)
	}
	original := scan.local
	fresh := newTestStore(t)
	cfg, _, err := config.Load(original.Home())
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Save(fresh.Home(), cfg); err != nil {
		t.Fatal(err)
	}
	if err := fresh.SaveRegistration(scan.reg); err != nil {
		t.Fatal(err)
	}
	out := state.PendingPublication{Bundle: p.Bundle, SourceKey: m.SourceBundle.Key, SourceSHA256: m.SourceBundle.SHA256, SourceBytes: current, MetadataKey: p.MetadataKey, MetadataBytes: p.MetadataBytes, ReadyAt: p.ReadyAt, SkillEvidence: p.SkillEvidence, History: &state.PendingHistory{Version: 1, Preparing: true, Inputs: p.History.Inputs, FilterVersion: archive.FilterVersion, AdapterVersion: p.Bundle.Capture.AdapterVersion}}
	for _, input := range out.History.Inputs {
		raw, err := original.ReadPendingSource(scan.id(), state.PendingSource{Reference: input.Reference, Name: input.Reference.SHA256 + ".gz"})
		if err != nil {
			t.Fatal(err)
		}
		stage, err := fresh.StagePendingSource(scan.id(), input.Reference, raw)
		if err != nil {
			t.Fatal(err)
		}
		out.History.Sources = append(out.History.Sources, stage)
	}
	scan.local = fresh
	scan.published, err = fresh.LoadPublishedState(scan.id())
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh.SavePending(scan.id(), out); err != nil {
		t.Fatal(err)
	}
	return out
}
