package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
)

func TestBackfillPublishedSelectionRemainsPendingUntilLocalSettlement(t *testing.T) {
	f, bucket := newImportFixture(t)
	cfg := mustLoadConfig(t, f.data)
	cfg.DurableStorageProtection, cfg.PublicationCompositionProtection = true, true
	cfg.ImportStagingProtection, cfg.StagedSelectionProtection = true, true
	must(t, config.Save(f.data, cfg))
	out, stderr, code := f.importRun(t, nil, false, "--yes", "--background", "--harness", "claude", "--project", filepath.Join(f.userHome, "agent-archive"), "--until", "2026-09-20")
	if code != 0 {
		t.Fatal("prepare actual typed selection", code, stderr, out)
	}
	store, err := state.Open(f.data)
	must(t, err)
	regs, err := store.LoadRegistrations()
	must(t, err)
	var reg archive.SessionRegistration
	for _, candidate := range regs {
		if candidate.InBatch(firstImport) {
			reg = candidate
		}
	}
	stage, found, err := store.LoadImportStage(t.Context(), reg)
	must(t, err)
	if !found || !stage.TypedSelection {
		t.Fatal("actual confirmed selection was not typed")
	}
	cfg = mustLoadConfig(t, f.data)
	packed, err := archive.BuildCompressedSource(stage.Bundle)
	must(t, err)
	key, err := archive.SourceObjectKey(stage.Bundle, packed.SHA256)
	must(t, err)
	ref := archive.SourceReference{Key: key, SHA256: packed.SHA256, CompressedBytes: len(packed.Bytes)}
	parser, ok := f.env.agentRegistry().LookupParser(reg.Harness.Name)
	if !ok {
		t.Fatal("missing builtin parser")
	}
	analysis, parseErr := agentapi.Analyze(t.Context(), parser, stage.Bundle)
	metadata, err := archive.BuildMetadataWithAnalysis(stage.Bundle, analysis, parseErr, cfg.MachineID, reg.SessionStartedAt, reg.AdmittedAt, ref, archive.ParserInfo{Version: parser.Version()})
	must(t, err)
	metadata.ApplyRegistrationProvenance(reg)
	metadataBytes, err := json.Marshal(metadata)
	must(t, err)
	metadataKey, err := archive.MetadataObjectKey(reg.Harness.Name, reg.ArchiveSessionID)
	must(t, err)
	request, found, err := store.LoadRequest(reg.ArchiveSessionID)
	must(t, err)
	if !found {
		t.Fatal("selected owner lost its covered request")
	}
	pending := state.PendingPublication{Bundle: stage.Bundle, SourceKey: key, SourceSHA256: packed.SHA256, SourceBytes: packed.Bytes, MetadataKey: metadataKey, MetadataBytes: metadataBytes, SkillEvidence: string(cfg.EffectiveSkillEvidence()), RequestToken: request.Token, ReadyAt: reg.AdmittedAt, Attempted: true}
	pending, err = store.FreezeStagedSelection(t.Context(), reg.ArchiveSessionID, pending, state.ReadyPredecessorAbsent, nil)
	must(t, err)
	must(t, store.SavePending(reg.ArchiveSessionID, pending))
	must(t, bucket.Put(t.Context(), key, packed.Bytes))
	must(t, bucket.Put(t.Context(), metadataKey, metadataBytes))
	published, err := store.LoadPublishedState(reg.ArchiveSessionID)
	must(t, err)
	must(t, published.SaveReadyPublication(pending, reg.AdmittedAt))
	// This is the existing durable boundary after remote commit and local
	// publication recording, before exact request/pending/stage settlement.
	beforeRemote := bucketSnapshot(t, bucket)
	stagePath := filepath.Join(f.data, "sessions", reg.ArchiveSessionID, "import-stage", "stage.json")
	pendingPath := filepath.Join(f.data, "pending", reg.ArchiveSessionID+".json")
	beforeStage, err := os.ReadFile(stagePath)
	must(t, err)
	beforePending, err := os.ReadFile(pendingPath)
	must(t, err)
	allowedCfg := cfg
	cfg.Archive.Projects = append([]archive.ProjectActivation(nil), cfg.Archive.Projects...)
	for i := range cfg.Archive.Projects {
		cfg.Archive.Projects[i].Included = false
	}
	must(t, config.Save(f.data, cfg))
	cfg = mustLoadConfig(t, f.data)
	owed, err := store.Outstanding(reg, true)
	must(t, err)
	if cfg.AcceptSession(reg) || !owed.Published || !owed.Requested || !owed.Upload {
		t.Fatal("missing actual policy-held postcommit obligation", owed)
	}
	u := upload{env: f.env, home: f.data, batch: firstImport}
	must(t, u.refresh())
	if len(u.pending) != 1 {
		t.Error("published summary hid unsettled confirmed owner", len(u.pending))
	}
	batches, err := backfill.LoadBatches(f.data)
	must(t, err)
	history, err := batchUploadState(store, cfg, regs, batches[0], true)
	must(t, err)
	if !strings.Contains(history, "waiting") || strings.HasPrefix(history, "uploaded") {
		t.Error("history hid unsettled policy-held owner", history)
	}
	guard, err := local.LockCollectorGuard(f.data)
	must(t, err)
	defer guard.Release()
	if err := store.VerifyAndBeginStagedRelease(t.Context(), reg.ArchiveSessionID, bucket, guard); !errors.Is(err, state.ErrReadySelectionFrozen) {
		t.Fatal("policy hold did not preserve existing release authority", err)
	}
	afterStage, err := os.ReadFile(stagePath)
	must(t, err)
	afterPending, err := os.ReadFile(pendingPath)
	must(t, err)
	currentRequest, found, err := store.LoadRequest(reg.ArchiveSessionID)
	must(t, err)
	if !found || currentRequest.Token != request.Token || !bytes.Equal(beforeStage, afterStage) || !bytes.Equal(beforePending, afterPending) || beforeRemote != bucketSnapshot(t, bucket) {
		t.Fatal("observation or refused release changed retained owner or remote bytes")
	}
	must(t, config.Save(f.data, allowedCfg))
	must(t, store.VerifyAndBeginStagedRelease(t.Context(), reg.ArchiveSessionID, bucket, guard))
	must(t, u.refresh())
	if len(u.pending) != 0 || beforeRemote != bucketSnapshot(t, bucket) {
		t.Fatal("exact local settlement did not complete without remote changes")
	}
	_, err = reader.LoadSource(t.Context(), bucket, metadata, reader.Limits{})
	must(t, err)
	// After exact first-stage settlement, ordinary ongoing requests retain
	// their existing configuration filtering rather than reopening this import.
	must(t, store.SaveRequest(reg.ArchiveSessionID, "synthetic-ongoing-work", reg.AdmittedAt))
	must(t, config.Save(f.data, cfg))
	must(t, u.refresh())
	if len(u.pending) != 0 {
		t.Fatal("settled ongoing owner bypassed current configuration")
	}
}
