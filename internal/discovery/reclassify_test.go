package discovery

import (
	"testing"

	"github.com/wangjohn/agent-archive/internal/codexmeta"
)

// Regression: 2026-10 the owner's three agent-created Codex threads stayed
// refused after capture learned to read them: their archived rollouts never
// change, so the cached unsupported_execution observation stood and the
// rollout lookup never saw their identity.
// A catalog from an older classifier rereads exactly those refusals, which
// carry no identity, once.
func TestReclassifyRereadsOnlyOlderExecutionRefusals(t *testing.T) {
	t.Parallel()
	id := &codexmeta.CodexIdentity{ThreadID: "t", RolloutID: "t"}
	c := catalog{Cache: map[string]cached{
		"/codex/archived_sessions/agent-created": {Observation: Observation{Outcome: outcomeUnsupportedExec}},
		"/codex/sessions/usable":                 {Observation: Observation{Outcome: outcomeUsable, Identity: id}},
		"/codex/sessions/incomplete":             {Observation: Observation{Outcome: outcomeIncomplete}},
		"/codex/sessions/own-task-rejected":      {Observation: Observation{Outcome: outcomeOwnTaskRejected}},
		// A guardian review child keeps its identity through the refusal.
		"/codex/sessions/guardian-child": {Observation: Observation{Outcome: outcomeUnsupportedExec, Identity: id}},
	}}
	c.reclassify()
	if _, kept := c.Cache["/codex/archived_sessions/agent-created"]; kept || len(c.Cache) != 4 || c.Classifier != headerClassifier {
		t.Fatalf("cache=%v classifier=%d", c.Cache, c.Classifier)
	}
	// A refusal cached under the current classifier stands: it is not
	// reread on every pass.
	c.Cache["/codex/sessions/future-source"] = cached{Observation: Observation{Outcome: outcomeUnsupportedExec}}
	c.reclassify()
	if _, kept := c.Cache["/codex/sessions/future-source"]; !kept {
		t.Fatal("a current refusal was dropped again")
	}
}
