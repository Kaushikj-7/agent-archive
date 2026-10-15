package search

import (
	"context"
	"strconv"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/agents/codex"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/reader"
)

func TestVerifiedPreservedUnknownSidecarReusesUnchangedProjection(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"display", "active-source"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			m, active, raw := workerHistory(t)
			m.History.Preserved[0].FilterVersion = ""
			m.History.Preserved[0].SourceSchemaVersion = 0
			s := openTest(t)
			parsers := &preservedReuseParsers{base: builtin.NewBuiltins()}
			at := m.CapturedAt
			counted := &countedObjects{ObjectStore: raw}
			if err := s.Observe(t.Context(), m, parsers, at, 0); err != nil {
				t.Fatal(err)
			}
			first, err := s.Run(t.Context(), counted, parsers, at, Budget{})
			if err != nil || first.Loaded != 2 || first.Committed != 1 || len(first.Gaps) != 0 {
				t.Fatalf("initial verified preservation %+v %v", first, err)
			}
			initialParses := parsers.parses
			if kind == "display" {
				m.Name = "display-only title"
			} else {
				preserved := m.History.Preserved
				active.NativeRecords = append(active.NativeRecords, map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": "new growing active checksum"}})
				active.Ordinals = append(active.Ordinals, 1)
				active.History.Spans[0].EndRecord = 2
				active.History.Spans[0].EndOrdinal = 2
				changed, _, sourceObjects := publishFixture(t, active, codex.Parser{})
				changed.History.Preserved = preserved
				source, err := sourceObjects.Get(context.Background(), changed.SourceBundle.Key)
				if err != nil {
					t.Fatal(err)
				}
				if err = raw.Put(t.Context(), changed.SourceBundle.Key, source); err != nil {
					t.Fatal(err)
				}
				m = changed
			}
			if err = s.Observe(t.Context(), m, parsers, at, 0); err != nil {
				t.Fatal(err)
			}
			next, err := s.Run(t.Context(), counted, parsers, at, Budget{})
			expected := 0
			if kind == "active-source" {
				expected = 1
			}
			t.Logf("kind=%s initial GETs/parses=%d following GETs/parses=%d reuses=%d cumulative source GETs=%d actual initial parses=%d actual following parses=%d", kind, first.Loaded, next.Loaded, next.Reused, counted.gets, initialParses, parsers.parses-initialParses)
			if err != nil || next.Loaded != expected || next.Reused != expected || next.Committed != 1 || next.Pending != 0 || len(next.Gaps) != 0 || counted.gets != 2+expected || initialParses != 2 || parsers.parses-initialParses != expected {
				t.Fatalf("unchanged verified preserved source was reprocessed: %+v %v", next, err)
			}
		})
	}
}

// Missing sidecar provenance never authorizes unsupported source text, including
// when a previously indexed session gains a new retained source request.
func TestWorkerUnknownPreservedSourcePrivacyRemainsClosed(t *testing.T) {
	t.Parallel()
	current, err := strconv.Atoi(archive.FilterVersion)
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"", "unknown", strconv.Itoa(current + 1)} {
		for _, alreadyIndexed := range []bool{false, true} {
			t.Run(version+"/indexed="+strconv.FormatBool(alreadyIndexed), func(t *testing.T) {
				t.Parallel()
				metadata, _, objects := workerHistory(t)
				metadata.History.Preserved[0].FilterVersion = ""
				metadata.History.Preserved[0].SourceSchemaVersion = 0
				s := openTest(t)
				parsers := builtin.NewBuiltins()
				at := metadata.CapturedAt
				if alreadyIndexed {
					if err := s.Observe(t.Context(), metadata, parsers, at, 0); err != nil {
						t.Fatal(err)
					}
					if progress, err := s.Run(t.Context(), objects, parsers, at, Budget{}); err != nil || progress.Committed != 1 {
						t.Fatalf("initial work: %+v %v", progress, err)
					}
				}
				preserved := &metadata.History.Preserved[0]
				bundle, err := reader.LoadRevision(t.Context(), objects, metadata, preserved.RevisionID, reader.Limits{})
				if err != nil {
					t.Fatal(err)
				}
				bundle.Capture.FilterVersion = version
				changed, _, sourceObjects := publishFixture(t, bundle, codex.Parser{})
				data, err := sourceObjects.Get(t.Context(), changed.SourceBundle.Key)
				if err != nil {
					t.Fatal(err)
				}
				if err = objects.Put(t.Context(), changed.SourceBundle.Key, data); err != nil {
					t.Fatal(err)
				}
				preserved.Source = changed.SourceBundle
				preserved.FilterVersion = ""
				preserved.SourceSchemaVersion = 0
				if err = s.Observe(t.Context(), metadata, parsers, at, 0); err != nil {
					t.Fatal(err)
				}
				page, err := s.Search(t.Context(), Query{Text: "old"})
				if err != nil || len(page.Hits) != 0 {
					t.Fatalf("unknown new request retained old text: %+v %v", page, err)
				}
				progress, err := s.Run(t.Context(), objects, parsers, at, Budget{})
				if err != nil || progress.Committed != 1 || len(progress.Gaps) != 1 || progress.Gaps[0].Reason != "historical_visibility" {
					t.Fatalf("unsupported source lacked explicit gap: %+v %v", progress, err)
				}
				page, err = s.Search(t.Context(), Query{Text: "old"})
				if err != nil || len(page.Hits) != 0 {
					t.Fatalf("unsupported source text indexed: %+v %v", page, err)
				}
			})
		}
	}
}

type preservedReuseParsers struct {
	base   agentapi.ParsersLookup
	parses int
}

func (p *preservedReuseParsers) LookupParser(name string) (agentapi.TranscriptParser, bool) {
	parser, ok := p.base.LookupParser(name)
	if !ok {
		return nil, false
	}
	return preservedReuseParser{TranscriptParser: parser, owner: p}, true
}

type preservedReuseParser struct {
	agentapi.TranscriptParser
	owner *preservedReuseParsers
}

func (p preservedReuseParser) Parse(ctx context.Context, bundle archive.SourceBundle) (archive.Analysis, error) {
	p.owner.parses++
	return p.TranscriptParser.Parse(ctx, bundle)
}
