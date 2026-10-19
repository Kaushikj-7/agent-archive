package muse

import (
	"bytes"
	"reflect"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

// The observed native profile contains exactly one discriminator and no subject.
// It validates an omitted body, without supplying a committed file-read join.
func TestBroadReadProfilePreservesOmissionOnly(t *testing.T) {
	t.Parallel()
	baseline, err := filterBytes(t, effectBodies(t, map[string]any{"kind": "ineligible"}, completeOutcome()), agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := filterBytes(t, effectBodies(t, map[string]any{"kind": "broad_read"}, completeOutcome()), agentapi.ReadLimits{})
	if err != nil {
		t.Fatal("observed native broad-read profile refused", err)
	}
	if !reflect.DeepEqual(baseline, out) {
		t.Fatal("broad-read profile changed retained projection")
	}
	wire := bytes.Join(out.Records, []byte("\n"))
	for _, private := range []string{"broad_read", "parallel_profile", "effect_join", "PRIVATE_EFFECT_"} {
		if bytes.Contains(wire, []byte(private)) {
			t.Fatal("private profile or read join retained")
		}
	}
	analysis, err := (Parser{}).Parse(t.Context(), bundleFor(t, out))
	if err != nil {
		t.Fatal(err)
	}
	baseAnalysis, err := (Parser{}).Parse(t.Context(), bundleFor(t, baseline))
	if err != nil || !reflect.DeepEqual(baseAnalysis, analysis) {
		t.Fatal("profile changed derived facts", err)
	}
	again, err := (Filter{}).Refilter(t.Context(), bundleFor(t, out), time.Time{})
	if err != nil || !reflect.DeepEqual(out, again) {
		t.Fatal("profile changed refilter", err)
	}
}

func TestBroadReadProfileRejectsUnqualifiedShapes(t *testing.T) {
	t.Parallel()
	for name, profile := range map[string]any{
		"subject":      map[string]any{"kind": "broad_read", "subject": "PRIVATE_EFFECT_SUBJECT_SENTINEL"},
		"extra":        map[string]any{"kind": "broad_read", "future": true},
		"missing-kind": map[string]any{},
		"null":         nil,
		"null-kind":    map[string]any{"kind": nil},
		"number-kind":  map[string]any{"kind": 1},
		"array-kind":   map[string]any{"kind": []any{"broad_read"}},
		"future-kind":  map[string]any{"kind": "future_broad_read"},
		"string":       "broad_read",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out, err := filterBytes(t, effectBodies(t, profile, completeOutcome()), agentapi.ReadLimits{})
			if err == nil || len(out.Records) != 0 {
				t.Fatal("unqualified broad-read shape admitted")
			}
		})
	}
}

func TestBroadReadProfileCannotCreateCommittedFileReadJoin(t *testing.T) {
	t.Parallel()
	raw := changeRead(t, func(rows []map[string]any) []map[string]any {
		effect := readRow(rows, "tool_batch.effect.started", "")
		object(object(effect["payload"])["record"])["parallel_profile"] = map[string]any{"kind": "broad_read"}
		return rows
	})
	out, err := filterBytes(t, raw, agentapi.ReadLimits{})
	if err == nil || len(out.Records) != 0 {
		t.Fatal("broad-read profile admitted approval-free committed file-read lane")
	}
}
