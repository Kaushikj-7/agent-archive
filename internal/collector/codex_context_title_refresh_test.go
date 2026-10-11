package collector

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/codex"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

const legacyOpenPage = `<external_codex_apps_open_page>{"page_id":null}</external_codex_apps_open_page>`

// legacyContextFilter retains a context-only user record's text the way
// filter 15 did, before filter 16 dropped the open-page wrapper.
type legacyContextFilter struct{ countedCodexFilter }

func (f legacyContextFilter) Filter(ctx context.Context, in agentapi.NativeInput, c agentapi.FilterContext) (archive.FilteredTranscript, error) {
	out, err := f.countedCodexFilter.Filter(ctx, in, c)
	for i, raw := range out.Records {
		var record map[string]any
		if json.Unmarshal(raw, &record) != nil {
			continue
		}
		if payload, ok := record["payload"].(map[string]any); ok && payload["id"] == "ctx" {
			payload["content"] = []any{map[string]any{"type": "input_text", "text": legacyOpenPage}}
			out.Records[i], _ = json.Marshal(record)
		}
	}
	return out, err
}

// pre027Parser derives the title the way Codex parser 0.26.0 did from such
// a source: the first user text, wrapper and all.
type pre027Parser struct{ codex.Parser }

func (pre027Parser) Version() string { return "0.26.0" }

func (p pre027Parser) Parse(ctx context.Context, b archive.SourceBundle) (archive.Analysis, error) {
	analysis, err := p.Parser.Parse(ctx, b)
	analysis.Facts.TextTitle = legacyOpenPage
	return analysis, err
}

type legacyTitleBindings struct{ parser agentapi.TranscriptParser }

func (b *legacyTitleBindings) LookupParser(name string) (agentapi.TranscriptParser, bool) {
	return b.parser, name == "codex"
}

func (*legacyTitleBindings) LookupSources(name string) (agentapi.SourceProvider, agentapi.TranscriptFilter, bool) {
	return codex.SourceProvider{}, legacyContextFilter{}, name == "codex"
}

// A session archived before filter 16 keeps the open-page wrapper in its
// retained source. The parser bump re-derives its metadata from that source,
// after the native log is gone, and the title becomes the typed prompt
// without a new source upload.
func TestCodexParserBumpRetitlesRetainedOpenPageContext(t *testing.T) {
	t.Parallel()
	transcript := `{"type":"turn_context","model":"gpt-test"}
{"type":"response_item","payload":{"type":"message","id":"ctx","role":"user","content":[{"type":"input_text","text":"placeholder"}]}}
{"type":"response_item","payload":{"type":"message","id":"p1","role":"user","content":[{"type":"input_text","text":"Fix the flaky upload test"}]}}
{"type":"response_item","id":"m1","payload":{"type":"message","role":"assistant","content":"visible"}}`
	local := newTestStore(t)
	path := writeTranscript(t, t.TempDir(), "session.jsonl", transcript)
	reg := registration(t, path)
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	bindings := &legacyTitleBindings{parser: pre027Parser{}}
	remote := &countedPublications{MemoryStore: storagetest.NewMemoryStore()}
	now := reg.RegisteredAt.Add(time.Hour)
	opts := Options{Sources: bindings, Parsers: bindings, MachineID: "machine", Now: func() time.Time { return now }}
	if result, err := Run(context.Background(), local, remote, opts); err != nil || len(result.Errors) != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	old := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	if old.Title != legacyOpenPage {
		t.Fatalf("seeded title = %q", old.Title)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	bindings.parser = codex.Parser{}
	now = now.Add(time.Hour)
	remote.keys = nil
	if result, err := Run(context.Background(), local, remote, opts); err != nil || len(result.Errors) != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	next := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	if next.Title != "Fix the flaky upload test" || next.Parser.Version != (codex.Parser{}).Version() {
		t.Fatalf("refreshed title %q parser %q", next.Title, next.Parser.Version)
	}
	if next.SourceBundle != old.SourceBundle || slices.Contains(remote.keys, old.SourceBundle.Key) {
		t.Fatalf("refresh rewrote the source: %v", remote.keys)
	}
}
