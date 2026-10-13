package nativefacts

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// CapturePolicy freezes the expected native derivation policy, never proof authority.
type CapturePolicy struct {
	FilterVersion string           `json:"filter_version"`
	SkillEvidence CaptureSkillMode `json:"skill_evidence"`
}

type requestWire struct {
	Registration archive.SessionRegistration `json:"registration"`
	Reviewed     agentapi.DatabaseChat       `json:"reviewed"`
	Locator      string                      `json:"locator"`
	At           time.Time                   `json:"at"`
	Policy       CapturePolicy               `json:"policy"`
}

// CaptureRequest retains a bounded immutable host expectation before port dispatch.
// Copies share terminal Close; the descriptor alone cannot mint native proof.
type CaptureRequest struct{ state *requestState }

type requestState struct {
	mu        sync.Mutex
	closed    bool
	borrowers int
	ctx       context.Context
	cancel    context.CancelCauseFunc
	budget    *agentapi.NativeReadBudget
	owner     *agentapi.NativeReadReservation
	raw       []byte
	charged   int64
	used      int64
	policy    CapturePolicy
}

// FreezeCaptureRequest snapshots actual host inputs under their original scope.
func FreezeCaptureRequest(ctx context.Context, reg archive.SessionRegistration, reviewed agentapi.DatabaseChat, locator string, at time.Time, policy CapturePolicy, budget *agentapi.NativeReadBudget) (*CaptureRequest, error) {
	if ctx == nil || budget == nil || !filepath.IsAbs(locator) || at.IsZero() {
		return nil, agentapi.Wrap(agentapi.Unsafe, errors.New("invalid capture request"))
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	owner := budget.Reservation()
	const scratch int64 = 32 << 10
	if !owner.Reserve(scratch) {
		return nil, agentapi.Wrap(agentapi.Limit, agentapi.ErrReadBudget)
	}
	wire := requestWire{Registration: reg, Reviewed: reviewed, Locator: filepath.Clean(locator), At: at, Policy: policy}
	_, err := agentmeta.JSONWireBound(ctx, reg, 64<<10)
	if err != nil {
		owner.Release(scratch)
		return nil, err
	}
	n, err := agentmeta.JSONWireBound(ctx, wire, min(owner.Available(), (128<<20)-scratch)/2)
	owner.Release(scratch)
	if err != nil {
		return nil, err
	}
	if !owner.Reserve(2 * n) {
		return nil, agentapi.Wrap(agentapi.Limit, agentapi.ErrReadBudget)
	}
	raw, err := json.Marshal(wire)
	if err != nil {
		owner.Release(2 * n)
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		owner.Release(2 * n)
		return nil, err
	}
	lifetime, cancel := context.WithCancelCause(ctx)
	return &CaptureRequest{state: &requestState{ctx: lifetime, cancel: cancel, budget: budget, owner: owner, raw: raw, charged: 2 * n, used: 2 * n, policy: policy}}, nil
}

// RequestLease owns a detached descriptor view and borrows the original scope.
// It is not a capture proof and cannot authorize state writes.
type RequestLease struct{ lease *requestLeaseState }

type requestLeaseState struct {
	state        *requestState
	wire         requestWire
	charge       int64
	closed       bool
	released     bool
	scopes       []*scopeState
	scopeCharge  int64
	getterCharge int64
}

type scopeState struct {
	lease *requestLeaseState
	held  int64
}

// Borrow protects descriptor charges until its lease closes, including concurrent Close.
func (r *CaptureRequest) Borrow() (*RequestLease, error) {
	if r == nil || r.state == nil {
		return nil, agentapi.ErrClosed
	}
	s := r.state
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, agentapi.ErrClosed
	}
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	charge := 3 * int64(len(s.raw))
	if charge > (128<<20)-s.used || !s.owner.Reserve(charge) {
		return nil, agentapi.Wrap(agentapi.Limit, agentapi.ErrReadBudget)
	}
	var wire requestWire
	if err := json.Unmarshal(s.raw, &wire); err != nil {
		s.owner.Release(charge)
		return nil, err
	}
	s.used += charge
	s.borrowers++
	return &RequestLease{lease: &requestLeaseState{state: s, wire: wire, charge: charge}}, nil
}

