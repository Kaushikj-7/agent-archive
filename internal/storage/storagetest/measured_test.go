package storagetest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestMeasuredStoreCountsReaderExtensionsOnce(t *testing.T) {
	mem := NewMemoryStore()
	ctx := context.Background()
	if err := mem.Put(ctx, "sessions/a", []byte("abc")); err != nil {
		t.Fatal(err)
	}
	s := NewMeasuredStore(mem, 0)
	if _, err := s.Get(ctx, "sessions/a"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.GetVersioned(ctx, "sessions/a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetLimited(ctx, "sessions/a", 3); err != nil {
		t.Fatal(err)
	}
	objects, err := s.List(ctx, "sessions")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ListRange(ctx, "sessions", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ListPage(ctx, "sessions", "", 1); err != nil {
		t.Fatal(err)
	}
	got := s.Metrics()
	if got.Gets != 3 || got.Lists != 3 || got.Bytes != 9 || got.PeakReads != 1 || got.ListBytes != int64(3*(len(objects[0].Key)+len(objects[0].ETag)+16)) {
		t.Fatalf("metrics %#v", got)
	}
	s.Reset()
	if s.Metrics() != (ReadMetrics{}) {
		t.Fatal("reset retained metrics")
	}
}

func TestMeasuredStoreJoinsConcurrentCancelledReads(t *testing.T) {
	// Long delay would hang without context cancellation; cancellation returns
	// zero body bytes, while attempted requests still count.
	s := NewMeasuredStore(NewMemoryStore(), time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			_, err := s.Get(ctx, "missing")
			if !errors.Is(err, context.Canceled) {
				t.Errorf("read error %v", err)
			}
		})
	}
	// Wait on observed starts, not an arbitrary sleep.
	deadline := time.After(5 * time.Second)
	for s.Metrics().Gets < 8 {
		select {
		case <-deadline:
			cancel()
			wg.Wait()
			t.Fatal("reads did not start")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	wg.Wait()
	got := s.Metrics()
	if got.Gets != 8 || got.PeakReads != 8 || got.Bytes != 0 {
		t.Fatalf("metrics %#v", got)
	}
	s.Reset()
}
