package discovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/muse"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

const inventoryOwner = "00000000-0000-7000-8000-000000000001"

func inventoryFixture(t *testing.T) (string, archive.NativeRoot, []byte) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	raw, err := os.ReadFile(filepath.Join("..", "agents", "muse", "testdata", "synthetic-exec-empty.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return home, archive.NativeRoot{Agent: "muse", Configured: root, Canonical: root, Revision: "one"}, raw
}

func inventoryFile(t *testing.T, root archive.NativeRoot, day, id string, raw []byte) string {
	t.Helper()
	path := filepath.Join(root.Canonical, day, id, "session.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func inventoryOpen(t *testing.T, home string, root archive.NativeRoot, ids ...string) *NativeInventory {
	t.Helper()
	queries := []agentapi.LocatorRequest{}
	for _, id := range ids {
		queries = append(queries, agentapi.LocatorRequest{NativeID: id, Root: root})
	}
	l, err := NewNativeInventory(t.Context(), home, []archive.NativeRoot{root}, queries, agentapi.NewNativeReadBudget(agentapi.PassReadBudget), muse.ValidateNativeLocator)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := l.Close(); err != nil {
			t.Error(err)
		}
		used, _ := l.budget.Charged()
		if used != 0 {
			t.Errorf("inventory leak %d", used)
		}
	})
	return l
}

func inventoryLocate(t *testing.T, l *NativeInventory, id string) agentapi.LocatorResult {
	t.Helper()
	r, e := l.Locate(t.Context(), agentapi.LocatorRequest{NativeID: id, Root: l.root})
	if e != nil {
		t.Fatal(e)
	}
	return r
}

