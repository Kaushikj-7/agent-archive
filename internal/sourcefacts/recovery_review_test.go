package sourcefacts

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestMappingReviewSharesObservationsWithoutChangingAssignments(t *testing.T) {
	key := archive.RepoKey("https://example.test/acme/repo")
	calls := 0
	r := NewRecoveryResolver([]archive.ProjectActivation{{Root: "/target", Included: true}, {Root: "/excluded", Included: false}}, nil, func(path string) string { return path }, func(_ context.Context, root string) RepositoryIdentity {
		calls++
		return RepositoryIdentity{Root: root, Key: key, Known: true}
	}, nil)
	for _, cwd := range []string{"/old/a", "/old/b"} {
		if outcome := r.ReviewMapping(t.Context(), cwd, key, "/target"); outcome != "" {
			t.Fatal(outcome)
		}
	}
	if calls != 1 || r.Operations != 1 || len(r.Mappings) != 0 || len(r.results) != 0 {
		t.Fatal("review did not share observations or changed authority", calls, r.Operations, r.Mappings, r.results)
	}
	if outcome := r.ReviewMapping(t.Context(), "/old/a", archive.RepoKey("https://example.test/other/repo"), "/target"); outcome != RecoveryMappingConflict {
		t.Fatal("conflicting key accepted", outcome)
	}
	if outcome := r.ReviewMapping(t.Context(), "/old/a", key, "/excluded"); outcome != RecoveryMappingConflict {
		t.Fatal("excluded target accepted", outcome)
	}
	if outcome := r.ReviewMapping(t.Context(), "/excluded/child", key, "/target"); outcome != RecoveryExcluded {
		t.Fatal("source exclusion bypassed", outcome)
	}
}

func TestMappingReviewReusesConflictingTargetObservationsForLaterMatchingGroups(t *testing.T) {
	const targetCount = 128
	key := archive.RepoKey("https://example.test/acme/repo")
	currentKey := key
	projects := make([]archive.ProjectActivation, targetCount)
	for i := range projects {
		projects[i] = archive.ProjectActivation{Root: fmt.Sprintf("/target/%03d", i), Included: true}
	}
	calls := 0
	r := NewRecoveryResolver(projects, map[string]string{"/mapped": projects[0].Root}, filepath.Clean, func(_ context.Context, root string) RepositoryIdentity {
		calls++
		return RepositoryIdentity{Root: root, Key: currentKey, Known: true, Validation: "semantic", ObservedRoot: root}
	}, nil)
	r.MaxOperations = 1024
	for group := range 8 {
		other := archive.RepoKey(fmt.Sprintf("https://example.test/other/repo-%d", group))
		for _, project := range projects {
			if outcome := r.ReviewMapping(t.Context(), fmt.Sprintf("/gone/%d", group), other, project.Root); outcome != RecoveryMappingConflict {
				t.Fatal("mismatching group lost its specific refusal", group, outcome)
			}
		}
	}
	if outcome := r.ReviewMapping(t.Context(), "/gone/matching", key, projects[0].Root); outcome != "" {
		t.Fatal("earlier mismatches exhausted a later compatible choice", outcome, calls, r.Operations)
	}
	if calls != targetCount || r.Operations != targetCount || len(r.mappedIdentities) != targetCount || len(r.results) != 0 || len(r.Mappings) != 1 {
		t.Fatal("target facts were not shared independently of group keys", calls, r.Operations, len(r.mappedIdentities), r.Mappings)
	}
	proof, outcome := r.Recover(t.Context(), "/mapped", key)
	if outcome != "" || proof.Method != "explicit_mapping" {
		t.Fatal("ordinary exact mapping lost its frozen target", proof, outcome)
	}
	currentKey = archive.RepoKey("https://example.test/acme/changed")
	if outcome := r.ReviewMapping(t.Context(), "/gone/changed", currentKey, projects[0].Root); outcome != RecoveryMappingConflict || calls != targetCount {
		t.Fatal("review replaced a planning observation with a new identity", outcome, calls)
	}
	r.ResetValidationContext(t.Context())
	if r.CurrentSlice(proof) {
		t.Fatal("shared review observations granted stale admission authority")
	}
}
