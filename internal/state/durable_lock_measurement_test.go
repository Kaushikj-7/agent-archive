package state

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/trace"
)

// Trace is process-wide, so this opt-in physical measurement is sequential.
// It measures actual guarded-writer lock spans, not untraced throughput.
func TestDurableWriterReportsActualLockSpans(t *testing.T) {
	if os.Getenv("AGENT_ARCHIVE_DURABLE_LOCK_MEASURE") != "1" {
		t.Skip("set AGENT_ARCHIVE_DURABLE_LOCK_MEASURE=1 for isolated synthetic lock measurements")
	}
	s := newTestStore(t)
	for i := range 5000 {
		if err := os.MkdirAll(filepath.Join(s.home, "sessions", fmt.Sprintf("empty-%05d", i)), 0700); err != nil {
			t.Fatal(err)
		}
	}
	stopTrace := trace.Enable()
	defer stopTrace()
	data := []byte("synthetic measured selected source")
	if _, err := s.StagePendingSource("measured", durableRef(data), data); err != nil {
		t.Fatal(err)
	}
	// Capacity refusal must close both actual hold spans as well.
	sparseDurable(t, filepath.Join(s.home, "pending", "full.json"), durableStorageQuota/2)
	if _, err := s.StagePendingSource("refused", durableRef(data), data); !errors.Is(err, ErrDurableStorageCapacity) {
		t.Fatal("missing actual capacity refusal", err)
	}
	if err := os.Remove(filepath.Join(s.home, "pending", "full.json")); err != nil {
		t.Fatal(err)
	}
	// Actual lock contention ends the wait span without creating a hold.
	func() {
		unlock, err := local.NamedLock(s.home, "hooks.lock")
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
		if err := config.WithDurableStorage(s.home, func(config.DurableStorageGuard) error {
			t.Fatal("entered contended hooks guard")
			return nil
		}); !errors.Is(err, local.ErrBusy) {
			t.Fatal("missing hooks wait refusal", err)
		}
	}()
	func() {
		unlock, err := local.NamedLock(s.home, "temporary-quota")
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
		if err := config.WithDurableStorage(s.home, func(g config.DurableStorageGuard) error {
			q, err := s.openDurableQuota(g)
			if q != nil {
				_ = q.Close()
			}
			return err
		}); !errors.Is(err, local.ErrBusy) {
			t.Fatal("missing quota wait refusal", err)
		}
	}()
	var raw bytes.Buffer
	trace.Write(&raw)
	measured := raw.String()
	for _, name := range []string{"durable hooks wait", "durable hooks hold", "durable quota wait", "durable quota hold"} {
		if !strings.Contains(measured, name) {
			t.Fatal("missing actual lock span", name)
		}
	}
	if strings.Contains(measured, "unfinished") {
		t.Fatal("lock wait/hold span escaped return", measured)
	}
	if strings.Contains(measured, s.home) || strings.Contains(measured, "measured selected source") {
		t.Fatal("trace retained private fixture data")
	}
	t.Log("physical inventory:5000 empty session directories, one small selected source; traced actual writer spans (not plain performance):\n" + measured)
}
