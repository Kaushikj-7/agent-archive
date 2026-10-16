package state

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

func summaryCacheFixture(t *testing.T) (*Store, string) {
	t.Helper()
	s := newTestStore(t)
	p := publicationFixture(t, publicationThread, time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC))
	p.History = &PendingHistory{Version: 1}
	ready, err := PreparePublicationV2(p, PublicationPredecessor{State: PredecessorAbsent}, "destination", "admission", "policy", PublicationCapture)
	if err != nil {
		t.Fatal(err)
	}
	published, err := s.LoadPublishedState(p.Bundle.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err = published.SaveCommittedPublication(ready, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(s.summaryCache.entries) != 1 {
		t.Fatal("validated write did not seed cache")
	}
	return s, p.Bundle.ArchiveSessionID
}

func TestPublishedSummaryCacheWarmColdAndPassOwnership(t *testing.T) {
	s, id := summaryCacheFixture(t)
	info, err := os.Stat(s.publishedPath(id))
	if err != nil {
		t.Fatal(err)
	}
	before, hashed := PublishedStateLoads(), SummaryValidationBytes()
	for range 2 {
		budget := agentapi.NewNativeReadBudget(2 << 20)
		read, closeRead := s.WithReadBudget(t.Context(), budget)
		summary, found, err := read.LoadPublishedSummary(id)
		if err != nil || !found || !summary.Published {
			t.Fatal(found, err)
		}
		used, _ := budget.Charged()
		if used < s.summaryCache.bytes {
			t.Fatalf("cache not borrowed: %d < %d", used, s.summaryCache.bytes)
		}
		closeRead()
		closeRead()
		if used, _ = budget.Charged(); used != 0 {
			t.Fatal("pass leaked cache or summary charge", used)
		}
	}
	if PublishedStateLoads() != before {
		t.Fatal("warm summary decoded full state")
	}
	if SummaryValidationBytes()-hashed < 2*info.Size() {
		t.Fatal("cache hit did not hash complete actual file")
	}
	cold := OpenReadOnly(s.Home())
	if _, found, err := cold.LoadPublishedSummary(id); err != nil || !found {
		t.Fatal(found, err)
	}
	if PublishedStateLoads() != before+1 {
		t.Fatal("cold restart reused prior Store authority")
	}
	if _, _, err := cold.LoadPublishedSummary(id); err != nil {
		t.Fatal(err)
	}
	if PublishedStateLoads() != before+1 {
		t.Fatal("validated cold result was not cached")
	}
}

func TestPublishedSummaryCacheRefusesSameStampChangedAuthority(t *testing.T) {
	for _, configuration := range []bool{false, true} {
		t.Run(map[bool]string{false: "body", true: "config"}[configuration], func(t *testing.T) {
			s, id := summaryCacheFixture(t)
			path := s.publishedPath(id)
			if configuration {
				path = filepath.Join(s.Home(), "config.json")
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			old, next := []byte(`"publication_version":5`), []byte(`"publication_version":9`)
			if configuration {
				old, next = []byte("source-set-publication-v17"), []byte("source-set-publication-v99")
			}
			changed := bytes.Replace(raw, old, next, 1)
			if bytes.Equal(raw, changed) || len(raw) != len(changed) {
				t.Fatal("mutation did not preserve size")
			}
			if err = os.WriteFile(path, changed, 0600); err != nil {
				t.Fatal(err)
			}
			if err = os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
			if _, _, err = s.LoadPublishedSummary(id); !errors.Is(err, ErrDurableStorageRecovery) {
				t.Fatal("changed authority reused cached summary", err)
			}
		})
	}
}

func TestPublishedSummaryCacheDeepOwnershipEvictionAndScopeRefusal(t *testing.T) {
	s, id := summaryCacheFixture(t)
	e := s.summaryCache.entries[0]
	original := e.summary
	original.LinkedPublished = []string{"child"}
	original.CapturedAt = original.CapturedAt.In(time.FixedZone(strings.Repeat("zone", 2048), 3600))
	s.cacheSummary(id, original, e.sum, e.config, e.home)
	original.LinkedPublished[0] = "caller-poison"
	got, found, err := s.cachedSummary(id, e.sum, e.config, e.home)
	if err != nil || !found || len(got.LinkedPublished) != 1 || got.LinkedPublished[0] != "child" {
		t.Fatal(got, found, err)
	}
	if !got.CapturedAt.Equal(original.CapturedAt) || got.CapturedAt.Location() != time.UTC {
		t.Fatal("cache retained caller-owned timezone graph or changed timestamp")
	}
	got.LinkedPublished[0] = "return-poison"
	got, found, err = s.cachedSummary(id, e.sum, e.config, e.home)
	if err != nil || !found || got.LinkedPublished[0] != "child" {
		t.Fatal("returned summary aliases cache", got, err)
	}
	for i := range 100 {
		s.cacheSummary(strings.Repeat("x", i+1), e.summary, e.sum, e.config, e.home)
	}
	if s.summaryCache.bytes > publishedSummaryCacheLimit || len(s.summaryCache.entries) > publishedSummaryCacheEntries {
		t.Fatal("unbounded cache")
	}
	if _, found, _ = s.cachedSummary(id, e.sum, e.config, e.home); found {
		t.Fatal("LRU did not evict oldest entry")
	}
	s.cacheSummary(id, e.summary, e.sum, e.config, e.home)
	budget := agentapi.NewNativeReadBudget(2 << 20)
	read, closeRead := s.WithReadBudget(t.Context(), budget)
	// A later pass must borrow ALL retained entries, not just the requested row.
	held := (2 << 20) - s.summaryCache.bytes + 1
	hold := budget.Reservation()
	if !hold.Reserve(held) {
		t.Fatal("fixture reservation")
	}
	if _, found, err = read.cachedSummary(id, e.sum, e.config, e.home); err != nil || found || s.summaryCache.bytes != 0 {
		t.Fatal("unchargeable cache not evicted", found, err)
	}
	closeRead()
	hold.Release(held)
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("eviction leaked pass charges", used)
	}
}

func TestPublishedSummaryCacheDelayedReleaseCannotCreditNewScope(t *testing.T) {
	s, id := summaryCacheFixture(t)
	e := s.summaryCache.entries[0]
	budget := agentapi.NewNativeReadBudget(2 << 20)
	endA := budget.BeginScope(1 << 20)
	read, closeRead := s.WithReadBudget(t.Context(), budget)
	if _, found, err := read.cachedSummary(id, e.sum, e.config, e.home); err != nil || !found {
		t.Fatal(found, err)
	}
	endA()
	endB := budget.BeginScope(1 << 20)
	defer endB()
	ownerB := budget.Reservation()
	if !ownerB.Reserve(1 << 20) {
		t.Fatal("fill scope B")
	}
	defer ownerB.Release(1 << 20)
	closeRead()
	closeRead()
	if ownerB.Available() != 0 {
		t.Fatal("old cache release credited new scope", ownerB.Available())
	}
}

func TestPublishedSummaryCacheCanceledAndReplacedRoot(t *testing.T) {
	s, id := summaryCacheFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	budget := agentapi.NewNativeReadBudget(1 << 20)
	read, closeRead := s.WithReadBudget(ctx, budget)
	beforeCanceled := PublishedStateLoads()
	if _, _, err := read.LoadPublishedSummary(id); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled hit succeeded", err)
	}
	closeRead()
	if used, _ := budget.Charged(); used != 0 || PublishedStateLoads() != beforeCanceled {
		t.Fatal("canceled cache read retained work", used)
	}
	oldHome := s.Home() + "-old"
	if err := os.Rename(s.Home(), oldHome); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.RemoveAll(oldHome); err != nil {
			t.Error(err)
		}
	}()
	if err := os.MkdirAll(filepath.Join(s.Home(), "published"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"config.json", filepath.Join("published", id+".json")} {
		raw, err := os.ReadFile(filepath.Join(oldHome, path))
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(s.Home(), path), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	before := PublishedStateLoads()
	if _, found, err := s.LoadPublishedSummary(id); err != nil || !found {
		t.Fatal(found, err)
	}
	if PublishedStateLoads() != before+1 {
		t.Fatal("replaced root inherited old cache authority")
	}
}

func TestPublishedSummaryCacheDigestBelongsToDecoderBytes(t *testing.T) {
	s, id := summaryCacheFixture(t)
	s.summaryCache.clear()
	path := s.publishedPath(id)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	a, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b := bytes.Replace(a, []byte(`"publication_version":5`), []byte(`"publication_version":9`), 1)
	if bytes.Equal(a, b) {
		t.Fatal("no-op mutation")
	}
	write := func(raw []byte) {
		t.Helper()
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
			t.Fatal(err)
		}
	}
	write(b) // outer hash sees B
	s.summaryDecodeBoundary = func(after bool) {
		if after {
			write(b)
		} else {
			write(a)
		}
	}
	// A is valid and decoded between two identical observations of B. A cache
	// bound to either surrounding hash rather than decoder bytes would bless B.
	if _, _, err = s.LoadPublishedSummary(id); !errors.Is(err, ErrDurableStorageRecovery) {
		t.Fatal("decoder/hash A-B swap accepted", err)
	}
	if len(s.summaryCache.entries) != 0 {
		t.Fatal("failed decode binding seeded cache")
	}
	s.summaryDecodeBoundary = nil
	if _, _, err = s.LoadPublishedSummary(id); !errors.Is(err, ErrDurableStorageRecovery) {
		t.Fatal("poisoned cache licensed future wire", err)
	}
}
