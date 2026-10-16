package discovery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
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
)

func inventoryMeasurementID(i int) string { return fmt.Sprintf("00000000-0000-7000-8000-%012x", i+1) }

// Construct synthetic permission bytes for distinct owners using the documented
// permission-v1 checksum encoding. Production startup independently validates it.
func inventoryMeasurementRaw(t *testing.T, raw []byte, id string) []byte {
	t.Helper()
	raw = bytes.ReplaceAll(raw, []byte(inventoryOwner), []byte(id))
	lines := bytes.SplitN(raw, []byte("\n"), 2)
	var wire struct {
		Transaction string `json:"transaction_id"`
		Children    []struct {
			Index  int    `json:"child_index"`
			Record string `json:"record_json"`
		} `json:"children"`
		Sum string `json:"content_sha256"`
	}
	if err := json.Unmarshal(lines[0], &wire); err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	_, _ = h.Write([]byte("muse.session_permission_transaction.v1\x00"))
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], 1)
	_, _ = h.Write(b[:])
	uuid, err := hex.DecodeString(strings.ReplaceAll(wire.Transaction, "-", ""))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = h.Write(uuid)
	binary.BigEndian.PutUint32(b[:4], uint32(len(wire.Children)))
	_, _ = h.Write(b[:4])
	for i, child := range wire.Children {
		binary.BigEndian.PutUint32(b[:4], uint32(i))
		_, _ = h.Write(b[:4])
		binary.BigEndian.PutUint64(b[:], uint64(len(child.Record)))
		_, _ = h.Write(b[:])
		_, _ = h.Write([]byte(child.Record))
	}
	return bytes.Replace(raw, []byte(wire.Sum), []byte("sha256:"+hex.EncodeToString(h.Sum(nil))), 1)
}

type inventoryMeasurementResult struct {
	Selected []string              `json:"Selected"`
	Counts   NativeInventoryCounts `json:"Counts"`
	Peak     int64                 `json:"Peak"`
}

