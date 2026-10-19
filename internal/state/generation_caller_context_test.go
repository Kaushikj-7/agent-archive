package state

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestGenerationRecoveryEmptyRootsHonorsCallerCancellation(t *testing.T) {
	s := newTestStore(t)
	if err := s.ResumeGenerationRecoveries(t.Context()); err != nil {
		t.Fatal("fresh empty recovery positive control", err)
	}
	t.Log("fresh empty recovery positive control PASS")
	before, err := os.ReadFile(filepath.Join(s.home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	recoveryErr := s.ResumeGenerationRecoveries(ctx)
	after, err := os.ReadFile(filepath.Join(s.home, "config.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("canceled recovery changed configuration", err)
	}
	if _, err := os.Stat(filepath.Join(s.home, generationRecoveryDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("canceled empty recovery created work", err)
	}
	if !errors.Is(recoveryErr, context.Canceled) {
		t.Fatal("empty recovery ignored caller cancellation", recoveryErr)
	}
}
