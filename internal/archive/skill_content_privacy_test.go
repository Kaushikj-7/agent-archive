package archive

import (
	"strings"
	"testing"
)

// Every credential family and its composed passes must preserve the marker:
// otherwise marker-free fixed points cease to have a singleton R-preimage.
func TestSkillContentRedactionMarkerInvariant(t *testing.T) {
	t.Parallel()
	inputs := append([]string(nil), gateSeeds()...)
	for _, template := range credentialTemplates {
		inputs = append(inputs, strings.ReplaceAll(template, "%s", "SyntheticCredential42"))
	}
	inputs = append(inputs, "host:5432:database:user:SyntheticCredential42", "password: |\n  SyntheticCredential42\nnext: safe", "-----BEGIN PRIVATE KEY-----\nMIIEpAIBAAKCAQEA7synMIIEpAIBAAKCAQEA7syn\n-----END PRIVATE KEY-----")
	changed := 0
	for _, input := range inputs {
		out := RedactText(input)
		if out != input {
			changed++
			if !strings.Contains(out, redactedMarker) || MarkerFreeRedactionFixedPoint(input, out) {
				t.Fatal("changed redactor output lost its marker")
			}
		}
		if next := RedactText(out); next != out && !strings.Contains(next, redactedMarker) {
			t.Fatal("later pass removed every marker")
		}
		joined := "password=FirstSyntheticCredential42\n" + input + "\nAuthorization: Bearer SecondSyntheticCredential42"
		if out := RedactText(joined); out != joined && !strings.Contains(out, redactedMarker) {
			t.Fatal("composed families lost the marker")
		}
	}
	if changed < len(credentialTemplates) {
		t.Fatal("credential-family coverage unexpectedly disappeared")
	}
	for _, s := range []string{"ordinary markdown\n", "---\nname: declared-label\n---\nAn ordinary skill.\n"} {
		if !MarkerFreeRedactionFixedPoint(s, RedactText(s)) {
			t.Fatal("ordinary content refused")
		}
	}
	for _, s := range []string{"[REDACTED]", "password=[REDACTED]", "prose containing [REDACTED]"} {
		if MarkerFreeRedactionFixedPoint(s, RedactText(s)) {
			t.Fatal("literal marker admitted")
		}
	}
}
