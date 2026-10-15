package search

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
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
	// Body references may belong to both active and retained source revisions.
	historical := p
	historical.Source.Reference.Key = "preserved"
	historical.Source.Reference.SHA256 = strings.Repeat("b", 64)
	historical.Passages = append([]Passage(nil), p.Passages[25:]...)
	p.Passages = p.Passages[:25]
	for i := range historical.Passages {
		historical.Passages[i].Reference.Source = historical.Source
	}
	if _, err := s.Replace(t.Context(), m, []Projection{p, historical}); err != nil {
		t.Fatal(err)
	}
	// A colliding session ID in another harness must not leak into navigation.
	other, projection := testProjection("same-id", "needle")
	other.Harness.Name = "claude"
	if _, err := s.Replace(t.Context(), other, []Projection{projection}); err != nil {
		t.Fatal(err)
	}
	// Move one retained member beyond the other session's rows: the range must
	// remain exact without assuming that a session's row IDs are contiguous.
	widenSessionMatchFixture(t, s, "codex/same-id")
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
			if ref.Record >= 25 && ref.Source.Reference.Key != "preserved" {
				t.Fatalf("retained source reference lost: %+v", ref)
			}
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
	for _, fixture := range []struct {
		count   int
		widened bool
	}{{10, false}, {250, false}, {250, true}} {
		b.Run(fmt.Sprintf("sessions%d/widened%v", fixture.count, fixture.widened), func(b *testing.B) {
			count := fixture.count
			s := openTest(b)
			benchmarkCorpus(b, s, count, 45)
			if fixture.widened {
				widenSessionMatchFixture(b, s, "codex/session-000000")
			}
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
			for _, tc := range []struct {
				name   string
				cursor string
			}{{"first", ""}, {"forward", first.Next}, {"reverse", second.Previous}} {
				b.Run(tc.name, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						page, err := s.Matches(b.Context(), q, "codex", "session-000000", results.Generation, tc.cursor)
						if err != nil || len(page.References) != 20 {
							b.Fatalf("%+v %v", page, err)
						}
					}
					b.ReportMetric(float64(count*45), "corpus_passages")
					b.ReportMetric(20, "retained_refs/op")
					b.ReportMetric(0, "source_GET/op")
				})
			}
		})
	}
}

// widenSessionMatchFixture preserves all text/reference/membership bytes while
// moving one internal member ID beyond unrelated rows. This models a sparse
// membership span without relying on today's atomic replacement allocation.
func widenSessionMatchFixture(tb testing.TB, s *Store, session string) {
	tb.Helper()
	tx, err := s.db.BeginTx(tb.Context(), nil)
	if err != nil {
		tb.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	var member, last int64
	if err = tx.QueryRowContext(tb.Context(), "SELECT max(id) FROM search_passages WHERE session=? AND title_hit=0", session).Scan(&member); err != nil {
		tb.Fatal(err)
	}
	if err = tx.QueryRowContext(tb.Context(), "SELECT max(id) FROM search_passages").Scan(&last); err != nil {
		tb.Fatal(err)
	}
	if _, err = tx.ExecContext(tb.Context(), "UPDATE search_passages SET id=? WHERE id=?", last+1, member); err != nil {
		tb.Fatal(err)
	}
	if _, err = tx.ExecContext(tb.Context(), "UPDATE search_fts SET rowid=? WHERE rowid=?", last+1, member); err != nil {
		tb.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		tb.Fatal(err)
	}
}

// This records actual join order rather than claiming a selected-session WHERE
// clause alone makes a broad FTS posting scan cheap. Timing comes from the
// separate advisory benchmark on the same corpus and production query.
func TestSessionMatchQueryPlanWithUnrelatedCommonHits(t *testing.T) {
	s := openTest(t)
	benchmarkCorpus(t, s, 250, 45)
	q := Query{Text: "common"}
	expression, err := MatchExpression(q.Text)
	if err != nil {
		t.Fatal(err)
	}
	results, err := s.Search(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.Matches(t.Context(), q, "codex", "session-000000", results.Generation, "")
	if err != nil || len(first.References) != 20 || first.Next == "" {
		t.Fatalf("first bounded page: %+v %v", first, err)
	}
	second, err := s.Matches(t.Context(), q, "codex", "session-000000", results.Generation, first.Next)
	if err != nil || len(second.References) != 20 || second.Previous == "" {
		t.Fatalf("forward bounded page: %+v %v", second, err)
	}
	for _, token := range []string{"", first.Next, second.Previous} {
		cursor, err := decodeMatchCursor(token, results.Generation, queryIdentity(q), "codex/session-000000")
		if err != nil {
			t.Fatal(err)
		}
		sqlText, args := matchPageSQL(q, expression, "codex", "session-000000", cursor, token != "")
		plan, err := s.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+sqlText, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = plan.Close() }()
		var details []string
		for plan.Next() {
			var id, parent, unused int
			var detail string
			if err = plan.Scan(&id, &parent, &unused, &detail); err != nil {
				_ = plan.Close()
				t.Fatal(err)
			}
			details = append(details, fmt.Sprintf("%d/%d %s", id, parent, detail))
		}
		err = plan.Err()
		_ = plan.Close()
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("11250 common matching passages; selected session45; retained refs20; continuation=%v reverse=%v\n%s", token != "", cursor.Reverse, strings.Join(details, "\n"))
		joined := strings.Join(details, " ")
		if !strings.Contains(joined, "VIRTUAL TABLE INDEX 0:M2><") || !strings.Contains(joined, "search_membership (session=?)") {
			t.Fatal("production match query lost membership bounds or its single range-constrained FTS cursor", details)
		}
	}
}
