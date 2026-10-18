package discovery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

// An index can retain the spelling of an approved native home through a symlink.
// The alias supplies only the home prefix; sources still use confined opens.
func TestRolloutLookupUsesTrustedHomeAlias(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	id := writeRollout(t, root, cfg.Archive.Projects[0].Root, at, 1, "sessions")
	name := "rollout-2026-10-01T12-00-00-" + id + ".jsonl"
	path := filepath.Join(root, "sessions", name)
	alias := filepath.Join(t.TempDir(), "native-home")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	db := hintDatabase(t, root, true)
	addHint(t, db, id, filepath.Join(alias, "sessions", name), at)
	lookup, err := NewCodexRolloutLookup(t.Context(), store, []string{alias}, ForegroundLookup)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lookup.CloseReadOnly(); err != nil {
			t.Error(err)
		}
	})
	set, err := lookup.Thread(t.Context(), id)
	if err != nil || set.Current == nil || set.Current.Path != path {
		t.Fatalf("trusted home alias did not select the confined physical locator: %+v: %v (%v)", set, err, errors.Unwrap(err))
	}
	for range 8 {
		if err := lookup.Check(t.Context(), id, set.Revision); err != nil {
			t.Fatal(err)
		}
	}
	if lookup.probes != 1 || lookup.queries != 18 {
		t.Fatalf("unchanged selection repeated native work: probes=%d queries=%d", lookup.probes, lookup.queries)
	}
	if err := lookup.CloseReadOnly(); err != nil {
		t.Fatal(err)
	}
	if used, _ := lookup.NativeReadBudget().Charged(); used != 0 {
		t.Fatal("lookup leaked shared native charge", used)
	}
	if _, err := lookup.Thread(t.Context(), id); !errors.Is(err, agentapi.ErrClosed) {
		t.Fatal("closed lookup was reused", err)
	}
}

func TestRolloutLookupHomeAliasDoesNotAuthorizeOtherLocators(t *testing.T) {
	t.Parallel()
	for _, policy := range []CodexLookupPolicy{BackgroundLookup, ForegroundLookup} {
		for _, mode := range []string{"outside", "wrong_home", "unapproved_alias", "traversal", "descendant_escape", "file_symlink", "wrong_thread"} {
			t.Run(fmt.Sprintf("%d/%s", policy, mode), func(t *testing.T) {
				t.Parallel()
				store, cfg, at, root := fixture(t)
				id := writeRollout(t, root, cfg.Archive.Projects[0].Root, at, 1, "sessions")
				name := "rollout-2026-10-01T12-00-00-" + id + ".jsonl"
				alias := filepath.Join(t.TempDir(), "native-home")
				if err := os.Symlink(root, alias); err != nil {
					t.Fatal(err)
				}
				other := t.TempDir()
				writeRollout(t, other, cfg.Archive.Projects[0].Root, at, 1, "sessions")
				path := filepath.Join(alias, "sessions", name)
				homes := []string{alias}
				switch mode {
				case "outside":
					path = filepath.Join(other, "sessions", name)
				case "wrong_home":
					homes = append(homes, other)
					path = filepath.Join(other, "sessions", name)
				case "unapproved_alias":
					unapproved := filepath.Join(t.TempDir(), "other-spelling")
					if err := os.Symlink(root, unapproved); err != nil {
						t.Fatal(err)
					}
					path = filepath.Join(unapproved, "sessions", name)
				case "traversal":
					path = alias + "/sessions/../sessions/" + name
				case "descendant_escape":
					if err := os.Symlink(filepath.Join(other, "sessions"), filepath.Join(root, "sessions", "escape")); err != nil {
						t.Fatal(err)
					}
					path = filepath.Join(alias, "sessions", "escape", name)
				case "file_symlink":
					if err := os.Symlink(filepath.Join(other, "sessions", name), filepath.Join(root, "sessions", "link.jsonl")); err != nil {
						t.Fatal(err)
					}
					path = filepath.Join(alias, "sessions", "link.jsonl")
				case "wrong_thread":
					otherID := writeRollout(t, root, cfg.Archive.Projects[0].Root, at, 2, "sessions")
					path = filepath.Join(alias, "sessions", "rollout-2026-10-01T12-00-00-"+otherID+".jsonl")
				}
				db := hintDatabase(t, root, true)
				addHint(t, db, id, path, at)
				lookup, err := NewCodexRolloutLookup(t.Context(), store, homes, policy)
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := lookup.CloseReadOnly(); err != nil {
						t.Error(err)
					}
				}()
				if set, err := lookup.Thread(t.Context(), id); err == nil || set.Current != nil {
					t.Fatalf("untrusted locator became a selection: %+v %v", set, err)
				}
			})
		}
	}
}

