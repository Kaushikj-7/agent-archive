package state

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

const accountingFactBytes int64 = 1024

type publicationAccountingFact struct {
	path  string
	stamp os.FileInfo
	sha   [32]byte
	mode  int
}

type publicationAccountingContext struct {
	home      os.FileInfo
	config    os.FileInfo
	configSHA [32]byte
}

type publicationAccounting struct {
	mu          sync.Mutex
	ctx         context.Context
	budget      *agentapi.NativeReadBudget
	reservation *agentapi.NativeReadReservation
	seeded      bool
	seedValid   bool
	closed      bool
	binding     publicationAccountingContext
	facts       []publicationAccountingFact
	charge      int64
}

// WithPublicationAccounting owns accounting facts only during the existing
// source pass. It grants no data-read, native, publication or cleanup authority.
func (s *Store) WithPublicationAccounting(ctx context.Context, budget *agentapi.NativeReadBudget) (*Store, func()) {
	scoped := *s
	scoped.publicationAccounting = nil
	reservation := budget.Reservation()
	if budget == nil || !reservation.Reserve(accountingFactBytes) {
		return &scoped, func() {}
	}
	a := &publicationAccounting{ctx: ctx, budget: budget, reservation: reservation, charge: accountingFactBytes}
	scoped.publicationAccounting = a
	var once sync.Once
	return &scoped, func() {
		once.Do(func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.closed = true
			a.facts = nil
			a.reservation.Release(a.charge)
			a.charge = 0
		})
	}
}

func (s *Store) accountingScope() *publicationAccounting {
	a := s.publicationAccounting
	if a == nil || s.resourceBudget != a.budget {
		return nil
	}
	return a
}

func (s *Store) invalidatePublishedAccounting(path string) {
	a := s.publicationAccounting
	if a == nil {
		return
	}
	rel, err := filepath.Rel(s.home, path)
	if err != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range a.facts {
		if a.facts[i].path == rel {
			a.facts[i].mode = 0
		}
	}
}

func (a *publicationAccounting) clear() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range a.facts {
		a.facts[i].mode = 0
	}
}

func (a *publicationAccounting) active() bool {
	return !a.closed && a.ctx.Err() == nil
}

func (a *publicationAccounting) install(f publicationAccountingFact, binding publicationAccountingContext) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.active() {
		return false
	}
	if a.binding.home != nil && (!sameDurableStamp(a.binding.config, binding.config) || !os.SameFile(a.binding.home, binding.home) || a.binding.configSHA != binding.configSHA) {
		for i := range a.facts {
			a.facts[i].mode = 0
		}
	}
	a.binding = binding
	for i := range a.facts {
		if a.facts[i].path == f.path {
			a.facts[i] = f
			return true
		}
	}
	if len(a.facts) >= durableEntryLimit || !a.reservation.Reserve(accountingFactBytes) {
		return false
	}
	a.charge += accountingFactBytes
	if len(a.facts) == cap(a.facts) {
		capacity := min(durableEntryLimit, max(16, cap(a.facts)*2))
		allocation := int64(capacity) * 128
		if !a.reservation.Reserve(allocation) {
			a.reservation.Release(accountingFactBytes)
			a.charge -= accountingFactBytes
			return false
		}
		next := make([]publicationAccountingFact, len(a.facts), capacity)
		copy(next, a.facts)
		old := int64(cap(a.facts)) * 128
		a.facts = next
		a.reservation.Release(old)
		a.charge += allocation - old
	}
	a.facts = append(a.facts, f)
	return true
}

