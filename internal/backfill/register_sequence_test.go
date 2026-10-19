package backfill

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

// registrationSequenceFixture supplies actual private state, not a fake hold.
func registrationSequenceFixture(t *testing.T, count, children int) (Registration, []Candidate) {
	t.Helper()
	r, first, cfg := stagingFixture(t)
	r.DurableSources, r.MaxHoldSteps = false, 0
	r.ReadBudget = agentapi.NewNativeReadBudget(agentapi.PassReadBudget)
	candidates := make([]Candidate, count)
	for i := range candidates {
		candidate := first
		candidate.NativeSessionID = fmt.Sprintf("sequence-%03d", i)
		candidate.TranscriptPath = filepath.Join(first.ProjectRoot, candidate.NativeSessionID+".jsonl")
		if err := os.WriteFile(candidate.TranscriptPath, []byte("{}\n"), 0600); err != nil {
			t.Fatal(err)
		}
		candidates[i] = candidate
	}
	for i := range children {
		child := Subagent{AgentID: fmt.Sprintf("child-%03d", i), Path: filepath.Join(first.ProjectRoot, fmt.Sprintf("child-%03d.jsonl", i))}
		if err := os.WriteFile(child.Path, []byte("{}\n"), 0600); err != nil {
			t.Fatal(err)
		}
		candidates[0].Subagents = append(candidates[0].Subagents, child)
	}
	if err := config.Save(r.Home, cfg); err != nil {
		t.Fatal(err)
	}
	return r, candidates
}

type observedCandidateSequence struct {
	CandidateSequence
	t             *testing.T
	r             Registration
	page          int
	acknowledged  int
	nextCalls     int
	closed        int
	produced      int
	beforeAdvance func(int)
	nextError     error
	closeError    error
}

func (s *observedCandidateSequence) outsideHold() {
	s.t.Helper()
	unlock, err := local.NamedLock(s.r.Home, "hooks.lock")
	if err != nil {
		s.t.Fatal("sequence I/O ran inside hooks.lock", err)
	}
	unlock()
}

func (s *observedCandidateSequence) Next(ctx context.Context, limit int) ([]Candidate, error) {
	s.outsideHold()
	s.nextCalls++
	if limit > maxHoldSteps || limit <= 0 {
		s.t.Fatal("unbounded candidate request", limit)
	}
	if s.produced != s.acknowledged {
		s.t.Fatal("new page fetched before durable disposition", s.produced, s.acknowledged)
	}
	if s.nextCalls > 1 && s.nextError != nil {
		return nil, s.nextError
	}
	page, err := s.CandidateSequence.Next(ctx, min(limit, s.page))
	s.produced += len(page)
	return page, err
}

func (s *observedCandidateSequence) Advance(n int) error {
	s.outsideHold()
	if s.beforeAdvance != nil {
		s.beforeAdvance(n)
	}
	if err := s.CandidateSequence.Advance(n); err != nil {
		return err
	}
	s.acknowledged += n
	return nil
}

func (s *observedCandidateSequence) Close() error {
	s.outsideHold()
	s.closed++
	return errors.Join(s.CandidateSequence.Close(), s.closeError)
}

