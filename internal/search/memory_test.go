package search

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/storage"
)

func TestMemoryStoreSearchesWithoutFilesAndDropsUnmatchedText(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	s, err := OpenMemory(t.Context(), "test/context")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	var mode int
	if err = s.db.QueryRowContext(t.Context(), "PRAGMA temp_store").Scan(&mode); err != nil || mode != 2 {
		t.Fatalf("temp_store %d %v, want memory", mode, err)
	}
	for id, text := range map[string]string{"hit": "checksum mismatch in upload", "miss": "nothing relevant here"} {
		m, p := testProjection(id, text)
		if _, err = s.Replace(t.Context(), m, []Projection{p}); err != nil {
			t.Fatal(err)
		}
	}
	q := Query{Text: "checksum"}
	for id, want := range map[string]int{"hit": 1, "miss": 0} {
		if got, e := s.SessionMatches(t.Context(), q, "codex", id); e != nil || got != want {
			t.Fatalf("%s matches %d %v, want %d", id, got, e, want)
		}
	}
	if err = s.DropSessionText(t.Context(), "codex", "miss", false); err != nil {
		t.Fatal(err)
	}
	if err = s.DropSessionText(t.Context(), "codex", "hit", true); err != nil {
		t.Fatal(err)
	}
	var passages, sessions int
	if err = s.db.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM search_passages WHERE session='codex/miss'),(SELECT count(*) FROM search_sessions)").Scan(&passages, &sessions); err != nil || passages != 0 || sessions != 2 {
		t.Fatalf("after drop: %d passages of the unmatched session, %d sessions, %v", passages, sessions, err)
	}
	page, err := s.Search(t.Context(), q)
	if err != nil || len(page.Hits) != 1 || page.Hits[0].SessionID != "hit" || page.Hits[0].Reference == nil {
		t.Fatalf("search after drop: %+v %v", page, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("memory store wrote %v %v", entries, err)
	}
}

// A session's sources together stay within one session's limits, however
// many there are: the totals carry from each source to the next.
func TestSessionTotalsSpanSources(t *testing.T) {
	t.Parallel()
	_, p := testProjection("s", "")
	p.Passages = make([]Passage, maxSessionPassages/2+1)
	var totals sessionTotals
	if err := totals.add(p); err != nil {
		t.Fatal(err)
	}
	if err := totals.add(p); !errors.Is(err, storage.ErrObjectTooLarge) {
		t.Fatalf("second source past the passage limit: %v", err)
	}
	totals = sessionTotals{text: maxSessionBytes - 1}
	_, p = testProjection("s", "ab")
	if err := totals.add(p); !errors.Is(err, storage.ErrObjectTooLarge) {
		t.Fatalf("text past the limit: %v", err)
	}
}

// Size counts the text a store in memory keeps, and stops counting it once
// dropped.
func TestMemoryStoreSizeFollowsKeptText(t *testing.T) {
	t.Parallel()
	s, err := OpenMemory(t.Context(), "test/context")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	empty, err := s.Size(t.Context())
	if err != nil || empty <= 0 {
		t.Fatalf("empty store size %d %v", empty, err)
	}
	m, p := testProjection("large", "")
	p.Passages = nil
	text := strings.Repeat("checksum mismatch in the upload ", 64)
	for i := range 1 << 9 {
		p.Passages = append(p.Passages, Passage{Reference: Reference{Source: p.Source, Record: i, Start: 0, End: len(text)}, Role: "user", Text: text})
	}
	if _, err = s.Replace(t.Context(), m, []Projection{p}); err != nil {
		t.Fatal(err)
	}
	kept, err := s.Size(t.Context())
	if err != nil || kept-empty < 1<<20 {
		t.Fatalf("size with 1 MiB of text kept: %d, empty %d, %v", kept, empty, err)
	}
	if err = s.DropSessionText(t.Context(), "codex", "large", false); err != nil {
		t.Fatal(err)
	}
	dropped, err := s.Size(t.Context())
	if err != nil || dropped-empty >= 1<<20 {
		t.Fatalf("size after the text was dropped: %d, empty %d, %v", dropped, empty, err)
	}
}