// accountingHash opens and closes the actual file and hashes borrowed fixed
// scratch. Stamps never replace checking complete content, including EOF.
func (s *Store) accountingHash(home *local.RootedHome, path string, expected os.FileInfo) (sum [32]byte, err error) {
	if expected == nil || !expected.Mode().IsRegular() || expected.Size() < 0 || expected.Size() > durableStorageQuota {
		return sum, ErrDurableStorageRecovery
	}
	if err = s.durableContext().Err(); err != nil {
		return sum, err
	}
	pathBytes := int64(len(path))
	if int64(len(s.home)) > math.MaxInt64-pathBytes {
		return sum, errStateBudget
	}
	pathBytes += int64(len(s.home))
	if pathBytes > (math.MaxInt64-1024)/4 {
		return sum, errStateBudget
	}
	overhead := 1024 + 4*pathBytes
	if s.resourceBudget == nil {
		return sum, errStateBudget
	}
	reservation := s.resourceBudget.Reservation()
	if !reservation.Reserve(overhead) {
		return sum, errStateBudget
	}
	defer reservation.Release(overhead)
	scratch := min(expected.Size(), int64(128<<10))
	if !reservation.Reserve(scratch) {
		scratch = min(expected.Size(), int64(32<<10))
		if !reservation.Reserve(scratch) {
			return sum, errStateBudget
		}
	}
	defer reservation.Release(scratch)
	buffer := make([]byte, scratch)
	f, err := home.Root.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return sum, err
	}
	defer func() {
		if e := s.closePublicationFile(f); e != nil {
			sum = [32]byte{}
			err = errors.Join(err, e)
		}
	}()
	opened, err := f.Stat()
	if err != nil || !sameDurableStamp(expected, opened) {
		return sum, errors.Join(ErrDurableStorageRecovery, err)
	}
	hash := sha256.New()
	left := expected.Size()
	for left > 0 {
		if err = s.durableContext().Err(); err != nil {
			return sum, err
		}
		n, e := f.Read(buffer[:min(int64(len(buffer)), left)])
		if n > 0 {
			_, _ = hash.Write(buffer[:n])
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
	n, e := f.Read(extra[:])
	if n != 0 || !errors.Is(e, io.EOF) {
		return sum, errors.Join(ErrDurableStorageRecovery, e)
	}
	opened, err = f.Stat()
	named, e := home.Root.Lstat(path)
	if err != nil || e != nil || !sameDurableStamp(expected, opened) || !sameDurableStamp(expected, named) {
		return sum, errors.Join(ErrDurableStorageRecovery, err, e)
	}
	if err = home.Check(); err != nil {
		return sum, err
	}
	if err = s.durableContext().Err(); err != nil {
		return sum, err
	}
	copy(sum[:], hash.Sum(nil))
	return sum, nil
}

func (s *Store) accountingBinding(home *local.RootedHome) (binding publicationAccountingContext, err error) {
	binding.home, err = home.Root.Stat(".")
	if err != nil {
		return binding, err
	}
	binding.config, err = home.Root.Lstat("config.json")
	if err != nil {
		return binding, err
	}
	binding.configSHA, err = s.accountingHash(home, "config.json", binding.config)
	return binding, err
}

func (s *Store) preparePublicationAccounting(id string) {
	a := s.accountingScope()
	if a == nil {
		return
	}
	a.mu.Lock()
	if a.closed || a.seeded {
		a.mu.Unlock()
		return
	}
	a.seeded = true
	a.mu.Unlock()
	if err := s.seedPublicationAccounting(a, id); err != nil {
		a.clear()
		return
	}
	a.mu.Lock()
	a.seedValid = true
	a.mu.Unlock()
}

func (s *Store) seedPublicationAccounting(a *publicationAccounting, id string) (err error) {
	home, err := local.OpenRootedHome(s.home)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, s.closePublicationRoot(home.Root)) }()
	unlock, err := local.RootedLockWait(home, "hooks.lock", time.Second)
	if err != nil {
		return err
	}
	defer unlock()
	cfgInfo, err := home.Root.Lstat("config.json")
	if err != nil || cfgInfo.Size() < 0 || cfgInfo.Size() > durableStorageQuota/2 {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	if !a.reservation.Reserve(2 * cfgInfo.Size()) {
		return errStateBudget
	}
	cfg, found, observedSHA, e := config.LoadRootedObservedBounded(home, cfgInfo.Size())
	a.reservation.Release(2 * cfgInfo.Size())
	if e != nil || !found || cfg.SchemaVersion != 17 || !cfg.SourceSetPublicationProtection || !cfg.DurableStorageProtection {
		return errors.Join(ErrDurableStorageRecovery, e)
	}
	binding, err := s.accountingBinding(home)
	if err != nil || !sameDurableStamp(cfgInfo, binding.config) || observedSHA != binding.configSHA {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	a.mu.Lock()
	a.binding = binding
	a.mu.Unlock()
	dir, err := home.Root.Open("published")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, s.closePublicationFile(dir)) }()
	const enumeration = 64 << 10
	if !a.reservation.Reserve(enumeration) {
		return errStateBudget
	}
	defer a.reservation.Release(enumeration)
	remaining := durableEntryLimit
	for {
		if err = s.durableContext().Err(); err != nil {
			return err
		}
		entries, e := dir.ReadDir(128)
		if e != nil && !errors.Is(e, io.EOF) {
			return e
		}
		for _, entry := range entries {
			remaining--
			if remaining < 0 {
				return ErrDurableStorageRecovery
			}
			s.seedPublishedAccountingPeer(a, home, binding, entry.Name(), id)
		}
		if errors.Is(e, io.EOF) {
			break
		}
	}
	final, err := s.accountingBinding(home)
	if err != nil || !sameDurableStamp(binding.config, final.config) || binding.configSHA != final.configSHA || !os.SameFile(binding.home, final.home) {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	return nil
}

