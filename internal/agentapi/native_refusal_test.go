package agentapi

import (
	"errors"
	"strings"
	"testing"
)

func TestNativeRefusalReportsOnlySchemaIdentifierFamilies(t *testing.T) {
	t.Parallel()
	cause := errors.New("closed format")
	for family, want := range map[string]string{
		"runtime.session/approval/requested": "runtime.session/approval/requested",
		"runtime.session_start.intake_v3":    "runtime.session_start.intake_v3",
		"Hello World":                        "",
		"/Users/someone/project":             "",
		"sk-ant-api03-invented":              "",
		"session.token_9f8e7d6c":             "",
		strings.Repeat("a.", 60) + "a":       "",
	} {
		got := NewNativeRefusalError(RefusalUnsupportedRecord, family, 7, cause)
		if got.Family != want || !errors.Is(got, cause) {
			t.Fatalf("family %q kept as %q", family, got.Family)
		}
		if want == "" && strings.Contains(got.Error(), family) {
			t.Fatalf("message carries refused family: %s", got.Error())
		}
	}
}