// Context returns the original actual capture context.
func (l *RequestLease) Context() context.Context { return l.lease.state.ctx }

// Budget returns the original ledger identity, never a recaptured reservation.
func (l *RequestLease) Budget() *agentapi.NativeReadBudget { return l.lease.state.budget }

// Reservation borrows the request's original captured resource scope.
func (l *RequestLease) Reservation() *CapturedScope {
	s := l.lease.state
	s.mu.Lock()
	defer s.mu.Unlock()
	const charge int64 = 64
	if s.closed || l.lease.closed || charge > (128<<20)-s.used || !s.owner.Reserve(charge) {
		return &CapturedScope{}
	}
	s.used += charge
	l.lease.scopeCharge += charge
	o := &scopeState{lease: l.lease}
	l.lease.scopes = append(l.lease.scopes, o)
	return &CapturedScope{scope: o}
}

// Registration returns this lease's detached, charged owner snapshot.
func (l *RequestLease) Registration() (reg archive.SessionRegistration, err error) {
	if l == nil || l.lease == nil {
		return reg, agentapi.ErrClosed
	}
	s := l.lease.state
	s.mu.Lock()
	defer s.mu.Unlock()
	if l.lease.closed || s.closed {
		return reg, agentapi.ErrClosed
	}
	if err = s.ctx.Err(); err != nil {
		return reg, err
	}
	const scratch int64 = 32 << 10
	if scratch > (128<<20)-s.used || !s.owner.Reserve(scratch) {
		return reg, agentapi.Wrap(agentapi.Limit, agentapi.ErrReadBudget)
	}
	n, err := agentmeta.JSONWireBound(s.ctx, l.lease.wire.Registration, 64<<10)
	s.owner.Release(scratch)
	if err != nil {
		return reg, err
	}
	if 3*n > (128<<20)-s.used || !s.owner.Reserve(3*n) {
		return reg, agentapi.Wrap(agentapi.Limit, agentapi.ErrReadBudget)
	}
	raw, err := json.Marshal(l.lease.wire.Registration)
	if err == nil {
		err = json.Unmarshal(raw, &reg)
	}
	if err = errors.Join(err, s.ctx.Err()); err != nil {
		s.owner.Release(3 * n)
		return archive.SessionRegistration{}, err
	}
	s.owner.Release(2 * n)
	s.used += n
	l.lease.getterCharge += n
	return reg, nil
}

// Reviewed returns the original compact planning comparison.
func (l *RequestLease) Reviewed() agentapi.DatabaseChat {
	if l == nil || l.lease == nil {
		return agentapi.DatabaseChat{}
	}
	s := l.lease.state
	s.mu.Lock()
	defer s.mu.Unlock()
	if l.lease.closed {
		return agentapi.DatabaseChat{}
	}
	return l.lease.wire.Reviewed
}

// Locator returns the frozen host locator.
func (l *RequestLease) Locator() string {
	if l == nil || l.lease == nil {
		return ""
	}
	s := l.lease.state
	s.mu.Lock()
	defer s.mu.Unlock()
	if l.lease.closed {
		return ""
	}
	return l.lease.wire.Locator
}

// At returns the fixed capture timestamp.
func (l *RequestLease) At() time.Time {
	if l == nil || l.lease == nil {
		return time.Time{}
	}
	s := l.lease.state
	s.mu.Lock()
	defer s.mu.Unlock()
	if l.lease.closed {
		return time.Time{}
	}
	return l.lease.wire.At
}

// Policy returns the frozen derivation policy.
func (l *RequestLease) Policy() CapturePolicy {
	if l == nil || l.lease == nil {
		return CapturePolicy{}
	}
	s := l.lease.state
	s.mu.Lock()
	defer s.mu.Unlock()
	if l.lease.closed {
		return CapturePolicy{}
	}
	return l.lease.wire.Policy
}

