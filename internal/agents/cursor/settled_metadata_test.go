package cursor

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestSettledRosterPreflightPreservesCanonicalFacts(t *testing.T) {
	ids := make([]string, 64)
	for i := range ids {
		ids[i] = fmt.Sprintf("child-%02d", 63-i)
	}
	raw, err := json.Marshal(ids)
	if err != nil {
		t.Fatal(err)
	}
	if err = validateSettledRoster(t.Context(), raw); err != nil {
		t.Fatal(err)
	}
	facts := composerReviewFacts(map[string]json.RawMessage{"subagentComposerIds": raw})
	if facts.ChildIDsSHA256 == "" {
		t.Fatal("canonical roster digest missing")
	}
	ids = append(ids, "child-overflow")
	raw, err = json.Marshal(ids)
	if err != nil {
		t.Fatal(err)
	}
	if err = validateSettledRoster(t.Context(), raw); err == nil {
		t.Fatal("oversize roster reached lenient decoder")
	}
	for _, raw := range []string{`null`, `{}`, `[null]`, `[1]`, `[""]`, `["same","same"]`} {
		if err = validateSettledRoster(t.Context(), []byte(raw)); err == nil {
			t.Fatalf("malformed roster accepted: %s", raw)
		}
	}
	oversized, err := json.Marshal([]string{strings.Repeat("x", 256)})
	if err != nil {
		t.Fatal(err)
	}
	if err = validateSettledRoster(t.Context(), oversized); err == nil {
		t.Fatal("oversize ID reached lenient roster decode")
	}

}

func TestSettledMetadataRejectsUnqualifiedRouting(t *testing.T) {
	for _, raw := range []string{
		`{"composerId":"foreign","workspaceIdentifier":null}`,
		`{"composerId":"foreign","workspaceIdentifier":{"uri":null}}`,
		`{"composerId":"foreign","workspaceIdentifier":{"uri":{"Scheme":"file","path":"/project"}}}`,
		`{"composerId":"foreign","workspaceIdentifier":{"uri":{"scheme":null,"path":"/project"}}}`,
		`{"composerId":"foreign","workspaceIdentifier":{"uri":{"authority":1,"path":"/project"}}}`,
		`{"composerId":"foreign","workspaceIdentifier":{"uri":{"fsPath":false}}}`,
	} {
		if err := validateSettledMetadata(t.Context(), "composerData:foreign", []byte(raw)); err == nil {
			t.Fatalf("unqualified global routing accepted: %s", raw)
		}
	}
	for _, raw := range []string{
		`{"composerId":"foreign"}`,
		`{"composerId":"foreign","workspaceIdentifier":{"uri":{"scheme":"file","path":"/project","unrelated":1}}}`,
	} {
		if err := validateSettledMetadata(t.Context(), "composerData:foreign", []byte(raw)); err != nil {
			t.Fatalf("qualified global routing: %v", err)
		}
	}
}
