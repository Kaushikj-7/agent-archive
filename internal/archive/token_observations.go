package archive

import (
	"errors"
	"slices"
	"sort"
	"unicode/utf8"
)

// MaxObservedTokens is the largest exactly representable published observation.
const MaxObservedTokens int64 = 1<<53 - 1

// ModelTokenObservation holds recorded input/output, without billing semantics.
type ModelTokenObservation struct {
	Model  string `json:"model"`
	Input  int64  `json:"input"`
	Output int64  `json:"output"`
}

// TokenObservationSummary keeps observations separate from normalized accounting.
// An available observation never implies complete usage, composition or pricing.
type TokenObservationSummary struct {
	Input       *int64                  `json:"input"`
	Output      *int64                  `json:"output"`
	Models      []ModelTokenObservation `json:"models,omitempty"`
	Observed    Availability            `json:"observed"`
	Coverage    Availability            `json:"coverage"`
	Composition Availability            `json:"composition"`
	Pricing     Availability            `json:"pricing"`
}

// CloneTokenObservations returns an independent snapshot, including numeric pointers.
func CloneTokenObservations(s *TokenObservationSummary) *TokenObservationSummary {
	if s == nil {
		return nil
	}
	c := *s
	if s.Input != nil {
		n := *s.Input
		c.Input = &n
	}
	if s.Output != nil {
		n := *s.Output
		c.Output = &n
	}
	c.Models = append([]ModelTokenObservation(nil), s.Models...)
	return &c
}

// ObservationAccumulator aggregates only exact input/output observations. It owns
// bounded model storage, folds excess labels deterministically, and refuses overflow.
type ObservationAccumulator struct {
	input    int64
	output   int64
	models   map[string]ModelTokenObservation
	present  bool
	overflow bool
}

// Add records one validated observation; no calls, cache or billing are inferred.
func (a *ObservationAccumulator) Add(model string, input, output int64) error {
	if input < 0 || output < 0 || input > MaxObservedTokens-a.input || output > MaxObservedTokens-a.output || a.overflow {
		a.overflow = true
		return errors.New("token observations exceed exact limit")
	}
	a.present = true
	a.input += input
	a.output += output
	if a.models == nil {
		a.models = map[string]ModelTokenObservation{}
	}
	model = boundModelName(model)
	if model == "" {
		model = UnknownModel
	}
	m := a.models[model]
	m.Model = model
	m.Input += input
	m.Output += output
	a.models[model] = m
	if len(a.models) > MaxModelTokens {
		names := make([]string, 0, len(a.models))
		for name := range a.models {
			if name != OtherModels {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		for _, name := range names[MaxModelTokens-1:] {
			m := a.models[name]
			other := a.models[OtherModels]
			other.Model = OtherModels
			other.Input += m.Input
			other.Output += m.Output
			a.models[OtherModels] = other
			delete(a.models, name)
		}
	}
	return nil
}

// AddSummary accepts only explicitly available complete numeric observations.
// Unknown/missing availability never enables numbers. Invalid/overflowing input
// makes the aggregate unavailable rather than publishing a fabricated sum.
func (a *ObservationAccumulator) AddSummary(s *TokenObservationSummary) {
	if s != nil && s.Observed.State == AvailabilityUnavailable && s.Observed.Reason == AvailabilityReasonObservationLimit {
		a.overflow = true
		a.present = true
		return
	}
	if s == nil || !s.Observed.Available() {
		return
	}
	if !s.HasObservations() {
		a.overflow = true
		a.present = true
		return
	}
	for _, m := range s.Models {
		_ = a.Add(m.Model, m.Input, m.Output)
	}
}

// Summary reports recorded observations; all broader accounting stays unavailable.
func (a *ObservationAccumulator) Summary() *TokenObservationSummary {
	if !a.present && !a.overflow {
		return nil
	}
	unavailable := Availability{State: AvailabilityUnavailable, Reason: AvailabilityReasonNotRecorded}
	if a.overflow {
		observed := Availability{State: AvailabilityUnavailable, Reason: AvailabilityReasonObservationLimit}
		return &TokenObservationSummary{Observed: observed, Coverage: unavailable, Composition: unavailable, Pricing: unavailable}
	}
	input, output := a.input, a.output
	s := &TokenObservationSummary{Input: &input, Output: &output, Observed: Availability{State: AvailabilityAvailable}, Coverage: unavailable, Composition: unavailable, Pricing: unavailable}
	for _, m := range a.models {
		s.Models = append(s.Models, m)
	}
	sort.Slice(s.Models, func(i, j int) bool { return s.Models[i].Model < s.Models[j].Model })
	return s
}

// validTokenObservationSummary checks the bounded optional public wire value.
func validTokenObservationSummary(s *TokenObservationSummary) bool {
	if s == nil {
		return true
	}
	for _, a := range []Availability{s.Observed, s.Coverage, s.Composition, s.Pricing} {
		if !slices.Contains([]AvailabilityState{AvailabilityAvailable, AvailabilityUnavailable, AvailabilityUnknown}, a.State) || !slices.Contains([]AvailabilityReason{"", AvailabilityReasonText, AvailabilityReasonNotRecorded, AvailabilityReasonHistoricalFilter, AvailabilityReasonParseFailed, AvailabilityReasonObservationLimit}, a.Reason) {
			return false
		}
	}
	for _, n := range []*int64{s.Input, s.Output} {
		if n != nil && (*n < 0 || *n > MaxObservedTokens) {
			return false
		}
	}
	if len(s.Models) > MaxModelTokens || s.Observed.Available() && !s.HasObservations() {
		return false
	}
	for _, m := range s.Models {
		if m.Model == "" || utf8.RuneCountInString(m.Model) > maxModelNameRunes || m.Input < 0 || m.Output < 0 || m.Input > MaxObservedTokens || m.Output > MaxObservedTokens {
			return false
		}
	}
	return true
}

// HasObservations reports explicitly available, bounded and exact input/output
// observations, including measured zero. It grants no coverage or pricing facts.
func (s *TokenObservationSummary) HasObservations() bool {
	if s == nil || !s.Observed.Available() || s.Input == nil || s.Output == nil || len(s.Models) == 0 || len(s.Models) > MaxModelTokens {
		return false
	}
	var input, output int64
	for _, m := range s.Models {
		if m.Model == "" || utf8.RuneCountInString(m.Model) > maxModelNameRunes || m.Input < 0 || m.Output < 0 || m.Input > MaxObservedTokens-input || m.Output > MaxObservedTokens-output {
			return false
		}
		input += m.Input
		output += m.Output
	}
	return input == *s.Input && output == *s.Output
}
