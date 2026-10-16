package stats

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

func observed(input, output int64) option {
	return func(m *archive.Metadata) {
		var a archive.ObservationAccumulator
		if a.Add("claude-sonnet-4", input, output) != nil {
			panic("synthetic observation")
		}
		m.TokenObservations = a.Summary()
	}
}

func TestObservationsNeverBecomePricedNormalizedUsage(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	table := DefaultPriceTable()
	table = table.WithOverrides(PriceTable{Version: "synthetic-override", Currency: "USD", Models: []ModelPrice{{ID: "claude-sonnet-4", InputPerMTok: 123, OutputPerMTok: 456}}})
	only := meta("observed", "muse", now, observed(202, 46))
	opts := Options{Now: now, Days: 1, Location: time.UTC, PriceTable: table, By: GroupProject}
	for _, zero := range []bool{false, true} {
		m := only
		if zero {
			observed(0, 0)(&m)
		}
		s := Compute([]archive.Metadata{m}, opts)
		if s.Overview.Cost.Value != nil || s.Overview.Tokens.Value != nil || s.Composition != nil || s.Overview.CacheShare != nil || len(s.Models) != 0 || s.TokenObservations == nil || !s.TokenObservations.Observed.Available() || s.Coverage.ObservationOnlySessions != 1 || s.Overview.Cost.UnpricedTokens != 0 || !s.Overview.Cost.Partial {
			t.Fatalf("priced observation: %+v", s)
		}
		for _, g := range s.Groups.Rows {
			if !g.Cost.Partial || g.Cost.ObservationOnlySessions != 1 || g.Cost.UnpricedTokens != 0 {
				t.Fatalf("group cost %+v", g.Cost)
			}
		}
	}
	// Same model in both lanes keeps provenance before model merge and pricing.
	legacy := meta("priced", "claude", now, func(m *archive.Metadata) {
		m.ModelTokens = []archive.ModelTokens{{Model: "claude-sonnet-4", InputTokens: ip(1000), OutputTokens: ip(100)}}
	})
	priced := Compute([]archive.Metadata{legacy}, opts)
	mixed := Compute([]archive.Metadata{legacy, only}, opts)
	if priced.Overview.Cost.Value == nil || mixed.Overview.Cost.Value == nil || *priced.Overview.Cost.Value != *mixed.Overview.Cost.Value || !mixed.Overview.Cost.Partial || mixed.Overview.Cost.ObservationOnlySessions != 1 || mixed.Overview.Cost.UnpricedTokens != 0 || *mixed.TokenObservations.Input != 202 {
		t.Fatal("mixed priced subset")
	}
	reversed := Compute([]archive.Metadata{only, legacy}, opts)
	if !reflect.DeepEqual(mixed, reversed) {
		t.Fatal("order dependent")
	}
	// Root and child belong to the existing unit exactly once; Prepare owns data.
	child := only
	child.SessionID = "child"
	child.ParentSessionID = legacy.SessionID
	prepared := Prepare([]archive.Metadata{legacy, child}, PrepareOptions{Location: time.UTC, PriceTable: table})
	before := prepared.Compute(opts)
	raw, _ := json.Marshal(before)
	*child.TokenObservations.Input = 900
	child.TokenObservations.Models[0].Model = "mutated"
	after := prepared.Compute(opts)
	again, _ := json.Marshal(after)
	if string(raw) != string(again) || before.ObservationSessions != 1 || *before.TokenObservations.Input != 202 || before.Coverage.Sessions != 1 {
		t.Fatal("Prepared ownership or root/child duplicate")
	}
}

func TestObservationAggregateOverflowRetainsExcludedCoverage(t *testing.T) {
	const futureAvailability archive.AvailabilityState = "future"
	t.Parallel()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	a := meta("a", "muse", now, observed(archive.MaxObservedTokens, 0))
	b := meta("b", "muse", now, observed(1, 0))
	s := Compute([]archive.Metadata{a, b}, Options{Now: now, Days: 1, Location: time.UTC})
	if s.TokenObservations == nil || s.TokenObservations.Observed.Available() || s.TokenObservations.Input != nil || s.TokenObservations.Observed.Reason != archive.AvailabilityReasonObservationLimit || s.Coverage.ObservationOnlySessions != 2 || s.Overview.Cost.Value != nil || s.Overview.Cost.UnpricedTokens != 0 {
		t.Fatal("fabricated overflow aggregate")
	}
	for _, state := range []archive.AvailabilityState{"", archive.AvailabilityUnknown, archive.AvailabilityUnavailable, futureAvailability} {
		a.TokenObservations.Observed.State = state
		s = Compute([]archive.Metadata{a}, Options{Now: now, Days: 1})
		if s.TokenObservations != nil || s.ObservationSessions != 0 || s.Overview.Cost.Value != nil {
			t.Fatal("unknown availability enabled observation")
		}
	}
}