func TestRegistrationSequencePagesPreserveParentChildren(t *testing.T) {
	t.Parallel()
	for _, page := range []int{1, 7, 8, 9, 49, 50} {
		t.Run(fmt.Sprint(page), func(t *testing.T) {
			t.Parallel()
			r, candidates := registrationSequenceFixture(t, page+2, maxHoldSteps+3)
			sequence := &observedCandidateSequence{CandidateSequence: SliceCandidates(candidates), t: t, r: r, page: page}
			var registered []string
			children := 0
			r.AfterHold = func(sessions, subagents []string) error {
				sequence.outsideHold()
				for _, id := range sessions {
					reg, found, err := r.Store.LoadRegistration(id)
					if err != nil || !found || !reg.InBatch(r.Batch) || !reg.AdmittedAt.Equal(r.AdmittedAt) {
						t.Fatal("delta preceded durable ownership", found, reg, err)
					}
					registered = append(registered, reg.NativeSessionID)
				}
				children += len(subagents)
				return nil
			}
			sequence.beforeAdvance = func(n int) {
				if len(registered) != sequence.acknowledged+n {
					t.Fatal("progress acknowledged before callback", registered, sequence.acknowledged, n)
				}
				if sequence.acknowledged == 0 && children != len(candidates[0].Subagents) {
					t.Fatal("partial parent acknowledged without its selected children", children)
				}
			}
			summary, err := r.RunSequence(sequence)
			if err != nil || summary.RegisteredSessions != len(candidates) || summary.RegisteredSubagents != len(candidates[0].Subagents) || summary.IndependentByHarness[archive.HarnessClaude] != len(candidates) {
				t.Fatal("sequence outcomes", summary, err)
			}
			want := make([]string, len(candidates))
			for i, candidate := range candidates {
				want[i] = candidate.NativeSessionID
			}
			if !reflect.DeepEqual(registered, want) || sequence.acknowledged != len(candidates) || sequence.closed != 1 || sequence.nextCalls < 3 {
				t.Fatal("page or equal-time input order changed", registered, sequence)
			}
			if used, _ := r.ReadBudget.Charged(); used != 0 {
				t.Fatal("scalar consumer retained page/state/result charges", used)
			}
		})
	}
}

func TestRegistrationSequenceStopsWithoutAcknowledgingPartialParent(t *testing.T) {
	t.Parallel()
	r, candidates := registrationSequenceFixture(t, 2, 3)
	r.MaxHoldSteps = 1
	ctx, cancel := context.WithCancel(t.Context())
	r.Context = ctx
	r.Stop = func() bool {
		children, err := r.Store.LoadSubagentCandidates()
		if err != nil {
			t.Fatal(err)
		}
		if len(children) != 0 {
			cancel()
		}
		return false
	}
	sequence := &observedCandidateSequence{CandidateSequence: SliceCandidates(candidates), t: t, r: r, page: 1}
	summary, err := r.RunSequence(sequence)
	if !errors.Is(err, ErrStopped) || summary.RegisteredSessions != 0 || sequence.acknowledged != 0 || sequence.nextCalls != 1 || sequence.closed != 1 {
		t.Fatal("cancellation advanced unfinished parent", summary, sequence, err)
	}
	if used, _ := r.ReadBudget.Charged(); used != 0 {
		t.Fatal("cancellation retained charges", used)
	}
	// Rebuild the disposable sequence and reconcile existing reservations/IDs.
	r.Context, r.Stop = t.Context(), nil
	result, err := r.Run(candidates)
	defer result.Close()
	if err != nil || len(result.Sessions) != 2 || len(result.Subagents) != 3 {
		t.Fatal("restart lost partial parent/child ownership", result, err)
	}
}

func TestRegistrationSequenceProducerAndCallbackErrorsKeepProgress(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"producer", "callback", "joined_eof"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			r, candidates := registrationSequenceFixture(t, 2, 0)
			failure, cleanup := errors.New("synthetic sequence failure"), errors.New("synthetic sequence close")
			sequence := &observedCandidateSequence{CandidateSequence: SliceCandidates(candidates), t: t, r: r, page: 1, closeError: cleanup}
			wantAcknowledged := 1
			switch phase {
			case "producer":
				sequence.nextError = failure
			case "callback":
				wantAcknowledged = 0
				r.AfterHold = func([]string, []string) error { return failure }
			case "joined_eof":
				sequence.nextError = errors.Join(io.EOF, failure)
			}
			summary, err := r.RunSequence(sequence)
			if !errors.Is(err, failure) || !errors.Is(err, cleanup) || summary.RegisteredSessions != 1 || sequence.acknowledged != wantAcknowledged || sequence.closed != 1 {
				t.Fatal("error or unfinished tail hidden", summary, sequence, err)
			}
			if used, _ := r.ReadBudget.Charged(); used != 0 {
				t.Fatal("error retained charges", used)
			}
		})
	}
}

