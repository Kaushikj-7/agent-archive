package cli

import (
	"encoding/json"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/stats"
)

func TestStatsCommandReusesPreparedWindowAccounting(t *testing.T) {
	sessions := []archive.Metadata{syntheticSession{
		id: "prepared", harness: "claude", captured: statsNow,
		perModel: []modelTokenSpec{{model: "claude-opus-5-5", input: 100, output: 50}},
	}.build()}
	opts := stats.Options{Now: statsNow, Location: statsNow.Location(), Days: 7, AllRows: true}
	first, prepared := computeStats(sessions, opts, true)
	if prepared == nil {
		t.Fatal("interactive command did not retain prepared accounting")
	}
	want, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	// Metadata is needed only during the initial computation. Later window
	// changes must continue to use the snapshot even if the input is discarded.
	sessions[0] = archive.Metadata{}
	inputs := statsInputs{prepared: prepared, hasSessions: true, now: statsNow}
	for _, days := range []int{30, 90, 7} {
		inputs.compute(days, true)
	}
	got, err := json.Marshal(inputs.compute(7, true))
	if err != nil || string(got) != string(want) {
		t.Fatalf("window recalculation changed prepared accounting: %v", err)
	}
	_, static := computeStats(nil, opts, false)
	if static != nil {
		t.Fatal("static command retained interactive accounting")
	}
}
