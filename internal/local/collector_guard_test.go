package local

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCollectorGuardProcessCrashRetainsExactOrigin(t *testing.T) {
	if home := os.Getenv("AGENT_ARCHIVE_GUARD_TEST_HOME"); home != "" {
		guard, err := LockCollectorGuard(home)
		if err != nil {
			t.Fatal(err)
		}
		origin, err := guard.Origin()
		if err != nil {
			t.Fatal(err)
		}
		fmt.Println(origin)
		_, _ = bufio.NewReader(os.Stdin).ReadByte()
		os.Exit(0) // Kernel closes the flock; no application release runs.
	}
	home := t.TempDir()
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestCollectorGuardProcessCrashRetainsExactOrigin$")
	command.Env = append(os.Environ(), "AGENT_ARCHIVE_GUARD_TEST_HOME="+home)
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = input.Close(); _ = command.Process.Kill(); _ = command.Wait() }()
	scanner := bufio.NewScanner(output)
	if !scanner.Scan() {
		t.Fatal("child lock acquisition failed", scanner.Err())
	}
	origin := scanner.Text()
	if _, err = LockCollectorGuard(home); !errors.Is(err, ErrBusy) {
		t.Fatal("another process acquired the live lock", err)
	}
	if err = command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err = command.Wait(); err == nil {
		t.Fatal("child did not crash")
	}
	guard, err := LockCollectorGuard(home)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Release()
	recovered, err := guard.Origin()
	if err != nil || recovered != origin {
		t.Fatal("crash changed original lock authority", err)
	}
	copyHome := t.TempDir()
	principal, err := os.ReadFile(filepath.Join(home, "collector-principal.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = WriteBytes(filepath.Join(copyHome, "collector-principal.json"), principal); err != nil {
		t.Fatal(err)
	}
	copied, err := LockCollectorGuard(copyHome)
	if err != nil {
		t.Fatal(err)
	}
	defer copied.Release()
	if _, err = copied.BeginOperation(origin); err == nil {
		t.Fatal("copied home recovered foreign origin")
	}
}

func TestCollectorGuardRequiresPositiveJournalRemoval(t *testing.T) {
	home := t.TempDir()
	guard, err := LockCollectorGuard(home)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Release()
	origin, err := guard.Origin()
	if err != nil {
		t.Fatal(err)
	}
	journal := CatalogJournal{Owner: strings.Repeat("a", 32), Origin: origin, Destination: "private", SessionID: "session", MutationID: "mutation", SHA256: strings.Repeat("b", 64)}
	if _, err = guard.ProveJournalRemoval(journal); err == nil {
		t.Fatal("absence was accepted as removal proof")
	}
	if err = Write(filepath.Join(home, "pending", "session.json"), journal); err != nil {
		t.Fatal(err)
	}
	if err = guard.RecordJournalRemoval(journal); err != nil {
		t.Fatal(err)
	}
	if _, err = guard.ProveJournalRemoval(journal); err == nil {
		t.Fatal("record before unlink was accepted")
	}
	if err = os.Remove(filepath.Join(home, "pending", "session.json")); err != nil {
		t.Fatal(err)
	}
	if _, err = guard.ProveJournalRemoval(journal); err != nil {
		t.Fatal(err)
	}
}

func TestCollectorGuardReleaseClosesAdmissionAndJoinsInflight(t *testing.T) {
	home := t.TempDir()
	guard, err := LockCollectorGuard(home)
	if err != nil {
		t.Fatal(err)
	}
	origin, err := guard.Origin()
	if err != nil {
		t.Fatal(err)
	}
	done, err := guard.BeginOperation(origin)
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	released := make(chan struct{})
	go func() { guard.Release(); close(released) }()
	deadline := time.Now().Add(time.Second)
	for {
		guard.mu.Lock()
		held := guard.held
		guard.mu.Unlock()
		if !held {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("release did not close admission")
		}
		runtime.Gosched()
	}
	if _, err = guard.BeginOperation(origin); !errors.Is(err, ErrBusy) {
		t.Fatal("release admitted new work", err)
	}
	select {
	case <-released:
		t.Fatal("release failed to join existing operation")
	default:
	}
	if _, err = LockCollectorGuard(home); !errors.Is(err, ErrBusy) {
		t.Fatal("flock released before operation joined", err)
	}
	done()
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("release did not complete after operation joined")
	}
	next, err := LockCollectorGuard(home)
	if err != nil {
		t.Fatal(err)
	}
	next.Release()
}

func TestCollectorGuardRemovalInventoryBoundsAndCrashTemporaries(t *testing.T) {
	home := t.TempDir()
	guard, err := LockCollectorGuard(home)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Release()
	origin, err := guard.Origin()
	if err != nil {
		t.Fatal(err)
	}
	journal := CatalogJournal{Owner: strings.Repeat("a", 32), Origin: origin, Destination: "private", SessionID: "session", MutationID: "mutation", SHA256: strings.Repeat("b", 64)}
	if err = guard.RecordJournalRemoval(journal); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "catalog-completions")
	if err = os.WriteFile(filepath.Join(path, tempPrefix+"crashed"), []byte("incomplete"), 0600); err != nil {
		t.Fatal(err)
	}
	records, err := guard.JournalRemovals()
	if err != nil || len(records) != 1 || records[0] != journal {
		t.Fatal("temporary granted authority or hid exact record", err)
	}
	for n := range 255 {
		if err = os.WriteFile(filepath.Join(path, fmt.Sprintf("%s%d", tempPrefix, n)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = guard.JournalRemovals(); err == nil {
		t.Fatal("over-capacity inventory accepted")
	}
}
