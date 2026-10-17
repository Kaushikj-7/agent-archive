package collector

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agents/codex"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// A provider that derives its context from the retained publication (one
// without agentapi.RegistrationLabelContextProvider) is still not asked about
// a history source set, and no name it gave earlier is applied to one.
func TestRetainedContextLabelsExcludeHistorySourceSets(t *testing.T) {
	for _, warm := range []bool{false, true} {
		t.Run(map[bool]string{false: "cold", true: "cached debt during backoff"}[warm], func(t *testing.T) {
			local := newTestStore(t)
			reg := registration(t, writeTranscript(t, t.TempDir(), "session.jsonl", codexTranscript))
			reg.NativeSessionID = "01900000-0000-7000-8000-000000000001"
			if err := local.SaveRegistration(reg); err != nil {
				t.Fatal(err)
			}
			remote := storagetest.NewMemoryStore()
			now := reg.RegisteredAt.Add(time.Hour)
			provider := &mutableLabels{label: archive.SessionLabel{State: archive.SessionLabelPresent, Name: "Retained ordinary name", Source: archive.SessionLabelDatabase, Contract: codex.LabelContract}}
			opts := Options{Sources: testSources, Parsers: testParsers, Labels: mutableLabelLookup{provider}, MachineID: "machine", Now: func() time.Time { return now }}
			if result, err := Run(t.Context(), local, remote, opts); err != nil || len(result.Errors) != 0 {
				t.Fatal(result, err)
			}
			p := &pass{ctx: t.Context(), unreadable: map[string]bool{}, local: local, opts: opts, registrations: []archive.SessionRegistration{reg}, now: now.Add(time.Hour), result: Result{Errors: map[string]error{}}}
			closeSources := openCursorPass(p.registrations, &p.opts)
			defer func() {
				if err := closeSources(); err != nil {
					t.Error(err)
				}
			}()
			defer p.releaseLabelResources()
			if warm {
				p.observeLabels(t.Context())
			}
			published, err := local.LoadPublishedState(reg.ArchiveSessionID)
			if err != nil {
				t.Fatal(err)
			}
			metadata := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
			metadata.SchemaVersion = archive.HistoryMetadataSchemaVersion
			metadata.History = &archive.RevisionHistory{CurrentRevision: reg.NativeSessionID}
			if _, err := metadata.SourceReferences(); err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(metadata)
			if err != nil {
				t.Fatal(err)
			}
			injectPublishedMetadata(t, local, reg.ArchiveSessionID, published.Metadata(), encoded)
			calls := provider.calls
			p.observeLabels(t.Context())
			cache, err := local.LoadLabels()
			if err != nil {
				t.Fatal(err)
			}
			if provider.calls != calls || len(p.opts.labels) != 0 {
				t.Fatalf("history authority admitted external naming: calls %d/%d labels %v cache %v", calls, provider.calls, p.opts.labels, cache.Entries)
			}
		})
	}
}

// A thread that continues into a later rollout becomes a history source set,
// and keeps the name it had: the label is retained in the history source and
// becomes the published session's name.
func TestSessionNameCarriesIntoHistorySourceSet(t *testing.T) {
	scan, _ := reconciliationFixture(t)
	defer scan.releaseRetained()
	scan.opts.labels = map[string]state.LabelEntry{scan.id(): {Label: archive.SessionLabel{NativeID: revisionThread, State: archive.SessionLabelPresent, Name: "Native thread name", Source: archive.SessionLabelIndex, Contract: codex.LabelContract}, ObservedAt: scan.now}}
	read, ok, err := scan.read()
	if err != nil || !ok {
		t.Fatal("history read failed", ok, err)
	}
	candidate, _, err := scan.build(read)
	if err != nil || candidate.History == nil {
		t.Fatal("label blocked supported history candidate", err)
	}
	if label, _, ok := archive.CurrentSessionLabel(candidate); !ok || label.Name != "Native thread name" {
		t.Fatalf("history candidate lost the name: %+v %v", label, ok)
	}
	for pass := range 3 {
		result, err := Run(t.Context(), scan.local, scan.remote, scan.opts)
		if err != nil {
			t.Fatal(err)
		}
		if pass == 2 && (len(result.Errors) != 0 || len(result.Published) != 1) {
			t.Fatal("history publication did not complete", result)
		}
	}
	metadata := fetchMetadata(t, scan.remote, "codex", scan.id())
	bundle := fetchBundle(t, scan.remote, metadata)
	if metadata.History == nil || len(metadata.History.Preserved) != 2 || bundle.History == nil {
		t.Fatal("history source set was flattened", metadata.History)
	}
	if label, _, ok := archive.CurrentSessionLabel(bundle); !ok || label.Name != "Native thread name" {
		t.Fatalf("published history source lost the name: %+v %v", label, ok)
	}
	if metadata.Name != "Native thread name" || archive.DisplayTitle(metadata) != "Native thread name" {
		t.Fatalf("name %q, display title %q", metadata.Name, archive.DisplayTitle(metadata))
	}
}
