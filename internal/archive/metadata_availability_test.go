package archive

import "testing"

func TestMetadataDoesNotMeasureUnavailableToolResults(t *testing.T) {
	t.Parallel()
	a := Analysis{Observability: Observability{
		StructuredCounts: Availability{State: AvailabilityAvailable},
		ToolResults:      Availability{State: AvailabilityUnavailable, Reason: AvailabilityReasonNotRecorded},
	}}
	m := assembleAnalyzedMetadata(SourceBundle{}, a, Metadata{})
	if m.Counts.Turns == nil || *m.Counts.Turns != 0 {
		t.Fatalf("known prompt count = %v", m.Counts.Turns)
	}
	if m.Counts.ToolResults != nil {
		t.Fatalf("unavailable results became measured count %d", *m.Counts.ToolResults)
	}
}

func TestMetadataSeparatesToolEvidenceFromConversation(t *testing.T) {
	t.Parallel()
	available := Availability{State: AvailabilityAvailable}
	for _, state := range []AvailabilityState{AvailabilityUnknown, AvailabilityUnavailable} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			missing := Availability{State: state}
			a := Analysis{View: NormalizedView{ToolCalls: []NormalizedToolCall{{Name: "Edit"}}, ToolResults: []NormalizedToolResult{{IsError: true}}}, Observability: Observability{
				StructuredCounts: available, ToolCalls: missing, FilesTouched: missing, GitActivity: missing,
				ToolResults: missing, ToolErrors: available,
			}}
			m := assembleAnalyzedMetadata(SourceBundle{}, a, Metadata{})
			for name, count := range map[string]*int{"calls": m.Counts.ToolCalls, "results": m.Counts.ToolResults, "files": m.Counts.FilesTouched, "commits": m.Counts.Commits, "pushes": m.Counts.Pushes, "created PRs": m.Counts.PRsCreated, "merged PRs": m.Counts.PRsMerged} {
				if count != nil {
					t.Errorf("%s measured unavailable evidence: %d", name, *count)
				}
			}
			if m.Counts.ToolErrors == nil || *m.Counts.ToolErrors != 1 {
				t.Fatalf("independent errors: %v", m.Counts.ToolErrors)
			}
			if m.Counts.Turns == nil || m.Counts.Messages == nil {
				t.Fatal("lost conversation counts")
			}
			a.Observability.ToolCalls = available
			m = assembleAnalyzedMetadata(SourceBundle{}, a, Metadata{})
			if m.Counts.ToolCalls == nil || *m.Counts.ToolCalls != 1 || m.Counts.ToolResults != nil || m.Counts.FilesTouched != nil || m.Counts.Commits != nil {
				t.Fatalf("invocation-only evidence: %+v", m.Counts)
			}
		})
	}
}

func TestMetadataMeasuredZeroAndLegacyObservability(t *testing.T) {
	t.Parallel()
	available := Availability{State: AvailabilityAvailable}
	for _, explicit := range []bool{false, true} {
		o := Observability{StructuredCounts: available, ToolResults: available}
		if explicit {
			o.ToolCalls, o.FilesTouched, o.GitActivity = available, available, available
		}
		m := assembleAnalyzedMetadata(SourceBundle{}, Analysis{Observability: o}, Metadata{})
		for name, count := range map[string]*int{"calls": m.Counts.ToolCalls, "results": m.Counts.ToolResults, "files": m.Counts.FilesTouched, "commits": m.Counts.Commits, "pushes": m.Counts.Pushes, "created PRs": m.Counts.PRsCreated, "merged PRs": m.Counts.PRsMerged} {
			if count == nil || *count != 0 {
				t.Errorf("explicit=%v: %s zero = %v", explicit, name, count)
			}
		}
	}
	a := Analysis{Observability: Observability{StructuredCounts: available}}
	if count := analyzedCounts(a, 0, 0, 0).ToolResults; count != nil {
		t.Fatalf("unset result availability measured %d", *count)
	}
}

func TestMetadataDoesNotInheritReasonOnlyObservability(t *testing.T) {
	t.Parallel()
	a := Analysis{Observability: Observability{
		StructuredCounts: Availability{State: AvailabilityAvailable},
		ToolCalls:        Availability{Reason: AvailabilityReasonNotRecorded},
		FilesTouched:     Availability{Reason: AvailabilityReasonNotRecorded},
		GitActivity:      Availability{Reason: AvailabilityReasonNotRecorded},
	}}
	m := assembleAnalyzedMetadata(SourceBundle{}, a, Metadata{})
	if m.Counts.ToolCalls != nil || m.Counts.FilesTouched != nil || m.Counts.Commits != nil || m.Counts.Pushes != nil || m.Counts.PRsCreated != nil || m.Counts.PRsMerged != nil {
		t.Fatalf("nonzero availability declaration inherited known evidence: %+v", m.Counts)
	}
}
