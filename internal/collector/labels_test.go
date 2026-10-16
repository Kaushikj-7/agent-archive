package collector

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/codex"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type mutableLabels struct {
	label archive.SessionLabel
	calls int
}

func (p *mutableLabels) LookupLabelsNative(_ context.Context, _ agentapi.LabelEnvironment, requests []agentapi.LabelRequest) map[string]archive.SessionLabel {
	p.calls++
	out := map[string]archive.SessionLabel{}
	for _, r := range requests {
		if p.label.State != "" {
			l := p.label
			l.NativeID = r.Registration.NativeSessionID
			out[r.Registration.ArchiveSessionID] = l
		}
	}
	return out
}

type mutableLabelProvider struct{ *mutableLabels }

func (p mutableLabelProvider) LookupLabels(ctx context.Context, env agentapi.LabelEnvironment, r []agentapi.LabelRequest) map[string]archive.SessionLabel {
	return p.LookupLabelsNative(ctx, env, r)
}

type mutableLabelLookup struct{ *mutableLabels }

func (p mutableLabelLookup) LookupLabels(string) (agentapi.LabelProvider, bool) {
	return mutableLabelProvider(p), true
}

func TestExternalRenamePublishesRetainedSourceWithoutReadingTranscript(t *testing.T) {
	for _, tc := range []struct {
		name    string
		missing bool
		upgrade bool
	}{{name: "unchanged"}, {name: "rotated", missing: true}, {name: "filter upgrade", upgrade: true}} {
		t.Run(tc.name, func(t *testing.T) {
			local := newTestStore(t)
			path := writeTranscript(t, t.TempDir(), "session.jsonl", codexTranscript)
			reg := registration(t, path)
			reg.NativeSessionID = "01900000-0000-7000-8000-000000000001"
			if err := local.SaveRegistration(reg); err != nil {
				t.Fatal(err)
			}
			filter := &operationFilter{}
			bindings := &operationBindings{parser: &operationParser{version: "0.1.0"}, filter: filter}
			remote := &countedPublications{MemoryStore: storagetest.NewMemoryStore()}
			now := reg.RegisteredAt.Add(time.Hour)
			provider := &mutableLabels{}
			opts := Options{Sources: bindings, Parsers: bindings, Labels: mutableLabelLookup{provider}, MachineID: "machine", Now: func() time.Time { return now }}
			result, err := Run(context.Background(), local, remote, opts)
			if err != nil || len(result.Errors) > 0 {
				t.Fatalf("%+v %v", result, err)
			}
			before := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
			beforeBundle := fetchBundle(t, remote, before)
			if tc.upgrade {
				simulateFilterUpgrade(t, local)
			}
			if tc.missing {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			now = now.Add(time.Hour)
			provider.label = archive.SessionLabel{State: archive.SessionLabelPresent, Name: "Invented native rename", Source: archive.SessionLabelDatabase, Contract: codex.LabelContract}
			filter.calls = 0
			remote.keys = nil
			result, err = Run(context.Background(), local, remote, opts)
			if err != nil || len(result.Errors) > 0 || len(result.Published) != 1 {
				t.Fatalf("%+v %v", result, err)
			}
			if tc.upgrade {
				// The stale-filter fixture intentionally disagrees with its
				// acknowledged sidecar. Ordinary refiltering repairs that state
				// before it may certify a native label lookup.
				repaired := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
				if provider.calls != 0 || repaired.Name != before.Name || repaired.SourceBundle != before.SourceBundle || !repaired.CapturedAt.Equal(before.CapturedAt) {
					t.Fatal("stale-filter state authorized a native label")
				}
				published, err := local.LoadPublishedState(reg.ArchiveSessionID)
				if err != nil {
					t.Fatal(err)
				}
				if _, found, err := published.LastPublishedMetadata(); err != nil || !found {
					t.Fatal("ordinary refilter did not restore acknowledged authority", err)
				}
				now = now.Add(time.Hour)
				result, err = Run(context.Background(), local, remote, opts)
				if err != nil || len(result.Errors) > 0 || len(result.Published) != 1 {
					t.Fatalf("rename after authority repair: %+v %v", result, err)
				}
			}
			after := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
			afterBundle := fetchBundle(t, remote, after)
			encoded, err := archive.BuildCompressedSource(afterBundle)
			if err != nil || encoded.SHA256 != after.SourceBundle.SHA256 || len(encoded.Bytes) != after.SourceBundle.CompressedBytes || after.MachineID != opts.MachineID || after.NativeSessionID != reg.NativeSessionID || after.FilterVersion != archive.FilterVersion || after.SchemaVersion != archive.MetadataSchemaVersion || after.History != nil {
				t.Fatal("rename did not preserve supported ordinary publication authority", err)
			}
			if !tc.upgrade && filter.calls != 0 {
				t.Fatalf("name-only refresh filtered native input %d times", filter.calls)
			}
			if tc.upgrade && filter.calls != 1 {
				t.Fatalf("codec upgrade did not normally refilter native input: %d", filter.calls)
			}
			if after.Name != "Invented native rename" || after.Title != before.Title || after.SourceBundle == before.SourceBundle || !after.CapturedAt.Equal(before.CapturedAt) || !after.StartedAt.Equal(before.StartedAt) || !reflect.DeepEqual(after.EndedAt, before.EndedAt) || !reflect.DeepEqual(before.Counts, after.Counts) || !reflect.DeepEqual(beforeBundle.NativeRecords, afterBundle.NativeRecords) {
				t.Fatalf("rename changed conversation facts: before=%+v after=%+v", before, after)
			}
			now = now.Add(time.Hour)
			provider.label = archive.SessionLabel{}
			remote.keys = nil
			result, err = Run(context.Background(), local, remote, opts)
			if err != nil || len(result.Errors) > 0 || len(remote.keys) != 0 {
				t.Fatalf("unavailable republished: %+v %v %v", result, err, remote.keys)
			}
			if got := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID); got.Name != after.Name {
				t.Fatal("unavailable cleared last good name")
			}
			now = now.Add(time.Hour)
			provider.label = archive.SessionLabel{State: archive.SessionLabelPresent, Name: " \n    <external_codex_apps_open_page>example</external_codex_apps_open_page>", Source: archive.SessionLabelDatabase, Contract: codex.LabelContract}
			result, err = Run(context.Background(), local, remote, opts)
			if err != nil || len(result.Errors) > 0 || len(remote.keys) != 0 {
				t.Fatalf("unstable name republished: %+v %v %v", result, err, remote.keys)
			}
			if got := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID); got.Name != after.Name {
				t.Fatal("unstable name cleared last good name")
			}
		})
	}
}

