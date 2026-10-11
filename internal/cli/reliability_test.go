package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/revocation"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type failingOutput struct{ short bool }

func (w failingOutput) Write(p []byte) (int, error) {
	if w.short {
		return len(p) - 1, nil
	}
	return 0, errors.New("synthetic output failure")
}

func TestJSONOutputMustReachWriter(t *testing.T) {
	t.Parallel()
	for _, w := range []io.Writer{failingOutput{}, failingOutput{short: true}} {
		var stderr bytes.Buffer
		if code := printJSON(w, &stderr, map[string]string{"text": "test"}); code != 1 {
			t.Fatalf("code=%d", code)
		}
		if code := runStatusCommand([]string{"--json"}, w, &stderr, testEnv(t, t.TempDir(), time.Now())); code != 1 {
			t.Fatalf("status code=%d", code)
		}
	}
	var out, errOut bytes.Buffer
	if code := printJSON(&out, &errOut, map[string]string{"text": "hello\u202e"}); code != 0 {
		t.Fatal(errOut.String())
	}
	var decoded map[string]string
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil || strings.Contains(out.String(), "\u202e") {
		t.Fatalf("output=%q err=%v", out.String(), err)
	}
}

func TestFeedbackRegularFileContract(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "feedback")
	if err := os.WriteFile(path, []byte("useful text"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, link} {
		if data, err := readFeedbackFile(p); err != nil || string(data) != "useful text" {
			t.Fatalf("data=%q err=%v", data, err)
		}
	}
	if err := os.WriteFile(path, []byte{0xff}, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readFeedbackFile(path); err == nil {
		t.Fatal("invalid UTF8 accepted")
	}
	if _, err := readFeedbackFile(dir); err == nil {
		t.Fatal("directory accepted")
	}
	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	synctest.Test(t, func(t *testing.T) {
		if _, err := readFeedbackFile(fifo); err == nil {
			t.Fatal("FIFO accepted")
		}
		if err := os.WriteFile(path, []byte("text"), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := readFeedbackFileWithOpen(path, func(p string) (*os.File, error) {
			if err := os.Remove(p); err != nil {
				return nil, err
			}
			if err := syscall.Mkfifo(p, 0600); err != nil {
				return nil, err
			}
			return os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		})
		if err == nil {
			t.Fatal("substituted FIFO accepted")
		}
	})
}

func TestHandoffOutputIsPrivateAndAtomic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "handoff")
	if err := writeHandoffOutput(path, []byte("old"), false); err != nil {
		t.Fatal(err)
	}
	if err := writeHandoffOutput(path, []byte("new"), false); err == nil {
		t.Fatal("overwrote existing")
	}
	if err := writeHandoffOutput(path, []byte("new"), true); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("info=%v err=%v", info, err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := writeHandoffOutput(link, []byte("bad"), true); err == nil {
		t.Fatal("accepted symlink")
	}
	if err := writeHandoffOutput(dir, []byte("bad"), true); err == nil {
		t.Fatal("accepted directory")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "new" {
		t.Fatalf("data=%q err=%v", data, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("temporary artifacts: %v %v", entries, err)
	}
}

func TestPurgeUsageBeforeConfiguration(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"--mode", "invalid"}, {"--mode", "old-filter"}, {"--before-filter", "3"}, {"--mode", "old-filter", "--before-filter", "no"}, {"--mode", "old-filter", "--before-filter", "0"}} {
		env := testEnv(t, t.TempDir(), time.Now())
		env.Home = func() (string, error) { t.Fatal("usage read home"); return "", nil }
		var out, stderr bytes.Buffer
		if code := runPurgePlan(args, &out, &stderr, env); code != 2 {
			t.Fatalf("args=%v code=%d stderr=%s", args, code, stderr.String())
		}
	}
}

func TestReadContextCancellationAndCleanup(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		signals := make(chan os.Signal, 1)
		stopped := false
		env := Env{Interrupts: func() (<-chan os.Signal, func()) { return signals, func() { stopped = true } }}
		ctx, stop := readCommandContext(env)
		signals <- os.Interrupt
		<-ctx.Done()
		stop()
		if !stopped {
			t.Fatal("signal subscription leaked")
		}
	})
}

