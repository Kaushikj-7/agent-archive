package collector

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// putCountingStore counts uploads.
type putCountingStore struct {
	*storagetest.MemoryStore
	mu   sync.Mutex
	puts int
}

func (c *putCountingStore) Put(ctx context.Context, k string, b []byte) error {
	c.mu.Lock()
	c.puts++
	c.mu.Unlock()
	return c.MemoryStore.Put(ctx, k, b)
}

func (c *putCountingStore) takePuts() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := c.puts
	c.puts = 0
	return n
}

// A published state or superseded ledger that no longer
// decodes used to fail its session on every pass, with no way out; a corrupt
// ledger also brought back the C-1 loop, re-uploading the same publication
// every pass because recording the superseded source failed after the upload
// and before the publication was saved. Each is now moved aside once,
// reported once, and the session carries on: a lost published state or
// legacy pending publication as never published, a lost ledger as empty.
// Storage-protected pending bytes instead remain owed and cannot be substituted.
func TestCorruptCollectorOwnedStateIsMovedAsideAndTheSessionRecovers(t *testing.T) {
	t.Parallel()
	for _, dir := range []string{"superseded"} {
		t.Run(dir, func(t *testing.T) {
			t.Parallel()
			local := newTestStore(t)
			remote := &putCountingStore{MemoryStore: storagetest.NewMemoryStore()}
			path := claudeSession(t, local, claudePromptLine+"\n")
			at := time.Date(2026, 9, 22, 13, 0, 0, 0, time.UTC)
			if r := runAt(t, local, remote, at); len(r.Published) != 1 {
				t.Fatalf("%#v", r)
			}
			corrupt := filepath.Join(local.Home(), dir, "session-1.json")
			if err := os.MkdirAll(filepath.Dir(corrupt), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(corrupt, []byte(`{"bundle":{"sche`), 0o600); err != nil {
				t.Fatal(err)
			}
			if dir == "pending" {
				remote.takePuts()
				for range 2 {
					r := runAt(t, local, remote, at.Add(time.Minute))
					if !errors.Is(r.Errors["session-1"], state.ErrDurableStorageRecovery) || len(r.Published) != 0 {
						t.Fatalf("protected corruption: %+v", r)
					}
				}
				raw, err := os.ReadFile(corrupt)
				if err != nil || string(raw) != `{"bundle":{"sche` {
					t.Fatal("protected pending changed", err)
				}
				if pending, err := local.HasPending("session-1"); !pending || err != nil {
					t.Fatalf("protected pending visibility: %v %v", pending, err)
				}
				if len(local.QuarantinedFiles()) != 0 || remote.takePuts() != 0 {
					t.Fatal("protected pending quarantined or substituted")
				}
				return
			}
			var failures int
			content := claudePromptLine + "\n"
			for i := 1; i <= 3; i++ {
				content += fmt.Sprintf(`{"type":"user","uuid":"p%d","sessionId":"native-claude","timestamp":"2026-09-22T12:0%d:00Z","message":{"role":"user","content":"Prompt %d."}}`, i+1, i, i) + "\n"
				writeTranscript(t, dirOf(path), baseOf(path), content)
				at = at.Add(10 * time.Minute)
				if err := local.SaveRequest("session-1", "stop", at); err != nil {
					t.Fatal(err)
				}
				r := runAt(t, local, remote, at)
				if err := r.Errors["session-1"]; err != nil {
					if !errors.Is(err, state.ErrQuarantined) {
						t.Fatalf("pass %d: %v", i, err)
					}
					failures++
				}
				if i == 3 && len(r.Published) != 1 {
					t.Fatalf("pass %d did not publish: %#v", i, r)
				}
			}
			if failures > 1 {
				t.Fatalf("the corrupt file was reported %d times, want at most once", failures)
			}
			if pending, _ := local.HasPending("session-1"); pending {
				t.Fatal("a publication is still pending")
			}
			if quarantined := local.QuarantinedFiles(); len(quarantined) != 1 || !strings.HasPrefix(quarantined[0], dir+"/session-1.json.") {
				t.Fatalf("quarantined = %v", quarantined)
			}
			// Settled: the next pass uploads nothing.
			remote.takePuts()
			runAt(t, local, remote, at.Add(time.Minute))
			if puts := remote.takePuts(); puts != 0 {
				t.Fatalf("a settled session uploaded %d object(s)", puts)
			}
		})
	}
}

// A ledger of an unexpected shape (written by a newer version before a
// downgrade) is not moved aside, but it no longer holds a publication hostage
// either: the publication is saved, the failure reported, and nothing is
// uploaded again on the next pass.
func TestUnreadableLedgerDoesNotReuploadEveryPass(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	remote := &putCountingStore{MemoryStore: storagetest.NewMemoryStore()}
	path := claudeSession(t, local, claudePromptLine+"\n")
	at := time.Date(2026, 9, 22, 13, 0, 0, 0, time.UTC)
	runAt(t, local, remote, at)
	if err := os.MkdirAll(filepath.Join(local.Home(), "superseded"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local.Home(), "superseded", "session-1.json"), []byte(`{"key":"not a list"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	writeTranscript(t, dirOf(path), baseOf(path), claudePromptLine+"\n"+toolResultLine("r1", 10)+"\n")
	remote.takePuts()
	at = at.Add(10 * time.Minute)
	r := runAt(t, local, remote, at)
	if len(r.Published) != 1 || r.Errors["session-1"] == nil {
		t.Fatalf("want a publication reported with the ledger failure: %#v", r)
	}
	if puts := remote.takePuts(); puts != 3 {
		t.Fatalf("puts = %d, want one source, canonical metadata, and v3 hint", puts)
	}
	for i := range 2 {
		at = at.Add(10 * time.Minute)
		runAt(t, local, remote, at)
		if puts := remote.takePuts(); puts != 0 {
			t.Fatalf("pass %d uploaded %d object(s) again", i, puts)
		}
	}
	if pending, _ := local.HasPending("session-1"); pending {
		t.Fatal("the publication was left pending")
	}
}

// A scan journal that no longer decodes reads as a scan still owed, which
// the scan then rewrites; it never fails the session.
func TestCorruptScanJournalIsRewritten(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	remote := storagetest.NewMemoryStore()
	claudeSession(t, local, claudePromptLine+"\n")
	at := time.Date(2026, 9, 22, 13, 0, 0, 0, time.UTC)
	runAt(t, local, remote, at)
	if err := os.WriteFile(filepath.Join(local.Home(), "pending-scans", "session-1.json"), []byte(`tr`), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if r := runAt(t, local, remote, at.Add(time.Duration(i+1)*time.Minute)); len(r.Errors) != 0 {
			t.Fatalf("pass %d: %v", i, r.Errors)
		}
	}
	if pending, err := local.ScanPending("session-1"); err != nil || pending {
		t.Fatalf("scan still pending: %v %v", pending, err)
	}
}
