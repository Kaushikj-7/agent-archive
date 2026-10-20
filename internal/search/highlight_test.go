package search

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestHitsAndMatchesCarryTheIndexedPassage(t *testing.T) {
	s := openTest(t)
	m, p := testProjection("s1", "first checksum passage")
	second := p.Passages[0]
	second.Reference.Record, second.Role, second.Text = 1, "assistant", "second checksum reply"
	second.Reference.End = len(second.Text)
	p.Passages = append(p.Passages, second)
	m.Name = "Checksum title"
	if _, err := s.Replace(t.Context(), m, []Projection{p}); err != nil {
		t.Fatal(err)
	}
	page, err := s.Search(t.Context(), Query{Text: "checksum"})
	if err != nil || len(page.Hits) != 1 {
		t.Fatalf("%+v %v", page, err)
	}
	if hit := page.Hits[0]; hit.Reference != nil || hit.Passage != "Checksum title" || hit.Project != "" {
		t.Fatalf("title match passage: %+v", hit)
	}
	page, err = s.Search(t.Context(), Query{Text: "reply"})
	if err != nil || len(page.Hits) != 1 || page.Hits[0].Passage != "second checksum reply" || page.Hits[0].Role != "assistant" {
		t.Fatalf("body match passage: %+v %v", page, err)
	}
	matches, err := s.Matches(t.Context(), Query{Text: "checksum"}, "codex", "s1", page.Generation, "")
	if err != nil || len(matches.Passages) != len(matches.References) || len(matches.Passages) != 2 {
		t.Fatalf("%+v %v", matches, err)
	}
	for i, passage := range matches.Passages {
		if passage.Reference != matches.References[i] || !strings.Contains(passage.Text, "checksum") || passage.Role == "" {
			t.Fatalf("passage %d does not match its reference: %+v", i, passage)
		}
	}
}

func spanTexts(text string, spans []Span) []string {
	out := []string{}
	for _, s := range spans {
		out = append(out, text[s.Start:s.End])
	}
	return out
}

func TestMatchSpansFollowIndexTokens(t *testing.T) {
	for _, tc := range []struct {
		query string
		text  string
		want  []string
	}{
		{"read performance", "Improve the READ performance; reading is slow.", []string{"READ", "performance"}},
		{"perf*", "Performance, perfect, and superperf.", []string{"Performance", "perfect"}},
		{`"read performance"`, "read the performance read performance", []string{"read performance"}},
		{`"read perf"*`, "read performance and read, perfect", []string{"read performance", "read, perfect"}},
		{"foo_bar.go", "edit foo_bar.go and foo bar go, not foobar.go", []string{"foo_bar.go", "foo bar go"}},
		{"café", "Café and cafe", []string{"Café"}},
		{"界", "世界 界", []string{"界"}},
		{"needle", "no match here", []string{}},
		{"needle needle", "needle", []string{"needle"}},
		{`"unterminated`, "unterminated", []string{}},
		{"aa", "aaa aa aa", []string{"aa", "aa"}},
	} {
		got := spanTexts(tc.text, MatchSpans(tc.query, tc.text))
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("MatchSpans(%q, %q) = %q, want %q", tc.query, tc.text, got, tc.want)
		}
	}
}

func TestMatchSpansJoinOverlaps(t *testing.T) {
	text := "read performance"
	spans := MatchSpans(`"read performance" performance read`, text)
	if len(spans) != 1 || spans[0] != (Span{0, len(text)}) {
		t.Fatalf("overlaps not joined: %+v", spans)
	}
}

// ftsMatches opens an in-memory table with the index's tokenizer, so the
// tests compare MatchSpans with what the index itself matches. ok is false
// when the index refuses the expression (a NUL ends SQLite's text early),
// which a search reports as an error rather than highlighting anything.
func ftsMatches(tb testing.TB) func(query, text string) (matched, ok bool) {
	tb.Helper()
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		tb.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	tb.Cleanup(func() { _ = db.Close() })
	if _, err = db.ExecContext(ctx, "CREATE VIRTUAL TABLE f USING fts5(body,tokenize='unicode61 remove_diacritics 0')"); err != nil {
		tb.Fatal(err)
	}
	return func(query, text string) (bool, bool) {
		expression, err := MatchExpression(query)
		if err != nil {
			return false, true
		}
		if _, err = db.ExecContext(ctx, "DELETE FROM f"); err != nil {
			return false, false
		}
		if _, err = db.ExecContext(ctx, "INSERT INTO f(rowid,body) VALUES(1,?)", text); err != nil {
			return false, false
		}
		var n int
		if err = db.QueryRowContext(ctx, "SELECT count(*) FROM f WHERE f MATCH ?", expression).Scan(&n); err != nil {
			return false, false
		}
		return n > 0, true
	}
}

