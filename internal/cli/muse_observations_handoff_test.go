package cli

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestMuseAutomaticHandoffPreservesObservationsThroughLiveProof(t *testing.T) {
	t.Parallel()
	env, data, workspace, counter := museNativeEnv(t)
	raw := museNativeFixtureBytes(t, "synthetic-own-observations.jsonl", museSyntheticID, workspace)
	writeMuseNative(t, data, museSyntheticID, raw)
	meter := &nativeReadMeter{}
	env.nativeFS = meter
	out, errOut, code := runHandoff(t, env, "--latest", "--source", "local", "--harness", "muse", "--format", "json")
	if code != 0 {
		t.Fatalf("handoff failed: %d %s", code, errOut)
	}
	var handoff archive.Handoff
	if err := json.Unmarshal([]byte(out), &handoff); err != nil {
		t.Fatal(err)
	}
	summary := handoff.Session.TokenObservations
	if !summary.HasObservations() || *summary.Input != 202 || *summary.Output != 46 || summary.Coverage.Available() || summary.Composition.Available() || summary.Pricing.Available() || counter.calls.Load() != 1 {
		t.Fatal("automatic live proof lost observations or inferred accounting")
	}
	_, opens, active, _ := meter.counts()
	if opens != 2 || active != 0 || strings.Contains(out, "muse-ordinary") {
		t.Fatal("live snapshot recheck/lifetime or portable proof changed")
	}
	if _, err := os.Stat(museNativeArchiveHome(t, env)); !os.IsNotExist(err) {
		t.Fatal("local handoff created capture state", err)
	}
}
