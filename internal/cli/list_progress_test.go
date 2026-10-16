package cli

import (
	"bytes"
	"context"
	"testing"

	"github.com/wangjohn/agent-archive/internal/storage"
)

func TestListProgressCountsEachSessionOnce(t *testing.T) {
	p := newListProgress()
	if got := p.label("Listing sessions…"); got != "Listing sessions…" {
		t.Fatalf("label before anything = %q", got)
	}
	ctx := withListProgress(context.Background(), p)
	page := []storage.Object{
		{Key: "sessions/claude/a/metadata.json"},
		{Key: "sessions/claude/a/source/1.jsonl.zst"},
		{Key: "sessions/codex/b/metadata.json"},
		{Key: "listing-index/v2/sessions/claude/c/metadata.json"},
	}
	storage.ObserveListPage(ctx, page)
	// A second listing of the same prefix does not count them again.
	storage.ObserveListPage(ctx, page[:1])
	if got := p.label("Listing sessions…"); got != "Listing sessions… scanned 2" {
		t.Fatalf("label after listing = %q", got)
	}

	read := listReadProgress(ctx)
	if read == nil {
		t.Fatal("listReadProgress found no progress on ctx")
	}
	// Reads report out of order; the highest done stays.
	read(1200, 1500)
	read(900, 1500)
	if got := p.label("Listing sessions…"); got != "Listing sessions… loading 1,200 of 1,500" {
		t.Fatalf("label while reading = %q", got)
	}
	if listReadProgress(context.Background()) != nil {
		t.Fatal("listReadProgress without progress on ctx is not nil")
	}
}

func TestPausingWriterStopsBeforeWritingAndUnwraps(t *testing.T) {
	var out bytes.Buffer
	stops := 0
	w := &pausingWriter{w: &out, stop: func() {
		if out.Len() != 0 {
			t.Error("stop ran after the write")
		}
		stops++
	}}
	if _, err := w.Write([]byte("note\n")); err != nil {
		t.Fatal(err)
	}
	if stops != 1 || out.String() != "note\n" {
		t.Fatalf("stops = %d, out = %q", stops, out.String())
	}
	if underlyingWriter(w) != &out {
		t.Fatal("underlyingWriter does not unwrap a pausingWriter")
	}
}
