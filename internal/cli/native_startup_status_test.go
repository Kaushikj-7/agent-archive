package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/capture"
)

func TestMuseStartupStatusDeduplicatesAndBoundsPrivateQueue(t *testing.T) {
	t.Parallel()
	f := newMuseRootCaptureFixture(t)
	const id = "00000000-0000-7000-8000-000000000001"
	payload, err := json.Marshal(map[string]any{"hook_event_name": "SessionStart", "session_id": id, "cwd": f.project, "transcript_path": nil, "source": "startup"})
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if code := runHookCommand([]string{"--harness", "muse"}, bytes.NewReader(payload), &stderr, f.env); code != 0 || stderr.Len() != 0 {
		t.Fatalf("startup: %d %s", code, &stderr)
	}
	directory := filepath.Join(f.home, "admission-intents")
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 {
		t.Fatalf("queue: %v %v", entries, err)
	}
	raw, err := os.ReadFile(filepath.Join(directory, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "duplicate.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	observation, err := capture.ReadNativeStartupObservation(t.Context(), f.home, f.cfg, nil, f.observed)
	if err != nil || !observation.Complete || observation.Pending != 1 || !observation.ObservedAt.Equal(f.observed) {
		t.Fatalf("duplicate inflated status: %+v %v", observation, err)
	}
	output, err := json.Marshal(observation)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(output, []byte(id)) || bytes.Contains(output, []byte(f.project)) {
		t.Fatal("private queue identity escaped status")
	}
	future := bytes.Replace(raw, []byte(`"version":2`), []byte(`"version":999`), 1)
	path := filepath.Join(directory, "future.json")
	if err := os.WriteFile(path, future, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "bounded.json"), bytes.Repeat([]byte(" "), (512<<10)+1), 0600); err != nil {
		t.Fatal(err)
	}
	observation, err = capture.ReadNativeStartupObservation(t.Context(), f.home, f.cfg, nil, f.observed)
	if err != nil || observation.Complete || observation.Pending != 1 {
		t.Fatalf("bounded or future record inflated status: %+v %v", observation, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, future) {
		t.Fatalf("status changed future record: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := capture.ReadNativeStartupObservation(ctx, f.home, f.cfg, nil, f.observed); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}
