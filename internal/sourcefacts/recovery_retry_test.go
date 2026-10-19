package sourcefacts

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

func roundTripRecoveryInventory(t *testing.T, inv *RecoveryInventory) *RecoveryInventory {
	t.Helper()
	path := filepath.Join(t.TempDir(), "inventory.json")
	if err := local.WriteCompact(path, inv); err != nil {
		t.Fatal(err)
	}
	var loaded RecoveryInventory
	if err := local.Read(path, &loaded); err != nil {
		t.Fatal(err)
	}
	return &loaded
}

func TestRecoveryRenewsPersistedIncompleteInventory(t *testing.T) {
	key := archive.RepoKey("https://example.test/acme/repo")
	other := archive.RepoKey("https://example.test/acme/other")
	for _, failure := range []RecoveryOutcome{RecoveryOriginCycle, RecoveryOriginMissing, RecoveryOriginUnreadable, RecoveryOriginConflict, RecoveryOriginUnsupported, RecoveryOriginUnresolved, RecoveryOriginLimit, RecoveryOriginChanged} {
		t.Run(string(failure), func(t *testing.T) {
			projects := []archive.ProjectActivation{{Root: "/a", Included: false}, {Root: "/b", Included: true}, {Root: "/c", Included: true}}
			inv := &RecoveryInventory{}
			restored, calls := false, 0
			lookup := func(_ context.Context, root string) RepositoryIdentity {
				calls++
				if root == "/a" && !restored {
					return RepositoryIdentity{Root: root, Failure: failure}
				}
				k := other
				if root == "/c" {
					k = key
				}
				return RepositoryIdentity{Root: root, Key: k, Known: true}
			}
			first := NewRecoveryResolver(projects, nil, filepath.Clean, lookup, inv)
			first.MaxOperations = 1
			if _, outcome := first.Recover(t.Context(), "/gone", key); outcome != RecoveryBudgetExhausted || inv.Cursor != 1 {
				t.Fatal("expected a partial inventory", outcome, inv.Cursor)
			}
			loaded := roundTripRecoveryInventory(t, inv)
			restored = true
			resumed := NewRecoveryResolver(projects, nil, filepath.Clean, lookup, loaded)
			resumed.Validate = func(id RepositoryIdentity) bool { return id.Known }
			proof, outcome := resumed.Recover(t.Context(), "/gone", key)
			if outcome != "" || proof.Root != "/c" || calls != 4 || !resumed.Current(proof) {
				t.Fatalf("repaired partial inventory did not resume: proof=%+v outcome=%s calls=%d", proof, outcome, calls)
			}
			if _, outcome := resumed.Recover(t.Context(), "/second-gone", key); outcome != "" || calls != 4 {
				t.Fatal("same-slice recovery repeated repository observations", outcome, calls)
			}
		})
	}
}

func TestRecoveryIncompleteIdentityRenewsOncePerValidationSlice(t *testing.T) {
	key := archive.RepoKey("https://example.test/acme/repo")
	projects := []archive.ProjectActivation{{Root: "/a", Included: true}, {Root: "/b", Included: true}}
	for _, competing := range []bool{false, true} {
		t.Run(map[bool]string{false: "unrelated", true: "competing"}[competing], func(t *testing.T) {
			restored, calls := false, 0
			r := NewRecoveryResolver(projects, map[string]string{"/mapped": "/b"}, filepath.Clean, func(_ context.Context, root string) RepositoryIdentity {
				calls++
				if root == "/a" && !restored {
					return RepositoryIdentity{Root: root, Failure: RecoveryOriginMissing}
				}
				k := key
				if root == "/a" && !competing {
					k = archive.RepoKey("https://example.test/acme/other")
				}
				return RepositoryIdentity{Root: root, Key: k, Known: true}
			}, nil)
			r.Validate = func(id RepositoryIdentity) bool { return id.Known }
			for i := range 100 {
				if _, outcome := r.Recover(t.Context(), fmt.Sprintf("/gone/%d", i), key); outcome != RecoveryOriginMissing || calls != 2 {
					t.Fatal("incomplete evidence was retried per candidate", outcome, calls)
				}
			}
			if proof, outcome := r.Recover(t.Context(), "/mapped", key); outcome != "" || proof.Method != "explicit_mapping" || calls != 3 {
				t.Fatal("unrelated incomplete inventory blocked reviewed mapping", proof, outcome, calls)
			}
			restored = true
			r.ResetValidationContext(t.Context())
			_, outcome := r.Recover(t.Context(), "/restored", key)
			want := RecoveryOutcome("")
			if competing {
				want = RecoveryAmbiguous
			}
			if outcome != want || calls != 4 {
				t.Fatal("new validation slice did not renew incomplete evidence", outcome, calls)
			}
		})
	}
}

