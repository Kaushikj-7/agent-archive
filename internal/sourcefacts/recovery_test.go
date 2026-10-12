package sourcefacts

import (
	"context"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/archive"
	"os"
	"path/filepath"
	"testing"
)

func TestRecoveryCompleteInventory(t *testing.T) {
	key := archive.RepoKey("git@example.test:acme/widget.git")
	for _, tc := range []struct {
		name     string
		projects []archive.ProjectActivation
		ids      map[string]RepositoryIdentity
		want     RecoveryOutcome
	}{
		{"unique", []archive.ProjectActivation{{Root: "/a", Included: true}}, map[string]RepositoryIdentity{"/a": {Root: "/a", Key: key, Known: true}}, ""},
		{"excluded clone", []archive.ProjectActivation{{Root: "/a", Included: true}, {Root: "/b", Included: false}}, map[string]RepositoryIdentity{"/a": {Root: "/a", Key: key, Known: true}, "/b": {Root: "/b", Key: key, Known: true}}, RecoveryAmbiguous},
		{"excluded unique", []archive.ProjectActivation{{Root: "/a", Included: false}}, map[string]RepositoryIdentity{"/a": {Root: "/a", Key: key, Known: true}}, RecoveryExcluded},
		{"unknown clone", []archive.ProjectActivation{{Root: "/a", Included: true}, {Root: "/b", Included: false}}, map[string]RepositoryIdentity{"/a": {Root: "/a", Key: key, Known: true}}, RecoveryInventoryUnavailable},
		{"scratch is known", []archive.ProjectActivation{{Root: "/a", Included: true}, {Root: "/scratch", Included: true}}, map[string]RepositoryIdentity{"/a": {Root: "/a", Key: key, Known: true}, "/scratch": {Known: true}}, ""},
		{"nested exclusion", []archive.ProjectActivation{{Root: "/a", Included: true}, {Root: "/a/private", Included: false}}, map[string]RepositoryIdentity{"/a": {Root: "/a", Key: key, Known: true}, "/a/private": {Root: "/a", Key: key, Known: true}}, RecoverySubtreeUnavailable},
		{"same basename different remote", []archive.ProjectActivation{{Root: "/a/widget", Included: true}}, map[string]RepositoryIdentity{"/a/widget": {Root: "/a/widget", Key: archive.RepoKey("https://other.test/org/widget"), Known: true}}, RecoveryRepositoryUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			r := NewRecoveryResolver(tc.projects, nil, filepath.Clean, func(_ context.Context, root string) RepositoryIdentity { calls++; return tc.ids[root] }, nil)
			proof, outcome := r.Recover(context.Background(), "/gone/widget", key)
			if outcome != tc.want {
				t.Fatalf("%s: %+v", outcome, proof)
			}
			if outcome == "" && (proof.OriginalCwd != "/gone/widget" || proof.RecordedRepoKey != key || proof.Context == "" || proof.InventoryDigest == "") {
				t.Fatalf("missing proof %+v", proof)
			}
			r.Recover(context.Background(), "/gone/second", key)
			if calls != len(tc.projects) {
				t.Fatalf("inventory reread %d", calls)
			}
		})
	}
}