func TestRolloutLookupHomeAliasChangesInvalidateCachedSelection(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"missing_alias", "retarget_alias", "replaced_root", "secondary_alias"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			store, cfg, at, root := fixture(t)
			id := writeRollout(t, root, cfg.Archive.Projects[0].Root, at, 1, "sessions")
			alias := filepath.Join(t.TempDir(), "native-home")
			if err := os.Symlink(root, alias); err != nil {
				t.Fatal(err)
			}
			homes := []string{root, alias}
			changed := alias
			if mode == "secondary_alias" {
				changed = filepath.Join(t.TempDir(), "second-home-spelling")
				if err := os.Symlink(root, changed); err != nil {
					t.Fatal(err)
				}
				homes = append(homes, changed)
			}
			db := hintDatabase(t, root, true)
			addHint(t, db, id, filepath.Join(alias, "sessions", "rollout-2026-10-01T12-00-00-"+id+".jsonl"), at)
			lookup, err := NewCodexRolloutLookup(t.Context(), store, homes, ForegroundLookup)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := lookup.CloseReadOnly(); err != nil {
					t.Error(err)
				}
			}()
			set, err := lookup.Thread(t.Context(), id)
			if err != nil || set.Current == nil {
				t.Fatal("initial alias selection failed", set, err)
			}
			if mode == "replaced_root" {
				if err := os.Rename(root, root+"-previous"); err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := os.RemoveAll(root + "-previous"); err != nil {
						t.Error(err)
					}
				}()
				if err := os.Mkdir(root, 0700); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Remove(changed); err != nil {
					t.Fatal(err)
				}
				if mode != "missing_alias" {
					if err := os.Symlink(t.TempDir(), changed); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := lookup.Thread(t.Context(), id); agentapi.Failure(err) != agentapi.Changed {
				t.Fatal("cached thread ignored changed home", err)
			}
			if err := lookup.Check(t.Context(), id, set.Revision); agentapi.Failure(err) != agentapi.Changed {
				t.Fatal("selection check ignored changed home", err)
			}
			if _, err := lookup.Rollout(t.Context(), id); agentapi.Failure(err) != agentapi.Changed {
				t.Fatal("rollout lookup ignored changed home", err)
			}
		})
	}
}

func TestRolloutLookupHomeAliasCannotSwitchToAnotherApprovedHome(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	other := t.TempDir()
	id := writeRollout(t, root, cfg.Archive.Projects[0].Root, at, 1, "sessions")
	writeRollout(t, other, cfg.Archive.Projects[0].Root, at, 1, "sessions")
	alias := filepath.Join(t.TempDir(), "native-home")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	db := hintDatabase(t, root, true)
	addHint(t, db, id, filepath.Join(alias, "sessions", "rollout-2026-10-01T12-00-00-"+id+".jsonl"), at)
	lookup, err := NewCodexRolloutLookup(t.Context(), store, []string{alias, other}, ForegroundLookup)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lookup.CloseReadOnly(); err != nil {
			t.Error(err)
		}
	}()
	// Both physical homes were supplied, but that does not permit this spelling
	// to switch its identity before the lookup's first native acquisition.
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := lookup.Thread(t.Context(), id); agentapi.Failure(err) != agentapi.Changed {
		t.Fatal("alias switched approved owners", err)
	}
	if lookup.queries != 0 || lookup.probes != 0 {
		t.Fatal("changed home acquired native evidence", lookup.queries, lookup.probes)
	}
}

func TestRolloutLookupHomeAliasFencesConstructorApproval(t *testing.T) {
	t.Parallel()
	store, _, at, root := fixture(t)
	other := t.TempDir()
	alias := filepath.Join(t.TempDir(), "native-home")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	mutated := false
	lookup, err := NewCodexRolloutLookupWithOptions(t.Context(), store, []string{alias, other}, CodexLookupOptions{Policy: ForegroundLookup, Now: func() time.Time {
		if !mutated {
			mutated = true
			if err := os.Remove(alias); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(other, alias); err != nil {
				t.Fatal(err)
			}
		}
		return at
	}})
	if lookup != nil {
		if err := lookup.CloseReadOnly(); err != nil {
			t.Error(err)
		}
	}
	if !mutated || lookup != nil || agentapi.Failure(err) != agentapi.Changed {
		t.Fatal("constructor renewed a spelling onto another approved home", mutated, lookup != nil, err)
	}
}

