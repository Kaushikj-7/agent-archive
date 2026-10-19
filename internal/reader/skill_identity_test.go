package reader

import (
	"encoding/json"
	"github.com/wangjohn/agent-archive/internal/archive"
	"reflect"
	"testing"
)

func TestSearchSummaryPreservesInstalledSkillIdentities(t *testing.T) {
	t.Parallel()
	original := archive.Metadata{SkillsAvailable: []archive.SkillSnapshot{{Name: "shared-name", SHA256: "same-hash", Coverage: archive.SkillCoverageInstalledOnly, InstalledIdentities: []archive.SkillDeclarationIdentity{{Scope: "package-one", ID: "first"}, {Scope: "package-one", ID: "second"}, {Scope: "package-two", ID: "first"}}}}}
	raw, err := json.Marshal(summarize(original))
	if err != nil {
		t.Fatal(err)
	}
	var decoded SearchSummary
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded.Metadata().SkillsAvailable, original.SkillsAvailable) {
		t.Fatal("catalog projection dropped installed identity")
	}
	// These are installed identities, not evidence for the availability filter.
	if matchesAvailableSkill(decoded.Metadata(), Filter{Skill: "shared-name"}) {
		t.Fatal("logical installed provenance became eligible")
	}
}
