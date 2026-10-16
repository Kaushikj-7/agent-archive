package state

import (
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

// This Store-owned heap cache has a lifetime independent of a collector pass.
// Its finite allowance is not extra source-data capacity: each read scope must
// borrow all retained bytes from its own captured ledger before using the cache.
const publishedSummaryCacheLimit int64 = 128 << 10

const publishedSummaryCacheEntries = 64

type publishedSummaryEntry struct {
	id      string
	sum     [32]byte
	config  [32]byte
	home    os.FileInfo
	summary PublishedSummary
	bytes   int64
}

type publishedSummaryCache struct {
	mu      sync.Mutex
	entries []publishedSummaryEntry // least recently used first
	bytes   int64
}

type publishedSummaryBorrow struct {
	n     int64
	owner stateReservation
}

var summaryValidationBytes atomic.Int64

func summaryOwnedBytes(id string, p PublishedSummary) int64 {
	n := int64(1024 + len(id) + len(p.LabelSourceChecksum) + len(p.SourceSetDigest) + len(p.CurrentRevision) + len(p.Harness) + len(p.Status) + len(p.BlockedReason))
	for _, link := range p.LinkedPublished {
		n += int64(32 + len(link))
	}
	return 2 * n // owned strings, slice slots, file identity and bounded bookkeeping
}

func clonePublishedSummary(p PublishedSummary) PublishedSummary {
	// A time.Location may own arbitrary zone tables or a caller-supplied name.
	// Retain only timestamp instants, with no pointer into that caller's graph.
	p.MeaningfulCapturedAt = p.MeaningfulCapturedAt.UTC()
	p.CapturedAt = p.CapturedAt.UTC()
	p.AgeFrom = p.AgeFrom.UTC()
	p.LastPublishedAt = p.LastPublishedAt.UTC()
	p.LabelSourceChecksum = strings.Clone(p.LabelSourceChecksum)
	p.SourceSetDigest = strings.Clone(p.SourceSetDigest)
	p.CurrentRevision = strings.Clone(p.CurrentRevision)
	p.Harness = strings.Clone(p.Harness)
	p.Status = CacheStatus(strings.Clone(string(p.Status)))
	p.BlockedReason = BlockedReason(strings.Clone(string(p.BlockedReason)))
	if p.LinkedPublished != nil {
		links := make([]string, len(p.LinkedPublished))
		for i, v := range p.LinkedPublished {
			links[i] = strings.Clone(v)
		}
		p.LinkedPublished = links
	}
	return p
}

func (s *Store) ownPublishedSummary(p PublishedSummary) (PublishedSummary, error) {
	n := summaryOwnedBytes("", p)
	owner := s.readReservation()
	if !owner.Reserve(n) {
		return PublishedSummary{}, errStateBudget
	}
	if s.resourceReleases != nil {
		*s.resourceReleases = append(*s.resourceReleases, func() { owner.Release(n) })
	} else {
		owner.Release(n)
	}
	return clonePublishedSummary(p), nil
}

// Cache borrowing ends with the existing read scope, never with a later pass's
// mutable reservation. Unscoped callers use the fixed Store-owned allowance.
func (s *Store) borrowPublishedSummaries(n int64) bool {
	if s.resourceBudget == nil {
		return true
	}
	b := s.summaryBorrow
	if b == nil || s.resourceReleases == nil {
		return false
	}
	if b.owner == nil {
		owner := s.readReservation()
		if !owner.Reserve(n) {
			return false
		}
		b.owner, b.n = owner, n
		*s.resourceReleases = append(*s.resourceReleases, func() { b.owner.Release(b.n); b.n = 0 })
		return true
	}
	if n > b.n {
		if !b.owner.Reserve(n - b.n) {
			return false
		}
		b.n = n
	}
	return true
}

func (c *publishedSummaryCache) clear() { c.entries = nil; c.bytes = 0 }

func (c *publishedSummaryCache) remove(i int) {
	c.bytes -= c.entries[i].bytes
	copy(c.entries[i:], c.entries[i+1:])
	c.entries[len(c.entries)-1] = publishedSummaryEntry{}
	c.entries = c.entries[:len(c.entries)-1]
}

func (s *Store) cacheSummary(id string, p PublishedSummary, sum, cfg [32]byte, home os.FileInfo) {
	c := s.summaryCache
	if c == nil {
		return
	}
	n := summaryOwnedBytes(id, p)
	if n > publishedSummaryCacheLimit {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.entries {
		if c.entries[i].id == id {
			c.remove(i)
			break
		}
	}
	for len(c.entries) > 0 && (len(c.entries) >= publishedSummaryCacheEntries || c.bytes+n > publishedSummaryCacheLimit) {
		c.remove(0)
	}
	if !s.borrowPublishedSummaries(c.bytes + n) {
		c.clear()
		return
	}
	c.entries = append(c.entries, publishedSummaryEntry{id: strings.Clone(id), summary: clonePublishedSummary(p), sum: sum, config: cfg, home: home, bytes: n})
	c.bytes += n
}

func (s *Store) cachedSummary(id string, sum, cfg [32]byte, home os.FileInfo) (PublishedSummary, bool, error) {
	c := s.summaryCache
	if c == nil {
		return PublishedSummary{}, false, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !s.borrowPublishedSummaries(c.bytes) {
		c.clear()
		return PublishedSummary{}, false, nil
	}
	for i, e := range c.entries {
		if e.id != id {
			continue
		}
		if e.sum != sum || e.config != cfg || !os.SameFile(e.home, home) {
			c.remove(i)
			return PublishedSummary{}, false, nil
		}
		// The returned view is owned by this read scope, independently of eviction
		// and independently of any caller mutating its linked-session slice.
		result, err := s.ownPublishedSummary(e.summary)
		if err != nil {
			return PublishedSummary{}, false, err
		}
		copy(c.entries[i:], c.entries[i+1:])
		c.entries[len(c.entries)-1] = e
		return result, true, nil
	}
	return PublishedSummary{}, false, nil
}

// summaryConfig observes the exact bytes validated by the normal configuration
// decoder. Same-stamp edits cannot inherit a previous configuration's authority.
func (s *Store) summaryConfig(home *local.RootedHome) (sum [32]byte, identity os.FileInfo, err error) {
	info, err := home.Root.Lstat("config.json")
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > durableStorageQuota {
		return sum, nil, errors.Join(ErrDurableStorageRecovery, err)
	}
	n := 3*info.Size() + (32 << 10)
	owner := s.readReservation()
	if !owner.Reserve(n) {
		return sum, nil, errStateBudget
	}
	defer owner.Release(n)
	cfg, found, sum, err := config.LoadRootedObservedBounded(home, info.Size())
	if err != nil || !found || !cfg.SourceSetPublicationProtection {
		return sum, nil, errors.Join(ErrDurableStorageRecovery, err)
	}
	identity, err = home.Root.Stat(".")
	return sum, identity, err
}

func (s *Store) summaryFileHash(home *local.RootedHome, path string, expected os.FileInfo) (sum [32]byte, err error) {
	if expected == nil || !expected.Mode().IsRegular() || expected.Size() < 0 || expected.Size() > durableStorageQuota {
		return sum, ErrDurableStorageRecovery
	}
	owner := s.readReservation()
	const scratch = 32 << 10
	if !owner.Reserve(scratch) {
		return sum, errStateBudget
	}
	defer owner.Release(scratch)
	f, err := home.Root.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return sum, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	opened, err := f.Stat()
	if err != nil || !sameDurableStamp(expected, opened) {
		return sum, errors.Join(ErrDurableStorageRecovery, err)
	}
	hash := sha256.New()
	buf := make([]byte, scratch)
	left := expected.Size()
	for left > 0 {
		if err = s.durableContext().Err(); err != nil {
			return sum, err
		}
		n, e := f.Read(buf[:min(int64(len(buf)), left)])
		if n > 0 {
			hash.Write(buf[:n])
			summaryValidationBytes.Add(int64(n))
			left -= int64(n)
		}
		if e != nil {
			return sum, errors.Join(ErrDurableStorageRecovery, e)
		}
		if n == 0 {
			return sum, io.ErrNoProgress
		}
	}
	var extra [1]byte
	if n, e := f.Read(extra[:]); n != 0 || !errors.Is(e, io.EOF) {
		return sum, errors.Join(ErrDurableStorageRecovery, e)
	}
	opened, err = f.Stat()
	named, e := home.Root.Lstat(path)
	if err != nil || e != nil || !sameDurableStamp(expected, opened) || !sameDurableStamp(expected, named) {
		return sum, errors.Join(ErrDurableStorageRecovery, err, e)
	}
	copy(sum[:], hash.Sum(nil))
	return sum, errors.Join(home.Check(), s.durableContext().Err())
}

// Successful source-set writes already validate the typed selecting state and
// supply the digest of precisely the encoded bytes. Seeding is optional; a
// failed cache admission never changes the completed durable write's result.
func (s *Store) cacheSavedSummary(id string, p PublishedSummary, sum [32]byte) {
	if s.summaryCache == nil {
		return
	}
	cfg, identity, valid := s.savedSummaryBinding(id, sum)
	if valid {
		s.cacheSummary(id, p, sum, cfg, identity)
	}
}

func (s *Store) savedSummaryBinding(id string, sum [32]byte) (cfg [32]byte, identity os.FileInfo, valid bool) {
	home, err := local.OpenRootedHome(s.home)
	if err != nil {
		return
	}
	defer func() {
		if err := home.Close(); err != nil {
			valid = false
		}
	}()
	cfg, identity, err = s.summaryConfig(home)
	if err != nil {
		return
	}
	path := filepath.Join("published", id+".json")
	info, err := home.Root.Lstat(path)
	if err != nil {
		return
	}
	actual, err := s.summaryFileHash(home, path, info)
	if err != nil || actual != sum {
		return
	}
	cfgAfter, _, err := s.summaryConfig(home)
	if err != nil || cfgAfter != cfg {
		return
	}
	return cfg, identity, true
}

func (s *Store) validatedPublishedSummary(id string, home *local.RootedHome, path string, info os.FileInfo, sourceSet bool) (summary PublishedSummary, err error) {
	var cfgDigest, bodyDigest [32]byte
	var rootIdentity os.FileInfo
	cached, cacheable := false, false
	if sourceSet {
		cfgDigest, rootIdentity, err = s.summaryConfig(home)
		if err != nil {
			return summary, err
		}
		bodyDigest, err = s.summaryFileHash(home, path, info)
		if err != nil {
			return summary, err
		}
		summary, cached, err = s.cachedSummary(id, bodyDigest, cfgDigest, rootIdentity)
		if err != nil {
			return summary, err
		}
	}
	if !cached {
		read, closeRead := s.WithReadBudget(s.durableContext(), s.resourceBudget)
		defer closeRead()
		var decodedDigest [32]byte
		read.publishedReadDigest = &decodedDigest
		if s.summaryDecodeBoundary != nil {
			s.summaryDecodeBoundary(false)
		}
		published, e := read.LoadPublishedState(id)
		if e != nil || !published.Found() {
			return PublishedSummary{}, errors.Join(ErrDurableStorageRecovery, e)
		}
		if published.state.PublicationVersion == 5 && !sourceSet {
			return PublishedSummary{}, ErrDurableStorageRecovery
		}
		summary = published.Summary()
		if s.summaryDecodeBoundary != nil {
			s.summaryDecodeBoundary(true)
		}
		if published.state.PublicationVersion == 5 {
			cacheable = true
			actual, e := s.summaryFileHash(home, path, info)
			if e != nil || actual != decodedDigest {
				return PublishedSummary{}, errors.Join(ErrDurableStorageRecovery, e)
			}
			bodyDigest = decodedDigest
		}
	}
	if sourceSet {
		cfgAfter, _, e := s.summaryConfig(home)
		if e != nil || cfgAfter != cfgDigest {
			return PublishedSummary{}, errors.Join(ErrDurableStorageRecovery, e)
		}
		if !cached && cacheable {
			s.cacheSummary(id, summary, bodyDigest, cfgDigest, rootIdentity)
			return s.ownPublishedSummary(summary)
		}
	}
	return summary, nil
}
