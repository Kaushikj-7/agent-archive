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
	"sync/atomic"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// exactFailureStore blocks metadata probes until interruption or returns a read failure.
type exactFailureStore struct {
	storage.ObjectStore
	signals chan os.Signal
	failure error
	started atomic.Bool
	lists   atomic.Int64
}

func (s *exactFailureStore) Get(ctx context.Context, key string) ([]byte, error) {
	if s.failure != nil {
		return nil, s.failure
	}
	if s.started.CompareAndSwap(false, true) {
		s.signals <- os.Interrupt
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func (s *exactFailureStore) List(ctx context.Context, prefix string) ([]storage.Object, error) {
	s.lists.Add(1)
	return s.ObjectStore.List(ctx, prefix)
}

func TestHandoffExactReadFailureStopsBeforeFallbackAndSideEffects(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		for _, worktree := range []bool{false, true} {
			t.Run(strings.Join([]string{map[bool]string{true: "interrupted", false: "operational"}[interrupted], map[bool]string{true: "worktree", false: "launch"}[worktree]}, "/"), func(t *testing.T) {
				f := newPickerFixture(t)
				got := launchRecorder(t, &f)
				signals := make(chan os.Signal, 1)
				var failure error
				if !interrupted {
					failure = errors.New("synthetic read failure")
				}
				store := &exactFailureStore{ObjectStore: f.mem, signals: signals, failure: failure}
				phases := 0
				f.env.Interrupts = func() (<-chan os.Signal, func()) { phases++; return signals, func() {} }
				f.env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return store, nil }
				args := []string{f.archiveOnly, "--source", "archive", "--to", "claude"}
				if worktree {
					args = append(args, "--worktree")
				}
				_, stderr, code := runHandoff(t, f.env, args...)
				want := "synthetic read failure"
				if interrupted {
					want = "context canceled"
				}
				if code != 1 || !strings.Contains(stderr, want) || store.lists.Load() != 0 || phases != 1 || got.Dir != "" {
					t.Fatalf("code=%d stderr=%q lists=%d phases=%d launch=%+v", code, stderr, store.lists.Load(), phases, got)
				}
				if _, err := os.Stat(filepath.Join(f.home, handoffDir)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("handoff files created: %v", err)
				}
				path := resolvedPath(t, f.project) + "-handoff-" + f.archiveOnly[:8]
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("worktree created: %v", err)
				}
			})
		}
	}
}

func publishZeroPromptIdentity(t *testing.T, f *pickerFixture) {
	t.Helper()
	original, bundle, err := reader.RefreshAndLoad(t.Context(), f.mem, "sessions/codex/"+f.archiveOnly+"/metadata.json", reader.Limits{})
	must(t, err)
	bundle.SchemaVersion = archive.SourceSchemaVersion
	bundle.History = nil
	bundle.Capture.Harness.Name = "claude"
	bundle.NativeRecords = nil
	bundle.NativeText = nil
	bundle.SupplementalEvidence = nil
	packed, err := archive.BuildCompressedSource(bundle)
	must(t, err)
	key, err := archive.SourceObjectKey(bundle, packed.SHA256)
	must(t, err)
	must(t, f.mem.Put(t.Context(), key, packed.Bytes))
	parser, ok := f.env.agentRegistry().LookupParser("claude")
	if !ok {
		t.Fatal("missing parser")
	}
	analysis, parseErr := agentapi.Analyze(t.Context(), parser, bundle)
	m, err := archive.BuildMetadataWithAnalysis(bundle, analysis, parseErr, original.MachineID, original.StartedAt, original.MetadataDerivedAt, archive.SourceReference{Key: key, SHA256: packed.SHA256, CompressedBytes: len(packed.Bytes)}, archive.ParserInfo{Version: parser.Version()})
	must(t, err)
	if m.Counts.Turns == nil || *m.Counts.Turns != 0 {
		t.Fatalf("expected zero prompts: %+v", m.Counts)
	}
	data, err := json.Marshal(m)
	must(t, err)
	must(t, f.mem.Put(t.Context(), "sessions/claude/"+f.archiveOnly+"/metadata.json", data))
	_, err = reader.LoadSource(t.Context(), f.mem, m, reader.Limits{})
	must(t, err)
}

func TestHandoffSharedFullIDRequiresHarnessBeforePromptEligibility(t *testing.T) {
	for _, worktree := range []bool{false, true} {
		t.Run(map[bool]string{false: "launch", true: "worktree"}[worktree], func(t *testing.T) {
			f := newPickerFixture(t)
			publishZeroPromptIdentity(t, &f)
			got := launchRecorder(t, &f)
			args := []string{f.archiveOnly, "--source", "archive", "--to", "claude"}
			if worktree {
				args = append(args, "--worktree")
			}
			_, stderr, code := runHandoff(t, f.env, args...)
			if code != 1 || !strings.Contains(stderr, "--harness") || got.Dir != "" {
				t.Fatalf("code=%d stderr=%q launch=%+v", code, stderr, got)
			}
			if _, err := os.Stat(filepath.Join(f.home, handoffDir)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("handoff files created: %v", err)
			}
			if _, err := os.Stat(resolvedPath(t, f.project) + "-handoff-" + f.archiveOnly[:8]); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("worktree created: %v", err)
			}
			f.env.IsTerminal = func(any) bool { return false }
			out, stderr, code := runHandoff(t, f.env, f.archiveOnly, "--source", "archive", "--harness", "codex")
			if code != 0 || !strings.Contains(out, "Archived elsewhere") {
				t.Fatalf("explicit harness: code=%d stderr=%q", code, stderr)
			}
		})
	}
}

