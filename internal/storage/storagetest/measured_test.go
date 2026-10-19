package storagetest

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/storage"
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

type qualifiedMeasuredFixture struct{ *MemoryStore }

func (*qualifiedMeasuredFixture) CatalogAtomicQualification() error { return nil }

func TestMeasuredStoreForwardsAtomicCapabilitiesAndCountsBoundedReads(t *testing.T) {
	underlying := &qualifiedMeasuredFixture{NewMemoryStore()}
	measured := NewMeasuredStore(underlying, 0)
	if err := measured.CatalogAtomicQualification(); err != nil {
		t.Fatal("qualification lost", err)
	}
	etag, err := measured.PutConditional(t.Context(), "synthetic", []byte("abc"), storage.PutCondition{CreateOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = measured.PutConditional(t.Context(), "synthetic", []byte("def"), storage.PutCondition{CreateOnly: true}); !errors.Is(err, storage.ErrPreconditionFailed) {
		t.Fatal(err)
	}
	b, gotTag, err := measured.GetLimitedVersioned(t.Context(), "synthetic", 3)
	if err != nil || gotTag != etag || string(b) != "abc" {
		t.Fatal(err)
	}
	if _, err = measured.Stat(t.Context(), "synthetic"); err != nil {
		t.Fatal(err)
	}
	if got := measured.Metrics(); got.Gets != 1 || got.Bytes != 3 || got.Lists != 0 {
		t.Fatal("conditional writes or HEAD counted as GET", got)
	}
	if measured.CatalogMetadataAuthority() {
		t.Fatal("raw qualified store invented metadata authority")
	}
	unqualified := NewMeasuredStore(NewMemoryStore(), 0)
	if !errors.Is(unqualified.CatalogAtomicQualification(), storage.ErrAtomicCatalogUnqualified) {
		t.Fatal("memory fixture invented qualification")
	}
	// Existing benchmarks replace this field between iterations.
	replacement := NewMemoryStore()
	unqualified.MemoryStore = replacement
	if err = unqualified.Put(t.Context(), "replacement", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err = replacement.Get(t.Context(), "replacement"); err != nil {
		t.Fatal("legacy fixture replacement lost", err)
	}
}

func TestMeasuredListingCostsCaptureIsOptionalPrivateAndReset(t *testing.T) {
	t.Parallel()
	mem := NewMemoryStore()
	if err := mem.Put(t.Context(), "sessions/a", []byte("body")); err != nil {
		t.Fatal(err)
	}
	measured := NewMeasuredStore(mem, 0)
	measured.ListingModel = &ListingLatencyModel{}
	if _, err := measured.List(t.Context(), "sessions/"); err != nil {
		t.Fatal(err)
	}
	if len(measured.ListingCosts()) != 0 {
		t.Fatal("timed default retained allocating request records")
	}
	measured.Reset()
	measured.CaptureListingCosts = true
	if _, err := measured.ListRange(t.Context(), "sessions/", "", "sessions/a"); err != nil {
		t.Fatal(err)
	}
	costs := measured.ListingCosts()
	if len(costs) != 1 || costs[0].Prefix != "sessions/" || costs[0].Through != "sessions/a" || costs[0].Cost.Pages != 1 || costs[0].Cost.Keys != 1 || measured.Metrics().Lists != 1 {
		t.Fatal("incorrect recorded service", costs, measured.Metrics())
	}
	costs[0].Cost.Pages = 100
	if measured.ListingCosts()[0].Cost.Pages != 1 {
		t.Fatal("caller changed recorded service")
	}
	measured.Reset()
	if len(measured.ListingCosts()) != 0 || measured.Metrics() != (ReadMetrics{}) {
		t.Fatal("reset retained request costs")
	}
	if costs[0].Cost.Pages != 100 {
		t.Fatal("reset changed prior private snapshot")
	}
}

func TestMeasuredSuccessfulGETKeysAreOptionalPrivateAndReset(t *testing.T) {
	t.Parallel()
	mem := NewMemoryStore()
	for _, key := range []string{"ordinary", "versioned", "bounded"} {
		if err := mem.Put(t.Context(), key, []byte("body")); err != nil {
			t.Fatal(err)
		}
	}
	measured := NewMeasuredStore(mem, 0)
	if _, err := measured.Get(t.Context(), "ordinary"); err != nil {
		t.Fatal(err)
	}
	if len(measured.SuccessfulGETKeys()) != 0 {
		t.Fatal("timed default retained body identities")
	}
	measured.Reset()
	measured.CaptureGETKeys = true
	if _, err := measured.Get(t.Context(), "ordinary"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := measured.GetVersioned(t.Context(), "versioned"); err != nil {
		t.Fatal(err)
	}
	if _, err := measured.GetLimited(t.Context(), "bounded", 4); err != nil {
		t.Fatal(err)
	}
	if _, err := measured.Get(t.Context(), "ordinary"); err != nil {
		t.Fatal(err)
	}
	if _, err := measured.Get(t.Context(), "missing"); err == nil {
		t.Fatal("missing fixture read unexpectedly succeeded")
	}
	if _, err := measured.GetLimited(t.Context(), "bounded", 1); err == nil {
		t.Fatal("oversized fixture read unexpectedly succeeded")
	}
	keys := measured.SuccessfulGETKeys()
	if !slices.Equal(keys, []string{"ordinary", "versioned", "bounded", "ordinary"}) {
		t.Fatal("successful variant/duplicate capture incorrect", keys)
	}
	keys[0] = "caller-owned"
	if measured.SuccessfulGETKeys()[0] != "ordinary" {
		t.Fatal("caller changed captured body identities")
	}
	measured.Reset()
	if len(measured.SuccessfulGETKeys()) != 0 || measured.Metrics() != (ReadMetrics{}) {
		t.Fatal("reset retained body capture")
	}
	if keys[0] != "caller-owned" {
		t.Fatal("reset changed prior snapshot")
	}
}

type partialErrorGETStore struct{ *MemoryStore }

var errSyntheticPartialGET = errors.New("synthetic incomplete GET")

func (*partialErrorGETStore) Get(context.Context, string) ([]byte, error) {
	return []byte("partial"), errSyntheticPartialGET
}

func (*partialErrorGETStore) GetVersioned(context.Context, string) ([]byte, string, error) {
	return []byte("partial"), "synthetic-etag", errSyntheticPartialGET
}

func (*partialErrorGETStore) GetLimited(context.Context, string, int64) ([]byte, error) {
	return []byte("partial"), errSyntheticPartialGET
}

func TestMeasuredSuccessfulGETBytesExcludePartialFailures(t *testing.T) {
	t.Parallel()
	measured := NewMeasuredStore(&partialErrorGETStore{NewMemoryStore()}, 0)
	measured.CaptureGETKeys = true
	if data, err := measured.Get(t.Context(), "ordinary"); len(data) == 0 || !errors.Is(err, errSyntheticPartialGET) {
		t.Fatal("invalid partial ordinary fixture", len(data), err)
	}
	if data, _, err := measured.GetVersioned(t.Context(), "versioned"); len(data) == 0 || !errors.Is(err, errSyntheticPartialGET) {
		t.Fatal("invalid partial versioned fixture", len(data), err)
	}
	if data, err := measured.GetLimited(t.Context(), "bounded", 100); len(data) == 0 || !errors.Is(err, errSyntheticPartialGET) {
		t.Fatal("invalid partial bounded fixture", len(data), err)
	}
	if got := measured.Metrics(); got.Gets != 3 || got.Bytes != 0 || len(measured.SuccessfulGETKeys()) != 0 {
		t.Fatal("partial failures counted as successful bodies", got, measured.SuccessfulGETKeys())
	}
	measured.Reset() // Every failed read must release its active lease.
}
