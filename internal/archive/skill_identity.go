package archive

// SkillDeclarationIdentity identifies an installed declaration within a logical
// package scope. Neither field is a filesystem path or native eligibility claim.
type SkillDeclarationIdentity struct {
	Scope string `json:"scope"`
	ID    string `json:"id"`
}

// Valid accepts only bounded logical tokens safe for installed provenance.
func (identity SkillDeclarationIdentity) Valid() bool {
	if len(identity.Scope) == 0 || len(identity.Scope) > 160 || len(identity.ID) == 0 || len(identity.ID) > 80 {
		return false
	}
	for _, c := range []byte(identity.Scope) {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.' || c == ':' {
			continue
		}
		return false
	}
	for i, c := range []byte(identity.ID) {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || i > 0 && (c == '.' || c == '_' || c == '-') {
			continue
		}
		return false
	}
	return true
}

const maxInstalledSkillIdentities = 256

type installedSkillIdentitySet struct {
	values  map[SkillDeclarationIdentity]bool
	omitted bool
}

func (set *installedSkillIdentitySet) add(identity SkillDeclarationIdentity) bool {
	if set.omitted {
		return false
	}
	if set.values[identity] {
		return true
	}
	if len(set.values) == maxInstalledSkillIdentities {
		set.values = nil
		set.omitted = true
		return false
	}
	if set.values == nil {
		set.values = map[SkillDeclarationIdentity]bool{}
	}
	set.values[identity] = true
	return true
}

func recordInstalledSkillIdentity(identities map[string]*installedSkillIdentitySet, harness, key string, evidence SupplementalEvidence, payload map[string]any, coverage SkillCoverage) bool {
	if CanonicalHarness(harness) != "muse" || coverage != SkillCoverageInstalledOnly || (evidence.Kind != EvidenceKindSkillInventory && evidence.Kind != EvidenceKindSkillSnapshot) || evidence.Provenance != "filesystem:muse" {
		return false
	}
	if _, present := payload["id"]; !present {
		return false
	} // Legacy evidence is unchanged.
	declared := SkillDeclarationIdentity{Scope: firstString(evidence.Payload, "scope"), ID: firstString(payload, "id")}
	if !declared.Valid() {
		return true
	}
	set := identities[key]
	if set == nil {
		set = &installedSkillIdentitySet{}
		identities[key] = set
	}
	return !set.add(declared)
}
