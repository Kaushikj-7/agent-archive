package storage

import (
	"context"
	"slices"
	"testing"
)

func TestS3StoreListsReportEachPageToTheObserver(t *testing.T) {
	fake := &pagingS3{keys: rangeTestKeys(7), pageSize: 3}
	store := newPagingStore(t, fake)
	// Without an observer a listing reports nothing and still succeeds.
	if _, err := store.List(context.Background(), "sessions"); err != nil {
		t.Fatal(err)
	}
	var pages [][]string
	ctx := WithListObserver(context.Background(), func(objects []Object) { pages = append(pages, objectKeys(objects)) })

	if _, err := store.List(ctx, "sessions"); err != nil {
		t.Fatal(err)
	}
	if len(pages) != 3 || len(pages[0]) != 3 || len(pages[1]) != 3 || len(pages[2]) != 1 {
		t.Fatalf("List pages = %v, want pages of 3, 3 and 1", pages)
	}

	// A range reports only its own keys, including the last page it stops
	// partway through.
	pages = nil
	got, err := store.ListRange(ctx, "sessions", "sessions/claude/k01", "sessions/claude/k05")
	if err != nil {
		t.Fatal(err)
	}
	if reported := slices.Concat(pages...); !slices.Equal(reported, objectKeys(got)) {
		t.Fatalf("ListRange reported %v, want exactly its result %v", reported, objectKeys(got))
	}
}
