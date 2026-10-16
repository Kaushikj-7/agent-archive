package collector

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type museCountingFilter struct {
	agentapi.TranscriptFilter
	calls *int
}

func (f museCountingFilter) Filter(ctx context.Context, in agentapi.NativeInput, c agentapi.FilterContext) (archive.FilteredTranscript, error) {
	*f.calls++
	return f.TranscriptFilter.Filter(ctx, in, c)
}

type museCountingSources struct {
	agentapi.SourcesLookup
	calls *int
}

func (s museCountingSources) LookupSources(name string) (agentapi.SourceProvider, agentapi.TranscriptFilter, bool) {
	p, f, ok := s.SourcesLookup.LookupSources(name)
	if name == "muse" && ok {
		f = museCountingFilter{f, s.calls}
	}
	return p, f, ok
}

type museCountingStore struct {
	*storagetest.MemoryStore
	puts int
}

func (s *museCountingStore) Put(ctx context.Context, key string, raw []byte) error {
	s.puts++
	return s.MemoryStore.Put(ctx, key, raw)
}

func TestMuseLocalSourceDoesNotAuthorizeRegisteredCapture(t *testing.T) {
	t.Parallel()
	raw, e := os.ReadFile(filepath.Join("..", "agents", "muse", "testdata", "synthetic-exec-conversation.jsonl"))
	if e != nil {
		t.Fatal(e)
	}
	path := writeTranscript(t, t.TempDir(), "session.jsonl", string(raw))
	registry := builtin.NewBuiltins()
	calls := 0
	sources := museCountingSources{registry, &calls}
	out, _, e := FilterSource(t.Context(), "muse", agentapi.SourceRef{Path: path}, time.Now(), sources)
	if e != nil || len(out.Records) == 0 || calls != 1 {
		t.Fatalf("explicit local source: %d %v", calls, e)
	}
	calls = 0
	local := newTestStore(t)
	reg := registration(t, path)
	reg.Harness = archive.Harness{Name: "muse"}
	reg.NativeSessionID = out.SessionIDs[0]
	reg.ProjectRoot = "/work/synthetic-muse"
	reg.SessionStartedAt = out.NativeStartAt
	if e := local.SaveRegistration(reg); e != nil {
		t.Fatal(e)
	}
	prior := state.ScanSignature{TranscriptSize: 123, TranscriptMtime: 456, FilterVersion: "prior", ParserVersion: "prior"}
	if e := local.SaveScanSignature(reg.ArchiveSessionID, prior); e != nil {
		t.Fatal(e)
	}
	if e := local.SaveRequest(reg.ArchiveSessionID, "stop", time.Now()); e != nil {
		t.Fatal(e)
	}
	before, e := local.LoadRequests()
	if e != nil {
		t.Fatal(e)
	}
	cloud := &museCountingStore{MemoryStore: storagetest.NewMemoryStore()}
	result, e := Run(t.Context(), local, cloud, Options{Sources: sources, Parsers: registry, MachineID: "m", Now: func() time.Time { return time.Now() }})
	if e != nil || len(result.Errors) == 0 || len(result.Published) != 0 || cloud.puts != 0 || calls != 0 {
		t.Fatalf("capture crossed source fence: result=%+v error=%v puts=%d filters=%d", result, e, cloud.puts, calls)
	}
	if !errors.Is(result.Errors[reg.ArchiveSessionID], agentapi.ErrInvalidSourceBinding) {
		t.Fatalf("missing admission reason: %v", result.Errors)
	}
	after, found, e := local.LoadScanSignature(reg.ArchiveSessionID)
	if e != nil || !found || !reflect.DeepEqual(after, prior) {
		t.Fatalf("prior signature changed: %+v %v", after, e)
	}
	requests, e := local.LoadRequests()
	if e != nil || !reflect.DeepEqual(requests, before) {
		t.Fatalf("request acknowledged: %+v %v", requests, e)
	}
	saved, found, e := local.LoadRegistration(reg.ArchiveSessionID)
	if e != nil || !found || !reflect.DeepEqual(saved, reg) {
		t.Fatalf("registration changed: %+v %v", saved, e)
	}
	// Direct reads and capture pass reuse both retain the explicit context fence.
	reader, ok := newSourceReader(reg, Options{Sources: sources})
	if !ok {
		t.Fatal("reader missing")
	}
	if _, e := reader.Signature(t.Context()); e == nil {
		t.Fatal("direct registered source accepted")
	}
	opts := Options{Sources: sources}
	closePasses := openCursorPass(nil, &opts)
	defer func() {
		if e := closePasses(); e != nil {
			t.Error(e)
		}
	}()
	r := reader.(providerReader)
	r.localRead = true
	r.passes = opts.sourcePasses
	p, _, _ := registry.LookupSources("muse")
	if _, _, e := r.pass(t.Context(), p, "muse"); e == nil {
		t.Fatal("local exception leaked into capture pass cache")
	}
}
