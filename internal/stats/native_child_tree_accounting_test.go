package stats

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// These are consumer metadata inputs, not a claim that Muse child admission or
// any native child role/coverage grammar is qualified.
func TestMuseNativeChildMetadataTreeRollupCountsEachOwnActivityOnce(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	root := meta("root", "muse", now, turns(5), observed(100, 10), toolResults(2, 1))
	child := meta("child", "muse", now.AddDate(0, 0, -40), turns(70), observed(20, 2), toolResults(3, 0), parentOf(root.SessionID))
	grandchild := meta("grandchild", "muse", now.AddDate(0, 0, 2), turns(80), observed(30, 3), toolResults(5, 1), parentOf(child.SessionID))
	sibling := meta("sibling", "muse", now, turns(90), observed(40, 4), toolResults(7, 2), parentOf(root.SessionID))
	for _, m := range []*archive.Metadata{&child, &grandchild, &sibling} {
		m.NativeChild = true
	}
	oldChild := child
	oldChild.MetadataDerivedAt = child.MetadataDerivedAt.Add(-time.Hour)
	observed(9999, 9999)(&oldChild)
	toolResults(9999, 9999)(&oldChild)
	inputs := []archive.Metadata{grandchild, oldChild, root, sibling, child}
	options := Options{Now: now, Days: 1, Location: time.UTC}
	var expected Stats
	for i := range len(inputs) {
		permuted := slices.Concat(inputs[i:], inputs[:i])
		prepared := Prepare(permuted, PrepareOptions{Location: time.UTC})
		for _, got := range []Stats{Compute(permuted, options), prepared.Compute(options)} {
			if got.Coverage.Sessions != 1 || got.Coverage.SubagentSessions != 3 || got.Coverage.OrphanSubagents != 0 || got.ObservationSessions != 4 {
				t.Fatalf("owner/deduplication coverage: %+v", got.Coverage)
			}
			if got.Overview.Prompts.Value == nil || *got.Overview.Prompts.Value != 5 || got.Overview.Sessions.Value == nil || *got.Overview.Sessions.Value != 1 {
				t.Fatalf("child prompts or sessions became root activity: %+v", got.Overview)
			}
			// 100+20+30+40 input, 10+2+3+4 output; children have own
			// observations rather than parent totals copied into each member.
			obs := got.TokenObservations
			if obs == nil || obs.Input == nil || *obs.Input != 190 || obs.Output == nil || *obs.Output != 19 || !obs.Observed.Available() {
				t.Fatalf("own observations did not roll up once: %+v", obs)
			}
			if got.Overview.Tokens.Value != nil || got.Overview.Cost.Value != nil || got.Coverage.ObservationOnlySessions != 4 {
				t.Fatal("Muse observations became normalized or billable tokens")
			}
			if tools := got.Highlights.ToolErrors; tools == nil || tools.Results != 17 || tools.Errors != 4 {
				t.Fatalf("own tool activity did not roll up once: %+v", tools)
			}
			if i == 0 {
				expected = got
			} else if !reflect.DeepEqual(got, expected) {
				t.Fatal("metadata ordering changed the tree or root date window")
			}
		}
	}
}

func TestMuseNativeChildOrphanMetadataKeepsUnknownOwnActivity(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	for _, parent := range []string{"", "missing-parent"} {
		orphan := meta("orphan", "muse", now)
		orphan.NativeChild, orphan.ParentSessionID = true, parent
		for _, days := range []int{1, 7} {
			got := Compute([]archive.Metadata{orphan}, Options{Now: now, Days: days, Location: time.UTC})
			if got.Coverage.Sessions != 1 || got.Coverage.OrphanSubagents != 1 || got.Coverage.SubagentSessions != 0 {
				t.Fatalf("unresolved child acquired a root role: %+v", got.Coverage)
			}
			if got.TokenObservations != nil || got.ObservationSessions != 0 || got.Overview.Prompts.Value != nil || got.Overview.Tokens.Value != nil || got.Highlights.ToolErrors != nil {
				t.Fatal("missing own activity became a reported zero or inherited activity")
			}
		}
		outside := Compute([]archive.Metadata{orphan}, Options{Now: now.AddDate(0, 0, 2), Days: 1, Location: time.UTC})
		if outside.Coverage.Sessions != 0 || outside.Coverage.OrphanSubagents != 0 || outside.TokenObservations != nil {
			t.Fatal("orphan borrowed a missing parent's date window")
		}
	}
}
