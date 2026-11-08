package discovery

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
)

func newNativeInventory(ctx context.Context, roots []archive.NativeRoot, budget *agentapi.NativeReadBudget, validate NativeLocatorValidator) (*NativeInventory, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if budget == nil || validate == nil {
		return nil, agentapi.ErrInvalidSourceBinding
	}
	root, err := nativeInventoryRoot(roots)
	if err != nil {
		return nil, err
	}
	return &NativeInventory{ctx: ctx, root: root, budget: budget, validate: validate, remaining: Budget, tokens: map[string]nativeSelection{}}, nil
}

func (l *NativeInventory) setNonce() error {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	l.nonce = hex.EncodeToString(nonce[:])
	return nil
}

// NewNativeImportInventory owns an ephemeral historical observation pass. It
// never reads, restores or persists the collector's discovery catalog.
func NewNativeImportInventory(ctx context.Context, roots []archive.NativeRoot, budget *agentapi.NativeReadBudget, validate NativeLocatorValidator) (*NativeInventory, error) {
	start := time.Now()
	l, err := newNativeInventory(ctx, roots, budget, validate)
	if err != nil {
		return nil, err
	}
	defer func() { elapsed := time.Since(start); l.remaining -= elapsed; l.counts.Active += elapsed }()
	if l.root.Agent != "muse" {
		return nil, l.failConstruction(agentapi.ErrInvalidSourceBinding)
	}
	if !l.reserve(8 * (maxCatalogBytes / 32)) {
		return nil, l.failConstruction(errCatalogBudget)
	}
	l.historical = true
	l.reset(nil, "")
	if err := l.setNonce(); err != nil {
		return nil, l.failConstruction(err)
	}
	return l, nil
}

// NativeImportCandidates borrows bounded namespace observations until Close.
// Complete reports namespace coverage only; each ID still needs Locate and
// same-source proof. Partial observations never establish unique source identity.
type NativeImportCandidates struct {
	IDs      []string
	Complete bool
}

// Candidates acquires the same bounded complete dirent pages used by ordinary
// lookup. It freezes this pass once; it does not start a second census on retry.
func (l *NativeInventory) Candidates(ctx context.Context) (NativeImportCandidates, error) {
	if err := l.allowed(ctx); err != nil {
		return NativeImportCandidates{}, err
	}
	if !l.historical {
		return NativeImportCandidates{}, agentapi.ErrInvalidSourceBinding
	}
	if l.projected {
		return NativeImportCandidates{l.eligible, l.complete}, nil
	}
	l.projected = true
	start := time.Now()
	l.deadline = start.Add(l.remaining - Budget/4)
	defer l.timed(start)
	work, cancel := context.WithDeadline(ctx, l.deadline)
	defer cancel()
	if err := l.acquire(work, l.directory); err != nil {
		return NativeImportCandidates{}, err
	}
	l.eligible = l.progress.Owners
	for _, d := range l.progress.Directories {
		if !completeNative(d) {
			return NativeImportCandidates{l.eligible, false}, nil
		}
	}
	if l.stopped {
		return NativeImportCandidates{l.eligible, false}, nil
	}
	if err := l.coverage(work); err != nil {
		return NativeImportCandidates{}, err
	}
	l.complete = true
	l.purposes = make([]uint8, len(l.eligible))
	for i := range l.purposes {
		l.purposes[i] = 4
	}
	return NativeImportCandidates{l.eligible, true}, nil
}

func (l *NativeInventory) collectImportOwners() {
	owners := make([]string, 0, len(l.progress.Owners))
	for _, d := range l.progress.Directories {
		owners = append(owners, d.Matches...)
	}
	slices.Sort(owners)
	l.progress.Owners = slices.Compact(owners)
	sum := sha256.Sum256([]byte(strings.Join(l.progress.Owners, "\x00")))
	l.progress.Query = hex.EncodeToString(sum[:])
}