func TestRecoveryMappingsAndExplicitRules(t *testing.T) {
	key := archive.RepoKey("https://example.test/acme/repo")
	projects := []archive.ProjectActivation{{Root: "/a", Included: true}, {Root: "/b", Included: false}, {Root: "/gone/private", Included: false}}
	calls := 0
	lookup := func(_ context.Context, path string) RepositoryIdentity {
		calls++
		return RepositoryIdentity{Root: path, Key: key, Known: true}
	}
	r := NewRecoveryResolver(projects, map[string]string{"/gone": "/a", "/gone/private": "/a"}, filepath.Clean, lookup, nil)
	proof, outcome := r.Recover(context.Background(), "/gone", key)
	if outcome != "" || proof.Root != "/a" || proof.Method != "explicit_mapping" {
		t.Fatalf("%+v %s", proof, outcome)
	}
	if calls != 1 {
		t.Fatalf("exact mapping unnecessarily swept other clones: %d", calls)
	}
	_, outcome = r.Recover(context.Background(), "/gone/private", key)
	if outcome != RecoveryExcluded {
		t.Fatal(outcome)
	}
	_, outcome = r.Recover(context.Background(), "/gone", archive.RepoKey("https://different.test/repo"))
	if outcome != RecoveryMappingConflict {
		t.Fatal(outcome)
	}
	_, outcome = r.Recover(context.Background(), "/gone/descendant", key)
	if outcome != RecoveryAmbiguous {
		t.Fatal(outcome)
	}
}

