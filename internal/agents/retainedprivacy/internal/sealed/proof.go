package sealed

import (
	"bytes"
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
)

// Facts retain original stage/capture policy separately from current derivation.
type Facts struct {
	OwnerSHA256    string                  `json:"owner_sha256"`
	CoveredToken   string                  `json:"covered_token"`
	HadRequest     bool                    `json:"had_request"`
	StageSHA256    string                  `json:"stage_sha256"`
	Input          archive.SourceReference `json:"input"`
	Output         archive.SourceReference `json:"output"`
	OriginalPolicy Policy                  `json:"original_policy"`
	TargetPolicy   Policy                  `json:"target_policy"`
}

// Proof owns fixed retained work. Copies share one terminal Close.
type Proof struct{ transform *transform }

type transform struct {
	mu        sync.Mutex
	lease     *Lease
	bundle    archive.SourceBundle
	source    archive.CompressedSource
	facts     Facts
	bundleSHA string
	release   func()
	closed    bool
}

// New transfers only a completed fixed producer's output into proof custody.
func New(lease *Lease, bundle archive.SourceBundle, source archive.CompressedSource, facts Facts, digest string, release func()) *Proof {
	return &Proof{transform: &transform{lease: lease, bundle: bundle, source: source, facts: facts, bundleSHA: digest, release: release}}
}

// Digest charges bounded encoding scratch to the captured owner.
func Digest(ctx context.Context, budget *agentapi.NativeReadBudget, value any) (string, error) {
	owner := budget.Reservation()
	const scratch int64 = 32 << 10
	if !owner.Reserve(scratch) {
		return "", agentapi.ErrReadBudget
	}
	n, err := agentmeta.JSONWireBound(ctx, value, min(int64(32<<20), owner.Available()/2))
	owner.Release(scratch)
	if err != nil {
		return "", err
	}
	if !owner.Reserve(2 * n) {
		return "", agentapi.ErrReadBudget
	}
	defer owner.Release(2 * n)
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), ctx.Err()
}

// WithVerifiedTransform lends detached output only for the exact host request.
// The callback receives no native capture permission or original selecting proof.
func (p *Proof) WithVerifiedTransform(ctx context.Context, expected *Request, budget *agentapi.NativeReadBudget, visit func(context.Context, archive.SourceBundle, archive.CompressedSource, Facts) error) (err error) {
	if p == nil || p.transform == nil || visit == nil || ctx == nil {
		return agentapi.ErrClosed
	}
	t := p.transform
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return agentapi.ErrClosed
	}
	if err := t.lease.request.matches(expected, budget); err != nil {
		return err
	}
	if err := t.lease.Check(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ctx, closeLoan := joinLoanContext(ctx, t.lease.Context())
	defer closeLoan()
	pinned := t.lease.Budget()
	digest, err := Digest(ctx, pinned, t.bundle)
	if err != nil || digest != t.bundleSHA {
		return errors.Join(errors.New("retained transform bundle changed"), err)
	}
	sum := sha256.Sum256(t.source.Bytes)
	if hex.EncodeToString(sum[:]) != t.source.SHA256 || t.facts.Output.SHA256 != t.source.SHA256 || t.facts.Output.CompressedBytes != len(t.source.Bytes) {
		return errors.New("retained transform source changed")
	}
	owner := pinned.Reservation()
	const scratch int64 = 32 << 10
	if !owner.Reserve(scratch) {
		return agentapi.ErrReadBudget
	}
	n, err := agentmeta.JSONWireBound(ctx, t.bundle, min(int64(32<<20), owner.Available()/3))
	owner.Release(scratch)
	if err != nil {
		return err
	}
	charge := 3*n + int64(len(t.source.Bytes))
	if !owner.Reserve(charge) {
		return agentapi.ErrReadBudget
	}
	defer owner.Release(charge)
	raw, err := json.Marshal(t.bundle)
	if err != nil {
		return err
	}
	var detached archive.SourceBundle
	if err := json.Unmarshal(raw, &detached); err != nil {
		return err
	}
	source := archive.CompressedSource{Bytes: bytes.Clone(t.source.Bytes), SHA256: t.source.SHA256}
	if err := t.lease.Check(); err != nil {
		return err
	}
	err = visit(ctx, detached, source, t.facts)
	return errors.Join(err, t.lease.Check(), ctx.Err())
}

// Close releases output and request custody exactly once across copied proofs.
func (p *Proof) Close() error {
	if p == nil || p.transform == nil {
		return nil
	}
	t := p.transform
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	t.closed = true
	t.bundle = archive.SourceBundle{}
	t.source = archive.CompressedSource{}
	t.facts = Facts{}
	t.release()
	t.lease.Close()
	return t.lease.request.Close()
}

// A loan inherits both host cancellation and the original operation lifetime.
func joinLoanContext(caller, original context.Context) (context.Context, func()) {
	child, cancel := context.WithCancelCause(caller)
	stop := context.AfterFunc(original, func() { cancel(context.Cause(original)) })
	return loanContext{Context: child, original: original}, func() { stop(); cancel(nil) }
}

type loanContext struct {
	context.Context
	original context.Context
}

func (c loanContext) Err() error {
	if err := c.original.Err(); err != nil {
		return err
	}
	return c.Context.Err()
}

func (c loanContext) Deadline() (time.Time, bool) {
	a, ok := c.Context.Deadline()
	b, bok := c.original.Deadline()
	if bok && (!ok || b.Before(a)) {
		return b, true
	}
	return a, ok
}
