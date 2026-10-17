package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestLoadRegistrationsBoundsReadsAndKeepsSortedResults(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	dir := filepath.Join(home, "registrations")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for i := range 24 {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%02d.json", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Non-registration entries must not consume a file read.
	if err := os.Mkdir(filepath.Join(dir, "nested.json"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 24)
	release := make(chan struct{})
	var active, peak, reads atomic.Int32
	type result struct {
		regs []archive.SessionRegistration
		err  error
	}
	done := make(chan result, 1)
	go func() {
		regs, err := OpenReadOnly(home).loadRegistrations(func(path string, value any) error {
			reads.Add(1)
			current := active.Add(1)
			for prior := peak.Load(); current > prior; prior = peak.Load() {
				if peak.CompareAndSwap(prior, current) {
					break
				}
			}
			started <- struct{}{}
			<-release
			active.Add(-1)
			name := filepath.Base(path)
			// Reverse the filenames' order to prove the final identity sort survives.
			var n int
			if _, err := fmt.Sscanf(name, "%02d.json", &n); err != nil {
				return err
			}
			*value.(*archive.SessionRegistration) = archive.SessionRegistration{ArchiveSessionID: fmt.Sprintf("id-%02d", 23-n)}
			return nil
		})
		done <- result{regs, err}
	}()
	// Release even on failure, so no task-owned goroutine is left blocked.
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	for range 8 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("eight registration workers did not start")
		}
	}
	if got := active.Load(); got != 8 {
		t.Fatalf("active reads=%d, want 8", got)
	}
	close(release)
	got := <-done
	if got.err != nil || len(got.regs) != 24 {
		t.Fatalf("registrations=%d err=%v", len(got.regs), got.err)
	}
	if peak.Load() != 8 || reads.Load() != 24 {
		t.Fatalf("peak=%d reads=%d", peak.Load(), reads.Load())
	}
	for i, reg := range got.regs {
		if reg.ArchiveSessionID != fmt.Sprintf("id-%02d", i) {
			t.Fatalf("position %d: %q", i, reg.ArchiveSessionID)
		}
	}
}

func TestLoadRegistrationsKeepsFilenameErrorPrecedence(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	dir := filepath.Join(home, "registrations")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.json", "b.json", "c.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	laterFailed := make(chan struct{})
	_, err := OpenReadOnly(home).loadRegistrations(func(path string, _ any) error {
		switch filepath.Base(path) {
		case "a.json":
			select {
			case <-laterFailed:
			case <-time.After(5 * time.Second):
				return errors.New("reads did not overlap")
			}
			return errors.New("first failure")
		case "b.json":
			close(laterFailed)
			return errors.New("later failure")
		default:
			return os.ErrNotExist // Forgotten after enumeration.
		}
	})
	if err == nil || !strings.Contains(err.Error(), `read registration "a.json": first failure`) {
		t.Fatalf("error=%v", err)
	}
}
