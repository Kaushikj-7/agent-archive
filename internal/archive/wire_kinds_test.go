package archive

import "testing"

// TestWireKindSpellings pins the kind values written into handoff documents
// and source bundles, and the Cursor text role headers read from them. The
// typed constants are the only place these spellings live, so a renamed
// constant value would otherwise change the persisted format without failing
// any test.
func TestWireKindSpellings(t *testing.T) {
	t.Parallel()
	cases := []struct {
		got  string
		want string
	}{
		{string(HandoffStepText), "text"},
		{string(HandoffStepTool), "tool"},
		{string(HandoffStepShell), "shell"},
		{string(HandoffStepSummary), "summary"},
		{string(HandoffStepCollapsed), "collapsed"},
		{string(HandoffElisionToolOutput), "tool_output"},
		{string(HandoffElisionToolCalls), "tool_calls"},
		{string(HandoffElisionAssistantText), "assistant_text"},
		{string(HandoffElisionPromptText), "prompt_text"},
		{string(HandoffElisionAgentMessages), "agent_messages"},
		{string(HandoffElisionExchanges), "exchanges"},
		{string(HandoffStepOmitted), "omitted"},
		{string(SourceLineHeader), "header"},
		{string(SourceLineNativeRecord), "native_record"},
		{string(SourceLineNativeText), "native_text"},
		{string(SourceLineSupplementalEvidence), "supplemental_evidence"},
	}

	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("kind = %q, want %q", c.got, c.want)
		}
	}
}