func TestExternalRenameKeepsAttemptedPendingBytesAcrossNewerName(t *testing.T) {
	local := newTestStore(t)
	path := writeTranscript(t, t.TempDir(), "session.jsonl", codexTranscript)
	reg := registration(t, path)
	reg.NativeSessionID = "01900000-0000-7000-8000-000000000001"
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	remote := &metadataFailStore{MemoryStore: storagetest.NewMemoryStore()}
	now := reg.RegisteredAt.Add(time.Hour)
	provider := &mutableLabels{}
	opts := Options{Sources: testSources, Parsers: testParsers, Labels: mutableLabelLookup{provider}, MachineID: "machine", Now: func() time.Time { return now }}
	if result, err := Run(context.Background(), local, remote, opts); err != nil || len(result.Errors) > 0 {
		t.Fatalf("%+v %v", result, err)
	}
	provider.label = archive.SessionLabel{State: archive.SessionLabelPresent, Name: "First rename", Source: archive.SessionLabelDatabase, Contract: codex.LabelContract}
	now = now.Add(time.Hour)
	remote.failMetadata = true
	result, err := Run(context.Background(), local, remote, opts)
	if err != nil || len(result.Errors) == 0 {
		t.Fatalf("expected durable failure: %+v %v", result, err)
	}
	pending, found, err := local.LoadPending(reg.ArchiveSessionID)
	if err != nil || !found || !pending.Attempted {
		t.Fatalf("%+v %v %v", pending, found, err)
	}
	provider.label.Name = "Second rename"
	now = now.Add(time.Hour)
	remote.failMetadata = false
	result, err = Run(context.Background(), local, remote, opts)
	if err != nil || len(result.Errors) > 0 {
		t.Fatalf("%+v %v", result, err)
	}
	if got := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID); got.Name != "First rename" {
		t.Fatalf("pending bytes replaced: %q", got.Name)
	}
	now = now.Add(time.Hour)
	result, err = Run(context.Background(), local, remote, opts)
	if err != nil || len(result.Errors) > 0 {
		t.Fatalf("%+v %v", result, err)
	}
	if got := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID); got.Name != "Second rename" {
		t.Fatalf("newer fingerprint lost after retry: %q", got.Name)
	}
}