func TestNativeInventoryUniqueCurrentnessAndHistoricalPages(t *testing.T) {
	t.Parallel()
	home, root, raw := inventoryFixture(t)
	path := inventoryFile(t, root, "2026/10/15", inventoryOwner, raw)
	l := inventoryOpen(t, home, root, inventoryOwner)
	r := inventoryLocate(t, l, inventoryOwner)
	if r.Status != agentapi.LocatorFound || len(r.Candidates) != 1 || r.Candidates[0].Path != path {
		t.Fatal(r, l.Counts())
	}
	if err := l.Check(t.Context(), inventoryOwner, "forged"); err == nil {
		t.Fatal("caller token authorized")
	}
	if err := l.Check(t.Context(), inventoryOwner, r.Revision); err != nil {
		t.Fatal(err)
	}
	if l.Counts().Names != 4 || l.Counts().Probes != 2 || l.Counts().HeaderBytes == 0 {
		t.Fatal(l.Counts())
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	restored := inventoryOpen(t, home, root, inventoryOwner)
	restoredResult := inventoryLocate(t, restored, inventoryOwner)
	if restoredResult.Status != agentapi.LocatorFound || restored.Counts().Names != 0 {
		t.Fatal(restoredResult, restored.Counts())
	}
	if err := restored.Check(t.Context(), inventoryOwner, r.Revision); err == nil {
		t.Fatal("previous pass token authorized")
	}
	// Deep duplicate with restored day mtime leaves ancestors unchanged. Ctime
	// plus all-directory currentness invalidates prior pages and live tokens.
	day := filepath.Join(root.Canonical, "2026", "10", "15")
	info, err := os.Stat(day)
	if err != nil {
		t.Fatal(err)
	}
	secondDay := filepath.Join(root.Canonical, "2026", "10", "14")
	if err = os.MkdirAll(secondDay, 0700); err != nil {
		t.Fatal(err)
	}
	// First invalidate the new day edge, then acquire a fresh complete epoch.
	if err := restored.Check(t.Context(), inventoryOwner, restoredResult.Revision); err == nil {
		t.Fatal("stale namespace accepted")
	}
	_ = info
}

func TestNativeInventoryDuplicateUnderCompletedDayAndEmptyParent(t *testing.T) {
	t.Parallel()
	for _, empty := range []bool{false, true} {
		t.Run(strconv.FormatBool(empty), func(t *testing.T) {
			t.Parallel()
			home, root, raw := inventoryFixture(t)
			inventoryFile(t, root, "2026/10/15", inventoryOwner, raw)
			other := filepath.Join(root.Canonical, "2026", "10", "14")
			if empty {
				other = filepath.Join(other, inventoryOwner)
			}
			if err := os.MkdirAll(other, 0700); err != nil {
				t.Fatal(err)
			}
			l := inventoryOpen(t, home, root, inventoryOwner)
			r := inventoryLocate(t, l, inventoryOwner)
			if r.Status != agentapi.LocatorFound {
				t.Fatal(r)
			}
			before, err := os.Stat(other)
			if err != nil {
				t.Fatal(err)
			}
			inventoryFile(t, root, "2026/10/14", inventoryOwner, raw)
			if err = os.Chtimes(other, before.ModTime(), before.ModTime()); err != nil {
				t.Fatal(err)
			}
			if err = l.Check(t.Context(), inventoryOwner, r.Revision); err == nil {
				t.Fatal("late duplicate survived currentness")
			}
		})
	}
}

type inventoryAdversary string

const (
	inventoryDuplicate inventoryAdversary = "duplicate"
	inventoryMalformed inventoryAdversary = "malformed"
	inventorySymlink   inventoryAdversary = "symlink"
	inventoryEmpty     inventoryAdversary = "empty"
	inventoryNewOwner  inventoryAdversary = "new-owner"
)

func TestNativeInventoryAmbiguousMalformedSymlinkAndOwnerEpoch(t *testing.T) {
	t.Parallel()
	for _, mode := range []inventoryAdversary{inventoryDuplicate, inventoryMalformed, inventorySymlink, inventoryEmpty, inventoryNewOwner} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			home, root, raw := inventoryFixture(t)
			path := inventoryFile(t, root, "2026/10/15", inventoryOwner, raw)
			switch mode {
			case inventoryDuplicate:
				inventoryFile(t, root, "2026/10/14", inventoryOwner, raw)
			case inventoryMalformed:
				if err := os.WriteFile(path, []byte("bad\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case inventorySymlink:
				if err := os.Rename(filepath.Dir(path), filepath.Dir(path)+".moved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Dir(path)+".moved", filepath.Dir(path)); err != nil {
					t.Fatal(err)
				}
			case inventoryNewOwner:
				// Keep the valid baseline for a later out-of-snapshot request.
			case inventoryEmpty:
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			l := inventoryOpen(t, home, root, inventoryOwner)
			result, err := l.Locate(t.Context(), agentapi.LocatorRequest{NativeID: inventoryOwner, Root: root})
			if mode == inventoryNewOwner {
				if result.Status != agentapi.LocatorFound || err != nil {
					t.Fatal(result, err)
				}
				foreign := agentapi.LocatorRequest{NativeID: "00000000-0000-7000-8000-000000000002", Root: root}
				if r, e := l.Locate(t.Context(), foreign); e != nil || r.Status != agentapi.LocatorRefused {
					t.Fatal(r, e)
				}
				return
			}
			if result.Status == agentapi.LocatorFound {
				t.Fatal(mode, result, err)
			}
			if mode == inventoryDuplicate && result.Status != agentapi.LocatorAmbiguous {
				t.Fatal(result, err)
			}
		})
	}
}

func TestNativeInventoryCatalogMergeResetAndContention(t *testing.T) {
	t.Parallel()
	home, root, raw := inventoryFixture(t)
	inventoryFile(t, root, "2026/10/15", inventoryOwner, raw)
	path := filepath.Join(home, "discovery-catalog.json")
	if err := os.WriteFile(path, []byte(`{"version":6,"roots":["codex"],"future_codex":{"v":1}}`), 0600); err != nil {
		t.Fatal(err)
	}
	l := inventoryOpen(t, home, root, inventoryOwner)
	if inventoryLocate(t, l, inventoryOwner).Status != agentapi.LocatorFound {
		t.Fatal("initial")
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	// The actual current cooperating Codex writer preserves the native member.
	if err := persistNativeCatalog(t.Context(), nil, path, catalog{Version: 6, Roots: []string{"new-codex"}}); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "native_inventory") || !strings.Contains(string(contents), "future_codex") {
		t.Fatal("lost unrelated member")
	}
	restored := inventoryOpen(t, home, root, inventoryOwner)
	if inventoryLocate(t, restored, inventoryOwner).Status != agentapi.LocatorFound || restored.Counts().Names != 0 {
		t.Fatal("cooperating writer reset")
	}
	// A concurrent actual-member replacement must not overwrite current ownership.
	var object map[string]json.RawMessage
	if err = json.Unmarshal(contents, &object); err != nil {
		t.Fatal(err)
	}
	delete(object, "native_inventory")
	changed, _ := json.Marshal(object)
	if err = os.WriteFile(path, changed, 0600); err != nil {
		t.Fatal(err)
	}
	if err := restored.Close(); !errors.Is(err, errNativeCatalogChanged) {
		t.Fatal(err)
	}
	restored.err = nil
	reset := inventoryOpen(t, home, root, inventoryOwner)
	if inventoryLocate(t, reset, inventoryOwner).Status != agentapi.LocatorFound || reset.Counts().Names == 0 {
		t.Fatal("dropped observations retained authority")
	}
}

func FuzzNativeInventoryName(f *testing.F) {
	for _, s := range []string{"2026", "01", "15", inventoryOwner, "../2026", "2026/10/15"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if nativeOwner(s) && (len(s) != 36 || strings.ContainsAny(s, "/\\")) {
			t.Fatal("unsafe owner")
		}
		if nativeDirPath(s) && s != "." && !filepath.IsLocal(s) {
			t.Fatal("unsafe directory")
		}
	})
}