func TestHandoffMissingFullIDStillMatchesLocalTitle(t *testing.T) {
	f := newPickerFixture(t)
	query := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	f.addSession(t, "codex", "mention-missing", "Explain "+query, f.env.now().Add(time.Hour))
	out, stderr, code := runHandoff(t, f.env, query)
	if code != 0 || !strings.Contains(out, "Explain "+query) || !strings.Contains(out, "source: local") {
		t.Fatalf("code=%d stderr=%q output=%q", code, stderr, out)
	}
}

func TestHandoffWorktreePreservesTrailingSpacePathsAndNestedEdits(t *testing.T) {
	repo := newTestRepo(t)
	top := repo + " "
	must(t, os.Rename(repo, top))
	nested := filepath.Join(top, "sub", "inner ")
	writeTestFile(t, filepath.Join(nested, "tracked.txt"), "initial\n", 0600)
	mustGit(t, top, "add", ".")
	mustGit(t, top, "commit", "-qm", "nested directory")
	writeTestFile(t, filepath.Join(nested, "tracked.txt"), "carried\n", 0600)
	writeTestFile(t, filepath.Join(nested, "untracked.txt"), "new\n", 0600)
	var stderr bytes.Buffer
	dir, err := createHandoffWorktree(worktreeEnv(t, t.TempDir(), time.Now()), "", worktreeTarget("spaces123"), nested, &stderr)
	must(t, err)
	want := top + "-handoff-spaces12/sub/inner "
	if dir != want {
		t.Fatalf("launch directory %q, want %q", dir, want)
	}
	for name, want := range map[string]string{"tracked.txt": "carried\n", "untracked.txt": "new\n"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		must(t, err)
		if string(data) != want {
			t.Fatalf("%s=%q", name, data)
		}
	}
}

func TestHandoffLocalFullIDAmbiguityPrecedesTitleEligibility(t *testing.T) {
	f := newPickerFixture(t)
	reg, found, err := state.OpenReadOnly(f.home).LoadRegistration(f.notUploaded)
	must(t, err)
	if !found {
		t.Fatal("missing registration")
	}
	reg.Harness.Name = "claude"
	reg.NativeSessionID = "other-native-session"
	data, err := json.Marshal(reg)
	must(t, err)
	// A second physical registration can be present in an imported/recovered listing.
	must(t, os.WriteFile(filepath.Join(f.home, "registrations", "other-registration.json"), data, 0600))
	opens := takeArchiveOffline(&f.env)
	_, stderr, code := runHandoff(t, f.env, f.notUploaded, "--source", "local")
	if code != 1 || !strings.Contains(stderr, "--harness") || *opens != 0 {
		t.Fatalf("code=%d stderr=%q archive opens=%d", code, stderr, *opens)
	}
	out, stderr, code := runHandoff(t, f.env, f.notUploaded, "--source", "local", "--harness", "codex")
	if code != 0 || !strings.Contains(out, "Not uploaded yet") {
		t.Fatalf("qualified local lookup: code=%d stderr=%q", code, stderr)
	}
}

func TestHandoffExactLocalScanFailureCannotSelectArchiveTitle(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	query := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	f.addSession(t, "codex", "archive-title", "Explain "+query, f.env.now().Add(time.Hour))
	f.sync(t)
	must(t, os.WriteFile(filepath.Join(f.home, "registrations", "broken.json"), []byte("{"), 0600))
	output := filepath.Join(t.TempDir(), "handoff.md")
	_, stderr, code := runHandoff(t, f.env, query, "--output", output)
	if code != 1 || !strings.Contains(stderr, "read registration") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("output created: %v", err)
	}
	// Ordinary words retain best-effort archive search, and an explicit
	// archive source does not inspect local registrations.
	for _, args := range [][]string{{"Explain"}, {query, "--source", "archive"}} {
		out, stderr, code := runHandoff(t, f.env, args...)
		if code != 0 || !strings.Contains(out, "Explain "+query) || (len(args) > 1 && !strings.Contains(out, "source: archive")) {
			t.Fatalf("args=%v code=%d stderr=%q output=%q", args, code, stderr, out)
		}
	}
}

func TestHandoffQueryReleasesReadSignalsBeforeLaunch(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	got := launchRecorder(t, &f)
	active := atomic.Int32{}
	f.env.Interrupts = func() (<-chan os.Signal, func()) {
		active.Add(1)
		return make(chan os.Signal), func() { active.Add(-1) }
	}
	launch := f.env.LaunchHandoff
	f.env.LaunchHandoff = func(spec launchSpec, in io.Reader, out, stderr io.Writer) error {
		if count := active.Load(); count != 0 {
			t.Errorf("%d archive read signal owners still active during launch", count)
		}
		return launch(spec, in, out, stderr)
	}
	_, stderr, code := runHandoff(t, f.env, f.notUploaded, "--to", "claude")
	if code != 0 || got.Dir == "" || active.Load() != 0 {
		t.Fatalf("code=%d stderr=%q launch=%+v active=%d", code, stderr, got, active.Load())
	}
}
