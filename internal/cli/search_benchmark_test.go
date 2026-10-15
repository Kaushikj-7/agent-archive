package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agents/codex"
	"github.com/wangjohn/agent-archive/internal/archive"
	"io"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func BenchmarkContentSearchMissingCLI(b *testing.B) {
	env := benchmarkEnv(b, b.TempDir(), storagetest.NewMemoryStore())
	args := []string{"search", "checksum", "--all-projects", "--json"}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if code := Run(args, nil, io.Discard, io.Discard, env); code != 0 {
			b.Fatal(code)
		}
	}
	b.ReportMetric(0, "source_GET/op")
}

func BenchmarkContentSearchEscapedDocument(b *testing.B) {
	doc := contentSearchDocument{SchemaVersion: 1, Kind: contentSearchResults, OK: true, Results: []contentSearchHit{}}
	for range 100 {
		doc.Results = append(doc.Results, contentSearchHit{Session: "synthetic", Title: strings.Repeat("title", 40), Snippet: strings.Repeat("\u0085", 128), Gaps: []contentSearchGap{}})
	}
	doc.Returned = len(doc.Results)
	opts := contentSearchOptions{json: true, outputBytes: 262144}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if code := writeContentSearch(doc, opts, io.Discard, io.Discard); code != 0 {
			b.Fatal(code)
		}
	}
}

func contentSearchCorpus(tb testing.TB, count int) (Env, *storagetest.MemoryStore) {
	tb.Helper()
	objects := storagetest.NewMemoryStore()
	env := benchmarkEnv(tb, tb.TempDir(), objects)
	at := storagetest.BenchmarkTime
	parser := codex.Parser{}
	for i := range count {
		raw := `{"type":"response_item","payload":{"type":"message","role":"user","content":"common checksum synthetic λ context"}}`
		filtered, err := (codex.Filter{}).FilterJSONL(strings.NewReader(raw))
		if err != nil {
			tb.Fatal(err)
		}
		id := fmt.Sprintf("synthetic-%04d", i)
		reg := archive.SessionRegistration{ArchiveSessionID: id, NativeSessionID: id, ProjectID: fmt.Sprintf("project-%d", i%4), ProjectRoot: "/synthetic", Harness: archive.Harness{Name: "codex"}, SessionStartedAt: at}
		bundle, err := archive.NewSourceBundle(reg, codex.Filter{}, filtered, at, nil)
		if err != nil {
			tb.Fatal(err)
		}
		compressed, err := archive.BuildCompressedSource(bundle)
		if err != nil {
			tb.Fatal(err)
		}
		sourceKey, err := archive.SourceObjectKey(bundle, compressed.SHA256)
		if err != nil {
			tb.Fatal(err)
		}
		if err = objects.Put(tb.Context(), sourceKey, compressed.Bytes); err != nil {
			tb.Fatal(err)
		}
		analysis, parseErr := parser.Parse(tb.Context(), bundle)
		metadata, err := archive.BuildMetadataWithAnalysis(bundle, analysis, parseErr, "synthetic", at, at, archive.SourceReference{Key: sourceKey, SHA256: compressed.SHA256, CompressedBytes: len(compressed.Bytes)}, archive.ParserInfo{Version: parser.Version()})
		if err != nil {
			tb.Fatal(err)
		}
		key, err := archive.MetadataObjectKey("codex", id)
		if err != nil {
			tb.Fatal(err)
		}
		data, err := json.Marshal(metadata)
		if err != nil {
			tb.Fatal(err)
		}
		if err = objects.Put(tb.Context(), key, data); err != nil {
			tb.Fatal(err)
		}
	}
	return env, objects
}

func warmContentSearch(tb testing.TB, env Env, count int) int {
	tb.Helper()
	for pass := 1; pass <= 64; pass++ {
		var out, diag bytes.Buffer
		if code := Run([]string{"search", "--refresh", "--json"}, nil, &out, &diag, env); code != 0 {
			tb.Fatalf("refresh %d %s %s", code, out.String(), diag.String())
		}
		var doc contentSearchDocument
		if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
			tb.Fatal(err)
		}
		if doc.Coverage.Indexed == count && doc.Coverage.Pending == 0 {
			return pass
		}
	}
	tb.Fatal("bounded corpus did not catch up")
	return 0
}

func BenchmarkContentSearchWarmCLI(b *testing.B) {
	env, _ := contentSearchCorpus(b, 40)
	passes := warmContentSearch(b, env, 40)
	args := []string{"search", "common", "--all-projects", "--json"}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if code := Run(args, nil, io.Discard, io.Discard, env); code != 0 {
			b.Fatal(code)
		}
	}
	b.ReportMetric(float64(passes), "warmup_passes")
	b.ReportMetric(0, "source_GET/op")
}
