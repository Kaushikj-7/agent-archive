package backfill

import (
	"context"
	"errors"
	"sync"
	"time"
	"unsafe"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

// CandidateSequence lends at most limit ordered candidates until Advance or Close.
// Next returns done only with an empty page after all candidates have been
// acknowledged. An empty page without done, or a nonempty page with done, is
// invalid. Every nonnil error is a failure. Advance acknowledges a completed prefix;
// fetching rows alone never acknowledges them. Close releases all remaining
// borrowed rows without acknowledging them. Neither operation grants authority.
type CandidateSequence interface {
	Next(context.Context, int) ([]Candidate, bool, error)
	Advance(int) error
	Close() error
}

// SliceCandidates adapts an already ordered slice without copying its contents.
// The caller retains ownership of the complete slice until the sequence closes.
// This adapter does not bound acquisition, sorting, or retention of that slice.
func SliceCandidates(candidates []Candidate) CandidateSequence {
	return &sliceCandidates{candidates: candidates}
}

type sliceCandidates struct {
	candidates []Candidate
	borrowed   int
	closed     bool
}

func (s *sliceCandidates) Next(ctx context.Context, limit int) ([]Candidate, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if s.closed || s.borrowed != 0 || limit <= 0 {
		return nil, false, errors.New("candidate sequence is closed or has unfinished work")
	}
	if len(s.candidates) == 0 {
		return nil, true, nil
	}
	s.borrowed = min(limit, len(s.candidates))
	return s.candidates[:s.borrowed], false, nil
}

func (s *sliceCandidates) Advance(n int) error {
	if s.closed || n < 0 || n > s.borrowed {
		return errors.New("candidate sequence acknowledgement exceeds borrowed work")
	}
	s.candidates = s.candidates[n:]
	s.borrowed -= n
	return nil
}

func (s *sliceCandidates) Close() error {
	s.candidates, s.borrowed, s.closed = nil, 0, true
	return nil
}

// RegistrationResult retains the IDs requested by legacy slice callers.
// Close releases their shared-ledger charge when no copy of the result is used.
// Production sequence consumers receive RegistrationSummary and AfterHold deltas.
type RegistrationResult struct {
	RegistrationSummary
	Sessions  []string
	Subagents []string
	owner     *registrationIDs
	retained  *RegistrationResult
}

// Close releases this result's charge once, including through copied results.
func (r *RegistrationResult) Close() {
	if r.owner != nil {
		r.owner.close()
	}
}

// prepareIDs reserves both the hold delta and optional legacy result before
// registration/request effects. Recording the completed IDs cannot then fail.
func (r *RegistrationResult) prepareIDs(nativeChild bool, id string, children []string) error {
	if nativeChild {
		ids := make([]string, 1, 1+len(children))
		ids[0] = id
		ids = append(ids, children...)
		return r.prepareIDList(false, ids)
	}
	if err := r.prepareIDList(true, []string{id}); err != nil {
		return err
	}
	return r.prepareIDList(false, children)
}

func (r *RegistrationResult) prepareIDList(sessions bool, ids []string) error {
	values := &r.Subagents
	if sessions {
		values = &r.Sessions
	}
	if r.owner != nil {
		if err := r.owner.prepare(values, ids); err != nil {
			return err
		}
	}
	if r.retained != nil {
		return r.retained.prepareIDList(sessions, ids)
	}
	return nil
}

func (r *RegistrationResult) recordIDs(nativeChild bool, id string, children []string) {
	if nativeChild {
		r.Subagents = append(r.Subagents, id)
	} else {
		r.Sessions = append(r.Sessions, id)
	}
	r.Subagents = append(r.Subagents, children...)
	if r.retained != nil {
		r.retained.recordIDs(nativeChild, id, children)
	}
}

type registrationIDs struct {
	reservation *agentapi.NativeReadReservation
	bytes       int64
	closed      sync.Once
}

func (r *registrationIDs) close() {
	r.closed.Do(func() { r.reservation.Release(r.bytes) })
}

func (r *registrationIDs) prepare(dst *[]string, values []string) error {
	var stringsBytes int64
	for _, value := range values {
		stringsBytes += int64(len(value))
	}
	capacity := cap(*dst)
	if len(*dst)+len(values) > capacity {
		capacity = max(2*capacity, len(*dst)+len(values))
	}
	arrayBytes := int64(capacity) * int64(unsafe.Sizeof(""))
	growth := stringsBytes
	if capacity != cap(*dst) {
		// Charge both arrays during growth; the old array is still live here.
		growth += arrayBytes
	}
	if !r.reservation.Reserve(growth) {
		return agentapi.ReadBudgetLimit(errors.New("registration result exceeds shared data budget; close the result before retrying"))
	}
	r.bytes += growth
	if capacity != cap(*dst) {
		next := make([]string, len(*dst), capacity)
		copy(next, *dst)
		oldBytes := int64(cap(*dst)) * int64(unsafe.Sizeof(""))
		*dst = next
		r.reservation.Release(oldBytes)
		r.bytes -= oldBytes
	}
	return nil
}

// RunSequence consumes bounded ordered work and returns scalar totals. AfterHold
// borrows only this hold's ID deltas; a callback retaining them owns its copies.
// The sequence is closed on completion, cancellation and producer error.
func (r Registration) RunSequence(sequence CandidateSequence) (RegistrationSummary, error) {
	return r.runSequence(sequence, nil)
}

func (r Registration) runSequence(sequence CandidateSequence, retained *RegistrationResult) (summary RegistrationSummary, resultErr error) {
	if sequence == nil {
		return summary, errors.New("registration requires an ordered candidate sequence")
	}
	defer func() { resultErr = errors.Join(resultErr, sequence.Close()) }()
	ctx := r.Context
	if ctx == nil {
		ctx = context.Background()
	}
	limit := maxHoldSteps
	if r.MaxHoldSteps > 0 {
		limit = min(limit, r.MaxHoldSteps)
	}
	// Project-key observations are shared for this invocation as before. They
	// remain observations; each hold checks current configuration and policy.
	repoKeys := &registrationRepoKeys{values: map[string]string{}, reservation: r.ReadBudget.Reservation()}
	defer func() { repoKeys.reservation.Release(repoKeys.bytes) }()
	for {
		if r.stopped(ctx) {
			return summary, ErrStopped
		}
		candidates, done, err := sequence.Next(ctx, limit)
		if err != nil {
			return summary, err
		}
		if done && len(candidates) == 0 {
			return summary, nil
		}
		if done || len(candidates) == 0 || len(candidates) > limit {
			return summary, errors.New("candidate sequence returned an invalid page or completion; rebuild the plan")
		}
		if err := r.runPage(ctx, sequence, candidates, repoKeys, retained, &summary); err != nil {
			return summary, err
		}
	}
}

func (r Registration) stopped(ctx context.Context) bool {
	return ctx.Err() != nil || r.Stop != nil && r.Stop()
}

func (r Registration) runPage(ctx context.Context, sequence CandidateSequence, candidates []Candidate, repoKeys *registrationRepoKeys, retained *RegistrationResult, summary *RegistrationSummary) error {
	// Candidates and their selection evidence are borrowed from the producer.
	// Charge the additional bounded vector and mutable parent/child bookkeeping
	// before allocating it. State's decoded stages retain their own charges.
	reservation := r.ReadBudget.Reservation()
	bytes := int64(len(candidates)) * int64(unsafe.Sizeof(parentWork{})+unsafe.Sizeof((*parentWork)(nil)))
	for _, candidate := range candidates {
		bytes += 1024 // IDs, links and map growth for the active parent.
		for _, child := range candidate.Subagents {
			bytes += 4096 + 4*int64(len(child.Path)+len(child.AgentID)+len(candidate.NativeSessionID))
		}
	}
	if !reservation.Reserve(bytes) {
		return agentapi.ReadBudgetLimit(errors.New("active registration work exceeds shared data budget; retry after other work finishes"))
	}
	defer reservation.Release(bytes)
	works := make([]*parentWork, len(candidates))
	for i, candidate := range candidates {
		works[i] = &parentWork{c: candidate}
		if r.ReadBudget != nil {
			// A partial parent owns its decoded state across holds. Completed
			// parents release it even while later rows of this page remain.
			works[i].store, works[i].closeStore = r.Store.WithReadBudget(ctx, r.ReadBudget)
		}
	}
	defer closeRegistrationWork(works)
	for i := 0; i < len(works); {
		if r.stopped(ctx) {
			return errors.Join(ErrStopped, r.cancelPrepared(ctx, works[i:]))
		}
		active := works[i:]
		if err := r.checkChats(ctx, active); err != nil {
			return err
		}
		if err := r.resolveRepoKeys(active, repoKeys); err != nil {
			return err
		}
		r.validateGroupProof(ctx, active)
		if err := r.stageGroup(ctx, active); err != nil {
			if r.stopped(ctx) {
				return errors.Join(ErrStopped, err, r.cancelPrepared(ctx, active))
			}
			return err
		}
		if r.stopped(ctx) {
			return errors.Join(ErrStopped, r.cancelPrepared(ctx, active))
		}
		delta := RegistrationResult{owner: &registrationIDs{reservation: r.ReadBudget.Reservation()}, retained: retained}
		before := i
		err := r.hold(works, &i, &delta)
		cleanupErr := r.cleanObsoleteStages(ctx, active)
		delta.RegisteredSessions, delta.RegisteredSubagents = len(delta.Sessions), len(delta.Subagents)
		summary.add(delta.RegistrationSummary)
		if r.AfterHold != nil && len(delta.Sessions)+len(delta.Subagents) != 0 {
			err = errors.Join(err, r.AfterHold(delta.Sessions, delta.Subagents))
		}
		delta.Close()
		err = errors.Join(err, cleanupErr)
		if err != nil {
			if errors.Is(err, ErrStopped) {
				return errors.Join(err, r.cancelPrepared(ctx, works[i:]))
			}
			return err
		}
		if i > before {
			if err := sequence.Advance(i - before); err != nil {
				return err
			}
			closeRegistrationWork(works[before:i])
			clear(works[before:i])
		}
		// Leave hooks their normal gap even when the next hold belongs to the
		// next page. Page rollover must not join two holds into one busy span.
		timer := time.NewTimer(holdGap)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	return nil
}

func closeRegistrationWork(works []*parentWork) {
	for _, work := range works {
		if work != nil && work.closeStore != nil {
			work.closeStore()
			work.closeStore = nil
		}
	}
}

func (s *RegistrationSummary) add(delta RegistrationSummary) {
	s.RegisteredSessions += delta.RegisteredSessions
	s.RegisteredSubagents += delta.RegisteredSubagents
	s.AlreadyArchived += delta.AlreadyArchived
	s.Gone += delta.Gone
	s.NotAdmitted += delta.NotAdmitted
	s.StartInFuture += delta.StartInFuture
	s.Invalid += delta.Invalid
	s.SubagentsInvalid += delta.SubagentsInvalid
	for harness, count := range delta.IndependentByHarness {
		if s.IndependentByHarness == nil {
			s.IndependentByHarness = map[string]int{}
		}
		s.IndependentByHarness[harness] += count
	}
}

// Repository observations retain their charge across page and hold boundaries.
type registrationRepoKeys struct {
	values      map[string]string
	reservation *agentapi.NativeReadReservation
	bytes       int64
}

func (k *registrationRepoKeys) add(root, key string) error {
	// Include map buckets and their growth overlap, in addition to string data.
	bytes := 256 + int64(len(root)+len(key))
	if !k.reservation.Reserve(bytes) {
		return agentapi.ReadBudgetLimit(errors.New("registration project observations exceed shared data budget; retry after other work finishes"))
	}
	k.bytes += bytes
	k.values[root] = key
	return nil
}
