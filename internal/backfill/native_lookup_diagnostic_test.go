package backfill

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestNativeLookupWarningIsOptionalClosedAndIndependentOfSkips(t *testing.T) {
	const private = "synthetic-private-native-path-and-content"
	const privateDetail DiagnosticDetail = private
	const privateAction DiagnosticAction = private
	for _, tc := range []struct {
		name       string
		diagnostic *Diagnostic
		visible    bool
	}{
		{name: "complete"},
		{name: "incomplete", diagnostic: &Diagnostic{Detail: DetailLookupNoProgress, Action: ActionRetry}, visible: true},
		{name: "other cause", diagnostic: &Diagnostic{Detail: DetailHistoryBaseUnavailable, Action: ActionRetry}},
		{name: "unknown cause", diagnostic: &Diagnostic{Detail: privateDetail, Action: ActionRetry}},
		{name: "unknown action", diagnostic: &Diagnostic{Detail: DetailLookupNoProgress, Action: privateAction}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := Plan{NativeLookupIncomplete: tc.diagnostic, CursorDatabaseChecked: true, Candidates: []Candidate{{Harness: "claude", ProjectRoot: "/synthetic/project", ProjectKind: ProjectKindRepository}}}
			if len(p.Skipped()) != 0 {
				t.Fatal("warning fixture has skipped candidates")
			}
			var human, structured bytes.Buffer
			RenderText(&human, p)
			if err := RenderJSON(&structured, p); err != nil {
				t.Fatal(err)
			}
			var document map[string]json.RawMessage
			if err := json.Unmarshal(structured.Bytes(), &document); err != nil {
				t.Fatal(err)
			}
			_, visible := document["native_lookup_incomplete"]
			if visible != tc.visible || strings.Contains(human.String(), "Warning: native lookup is incomplete") != tc.visible {
				t.Fatal("inventory warning visibility does not match incomplete evidence", human.String(), structured.String())
			}
			if tc.visible && (!strings.Contains(structured.String(), `"detail": "lookup_no_progress"`) || !strings.Contains(structured.String(), `"action": "retry"`)) {
				t.Fatal("inventory warning lost its finite cause/action", structured.String())
			}
			if strings.Contains(human.String(), private) || strings.Contains(structured.String(), private) {
				t.Fatal("inventory warning leaked an unchecked diagnostic")
			}
		})
	}
}
