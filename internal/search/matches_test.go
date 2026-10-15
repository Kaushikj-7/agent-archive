package search

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestMatchPagesPreserveScopeRoleOrderAndGeneration(t *testing.T) {
	s := openTest(t)
	m, p := testProjection("same-id", "needle")
	m.Name = "needle title"
	p.Passages = nil
	for i := range 45 {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		p.Passages = append(p.Passages, Passage{Reference: Reference{Source: p.Source, Record: i, Start: 0, End: 6}, Role: role, Text: "needle"})
	}
	if _, err := s.Replace(t.Context(), m, []Projection{p}); err != nil {
		t.Fatal(err)
	}
	// A colliding session ID in another harness must not leak into navigation.
	other, projection := testProjection("same-id", "needle")
	other.Harness.Name = "claude"
	if _, err := s.Replace(t.Context(), other, []Projection{projection}); err != nil {
		t.Fatal(err)
	}
	q := Query{Text: "needle", Scope: Scope{Restricted: true, ProjectIDs: []string{"p"}}}
	results, err := s.Search(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	var cursors []string
	var pages [][]Reference
	cursor := ""
	for {
		cursors = append(cursors, cursor)
		page, err := s.Matches(t.Context(), q, "codex", "same-id", results.Generation, cursor)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.References) > 20 {
			t.Fatalf("unbounded page: %d", len(page.References))
		}
		pages = append(pages, page.References)
		for _, ref := range page.References {
			if ref.Source.SessionID != "same-id" {
				t.Fatalf("wrong session: %+v", ref)
			}
		}
		if page.Previous != "" {
			previous, err := s.Matches(t.Context(), q, "codex", "same-id", results.Generation, page.Previous)
			if err != nil || !reflect.DeepEqual(previous.References, pages[len(pages)-2]) {
				t.Fatalf("reverse page: %v %+v", err, previous)
			}
		}
		cursor = page.Next
		if cursor == "" {
			break
		}
		if len(pages) > 3 {
			t.Fatal("repeated cursor")
		}
	}
	var records []int
	for _, page := range pages {
		for _, ref := range page {
			records = append(records, ref.Record)
		}
	}
	var expected []int
	for parity := range 2 {
		for i := parity; i < 45; i += 2 {
			expected = append(expected, i)
		}
	}
	if !reflect.DeepEqual(records, expected) {
		t.Fatalf("body order %v want %v", records, expected)
	}
	if len(pages) != 3 || len(pages[2]) != 5 {
		t.Fatalf("bounds: %v", pages)
	}
	wrong := q
	wrong.Scope.ProjectIDs = []string{"other"}
	if _, err = s.Matches(t.Context(), wrong, "codex", "same-id", results.Generation, cursors[1]); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("changed query: %v", err)
	}
	if _, err = s.Matches(t.Context(), q, "claude", "same-id", results.Generation, cursors[1]); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("changed identity: %v", err)
	}
	if page, err := s.Matches(t.Context(), wrong, "codex", "same-id", results.Generation, ""); err != nil || len(page.References) != 0 {
		t.Fatalf("scope widened: %+v %v", page, err)
	}
	m.Name = fmt.Sprintf("changed %d", len(records))
	if _, err = s.Replace(t.Context(), m, []Projection{p}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Matches(t.Context(), q, "codex", "same-id", results.Generation, ""); !errors.Is(err, ErrCursorChanged) {
		t.Fatalf("changed generation: %v", err)
	}
}

func BenchmarkSessionMatchPages(b *testing.B) {
	s := openTest(b)
	benchmarkCorpus(b, s, 250, 45)
	q := Query{Text: "common"}
	results, err := s.Search(b.Context(), q)
	if err != nil {
		b.Fatal(err)
	}
	first, err := s.Matches(b.Context(), q, "codex", "session-000000", results.Generation, "")
	if err != nil {
		b.Fatal(err)
	}
	second, err := s.Matches(b.Context(), q, "codex", "session-000000", results.Generation, first.Next)
	if err != nil {
		b.Fatal(err)
	}
	for _, tc := range []struct{ name, cursor string }{{"first", ""}, {"forward", first.Next}, {"reverse", second.Previous}} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				page, err := s.Matches(b.Context(), q, "codex", "session-000000", results.Generation, tc.cursor)
				if err != nil || len(page.References) != 20 {
					b.Fatalf("%+v %v", page, err)
				}
			}
			b.ReportMetric(11250, "corpus_passages")
			b.ReportMetric(20, "retained_refs/op")
			b.ReportMetric(0, "source_GET/op")
		})
	}
}
