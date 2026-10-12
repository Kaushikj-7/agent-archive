//go:build linux || darwin

package cursorstore

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// The stale regular-file observation then FIFO replacement is deterministic.
// A child owns the possibly blocking call; the parent always kills and waits it.
func TestShmHeaderRejectsFIFOReplacementWithoutBlocking(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestShmHeaderFIFOReplacementProcess$")
	cmd.Env = append(os.Environ(), "AGENT_ARCHIVE_SHM_FIFO_TEST=1")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatal("shm open blocked after FIFO replacement; child killed and waited")
	}
	if err != nil {
		t.Fatalf("FIFO replacement child: %v: %s", err, out)
	}
}

func TestShmHeaderFIFOReplacementProcess(t *testing.T) {
	if os.Getenv("AGENT_ARCHIVE_SHM_FIFO_TEST") != "1" {
		t.Skip("isolated process helper, run by parent")
	}
	name := filepath.Join(t.TempDir(), "state.vscdb-shm")
	if err := os.WriteFile(name, bytes.Repeat([]byte{1}, shmHeaderSize), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(name, name+".original"); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(name, 0o600); err != nil {
		t.Fatal(err)
	}
	if header, ok := shmHeaderFile(name, info); ok || header != nil {
		t.Fatal("FIFO replacement supplied a header")
	}
}

func TestShmHeaderRegularAndRefusalControls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.vscdb")
	name := path + "-shm"
	want := bytes.Repeat([]byte{1}, shmHeaderSize)
	if err := os.WriteFile(name, want, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, ok := shmHeader(path); !ok || !bytes.Equal(got, want) {
		t.Fatal("regular header refused")
	}
	info, err := os.Lstat(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(name, name+".original"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, want, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := shmHeaderFile(name, info); ok {
		t.Fatal("different regular-file identity accepted")
	}
	if err := os.Remove(name); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(name+".original", name); err != nil {
		t.Fatal(err)
	}
	if _, ok := shmHeaderFile(name, info); ok {
		t.Fatal("replacement symlink accepted")
	}
	if err := os.Remove(name); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, want[:10], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := shmHeader(path); ok {
		t.Fatal("short header accepted")
	}
}
