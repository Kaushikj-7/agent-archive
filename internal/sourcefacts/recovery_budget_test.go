package sourcefacts

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// stampedInventory has twelve configured roots of deps dependency stamps each;
// only the last root holds key. lookups counts repository observations.
func stampedInventory(key string, deps int, lookups *int) ([]archive.ProjectActivation, RepositoryLookup) {
	var projects []archive.ProjectActivation
	for i := range 12 {
		projects = append(projects, archive.ProjectActivation{Root: fmt.Sprintf("/p%02d", i), Included: true})
	}
	lookup := func(_ context.Context, path string) RepositoryIdentity {
		*lookups++
		k := ""
		if path == projects[len(projects)-1].Root {
			k = key
		}
		stamps := make([]RepositoryDependency, deps)
		for j := range stamps {
			stamps[j] = RepositoryDependency{Path: fmt.Sprintf("%s/.git/config.%d", path, j), Stamp: "synthetic"}
		}
		return RepositoryIdentity{Root: path, Key: k, Known: true, Dependencies: stamps}
	}
	return projects, lookup
}

// Planning used to recheck every stamp for every candidate against one
// allowance sized for a single prefix validation, so the tail of a large
// plan was project_budget_exhausted however often it was retried.
func TestRecoveryPlanningValidatesInventoryPrefixOncePerSlice(t *testing.T) {
	key := archive.RepoKey("https://example.test/acme/repo")
	const deps = 20
	lookups := 0
	projects, lookup := stampedInventory(key, deps, &lookups)
	inv := &RecoveryInventory{}
	// A resumed unfinished prefix is validated once, then not again.
	warm := NewRecoveryResolver(projects, nil, filepath.Clean, lookup, inv)
	warm.MaxOperations = 6
	if _, outcome := warm.Recover(t.Context(), "/warm", key); outcome != RecoveryBudgetExhausted || inv.Cursor != 6 {
		t.Fatal(outcome, inv.Cursor)
	}
	r := NewRecoveryResolver(projects, nil, filepath.Clean, lookup, inv)
	validations := 0
	r.Validate = func(RepositoryIdentity) bool { validations++; return true }
	r.ResetValidationContext(t.Context())
	// The old per-candidate cost (12*20 stamps) exhausted the allowance of
	// 2*12*129 = 3,096 at the 13th candidate.
	var proofs []archive.ProjectResolution
	for i := range 200 {
		proof, outcome := r.Recover(t.Context(), fmt.Sprintf("/deleted/worktree-%d/repo", i), key)
		if outcome != "" || proof.Root != projects[len(projects)-1].Root {
			t.Fatalf("candidate %d: %s after %d metadata operations", i, outcome, r.MetadataOperations)
		}
		proofs = append(proofs, proof)
	}
	// Bound: one validation of the inventory prefix per slice, however many
	// candidates are planned.
	if bound := len(projects) * deps; r.MetadataOperations > bound || validations != 6 || r.MetadataExhausted || lookups != len(projects) {
		t.Fatalf("planning spent %d metadata operations (%d validations, %d lookups), bound %d", r.MetadataOperations, validations, lookups, bound)
	}
	r.ResetValidationContext(t.Context())
	for _, proof := range proofs {
		if !r.CurrentSlice(proof) {
			t.Fatal("admission slice rejected an unchanged inventory")
		}
	}
}

// Skipping the per-candidate recheck must not hide a repository change:
// admission still rechecks every entry, a failed Current makes the next
// candidate revalidate, and a new slice revalidates from the start.
func TestRecoveryPlanningStillCatchesRepositoryChanges(t *testing.T) {
	key := archive.RepoKey("https://example.test/acme/repo")
	lookups := 0
	projects, lookup := stampedInventory(key, 2, &lookups)
	stale := ""
	r := NewRecoveryResolver(projects, nil, filepath.Clean, lookup, nil)
	r.Validate = func(id RepositoryIdentity) bool { return id.Root != stale }
	r.ResetValidationContext(t.Context())
	proof, outcome := r.Recover(t.Context(), "/deleted/a", key)
	if outcome != "" {
		t.Fatal(outcome)
	}
	// Changed between planning and admission: the admission slice rejects it.
	stale = projects[3].Root
	r.ResetValidationContext(t.Context())
	if r.CurrentSlice(proof) || r.MetadataExhausted {
		t.Fatal("admission accepted a changed configured repository")
	}
	// A failed Current makes the next candidate revalidate the prefix and
	// reobserve from the changed entry instead of planning on stale entries.
	stale = ""
	before := lookups
	if _, outcome := r.Recover(t.Context(), "/deleted/validated", key); outcome != "" || lookups != before {
		t.Fatal("unchanged prefix reobserved", outcome, lookups-before)
	}
	stale = projects[3].Root
	if r.Current(proof) {
		t.Fatal("changed configured repository current")
	}
	if _, outcome := r.Recover(t.Context(), "/deleted/b", key); outcome != "" || lookups != before+len(projects)-3 {
		t.Fatal("failed Current did not force revalidation", outcome, lookups-before)
	}
	// A new slice revalidates the prefix even without a failed check.
	stale = projects[5].Root
	r.ResetValidation()
	before = lookups
	if _, outcome := r.Recover(t.Context(), "/deleted/c", key); outcome != "" || lookups != before+len(projects)-5 {
		t.Fatal("new slice skipped prefix validation", outcome, lookups-before)
	}
	// Within the slice, the freshly reobserved entries are not charged again.
	stale = ""
	before, spent := lookups, r.MetadataOperations
	if _, outcome := r.Recover(t.Context(), "/deleted/d", key); outcome != "" || lookups != before || r.MetadataOperations != spent {
		t.Fatal("validated prefix rechecked", outcome, lookups-before, r.MetadataOperations-spent)
	}
}

// An allowance exhausted earlier in the slice (for example by the semantic
// sweep) must not hide a change a later Current does observe: the next
// candidate is not planned on the stale prefix. A Current that fails only for
// its own budget keeps the validated prefix, so later candidates still plan.
func TestRecoveryEarlierExhaustionDoesNotHideObservedChange(t *testing.T) {
	key := archive.RepoKey("https://example.test/acme/repo")
	lookups := 0
	projects, lookup := stampedInventory(key, 2, &lookups)
	stale := ""
	r := NewRecoveryResolver(projects, nil, filepath.Clean, lookup, nil)
	r.Validate = func(id RepositoryIdentity) bool { return id.Root != stale }
	r.ResetValidationContext(t.Context())
	proof, outcome := r.Recover(t.Context(), "/deleted/a", key)
	if outcome != "" {
		t.Fatal(outcome)
	}
	r.MetadataExhausted = true
	stale = projects[3].Root
	if r.Current(proof) || !r.MetadataExhausted {
		t.Fatal("changed configured repository current, or exhaustion forgotten")
	}
	if _, outcome := r.Recover(t.Context(), "/deleted/b", key); outcome == "" {
		t.Fatal("candidate planned on a prefix a failed Current showed stale")
	}

	stale = ""
	r.ResetValidationContext(t.Context())
	if _, outcome := r.Recover(t.Context(), "/deleted/c", key); outcome != "" {
		t.Fatal(outcome)
	}
	r.MetadataOperations = r.metadataLimit()
	if r.Current(proof) || !r.MetadataExhausted {
		t.Fatal("Current beyond the allowance")
	}
	if _, outcome := r.Recover(t.Context(), "/deleted/d", key); outcome != "" {
		t.Fatal("budget-only Current failure discarded the validated prefix", outcome)
	}
}
