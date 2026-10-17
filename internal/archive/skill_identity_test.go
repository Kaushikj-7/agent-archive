package archive

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func installedIdentityEvidence(scope, id string) SupplementalEvidence {
	return SupplementalEvidence{Kind: EvidenceKindSkillSnapshot, Provenance: "filesystem:muse", Payload: map[string]any{"name": "shared-name", "sha256": "same-hash", "scope": scope, "id": id, "coverage": string(SkillCoverageInstalledOnly)}}
}

func TestMuseInstalledIdentityContentGroupingAndHistory(t *testing.T) {
	t.Parallel()
	first := installedIdentityEvidence("package-one", "first")
	second := installedIdentityEvidence("package-one", "second")
	other := installedIdentityEvidence("package-two", "first")
	inventory := SupplementalEvidence{Kind: EvidenceKindSkillInventory, Provenance: "filesystem:muse", Payload: map[string]any{"scope": "package-one", "coverage": string(SkillCoverageInstalledOnly), "skills": []any{map[string]any{"name": "shared-name", "sha256": "same-hash", "id": "first"}, map[string]any{"name": "shared-name", "sha256": "same-hash", "id": "second"}}}}
	if supplementalIdentity(first) == supplementalIdentity(second) {
		t.Fatal("declared snapshot identity ignored ID")
	}
	merged := MergeSupplementalEvidence([]SupplementalEvidence{first}, []SupplementalEvidence{second, other, first, inventory})
	if len(merged) != 4 {
		t.Fatalf("declaration history collapsed/duplicated: %d", len(merged))
	}
	bundle := SourceBundle{Capture: SourceCapture{Harness: Harness{Name: "muse"}}, SupplementalEvidence: merged}
	var metadata Metadata
	deriveSkills(bundle, nil, &metadata)
	if len(metadata.SkillsAvailable) != 1 || len(metadata.SkillsAvailable[0].InstalledIdentities) != 3 {
		t.Fatalf("logical identities lost: %+v", metadata.SkillsAvailable)
	}
	expected := []SkillDeclarationIdentity{{Scope: "package-one", ID: "first"}, {Scope: "package-one", ID: "second"}, {Scope: "package-two", ID: "first"}}
	for i, value := range expected {
		if metadata.SkillsAvailable[0].InstalledIdentities[i] != value {
			t.Fatal("identities not sorted unique")
		}
	}
	if len(metadata.SkillsUsed) != 0 || metadata.SkillDetection == SkillDetectionObserved || len(metadata.CaptureGaps) != 0 {
		t.Fatal("installed identity became use/coverage gap")
	}
	// An actual old typed reader ignores the optional collection and retains one
	// unchanged content row rather than indistinguishable duplicate skill rows.
	encoded, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	var legacy struct {
		SkillsAvailable []struct {
			Name     string        `json:"name"`
			SHA256   string        `json:"sha256"`
			Coverage SkillCoverage `json:"coverage"`
		} `json:"skills_available"`
	}
	if err := json.Unmarshal(encoded, &legacy); err != nil || len(legacy.SkillsAvailable) != 1 || legacy.SkillsAvailable[0].Name != "shared-name" || legacy.SkillsAvailable[0].Coverage != SkillCoverageInstalledOnly {
		t.Fatal("old typed row compatibility lost", err)
	}
	// Other harness metadata/parser semantics remain unchanged.
	bundle.Capture.Harness.Name = "codex"
	metadata = Metadata{}
	deriveSkills(bundle, nil, &metadata)
	if len(metadata.SkillsAvailable) != 1 || len(metadata.SkillsAvailable[0].InstalledIdentities) != 0 || len(metadata.CaptureGaps) != 0 {
		t.Fatal("Muse identity derivation affected another harness")
	}
}

func TestMuseInstalledIdentityOverflowIsStickyAndMalformedIsBounded(t *testing.T) {
	t.Parallel()
	items := make([]SupplementalEvidence, 0, 300)
	for i := 0; i < 257; i++ {
		items = append(items, installedIdentityEvidence("package", fmt.Sprintf("skill-%03d", i)))
	}
	// Repeat a previously valid identity after overflow: collection stays omitted.
	items = append(items, installedIdentityEvidence("package", "skill-000"))
	for _, bad := range []SupplementalEvidence{installedIdentityEvidence("/PRIVATE_PATH_SENTINEL", "ok"), installedIdentityEvidence("scope", "bad\nid"), installedIdentityEvidence("scope", strings.Repeat("x", 81))} {
		items = append(items, bad, bad)
	}
	var metadata Metadata
	deriveSkills(SourceBundle{Capture: SourceCapture{Harness: Harness{Name: "muse"}}, SupplementalEvidence: items}, nil, &metadata)
	if len(metadata.SkillsAvailable) != 1 || len(metadata.SkillsAvailable[0].InstalledIdentities) != 0 || len(metadata.CaptureGaps) != 1 || metadata.CaptureGaps[0].Code != "installed_skill_identity_incomplete" {
		t.Fatalf("overflow/malformed provenance not bounded: %+v", metadata)
	}
	raw, _ := json.Marshal(metadata)
	if strings.Contains(string(raw), "PRIVATE_PATH_SENTINEL") || strings.Contains(string(raw), "bad\\nid") {
		t.Fatal("omitted identity leaked through gap")
	}
	// Malformed alone leaves other validated identities and only one stable gap.
	metadata = Metadata{}
	deriveSkills(SourceBundle{Capture: SourceCapture{Harness: Harness{Name: "muse"}}, SupplementalEvidence: []SupplementalEvidence{installedIdentityEvidence("ok", "valid"), installedIdentityEvidence("/private", "bad"), installedIdentityEvidence("/private", "bad")}}, nil, &metadata)
	if len(metadata.SkillsAvailable[0].InstalledIdentities) != 1 || len(metadata.CaptureGaps) != 1 {
		t.Fatal("malformed identity removed valid data or multiplied gaps")
	}
}

func TestMuseInstalledIdentityLegacyBytesAndMergeIdentityUnchanged(t *testing.T) {
	t.Parallel()
	item := installedIdentityEvidence("scope", "first")
	delete(item.Payload, "id")
	before := string(item.Kind) + "\x00filesystem:muse\x00shared-name\x00scope"
	if supplementalIdentity(item) != before {
		t.Fatal("absent id changed historical identity")
	}
	var metadata Metadata
	deriveSkills(SourceBundle{Capture: SourceCapture{Harness: Harness{Name: "muse"}}, SupplementalEvidence: []SupplementalEvidence{item}}, nil, &metadata)
	raw, err := json.Marshal(metadata.SkillsAvailable)
	if err != nil || string(raw) != `[{"name":"shared-name","sha256":"same-hash","coverage":"installed_only"}]` || len(metadata.CaptureGaps) != 0 {
		t.Fatalf("legacy optional-field bytes changed: %s %v", raw, err)
	}
}