type failedTemporaryOutput struct {
	*os.File
	step string
}

func (f failedTemporaryOutput) Write(p []byte) (int, error) {
	if f.step == "write" {
		return 0, errors.New("write failed")
	}
	return f.File.Write(p)
}

func (f failedTemporaryOutput) Close() error {
	err := f.File.Close()
	if f.step == "close" {
		return errors.New("close failed")
	}
	return err
}

func TestAtomicOutputFailurePreservesPreviousFile(t *testing.T) {
	t.Parallel()
	for _, step := range []string{"write", "close", "replace"} {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "output")
			if err := os.WriteFile(path, []byte("previous"), 0600); err != nil {
				t.Fatal(err)
			}
			ops := outputFileOperations{
				createTemp: func(dir, pattern string) (atomicOutputFile, error) {
					f, err := os.CreateTemp(dir, pattern)
					if err != nil {
						return nil, err
					}
					return failedTemporaryOutput{File: f, step: step}, nil
				},
				link: os.Link,
				rename: func(old, destination string) error {
					if step == "replace" {
						return errors.New("replace failed")
					}
					return os.Rename(old, destination)
				},
			}
			if err := writeAtomicOutput(path, []byte("new"), true, ops); err == nil {
				t.Fatal("injected failure succeeded")
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != "previous" {
				t.Fatalf("data=%q err=%v", data, err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 {
				t.Fatalf("temporary artifacts: %v %v", entries, err)
			}
		})
	}
}

// stalledCommandStore exposes only the required store port, forcing the reader
// to use this listing rather than an optional in-memory listing shortcut.
type stalledCommandStore struct {
	storage.ObjectStore
	signals chan os.Signal
}

func (s stalledCommandStore) List(ctx context.Context, _ string) ([]storage.Object, error) {
	s.signals <- os.Interrupt
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestListInterruptCancelsStalledStore(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	now := time.Now()
	setUpTestConfig(t, home, project, now.Add(-time.Hour))
	synctest.Test(t, func(t *testing.T) {
		signals := make(chan os.Signal, 1)
		env := testEnv(t, home, now)
		env.Interrupts = func() (<-chan os.Signal, func()) { return signals, func() {} }
		env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
			return stalledCommandStore{ObjectStore: storagetest.NewMemoryStore(), signals: signals}, nil
		}
		var out, stderr bytes.Buffer
		if code := runListCommand([]string{"--json", "--all-projects"}, nil, &out, &stderr, env); code != 1 {
			t.Fatalf("code=%d stderr=%s", code, stderr.String())
		}
		if out.Len() != 0 {
			t.Fatalf("partial successful output=%s", out.String())
		}
	})
}

func TestRevocationJSONOutputChecksDelivery(t *testing.T) {
	t.Parallel()
	for _, w := range []io.Writer{failingOutput{}, failingOutput{short: true}} {
		if err := printRevocation(w, revocation.Journal{}, true); err == nil {
			t.Fatal("JSON output failure ignored")
		}
		if code := revocationOutputCode(w, io.Discard, revocation.Journal{}, true); code != 1 {
			t.Fatalf("revocation output code=%d", code)
		}
	}
}

func TestMachinesUsageBeforeConfiguration(t *testing.T) {
	t.Parallel()
	env := testEnv(t, t.TempDir(), time.Now())
	env.Home = func() (string, error) { t.Fatal("usage read home"); return "", nil }
	var out, stderr bytes.Buffer
	if code := runMachinesCommand([]string{"--yes"}, nil, &out, &stderr, env); code != 2 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
}

