package collector

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"sync"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
)

// NativeLocatorPass freezes the pending ordinary owner queries and committed
// roots after loading work. The factory borrows Store under the collector lock.
// ReadBudget is the existing source ledger; it is never a renewed allowance.
type NativeLocatorPass struct {
	ReadBudget *agentapi.NativeReadBudget
	Queries    []agentapi.LocatorRequest
	Roots      []archive.NativeRoot
	Store      *state.Store
}

var errNativeLocatorComposition = errors.New("native locator factory and borrowed lookup are mutually exclusive")

func pendingNativeRoot(reg archive.SessionRegistration, opts Options) (archive.NativeRoot, bool) {
	if reg.Harness.Name != "muse" || !reg.ReadsTranscriptFile() || reg.TranscriptPath != "" || reg.NativeBinding != nil || reg.CodexBinding != nil || reg.NativeSessionID == "" || reg.Origin == archive.SessionOriginImport || reg.HistoricalProject != nil || reg.CaptureFrozen || reg.NativeChild || reg.ParentSessionID != "" || reg.ParentNativeSessionID != "" || reg.PreviousGenerationID != "" {
		return archive.NativeRoot{}, false
	}
	for _, root := range opts.NativeRoots {
		if root.Agent == "muse" && root.Validate() == nil {
			return root, true
		}
	}
	return archive.NativeRoot{}, false
}

func pendingNativeEligible(reg archive.SessionRegistration, opts Options) bool {
	_, ok := pendingNativeRoot(reg, opts)
	return ok && (opts.NativeLocators != nil || opts.NativeLocatorFactory != nil)
}

func (p *pass) openNativeInventory() (func() error, error) {
	factory := p.opts.NativeLocatorFactory
	if factory == nil {
		return nil, nil
	}
	if p.opts.NativeLocators != nil {
		return nil, errNativeLocatorComposition
	}
	// Query snapshots share the existing ledger too. Count before allocation,
	// and mirror the existing discovery catalog's 8192-owner bound.
	count := len(p.startupCandidates)
	for _, reg := range p.registrations {
		if !p.opts.selectsSession(reg) || p.opts.AcceptSession != nil && !p.opts.AcceptSession(reg) {
			continue
		}
		if _, ok := pendingNativeRoot(reg, p.opts); ok {
			count++
		}
	}
	if count > 8192 {
		return nil, agentapi.ReadBudgetLimit(errors.New("native pending query set exceeds catalog bound"))
	}
	budget := p.opts.sourcePasses.env.ReadBudget
	// #nosec G115 -- fixed compiled struct sizes; count is bounded above by 8192.
	charge := int64(count)*(int64(reflect.TypeFor[agentapi.LocatorRequest]().Size())+128) + int64(len(p.opts.NativeRoots))*int64(reflect.TypeFor[archive.NativeRoot]().Size())
	if !budget.ReserveUnscoped(charge) {
		return nil, agentapi.ReadBudgetLimit(errors.New("native query snapshot unavailable"))
	}
	scope := NativeLocatorPass{ReadBudget: budget, Roots: append([]archive.NativeRoot(nil), p.opts.NativeRoots...), Store: p.local, Queries: make([]agentapi.LocatorRequest, 0, count)}
	for _, reg := range p.registrations {
		if !p.opts.selectsSession(reg) || p.opts.AcceptSession != nil && !p.opts.AcceptSession(reg) {
			continue
		}
		if root, ok := pendingNativeRoot(reg, p.opts); ok {
			scope.Queries = append(scope.Queries, agentapi.LocatorRequest{NativeID: reg.NativeSessionID, Root: root})
		}
	}
	for _, candidate := range p.startupCandidates {
		scope.Queries = append(scope.Queries, agentapi.LocatorRequest{Purpose: agentapi.LocatorStartupCandidate, NativeID: candidate.intent.NativeSessionID, Root: candidate.root, Observed: candidate.intent.ObservedAt})
	}
	sort.Slice(scope.Queries, func(i, j int) bool { return scope.Queries[i].NativeID < scope.Queries[j].NativeID })
	lookup, closeLookup, err := factory(p.ctx, scope)
	var once sync.Once
	var closeErr error
	finalize := func() error {
		once.Do(func() {
			if closeLookup != nil {
				closeErr = closeLookup()
			}
			budget.ReleaseUnscoped(charge)
		})
		return closeErr
	}
	if err != nil {
		return finalize, err
	}
	if lookup != nil && closeLookup == nil {
		return finalize, errors.New("owned native lookup requires a finalizer")
	}
	p.opts.NativeLocators = lookup
	p.opts.sourcePasses.env.NativeLocators = lookup
	return finalize, nil
}

type pendingNativeReader struct {
	reg  archive.SessionRegistration
	opts Options
}