func TestRecoveryIncompletePrefixDoesNotStarveInventory(t *testing.T) {
	key := archive.RepoKey("https://example.test/acme/repo")
	projects := make([]archive.ProjectActivation, 300)
	for i := range projects {
		projects[i] = archive.ProjectActivation{Root: fmt.Sprintf("/p%03d", i), Included: true}
	}
	inv, calls := &RecoveryInventory{}, 0
	for pass := range 3 {
		r := NewRecoveryResolver(projects, nil, filepath.Clean, func(_ context.Context, root string) RepositoryIdentity {
			calls++
			return RepositoryIdentity{Root: root, Failure: RecoveryOriginMissing}
		}, inv)
		r.Validate = func(id RepositoryIdentity) bool { return id.Known }
		if _, outcome := r.Recover(t.Context(), "/gone", key); outcome != RecoveryBudgetExhausted || r.Operations > r.MaxOperations {
			t.Fatal("unbounded incomplete observation", outcome, r.Operations)
		}
		if inv.Cursor != min((pass+1)*128, len(projects)) {
			t.Fatal("incomplete prefix starved later roots", pass, inv.Cursor)
		}
		before := calls
		for range 100 {
			if _, outcome := r.Recover(t.Context(), "/other-gone", key); outcome != RecoveryBudgetExhausted || calls != before {
				t.Fatal("spent slice repeated incomplete observations", outcome, calls-before)
			}
		}
	}
	if calls != 384 || len(inv.Entries) != len(projects) {
		t.Fatal("unexpected bounded sweep work", calls, len(inv.Entries))
	}
}

func TestRecoveryCompletedInventoryStartsFresh(t *testing.T) {
	key := archive.RepoKey("https://example.test/acme/repo")
	projects := []archive.ProjectActivation{{Root: "/a", Included: true}}
	inv := &RecoveryInventory{Context: RecoveryContext(projects, nil, filepath.Clean), Cursor: 1, Entries: []RepositoryIdentity{{Root: "/a", Failure: RecoveryOriginMissing}}}
	calls := 0
	r := NewRecoveryResolver(projects, nil, filepath.Clean, func(_ context.Context, root string) RepositoryIdentity {
		calls++
		return RepositoryIdentity{Root: root, Key: key, Known: true}
	}, inv)
	if inv.Cursor != 0 {
		t.Fatal("completed inventory was not reset")
	}
	if _, outcome := r.Recover(t.Context(), "/gone", key); outcome != "" || calls != 1 {
		t.Fatal("completed inventory did not reobserve", outcome, calls)
	}
}

func TestRecoveryIncompletePrefixAtExactOperationWindows(t *testing.T) {
	key := archive.RepoKey("https://example.test/acme/repo")
	for _, roots := range []int{256, 384} {
		t.Run(strconv.Itoa(roots), func(t *testing.T) {
			projects := make([]archive.ProjectActivation, roots)
			for i := range projects {
				projects[i] = archive.ProjectActivation{Root: fmt.Sprintf("/p%03d", i), Included: true}
			}
			inv, calls := &RecoveryInventory{}, 0
			passes := 2 * roots / 128
			for pass := range passes {
				r := NewRecoveryResolver(projects, nil, filepath.Clean, func(_ context.Context, root string) RepositoryIdentity {
					calls++
					if root == projects[0].Root && pass == 0 {
						return RepositoryIdentity{Root: root, Failure: RecoveryOriginMissing}
					}
					k := ""
					if root == projects[len(projects)-1].Root {
						k = key
					}
					return RepositoryIdentity{Root: root, Key: k, Known: true}
				}, inv)
				r.Validate = func(id RepositoryIdentity) bool { return id.Known }
				proof, outcome := r.Recover(t.Context(), "/gone", key)
				if r.Operations != 128 {
					t.Fatal("exact operation window was not bounded", pass, r.Operations)
				}
				if pass == passes-1 {
					if outcome != "" || proof.Root != projects[len(projects)-1].Root || !r.Current(proof) {
						t.Fatal("boundary inventory did not eventually recover", proof, outcome)
					}
				} else if outcome != RecoveryBudgetExhausted && outcome != RecoveryOriginMissing {
					t.Fatal("incomplete boundary inventory granted authority", pass, outcome)
				}
				inv = roundTripRecoveryInventory(t, inv)
			}
			if calls != 2*roots {
				t.Fatal("boundary recovery repeated unbounded work", calls)
			}
		})
	}
}

func TestRecoveryDoesNotImmediatelyRetryFreshIncompleteTail(t *testing.T) {
	key := archive.RepoKey("https://example.test/acme/repo")
	projects := []archive.ProjectActivation{{Root: "/a", Included: true}, {Root: "/b", Included: true}}
	calls := 0
	r := NewRecoveryResolver(projects, nil, filepath.Clean, func(_ context.Context, root string) RepositoryIdentity {
		calls++
		if root == "/b" && calls == 2 {
			return RepositoryIdentity{Root: root, Failure: RecoveryOriginChanged}
		}
		k := ""
		if root == "/a" {
			k = key
		}
		return RepositoryIdentity{Root: root, Key: k, Known: true}
	}, nil)
	for range 100 {
		if _, outcome := r.Recover(t.Context(), "/gone", key); outcome != RecoveryOriginChanged || calls != 2 {
			t.Fatal("fresh tail failure retried inside its observation slice", outcome, calls)
		}
	}
	r.ResetValidationContext(t.Context())
	if _, outcome := r.Recover(t.Context(), "/gone", key); outcome != "" || calls != 3 {
		t.Fatal("next validation slice did not renew tail failure", outcome, calls)
	}
}