func TestNativeInventoryMeasurementProcessStep(t *testing.T) {
	home := os.Getenv("AA_NATIVE_INVENTORY_MEASURE_HOME")
	if home == "" {
		t.Skip("fresh-process measurement helper")
	}
	rootPath := os.Getenv("AA_NATIVE_INVENTORY_MEASURE_ROOT")
	root := archive.NativeRoot{Agent: "muse", Configured: rootPath, Canonical: rootPath, Revision: "one"}
	count, err := strconv.Atoi(os.Getenv("AA_NATIVE_INVENTORY_MEASURE_OWNERS"))
	if err != nil || count < 1 || count > 512 {
		t.Fatal("invalid synthetic owner count")
	}
	queries := []agentapi.LocatorRequest{}
	for i := range count {
		queries = append(queries, agentapi.LocatorRequest{NativeID: inventoryMeasurementID(i), Root: root})
	}
	budget := agentapi.NewNativeReadBudget(agentapi.PassReadBudget)
	l, err := NewNativeInventory(t.Context(), home, []archive.NativeRoot{root}, queries, budget, muse.ValidateNativeLocator)
	if err != nil {
		t.Fatal(err)
	}
	selected := []string{}
	for _, q := range queries {
		r, err := l.Locate(t.Context(), q)
		if err != nil {
			continue
		}
		if r.Status == agentapi.LocatorFound {
			if err := l.Check(t.Context(), q.NativeID, r.Revision); err == nil {
				selected = append(selected, q.NativeID)
			}
		}
	}
	if err = l.Close(); err != nil {
		t.Fatal(err)
	}
	used, peak := budget.Charged()
	if used != 0 {
		t.Fatal("charge after close", used)
	}
	result, _ := json.Marshal(inventoryMeasurementResult{Selected: selected, Counts: l.Counts(), Peak: peak})
	if err = os.WriteFile(filepath.Join(home, "measurement-result.json"), result, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestNativeInventoryFewAndManyDayFreshProcessMeasurements(t *testing.T) {
	// Sequential: measurements own child process groups, fixtures and active budgets.
	for _, count := range []int{1, 128, 512} {
		for _, days := range []int{2, 32} {
			t.Run(fmt.Sprintf("owners%d-days%d", count, days), func(t *testing.T) {
				home, root, raw := inventoryFixture(t)
				for i := range count {
					id := inventoryMeasurementID(i)
					day := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i%days).Format("2006/01/02")
					inventoryFile(t, root, day, id, inventoryMeasurementRaw(t, raw, id))
				}
				exe, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				seen := map[string]bool{}
				total := NativeInventoryCounts{}
				peak := int64(0)
				for tick := 0; tick < 128 && len(seen) < count; tick++ {
					ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
					cmd := exec.CommandContext(ctx, exe, "-test.run=^TestNativeInventoryMeasurementProcessStep$", "-test.count=1")
					cmd.Env = append(os.Environ(), "AA_NATIVE_INVENTORY_MEASURE_HOME="+home, "AA_NATIVE_INVENTORY_MEASURE_ROOT="+root.Canonical, fmt.Sprintf("AA_NATIVE_INVENTORY_MEASURE_OWNERS=%d", count))
					out, err := cmd.CombinedOutput()
					cancel()
					if err != nil {
						t.Fatalf("tick%d: %v %s", tick, err, out)
					}
					raw, err := os.ReadFile(filepath.Join(home, "measurement-result.json"))
					if err != nil {
						t.Fatal(err)
					}
					var result inventoryMeasurementResult
					if err = json.Unmarshal(raw, &result); err != nil {
						t.Fatal(err)
					}
					for _, id := range result.Selected {
						seen[id] = true
					}
					c := result.Counts
					total.Names += c.Names
					total.UncommittedNames += c.UncommittedNames
					total.DirentBytes += c.DirentBytes
					total.Stats += c.Stats
					total.Opens += c.Opens
					total.HeaderBytes += c.HeaderBytes
					total.HeaderReads += c.HeaderReads
					total.Probes += c.Probes
					total.Active += c.Active
					total.PageReads += c.PageReads
					total.GenerationChecks += c.GenerationChecks
					total.Sweeps += c.Sweeps
					total.Selections += c.Selections
					total.Checks += c.Checks
					total.AcquireStats += c.AcquireStats
					total.AcquireOpens += c.AcquireOpens
					total.SweepStats += c.SweepStats
					total.SweepOpens += c.SweepOpens
					total.CandidateStats += c.CandidateStats
					total.CandidateOpens += c.CandidateOpens
					if c.Stats != c.AcquireStats+c.SweepStats+c.CandidateStats || c.Opens != c.AcquireOpens+c.SweepOpens+c.CandidateOpens {
						t.Fatal("operation stage accounting mismatch", c)
					}
					peak = max(peak, result.Peak)
					if c.Names > 2048 || c.Probes > HeaderProbes || c.Stats+c.Opens > int64(maxDirectories*16) {
						t.Fatal("pass exceeded limits", c)
					}
					t.Logf("owners=%d days=%d tick=%d pid=%d selected=%d seen=%d counts=%+v peak=%d", count, days, tick, cmd.Process.Pid, len(result.Selected), len(seen), c, result.Peak)
				}
				if len(seen) != count {
					t.Fatalf("fairness incomplete %d/%d", len(seen), count)
				}
				t.Logf("TOTAL owners=%d days=%d counts=%+v peak=%d", count, days, total, peak)
			})
		}
	}
}

func TestNativeInventoryActualOldWriterDropRequiresEnumeration(t *testing.T) {
	// Sequential: explicitly supplied run-owned exact-source old test executable.
	executable := os.Getenv("AA_NATIVE_OLD_WRITER_EXECUTABLE")
	if executable == "" {
		t.Skip("run-owned pinned old writer executable not supplied")
	}
	for _, route := range []string{"nil", "lookup"} {
		t.Run(route, func(t *testing.T) {
			home, root, raw := inventoryFixture(t)
			inventoryFile(t, root, "2026/10/15", inventoryOwner, raw)
			l := inventoryOpen(t, home, root, inventoryOwner)
			if inventoryLocate(t, l, inventoryOwner).Status != agentapi.LocatorFound {
				t.Fatal("initial")
			}
			if err := l.Close(); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestNativeInventoryActualOldWriterProbe$", "-test.count=1", "-test.v")
			cmd.Env = append(os.Environ(), "AA_NATIVE_OLD_WRITER_HOME="+home, "AA_NATIVE_OLD_WRITER_ROUTE="+route)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatal(err, string(output))
			}
			t.Logf("actual pinned writer route=%s pid=%d: %s", route, cmd.Process.Pid, output)
			// Grow unrelated names beyond a pass; missing observations cannot be rebuilt
			// from hints or used to produce a certificate in this first new process.
			for i := range 2300 {
				if err := os.WriteFile(filepath.Join(root.Canonical, "2026", "10", "15", fmt.Sprintf("invalid-%05d", i)), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			reset := inventoryOpen(t, home, root, inventoryOwner)
			r := inventoryLocate(t, reset, inventoryOwner)
			if r.Status != agentapi.LocatorPending || reset.Counts().Names == 0 {
				t.Fatal(r, reset.Counts())
			}
		})
	}
}
