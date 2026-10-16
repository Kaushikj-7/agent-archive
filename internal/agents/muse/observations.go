package muse

import (
	"github.com/wangjohn/agent-archive/internal/archive"
	"unicode/utf8"
)

func completionEnriched(f map[string]any) bool {
	for _, key := range []string{"run_id", "source_run_record_id", "source_run_record_sequence", "model", "input_tokens", "output_tokens"} {
		if _, ok := f[key]; ok {
			return true
		}
	}
	return false
}

func completionFields(r retained) bool {
	f := r.Fields
	return r.Causation == nil && keys(f, "kind", "event_kind", "run_id", "source_run_record_id", "source_run_record_sequence", "model", "input_tokens", "output_tokens") && str(f["kind"]) == "run" && str(f["event_kind"]) == "model_completed" && identifier(f["run_id"]) && identifier(f["source_run_record_id"]) && integer(f["source_run_record_sequence"], 1) && observedModel(f["model"]) && integer(f["input_tokens"], 0) && integer(f["output_tokens"], 0)
}

func observedModel(v any) bool {
	s, ok := v.(string)
	return ok && s != "" && utf8.RuneCountInString(s) <= 128
}

func projectCompletion(r retained, p, event map[string]any) (map[string]any, error) {
	usage := object(event["usage"])
	if r.Causation != nil || !keys(p, "kind", "run_id", "source_run_record_id", "source_run_record_sequence", "event") || !keys(event, "duration_ms", "kind", "model", "usage") || !integer(event["duration_ms"], 0) || !observedModel(event["model"]) || !identifier(p["run_id"]) || !identifier(p["source_run_record_id"]) || !integer(p["source_run_record_sequence"], 1) || !keys(usage, "input_tokens", "output_tokens", "cached_tokens", "cache_read_tokens", "cache_write_tokens", "reasoning_tokens") {
		return nil, errFormat
	}
	for _, v := range usage {
		if !integer(v, 0) {
			return nil, errFormat
		}
	}
	f := map[string]any{"kind": "run", "event_kind": "model_completed", "model": event["model"], "input_tokens": usage["input_tokens"], "output_tokens": usage["output_tokens"]}
	copyFields(f, p, "run_id", "source_run_record_id", "source_run_record_sequence")
	return f, nil
}

func observationGapsPresent(gaps []archive.CaptureGap) bool {
	partial, unqualified := false, false
	for _, g := range gaps {
		partial = partial || g.Code == "own_usage_observations_partial"
		unqualified = unqualified || g.Code == "cache_reasoning_cost_unqualified"
	}
	return partial && unqualified
}
