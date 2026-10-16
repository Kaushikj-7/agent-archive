package sealed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// Policy describes a closed retained-only projection, independently of old capture proof.
type Policy struct {
	FilterVersion  string `json:"filter_version"`
	AdapterVersion string `json:"adapter_version"`
	SkillEvidence  string `json:"skill_evidence"`
}

// Expectation contains immutable host facts, never selecting authority.
type Expectation struct {
	CoveredToken string                      `json:"covered_token"`
	HadRequest   bool                        `json:"had_request"`
	Registration archive.SessionRegistration `json:"registration"`
	StageSHA256  string                      `json:"stage_sha256"`
	Metadata     archive.Metadata            `json:"metadata"`
	OldPolicy    Policy                      `json:"old_policy"`
	Target       Policy                      `json:"target"`
}

// Request snapshots only expectations. Possessing one cannot mint a Proof.
type Request struct{ state *requestState }

type requestState struct {
	mu       sync.Mutex
	ctx      context.Context
	cancel   context.CancelCauseFunc
	budget   *agentapi.NativeReadBudget
	captured *agentapi.NativeReadBudget
	owner    *agentapi.NativeReadReservation
	raw      []byte
	source   []byte
	charged  int64
	closed   bool
	borrowed bool
}

// FreezeRequest detaches the host's retained input under the original scope.
func FreezeRequest(ctx context.Context, expected Expectation, source []byte, budget *agentapi.NativeReadBudget) (*Request, error) {
	if budget == nil {
		return nil, agentapi.ErrReadBudget
	}
	return FreezeRequestCaptured(ctx, expected, source, budget, budget.Reservation())
}

// FreezeRequestCaptured preserves an already captured owner while retaining the
// original ledger identity. The detached request obtains an independent charge
// owner; its close cannot release the caller's other outstanding reservations.
func FreezeRequestCaptured(ctx context.Context, expected Expectation, source []byte, budget *agentapi.NativeReadBudget, original *agentapi.NativeReadReservation) (*Request, error) {
	if ctx == nil || budget == nil || len(expected.StageSHA256) != 64 || len(source) == 0 || len(source) > 32<<20 {
		return nil, agentapi.ErrReadBudget
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	captured, err := budget.CaptureFromReservation(original)
	if err != nil {
		return nil, err
	}
	owner := captured.Reservation()
	const scratch int64 = 32 << 10
	if !owner.Reserve(scratch) {
		return nil, agentapi.ErrReadBudget
	}
	wire := expected
	n, err := agentmeta.JSONWireBound(ctx, wire, min(int64(128<<10), owner.Available()/2))
	owner.Release(scratch)
	if err != nil {
		return nil, err
	}
	charge := 2*n + int64(len(source))
	if !owner.Reserve(charge) {
		return nil, agentapi.ErrReadBudget
	}
	raw, err := json.Marshal(wire)
	if err != nil {
		owner.Release(charge)
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		owner.Release(charge)
		return nil, err
	}
	lifetime, cancel := context.WithCancelCause(ctx)
	return &Request{state: &requestState{ctx: lifetime, cancel: cancel, budget: budget, captured: captured, owner: owner, raw: raw, source: bytes.Clone(source), charged: charge}}, nil
}

// Lease is a detached input view on the original request's captured scope.
type Lease struct {
	request *Request
	Wire    Expectation
	Source  []byte
	custody *leaseCustody
}

// leaseCustody keeps copied input views on one terminal charge owner.
type leaseCustody struct {
	mu      sync.Mutex
	owner   *agentapi.NativeReadReservation
	charged int64
	closed  bool
}

// Borrow consumes this request once. Callers can cause refusal, never mint proof.
func (r *Request) Borrow() (*Lease, error) {
	if r == nil || r.state == nil {
		return nil, agentapi.ErrClosed
	}
	s := r.state
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.borrowed {
		return nil, agentapi.ErrClosed
	}
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	owner := s.captured.Reservation()
	charge := 2*int64(len(s.raw)) + int64(len(s.source))
	if !owner.Reserve(charge) {
		return nil, agentapi.ErrReadBudget
	}
	var wire Expectation
	if err := json.Unmarshal(s.raw, &wire); err != nil {
		owner.Release(charge)
		return nil, err
	}
	s.borrowed = true
	return &Lease{request: r, Wire: wire, Source: bytes.Clone(s.source), custody: &leaseCustody{owner: owner, charged: charge}}, nil
}

// Check rejects closure or cancellation before another borrowed operation.
func (l *Lease) Check() error {
	if l == nil || l.request == nil || l.custody == nil {
		return agentapi.ErrClosed
	}
	l.custody.mu.Lock()
	closed := l.custody.closed
	l.custody.mu.Unlock()
	if closed {
		return agentapi.ErrClosed
	}
	s := l.request.state
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return agentapi.ErrClosed
	}
	return s.ctx.Err()
}

// Context retains cancellation from the original host request.
func (l *Lease) Context() context.Context { return l.request.state.ctx }

// Budget is a facade over the original scope, even after another scope begins.
func (l *Lease) Budget() *agentapi.NativeReadBudget { return l.request.state.captured }

// Close releases only this detached view's independent charges.
func (l *Lease) Close() {
	if l == nil || l.custody == nil {
		return
	}
	c := l.custody
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	c.owner.Release(c.charged)
	c.charged = 0
	l.Source = nil
	l.Wire = Expectation{}
}

// Close ends all use of the request, including already-issued proof loans.
func (r *Request) Close() error {
	if r == nil || r.state == nil {
		return nil
	}
	s := r.state
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.cancel(agentapi.ErrClosed)
	s.owner.Release(s.charged)
	s.charged = 0
	s.raw = nil
	s.source = nil
	return nil
}

func (r *Request) matches(expected *Request, budget *agentapi.NativeReadBudget) error {
	if r == nil || r != expected || r.state == nil || budget == nil || r.state.budget != budget {
		return errors.New("retained transform request or resource owner changed")
	}
	return nil
}
