package collector

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// Rejected work still participates in complete loadWork diagnostic bookkeeping.
// A wrong-shaped request is deliberately not quarantinable and must survive byte-exact.
func TestRejectedRequestDiagnosticSurvivesExecutionFilter(t *testing.T) {
	s := newTestStore(t)
	cfg := config.Config{}
	reg := registration(t, writeTranscript(t, t.TempDir(), "rejected.jsonl", codexTranscript))
	reg.ArchiveSessionID = "rejected"
	reg.DestinationID = "previous-destination"
	if cfg.AcceptSession(reg) {
		t.Fatal("rejection control failed")
	}
	if err := s.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	request := filepath.Join(s.Home(), "requests", "rejected.json")
	raw := []byte(`{"archive_session_id":["wrong-shape"]}`)
	if err := os.WriteFile(request, raw, 0600); err != nil {
		t.Fatal(err)
	}
	registration := filepath.Join(s.Home(), "registrations", "rejected.json")
	before, err := os.ReadFile(registration)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), s, storagetest.NewMemoryStore(), Options{
		MachineID: "synthetic", Sources: refuseNativeSources{t}, AcceptSession: cfg.AcceptSession,
	})
	if err != nil || result.Errors["rejected"] == nil || errors.Is(result.Errors["rejected"], state.ErrQuarantined) || len(result.Errors) != 1 || result.Scanned != 0 || len(result.Unreached) != 0 {
		t.Fatalf("diagnostic result: %+v %v", result, err)
	}
	status, err := s.LoadStatus()
	// A readable registration with a wrong-shaped request produces an error, but no accepted pending execution work.
	if err != nil || status.PendingCount != 0 || status.LastError == "" {
		t.Fatalf("diagnostic status: %+v %v", status, err)
	}
	for path, want := range map[string][]byte{request: raw, registration: before} {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("diagnostic changed %s: %v", path, err)
		}
	}
}

// The full durable census must retain orphan evidence even when all registered work is rejected.
func TestRejectedExecutionRetainsDurableOrphan(t *testing.T) {
	s := newTestStore(t)
	cfg := config.Config{}
	reg := registration(t, writeTranscript(t, t.TempDir(), "rejected.jsonl", codexTranscript))
	reg.ArchiveSessionID, reg.DestinationID = "rejected", "previous-destination"
	if cfg.AcceptSession(reg) {
		t.Fatal("rejection control failed")
	}
	if err := s.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveRequest("rejected", "hook", time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	request := filepath.Join(s.Home(), "requests", "rejected.json")
	before, err := os.ReadFile(request)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("synthetic-frozen-orphan")
	ref := archive.SourceReference{SHA256: storage.SHA256Hex(data), Key: "synthetic", CompressedBytes: len(data)}
	stage, err := s.StagePendingSource("orphan", ref, data)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), s, storagetest.NewMemoryStore(), Options{MachineID: "synthetic", Sources: refuseNativeSources{t}, AcceptSession: cfg.AcceptSession})
	if err != nil || len(result.Errors) != 1 || !errors.Is(result.Errors["orphan"], state.ErrDurableStorageRecovery) || result.Scanned != 0 || len(result.Unreached) != 0 {
		t.Fatalf("durable result: %+v %v", result, err)
	}
	status, err := s.LoadStatus()
	if err != nil || status.PendingCount != 1 {
		t.Fatalf("durable status: %+v %v", status, err)
	}
	got, err := s.ReadPendingSource("orphan", stage)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("durable original lost: %v", err)
	}
	after, err := os.ReadFile(request)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("rejected request changed: %v", err)
	}
	regs, err := s.LoadRegistrations()
	if err != nil || len(regs) != 1 || regs[0].ArchiveSessionID != "rejected" {
		t.Fatalf("durable census: %+v %v", regs, err)
	}
}