func TestExternalRenameCombinesChangedConversationWithCurrentName(t *testing.T) {
	local := newTestStore(t)
	path := writeTranscript(t, t.TempDir(), "session.jsonl", codexTranscript)
	reg := registration(t, path)
	reg.NativeSessionID = "01900000-0000-7000-8000-000000000001"
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	remote := storagetest.NewMemoryStore()
	now := reg.RegisteredAt.Add(time.Hour)
	provider := &mutableLabels{}
	filter := &operationFilter{}
	bindings := &operationBindings{parser: &operationParser{version: "0.1.0"}, filter: filter}
	opts := Options{Sources: bindings, Parsers: bindings, Labels: mutableLabelLookup{provider}, MachineID: "machine", Now: func() time.Time { return now }}
	if result, err := Run(context.Background(), local, remote, opts); err != nil || len(result.Errors) > 0 {
		t.Fatalf("%+v %v", result, err)
	}
	before := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString("\n{\"type\":\"event_msg\",\"timestamp\":\"2026-09-22T12:00:00Z\",\"payload\":{\"type\":\"user_message\",\"message\":\"Fresh conversation content\"}}\n")
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	provider.label = archive.SessionLabel{State: archive.SessionLabelPresent, Name: "Current rename", Source: archive.SessionLabelDatabase, Contract: codex.LabelContract}
	filter.calls = 0
	now = now.Add(time.Hour)
	if result, err := Run(context.Background(), local, remote, opts); err != nil || len(result.Errors) > 0 {
		t.Fatalf("%+v %v", result, err)
	}
	after := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	if filter.calls != 1 || after.Name != "Current rename" || len(fetchBundle(t, remote, after).NativeRecords) <= len(fetchBundle(t, remote, before).NativeRecords) || !after.CapturedAt.Equal(now) {
		t.Fatalf("changed source replaced by old retained source: filtered=%d before=%+v after=%+v", filter.calls, before, after)
	}
}

func TestLabelCapabilityUsesRegistrationHarnessAndGenericContext(t *testing.T) {
	local := newTestStore(t)
	path := writeTranscript(t, t.TempDir(), "session.jsonl", `{"type":"user","uuid":"message-1","message":{"role":"user","content":"Invented prompt"}}`)
	reg := registration(t, path)
	reg.Harness = archive.Harness{Name: "claude", Version: "2.1.0"}
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	remote := storagetest.NewMemoryStore()
	now := reg.RegisteredAt.Add(time.Hour)
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "machine", Now: func() time.Time { return now }}
	if result, err := Run(context.Background(), local, remote, opts); err != nil || len(result.Errors) > 0 {
		t.Fatalf("%+v %v", result, err)
	}
	provider := &fairLabels{requested: map[string]bool{}}
	opts.Labels = provider
	now = now.Add(time.Hour)
	if result, err := Run(context.Background(), local, remote, opts); err != nil || len(result.Errors) > 0 {
		t.Fatalf("%+v %v", result, err)
	}
	if !provider.requested[reg.ArchiveSessionID] {
		t.Fatal("non-Codex injected label capability was not selected")
	}
	cache, err := local.LoadLabels()
	if err != nil {
		t.Fatal(err)
	}
	if entry, ok := cache.Entries[reg.ArchiveSessionID]; !ok || entry.Context.Producer != "2.1.0" {
		t.Fatalf("generic producer/context not durable: %+v", cache)
	}
}