func (q *durableQuota) accountingClassification(path string, expected os.FileInfo) (int, bool, error) {
	a := q.store.accountingScope()
	if a == nil {
		return 0, false, nil
	}
	a.mu.Lock()
	// Inactive facts cannot classify; the authoritative fallback still checks
	// the actual operation context before reading or granting accounting credit.
	if !a.active() {
		a.mu.Unlock()
		return 0, false, nil
	}
	binding := a.binding
	var fact publicationAccountingFact
	for _, f := range a.facts {
		if f.path == path {
			fact = f
			break
		}
	}
	a.mu.Unlock()
	if fact.mode == 0 {
		return 0, false, nil
	}
	current, err := q.store.accountingBinding(q.home)
	if err != nil {
		a.clear()
		return 0, false, err
	}
	if !sameDurableStamp(binding.config, current.config) || binding.configSHA != current.configSHA || !os.SameFile(binding.home, current.home) {
		a.clear()
		return 0, false, nil
	}
	if !sameDurableStamp(fact.stamp, expected) {
		q.store.invalidatePublishedAccounting(filepath.Join(q.path, path))
		return 0, false, nil
	}
	sum, err := q.store.accountingHash(q.home, path, expected)
	if err != nil {
		q.store.invalidatePublishedAccounting(filepath.Join(q.path, path))
		return 0, false, err
	}
	if sum != fact.sha {
		q.store.invalidatePublishedAccounting(filepath.Join(q.path, path))
		return 0, false, nil
	}
	if err = q.guard.CheckHome(q.path); err != nil {
		a.clear()
		return 0, false, err
	}
	return fact.mode, true, nil
}

// savedAccountingCandidate is called only after the typed selecting producer
// encoded and durably saved its validated final state through the shared writer.
func (s *Store) savedAccountingCandidate(g config.DurableStorageGuard, path string, sum [32]byte, n int64) (fact *publicationAccountingFact, binding publicationAccountingContext, release func(), err error) {
	a := s.accountingScope()
	if a == nil {
		return nil, binding, func() {}, nil
	}
	if !a.reservation.Reserve(accountingFactBytes) {
		return nil, binding, func() {}, errStateBudget
	}
	release = func() { a.reservation.Release(accountingFactBytes) }
	home, err := g.RootedHome(s.home)
	if err != nil {
		return nil, binding, release, err
	}
	info, err := home.Root.Lstat(path)
	if err != nil || info.Size() != n {
		return nil, binding, release, errors.Join(ErrDurableStorageRecovery, err)
	}
	actual, err := s.accountingHash(home, path, info)
	if err != nil || actual != sum {
		return nil, binding, release, errors.Join(ErrDurableStorageRecovery, err)
	}
	binding, err = s.accountingBinding(home)
	if err != nil {
		return nil, binding, release, err
	}
	if err = g.CheckHome(s.home); err != nil {
		return nil, binding, release, err
	}
	return &publicationAccountingFact{path: path, stamp: info, sha: actual, mode: 2}, binding, release, nil
}

