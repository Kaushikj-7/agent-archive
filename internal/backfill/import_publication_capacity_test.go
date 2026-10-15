package backfill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/discovery"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// The canonical cohort exceeds the unchanged 1 GiB double-charged quota.
// Registration must drain verified publications before saturation and continue.
// Sequential to bound this explicit >512 MiB fixture's disk/CPU use.
func TestImportCapacityPublishesCanonicalEvidenceBeforeContinuingAdmission(t *testing.T) {
	if testing.Short() {
		t.Skip("real fixed-quota canonical publication regression")
	}
	verifyCanonicalPublicationCapacity(t, false)
}

// Live lookup preserves legacy nil binding while real selected evidence drains.
func TestImportCapacityPublishesLegacyLiveLocatorEvidence(t *testing.T) {
	if testing.Short() {
		t.Skip("real fixed-quota live-locator publication regression")
	}
	verifyCanonicalPublicationCapacity(t, true)
}

func verifyCanonicalPublicationCapacity(t *testing.T, live bool) {
	t.Helper()
	r, c, cfg := stagingFixture(t)
	nativeHome := filepath.Join(c.ProjectRoot, ".codex")
	nativeDir := filepath.Join(nativeHome, "sessions")
	if live {
		if err := os.MkdirAll(nativeDir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	const count = 24
	const retained = 24 << 20
	candidates := make([]Candidate, count)
	for i := range candidates {
		next := c
		next.Harness = "codex"
		next.NativeSessionID = fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1)
		next.TranscriptPath = filepath.Join(c.ProjectRoot, "rollout-"+next.NativeSessionID+".jsonl")
		if live {
			next.NativeHome = nativeHome
			next.TranscriptPath = filepath.Join(nativeDir, "rollout-"+next.NativeSessionID+".jsonl")
		}
		file, err := os.Create(next.TranscriptPath)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = file.WriteString(codexTranscript(next.NativeSessionID, next.NativeSessionID, next.ProjectRoot, next.StartedAt)); err != nil {
			t.Fatal(errors.Join(err, file.Close()))
		}
		row := `{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + strings.Repeat("a", 32<<10) + `"}]}}` + "\n"
		for range retained / (32 << 10) {
			if _, err = file.WriteString(row); err != nil {
				t.Fatal(errors.Join(err, file.Close()))
			}
		}
		if err = file.Close(); err != nil {
			t.Fatal(err)
		}
		candidates[i] = next
	}
	var lookup *discovery.CodexRolloutLookup
	if live {
		var err error
		lookup, err = discovery.NewCodexRolloutLookup(t.Context(), state.OpenReadOnly(r.Home), []string{nativeHome})
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := lookup.Close(); err != nil {
				t.Error(err)
			}
		}()
		for _, candidate := range candidates {
			if _, err := lookup.Thread(t.Context(), candidate.NativeSessionID); err != nil && t.Context().Err() != nil {
				t.Fatal(err)
			}
		}
		for {
			health, err := lookup.ObserveReadOnly(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if !health.Pending {
				break
			}
		}
		r.CodexRollouts = lookup
		r.ConfiguredCodexHomes = []string{nativeHome}
	}
	var canonicalBytes int64
	staged := map[string]string{}
	r.AfterStage = func(id string) error {
		stages, _, err := r.Store.ImportStages(t.Context(), "", 50)
		if err != nil {
			return err
		}
		for _, stage := range stages {
			if stage.Registration.ArchiveSessionID != id {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(r.Home, "sessions", id, "import-stage", "stage.json"))
			if err != nil {
				return err
			}
			selected, found, err := r.Store.LoadImportStage(t.Context(), stage.Registration)
			if err != nil || !found {
				return fmt.Errorf("load complete selected stage: %w", err)
			}
			if live && (selected.Registration.CodexBinding != nil || selected.TypedSelection || selected.Bundle.History != nil || len(selected.Children) != 0 || len(selected.Preserved) != 0) {
				return fmt.Errorf("live selection escaped legacy ordinary envelope")
			}
			canonical, err := json.Marshal(selected)
			if err != nil {
				return err
			}
			if string(raw) != string(canonical)+"\n" {
				return fmt.Errorf("noncanonical selected stage")
			}
			canonicalBytes += int64(len(raw))
			staged[id] = stage.SHA256
			// The drain and final pass must publish the retained selection itself.
			if live {
				return nil
			}
			return os.Remove(stage.Registration.TranscriptPath)
		}
		return fmt.Errorf("selected stage missing")
	}
	remote := storagetest.NewMemoryStore()
	var homes []string
	if live {
		homes = []string{nativeHome}
	}
	opts := collector.Options{Sources: testSources, Parsers: testSources, MachineID: "synthetic", Now: func() time.Time { return fixedNow.Add(time.Minute) }, AcceptSession: cfg.AcceptSession, CodexRollouts: r.CodexRollouts, ConfiguredCodexHomes: homes}
	drains, published := 0, 0
	collect := func(ctx context.Context) error {
		reopened, err := state.Open(r.Home)
		if err != nil {
			return err
		}
		result, err := collector.Run(ctx, reopened, remote, opts)
		defer result.CloseStagedResults()
		if err != nil {
			return err
		}
		if len(result.Errors) > 0 {
			return fmt.Errorf("publication errors: %v", result.Errors)
		}
		published += len(result.Published)
		if len(result.Published) == 0 {
			return state.ErrDurableStorageCapacity
		}
		return nil
	}
	r.OnCapacity = func(ctx context.Context) error { drains++; return collect(ctx) }
	result, err := r.Run(candidates)
	if err != nil || len(result.Sessions) != count {
		t.Fatalf("canonical admission: sessions=%d drains=%d err=%v", len(result.Sessions), drains, err)
	}
	if drains == 0 {
		t.Fatal("cohort never exercised actual capacity publication")
	}
	if canonicalBytes <= 1<<29 {
		t.Fatalf("cohort did not exceed original charged quota: wire=%d", canonicalBytes)
	}
	if err = collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	if published != count || len(staged) != count {
		t.Fatalf("published %d staged %d want %d", published, len(staged), count)
	}
	reopened, err := state.Open(r.Home)
	if err != nil {
		t.Fatal(err)
	}
	regs, err := reopened.LoadRegistrations()
	if err != nil || len(regs) != count {
		t.Fatalf("final registrations %d %v", len(regs), err)
	}
	for _, reg := range regs {
		if live && reg.CodexBinding != nil {
			t.Fatal("legacy binding was fabricated")
		}
		if reg.StagedImportSHA256 != staged[reg.ArchiveSessionID] {
			t.Fatal("selected digest changed")
		}
		local, err := reopened.LoadPublishedState(reg.ArchiveSessionID)
		if err != nil || !local.Summary().Published {
			t.Fatalf("unverified local publication: %v", err)
		}
		key, err := archive.MetadataObjectKey(reg.Harness.Name, reg.ArchiveSessionID)
		if err != nil {
			t.Fatal(err)
		}
		metadata, err := reader.ReadMetadata(t.Context(), remote, key)
		if err != nil {
			t.Fatal(err)
		}
		bundle, err := reader.LoadSource(t.Context(), remote, metadata, reader.Limits{})
		if err != nil {
			t.Fatal(err)
		}
		if bundle.NativeSessionID != reg.NativeSessionID {
			t.Fatal("published wrong selected identity")
		}
		bytes := 0
		for _, row := range bundle.NativeRecords {
			payload, _ := row["payload"].(map[string]any)
			if payload["role"] != "assistant" {
				continue
			}
			content, _ := payload["content"].([]any)
			for _, value := range content {
				part, _ := value.(map[string]any)
				text, _ := part["text"].(string)
				if text != strings.Repeat("a", 32<<10) {
					t.Fatal("published retained content differs")
				}
				bytes += len(text)
			}
		}
		if bytes != retained {
			t.Fatalf("published retained bytes %d want %d", bytes, retained)
		}
		if _, found, err := reopened.LoadImportStage(t.Context(), reg); err != nil || found {
			t.Fatalf("verified stage not released: %v", err)
		}
		if _, found, err := reopened.LoadPending(reg.ArchiveSessionID); err != nil || found {
			t.Fatalf("verified pending not released: %v", err)
		}
	}
	t.Logf("canonical wire=%d drains=%d published=%d", canonicalBytes, drains, published)
}