// everyTermSpans reports whether each term of query has a span of its own in
// text: what an ANDed index match of the whole query needs.
func everyTermSpans(query, text string) bool {
	terms, err := parseTerms(query)
	if err != nil {
		return false
	}
	for _, term := range terms {
		single := `"` + term.text + `"`
		if term.prefix {
			single += "*"
		}
		if len(MatchSpans(single, text)) == 0 {
			return false
		}
	}
	return true
}

func TestMatchSpansAgreeWithTheIndex(t *testing.T) {
	matches := ftsMatches(t)
	texts := []string{
		"Improve the read performance of list and show.",
		"READ-PERFORMANCE: foo_bar.go, perfect",
		"Café crème, CAFÉ, cafe",
		"decomposed café and ̈x",
		"世界 界 ｆｕｌｌ width",
		"tab\tseparated\nlines and ünïcödé",
		"x1 y2 z_3 3.14 v2.0",
		"emoji 🙂 between words",
	}
	queries := []string{"read", "read performance", `"read performance"`, "perf*", "foo_bar.go", "café", "cafe", "界", "ｆｕｌｌ", "full", "separated lines", "ünïcödé", "3.14", "v2", "z_3", `"between words"`, "words*", "nothing"}
	for _, text := range texts {
		for _, query := range queries {
			want, ok := matches(query, text)
			if !ok {
				t.Fatalf("index refused %q", query)
			}
			if got := everyTermSpans(query, text); got != want {
				t.Errorf("query %q text %q: spans %v, index %v", query, text, got, want)
			}
		}
	}
}

// sharedTokenRange reports whether r is in the ranges where Go's Unicode
// tables and the index's Unicode 6.1 tables classify every character alike:
// the Latin, Greek and Cyrillic blocks, and the CJK unified ideographs.
func sharedTokenRange(r rune) bool {
	return r > 0 && r < 0x580 || r >= 0x4E00 && r <= 0x9FFF
}

func TestTokenCharactersMatchTheIndex(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()
	if _, err = db.ExecContext(t.Context(), "CREATE VIRTUAL TABLE f USING fts5(body,tokenize='unicode61 remove_diacritics 0')"); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var runes []rune
	for r := rune(1); r <= 0x9FFF; r++ {
		if !sharedTokenRange(r) {
			continue
		}
		runes = append(runes, r)
		// "qz" alone matches only where r separates two tokens.
		if _, err = tx.ExecContext(t.Context(), "INSERT INTO f(rowid,body) VALUES(?,?)", int64(r), "qz"+string(r)+"qz"); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	rows, err := db.QueryContext(t.Context(), "SELECT rowid FROM f WHERE f MATCH 'qz'")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	separates := map[rune]bool{}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		separates[rune(id)] = true
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, r := range runes {
		if isTokenRune(r) == separates[r] {
			t.Errorf("%U: token %v here, separator %v in the index", r, isTokenRune(r), separates[r])
		}
	}
}

func FuzzMatchSpansAgreeWithTheIndex(f *testing.F) {
	f.Add("read performance", "Improve the READ performance.")
	f.Add(`"foo bar"*`, "foo_barn")
	f.Add("café", "CAFÉ cafe")
	f.Add("x*", "\x00x\u202ey")
	matches := ftsMatches(f)
	f.Fuzz(func(t *testing.T, query, text string) {
		if !utf8.ValidString(text) || len(text) > 2048 {
			return
		}
		spans := MatchSpans(query, text)
		end := -1
		for _, s := range spans {
			if s.Start <= end || s.End <= s.Start || s.End > len(text) || !utf8.RuneStart(text[s.Start]) || s.End < len(text) && !utf8.RuneStart(text[s.End]) {
				t.Fatalf("bad spans %+v for %q", spans, text)
			}
			end = s.End
		}
		if strings.ContainsFunc(query+text, func(r rune) bool { return !sharedTokenRange(r) }) {
			return
		}
		want, ok := matches(query, text)
		if !ok {
			return
		}
		if got := everyTermSpans(query, text); got != want {
			t.Fatalf("query %q text %q: spans %v, index %v", query, text, got, want)
		}
	})
}
