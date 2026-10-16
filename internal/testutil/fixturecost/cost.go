// Package fixturecost records content-free setup costs from test fixtures.
package fixturecost

import (
	"encoding/json"
	"fmt"
	"maps"
	"testing"
	"time"
)

// Phase measures a single, non-overlapping setup phase. It is owned by its
// calling goroutine. Finish must run before starting a nested fixture phase.
type Phase struct {
	tb         testing.TB
	fixture    string
	phase      string
	started    time.Time
	operations map[string]int64
	limits     map[string]int64
}

// Start begins a phase with optional deterministic operation ceilings. A nil
// ceiling records costs without pretending that elapsed time is deterministic.
func Start(tb testing.TB, fixture, phase string, limits map[string]int64) *Phase {
	tb.Helper()
	return &Phase{tb: tb, fixture: fixture, phase: phase, started: time.Now(), operations: map[string]int64{}, limits: maps.Clone(limits)}
}

// Add records work at the operation's call site.
func (p *Phase) Add(operation string, count int64) {
	p.tb.Helper()
	if count < 0 {
		p.tb.Fatal("negative fixture operation count")
		return
	}
	p.operations[operation] += count
}

func exceeded(operations, limits map[string]int64) error {
	for operation, limit := range limits {
		count, observed := operations[operation]
		if !observed {
			return fmt.Errorf("%s: missing budget measurement", operation)
		}
		if limit < 0 || count > limit {
			return fmt.Errorf("%s: %d exceeds budget %d", operation, operations[operation], limit)
		}
	}
	return nil
}

// Finish emits one bounded receipt through Go's test output, then enforces work
// ceilings. Receipts measure wall time, not CPU; callers and reporters must not
// sum concurrent tests. Test identity comes from the enclosing Go JSON event.
func (p *Phase) Finish() {
	p.tb.Helper()
	receipt := struct {
		Version    int              `json:"version"`
		Fixture    string           `json:"fixture"`
		Phase      string           `json:"phase"`
		ElapsedNS  int64            `json:"elapsed_ns"`
		Operations map[string]int64 `json:"operations"`
	}{1, p.fixture, p.phase, time.Since(p.started).Nanoseconds(), p.operations}
	raw, err := json.Marshal(receipt)
	if err != nil {
		p.tb.Fatal(err)
		return
	}
	p.tb.Logf("CI_FIXTURE_METRIC_V1 %s", raw)
	if err := exceeded(p.operations, p.limits); err != nil {
		p.tb.Fatalf("fixture %s/%s work budget: %v", p.fixture, p.phase, err)
	}
}
