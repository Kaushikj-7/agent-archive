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
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/muse"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
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
	day := filepath.Join(root.Canonical, "2026/10/15")
	info, err := os.Stat(day)
	if err != nil {
		t.Fatal(err)
	}
	secondDay := filepath.Join(root.Canonical, "2026/10/14")
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
		t.Run(fmt.Sprint(empty), func(t *testing.T) {
			t.Parallel()
			home, root, raw := inventoryFixture(t)
			inventoryFile(t, root, "2026/10/15", inventoryOwner, raw)
			other := filepath.Join(root.Canonical, "2026/10/14")
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

func TestNativeInventoryAmbiguousMalformedSymlinkAndOwnerEpoch(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"duplicate", "malformed", "symlink", "empty", "new-owner"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			home, root, raw := inventoryFixture(t)
			path := inventoryFile(t, root, "2026/10/15", inventoryOwner, raw)
			switch mode {
			case "duplicate":
				inventoryFile(t, root, "2026/10/14", inventoryOwner, raw)
			case "malformed":
				if err := os.WriteFile(path, []byte("bad\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Rename(filepath.Dir(path), filepath.Dir(path)+".moved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Dir(path)+".moved", filepath.Dir(path)); err != nil {
					t.Fatal(err)
				}
			case "empty":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			l := inventoryOpen(t, home, root, inventoryOwner)
			result, err := l.Locate(t.Context(), agentapi.LocatorRequest{NativeID: inventoryOwner, Root: root})
			if mode == "new-owner" {
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
			if mode == "duplicate" && result.Status != agentapi.LocatorAmbiguous {
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
	seed := nativeProgress{Version: 1, Layout: "muse-root-v1", Classifier: 1, Root: archive.NativeRoot{Agent: "muse", Configured: "/synthetic", Canonical: "/synthetic", Revision: "one"}, Owners: []string{inventoryOwner}, Directories: []nativeDirectory{{Path: ".", Generation: nativeGeneration{Device: 1, Inode: 1, Mode: uint32(os.ModeDir | 0700), Size: 1, Mtime: 1, CtimeSec: 1}, Pages: []nativePage{{Kind: "EOF"}}}}}
	sum := sha256.Sum256([]byte(inventoryOwner))
	seed.Query = hex.EncodeToString(sum[:])
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
		Status agentapi.LocatorStatus
		Counts NativeInventoryCounts
	}{result.Status, l.Counts()})
	if err := os.WriteFile(filepath.Join(home, "step-result.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}
func TestNativeInventoryFreshProcessesResumeBeyondPassAndTimeStops(t *testing.T) {
	// Sequential: the child binary owns each bounded acquisition pass.
	for _, timeStop := range []bool{false, true} {
		t.Run(fmt.Sprint(timeStop), func(t *testing.T) {
			home, root, raw := inventoryFixture(t)
			inventoryFile(t, root, "2026/10/15", inventoryOwner, raw)
			day := filepath.Join(root.Canonical, "2026/10/15")
			for i := 0; i < 10004; i++ {
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
			for tick := 0; tick < 32; tick++ {
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
					Status agentapi.LocatorStatus
					Counts NativeInventoryCounts
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
