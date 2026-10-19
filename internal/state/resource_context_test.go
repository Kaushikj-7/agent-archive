package state

import (
	"context"
	"errors"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

func TestDurableContextPreservesOriginalIdentityAndReservation(t *testing.T) {
	t.Parallel()
	owner, cancelOwner := context.WithCancel(t.Context())
	t.Cleanup(cancelOwner)
	budget := agentapi.NewNativeReadBudget(256)
	scoped, closeScope := OpenReadOnly(t.TempDir()).WithReadBudget(owner, budget)
	t.Cleanup(closeScope)
	same, ctx, closeSame := scoped.withDurableContext(owner)
	t.Cleanup(closeSame)
	if same != scoped || ctx != owner {
		t.Fatal("same captured context replaced original read owner")
	}
	caller, cancelCaller := context.WithCancel(t.Context())
	t.Cleanup(cancelCaller)
	joined, work, closeWork := scoped.withDurableContext(caller)
	t.Cleanup(closeWork)
	reservation := joined.readReservation()
	if !reservation.Reserve(128) {
		t.Fatal("joined read lost original reservation")
	}
	defer reservation.Release(128)
	closeWork()
	if used, _ := budget.Charged(); used != 128 {
		t.Fatal("work cleanup released original loan", used)
	}
	if owner.Err() != nil || caller.Err() != nil {
		t.Fatal("work cleanup cancelled parent owner")
	}
	if !errors.Is(work.Err(), context.Canceled) {
		t.Fatal("temporary work context remained alive", work.Err())
	}
	if scoped.resourceContext != owner {
		t.Fatal("joining mutated original read scope")
	}
}

func TestDurableContextHonorsEitherCancellationOwner(t *testing.T) {
	t.Parallel()
	for _, original := range []bool{false, true} {
		t.Run(map[bool]string{false: "caller", true: "captured-owner"}[original], func(t *testing.T) {
			t.Parallel()
			owner, cancelOwner := context.WithCancel(t.Context())
			t.Cleanup(cancelOwner)
			caller, cancelCaller := context.WithCancel(t.Context())
			t.Cleanup(cancelCaller)
			scoped, closeScope := OpenReadOnly(t.TempDir()).WithReadBudget(owner, nil)
			t.Cleanup(closeScope)
			_, work, closeWork := scoped.withDurableContext(caller)
			t.Cleanup(closeWork)
			if original {
				cancelOwner()
			} else {
				cancelCaller()
			}
			if !errors.Is(work.Err(), context.Canceled) {
				t.Fatal("joined context revived cancelled owner", work.Err())
			}
		})
	}
}

type nonComparableDurableContext struct {
	context.Context
	marker []byte
}

func TestDurableContextRejoinsNonComparableOriginal(t *testing.T) {
	t.Parallel()
	owner, cancelOwner := context.WithCancel(t.Context())
	t.Cleanup(cancelOwner)
	original := nonComparableDurableContext{Context: owner, marker: []byte("owner")}
	scoped, closeScope := OpenReadOnly(t.TempDir()).WithReadBudget(original, nil)
	t.Cleanup(closeScope)
	joined, work, closeWork := scoped.withDurableContext(t.Context())
	t.Cleanup(closeWork)
	rejoined, current, closeCurrent := joined.withDurableContext(work)
	t.Cleanup(closeCurrent)
	if rejoined.resourceReleases != joined.resourceReleases {
		t.Fatal("rejoining changed the captured release owner")
	}
	cancelOwner()
	if !errors.Is(current.Err(), context.Canceled) {
		t.Fatal("rejoining revived the original owner", current.Err())
	}
}

type durableValidationTestGate string

const (
	durableValidationRoots     durableValidationTestGate = "roots"
	durableValidationSelecting durableValidationTestGate = "selecting"
)

func TestDurableValidationKeepsCallerAndCapturedCancellation(t *testing.T) {
	t.Parallel()
	for _, gate := range []durableValidationTestGate{durableValidationRoots, durableValidationSelecting} {
		for _, captured := range []bool{false, true} {
			t.Run(string(gate)+"/"+map[bool]string{false: "caller", true: "captured-owner"}[captured], func(t *testing.T) {
				t.Parallel()
				store := newTestStore(t)
				owner, cancelOwner := context.WithCancel(t.Context())
				t.Cleanup(cancelOwner)
				caller, cancelCaller := context.WithCancel(t.Context())
				t.Cleanup(cancelCaller)
				budget := agentapi.NewNativeReadBudget(1 << 20)
				scoped, closeScope := store.WithReadBudget(owner, budget)
				t.Cleanup(closeScope)
				reservation := scoped.readReservation()
				if !reservation.Reserve(128) {
					t.Fatal("original reservation refused")
				}
				defer reservation.Release(128)
				if captured {
					cancelOwner()
				} else {
					cancelCaller()
				}
				var err error
				switch gate {
				case durableValidationRoots:
					_, err = scoped.inspectDurableReadRoots(caller)
				case durableValidationSelecting:
					err = scoped.validateSelectingPublishedBudgeted(caller, publishedState{})
				}
				if !errors.Is(err, context.Canceled) {
					t.Fatal("validation discarded cancellation", err)
				}
				if used, _ := budget.Charged(); used != 128 {
					t.Fatal("validation cleanup changed the original loan", used)
				}
				if scoped.resourceContext != owner {
					t.Fatal("validation replaced the captured owner")
				}
			})
		}
	}
}
