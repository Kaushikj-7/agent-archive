package collector

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/muse"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/discovery"
	"github.com/wangjohn/agent-archive/internal/testutil/musefixture"
)

func startupOrderingFactory(context.Context, NativeLocatorPass) (agentapi.NativeLocatorLookup, func() error, error) {
	panic("scheduling must not invoke a factory")
}

func TestStartupCandidateOrderUsesRealInventoryCursor(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		order []int
	}{
		{"reverse", []int{7, 6, 5, 4, 3, 2, 1, 0}},
		{"random-permutation", []int{7, 3, 0, 6, 1, 5, 2, 4}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rootPath, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			root := archive.NativeRoot{Agent: "muse", Configured: rootPath, Canonical: rootPath, Revision: "one"}
			raw, err := os.ReadFile(filepath.Join("..", "agents", "muse", "testdata", "synthetic-exec-empty.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			queries := []agentapi.LocatorRequest{}
			for i := range tc.order {
				id := nativePendingRegistration(i).NativeSessionID
				path := filepath.Join(rootPath, "2026", "10", "15", id, "session.jsonl")
				if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(path, musefixture.Rewrite(t, raw, id, "/work/synthetic-muse"), 0600); err != nil {
					t.Fatal(err)
				}
				queries = append(queries, agentapi.LocatorRequest{Purpose: agentapi.LocatorStartupCandidate, NativeID: id, Root: root})
			}
			for _, scheduled := range []bool{false, true} {
				t.Run(map[bool]string{false: "original-order-control", true: "production-hint"}[scheduled], func(t *testing.T) {
					budget := agentapi.NewNativeReadBudget(agentapi.PassReadBudget)
					lookup, openErr := discovery.NewNativeInventory(t.Context(), t.TempDir(), []archive.NativeRoot{root}, queries, budget, muse.ValidateNativeLocator)
					if openErr != nil {
						t.Fatal(openErr)
					}
					defer func() {
						if closeErr := lookup.Close(); closeErr != nil {
							t.Error(closeErr)
						}
						if used, _ := budget.Charged(); used != 0 {
							t.Errorf("ledger retained %d", used)
						}
					}()
					p := pass{ctx: t.Context(), opts: Options{NativeRoots: []archive.NativeRoot{root}, NativeLocators: lookup, NativeLocatorFactory: startupOrderingFactory, sourcePasses: &sourcePassSet{env: agentapi.SourceEnvironment{ReadBudget: budget}}}}
					for _, i := range tc.order {
						p.startupCandidates = append(p.startupCandidates, startupCandidate{root: root, intent: agentapi.AdmissionIntent{NativeSessionID: nativePendingRegistration(i).NativeSessionID}})
					}
					beforeCounts := lookup.Counts()
					beforeUsed, _ := budget.Charged()
					if scheduled {
						p.orderStartupCandidates()
					}
					if lookup.Counts() != beforeCounts {
						t.Fatal("ordering performed source/currentness work")
					}
					if afterUsed, _ := budget.Charged(); afterUsed != beforeUsed {
						t.Fatal("ordering retained scratch", beforeUsed, afterUsed)
					}
					selected := map[string]bool{}
					for _, candidate := range p.startupCandidates {
						id := candidate.intent.NativeSessionID
						result, locateErr := lookup.Locate(t.Context(), agentapi.LocatorRequest{Purpose: agentapi.LocatorStartupCandidate, NativeID: id, Root: root})
						if locateErr != nil {
							t.Fatal(locateErr)
						}
						if result.Status == agentapi.LocatorFound {
							if checkErr := lookup.Check(t.Context(), id, result.Revision); checkErr != nil {
								t.Fatal(checkErr)
							}
							selected[id] = true
						}
					}
					if scheduled && len(selected) != len(tc.order) {
						t.Fatalf("ordered actual locator union=%d want=%d", len(selected), len(tc.order))
					}
					if !scheduled && (len(selected) == 0 || len(selected) == len(tc.order)) {
						t.Fatalf("original-order failure control not exercised: %d", len(selected))
					}
				})
			}
		})
	}
}

type startupOrderingHint struct {
	agentapi.NativeLocatorLookup
	order  []string
	cancel context.CancelFunc
	calls  int
}

func (h *startupOrderingHint) PendingOrder() []string {
	h.calls++
	if h.cancel != nil {
		h.cancel()
	}
	return h.order
}

type startupHintCase string

const (
	startupHintAbsent       startupHintCase = "absent"
	startupHintUnknown      startupHintCase = "unknown"
	startupHintDuplicate    startupHintCase = "duplicate"
	startupHintOversized    startupHintCase = "oversized"
	startupHintCanceled     startupHintCase = "canceled"
	startupHintCancelDuring startupHintCase = "cancel-during-hint"
	startupHintPressure     startupHintCase = "pressure"
	startupHintBorrowed     startupHintCase = "borrowed-lookup"
)

func TestStartupCandidateOrderInvalidHintsPreserveWorkAndReleaseScratch(t *testing.T) {
	t.Parallel()
	a, b := nativePendingRegistration(0).NativeSessionID, nativePendingRegistration(1).NativeSessionID
	for _, name := range []startupHintCase{startupHintAbsent, startupHintUnknown, startupHintDuplicate, startupHintOversized, startupHintCanceled, startupHintCancelDuring, startupHintPressure, startupHintBorrowed} {
		t.Run(string(name), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			hint := &startupOrderingHint{order: []string{a, b}}
			limit := agentapi.PassReadBudget
			switch name {
			case startupHintAbsent:
				hint.order = nil
			case startupHintUnknown:
				hint.order = []string{a, "outside-immutable-query-domain"}
			case startupHintDuplicate:
				hint.order = []string{a, a}
			case startupHintOversized:
				hint.order = make([]string, 8193)
			case startupHintCanceled:
				cancel()
			case startupHintCancelDuring:
				hint.cancel = cancel
			case startupHintPressure:
				limit = 1
			}
			budget := agentapi.NewNativeReadBudget(limit)
			factory := startupOrderingFactory
			if name == startupHintBorrowed {
				factory = nil
			}
			p := pass{ctx: ctx, opts: Options{NativeLocators: hint, NativeLocatorFactory: factory, sourcePasses: &sourcePassSet{env: agentapi.SourceEnvironment{ReadBudget: budget}}}, startupCandidates: []startupCandidate{{intent: agentapi.AdmissionIntent{NativeSessionID: b}}, {intent: agentapi.AdmissionIntent{NativeSessionID: a}}}}
			before := append([]startupCandidate(nil), p.startupCandidates...)
			p.orderStartupCandidates()
			if !reflect.DeepEqual(before, p.startupCandidates) {
				t.Fatal("invalid/canceled/refused hint changed candidate work")
			}
			if used, peak := budget.Charged(); used != 0 || peak > limit {
				t.Fatal("scratch charge not bounded/released", used, peak)
			}
			if (name == startupHintCanceled || name == startupHintPressure || name == startupHintBorrowed) && hint.calls != 0 {
				t.Fatal("fallback invoked hint before eligibility/charge")
			}
		})
	}
}
