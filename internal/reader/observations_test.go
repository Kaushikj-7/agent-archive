package reader

import (
	"github.com/wangjohn/agent-archive/internal/archive"
	"testing"
)

func TestModelFilterMatchesOnlyAvailableObservations(t *testing.T) {
	const futureAvailability archive.AvailabilityState = "future"
	t.Parallel()
	for _, zero := range []bool{false, true} {
		var a archive.ObservationAccumulator
		input := int64(101)
		if zero {
			input = 0
		}
		if err := a.Add("claude-sonnet-4", input, 0); err != nil {
			t.Fatal(err)
		}
		m := archive.Metadata{TokenObservations: a.Summary()}
		if !matches(m, Filter{Model: "claude-sonnet-4"}) || matches(m, Filter{Model: "absent"}) || !matches(m, Filter{}) {
			t.Fatal("available observed model filter")
		}
		for _, state := range []archive.AvailabilityState{"", archive.AvailabilityUnknown, archive.AvailabilityUnavailable, futureAvailability} {
			m.TokenObservations.Observed.State = state
			if matches(m, Filter{Model: "claude-sonnet-4"}) {
				t.Fatal("unavailable observation matched")
			}
		}
		m.TokenObservations.Observed = archive.Availability{State: archive.AvailabilityUnavailable, Reason: archive.AvailabilityReasonObservationLimit}
		if matches(m, Filter{Model: "claude-sonnet-4"}) {
			t.Fatal("overflow matched")
		}
		m.TokenObservations = a.Summary()
		m.TokenObservations.Input = nil
		if matches(m, Filter{Model: "claude-sonnet-4"}) {
			t.Fatal("missing observation matched")
		}
		m.TokenObservations = a.Summary()
		*m.TokenObservations.Input = archive.MaxObservedTokens + 1
		if matches(m, Filter{Model: "claude-sonnet-4"}) {
			t.Fatal("invalid overflow matched")
		}
		m.TokenObservations = nil
		if matches(m, Filter{Model: "claude-sonnet-4"}) || !matches(m, Filter{}) {
			t.Fatal("legacy empty metadata changed")
		}
		m.Models = []archive.ModelSummary{{Attributes: map[string]string{"gen_ai.request.model": "legacy"}}}
		if !matches(m, Filter{Model: "legacy"}) {
			t.Fatal("legacy model filter changed")
		}
	}
	var folded archive.ObservationAccumulator
	if folded.Add(archive.OtherModels, 1, 2) != nil {
		t.Fatal("synthetic")
	}
	if matches(archive.Metadata{TokenObservations: folded.Summary()}, Filter{Model: archive.OtherModels}) {
		t.Fatal("fold sentinel matched as a recorded model")
	}
}
