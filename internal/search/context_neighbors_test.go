package search

import (
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/reader"
)

func TestSearchContextNeighborsStayVisibleAndContinueRawBytes(t *testing.T) {
	raw := `{"type":"response_item","payload":{"type":"message","role":"user","content":"before visible"}}` + "\n" + `{"type":"response_item","payload":{"type":"message","role":"assistant","content":"` + strings.Repeat("λ", 6000) + `"}}` + "\n" + `{"type":"response_item","payload":{"type":"message","role":"tool","content":"hidden tool text"}}` + "\n" + `{"type":"response_item","payload":{"type":"message","role":"user","content":"after visible"}}`
	m, _, objects := verifiedFixture(t, "codex", raw)
	parsers := builtin.NewBuiltins()
	p, err := LoadProjection(t.Context(), objects, parsers, "test/context", m, "", reader.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	var ref Reference
	for _, v := range p.Passages {
		if v.Role == "assistant" {
			ref = v.Reference
			break
		}
	}
	if ref.End == 0 {
		t.Fatal("missing selected assistant")
	}
	out, err := ReadContext(t.Context(), objects, parsers, ref, ref.Start, 5, 2, 2, reader.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Selected.Text != "λλ" || out.Selected.End-ref.Start != 4 || out.Selected.Next != ref.Start+4 {
		t.Fatalf("raw UTF-8 continuation %+v", out.Selected)
	}
	if len(out.Before) != 1 || len(out.After) != 1 || strings.Contains(out.After[0].Context.Text, "tool") {
		t.Fatalf("visibility or same-message chunk duplication: %+v", out)
	}
	next, err := ReadPassage(t.Context(), objects, parsers, ref, out.Selected.Next, 5, reader.Limits{})
	if err != nil || next.Text != "λλ" {
		t.Fatalf("continuation %+v %v", next, err)
	}
}

func BenchmarkSearchContextNeighbors(b *testing.B) {
	raw := `{"type":"response_item","payload":{"type":"message","role":"user","content":"neighbor"}}` + "\n" + `{"type":"response_item","payload":{"type":"message","role":"assistant","content":"` + strings.Repeat("synthetic checksum λ ", 10000) + `"}}`
	m, _, objects := verifiedFixture(b, "codex", raw)
	parsers := builtin.NewBuiltins()
	p, err := LoadProjection(b.Context(), objects, parsers, "test/context", m, "", reader.Limits{})
	if err != nil {
		b.Fatal(err)
	}
	ref := p.Passages[1].Reference
	counted := &countedObjects{ObjectStore: objects}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err = ReadContext(b.Context(), counted, parsers, ref, ref.Start, 4096, 2, 2, reader.Limits{}); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(counted.gets)/float64(b.N), "object_GET/op")
	b.ReportMetric(1, "parse/op")
}

func TestSearchContextReadsOnThroughLaterPassagesOfOneMessage(t *testing.T) {
	long := strings.Repeat("synthetic λ line ", 1250) // 22,500 bytes: three passages
	raw := `{"type":"response_item","payload":{"type":"message","role":"user","content":"before visible"}}` + "\n" + `{"type":"response_item","payload":{"type":"message","role":"assistant","content":"` + long + `"}}` + "\n" + `{"type":"response_item","payload":{"type":"message","role":"user","content":"after visible"}}`
	m, _, objects := verifiedFixture(t, "codex", raw)
	parsers := builtin.NewBuiltins()
	p, err := LoadProjection(t.Context(), objects, parsers, "test/context", m, "", reader.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	var ref Reference
	for _, v := range p.Passages {
		if v.Role == "assistant" {
			ref = v.Reference
			break
		}
	}
	if ref.End >= len(long) {
		t.Fatalf("fixture message fits one passage: %+v", ref)
	}
	var read strings.Builder
	offset, reads := ref.Start, 0
	for {
		out, err := ReadContext(t.Context(), objects, parsers, ref, offset, 4096, 1, 1, reader.Limits{})
		if err != nil {
			t.Fatalf("read from %d: %v", offset, err)
		}
		if out.Selected.Start != offset {
			t.Fatalf("read from %d started at %d", offset, out.Selected.Start)
		}
		if len(out.Before) != 1 || len(out.After) != 1 || out.After[0].Context.Text != "after visible" {
			t.Fatalf("neighbors from %d: %+v", offset, out)
		}
		read.WriteString(out.Selected.Text)
		if reads++; out.Selected.Next == 0 || reads > 20 {
			break
		}
		offset = out.Selected.Next
	}
	if read.String() != long {
		t.Fatalf("read %d of %d bytes in %d reads", read.Len(), len(long), reads)
	}
}