func TestRegistrationResultRetainsOnlyItsOwnedCharge(t *testing.T) {
	t.Parallel()
	r, candidates := registrationSequenceFixture(t, 2, 0)
	budget := r.ReadBudget
	external := budget.Reservation()
	if !external.Reserve(37) {
		t.Fatal("external charge refused")
	}
	defer external.Release(37)
	end := budget.BeginScope(1 << 20)
	result, err := r.Run(candidates)
	end()
	if err != nil || result.RegisteredSessions != 2 || len(result.Sessions) != 2 {
		t.Fatal("slice adapter changed outcomes", result, err)
	}
	if used, _ := budget.Charged(); used <= 37 {
		t.Fatal("live legacy result escaped charging", used)
	}
	copy := result
	result.Close()
	copy.Close()
	if used, _ := budget.Charged(); used != 37 {
		t.Fatal("result close consumed another owner", used)
	}
}

func TestRegistrationReportingPressurePrecedesDurableAdmission(t *testing.T) {
	t.Parallel()
	r, candidates := registrationSequenceFixture(t, 1, 0)
	candidate := candidates[0]
	id, _, err := r.Store.EnsureArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.Claude, NativeID: candidate.NativeSessionID})
	if err != nil {
		t.Fatal(err)
	}
	budget := agentapi.NewNativeReadBudget(1)
	delta := RegistrationResult{owner: &registrationIDs{reservation: budget.Reservation()}}
	index := 0
	if err := r.hold([]*parentWork{{c: candidate, id: id}}, &index, &delta); !errors.Is(err, agentapi.ErrReadBudget) || index != 0 {
		t.Fatal("reporting pressure advanced parent", index, err)
	}
	delta.Close()
	if _, found, err := r.Store.LoadRegistration(id); err != nil || found {
		t.Fatal("reporting refusal followed durable admission", found, err)
	}
	requests, err := r.Store.LoadRequests()
	if err != nil || len(requests) != 0 || len(delta.Sessions)+len(delta.Subagents) != 0 {
		t.Fatal("reporting refusal queued unreported work", requests, delta, err)
	}
	result, err := r.Run(candidates)
	defer result.Close()
	if err != nil || len(result.Sessions) != 1 || result.Sessions[0] != id {
		t.Fatal("resource retry lost reserved identity", result, err)
	}
	reg, found, err := r.Store.LoadRegistration(id)
	if err != nil || !found || !reg.InBatch(r.Batch) || !reg.AdmittedAt.Equal(r.AdmittedAt) {
		t.Fatal("resource retry changed provenance", reg, err)
	}
}

func TestRegistrationSequenceRejectsNonprogressPages(t *testing.T) {
	t.Parallel()
	for _, page := range [][]Candidate{nil, make([]Candidate, maxHoldSteps+1)} {
		r, _ := registrationSequenceFixture(t, 1, 0)
		sequence := &invalidCandidateSequence{page: page}
		if _, err := r.RunSequence(sequence); err == nil || sequence.calls != 1 || !sequence.closed {
			t.Fatal("invalid sequence retried or left open", sequence, err)
		}
	}
}

type invalidCandidateSequence struct {
	page   []Candidate
	calls  int
	closed bool
}

func (s *invalidCandidateSequence) Next(context.Context, int) ([]Candidate, error) {
	s.calls++
	return s.page, nil
}

func (*invalidCandidateSequence) Advance(int) error { return nil }
func (s *invalidCandidateSequence) Close() error    { s.closed = true; return nil }

var _ CandidateSequence = (*observedCandidateSequence)(nil)
