package sourcefacts

import (
	"context"
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