func TestRecoveryInventoryResumesAndCancellation(t *testing.T) {
	key := archive.RepoKey("https://example.test/acme/repo")
	var projects []archive.ProjectActivation
	for i := range 300 {
		projects = append(projects, archive.ProjectActivation{Root: fmt.Sprintf("/p%03d", i), Included: true})
	}
	inv := &RecoveryInventory{}
	calls := 0
	lookup := func(_ context.Context, path string) RepositoryIdentity {
		calls++
		k := ""
		if path == "/p299" {
			k = key
		}
		return RepositoryIdentity{Root: path, Key: k, Known: true}
	}
	for pass := range 3 {
		r := NewRecoveryResolver(projects, nil, filepath.Clean, lookup, inv)
		_, outcome := r.Recover(context.Background(), "/gone", key)
		if pass < 2 && outcome != RecoveryBudgetExhausted {
			t.Fatal(outcome)
		}
		if pass == 2 && outcome != "" {
			t.Fatal(outcome)
		}
	}
	if calls != 300 {
		t.Fatalf("restarted inventory: %d", calls)
	}
	r := NewRecoveryResolver(projects, nil, filepath.Clean, lookup, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, outcome := r.Recover(ctx, "/gone", key)
	if outcome != RecoveryBudgetExhausted || r.Operations != 0 {
		t.Fatal(outcome, r.Operations)
	}
}

func BenchmarkRecoverySharedEvidence(b *testing.B) {
	for _, roots := range []int{1, 10, 128} {
		for _, n := range []int{1000, 10000, 100000} {
			b.Run(fmt.Sprintf("roots%d/sessions%d", roots, n), func(b *testing.B) {
				projects := make([]archive.ProjectActivation, 0, roots)
				for i := range roots {
					projects = append(projects, archive.ProjectActivation{Root: fmt.Sprintf("/p%03d", i), Included: true})
				}
				key := archive.RepoKey("https://example.test/acme/repo")
				calls := 0
				b.ReportAllocs()
				for range b.N {
					r := NewRecoveryResolver(projects, nil, filepath.Clean, func(_ context.Context, path string) RepositoryIdentity {
						calls++
						k := ""
						if path == projects[len(projects)-1].Root {
							k = key
						}
						return RepositoryIdentity{Root: path, Key: k, Known: true}
					}, nil)
					for range n {
						r.Recover(context.Background(), "/gone", key)
					}
				}
				b.ReportMetric(float64(calls)/float64(b.N), "lookups/op")
			})
		}
	}
}

func TestRecoveryRevalidatesResumedInventoryAndAliasScope(t *testing.T) {
	key := archive.RepoKey("https://example.test/acme/repo")
	projects := []archive.ProjectActivation{{Root: "/a", Included: true}, {Root: "/b", Included: true}}
	inv := &RecoveryInventory{}
	calls := 0
	changed := false
	lookup := func(_ context.Context, path string) RepositoryIdentity {
		calls++
		k := key
		if changed || path == "/b" {
			k = ""
		}
		return RepositoryIdentity{Root: path, Key: k, Known: true}
	}
	first := NewRecoveryResolver(projects, nil, filepath.Clean, lookup, inv)
	first.MaxOperations = 1
	_, outcome := first.Recover(context.Background(), "/gone", key)
	if outcome != RecoveryBudgetExhausted {
		t.Fatal(outcome)
	}
	changed = true
	second := NewRecoveryResolver(projects, nil, filepath.Clean, lookup, inv)
	second.Validate = func(RepositoryIdentity) bool { return false }
	_, outcome = second.Recover(context.Background(), "/gone", key)
	if outcome != RecoveryRepositoryUnavailable || calls != 3 {
		t.Fatal(outcome, calls)
	}
	resolve := func(path string) string {
		if path == "/alias" {
			return "/a"
		}
		return filepath.Clean(path)
	}
	alias := NewRecoveryResolver([]archive.ProjectActivation{{Root: "/a", Included: true}, {Root: "/alias", Included: false}}, map[string]string{"/gone": "/a"}, resolve, lookup, nil)
	if _, outcome := alias.Recover(context.Background(), "/gone", key); outcome != RecoveryMappingConflict {
		t.Fatal(outcome)
	}
}

func TestRecoveryCancellationKeepsFairCursor(t *testing.T) {
	key := archive.RepoKey("https://example.test/acme/repo")
	projects := []archive.ProjectActivation{{Root: "/a", Included: true}, {Root: "/b", Included: true}}
	ctx, cancel := context.WithCancel(context.Background())
	inv := &RecoveryInventory{}
	calls := 0
	first := NewRecoveryResolver(projects, nil, filepath.Clean, func(_ context.Context, path string) RepositoryIdentity {
		calls++
		if path == "/b" {
			cancel()
		}
		return RepositoryIdentity{Root: path, Known: true}
	}, inv)
	if _, outcome := first.Recover(ctx, "/gone", key); outcome != RecoveryBudgetExhausted || inv.Cursor != 1 {
		t.Fatal(outcome, inv.Cursor)
	}
	second := NewRecoveryResolver(projects, nil, filepath.Clean, func(_ context.Context, path string) RepositoryIdentity {
		calls++
		return RepositoryIdentity{Root: path, Key: key, Known: true}
	}, inv)
	if proof, outcome := second.Recover(context.Background(), "/gone", key); outcome != "" || proof.Root != "/b" || calls != 3 {
		t.Fatal(proof, outcome, calls)
	}
}

func TestRecoveryCachedProofRejectsChangedRepository(t *testing.T) {
	key := archive.RepoKey("https://example.test/acme/repo")
	for _, mapped := range []bool{false, true} {
		mappings := map[string]string(nil)
		if mapped {
			mappings = map[string]string{"/gone": "/a"}
		}
		r := NewRecoveryResolver([]archive.ProjectActivation{{Root: "/a", Included: true}}, mappings, filepath.Clean, func(context.Context, string) RepositoryIdentity {
			return RepositoryIdentity{Root: "/a", Key: key, Known: true}
		}, nil)
		current := true
		r.Validate = func(RepositoryIdentity) bool { return current }
		proof, outcome := r.Recover(t.Context(), "/gone", key)
		if outcome != "" || !r.Current(proof) {
			t.Fatal(proof, outcome)
		}
		current = false
		if _, outcome := r.Recover(t.Context(), "/gone", key); outcome != RecoveryInventoryUnavailable {
			t.Fatal("stale cached proof reused", outcome)
		}
	}
}

func TestRecoveryLargeStampedInventoryMakesProgress(t *testing.T) {
	key := archive.RepoKey("https://example.test/acme/repo")
	var projects []archive.ProjectActivation
	for i := range 300 {
		projects = append(projects, archive.ProjectActivation{Root: fmt.Sprintf("/p%03d", i), Included: true})
	}
	inv := &RecoveryInventory{}
	calls := 0
	for pass := range 3 {
		r := NewRecoveryResolver(projects, nil, filepath.Clean, func(_ context.Context, path string) RepositoryIdentity {
			calls++
			k := ""
			if path == "/p299" {
				k = key
			}
			deps := make([]RepositoryDependency, 128)
			for j := range deps {
				deps[j] = RepositoryDependency{Path: fmt.Sprintf("/metadata/%d", j), Stamp: "synthetic"}
			}
			return RepositoryIdentity{Root: path, Key: k, Known: true, Dependencies: deps}
		}, inv)
		r.Validate = func(RepositoryIdentity) bool { return true }
		proof, outcome := r.Recover(t.Context(), "/gone", key)
		if pass < 2 && outcome != RecoveryBudgetExhausted {
			t.Fatal(outcome)
		}
		if pass == 2 && (outcome != "" || !r.Current(proof)) {
			t.Fatal("completed inventory cannot admit", outcome, r.MetadataOperations)
		}
	}
	if calls != 300 {
		t.Fatal("Git work restarted", calls)
	}
}

func BenchmarkRecoveryImportSlicesWithStampedEvidence(b *testing.B) {
	for _, roots := range []int{1, 10, 128} {
		for _, records := range []int{1000, 10000, 100000} {
			b.Run(fmt.Sprintf("roots%d/sessions%d", roots, records), func(b *testing.B) {
				metadata := filepath.Join(b.TempDir(), "config")
				if err := os.WriteFile(metadata, []byte("synthetic"), 0600); err != nil {
					b.Fatal(err)
				}
				info, err := os.Stat(metadata)
				if err != nil {
					b.Fatal(err)
				}
				var projects []archive.ProjectActivation
				for i := range roots {
					projects = append(projects, archive.ProjectActivation{Root: fmt.Sprintf("/p%03d", i), Included: true})
				}
				key := archive.RepoKey("https://example.test/acme/repo")
				calls, stats := 0, 0
				b.ResetTimer()
				for range b.N {
					r := NewRecoveryResolver(projects, nil, filepath.Clean, func(_ context.Context, path string) RepositoryIdentity {
						calls++
						k := ""
						if path == projects[len(projects)-1].Root {
							k = key
						}
						return RepositoryIdentity{Root: path, Key: k, Known: true, Dependencies: []RepositoryDependency{{Path: metadata, Stamp: "synthetic"}}}
					}, nil)
					r.Validate = func(RepositoryIdentity) bool {
						stats++
						current, e := os.Stat(metadata)
						return e == nil && os.SameFile(info, current) && info.ModTime().Equal(current.ModTime())
					}
					proof, outcome := r.Recover(b.Context(), "/gone", key)
					if outcome != "" {
						b.Fatal(outcome)
					}
					for i := range records {
						if i%50 == 0 {
							r.ResetValidation()
						}
						if !r.CurrentSlice(proof) {
							b.Fatal("slice failed freshness")
						}
					}
				}
				b.ReportMetric(float64(calls)/float64(b.N), "lookups/op")
				b.ReportMetric(float64(stats)/float64(b.N), "stats/op")
			})
		}
	}
}

func TestRecoveryDistinctWorkingDirectoriesDoNotStarveLaterImports(t *testing.T) {
	key := archive.RepoKey("https://example.test/acme/repo")
	projects := []archive.ProjectActivation{{Root: "/configured", Included: true}}
	for pass := range 2 {
		calls := 0
		r := NewRecoveryResolver(projects, nil, filepath.Clean, func(context.Context, string) RepositoryIdentity {
			calls++
			return RepositoryIdentity{Root: "/configured", Key: key, Known: true}
		}, nil)
		for i := range 100000 {
			cwd := fmt.Sprintf("/deleted/worktree/%d", i)
			proof, outcome := r.Recover(t.Context(), cwd, key)
			if outcome != "" || proof.Root != "/configured" || proof.OriginalCwd != cwd {
				t.Fatalf("pass %d candidate %d: %+v %s", pass, i, proof, outcome)
			}
		}
		if calls != 1 || len(r.results) > 4096 {
			t.Fatalf("unbounded inventory/cache: calls=%d entries=%d", calls, len(r.results))
		}
	}
}

func TestRecoveryProofRetainsCanonicalWorkingDirectoryFreshness(t *testing.T) {
	key := archive.RepoKey("https://example.test/acme/repo")
	canonical := "/missing/original"
	resolve := func(path string) string {
		if path == "/deleted/alias" {
			return canonical
		}
		return filepath.Clean(path)
	}
	r := NewRecoveryResolver([]archive.ProjectActivation{{Root: "/configured", Included: true}}, nil, resolve, func(context.Context, string) RepositoryIdentity {
		return RepositoryIdentity{Root: "/configured", Key: key, Known: true}
	}, nil)
	r.Validate = func(RepositoryIdentity) bool { return true }
	proof, outcome := r.Recover(t.Context(), "/deleted/alias", key)
	if outcome != "" || proof.CanonicalCwd != canonical || !r.Current(proof) || !r.CurrentSlice(proof) {
		t.Fatal(proof, outcome)
	}
	canonical = "/missing/changed"
	r.ResetValidation()
	if r.Current(proof) || r.CurrentSlice(proof) {
		t.Fatal("changed original cwd reused")
	}
}

func TestSemanticRecoveryRechecksWholeInventoryOncePerSlice(t *testing.T) {
	key := archive.RepoKey("https://example.test/acme/repo")
	projects := []archive.ProjectActivation{{Root: "/synthetic/first", Included: true}, {Root: "/synthetic/second", Included: false}}
	calls := 0
	changed := false
	lookup := func(_ context.Context, root string) RepositoryIdentity {
		calls++
		k := key
		if root == "/synthetic/second" {
			k = archive.RepoKey("https://example.test/acme/other")
			if changed {
				k = key
			}
		}
		return RepositoryIdentity{Known: true, Root: root, Key: k, Validation: "semantic", ObservedRoot: root}
	}
	r := NewRecoveryResolver(projects, nil, filepath.Clean, lookup, nil)
	r.Validate = func(RepositoryIdentity) bool { return true }
	proof, outcome := r.Recover(t.Context(), "/synthetic/gone", key)
	if outcome != "" || proof.ValidationMethod != "semantic" {
		t.Fatal(proof, outcome)
	}
	r.ResetValidation()
	for range 100 {
		if !r.CurrentSlice(proof) {
			t.Fatal("stable inventory rejected")
		}
	}
	if calls != 4 {
		t.Fatalf("100 sessions require two inventory sweeps, got %d root observations", calls)
	}
	changed = true
	r.ResetValidation()
	if r.CurrentSlice(proof) {
		t.Fatal("new excluded clone did not invalidate unique match")
	}
	for range 100 {
		if r.CurrentSlice(proof) {
			t.Fatal("changed epoch was admitted")
		}
	}
	if calls != 6 {
		t.Fatal("failed sweep was repeated per candidate", calls)
	}
	changed = false
	r.ResetValidation()
	if !r.CurrentSlice(proof) || calls != 8 {
		t.Fatal("new slice did not retry failed inventory", calls)
	}
}

func TestSemanticRecoveryCancellationNeverAdmits(t *testing.T) {
	key := archive.RepoKey("https://example.test/acme/repo")
	ctx, cancel := context.WithCancel(t.Context())
	r := NewRecoveryResolver([]archive.ProjectActivation{{Root: "/synthetic/root", Included: true}}, nil, filepath.Clean, func(context.Context, string) RepositoryIdentity {
		return RepositoryIdentity{Known: true, Root: "/synthetic/root", Key: key, Validation: "semantic", ObservedRoot: "/synthetic/root"}
	}, nil)
	proof, outcome := r.Recover(ctx, "/synthetic/gone", key)
	if outcome != "" {
		t.Fatal(outcome)
	}
	cancel()
	r.ResetValidation()
	if r.CurrentSlice(proof) || !r.MetadataExhausted {
		t.Fatal("cancelled epoch admitted")
	}
}

func TestSemanticAdmissionRenewsCancelledPlanningContext(t *testing.T) {
	key := archive.RepoKey("https://example.test/acme/repo")
	planning, cancel := context.WithCancel(t.Context())
	lookups := 0
	r := NewRecoveryResolver([]archive.ProjectActivation{{Root: "/synthetic/root", Included: true}}, nil, filepath.Clean, func(ctx context.Context, _ string) RepositoryIdentity {
		if ctx.Err() != nil {
			t.Fatal("renewed admission used cancelled planning context")
		}
		lookups++
		return RepositoryIdentity{Known: true, Root: "/synthetic/root", Key: key, Validation: "semantic", ObservedRoot: "/synthetic/root"}
	}, nil)
	proof, outcome := r.Recover(planning, "/synthetic/gone", key)
	if outcome != "" {
		t.Fatal(outcome)
	}
	cancel()
	r.ResetValidationContext(t.Context())
	if !r.CurrentSlice(proof) || lookups != 2 {
		t.Fatal("admission did not renew full sweep", lookups)
	}
}

func TestRepositoryObservationBudgetKeepsInventoryProgress(t *testing.T) {
	key := archive.RepoKey("https://example.test/acme/repo")
	projects := []archive.ProjectActivation{{Root: "/synthetic/a", Included: true}, {Root: "/synthetic/b", Included: true}}
	calls := 0
	r := NewRecoveryResolver(projects, nil, filepath.Clean, func(_ context.Context, root string) RepositoryIdentity {
		calls++
		if root == "/synthetic/a" {
			return RepositoryIdentity{BudgetExhausted: true}
		}
		return RepositoryIdentity{Known: true, Root: root, Key: key}
	}, nil)
	_, outcome := r.Recover(t.Context(), "/synthetic/gone", key)
	if outcome != RecoveryBudgetExhausted || calls != 2 || r.Inventory.Cursor != 2 {
		t.Fatal(outcome, calls, r.Inventory.Cursor)
	}
}

func TestSemanticCachedSliceHonorsCancellation(t *testing.T) {
	key := archive.RepoKey("https://example.test/acme/repo")
	ctx, cancel := context.WithCancel(t.Context())
	r := NewRecoveryResolver([]archive.ProjectActivation{{Root: "/synthetic/root", Included: true}}, nil, filepath.Clean, func(context.Context, string) RepositoryIdentity {
		return RepositoryIdentity{Known: true, Root: "/synthetic/root", Key: key, Validation: "semantic", ObservedRoot: "/synthetic/root"}
	}, nil)
	proof, outcome := r.Recover(ctx, "/synthetic/gone", key)
	if outcome != "" || !r.CurrentSlice(proof) {
		t.Fatal(outcome)
	}
	cancel()
	if r.CurrentSlice(proof) || !r.MetadataExhausted {
		t.Fatal("cached slice ignored cancellation")
	}
}

func TestMappedRecoveryFreezesPlanningIdentityPerRoot(t *testing.T) {
	key := archive.RepoKey("https://example.test/acme/original")
	changedKey := archive.RepoKey("https://example.test/acme/changed")
	currentKey := key
	calls := 0
	r := NewRecoveryResolver([]archive.ProjectActivation{{Root: "/synthetic/root", Included: true}}, map[string]string{"/gone/first": "/synthetic/root", "/gone/second": "/synthetic/root"}, filepath.Clean, func(context.Context, string) RepositoryIdentity {
		calls++
		return RepositoryIdentity{Known: true, Root: "/synthetic/root", Key: currentKey, Validation: "semantic", ObservedRoot: "/synthetic/root"}
	}, nil)
	proof, outcome := r.Recover(t.Context(), "/gone/first", key)
	if outcome != "" {
		t.Fatal(outcome)
	}
	currentKey = changedKey
	_, outcome = r.Recover(t.Context(), "/gone/second", changedKey)
	if outcome != RecoveryMappingConflict || calls != 1 {
		t.Fatalf("later mapping replaced the planning epoch: outcome=%s calls=%d", outcome, calls)
	}
	r.ResetValidationContext(t.Context())
	if r.CurrentSlice(proof) {
		t.Fatal("original mapping admitted after destination identity changed")
	}
}

func TestSemanticRecoveryRejectsChangedObserverScope(t *testing.T) {
	key := archive.RepoKey("https://example.test/acme/repo")
	scope := "planned-observer"
	r := NewRecoveryResolver([]archive.ProjectActivation{{Root: "/synthetic/root", Included: true}}, nil, filepath.Clean, func(context.Context, string) RepositoryIdentity {
		return RepositoryIdentity{Known: true, Root: "/synthetic/root", Key: key, Validation: "semantic", ObservedRoot: "/synthetic/root", ObservationScope: scope}
	}, nil)
	proof, outcome := r.Recover(t.Context(), "/synthetic/gone", key)
	if outcome != "" {
		t.Fatal(outcome)
	}
	scope = "replacement-observer"
	r.ResetValidationContext(t.Context())
	if r.CurrentSlice(proof) || r.MetadataExhausted {
		t.Fatal("changed observer must invalidate the epoch, not exhaust its budget")
	}
}

func TestRecoveryKeylessProposedRootIsNonOwning(t *testing.T) {
	key := archive.RepoKey("https://example.test/acme/repo")
	projects := []archive.ProjectActivation{{Root: "/a", Included: true}, {Root: "/proposed", Included: true}}
	for _, tc := range []struct {
		name     string
		proposed bool
		id       RepositoryIdentity
		want     RecoveryOutcome
	}{
		{"proposed keyless checkout", true, RepositoryIdentity{Root: "/proposed"}, ""},
		{"configured keyless checkout", false, RepositoryIdentity{Root: "/proposed"}, RecoveryInventoryUnavailable},
		{"proposed unobserved", true, RepositoryIdentity{}, RecoveryInventoryUnavailable},
		{"proposed budget", true, RepositoryIdentity{Root: "/proposed", BudgetExhausted: true}, RecoveryBudgetExhausted},
		{"proposed clone", true, RepositoryIdentity{Root: "/proposed", Key: key, Known: true}, RecoveryAmbiguous},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fresh := tc.id
			r := NewRecoveryResolver(projects, nil, filepath.Clean, func(_ context.Context, root string) RepositoryIdentity {
				if root == "/a" {
					return RepositoryIdentity{Root: "/a", Key: key, Known: true}
				}
				return fresh
			}, nil)
			if tc.proposed {
				r.Proposed = map[string]bool{"/proposed": true}
			}
			proof, outcome := r.Recover(t.Context(), "/gone", key)
			if outcome != tc.want || (outcome == "" && proof.Root != "/a") {
				t.Fatalf("%s: %+v", outcome, proof)
			}
			if outcome != "" {
				return
			}
			r.ResetValidationContext(t.Context())
			if !r.CurrentSlice(proof) {
				t.Fatal("unchanged keyless proposed root rejected")
			}
			for name, changed := range map[string]RepositoryIdentity{
				"gained the recorded key": {Root: "/proposed", Key: key, Known: true},
				"moved its top level":     {Root: "/elsewhere"},
				"changed observer scope":  {Root: "/proposed", ObservationScope: "other"},
				"became unobservable":     {},
				"exhausted its budget":    {Root: "/proposed", BudgetExhausted: true},
			} {
				fresh = changed
				r.ResetValidationContext(t.Context())
				if r.CurrentSlice(proof) {
					t.Fatalf("proposed root that %s kept the unique match", name)
				}
			}
		})
	}
}
