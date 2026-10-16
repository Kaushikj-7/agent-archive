package fixturecost

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	_ "github.com/wangjohn/agent-archive/internal/testutil/golden" // registers -update for go test ./... -update
)

func TestOperationBudgetRejectsExcessAndInvalidLimits(t *testing.T) {
	for _, limits := range []map[string]int64{{"commits": 1}, {"commits": -1}} {
		if err := exceeded(map[string]int64{"commits": 2}, limits); err == nil {
			t.Fatal("accepted excessive work or invalid budget")
		}
	}
	if err := exceeded(nil, map[string]int64{"commits": 0}); err == nil {
		t.Fatal("accepted missing measurement")
	}
	if err := exceeded(map[string]int64{"commits": 2}, map[string]int64{"commits": 2}); err != nil {
		t.Fatal(err)
	}
}

// Embedding TB retains its private methods while overriding only the methods
// the phase contract uses. Fatal exits are captured without failing this test.
type receiptTB struct {
	testing.TB
	logs  []string
	fatal string
}

func (*receiptTB) Helper() {}

func (tb *receiptTB) Logf(format string, args ...any) {
	tb.logs = append(tb.logs, fmt.Sprintf(format, args...))
}

func (tb *receiptTB) Fatal(args ...any) {
	tb.fatal = fmt.Sprint(args...)
	panic(tb)
}

func (tb *receiptTB) Fatalf(format string, args ...any) {
	tb.fatal = fmt.Sprintf(format, args...)
	panic(tb)
}

func captureFatal(t *testing.T, tb *receiptTB, run func()) {
	t.Helper()
	defer func() {
		if got := recover(); got != tb || tb.fatal == "" {
			t.Fatalf("expected phase fatal, got %v (%q)", got, tb.fatal)
		}
	}()
	run()
}

func TestPhaseReceiptAndOwnedLimits(t *testing.T) {
	tb := &receiptTB{}
	limits := map[string]int64{"store_clones": 1}
	phase := Start(tb, "qualification", "clone", limits)
	// A caller cannot silently relax the phase's budget after Start.
	limits["store_clones"] = 9
	phase.Add("store_clones", 1)
	phase.Add("rows", 7)
	phase.Finish()
	if len(tb.logs) != 1 {
		t.Fatalf("got %d receipts, want 1", len(tb.logs))
	}
	prefix := "CI_FIXTURE_METRIC_V1 "
	if !strings.HasPrefix(tb.logs[0], prefix) {
		t.Fatal("missing receipt prefix")
	}
	var receipt struct {
		Version    int              `json:"version"`
		Fixture    string           `json:"fixture"`
		Phase      string           `json:"phase"`
		ElapsedNS  int64            `json:"elapsed_ns"`
		Operations map[string]int64 `json:"operations"`
	}
	decoder := json.NewDecoder(strings.NewReader(strings.TrimPrefix(tb.logs[0], prefix)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Version != 1 || receipt.Fixture != "qualification" || receipt.Phase != "clone" || receipt.ElapsedNS < 0 || !reflect.DeepEqual(receipt.Operations, map[string]int64{"store_clones": 1, "rows": 7}) {
		t.Fatalf("unexpected receipt: %+v", receipt)
	}
	phase.Add("store_clones", 1)
	captureFatal(t, tb, phase.Finish)
	if !strings.Contains(tb.fatal, "2 exceeds budget 1") {
		t.Fatal(tb.fatal)
	}
}

func TestPhaseRejectsNegativeAndMissingWork(t *testing.T) {
	t.Run("negative", func(t *testing.T) {
		tb := &receiptTB{}
		phase := Start(tb, "fixture", "phase", nil)
		captureFatal(t, tb, func() { phase.Add("work", -1) })
		if len(phase.operations) != 0 || len(tb.logs) != 0 {
			t.Fatal("negative work altered measurements")
		}
	})
	t.Run("missing", func(t *testing.T) {
		tb := &receiptTB{}
		phase := Start(tb, "fixture", "phase", map[string]int64{"work": 0})
		captureFatal(t, tb, phase.Finish)
		if !strings.Contains(tb.fatal, "missing budget measurement") || len(tb.logs) != 1 {
			t.Fatalf("missing work contract: %q, receipts=%d", tb.fatal, len(tb.logs))
		}
	})
}