func TestRolloutLookupHomeAliasKeepsSelectionAndResourceChecks(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"index_move", "source_move", "cancel", "pressure"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			store, cfg, at, root := fixture(t)
			id := writeRollout(t, root, cfg.Archive.Projects[0].Root, at, 1, "sessions")
			name := "rollout-2026-10-01T12-00-00-" + id + ".jsonl"
			alias := filepath.Join(t.TempDir(), "native-home")
			if err := os.Symlink(root, alias); err != nil {
				t.Fatal(err)
			}
			db := hintDatabase(t, root, true)
			addHint(t, db, id, filepath.Join(alias, "sessions", name), at)
			lookup, err := NewCodexRolloutLookup(t.Context(), store, []string{alias}, ForegroundLookup)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := lookup.CloseReadOnly(); err != nil {
					t.Error(err)
				}
			}()
			set, err := lookup.Thread(t.Context(), id)
			if err != nil || set.Current == nil {
				t.Fatal("initial alias selection failed", set, err)
			}
			ctx := t.Context()
			var release func()
			switch mode {
			case "index_move":
				other := writeRollout(t, root, cfg.Archive.Projects[0].Root, at, 2, "sessions")
				path := filepath.Join(root, "sessions", "rollout-2026-10-01T12-00-00-"+other+".jsonl")
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(strings.ReplaceAll(string(raw), other, id)), 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(ctx, "UPDATE threads SET rollout_path=? WHERE id=?", filepath.Join(alias, "sessions", filepath.Base(path)), id); err != nil {
					t.Fatal(err)
				}
			case "source_move":
				if err := os.Rename(filepath.Join(root, "sessions", name), filepath.Join(root, "sessions", "moved.jsonl")); err != nil {
					t.Fatal(err)
				}
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "pressure":
				pressure := lookup.readBudget.Available()
				if !lookup.readBudget.ReserveUnscoped(pressure) {
					t.Fatal("pressure reservation failed")
				}
				release = func() { lookup.readBudget.ReleaseUnscoped(pressure) }
			}
			err = lookup.Check(ctx, id, set.Revision)
			if release != nil {
				release()
			}
			if mode == "cancel" && !errors.Is(err, context.Canceled) || mode == "pressure" && agentapi.Failure(err) != agentapi.Limit || (mode == "index_move" || mode == "source_move") && agentapi.Failure(err) != agentapi.Changed {
				t.Fatal("alias bypassed existing selection/resource refusal", err)
			}
			if err := lookup.CloseReadOnly(); err != nil {
				t.Fatal(err)
			}
			if used, _ := lookup.readBudget.Charged(); used != 0 {
				t.Fatal("alias selection leaked charge", used)
			}
		})
	}
}

func TestRolloutLookupHomeAliasKeepsSchedulingPolicy(t *testing.T) {
	t.Parallel()
	for _, foreground := range []bool{false, true} {
		t.Run(fmt.Sprint(foreground), func(t *testing.T) {
			t.Parallel()
			store, cfg, at, root := fixture(t)
			id := writeRollout(t, root, cfg.Archive.Projects[0].Root, at, 1, "sessions")
			alias := filepath.Join(t.TempDir(), "native-home")
			if err := os.Symlink(root, alias); err != nil {
				t.Fatal(err)
			}
			db := hintDatabase(t, root, true)
			addHint(t, db, id, filepath.Join(alias, "sessions", "rollout-2026-10-01T12-00-00-"+id+".jsonl"), at)
			var lookup *CodexRolloutLookup
			var err error
			if foreground {
				now := at
				lookup, err = NewCodexRolloutLookupWithOptions(t.Context(), store, []string{alias}, CodexLookupOptions{Policy: ForegroundLookup, Now: func() time.Time { now = now.Add(2 * time.Second); return now }})
			} else {
				lookup, err = NewCodexRolloutLookup(t.Context(), store, []string{alias})
			}
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := lookup.CloseReadOnly(); err != nil {
					t.Error(err)
				}
			}()
			const external = int64(17)
			if !lookup.readBudget.ReserveUnscoped(external) {
				t.Fatal("external charge failed")
			}
			lookup.remaining = 0
			set, err := lookup.Thread(t.Context(), id)
			if foreground {
				if err != nil || set.Current == nil || lookup.work.scopes < 2 {
					t.Fatal("alias work did not retain foreground renewal", set, err)
				}
				if err := lookup.Check(t.Context(), id, set.Revision); err != nil {
					t.Fatal(err)
				}
			} else {
				var source *agentapi.SourceError
				if !lookup.AllowanceSpent() || !errors.As(err, &source) || source.Cause != agentapi.CauseLookupAllowanceSpent {
					t.Fatal("alias work renewed the default background allowance", err)
				}
			}
			if err := lookup.CloseReadOnly(); err != nil {
				t.Fatal(err)
			}
			if used, _ := lookup.readBudget.Charged(); used != external {
				t.Fatal("alias cleanup lost external charge", used)
			}
			lookup.readBudget.ReleaseUnscoped(external)
		})
	}
}
