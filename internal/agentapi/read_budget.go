package agentapi

import (
	"errors"
	"sync"
)

// NativeReadBudget bounds charged native extents, immutable caches and borrowed
// scratch across one pass. It is not a bound on filtered maps or process RSS.
type NativeReadBudget struct {
	mu    sync.Mutex
	limit int64
	used  int64
	peak  int64
	scope *readScope
}

// readScope attributes charges to one owner under its own ceiling.
type readScope struct {
	ceiling  int64
	used     int64
	exceeded bool
	// pressured: a charge within the ceiling was refused because others
	// held the shared ledger.
	pressured bool
}

// PassReadBudget is the shared ledger one collector pass gives its native
// sources, the Codex index and catalog, and the sessions it scans one at a
// time. It bounds the bytes they hold (record buffers, decoded rows,
// filtered output, retained state being rewritten), never file lengths read
// through a descriptor. Measured on the owner's machine, the largest
// Codex session (a 182 MB rollout, 31 MB filtered, 40 MB of retained state)
// held 107 MiB while refreshing its publication. It is PassCacheReserve
// above the per-session ceiling (collector.SessionReadCeiling), so a session
// within its ceiling always fits beside what the pass keeps between sessions.
const PassReadBudget int64 = 256 << 20

// PassCacheReserve is the room the pass keeps for what outlives one
// session's scan, all charged with ReserveUnscoped and capped: the Codex
// lookup's restored catalog with its coverage facts (32 MiB), coverage
// observations (8 MiB) and metadata inventory (16 MiB); the source's cached
// ancestor prefixes (16 MiB) and rollout headers (64 x 128 KiB): 80 MiB in
// all (discovery.PassCacheBytes plus codex.PassCacheBytes).
const PassCacheReserve int64 = 96 << 20

// NewNativeReadBudget creates a pass-owned shared charge ledger.
func NewNativeReadBudget(limit int64) *NativeReadBudget {
	return &NativeReadBudget{limit: max(limit, 0)}
}

// Reserve charges before allocation or borrowing; refusal changes no state.
func (b *NativeReadBudget) Reserve(bytes int64) bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if bytes < 0 {
		return false
	}
	if s := b.scope; s != nil && bytes > s.ceiling-s.used {
		s.exceeded = true
		return false
	}
	if bytes > b.limit-b.used {
		if b.scope != nil {
			b.scope.pressured = true
		}
		return false
	}
	b.used += bytes
	b.peak = max(b.peak, b.used)
	if b.scope != nil {
		b.scope.used += bytes
	}
	return true
}

// Release returns an exact owned charge. Imbalanced releases panic because
// quietly resetting the ledger would hide a violated safety invariant.
func (b *NativeReadBudget) Release(bytes int64) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if bytes < 0 || bytes > b.used {
		panic("unbalanced native read budget release")
	}
	b.used -= bytes
	if s := b.scope; s != nil {
		// A charge taken before the scope began can end inside it; the scope
		// then under-counts rather than refusing its owner too early.
		s.used -= min(bytes, s.used)
	}
}

// ReserveUnscoped charges a pass-lifetime cache against the shared limit
// only: it is no session's, so it neither counts toward the current scope
// nor marks the scope exceeded when refused. Its owner releases it with
// ReleaseUnscoped.
func (b *NativeReadBudget) ReserveUnscoped(bytes int64) bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if bytes < 0 || bytes > b.limit-b.used {
		return false
	}
	b.used += bytes
	b.peak = max(b.peak, b.used)
	return true
}

// ReserveUnscopedRequired is ReserveUnscoped for a pass-lifetime charge the
// current session cannot go on without (a rollout's header): its refusal
// marks the scope pressured, so the session is deferred to a later pass
// rather than failed.
func (b *NativeReadBudget) ReserveUnscopedRequired(bytes int64) bool {
	if b.ReserveUnscoped(bytes) {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.scope != nil {
		b.scope.pressured = true
	}
	return false
}

// ReleaseUnscoped returns a ReserveUnscoped charge without lowering the
// current scope's count.
func (b *NativeReadBudget) ReleaseUnscoped(bytes int64) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if bytes < 0 || bytes > b.used {
		panic("unbalanced native read budget release")
	}
	b.used -= bytes
}

// BeginScope attributes every later charge, until end, to one owner (one
// session's scan) under its own ceiling as well as the shared limit. Owners
// run one at a time, so a scope nests none: end restores the one before.
// ScopeExceeded then tells a refusal at the owner's own ceiling, which the
// same owner meets again however long it waits, from one where others held
// the shared ledger, which a later pass need not meet.
func (b *NativeReadBudget) BeginScope(ceiling int64) (end func()) {
	if b == nil {
		return func() {}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	previous := b.scope
	scope := &readScope{ceiling: max(ceiling, 0)}
	b.scope = scope
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.scope == scope {
			b.scope = previous
		}
	}
}

// Refuse records a charge of bytes the caller could not make, as Reserve
// would have refused it, for a caller that sized its request from Available
// and found it too large before asking: past the scope's own ceiling, it
// marks the scope exceeded.
func (b *NativeReadBudget) Refuse(bytes int64) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	switch s := b.scope; {
	case s == nil:
	case bytes > s.ceiling-s.used:
		s.exceeded = true
	case bytes > b.limit-b.used:
		s.pressured = true
	}
}

// ScopePressured reports whether the current scope was refused a charge
// within its own ceiling because others held the shared ledger.
func (b *NativeReadBudget) ScopePressured() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.scope != nil && b.scope.pressured
}

// ScopeExceeded reports whether the current scope refused a charge at its
// own ceiling.
func (b *NativeReadBudget) ScopeExceeded() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.scope != nil && b.scope.exceeded
}

// Available returns remaining capacity for a bounded optional cache: the
// lesser of the shared and the current scope's remaining capacity.
func (b *NativeReadBudget) Available() int64 {
	if b == nil {
		return 1 << 62
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	available := b.limit - b.used
	if s := b.scope; s != nil {
		available = min(available, s.ceiling-s.used)
	}
	return available
}

// Charged returns current and highest pass charges for measured evidence.
func (b *NativeReadBudget) Charged() (used, peak int64) {
	if b == nil {
		return 0, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used, b.peak
}

// CodexRolloutResourceBudget shares lookup copy charges with source providers.
type CodexRolloutResourceBudget interface{ NativeReadBudget() *NativeReadBudget }

// ErrReadBudget identifies transient shared resource pressure separately from
// permanent record/format ceilings that also use the Limit failure category.
var ErrReadBudget = errors.New("shared read data budget exhausted")

// ReadBudgetLimit keeps resource refusal pending through codec/error wrappers.
func ReadBudgetLimit(cause error) error { return Wrap(Limit, errors.Join(ErrReadBudget, cause)) }
