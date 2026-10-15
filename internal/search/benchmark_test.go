package search

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage"
)

type countedObjects struct {
	storage.ObjectStore
	gets  int
	bytes int
}

func (s *countedObjects) GetLimited(ctx context.Context, key string, limit int64) ([]byte, error) {
	s.gets++
	b, e := s.ObjectStore.(storage.LimitedGetter).GetLimited(ctx, key, limit)
	s.bytes += len(b)
	return b, e
}

func BenchmarkVerifiedProjection(b *testing.B) {
	raw := strings.Repeat(`{"type":"response_item","payload":{"type":"message","role":"user","content":"checksum source verification foo_bar.go Ελληνικά"}}`+"\n", 1000)
	m, _, objects := verifiedFixture(b, "codex", raw)
	counted := &countedObjects{ObjectStore: objects}
	parsers := builtin.NewBuiltins()
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for b.Loop() {
		if _, err := LoadProjection(b.Context(), counted, parsers, "test/context", m, "", reader.Limits{}); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(counted.gets)/float64(b.N), "source_GET/op")
	b.ReportMetric(float64(counted.bytes)/float64(b.N), "download_B/op")
	b.ReportMetric(1, "parse/op")
}

func benchmarkCorpus(b testing.TB, s *Store, count, messages int) {
	b.Helper()
	for i := range count {
		m, p := testProjection(fmt.Sprintf("session-%06d", i), fmt.Sprintf("common upload checksum foo_bar.go rare%d", i))
		m.ProjectID = fmt.Sprintf("project-%d", i%100)
		for n := 1; n < messages; n++ {
			next := p.Passages[0]
			next.Reference.Record = n
			p.Passages = append(p.Passages, next)
		}
		if _, err := s.Replace(b.Context(), m, []Projection{p}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWarmQueries(b *testing.B) {
	s := openTest(b)
	benchmarkCorpus(b, s, 2000, 10)
	for _, tc := range []struct {
		name string
		q    Query
	}{{"broad", Query{Text: "common", Limit: 20}}, {"scoped", Query{Text: "common", Project: "project-7", Limit: 20}}, {"rare", Query{Text: "rare17", Limit: 20}}} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := s.Search(b.Context(), tc.q); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(20000, "corpus_passages")
			b.ReportMetric(0, "source_GET/op")
		})
	}
}

func BenchmarkColdIndex(b *testing.B) {
	for b.Loop() {
		dir := b.TempDir()
		s, err := Open(b.Context(), dir, "test/context")
		if err != nil {
			b.Fatal(err)
		}
		benchmarkCorpus(b, s, 250, 20)
		var bytes int64
		for _, suffix := range []string{"", "-wal", "-shm"} {
			info, e := os.Stat(filepath.Join(dir, "content.sqlite") + suffix)
			if e == nil {
				bytes += info.Size()
			} else if !os.IsNotExist(e) {
				b.Fatal(e)
			}
		}
		b.ReportMetric(float64(bytes), "disk_B")
		b.ReportMetric(5000, "FTS_insert/op")
		b.ReportMetric(0, "source_GET/op")
		if err = s.Close(); err != nil {
			b.Fatal(err)
		}
	}
}