// readPublishedOwned shares the actual checked current decode with accounting.
// Partial legacy read states remain readable but never mint a quota fact.
func (s *Store) readPublishedOwned(path string, p *publishedState) (found bool, err error) {
	a := s.accountingScope()
	if a == nil {
		return s.readOwned(path, p)
	}
	a.mu.Lock()
	ready := a.seedValid && !a.closed && a.ctx.Err() == nil
	binding := a.binding
	a.mu.Unlock()
	if !ready {
		return s.readOwned(path, p)
	}
	err = s.readCurrentPublished(path, p, a, binding)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	case !s.collectorPass || !isCorruptJSON(err):
		return false, err
	}
	return false, s.moveAside(path, err)
}

//nolint:contextcheck // publication reads inherit the original scoped Store context through durableContext.
func (s *Store) readCurrentPublished(path string, p *publishedState, a *publicationAccounting, binding publicationAccountingContext) (err error) {
	if !a.reservation.Reserve(accountingFactBytes) {
		return errStateBudget
	}
	defer a.reservation.Release(accountingFactBytes)
	home, err := local.OpenRootedHome(s.home)
	if err != nil {
		return err
	}
	closed := false
	defer func() {
		if !closed {
			err = errors.Join(err, s.closePublicationRoot(home.Root))
		}
	}()
	rel, err := filepath.Rel(s.home, path)
	if err != nil {
		return err
	}
	info, err := home.Root.Lstat(rel)
	if err != nil {
		return err
	}
	sum, release, err := s.readPublishedDecoded(s.durableContext(), home, rel, info, p)
	keep := false
	defer func() {
		if !keep {
			release()
		}
	}()
	if err != nil {
		return err
	}
	current, err := s.accountingBinding(home)
	if err != nil {
		return err
	}
	if !sameDurableStamp(binding.config, current.config) || binding.configSHA != current.configSHA || !os.SameFile(binding.home, current.home) {
		a.clear()
		return ErrDurableStorageRecovery
	}
	if err = s.durableContext().Err(); err != nil {
		return err
	}
	err = s.closePublicationRoot(home.Root)
	closed = true
	if err != nil {
		a.clear()
		return err
	}
	if mode := publishedAccountingMode(*p); mode != 0 {
		a.install(publicationAccountingFact{path: rel, stamp: info, sha: sum, mode: mode}, current)
	}
	*s.resourceReleases = append(*s.resourceReleases, release)
	keep = true
	return nil
}

// A peer that cannot be fully classified receives no accounting fact; normal
// quota classification remains the authoritative fallback for that file.
//
//nolint:contextcheck // readPublishedClassification uses the context captured on this scoped Store, not a new background scope.
func (s *Store) seedPublishedAccountingPeer(a *publicationAccounting, home *local.RootedHome, binding publicationAccountingContext, name, id string) {
	if name == id+".json" {
		return
	}
	if strings.HasPrefix(name, ".pending-") || !strings.HasSuffix(name, ".json") || !safeFileComponent(strings.TrimSuffix(name, ".json")) {
		return
	}
	if !a.reservation.Reserve(accountingFactBytes) {
		return
	}
	path := filepath.Join("published", name)
	info, e := home.Root.Lstat(path)
	if e != nil {
		a.reservation.Release(accountingFactBytes)
		return
	}
	mode, sum, e := s.readPublishedClassification(s.durableContext(), home, path, info)
	if e != nil {
		a.reservation.Release(accountingFactBytes)
		return
	}
	a.install(publicationAccountingFact{path: path, stamp: info, sha: sum, mode: mode}, binding)
	a.reservation.Release(accountingFactBytes)
}
