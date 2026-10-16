package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type historyCrashStore struct {
	*storagetest.MemoryStore
	calls    int
	fail     int
	after    bool
	puts     int
	afterPut func(string)
}

func (s *historyCrashStore) boundary(key string) bool {
	if !strings.HasPrefix(key, "sessions/") {
		return false
	}
	s.calls++
	return s.fail > 0 && s.calls == s.fail
}

func (s *historyCrashStore) Put(ctx context.Context, key string, raw []byte) error {
	crash := s.boundary(key)
	if crash && !s.after {
		return errors.New("synthetic publication interruption")
	}
	if err := s.MemoryStore.Put(ctx, key, raw); err != nil {
		return err
	}
	if strings.HasPrefix(key, "sessions/") {
		s.puts++
	}
	if s.afterPut != nil {
		s.afterPut(key)
	}
	if crash {
		return errors.New("synthetic interrupted PUT response")
	}
	return nil
}

func (s *historyCrashStore) GetLimited(ctx context.Context, key string, n int64) ([]byte, error) {
	if s.boundary(key) {
		return nil, errors.New("synthetic bounded read interruption")
	}
	return s.MemoryStore.GetLimited(ctx, key, n)
}

func (s *historyCrashStore) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	if s.boundary(key) {
		return storage.ObjectInfo{}, errors.New("synthetic verification interruption")
	}
	return s.MemoryStore.Stat(ctx, key)
}

func runHistoryRetry(t *testing.T, scan *sessionScan, remote storage.ObjectStore) Result {
	t.Helper()
	reopened, err := openTestStore(scan.local.Home())
	if err != nil {
		t.Fatal(err)
	}
	scan.local = reopened
	opts := scan.opts
	opts.RepoKey = func(string) string { t.Fatal("frozen publication read current Git"); return "" }
	opts.SupplementalEvidence = func(archive.SessionRegistration, time.Time) ([]archive.SupplementalEvidence, error) {
		t.Fatal("frozen publication read current inventory")
		return nil, nil
	}
	opts.Retry = storage.RetryPolicy{MaxAttempts: 1}
	result, err := Run(t.Context(), reopened, remote, opts)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func assertCompleteHistory(t *testing.T, scan *sessionScan, p state.PendingPublication, remote storage.ObjectStore) {
	t.Helper()
	raw, err := remote.Get(t.Context(), p.MetadataKey)
	if err != nil || !bytes.Equal(raw, p.MetadataBytes) {
		t.Fatal("final bytes changed", err)
	}
	var m archive.Metadata
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	refs, err := m.SourceReferences()
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range refs {
		raw, err := remote.Get(t.Context(), ref.Key)
		if err != nil {
			t.Fatal("dangling final metadata", err)
		}
		if _, err := decodeHistoryStage(t.Context(), m, p, ref, raw); err != nil {
			t.Fatal(err)
		}
	}
	if _, found, err := scan.local.LoadPending(scan.id()); err != nil || found {
		t.Fatal("journal not cleaned", err)
	}
}

func TestRunHistoryPublicationReopensAtEveryRemoteBoundary(t *testing.T) {
	baseline, p := privacyJournal(t)
	cloud := &historyCrashStore{MemoryStore: baseline.remote.(*storagetest.MemoryStore)}
	result := runHistoryRetry(t, baseline, cloud)
	if len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatal(result)
	}
	assertCompleteHistory(t, baseline, p, cloud)
	boundaries := cloud.calls
	for _, after := range []bool{false, true} {
		for fail := 1; fail <= boundaries; fail++ {
			t.Run(fmt.Sprintf("%d/after=%v", fail, after), func(t *testing.T) {
				scan, p := privacyJournal(t)
				if err := scan.local.SaveRequest(scan.id(), "newer", scan.now); err != nil {
					t.Fatal(err)
				}
				newer, _, err := scan.local.LoadRequest(scan.id())
				if err != nil {
					t.Fatal(err)
				}
				// Native input is gone before any final remote operation.
				entries, err := os.ReadDir(scan.reg.ProjectRoot)
				if err != nil {
					t.Fatal(err)
				}
				for _, entry := range entries {
					if strings.HasSuffix(entry.Name(), ".jsonl") {
						if err := os.Remove(filepath.Join(scan.reg.ProjectRoot, entry.Name())); err != nil {
							t.Fatal(err)
						}
					}
				}
				remote := &historyCrashStore{MemoryStore: scan.remote.(*storagetest.MemoryStore), fail: fail, after: after}
				first := runHistoryRetry(t, scan, remote)
				if len(first.Errors) == 0 {
					t.Fatal("fail point did not stop publication", fail, first)
				}
				if _, found, err := scan.local.LoadPending(scan.id()); err != nil || !found {
					// Auxiliary listing failure retains its own durable repair;
					// exact source/metadata acknowledgement may already be complete.
					if err != nil || len(first.Published) != 1 || !strings.Contains(first.Errors[scan.id()].Error(), "listing maintenance pending") {
						t.Fatal("lost durable retry", err, first.Errors)
					}
					assertCompleteHistory(t, scan, p, remote)
					repairs, err := scan.local.ListingRepairs(32)
					if err != nil || len(repairs) != 1 {
						t.Fatal("listing retry lost", err)
					}
					remote.fail = 0
					pass := &pass{ctx: t.Context(), local: scan.local, remote: remote, result: Result{Errors: map[string]error{}}}
					pass.repairListingIndex()
					if len(pass.result.Errors) != 0 {
						t.Fatal(pass.result.Errors)
					}
					return
				}
				// If a final sidecar is visible at interruption, every ref must exist.
				if raw, err := remote.Get(t.Context(), p.MetadataKey); err == nil {
					var m archive.Metadata
					if err := json.Unmarshal(raw, &m); err != nil {
						t.Fatal(err)
					}
					refs, err := m.SourceReferences()
					if err != nil {
						t.Fatal(err)
					}
					for _, ref := range refs {
						if raw, err := remote.Get(t.Context(), ref.Key); err != nil || !storage.VerifySHA256(raw, ref.SHA256) {
							t.Fatal("dangling metadata", err)
						}
					}
				}
				committedRaw, _ := remote.Get(t.Context(), p.MetadataKey)
				committed := bytes.Equal(committedRaw, p.MetadataBytes)
				puts := remote.puts
				remote.fail = 0
				final := runHistoryRetry(t, scan, remote)
				if len(final.Errors) != 0 || len(final.Published) != 1 {
					t.Fatal("retry failed", final)
				}
				if committed && remote.puts != puts {
					t.Fatal("committed retry performed a needless PUT")
				}
				assertCompleteHistory(t, scan, p, remote)
				request, found, err := scan.local.LoadRequest(scan.id())
				if err != nil || !found || request.Token != newer.Token {
					t.Fatal("newer token lost", err)
				}
			})
		}
	}
}