// Check refuses a closed descriptor or cancelled original context before late mint or loan.
func (l *RequestLease) Check() error {
	l.lease.state.mu.Lock()
	defer l.lease.state.mu.Unlock()
	if l.lease.closed || l.lease.state.closed {
		return agentapi.ErrClosed
	}
	return l.lease.state.ctx.Err()
}

// Close releases the detached view once; terminal request release waits for all borrowers.
func (l *RequestLease) Close() {
	if l == nil || l.lease == nil {
		return
	}
	s := l.lease.state
	s.mu.Lock()
	defer s.mu.Unlock()
	l.lease.closed = true
	l.lease.finishClosed()
}

func (l *requestLeaseState) finishClosed() {
	if !l.closed || l.released {
		return
	}
	for _, o := range l.scopes {
		if o.held != 0 {
			return
		}
	}
	l.released = true
	l.state.owner.Release(l.charge + l.scopeCharge + l.getterCharge)
	l.state.used -= l.charge + l.scopeCharge + l.getterCharge
	l.wire = requestWire{}
	l.state.borrowers--
	l.state.releaseClosed()
}

func (s *requestState) releaseClosed() {
	if s.closed && s.borrowers == 0 && s.charged != 0 {
		s.owner.Release(s.charged)
		s.used -= s.charged
		s.charged = 0
		s.raw = nil
	}
}

// Close revokes future use immediately and releases only after active borrowers finish.
func (r *CaptureRequest) Close() {
	if r == nil || r.state == nil {
		return
	}
	s := r.state
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.cancel(agentapi.ErrClosed)
	s.releaseClosed()
}

// MatchesBudget checks ledger provenance without decoding or charging a view.
func (r *CaptureRequest) MatchesBudget(budget *agentapi.NativeReadBudget) bool {
	if r == nil || r.state == nil || budget == nil {
		return false
	}
	s := r.state
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed && s.budget == budget
}

// CapturedScope borrows the original request reservation and shared128MiB ceiling.
// It cannot construct a proof or replace the ledger/scope.
type CapturedScope struct{ scope *scopeState }

// Reserve charges this handle before allocation against the original scope.
func (o *CapturedScope) Reserve(n int64) bool {
	if o == nil || o.scope == nil {
		return false
	}
	q := o.scope
	s := q.lease.state
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || q.lease.closed || n < 0 || n > (128<<20)-s.used || !s.owner.Reserve(n) {
		return false
	}
	q.held += n
	s.used += n
	return true
}

// Release returns only this handle's exact charge, including deferred cleanup after Close.
func (o *CapturedScope) Release(n int64) {
	if o == nil || o.scope == nil {
		if n != 0 {
			panic("unreserved captured scope release")
		}
		return
	}
	q := o.scope
	s := q.lease.state
	s.mu.Lock()
	defer s.mu.Unlock()
	if n < 0 || n > q.held {
		panic("unbalanced captured scope release")
	}
	s.owner.Release(n)
	q.held -= n
	s.used -= n
	q.lease.finishClosed()
}

// Available reports the original scope's remaining extent; closed handles cannot grow.
func (o *CapturedScope) Available() int64 {
	if o == nil || o.scope == nil {
		return 0
	}
	q := o.scope
	s := q.lease.state
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || q.lease.closed {
		return 0
	}
	return min(s.owner.Available(), (128<<20)-s.used)
}

// Refuse records pressure against the original captured scope.
func (o *CapturedScope) Refuse(n int64) {
	if o == nil || o.scope == nil {
		return
	}
	o.scope.lease.state.owner.Refuse(n)
}

// MatchesPolicy compares current host policy without decoding or charging a view.
func (r *CaptureRequest) MatchesPolicy(policy CapturePolicy) bool {
	if r == nil || r.state == nil {
		return false
	}
	s := r.state
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed && s.policy == policy
}

// CaptureSkillMode retains the closed existing privacy policy values.
type CaptureSkillMode string

// CaptureSkillNone, CaptureSkillMetadata and CaptureSkillBody preserve policy wire values.
const (
	CaptureSkillNone     CaptureSkillMode = "none"
	CaptureSkillMetadata CaptureSkillMode = "metadata"
	CaptureSkillBody     CaptureSkillMode = "body"
)