func TestFileAbsenceCannotClearStrongerAPIName(t *testing.T) {
	previous := archive.SessionLabel{Source: archive.SessionLabelAPI, State: archive.SessionLabelPresent}
	for _, source := range []archive.SessionLabelSource{archive.SessionLabelIndex, archive.SessionLabelDatabase} {
		if !weakerLabelAbsence(archive.SessionLabel{Source: source, State: archive.SessionLabelAbsent}, previous) {
			t.Fatal("file absence can clear API name")
		}
	}
	if weakerLabelAbsence(archive.SessionLabel{Source: archive.SessionLabelAPI, State: archive.SessionLabelAbsent}, previous) {
		t.Fatal("authoritative API absence blocked")
	}
	if weakerLabelAbsence(archive.SessionLabel{Source: archive.SessionLabelDatabase, State: archive.SessionLabelPresent}, previous) {
		t.Fatal("verified present fallback blocked")
	}
}

func TestChangingNamingModeRefreshesLookupWithoutClearingRetainedAPIName(t *testing.T) {
	local := newTestStore(t)
	path := writeTranscript(t, t.TempDir(), "session.jsonl", codexTranscript)
	reg := registration(t, path)
	reg.NativeSessionID = "01900000-0000-7000-8000-000000000001"
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	remote := storagetest.NewMemoryStore()
	now := reg.RegisteredAt.Add(time.Hour)
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "machine", Now: func() time.Time { return now }}
	if result, err := Run(context.Background(), local, remote, opts); err != nil || len(result.Errors) > 0 {
		t.Fatal(result, err)
	}
	now = now.Add(time.Hour)
	provider := &mutableLabels{label: archive.SessionLabel{State: archive.SessionLabelPresent, Name: "Verified API name", Source: archive.SessionLabelAPI, Contract: codex.LabelAPIContract}}
	opts.Labels = mutableLabelLookup{provider}
	opts.LabelEnvironment = agentapi.LabelEnvironment{Mode: agentapi.LabelLookupNative, ProviderContract: codex.LabelAPIContract}
	if result, err := Run(context.Background(), local, remote, opts); err != nil || len(result.Errors) > 0 {
		t.Fatal(result, err)
	}
	before := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	if before.Name != "Verified API name" {
		t.Fatal(before.Name)
	}
	provider.label = archive.SessionLabel{State: archive.SessionLabelAbsent, Source: archive.SessionLabelDatabase, Contract: codex.LabelContract}
	now = now.Add(time.Second)
	opts.LabelEnvironment = agentapi.LabelEnvironment{Mode: agentapi.LabelLookupFiles, ProviderContract: codex.LabelContract}
	if result, err := Run(context.Background(), local, remote, opts); err != nil || len(result.Errors) > 0 || len(result.Published) > 0 {
		t.Fatal(result, err)
	}
	cache, err := local.LoadLabels()
	if err != nil {
		t.Fatal(err)
	}
	entry := cache.Entries[reg.ArchiveSessionID]
	if entry.Label.Source != archive.SessionLabelAPI || entry.Failures != 0 || !entry.NextAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("mode migration did not perform guarded fallback: %+v", entry)
	}
	after := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	if after.Name != before.Name || after.SourceBundle != before.SourceBundle || !after.CapturedAt.Equal(before.CapturedAt) {
		t.Fatal("file fallback erased stronger native evidence")
	}
}

// Reopening the cache keeps its local offset, whereas the normal bundle builder
// filters supplemental timestamps to UTC. A read refusal must not turn that
// representation difference into conversation activity on the next source scan.
func TestCachedLabelRefusalAndRecoveryPreserveSettledPublication(t *testing.T) {
	for _, legacyOffset := range []bool{false, true} {
		t.Run(map[bool]string{false: "new publication", true: "older retained offset"}[legacyOffset], func(t *testing.T) {
			testCachedLabelRefusalAndRecovery(t, legacyOffset)
		})
	}
}