func (r pendingNativeReader) Signature(ctx context.Context) (sourceState, error) {
	if err := ctx.Err(); err != nil {
		return sourceState{}, err
	}
	return sourceState{}, agentapi.Wrap(agentapi.Unavailable, errors.New("native locator pending"))
}

func (r pendingNativeReader) Filter(ctx context.Context, adapter archive.Adapter, limit int64) (archive.FilteredTranscript, sourceState, error) {
	var empty archive.FilteredTranscript
	root, ok := pendingNativeRoot(r.reg, r.opts)
	if r.opts.NativeLocators == nil {
		return empty, sourceState{}, agentapi.Wrap(agentapi.Unavailable, errors.New("native locator pending"))
	}
	if !ok || r.opts.retainedOwner == nil {
		return empty, sourceState{}, agentapi.ErrInvalidSourceBinding
	}
	result, err := r.opts.NativeLocators.Locate(ctx, agentapi.LocatorRequest{NativeID: r.reg.NativeSessionID, Root: root})
	if err != nil {
		return empty, sourceState{}, err
	}
	if result.Status != agentapi.LocatorFound || result.Current != nil || len(result.Candidates) != 1 || result.Revision == "" {
		return empty, sourceState{}, agentapi.Wrap(agentapi.Unavailable, errors.New("native locator incomplete or conflicting"))
	}
	ref := result.Candidates[0]
	if ref.Kind != archive.SourceKindFile || ref.Key != r.reg.NativeSessionID || ref.Path == "" {
		return empty, sourceState{}, agentapi.ErrInvalidSourceBinding
	}
	reader := providerReader{nativeRoot: &root, nativeLocators: r.opts.NativeLocators, nativeSelection: result.Revision, ref: ref, harness: r.reg.Harness.Name, startedAt: r.reg.SessionStartedAt, sources: r.opts.Sources, passes: r.opts.sourcePasses, resourceOwner: r.opts.retainedOwner, admission: sourceAdmission(r.reg)}
	return reader.Filter(ctx, adapter, limit)
}

// Only native IDs in the factory's immutable root/query domain can rank pending
// slots. Other live/requested/bound/frozen slots retain their exact positions.
func (p *pass) orderPendingNative(registrations []archive.SessionRegistration) {
	if p.opts.NativeLocatorFactory == nil {
		return
	}
	ordering, ok := p.opts.NativeLocators.(interface{ PendingOrder() []string })
	if !ok {
		return
	}
	order := ordering.PendingOrder()
	if len(order) > 8192 {
		return
	}
	rank := make(map[string]int, len(order))
	for i, id := range order {
		if _, seen := rank[id]; !seen {
			rank[id] = i
		}
	}
	positions := make([]int, 0, len(order))
	for i, reg := range registrations {
		if pendingNativeEligible(reg, p.opts) {
			positions = append(positions, i)
		}
	}
	desired := make([]int, len(positions))
	for i := range desired {
		desired[i] = i
	}
	sort.SliceStable(desired, func(i, j int) bool {
		a, knownA := rank[registrations[positions[desired[i]]].NativeSessionID]
		b, knownB := rank[registrations[positions[desired[j]]].NativeSessionID]
		if knownA != knownB {
			return knownA
		}
		return knownA && a < b
	})
	// Apply only pending-slot cycles with one registration temporary, avoiding a
	// second owner census allocation inside the shared cache headroom.
	for i := range desired {
		if desired[i] == i {
			continue
		}
		saved := registrations[positions[i]]
		current := i
		for {
			next := desired[current]
			desired[current] = current
			if next == i {
				registrations[positions[current]] = saved
				break
			}
			registrations[positions[current]] = registrations[positions[next]]
			current = next
		}
	}
}

func closeSourceAndNative(sources, native func() error) error {
	err := sources()
	if native != nil {
		err = errors.Join(err, native())
	}
	return err
}

func (p *pass) configureNativeInventory() (func() error, error) {
	finalize, err := p.openNativeInventory()
	if err == nil {
		return finalize, nil
	}
	if errors.Is(err, errNativeLocatorComposition) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return finalize, err
	}
	p.opts.NativeLocators = nil
	p.opts.sourcePasses.env.NativeLocators = nil
	p.result.Errors["native-inventory"] = errors.New("native inventory unavailable; retry next pass")
	return finalize, nil
}

func (p *pass) holdUnopenedScan(reg archive.SessionRegistration) bool {
	id := reg.ArchiveSessionID
	if pendingNativeEligible(reg, p.opts) && p.opts.NativeLocators == nil {
		p.pending++
		p.result.Waiting = append(p.result.Waiting, id)
		p.opts.progress(id, false)
		return true
	}
	if !p.unreadable[id] {
		return false
	}
	if !p.durableCounted[id] {
		p.pending++
	}
	p.opts.progress(id, false)
	return true
}