func FuzzNativeInventoryProgress(f *testing.F) {
	sum := sha256.Sum256([]byte(inventoryOwner))
	seed := nativeProgress{Query: hex.EncodeToString(sum[:]), Version: 1, Layout: "muse-root-v1", Classifier: 1, Root: archive.NativeRoot{Agent: "muse", Configured: "/synthetic", Canonical: "/synthetic", Revision: "one"}, Owners: []string{inventoryOwner}, Directories: []nativeDirectory{{Path: ".", Generation: nativeGeneration{Device: 1, Inode: 1, Mode: uint32(os.ModeDir | 0700), Size: 1, Mtime: 1, CtimeSec: 1}, Pages: []nativePage{{Kind: nativeEOF}}}}}
	encoded, _ := json.Marshal(seed)
	f.Add(encoded)
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"version":1,"layout":"muse-root-v1"}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		p, ok := decodeNativeProgress(raw)
		if ok {
			encoded, err := json.Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := decodeNativeProgress(encoded); !ok {
				t.Fatal("recognized observation failed roundtrip")
			}
		}
	})
}

// A test-only child exercises actual fresh-process object/state lifetimes, not
// an in-memory cookie carried by the measurement driver.
func TestNativeInventoryFreshProcessStep(t *testing.T) {
	home := os.Getenv("AA_NATIVE_INVENTORY_STEP_HOME")
	if home == "" {
		t.Skip("fresh-process helper")
	}
	rootPath := os.Getenv("AA_NATIVE_INVENTORY_STEP_ROOT")
	root := archive.NativeRoot{Agent: "muse", Configured: rootPath, Canonical: rootPath, Revision: "one"}
	l := inventoryOpen(t, home, root, inventoryOwner)
	if os.Getenv("AA_NATIVE_INVENTORY_TIME_STOP") == "1" {
		l.remaining = Budget/4 + 2*time.Millisecond
	}
	result, err := l.Locate(t.Context(), agentapi.LocatorRequest{NativeID: inventoryOwner, Root: root})
	if err != nil && agentapi.Failure(err) != agentapi.Unavailable {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(struct {
		Status agentapi.LocatorStatus `json:"Status"`
		Counts NativeInventoryCounts  `json:"Counts"`
	}{result.Status, l.Counts()})
	if err := os.WriteFile(filepath.Join(home, "step-result.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestNativeInventoryFreshProcessesResumeBeyondPassAndTimeStops(t *testing.T) {
	// Sequential: the child binary owns each bounded acquisition pass.
	for _, timeStop := range []bool{false, true} {
		t.Run(strconv.FormatBool(timeStop), func(t *testing.T) {
			home, root, raw := inventoryFixture(t)
			inventoryFile(t, root, "2026/10/15", inventoryOwner, raw)
			day := filepath.Join(root.Canonical, "2026", "10", "15")
			for i := range 10004 {
				if err := os.WriteFile(filepath.Join(day, fmt.Sprintf("invalid-%05d", i)), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			total, uncommitted := int64(0), int64(0)
			found := false
			for tick := range 32 {
				ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
				cmd := exec.CommandContext(ctx, exe, "-test.run=^TestNativeInventoryFreshProcessStep$", "-test.count=1")
				cmd.Env = append(os.Environ(), "AA_NATIVE_INVENTORY_STEP_HOME="+home, "AA_NATIVE_INVENTORY_STEP_ROOT="+root.Canonical)
				if timeStop && tick < 3 {
					cmd.Env = append(cmd.Env, "AA_NATIVE_INVENTORY_TIME_STOP=1")
				}
				out, err := cmd.CombinedOutput()
				cancel()
				if err != nil {
					t.Fatalf("tick%d: %v %s", tick, err, out)
				}
				raw, err := os.ReadFile(filepath.Join(home, "step-result.json"))
				if err != nil {
					t.Fatal(err)
				}
				var result struct {
					Status agentapi.LocatorStatus `json:"Status"`
					Counts NativeInventoryCounts  `json:"Counts"`
				}
				if err = json.Unmarshal(raw, &result); err != nil {
					t.Fatal(err)
				}
				if result.Counts.Names > 2048 {
					t.Fatal(result)
				}
				total += result.Counts.Names
				uncommitted += result.Counts.UncommittedNames
				t.Logf("time_stop=%v tick=%d pid=%d status=%s counts=%+v", timeStop, tick, cmd.Process.Pid, result.Status, result.Counts)
				if result.Status == agentapi.LocatorFound {
					found = true
					break
				}
			}
			if !found || total-uncommitted != 10008 || !timeStop && uncommitted != 0 {
				t.Fatalf("found=%v totalnames=%d uncommitted=%d", found, total, uncommitted)
			}
		})
	}
}

func TestNativeLocatorStartupUsesExistingAggregateHeaderCeiling(t *testing.T) {
	t.Parallel()
	if sourcefacts.HeaderBytes != 256<<10 {
		t.Fatal("shared header compatibility changed")
	}
	home, root, raw := inventoryFixture(t)
	path := inventoryFile(t, root, "2026/10/15", inventoryOwner, raw)
	l := inventoryOpen(t, home, root, inventoryOwner)
	// A large first physical record cannot spend seven independent allowances.
	oversized := append([]byte(strings.Repeat(" ", sourcefacts.HeaderBytes)), raw...)
	if err := os.WriteFile(path, oversized, 0600); err != nil {
		t.Fatal(err)
	}
	result, _ := l.Locate(t.Context(), agentapi.LocatorRequest{NativeID: inventoryOwner, Root: root})
	if result.Status == agentapi.LocatorFound || l.Counts().HeaderBytes > sourcefacts.HeaderBytes {
		t.Fatal(result, l.Counts())
	}
}

func TestNativeInventoryDateClassificationMatchesOrdinarySource(t *testing.T) {
	t.Parallel()
	for _, day := range []string{"0000/01/01", "9999/12/31", "2024/02/29"} {
		t.Run(day, func(t *testing.T) {
			t.Parallel()
			home, root, raw := inventoryFixture(t)
			inventoryFile(t, root, day, inventoryOwner, raw)
			l := inventoryOpen(t, home, root, inventoryOwner)
			if r := inventoryLocate(t, l, inventoryOwner); r.Status != agentapi.LocatorFound {
				t.Fatal(r)
			}
		})
	}
}

func TestNativeInventoryInvalidOwnerDoesNotBlockValidFrozenQueries(t *testing.T) {
	t.Parallel()
	home, root, raw := inventoryFixture(t)
	inventoryFile(t, root, "2026/10/15", inventoryOwner, raw)
	l := inventoryOpen(t, home, root, "unsafe/owner", inventoryOwner)
	if r := inventoryLocate(t, l, inventoryOwner); r.Status != agentapi.LocatorFound {
		t.Fatal(r)
	}
	if r, e := l.Locate(t.Context(), agentapi.LocatorRequest{NativeID: "unsafe/owner", Root: root}); e != nil || r.Status != agentapi.LocatorRefused {
		t.Fatal(r, e)
	}
}

func TestNativeInventoryCohortScopeRevisionAndSchedulingMarker(t *testing.T) {
	t.Parallel()
	home, root, raw := inventoryFixture(t)
	inventoryFile(t, root, "2026/10/15", inventoryOwner, raw)
	initial := inventoryOpen(t, home, root, inventoryOwner)
	if err := initial.Close(); err != nil {
		t.Fatal(err)
	}
	newcomer := inventoryMeasurementID(1)
	l := inventoryOpen(t, home, root, inventoryOwner, newcomer)
	if r := inventoryLocate(t, l, newcomer); r.Status != agentapi.LocatorPending || l.Counts().Names != 0 {
		t.Fatal("newcomer used historical cohort", r, l.Counts())
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	root.Revision = "two"
	revised := inventoryOpen(t, home, root, inventoryOwner, newcomer)
	if len(revised.progress.Owners) != 2 || len(revised.progress.Directories[0].Pages) != 0 {
		t.Fatal("revision retained old coverage")
	}
	revised.progress.Next = len(revised.progress.Owners)
	marker, err := json.Marshal(revised.progress)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := decodeNativeProgress(marker); !ok {
		t.Fatal("inclusive scheduling marker rejected")
	}
	revised.progress.Next++
	malformed, err := json.Marshal(revised.progress)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := decodeNativeProgress(malformed); ok {
		t.Fatal("out-of-range scheduling marker accepted")
	}
	revised.progress.Next = 0
	if r := inventoryLocate(t, revised, inventoryOwner); r.Status != agentapi.LocatorFound || revised.Counts().Names != 4 {
		t.Fatal("revision did not independently enumerate", r, revised.Counts())
	}
}

// Seed a genuine pre-EOF checkpoint using the shared pass name allowance.
func inventoryQueuedDay(t *testing.T, home string, root archive.NativeRoot, ids ...string) string {
	t.Helper()
	crowded := filepath.Join(root.Canonical, "2026", "10", "14")
	if err := os.MkdirAll(crowded, 0700); err != nil {
		t.Fatal(err)
	}
	for i := range 2300 {
		if err := os.WriteFile(filepath.Join(crowded, fmt.Sprintf("unrelated-%05d", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	l := inventoryOpen(t, home, root, ids...)
	if r := inventoryLocate(t, l, ids[0]); r.Status != agentapi.LocatorPending {
		t.Fatal(r)
	}
	queued := false
	for _, d := range l.progress.Directories {
		if d.Path == "2026/10/14" && len(d.Pages) > 0 && !completeNative(d) {
			queued = true
		}
	}
	if !queued || l.Counts().Names > 2048 {
		t.Fatal("fixture did not retain partial day", l.progress, l.Counts())
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return crowded
}

func inventoryBoundedCounts(t *testing.T, l *NativeInventory) {
	t.Helper()
	c := l.Counts()
	if c.Names > 2048 || c.Probes > HeaderProbes || c.Stats+c.Opens > int64(maxDirectories*16) || c.Stats != c.AcquireStats+c.SweepStats+c.CandidateStats || c.Opens != c.AcquireOpens+c.SweepOpens+c.CandidateOpens {
		t.Fatal("unbounded or unaccounted inventory work", c)
	}
}

func TestNativeInventoryQueuedDirectoryChangesRecoverAcrossRestarts(t *testing.T) {
	t.Parallel()
	type directoryChange string
	const (
		deleted           directoryChange = "deleted"
		renamed           directoryChange = "renamed"
		missingAncestor   directoryChange = "missing-ancestor"
		replacedDirectory directoryChange = "replaced-directory"
		replacedFile      directoryChange = "replaced-file"
		replacedSymlink   directoryChange = "replaced-symlink"
	)
	for _, change := range []directoryChange{deleted, renamed, missingAncestor, replacedDirectory, replacedFile, replacedSymlink} {
		t.Run(string(change), func(t *testing.T) {
			t.Parallel()
			home, root, raw := inventoryFixture(t)
			later := "00000000-0000-7000-8000-000000000002"
			day := "2026/10/16"
			if change == missingAncestor {
				day = "2026/11/16"
			}
			inventoryFile(t, root, day, inventoryOwner, raw)
			inventoryFile(t, root, day, later, inventoryMeasurementRaw(t, raw, later))
			crowded := inventoryQueuedDay(t, home, root, inventoryOwner, later)
			moved := filepath.Join(t.TempDir(), "old-day")
			if err := os.Rename(crowded, moved); err != nil {
				t.Fatal(err)
			}
			switch change {
			case deleted:
				// The old path is already absent.
			case missingAncestor:
				if err := os.Remove(filepath.Dir(crowded)); err != nil {
					t.Fatal(err)
				}
			case renamed:
				if err := os.Rename(moved, filepath.Join(filepath.Dir(crowded), "13")); err != nil {
					t.Fatal(err)
				}
			case replacedDirectory:
				if err := os.Mkdir(crowded, 0700); err != nil {
					t.Fatal(err)
				}
			case replacedFile:
				if err := os.WriteFile(crowded, nil, 0600); err != nil {
					t.Fatal(err)
				}
			case replacedSymlink:
				if err := os.Symlink(t.TempDir(), crowded); err != nil {
					t.Fatal(err)
				}
			}
			recovered := false
			for pass := range 4 {
				l := inventoryOpen(t, home, root, inventoryOwner, later)
				r := inventoryLocate(t, l, inventoryOwner)
				inventoryBoundedCounts(t, l)
				if (change == replacedFile || change == replacedSymlink) && pass < 2 {
					if r.Status != agentapi.LocatorPending || len(l.tokens) != 0 {
						t.Fatal("unsupported queued path authorized selection", r)
					}
					if pass == 1 {
						if err := os.Remove(crowded); err != nil {
							t.Fatal(err)
						}
					}
				}
				if r.Status == agentapi.LocatorFound {
					if inventoryLocate(t, l, later).Status != agentapi.LocatorFound {
						t.Fatal("later cohort owner starved")
					}
					if err := l.Check(t.Context(), inventoryOwner, r.Revision); err != nil {
						t.Fatal(err)
					}
					inventoryBoundedCounts(t, l)
					recovered = true
				}
				if err := l.Close(); err != nil {
					t.Fatal(err)
				}
				if recovered {
					break
				}
			}
			if !recovered {
				t.Fatal("valid pending owners blocked by stale queued directory")
			}
		})
	}
}

func TestNativeInventoryCompletedDirectoryDeletionRecovers(t *testing.T) {
	t.Parallel()
	home, root, raw := inventoryFixture(t)
	inventoryFile(t, root, "2026/10/16", inventoryOwner, raw)
	old := filepath.Join(root.Canonical, "2026", "10", "14")
	if err := os.MkdirAll(old, 0700); err != nil {
		t.Fatal(err)
	}
	l := inventoryOpen(t, home, root, inventoryOwner)
	if r := inventoryLocate(t, l, inventoryOwner); r.Status != agentapi.LocatorFound {
		t.Fatal(r)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(old); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		next := inventoryOpen(t, home, root, inventoryOwner)
		r, err := next.Locate(t.Context(), agentapi.LocatorRequest{Root: root, NativeID: inventoryOwner})
		// The first currentness sweep detects the changed completed parent.
		if err != nil && !errors.Is(err, transcriptio.ErrChanged) {
			t.Fatal(err)
		}
		inventoryBoundedCounts(t, next)
		if err := next.Close(); err != nil {
			t.Fatal(err)
		}
		if r.Status == agentapi.LocatorFound {
			return
		}
	}
	t.Fatal("completed day deletion blocked recovery")
}

func TestNativeInventoryTransientDirectoryPermissionRetainsProgress(t *testing.T) {
	t.Parallel()
	home, root, raw := inventoryFixture(t)
	inventoryFile(t, root, "2026/10/16", inventoryOwner, raw)
	crowded := inventoryQueuedDay(t, home, root, inventoryOwner)
	blocked := inventoryOpen(t, home, root, inventoryOwner)
	before, err := json.Marshal(blocked.progress)
	if err != nil {
		t.Fatal(err)
	}
	denied := 0
	if err := blocked.acquire(t.Context(), func(path string) (*os.File, nativeGeneration, error) {
		if strings.HasPrefix(path, "2026/10/") {
			denied++
			return nil, nativeGeneration{}, &os.PathError{Op: "open", Path: crowded, Err: os.ErrPermission}
		}
		return blocked.directory(path)
	}); err != nil {
		t.Fatal(err)
	}
	after, err := json.Marshal(blocked.progress)
	if err != nil {
		t.Fatal(err)
	}
	if denied == 0 || string(before) != string(after) || len(blocked.tokens) != 0 {
		t.Fatal("temporary denial changed coverage or selected", denied, blocked.Counts())
	}
	inventoryBoundedCounts(t, blocked)
	if err := blocked.Close(); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		next := inventoryOpen(t, home, root, inventoryOwner)
		r, err := next.Locate(t.Context(), agentapi.LocatorRequest{Root: root, NativeID: inventoryOwner})
		if err != nil && !errors.Is(err, transcriptio.ErrChanged) {
			t.Fatal(err)
		}
		inventoryBoundedCounts(t, next)
		if err := next.Close(); err != nil {
			t.Fatal(err)
		}
		if r.Status == agentapi.LocatorFound {
			return
		}
	}
	t.Fatal("temporary denial blocked stable progress after restoration")
}

func TestNativeInventoryReplacementDirectoryKeepsDuplicateAmbiguous(t *testing.T) {
	t.Parallel()
	home, root, raw := inventoryFixture(t)
	inventoryFile(t, root, "2026/10/16", inventoryOwner, raw)
	crowded := inventoryQueuedDay(t, home, root, inventoryOwner)
	if err := os.Rename(crowded, filepath.Join(t.TempDir(), "old-day")); err != nil {
		t.Fatal(err)
	}
	inventoryFile(t, root, "2026/10/14", inventoryOwner, raw)
	for range 3 {
		next := inventoryOpen(t, home, root, inventoryOwner)
		r := inventoryLocate(t, next, inventoryOwner)
		if r.Status == agentapi.LocatorFound || len(next.tokens) != 0 {
			t.Fatal("replacement bypassed uniqueness", r)
		}
		inventoryBoundedCounts(t, next)
		if err := next.Close(); err != nil {
			t.Fatal(err)
		}
		if r.Status == agentapi.LocatorAmbiguous {
			return
		}
	}
	t.Fatal("replacement duplicate was not classified")
}
