// Package nativecapture owns complete fixed native capture and private proof lifetimes.
package nativecapture

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/nativefacts"
)

// Proof lends only actually captured native bytes and shares terminal Close across copies.
type Proof struct{ capture *settledCapture }

type settledCapture struct {
	mu             sync.Mutex
	closed         bool
	reg            archive.SessionRegistration
	bundle         archive.SourceBundle
	facts          nativefacts.SettledImportFacts
	digest         string
	owner          *settledReservation
	charged        int64
	request        *nativefacts.CaptureRequest
	lease          *nativefacts.RequestLease
	originalBudget *agentapi.NativeReadBudget
}

// WithVerifiedCapture lends verified evidence only while its captured owner is live.
// The callback must not retain the borrowed bundle after Close.
func (e *Proof) WithVerifiedCapture(ctx context.Context, reg archive.SessionRegistration, expected *nativefacts.CaptureRequest, expectedBudget *agentapi.NativeReadBudget, visit func(context.Context, archive.SourceBundle, nativefacts.SettledImportFacts) error) (err error) {
	if e == nil || e.capture == nil || visit == nil {
		return agentapi.ErrClosed
	}
	c := e.capture
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return agentapi.ErrClosed
	}
	originalCtx := c.lease.Context()
	callerCtx := ctx
	if callerCtx == nil {
		return agentapi.Wrap(agentapi.Unsafe, errors.New("capture loan requires context"))
	}
	defer func() {
		if originalCtx.Err() != nil {
			err = errors.Join(err, originalCtx.Err(), context.Cause(originalCtx))
		}
		if callerCtx.Err() != nil {
			err = errors.Join(err, callerCtx.Err(), context.Cause(callerCtx))
		}
	}()
	if c.originalBudget == nil || c.originalBudget != expectedBudget || c.request != expected || !expected.MatchesBudget(expectedBudget) {
		return agentapi.Wrap(agentapi.Unsafe, errors.New("capture resource or request provenance changed"))
	}
	if err := c.lease.Check(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	loanCtx, cancel := context.WithCancelCause(ctx)
	stop := context.AfterFunc(originalCtx, func() { cancel(context.Cause(originalCtx)) })
	defer func() { stop(); cancel(nil) }()
	ctx = joinedLoanContext{Context: loanCtx, original: originalCtx}
	original, err := settledCaptureDigest(ctx, c.owner, c.reg, 64<<10)
	if err != nil {
		return err
	}
	requested, err := settledCaptureDigest(ctx, c.owner, reg, 64<<10)
	if err != nil {
		return err
	}
	if original != requested {
		return agentapi.Wrap(agentapi.Unsafe, errors.New("settled capture owner changed"))
	}
	digest, err := settledCaptureDigest(ctx, c.owner, c.bundle, 32<<20)
	if err != nil {
		return err
	}
	if digest != c.digest {
		return agentapi.Wrap(agentapi.Unsafe, errors.New("settled capture bytes changed"))
	}
	err = withSettledBundleCopy(ctx, c.owner, c.bundle, func(bundle archive.SourceBundle) error { return visit(ctx, bundle, c.facts) })
	return err
}

// Close releases this capture once, including when the evidence handle was copied.
func (e *Proof) Close() error {
	if e == nil || e.capture == nil {
		return nil
	}
	c := e.capture
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	c.bundle = archive.SourceBundle{}
	c.reg = archive.SessionRegistration{}
	c.facts = nativefacts.SettledImportFacts{}
	c.digest = ""
	c.owner.Release(c.charged)
	c.request.Close()
	c.lease.Close()
	c.owner = nil
	c.request = nil
	c.lease = nil
	c.originalBudget = nil
	c.charged = 0
	return nil
}

func settledCaptureDigest(ctx context.Context, owner *settledReservation, value any, limit int64) (string, error) {
	const scratch int64 = 32 << 10
	if !owner.Reserve(scratch) {
		return "", agentapi.Wrap(agentapi.Limit, agentapi.ErrReadBudget)
	}
	n, err := agentmeta.JSONWireBound(ctx, value, min(limit, owner.Available()/2))
	owner.Release(scratch)
	if err != nil {
		return "", err
	}
	if !owner.Reserve(2 * n) {
		return "", agentapi.Wrap(agentapi.Limit, agentapi.ErrReadBudget)
	}
	defer owner.Release(2 * n)
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), ctx.Err()
}

func withSettledBundleCopy(ctx context.Context, owner *settledReservation, bundle archive.SourceBundle, visit func(archive.SourceBundle) error) error {
	const scratch int64 = 32 << 10
	if !owner.Reserve(scratch) {
		return agentapi.Wrap(agentapi.Limit, agentapi.ErrReadBudget)
	}
	n, err := agentmeta.JSONWireBound(ctx, bundle, min(32<<20, owner.Available()/3))
	owner.Release(scratch)
	if err != nil {
		return err
	}
	if !owner.Reserve(3 * n) {
		return agentapi.Wrap(agentapi.Limit, agentapi.ErrReadBudget)
	}
	defer owner.Release(3 * n)
	raw, err := json.Marshal(bundle)
	if err != nil {
		return err
	}
	var owned archive.SourceBundle
	if err = json.Unmarshal(raw, &owned); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return visit(owned)
}

type settledReservation struct {
	original *nativefacts.CapturedScope
	limit    int64
	used     int64
}

func (r *settledReservation) Reserve(n int64) bool {
	if n < 0 || n > r.limit-r.used {
		r.original.Refuse(n)
		return false
	}
	if !r.original.Reserve(n) {
		return false
	}
	r.used += n
	return true
}

func (r *settledReservation) Release(n int64) {
	if n < 0 || n > r.used {
		panic("unbalanced settled reservation")
	}
	r.original.Release(n)
	r.used -= n
}

func (r *settledReservation) Available() int64 { return min(r.original.Available(), r.limit-r.used) }

func (r *settledReservation) Refuse(n int64) { r.original.Refuse(n) }

type joinedLoanContext struct {
	context.Context
	original context.Context
}

func (c joinedLoanContext) Err() error {
	if err := c.original.Err(); err != nil {
		return err
	}
	return c.Context.Err()
}

func (c joinedLoanContext) Deadline() (time.Time, bool) {
	a, ok := c.Context.Deadline()
	b, bok := c.original.Deadline()
	if bok && (!ok || b.Before(a)) {
		return b, true
	}
	return a, ok
}