type codexFileLabelLookup struct{}

func (codexFileLabelLookup) LookupLabels(name string) (agentapi.LabelProvider, bool) {
	return codex.LabelProvider{}, name == "codex"
}

func testCachedLabelRefusalAndRecovery(t *testing.T, legacyOffset bool) {
	t.Helper()
	local := newTestStore(t)
	nativeHome := t.TempDir()
	sessions := filepath.Join(nativeHome, "sessions")
	if err := os.Mkdir(sessions, 0700); err != nil {
		t.Fatal(err)
	}
	path := writeTranscript(t, sessions, "session.jsonl", `{"type":"session_meta","payload":{"id":"01900000-0000-7000-8000-000000000001","cli_version":"0.159.2","history_mode":"legacy"}}`+"\n"+codexTranscript)
	reg := registration(t, path)
	reg.NativeSessionID = "01900000-0000-7000-8000-000000000001"
	reg.Harness.Version = "0.159.2"
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	remote := &countedPublications{MemoryStore: storagetest.NewMemoryStore()}
	now := reg.RegisteredAt.Add(time.Hour).In(time.FixedZone("synthetic Pacific", -7*60*60))
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "machine", Now: func() time.Time { return now }, LabelEnvironment: agentapi.LabelEnvironment{Homes: []string{nativeHome}, VerifiedLegacyStorageHomes: []string{nativeHome}}}
	index := filepath.Join(nativeHome, "session_index.jsonl")
	writeIndex := func(name, unfinished string) {
		t.Helper()
		line, err := json.Marshal(map[string]string{"id": reg.NativeSessionID, "thread_name": name, "updated_at": "2026-01-01T00:00:00Z"})
		if err != nil {
			t.Fatal(err)
		}
		line = append(line, '\n')
		data := line
		if unfinished != "" {
			// A record Codex is still appending proves nothing.
			data = append(data, `{"id":"`+reg.NativeSessionID+`","thread_name":"`+unfinished...)
		}
		if err := os.WriteFile(index, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	name := "Selected native name"
	writeIndex(name, "")
	updateName := func(value string) {
		t.Helper()
		writeIndex(value, "")
	}
	run := func(want int) {
		t.Helper()
		remote.keys = nil
		result, err := Run(t.Context(), local, remote, opts)
		if err != nil || len(result.Errors) != 0 || len(result.Published) != want || (want == 0 && len(remote.keys) != 0) {
			t.Fatalf("published=%d want=%d writes=%v result=%+v err=%v", len(result.Published), want, remote.keys, result, err)
		}
	}
	reopen := func() {
		t.Helper()
		var err error
		local, err = state.Open(local.Home())
		if err != nil {
			t.Fatal(err)
		}
	}
	rescan := func() {
		t.Helper()
		// A harmless stat change exercises the normal source path without adding
		// a hook or any new conversation evidence.
		if err := os.Chtimes(path, now, now); err != nil {
			t.Fatal(err)
		}
	}
	run(1)
	opts.Labels = codexFileLabelLookup{}
	now = now.Add(time.Hour)
	run(1)
	before := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	bundle := fetchBundle(t, remote, before)
	if legacyOffset {
		// Seed a self-consistent synthetic publication in the older fast-path
		// format. Its committed bytes, reference and metadata all retain the local
		// offset, so the regression exercises compatibility without corruption.
		for i := range bundle.SupplementalEvidence {
			if bundle.SupplementalEvidence[i].Kind == archive.EvidenceKindSessionLabels {
				bundle.SupplementalEvidence[i].ObservedAt = now
			}
		}
		compressed, err := archive.BuildCompressedSource(bundle)
		if err != nil {
			t.Fatal(err)
		}
		key, err := archive.SourceObjectKey(bundle, compressed.SHA256)
		if err != nil {
			t.Fatal(err)
		}
		before.SourceBundle = archive.SourceReference{Key: key, SHA256: compressed.SHA256, CompressedBytes: len(compressed.Bytes)}
		metadata, err := json.Marshal(before)
		if err != nil {
			t.Fatal(err)
		}
		metadataKey, err := archive.MetadataObjectKey(reg.Harness.Name, reg.ArchiveSessionID)
		if err != nil {
			t.Fatal(err)
		}
		if err := remote.Put(t.Context(), key, compressed.Bytes); err != nil {
			t.Fatal(err)
		}
		if err := remote.Put(t.Context(), metadataKey, metadata); err != nil {
			t.Fatal(err)
		}
		published, err := local.LoadPublishedState(reg.ArchiveSessionID)
		if err != nil {
			t.Fatal(err)
		}
		if err := published.SavePublication(bundle, now, before.SourceBundle, metadata); err != nil {
			t.Fatal(err)
		}
		bundle = fetchBundle(t, remote, before)
	}
	label, observed, ok := archive.CurrentSessionLabel(bundle)
	if !ok || label.Name != name || !observed.Equal(now) {
		t.Fatal("missing selected observation")
	}
	if _, offset := observed.Zone(); offset != map[bool]int{false: 0, true: -7 * 60 * 60}[legacyOffset] {
		t.Fatal("publication did not use expected timestamp representation")
	}
	cache, err := local.LoadLabels()
	if err != nil {
		t.Fatal(err)
	}
	if _, offset := cache.Entries[reg.ArchiveSessionID].ObservedAt.Zone(); offset != -7*60*60 {
		t.Fatal("test did not retain offset in cached observation")
	}
	unchanged := func() {
		t.Helper()
		if after := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID); !reflect.DeepEqual(after, before) {
			t.Fatalf("unchanged observation changed metadata: before=%+v after=%+v", before, after)
		}
		if after := fetchBundle(t, remote, before); !reflect.DeepEqual(after, bundle) {
			t.Fatal("unchanged observation changed retained source")
		}
		cache, err := local.LoadLabels()
		if err != nil {
			t.Fatal(err)
		}
		entry := cache.Entries[reg.ArchiveSessionID]
		if entry.Label != label || !entry.ObservedAt.Equal(observed) {
			t.Fatal("cache lost last good selected observation")
		}
	}
	reopen()
	now = now.Add(time.Hour)
	run(0)
	unchanged()
	writeIndex(name, "Unseen name during refusal")
	now = now.Add(time.Hour)
	rescan()
	run(0)
	unchanged()
	cache, err = local.LoadLabels()
	if err != nil || cache.Entries[reg.ArchiveSessionID].Failures != 1 {
		t.Fatalf("refusal not recorded: %+v %v", cache, err)
	}
	reopen()
	now = now.Add(time.Hour)
	rescan()
	run(0)
	unchanged()
	updateName(name)
	now = now.Add(time.Hour)
	rescan()
	run(0)
	unchanged()
	cache, err = local.LoadLabels()
	if err != nil || cache.Entries[reg.ArchiveSessionID].Failures != 0 {
		t.Fatal("successful recovery did not reset failures", err)
	}
	name = "Genuine changed native name"
	updateName(name)
	now = now.Add(time.Hour)
	run(1)
	after := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	afterBundle := fetchBundle(t, remote, after)
	if after.Name != name || after.SourceBundle == before.SourceBundle || !after.CapturedAt.Equal(before.CapturedAt) || !after.StartedAt.Equal(before.StartedAt) || !reflect.DeepEqual(after.EndedAt, before.EndedAt) || !reflect.DeepEqual(after.Counts, before.Counts) || !reflect.DeepEqual(afterBundle.NativeRecords, bundle.NativeRecords) {
		t.Fatalf("real rename lost or changed activity: before=%+v after=%+v", before, after)
	}
	_, renamedAt, ok := archive.CurrentSessionLabel(afterBundle)
	if !ok || !renamedAt.Equal(now) {
		t.Fatal("real rename did not record its new observation")
	}
}