func TestShowTranscriptJSONChecksDelivery(t *testing.T) {
	t.Parallel()
	env, _, id := publishedFixture(t)
	for _, flags := range [][]string{{"--transcript", "--json"}, {"--normalized"}} {
		for _, w := range []io.Writer{failingOutput{}, failingOutput{short: true}} {
			var stderr bytes.Buffer
			args := append([]string{"show", id}, flags...)
			if code := Run(args, nil, w, &stderr, env); code != 1 {
				t.Fatalf("args=%v code=%d stderr=%s", args, code, stderr.String())
			}
		}
	}
}

func TestShowReleasesReadSignalsBeforePager(t *testing.T) {
	t.Parallel()
	for _, transcript := range []bool{false, true} {
		env, _, id := publishedFixture(t)
		synctest.Test(t, func(t *testing.T) {
			var out, stderr bytes.Buffer
			env.IsTerminal = func(stream any) bool { return stream == any(&out) }
			active := map[chan os.Signal]bool{}
			env.Interrupts = func() (<-chan os.Signal, func()) {
				signals := make(chan os.Signal, 1)
				active[signals] = true
				return signals, func() { delete(active, signals) }
			}
			env.RunPager = func(ctx context.Context, _ string, _ []string, _ io.Reader, _, _ io.Writer) error {
				if len(active) != 1 {
					t.Errorf("active signal subscriptions at pager=%d, want only pager", len(active))
				}
				// signal.Notify broadcasts to every active subscription.
				for signals := range active {
					signals <- os.Interrupt
				}
				synctest.Wait()
				if ctx.Err() != nil {
					t.Errorf("Ctrl-C cancelled pager context: %v", ctx.Err())
				}
				return nil
			}
			args := []string{"show", id}
			if transcript {
				args = append(args, "--transcript")
			}
			if code := Run(args, nil, &out, &stderr, env); code != 0 {
				t.Fatalf("code=%d stderr=%s", code, stderr.String())
			}
			if len(active) != 0 {
				t.Fatalf("leaked subscriptions=%d", len(active))
			}
		})
	}
}

func TestAmbiguousShowReleasesReadSignalsBeforeBrowserPager(t *testing.T) {
	t.Parallel()
	env, store, id := publishedFixture(t)
	key := "sessions/codex/" + id + "/metadata.json"
	raw, err := store.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	var metadata archive.Metadata
	if err := json.Unmarshal(raw, &metadata); err != nil {
		t.Fatal(err)
	}
	metadata.Title = "shared title"
	raw, err = json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), key, raw); err != nil {
		t.Fatal(err)
	}
	metadata.SessionID = strings.Repeat("f", 32)
	metadata.CapturedAt = metadata.CapturedAt.Add(-time.Hour)
	raw, err = json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), "sessions/codex/"+metadata.SessionID+"/metadata.json", raw); err != nil {
		t.Fatal(err)
	}
	synctest.Test(t, func(t *testing.T) {
		var out, stderr bytes.Buffer
		env.IsTerminal = func(any) bool { return true }
		active := map[chan os.Signal]bool{}
		env.Interrupts = func() (<-chan os.Signal, func()) {
			signals := make(chan os.Signal, 1)
			active[signals] = true
			return signals, func() { delete(active, signals) }
		}
		paged := false
		env.RunPager = func(ctx context.Context, _ string, _ []string, _ io.Reader, _, _ io.Writer) error {
			paged = true
			if len(active) != 1 {
				t.Errorf("subscriptions=%d, want only browser", len(active))
			}
			for signals := range active {
				signals <- os.Interrupt
			}
			synctest.Wait()
			if ctx.Err() != nil {
				t.Errorf("Ctrl-C cancelled browser pager: %v", ctx.Err())
			}
			return nil
		}
		if code := Run([]string{"show", "shared title"}, strings.NewReader("1\nt\nq\n"), &out, &stderr, env); code != 0 {
			t.Fatalf("code=%d stderr=%s", code, stderr.String())
		}
		if !paged {
			t.Fatalf("pager not reached: %s %s", out.String(), stderr.String())
		}
		if len(active) != 0 {
			t.Fatalf("leaked subscriptions=%d", len(active))
		}
	})
}
