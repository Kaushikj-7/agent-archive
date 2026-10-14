package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/agents/claude"
	"github.com/wangjohn/agent-archive/internal/agents/codex"
	"github.com/wangjohn/agent-archive/internal/agents/cursor"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func openTest(tb testing.TB) *Store {
	tb.Helper()
	s, err := Open(context.Background(), tb.TempDir(), "test/context")
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() {
		if err := s.Close(); err != nil {
			tb.Error(err)
		}
	})
	return s
}

func testProjection(id, text string) (archive.Metadata, Projection) {
	source := Source{Destination: "test/context", SessionID: id, Reference: archive.SourceReference{Key: "source", SHA256: strings.Repeat("a", 64), CompressedBytes: 1}, ParserVersion: "test", FilterVersion: archive.FilterVersion, SchemaVersion: archive.SourceSchemaVersion, ExtractorVersion: ExtractorVersion}
	return archive.Metadata{SessionID: id, ProjectID: "p", Harness: archive.Harness{Name: "codex"}, CapturedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}, Projection{Source: source, Passages: []Passage{{Reference: Reference{Source: source, Record: 0, Start: 0, End: len(text)}, Role: "user", Text: text}}}
}

func TestScopedLiteralSearchAndSnippets(t *testing.T) {
	s := openTest(t)
	for i := range 8 {
		m, p := testProjection(fmt.Sprintf("s%d", i), "Common checksum foo_bar.go: ENOENT café Ελληνικά 日本語 literal OR NEAR prefixing")
		switch i {
		case 0:
			m.Name = "Checksum title"
		case 1:
			m.ProjectID = "other"
		case 2:
			m.Harness.Name = "claude"
		case 3:
			m.Replay = &archive.Replay{}
		case 4:
			m.NativeChild = true
		case 5:
			m.CapturedAt = m.CapturedAt.Add(-48 * time.Hour)
		}
		if _, err := s.Replace(t.Context(), m, []Projection{p}); err != nil {
			t.Fatal(err)
		}
	}
	for _, query := range []string{"checksum", `"foo_bar.go: ENOENT"`, "café", "Ελληνικά", "日本語", "OR", "NEAR", "prefix*", `"literal OR"`} {
		page, err := s.Search(t.Context(), Query{Text: query, Project: "p", Harness: "codex", Replay: Hide, Children: Hide, From: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
		if err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		if len(page.Hits) != 3 {
			t.Fatalf("%s: %#v", query, page)
		}
		if page.Observation.Complete {
			t.Fatal("engine claimed unreconciled archive coverage")
		}
		for _, hit := range page.Hits {
			if hit.Snippet == "" || len([]rune(hit.Snippet)) > 512 {
				t.Fatalf("bad snippet: %#v", hit)
			}
		}
		if query == "checksum" && page.Hits[0].SessionID != "s0" {
			t.Fatalf("title should rank first: %#v", page)
		}
	}
	for _, query := range []string{"cafe", "prefix", "missing"} {
		page, err := s.Search(t.Context(), Query{Text: query})
		if err != nil || len(page.Hits) != 0 {
			t.Fatalf("%s: %#v %v", query, page, err)
		}
	}
	plans, err := s.explain(t.Context(), Query{Text: "common", Project: "p"})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(strings.Join(plans, "\n"))
	if !strings.Contains(strings.Join(plans, " "), "VIRTUAL TABLE INDEX") {
		t.Fatal("query did not use FTS")
	}
}

func TestLiteralSyntaxDoesNotExposeFTSOperators(t *testing.T) {
	for input, want := range map[string]string{"a OR b": `"a" AND "OR" AND "b"`, `"a b" c*`: `"a b" AND "c"*`, `"foo bar"*`: `"foo bar"*`, "-foo": `"-foo"`, "title:foo": `"title:foo"`} {
		got, err := MatchExpression(input)
		if err != nil || got != want {
			t.Fatalf("%q got %q %v", input, got, err)
		}
	}
	for _, input := range []string{"", "!!!", "*", "a*b", `"unterminated`, `"foo"bar`, strings.Repeat("a", 513), string([]byte{0xff})} {
		if _, err := MatchExpression(input); err == nil {
			t.Fatalf("accepted %q", input)
		}
	}
}

func TestGroupedPaginationAndWriteInvalidation(t *testing.T) {
	s := openTest(t)
	for i := range 7 {
		m, p := testProjection(fmt.Sprintf("s%d", i), "common content")
		p2 := p.Passages[0]
		p2.Reference.Record = 1
		p.Passages = append(p.Passages, p2)
		if _, err := s.Replace(t.Context(), m, []Projection{p}); err != nil {
			t.Fatal(err)
		}
	}
	var ids []string
	q := Query{Text: "common", Limit: 2}
	for {
		page, err := s.Search(t.Context(), q)
		if err != nil {
			t.Fatal(err)
		}
		for _, hit := range page.Hits {
			if hit.MatchCount != 2 || hit.Reference == nil {
				t.Fatalf("%#v", hit)
			}
			ids = append(ids, hit.SessionID)
		}
		if page.Next == "" {
			break
		}
		q.Cursor = page.Next
	}
	if strings.Join(ids, ",") != "s0,s1,s2,s3,s4,s5,s6" {
		t.Fatal(ids)
	}
	page, err := s.Search(t.Context(), Query{Text: "common", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	m, p := testProjection("new", "common")
	if _, err = s.Replace(t.Context(), m, []Projection{p}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Search(t.Context(), Query{Text: "common", Limit: 2, Cursor: page.Next}); !errors.Is(err, ErrCursorChanged) {
		t.Fatal(err)
	}
	if _, err = s.Search(t.Context(), Query{Text: "different", Cursor: page.Next}); err == nil {
		t.Fatal("cursor accepted different query")
	}
}

func TestSourceReuseMetadataAndPrivacyRemoval(t *testing.T) {
	s := openTest(t)
	m, p := testProjection("s", "retained checksum")
	first, err := s.Replace(t.Context(), m, []Projection{p})
	if err != nil || first.Inserted != 1 {
		t.Fatalf("%+v %v", first, err)
	}
	m.ProjectID = "new-project"
	m.CapturedAt = m.CapturedAt.Add(time.Hour)
	next, err := s.Replace(t.Context(), m, []Projection{p})
	if err != nil || !next.Unchanged || next.Inserted != 0 || next.Deleted != 0 {
		t.Fatalf("%+v %v", next, err)
	}
	p.Source.ParserVersion = "changed"
	p.Passages[0].Reference.Source = p.Source
	next, err = s.Replace(t.Context(), m, []Projection{p})
	if err != nil || next.Unchanged {
		t.Fatalf("%+v %v", next, err)
	}

}

func TestPrivateDestinationStorageAndReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "search")
	s, err := Open(t.Context(), dir, "a")
	if err != nil {
		t.Fatal(err)
	}
	m, p := testProjection("s", "checksum")
	p.Source.Destination = "a"
	p.Passages[0].Reference.Source = p.Source
	if _, err = s.Replace(t.Context(), m, []Projection{p}); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{dir, filepath.Join(dir, "content.sqlite")} {
		info, e := os.Stat(name)
		if e != nil || info.Mode().Perm()&0077 != 0 {
			t.Fatalf("private path %s: %v %v", name, info, e)
		}
	}
	s, err = Open(t.Context(), dir, "a")
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.Search(t.Context(), Query{Text: "checksum"})
	if err != nil || len(page.Hits) != 1 {
		t.Fatalf("%+v %v", page, err)
	}
	_ = s.Close()
	if wrong, e := Open(t.Context(), dir, "b"); e == nil {
		_ = wrong.Close()
		t.Fatal("cross-destination open accepted")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err = os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if wrong, e := Open(t.Context(), link, "a"); e == nil {
		_ = wrong.Close()
		t.Fatal("directory symlink accepted")
	}
}

// verifiedFixture uses the production filter, bundle codec, metadata builder,
// object store reader and parser. No native home, app or bucket is accessed.
func verifiedFixture(tb testing.TB, harness, raw string) (archive.Metadata, archive.SourceBundle, *storagetest.MemoryStore) {
	tb.Helper()
	var adapter interface {
		archive.Adapter
		FilterJSONL(io.Reader) (archive.FilteredTranscript, error)
	}
	var parser agentapi.TranscriptParser
	switch fixtureHarness(harness) {
	case fixtureCodex:
		adapter = codex.Filter{}
		parser = codex.Parser{}
	case fixtureClaude:
		adapter = claude.Filter{}
		parser = claude.Parser{}
	case fixtureCursor:
		adapter = cursor.Filter{}
		parser = cursor.Parser{}
	default:
		tb.Fatal("bad fixture harness")
	}
	filtered, err := adapter.FilterJSONL(strings.NewReader(raw))
	if err != nil {
		tb.Fatal(err)
	}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reg := archive.SessionRegistration{ArchiveSessionID: "session", NativeSessionID: "native", ProjectID: "p", ProjectRoot: "/synthetic", Harness: archive.Harness{Name: harness}, SessionStartedAt: at}
	bundle, err := archive.NewSourceBundle(reg, adapter, filtered, at, nil)
	if err != nil {
		tb.Fatal(err)
	}
	return publishFixture(tb, bundle, parser)
}

func publishFixture(tb testing.TB, bundle archive.SourceBundle, parser agentapi.TranscriptParser) (archive.Metadata, archive.SourceBundle, *storagetest.MemoryStore) {
	tb.Helper()
	compressed, err := archive.BuildCompressedSource(bundle)
	if err != nil {
		tb.Fatal(err)
	}
	key, err := archive.SourceObjectKey(bundle, compressed.SHA256)
	if err != nil {
		tb.Fatal(err)
	}
	objects := storagetest.NewMemoryStore()
	if err = objects.Put(context.Background(), key, compressed.Bytes); err != nil {
		tb.Fatal(err)
	}
	analysis, parseErr := parser.Parse(context.Background(), bundle)
	m, err := archive.BuildMetadataWithAnalysis(bundle, analysis, parseErr, "synthetic", bundle.Capture.CapturedAt, bundle.Capture.CapturedAt, archive.SourceReference{Key: key, SHA256: compressed.SHA256, CompressedBytes: len(compressed.Bytes)}, archive.ParserInfo{Version: parser.Version()})
	if err != nil {
		tb.Fatal(err)
	}
	putMetadata(tb, objects, m)
	return m, bundle, objects
}

func putMetadata(tb testing.TB, objects *storagetest.MemoryStore, m archive.Metadata) {
	tb.Helper()
	key, err := archive.MetadataObjectKey(m.Harness.Name, m.SessionID)
	if err != nil {
		tb.Fatal(err)
	}
	b, err := json.Marshal(m)
	if err != nil {
		tb.Fatal(err)
	}
	if err = objects.Put(context.Background(), key, b); err != nil {
		tb.Fatal(err)
	}
}

func TestVerifiedNativeVisibilityAndPassageReads(t *testing.T) {
	fixtures := map[string]string{
		"codex": `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"human checksum"}]}}
{"type":"event_msg","payload":{"type":"user_message","message":"human checksum"}}
{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"assistant checksum"}]}}
{"type":"response_item","payload":{"type":"function_call_output","call_id":"x","output":"tool secretword"}}
{"type":"response_item","payload":{"type":"reasoning","summary":[{"text":"reason secretword"}]}}`,
		"claude": `{"type":"user","uuid":"u1","message":{"role":"user","content":"human checksum"}}
{"type":"user","uuid":"meta","isMeta":true,"message":{"role":"user","content":"meta secretword"}}
{"type":"assistant","uuid":"a1","message":{"id":"message-a1","role":"assistant","content":[{"type":"thinking","thinking":"reason secretword"},{"type":"text","text":"assistant checksum"}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"tool secretword"}]}}`,
		"cursor": `{"type":"message","role":"user","content":"human checksum"}
{"type":"message","role":"assistant","content":"assistant checksum"}`,
	}
	for harness, raw := range fixtures {
		t.Run(harness, func(t *testing.T) {
			m, _, objects := verifiedFixture(t, harness, raw)
			parsers := builtin.NewBuiltins()
			projections, err := LoadSession(t.Context(), objects, parsers, "test/context", m, reader.Limits{})
			if err != nil {
				t.Fatal(err)
			}
			if len(projections) != 1 || len(projections[0].Passages) != 2 || projections[0].Gap != nil {
				t.Fatalf("%#v", projections)
			}
			s := openTest(t)
			if _, err = s.Replace(t.Context(), m, projections); err != nil {
				t.Fatal(err)
			}
			page, err := s.Search(t.Context(), Query{Text: "checksum"})
			if err != nil || len(page.Hits) != 1 {
				t.Fatalf("%#v %v", page, err)
			}
			absent, err := s.Search(t.Context(), Query{Text: "secretword"})
			if err != nil || len(absent.Hits) != 0 {
				t.Fatalf("hidden text indexed: %#v %v", absent, err)
			}
			ref := projections[0].Passages[0].Reference
			got, err := ReadPassage(t.Context(), objects, parsers, ref, ref.Start, 4, reader.Limits{})
			if err != nil || got.Text != "huma" || got.Next != 4 {
				t.Fatalf("%+v %v", got, err)
			}
			got, err = ReadPassage(t.Context(), objects, parsers, ref, got.Next, 8192, reader.Limits{})
			if err != nil || got.Text != "n checksum" || got.Next != 0 {
				t.Fatalf("%+v %v", got, err)
			}
			wrong := ref
			wrong.Source.ParserVersion = "old"
			if _, err = ReadPassage(t.Context(), objects, parsers, wrong, 0, 8192, reader.Limits{}); !errors.Is(err, ErrChanged) {
				t.Fatal(err)
			}
			if err = objects.Put(t.Context(), m.SourceBundle.Key, []byte("tampered")); err != nil {
				t.Fatal(err)
			}
			if _, err = LoadSession(t.Context(), objects, parsers, "test/context", m, reader.Limits{}); err == nil {
				t.Fatal("accepted tampered source")
			}
			if _, err = ReadPassage(t.Context(), objects, parsers, ref, 0, 8192, reader.Limits{}); !errors.Is(err, ErrUnavailable) {
				t.Fatal(err)
			}
			key, _ := archive.MetadataObjectKey(m.Harness.Name, m.SessionID)
			if err = objects.Delete(t.Context(), key); err != nil {
				t.Fatal(err)
			}
			if _, err = ReadPassage(t.Context(), objects, parsers, ref, 0, 8192, reader.Limits{}); !errors.Is(err, ErrDeleted) {
				t.Fatal(err)
			}
		})
	}
}

func TestBoundedUnicodePassagesAndIdentityDedup(t *testing.T) {
	text := strings.Repeat("界", 5000) + " checksum boundary"
	raw := fmt.Sprintf(`{"type":"user","uuid":"u1","message":{"role":"user","content":%q}}
{"type":"user","uuid":"u1","message":{"role":"user","content":%q}}
{"type":"user","uuid":"u2","message":{"role":"user","content":%q}}`, text, text, text)
	m, bundle, objects := verifiedFixture(t, "claude", raw)
	p, err := LoadProjection(t.Context(), objects, builtin.NewBuiltins(), "test/context", m, "", reader.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Passages) != 4 {
		t.Fatalf("expected two intentional prompts, each two chunks: %d", len(p.Passages))
	}
	for _, passage := range p.Passages {
		if len(passage.Text) > passageBytes || !utf8.ValidString(passage.Text) {
			t.Fatalf("bad chunk: %+v", passage.Reference)
		}
		turn := bundle.NativeRecords[passage.Reference.Record]
		_ = turn
		got, err := ReadPassage(t.Context(), objects, builtin.NewBuiltins(), passage.Reference, passage.Reference.Start, 8192, reader.Limits{})
		if err != nil || got.Text != passage.Text {
			t.Fatalf("chunk read: %v", err)
		}
	}
	s := openTest(t)
	if _, err = s.Replace(t.Context(), m, []Projection{p}); err != nil {
		t.Fatal(err)
	}
	page, err := s.Search(t.Context(), Query{Text: "checksum"})
	if err != nil || len(page.Hits) != 1 {
		t.Fatalf("%+v %v", page, err)
	}
}

func TestUnsupportedParserAndSourceBounds(t *testing.T) {
	m, _, objects := verifiedFixture(t, "codex", `{"type":"response_item","payload":{"type":"message","role":"user","content":"checksum"}}`)
	p, err := LoadProjection(t.Context(), objects, nil, "test/context", m, "", reader.Limits{})
	if err != nil || p.Gap == nil || p.Gap.Reason != "parser_unavailable" {
		t.Fatalf("%+v %v", p, err)
	}
	if _, err = LoadSession(t.Context(), objects, builtin.NewBuiltins(), "test/context", m, reader.Limits{MaxCompressedBytes: 1}); !errors.Is(err, storage.ErrObjectTooLarge) {
		t.Fatal(err)
	}
}

func TestRetainedAlternativesAndInheritedOwnership(t *testing.T) {
	const thread = "11111111-1111-4111-8111-111111111111"
	const previous = "22222222-2222-4222-8222-222222222222"
	const current = "33333333-3333-4333-8333-333333333333"
	const other = "44444444-4444-4444-8444-444444444444"
	_, seed, _ := verifiedFixture(t, "codex", `{"type":"response_item","payload":{"type":"message","role":"user","content":"shared checksum"}}`)
	old := seed
	old.SchemaVersion = archive.HistorySourceSchemaVersion
	old.NativeSessionID = thread
	old.Ordinals = []uint64{0}
	old.History = &archive.SourceHistory{ThreadID: thread, ActiveRolloutID: previous, Spans: []archive.HistorySpan{{ThreadID: thread, RolloutID: previous, FirstRecord: 0, EndRecord: 1, StartOrdinal: 0, EndOrdinal: 1}}}
	oldMetadata, _, oldObjects := publishFixture(t, old, codex.Parser{})
	active := old
	active.Capture.CapturedAt = active.Capture.CapturedAt.Add(time.Hour)
	newRecord := map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "assistant", "content": "new checksum"}}
	active.NativeRecords = append(append([]map[string]any(nil), old.NativeRecords...), newRecord)
	active.Ordinals = []uint64{0, 1}
	active.History = &archive.SourceHistory{ThreadID: thread, ActiveRolloutID: current, Spans: []archive.HistorySpan{{ThreadID: thread, RolloutID: previous, FirstRecord: 0, EndRecord: 1, StartOrdinal: 0, EndOrdinal: 1}, {ThreadID: thread, RolloutID: current, FirstRecord: 1, EndRecord: 2, StartOrdinal: 1, EndOrdinal: 2}}}
	metadata, _, objects := publishFixture(t, active, codex.Parser{})
	metadata.History.Preserved = []archive.RevisionReference{{RevisionID: previous, CapturedAt: old.Capture.CapturedAt, Source: oldMetadata.SourceBundle, SourceSchemaVersion: old.SchemaVersion, FilterVersion: old.Capture.FilterVersion}}
	bytes, err := oldObjects.Get(t.Context(), oldMetadata.SourceBundle.Key)
	if err != nil {
		t.Fatal(err)
	}
	if err = objects.Put(t.Context(), oldMetadata.SourceBundle.Key, bytes); err != nil {
		t.Fatal(err)
	}
	putMetadata(t, objects, metadata)
	projections, err := LoadSession(t.Context(), objects, builtin.NewBuiltins(), "test/context", metadata, reader.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(projections) != 2 || len(projections[0].Passages) != 2 || len(projections[1].Passages) != 1 {
		t.Fatalf("%#v", projections)
	}
	s := openTest(t)
	stats, err := s.Replace(t.Context(), metadata, projections)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Inserted != 3 {
		t.Fatalf("expected title plus two history messages: %+v", stats)
	}
	page, err := s.Search(t.Context(), Query{Text: "checksum"})
	if err != nil || len(page.Hits) != 1 || page.Hits[0].MatchCount != 3 {
		t.Fatalf("%+v %v", page, err)
	}
	// A preserved alternative retains distinct content, even at the same ordinal.
	projections[1].Passages[0].Text = "other checksum"
	projections[1].Passages[0].Reference.End = len("other checksum")
	if _, err = s.Replace(t.Context(), metadata, projections); err != nil {
		t.Fatal(err)
	}
	page, err = s.Search(t.Context(), Query{Text: "other"})
	if err != nil || len(page.Hits) != 1 {
		t.Fatalf("alternative lost: %+v %v", page, err)
	}
	// Copied other-thread history obeys the existing parser's ownership contract.
	inherited := active
	inherited.History = &archive.SourceHistory{ThreadID: thread, ActiveRolloutID: current, Spans: []archive.HistorySpan{{ThreadID: other, RolloutID: previous, FirstRecord: 0, EndRecord: 1, StartOrdinal: 0, EndOrdinal: 1}, {ThreadID: thread, RolloutID: current, FirstRecord: 1, EndRecord: 2, StartOrdinal: 1, EndOrdinal: 2}}}
	inheritedMetadata, _, inheritedObjects := publishFixture(t, inherited, codex.Parser{})
	projected, err := LoadSession(t.Context(), inheritedObjects, builtin.NewBuiltins(), "test/context", inheritedMetadata, reader.Limits{})
	if err != nil || len(projected[0].Passages) != 1 {
		t.Fatalf("inherited ownership: %+v %v", projected, err)
	}
}

func TestTitleChangeDoesNotRewriteBodyPassages(t *testing.T) {
	s := openTest(t)
	m, p := testProjection("s", "checksum body")
	m.Name = "old title"
	if _, err := s.Replace(t.Context(), m, []Projection{p}); err != nil {
		t.Fatal(err)
	}
	var before int64
	if err := s.db.QueryRowContext(t.Context(), "SELECT id FROM search_passages WHERE title_hit=0").Scan(&before); err != nil {
		t.Fatal(err)
	}
	m.Name = "new title"
	stats, err := s.Replace(t.Context(), m, []Projection{p})
	if err != nil || !stats.Unchanged || stats.Inserted != 1 || stats.Deleted != 1 {
		t.Fatalf("%+v %v", stats, err)
	}
	var after int64
	if err = s.db.QueryRowContext(t.Context(), "SELECT id FROM search_passages WHERE title_hit=0").Scan(&after); err != nil || before != after {
		t.Fatalf("body rewritten: %d %d %v", before, after, err)
	}
	page, err := s.Search(t.Context(), Query{Text: "old"})
	if err != nil || len(page.Hits) != 0 {
		t.Fatalf("old title indexed: %+v %v", page, err)
	}
}

func TestSourceReuseBindsInterpretationAndOwnership(t *testing.T) {
	_, p := testProjection("s", "checksum")
	original := p.Source
	want := original.Key()
	for _, change := range []func(*Source){func(s *Source) { s.Destination = "other" }, func(s *Source) { s.SessionID = "other" }, func(s *Source) { s.ParserVersion = "new" }, func(s *Source) { s.FilterVersion = "new" }, func(s *Source) { s.SchemaVersion++ }, func(s *Source) { s.ExtractorVersion = "new" }, func(s *Source) { s.RevisionID = "other" }} {
		changed := original
		change(&changed)
		if changed.Key() == want {
			t.Fatalf("source reuse ignored changed provenance: %+v", changed)
		}
	}
}

func TestCancelledQueriesAndConcurrentRefresh(t *testing.T) {
	s := openTest(t)
	m, p := testProjection("s", "checksum")
	if _, err := s.Replace(t.Context(), m, []Projection{p}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.Search(ctx, Query{Text: "checksum"}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		for range 10 {
			if _, err := s.Replace(t.Context(), m, []Projection{p}); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	for range 10 {
		page, err := s.Search(t.Context(), Query{Text: "checksum"})
		if err != nil || len(page.Hits) != 1 {
			t.Fatalf("query while writing: %+v %v", page, err)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// Explain returns the actual scoped ranking query plan for performance evidence.
func (s *Store) explain(ctx context.Context, q Query) ([]string, error) {
	expression, err := MatchExpression(q.Text)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+rankedQuery+" ORDER BY title_hit DESC,score,role_rank,captured DESC,session LIMIT 20", queryArgs(expression, q)...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
			return nil, err
		}
		out = append(out, fmt.Sprintf("%d/%d %s", id, parent, detail))
	}
	return out, rows.Err()
}

func TestTextOnlySourcesReportGapAndSameIDsRemainScoped(t *testing.T) {
	_, bundle, _ := verifiedFixture(t, "cursor", `{"type":"message","role":"user","content":"checksum"}`)
	bundle.NativeRecords = nil
	bundle.NativeText = []archive.TextTranscript{{Format: "cursor-transcript", Content: "User: checksum"}}
	metadata, _, objects := publishFixture(t, bundle, cursor.Parser{})
	projected, err := LoadSession(t.Context(), objects, builtin.NewBuiltins(), "test/context", metadata, reader.Limits{})
	if err != nil || len(projected) != 1 || projected[0].Gap == nil || projected[0].Gap.Reason != "unstructured_text" {
		t.Fatalf("%+v %v", projected, err)
	}
	s := openTest(t)
	for _, harness := range []string{"claude", "codex"} {
		m, p := testProjection("same-id", "checksum")
		m.Harness.Name = harness
		if _, err = s.Replace(t.Context(), m, []Projection{p}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.Search(t.Context(), Query{Text: "checksum"})
	if err != nil || len(page.Hits) != 2 || page.Hits[0].Harness == page.Hits[1].Harness {
		t.Fatalf("cross-harness collision: %+v %v", page, err)
	}
}

func TestExplicitSessionCapsAggregateReadsAndProjectionText(t *testing.T) {
	m, _, objects := verifiedFixture(t, "codex", `{"type":"response_item","payload":{"type":"message","role":"user","content":"checksum"}}`)
	m.SourceBundle.CompressedBytes = maxSessionBytes + 1
	counted := &countedObjects{ObjectStore: objects}
	if _, err := LoadSession(t.Context(), counted, builtin.NewBuiltins(), "test/context", m, reader.Limits{}); !errors.Is(err, storage.ErrObjectTooLarge) || counted.gets != 0 {
		t.Fatalf("aggregate cap read source: %v %d", err, counted.gets)
	}
	s := openTest(t)
	m, p := testProjection("s", strings.Repeat("a", passageBytes))
	for range maxSessionBytes / passageBytes {
		p.Passages = append(p.Passages, p.Passages[0])
	}
	if _, err := s.Replace(t.Context(), m, []Projection{p}); err == nil {
		t.Fatal("oversized session projection indexed")
	}
}

func TestHistoricalVisibilityIsAnExplicitGap(t *testing.T) {
	_, bundle, _ := verifiedFixture(t, "claude", `{"type":"user","message":{"role":"user","content":"possibly generated checksum"}}`)
	current, err := strconv.Atoi(archive.FilterVersion)
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"3", "5", "unknown", strconv.Itoa(current + 1)} {
		bundle.Capture.FilterVersion = version
		metadata, _, objects := publishFixture(t, bundle, claude.Parser{})
		p, err := LoadSession(t.Context(), objects, builtin.NewBuiltins(), "test/context", metadata, reader.Limits{})
		if err != nil || p[0].Gap == nil || p[0].Gap.Reason != "historical_visibility" || len(p[0].Passages) != 0 {
			t.Fatalf("%s: %+v %v", version, p, err)
		}
		ref := Reference{Source: p[0].Source, Record: 0, Start: 0, End: 27}
		if _, err = ReadPassage(t.Context(), objects, builtin.NewBuiltins(), ref, 0, 8192, reader.Limits{}); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("%s passage read: %v", version, err)
		}
	}
}

type fixtureHarness string

const (
	fixtureCodex  fixtureHarness = "codex"
	fixtureClaude fixtureHarness = "claude"
	fixtureCursor fixtureHarness = "cursor"
)

func TestPersistedReferenceAndCursorJSONRemainCompatible(t *testing.T) {
	const legacyReference = `{"Source":{"Destination":"","SessionID":"","RevisionID":"","Reference":{"key":"","sha256":"","compressed_bytes":0},"ParserVersion":"","FilterVersion":"","SchemaVersion":0,"ExtractorVersion":""},"Record":0,"Start":0,"End":0}`
	var reference Reference
	if err := json.Unmarshal([]byte(legacyReference), &reference); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(reference)
	if err != nil || string(encoded) != legacyReference {
		t.Fatalf("persisted reference changed: %s %v", encoded, err)
	}
	const legacyCursor = `{"Generation":1,"Query":"identity","TitleHit":0,"Score":-0.5,"Role":1,"Captured":7,"Session":"codex/s"}`
	var cursor pageCursor
	if err = json.Unmarshal([]byte(legacyCursor), &cursor); err != nil {
		t.Fatal(err)
	}
	encoded, err = json.Marshal(cursor)
	if err != nil || string(encoded) != legacyCursor {
		t.Fatalf("cursor changed: %s %v", encoded, err)
	}
	const legacyQuery = `{"Scope":{"Restricted":false,"Named":false,"RepoKey":"","ProjectName":"","ProjectIDs":null},"Text":"checksum","Project":"","Harness":"","From":"0001-01-01T00:00:00Z","To":"0001-01-01T00:00:00Z","Replay":"hide","Children":"only","Limit":0,"Cursor":""}`
	var query Query
	if err = json.Unmarshal([]byte(legacyQuery), &query); err != nil {
		t.Fatal(err)
	}
	encoded, err = json.Marshal(query)
	if err != nil || string(encoded) != legacyQuery {
		t.Fatalf("query identity encoding changed: %s %v", encoded, err)
	}
	if query.Replay != Hide || query.Children != Only {
		t.Fatalf("legacy choices changed: %+v", query)
	}
}