func TestRunCommittedHistoryRetryDoesNotRequireRemovedPrivateStages(t *testing.T) {
	scan, p := privacyJournal(t)
	cloud := scan.remote.(*storagetest.MemoryStore)
	endAttempt, err := scan.beginPublicationAttempt()
	if err != nil {
		t.Fatal(err)
	}
	defer endAttempt()
	if _, err := scan.sourceSetUpload(p); err != nil {
		t.Fatal(err)
	}
	for _, stage := range p.History.Sources {
		if err := os.Remove(filepath.Join(scan.local.Home(), "sessions", scan.id(), "pending-sources", stage.Name)); err != nil {
			t.Fatal(err)
		}
	}
	remote := &historyCrashStore{MemoryStore: cloud}
	result := runHistoryRetry(t, scan, remote)
	if len(result.Errors) != 0 || len(result.Published) != 1 || remote.puts != 0 {
		t.Fatal(result, remote.puts)
	}
	assertCompleteHistory(t, scan, p, remote)
}

func TestRunCommittedHistoryCleanupFailuresRetainRetry(t *testing.T) {
	type pointVariant0 string
	const (
		pointRetiredLedger0 pointVariant0 = "retired-ledger"
		pointCache0         pointVariant0 = "cache"
		pointRequest0       pointVariant0 = "request"
		pointPrivateStages0 pointVariant0 = "private-stages"
		pointSignature0     pointVariant0 = "native-signature"
	)
	for _, point := range []pointVariant0{pointRetiredLedger0, pointCache0, pointRequest0, pointPrivateStages0, pointSignature0} {
		t.Run(string(point), func(t *testing.T) {
			scan, p := privacyJournal(t)
			// The retired obligation comes from an actual typed privacy
			// successor of an exact acknowledged historical selecting manifest.
			older := p.Bundle
			older.Capture.FilterVersion = "14"
			for _, record := range older.NativeRecords {
				record["api_key"] = "sk-abcdefghijklmnopqrstuv"
			}
			packed, err := archive.BuildCompressedSource(older)
			if err != nil {
				t.Fatal(err)
			}
			key, err := archive.SourceObjectKey(older, packed.SHA256)
			if err != nil {
				t.Fatal(err)
			}
			retired := archive.SourceReference{Key: key, SHA256: packed.SHA256, CompressedBytes: len(packed.Bytes)}
			var previous archive.Metadata
			if err := json.Unmarshal(p.MetadataBytes, &previous); err != nil {
				t.Fatal(err)
			}
			previous.SourceBundle, previous.FilterVersion = retired, "14"
			previous.Title = "sk-abcdefghijklmnopqrstuv"
			previousBody, err := json.Marshal(previous)
			if err != nil {
				t.Fatal(err)
			}
			if err := scan.remote.Put(t.Context(), key, packed.Bytes); err != nil {
				t.Fatal(err)
			}
			if err := scan.remote.Put(t.Context(), p.MetadataKey, previousBody); err != nil {
				t.Fatal(err)
			}
			// Acknowledged selection requires all physical sources, including
			// the preserved inputs previously held only in this private fixture.
			for _, input := range p.History.Inputs {
				if input.RevisionID == previous.History.CurrentRevision {
					continue
				}
				raw, err := scan.readRetainedInputBytes(input.Reference)
				if err != nil {
					t.Fatal(err)
				}
				if err := scan.remote.Put(t.Context(), input.Reference.Key, raw); err != nil {
					t.Fatal(err)
				}
			}
			if err := scan.published.SavePublication(older, scan.now, retired, previousBody); err != nil {
				t.Fatal(err)
			}
			if err := scan.local.RemovePending(scan.id()); err != nil {
				t.Fatal(err)
			}
			p, err = scan.freezeRetainedMaintenance(previous, older, older.Capture.AdapterVersion)
			if err != nil {
				t.Fatal(err)
			}
			for p.History.Preparing {
				if err := scan.advanceHistoryPreparation(&p); err != nil {
					t.Fatal(err)
				}
			}
			if len(p.History.Retired) != 1 || p.History.Retired[0].Reference != retired || !p.History.Retired[0].PrivacySensitive {
				t.Fatal("actual privacy retirement proof missing", p.History.Retired)
			}
			if err := scan.local.SaveRequest(scan.id(), "stop", scan.now); err != nil {
				t.Fatal(err)
			}
			req, _, err := scan.local.LoadRequest(scan.id())
			if err != nil {
				t.Fatal(err)
			}
			p.RequestToken = req.Token
			if point == pointSignature0 {
				p.ScanSignature = &state.ScanSignature{SourceSetVersion: 2, ParserVersion: scan.parserVersion(), FilterVersion: p.Bundle.Capture.FilterVersion, AdapterVersion: p.Bundle.Capture.AdapterVersion}
			}
			if err := scan.local.SavePending(scan.id(), p); err != nil {
				t.Fatal(err)
			}
			cloud := scan.remote.(*storagetest.MemoryStore)
			if err := cloud.Put(t.Context(), key, packed.Bytes); err != nil {
				t.Fatal(err)
			}
			path := ""
			remote := &historyCrashStore{MemoryStore: cloud}
			remote.afterPut = func(key string) {
				if key != p.MetadataKey {
					return
				}
				remote.afterPut = nil
				switch point {
				case pointRetiredLedger0:
					path = filepath.Join(scan.local.Home(), "superseded", scan.id()+".json")
				case pointCache0:
					path = publishedPath(scan.local, scan.id())
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				case pointRequest0:
					path = filepath.Join(scan.local.Home(), "requests", scan.id()+".json")
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				case pointSignature0:
					path = filepath.Join(scan.local.Home(), "scan-signatures", scan.id()+".json")
				case pointPrivateStages0:
					path = filepath.Join(scan.local.Home(), "sessions", scan.id(), "pending-sources", "unsafe-obligation")
				}
				if err := os.MkdirAll(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			result := runHistoryRetry(t, scan, remote)
			if len(result.Errors) == 0 || len(result.Published) != 0 {
				t.Fatal("cleanup failure falsely completed", result)
			}
			if point == pointCache0 {
				if _, err := os.Stat(filepath.Join(scan.local.Home(), "pending", scan.id()+".json")); err != nil {
					t.Fatal("cleanup retry bytes lost", err)
				}
			} else if _, found, err := scan.local.LoadPublicationPending(scan.id()); err != nil || !found {
				t.Fatal("cleanup retry lost", err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if point == pointRequest0 {
				if err := scan.local.SaveRequest(scan.id(), "newer", scan.now.Add(time.Minute)); err != nil {
					t.Fatal(err)
				}
			}
			puts := remote.puts
			final := runHistoryRetry(t, scan, remote)
			if len(final.Errors) != 0 || len(final.Published) != 1 || remote.puts != puts {
				t.Fatal("committed cleanup retry", final, remote.puts, puts)
			}
			assertCompleteHistory(t, scan, p, remote)
			assertPrivateTreeHasNoSecret(t, scan.local.Home(), "sk-abcdefghijklmnopqrstuv")
			ledger, err := scan.local.LoadSuperseded(scan.id())
			if err != nil || len(ledger) != 1 || ledger[0].Key != retired.Key || !ledger[0].PrivacySensitive || !ledger[0].SupersededAt.Equal(scan.now) {
				t.Fatal("retirement lost", ledger, err)
			}
			if point == pointRequest0 {
				if _, found, err := scan.local.LoadRequest(scan.id()); err != nil || !found {
					t.Fatal("newer request lost", err)
				}
			}
		})
	}
}

func TestRunHistorySettlesWithoutPublishedDecodesOrSessionWrites(t *testing.T) {
	scan, _ := reconciliationFixture(t)
	opts := scan.opts
	opts.RepoKey = func(string) string { return "" }
	for range 5 {
		result, err := Run(t.Context(), scan.local, scan.remote, opts)
		if err != nil {
			t.Fatal(err)
		}
		for _, err := range result.Errors {
			if !errors.Is(err, archive.ErrHistoryMutationPending) {
				t.Fatal(result.Errors)
			}
		}
	}
	if _, found, err := scan.local.LoadScanSignature(scan.id()); err != nil || !found {
		t.Fatal("complete history did not settle", err)
	}
	result, cost := measurePass(t, scan.local, scan.remote, opts)
	if len(result.Errors) != 0 || len(result.Published) != 0 || cost.loads != 0 || cost.writes != 0 {
		t.Fatal("settled history work", result, cost)
	}
}

func TestRunHistoryRecoveryFrozenParserAndHookMaintenanceUseRetainedSet(t *testing.T) {
	scan, p := privacyJournal(t)
	if result := runHistoryRetry(t, scan, scan.remote); len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatal(result)
	}
	published, err := scan.local.LoadPublishedState(scan.id())
	if err != nil {
		t.Fatal(err)
	}
	if err := published.SaveBlocked(p.Bundle, scan.now, state.BlockedReasonTranscriptRewritten); err != nil {
		t.Fatal(err)
	}
	opts := scan.opts
	opts.RepoKey = func(string) string { return "" }
	build, closePreview, err := PrepareGenerationRecovery(t.Context(), scan.reg, scan.now.Add(time.Hour), opts)
	defer closePreview()
	if err != nil {
		t.Fatal(err)
	}
	successor, err := scan.local.BeginGenerationRecovery(scan.id(), scan.now.Add(time.Hour), build)
	if err != nil {
		t.Fatal(err)
	}
	pending, found, err := scan.local.LoadPending(successor)
	if err != nil || !found || pending.History == nil || pending.Bundle.History == nil {
		t.Fatal("history recovery not journaled", err)
	}
	// The frozen original has all three independent references; successor owns
	// one current self-contained snapshot under its own generation prefix.
	if err := os.RemoveAll(scan.reg.ProjectRoot); err != nil {
		t.Fatal(err)
	}
	frozen, found, err := scan.local.LoadRegistration(scan.id())
	if err != nil || !found || !frozen.CaptureFrozen {
		t.Fatal("original not frozen", err)
	}
	opts.AcceptSession = func(reg archive.SessionRegistration) bool { return reg.ArchiveSessionID == scan.id() }
	opts.ParserVersion = "synthetic-frozen-parser-maintenance"
	opts.CodexRollouts = nil
	opts.RepoKey = func(string) string { t.Fatal("frozen source3 read current Git"); return "" }
	opts.SupplementalEvidence = func(archive.SessionRegistration, time.Time) ([]archive.SupplementalEvidence, error) {
		t.Fatal("frozen source3 read current inventory")
		return nil, nil
	}
	evidence := archive.SupplementalEvidence{Kind: archive.EvidenceKindExplicitFeedback, Provenance: "synthetic-hook", ObservedAt: scan.now.Add(time.Hour), Payload: map[string]any{"text": "synthetic retained feedback"}}
	if err := scan.local.SaveRequest(scan.id(), "feedback", scan.now.Add(time.Hour), evidence); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		scan.local, err = openTestStore(scan.local.Home())
		if err != nil {
			t.Fatal(err)
		}
		result, err := Run(t.Context(), scan.local, scan.remote, opts)
		if err != nil {
			t.Fatal(err)
		}
		for _, err := range result.Errors {
			if !errors.Is(err, archive.ErrHistoryMutationPending) {
				t.Fatal(result.Errors)
			}
		}
	}
	raw, err := scan.remote.Get(t.Context(), p.MetadataKey)
	if err != nil {
		t.Fatal(err)
	}
	var m archive.Metadata
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.Parser.Version != opts.ParserVersion || len(m.History.Preserved) != 2 || !m.CapturedAt.Equal(p.Bundle.Capture.CapturedAt) {
		t.Fatal("frozen manifest/capture lost", m.History)
	}
	data, err := scan.remote.Get(t.Context(), m.SourceBundle.Key)
	if err != nil {
		t.Fatal(err)
	}
	b, err := reader.DecodeReferencedSource(t.Context(), m, data, reader.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("feedback")) && len(b.SupplementalEvidence) == 0 {
		t.Fatal("owed observation lost")
	}
	if _, found, err := scan.local.LoadRequest(scan.id()); err != nil || found {
		t.Fatal("frozen observation not covered", err)
	}
	result, cost := measurePass(t, scan.local, scan.remote, opts)
	if len(result.Errors) != 0 || cost.loads != 0 || cost.writes != 0 {
		t.Fatal("settled frozen history work", result, cost)
	}
}

func TestRunMixedHistoryPrivacyRemovesSecretsFromEveryAlternative(t *testing.T) {
	scan, p := privacyJournal(t)
	var m archive.Metadata
	if err := json.Unmarshal(p.MetadataBytes, &m); err != nil {
		t.Fatal(err)
	}
	const secret = "sk-abcdefghijklmnopqrstuv"
	for i, input := range p.History.Inputs {
		b, err := scan.loadHistoryInput(m, input)
		if err != nil {
			t.Fatal(err)
		}
		b.Capture.FilterVersion = "14"
		if i == 1 {
			b.SchemaVersion = archive.SourceSchemaVersion
			b.History = nil
			b.Ordinals = nil
		}
		for _, record := range b.NativeRecords {
			record["api_key"] = secret
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
			m.SourceBundle = ref
			m.FilterVersion = "14"
			p.Bundle = b
		} else {
			for n := range m.History.Preserved {
				r := &m.History.Preserved[n]
				if r.RevisionID == input.RevisionID {
					r.Source = ref
					r.SourceSchemaVersion = b.SchemaVersion
					r.FilterVersion = "14"
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
	if err := os.RemoveAll(scan.reg.ProjectRoot); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		result := runHistoryRetry(t, scan, scan.remote)
		for _, err := range result.Errors {
			if !errors.Is(err, archive.ErrHistoryMutationPending) {
				t.Fatal(result.Errors)
			}
		}
	}
	final, err := scan.remote.Get(t.Context(), p.MetadataKey)
	if err != nil {
		t.Fatal(err)
	}
	var clean archive.Metadata
	if err := json.Unmarshal(final, &clean); err != nil {
		t.Fatal(err)
	}
	refs, err := clean.SourceReferences()
	if err != nil || len(refs) != 3 {
		t.Fatal(refs, err)
	}
	formats := map[int]bool{}
	for _, ref := range refs {
		raw, err := scan.remote.Get(t.Context(), ref.Key)
		if err != nil {
			t.Fatal(err)
		}
		b, err := decodeHistoryStage(t.Context(), clean, p, ref, raw)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(encoded, []byte(secret)) || bytes.Contains(encoded, []byte("api_key")) {
			t.Fatal("complete privacy claim retained an alternative secret")
		}
		if !b.Capture.CapturedAt.Equal(scan.now) || b.Capture.FilterVersion != archive.FilterVersion {
			t.Fatal("capture changed or incomplete privacy")
		}
		formats[b.SchemaVersion] = true
		if err := b.ValidateHistory(); err != nil {
			t.Fatal("raw ordinal graph invalid", err)
		}
	}
	if !formats[archive.SourceSchemaVersion] || !formats[archive.HistorySourceSchemaVersion] {
		t.Fatal("mixed formats were flattened")
	}
}

func TestRunReactivationProtectsEveryReferenceAndPreservesMeaningfulCapture(t *testing.T) {
	scan, lookup := reconciliationFixture(t)
	opts := scan.opts
	opts.RepoKey = func(string) string { return "" }
	for range 3 {
		result, err := Run(t.Context(), scan.local, scan.remote, opts)
		if err != nil {
			t.Fatal(err)
		}
		for _, err := range result.Errors {
			if !errors.Is(err, archive.ErrHistoryMutationPending) {
				t.Fatal(result.Errors)
			}
		}
	}
	m := fetchMetadata(t, scan.remote, "codex", scan.id())
	var retained archive.RevisionReference
	for _, revision := range m.History.Preserved {
		if revision.RevisionID == revisionB {
			retained = revision
		}
	}
	if retained.RevisionID == "" {
		t.Fatal("B not retained")
	}
	current := lookup.refs[revisionB][0]
	lookup.set.Current = &current
	lookup.set.Revision = "reactivated-B"
	opts.Now = func() time.Time { return scan.now.Add(time.Hour) }
	for range 3 {
		result, err := Run(t.Context(), scan.local, scan.remote, opts)
		if err != nil {
			t.Fatal(err)
		}
		for _, err := range result.Errors {
			if !errors.Is(err, archive.ErrHistoryMutationPending) {
				t.Fatal(result.Errors)
			}
		}
	}
	final := fetchMetadata(t, scan.remote, "codex", scan.id())
	if final.History.CurrentRevision != revisionB || final.SourceBundle != retained.Source || !final.CapturedAt.Equal(retained.CapturedAt) {
		t.Fatal("reactivation invented capture or changed exact bytes", final.SourceBundle, final.CapturedAt)
	}
	refs, err := final.SourceReferences()
	if err != nil {
		t.Fatal(err)
	}
	protected := map[string]bool{}
	for _, ref := range refs {
		protected[ref.Key] = true
	}
	ledger, err := scan.local.LoadSuperseded(scan.id())
	if err != nil {
		t.Fatal(err)
	}
	for _, retired := range ledger {
		if protected[retired.Key] {
			t.Fatal("reactivated reference retired")
		}
	}
}

func TestRunNativeHistoryPublicationSettlesAcrossResumeAndRestart(t *testing.T) {
	scan, lookup := reconciliationFixture(t)
	nativeHome := t.TempDir()
	nativeDir := filepath.Join(nativeHome, "sessions")
	if err := os.MkdirAll(nativeDir, 0700); err != nil {
		t.Fatal(err)
	}
	for id, refs := range lookup.refs {
		for i := range refs {
			next := filepath.Join(nativeDir, filepath.Base(refs[i].Path))
			if err := os.Rename(refs[i].Path, next); err != nil {
				t.Fatal(err)
			}
			refs[i].Path = next
		}
		lookup.refs[id] = refs
	}
	scan.reg.TranscriptPath = lookup.refs[revisionThread][0].Path
	scan.reg.CodexBinding.Home = nativeHome
	scan.reg.CodexBinding.Path = scan.reg.TranscriptPath
	if err := scan.local.SaveRegistration(scan.reg); err != nil {
		t.Fatal(err)
	}
	scan.opts.ConfiguredCodexHomes = []string{nativeHome}
	active := lookup.refs[revisionC][0]
	lookup.set.Current = &active
	lookup.set.Candidates = []agentapi.SourceRef{active}
	opts := scan.opts
	var first Result
	var err error
	for range 3 {
		first, err = Run(t.Context(), scan.local, scan.remote, opts)
		if err != nil {
			t.Fatal(err)
		}
		for _, issue := range first.Errors {
			if !errors.Is(issue, archive.ErrHistoryMutationPending) {
				t.Fatal(issue)
			}
		}
		if len(first.Published) == 1 {
			break
		}
	}
	if len(first.Published) != 1 {
		t.Fatalf("history failed to converge: %v", first.Errors)
	}
	signature, found, err := scan.local.LoadScanSignature(scan.id())
	if err != nil || !found || signature.SourceSchemaVersion != archive.HistorySourceSchemaVersion {
		t.Fatalf("missing immediate settled history proof %#v %t %v", signature, found, err)
	}
	reopened, err := openTestStore(scan.local.Home())
	if err != nil {
		t.Fatal(err)
	}
	next, cost := measurePass(t, reopened, scan.remote, opts)
	if len(next.Errors) != 0 || len(next.Published) != 0 || len(next.Skipped) != 1 || cost.loads != 0 || cost.writes != 0 {
		t.Fatalf("unchanged restart decoded or rewrote state %#v %#v", next, cost)
	}
}

// Physical rollouts keep their own producer observations across upgrades.
func TestRunHistoryPreservesIndependentProducerVersions(t *testing.T) {
	scan, lookup := reconciliationFixture(t)
	defer scan.releaseRetained()
	for id, version := range map[string]string{revisionThread: "0.150.0", revisionB: "0.155.0"} {
		path := lookup.refs[id][0].Path
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		raw = bytes.ReplaceAll(raw, []byte("0.160.0"), []byte(version))
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := scan.local.SaveRequest(scan.id(), "stop", scan.now); err != nil {
		t.Fatal(err)
	}
	published := false
	for range 8 {
		reopened, err := openTestStore(scan.local.Home())
		if err != nil {
			t.Fatal(err)
		}
		scan.local = reopened
		result, err := Run(t.Context(), reopened, scan.remote, scan.opts)
		if err != nil {
			t.Fatal(err)
		}
		for _, issue := range result.Errors {
			if !errors.Is(issue, archive.ErrHistoryMutationPending) {
				t.Fatal(issue)
			}
		}
		if len(result.Published) > 0 {
			published = true
			break
		}
	}
	if !published {
		t.Fatal("upgraded history publication did not converge")
	}
	metadata := fetchMetadata(t, scan.remote, "codex", scan.id())
	if metadata.Harness.Version != "0.160.0" {
		t.Fatal("active producer version lost")
	}
	for id, version := range map[string]string{revisionThread: "0.150.0", revisionB: "0.155.0"} {
		bundle, err := reader.LoadRevision(t.Context(), scan.remote, metadata, id, reader.Limits{})
		if err != nil || bundle.Capture.Harness.Version != version {
			t.Fatalf("preserved %s producer: %+v %v", id, bundle.Capture.Harness, err)
		}
	}
	// Retained parser maintenance must also read each producer independently,
	// preserve every capture time and converge after the native files disappear.
	if err := os.RemoveAll(scan.reg.ProjectRoot); err != nil {
		t.Fatal(err)
	}
	scan.opts.ParserVersion = "synthetic-producer-upgrade-maintenance"
	maintained := false
	for range 8 {
		result := runHistoryRetry(t, scan, scan.remote)
		for _, issue := range result.Errors {
			if !errors.Is(issue, archive.ErrHistoryMutationPending) {
				t.Fatal(issue)
			}
		}
		if len(result.Published) > 0 {
			maintained = true
			break
		}
	}
	if !maintained {
		t.Fatal("upgraded retained maintenance did not converge")
	}
	final := fetchMetadata(t, scan.remote, "codex", scan.id())
	if !final.CapturedAt.Equal(metadata.CapturedAt) {
		t.Fatal("maintenance changed active age")
	}
	for i, revision := range final.History.Preserved {
		if !revision.CapturedAt.Equal(metadata.History.Preserved[i].CapturedAt) {
			t.Fatal("maintenance changed preserved age")
		}
		bundle, err := reader.LoadRevision(t.Context(), scan.remote, final, revision.RevisionID, reader.Limits{})
		if err != nil || bundle.Capture.Harness.Version != map[string]string{revisionThread: "0.150.0", revisionB: "0.155.0"}[revision.RevisionID] {
			t.Fatal("maintenance lost producer", err)
		}
	}

}

func (s *historyCrashStore) GetLimitedVersioned(ctx context.Context, key string, limit int64) ([]byte, string, error) {
	if s.boundary(key) {
		return nil, "", errors.New("synthetic bounded read interruption")
	}
	return s.MemoryStore.GetLimitedVersioned(ctx, key, limit)
}
