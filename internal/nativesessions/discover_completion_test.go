package nativesessions

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

func TestDiscoveryReusesHeaderReservationsUntilActualBudgetIsSpent(t *testing.T) {
	t.Parallel()
	root, cwd := t.TempDir(), t.TempDir()
	limits := discoveryLimits()
	limits.TotalBytes = 2 * limits.HeaderBytes
	for i := range 9 {
		id := fmt.Sprintf("00000000-0000-0000-0000-%012d", i)
		header, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{"id": id, "cwd": cwd}})
		raw := append(append(header, '\n'), bytes.Repeat([]byte("x"), int(limits.HeaderBytes))...)
		if err := os.WriteFile(filepath.Join(root, "rollout-2026-10-01-"+id+".jsonl"), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	files := &countedFiles{}
	r, err := Discover(t.Context(), builtin.NewBuiltins(), files, []StoreRoot{{Harness: "codex", Path: root, Recursive: true, Prefix: "rollout-", Suffix: ".jsonl"}}, Scope{Directories: []string{cwd}}, limits)
	if err != nil || !r.Coverage.IdentityComplete || len(r.Candidates) != 9 || files.reads != r.Coverage.ReadBytes || files.reads > limits.TotalBytes || files.active != 0 || files.peak > limits.Workers {
		t.Fatalf("coverage=%+v candidates=%d read=%d active=%d peak=%d err=%v", r.Coverage, len(r.Candidates), files.reads, files.active, files.peak, err)
	}
}

type unavailableNativeFile struct {
	OS
	path string
}

func (f unavailableNativeFile) OpenRegular(path string) (transcriptio.File, error) {
	if path == f.path {
		return nil, os.ErrPermission
	}
	return f.OS.OpenRegular(path)
}

func TestLatestDoesNotTrustTheTimestampOfAnUnopenedHistory(t *testing.T) {
	t.Parallel()
	root, cwd := nativeFixture(t, 2)
	path, err := filepath.EvalSymlinks(filepath.Join(root.Path, "project", "session-00000.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	files := unavailableNativeFile{path: path}
	r, err := Discover(t.Context(), builtin.NewBuiltins(), files, []StoreRoot{root}, Scope{Directories: []string{cwd}}, discoveryLimits())
	if err != nil || r.Coverage.IdentityComplete || r.Coverage.LatestComplete(time.Unix(100000, 0)) || len(r.Candidates) != 1 {
		t.Fatalf("coverage=%+v candidates=%d err=%v", r.Coverage, len(r.Candidates), err)
	}
}

func TestRemovedCheckoutDoesNotMakeDiscoveryIncomplete(t *testing.T) {
	t.Parallel()
	root, cwd := nativeFixture(t, 2)
	raw, _ := json.Marshal(map[string]any{"type": "user", "sessionId": "session-00000", "cwd": filepath.Join(t.TempDir(), "removed")})
	if err := os.WriteFile(filepath.Join(root.Path, "project", "session-00000.jsonl"), append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := Discover(context.Background(), builtin.NewBuiltins(), &countedFiles{}, []StoreRoot{root}, Scope{Directories: []string{cwd}}, discoveryLimits())
	if err != nil || !r.Coverage.IdentityComplete || len(r.Candidates) != 1 {
		t.Fatalf("coverage=%+v candidates=%d err=%v", r.Coverage, len(r.Candidates), err)
	}
}

func TestLatestCompletenessRequiresStrictlyOlderVerifiedUncertainty(t *testing.T) {
	t.Parallel()
	for _, age := range []time.Duration{-time.Hour, 0, time.Hour} {
		t.Run(age.String(), func(t *testing.T) {
			t.Parallel()
			root, cwd := nativeFixture(t, 2)
			at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
			for i := range 2 {
				p := filepath.Join(root.Path, "project", fmt.Sprintf("session-%05d.jsonl", i))
				modified := at
				if i == 0 {
					if err := os.WriteFile(p, []byte("{}\n"), 0600); err != nil {
						t.Fatal(err)
					}
					modified = at.Add(age)
				}
				if err := os.Chtimes(p, modified, modified); err != nil {
					t.Fatal(err)
				}
			}
			r, err := Discover(t.Context(), builtin.NewBuiltins(), &countedFiles{}, []StoreRoot{root}, Scope{Directories: []string{cwd}}, discoveryLimits())
			if err != nil || r.Coverage.IdentityComplete || len(r.Candidates) != 1 {
				t.Fatalf("coverage=%+v err=%v", r.Coverage, err)
			}
			if got := r.Coverage.LatestComplete(at); got != (age < 0) {
				t.Fatalf("latest complete=%v age=%v coverage=%+v", got, age, r.Coverage)
			}
		})
	}
}
