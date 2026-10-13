package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

func stagedPreparedFixture(t *testing.T, request bool) (*Store, PendingPublication, *local.CollectorGuard) {
	t.Helper()
	s := newTestStore(t)
	cfg, _, err := config.Load(s.Home())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Archive.Enabled = true
	cfg.DurableStorageProtection = true
	cfg.PublicationCompositionProtection = true
	cfg.ImportStagingProtection = true
	cfg.StagedSelectionProtection = true
	if err = config.Save(s.Home(), cfg); err != nil {
		t.Fatal(err)
	}
	cfg, _, err = config.Load(s.Home())
	if err != nil {
		t.Fatal(err)
	}
	// Fresh actual planning reservation: no ordinary owner is registered first.
	id, _, err := s.EnsureArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "native-owned"})
	if err != nil {
		t.Fatal(err)
	}
	reg := registration(t)
	reg.ArchiveSessionID = id
	reg.NativeSessionID = "native-owned"
	reg.ProjectID = "project"
	reg.Harness = archive.Harness{Name: "codex"}
	reg.DestinationID = cfg.DestinationID()
	reg.Origin = archive.SessionOriginImport
	reg.ImportBatch = archive.NewImportBatch("synthetic-batch")
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reg.AdmittedAt = at
	metadataKey, err := archive.MetadataObjectKey(reg.Harness.Name, id)
	if err != nil {
		t.Fatal(err)
	}
	initialMetadata := archive.Metadata{SchemaVersion: archive.MetadataSchemaVersion, SessionID: id, NativeSessionID: reg.NativeSessionID, ProjectID: reg.ProjectID, Harness: reg.Harness, CapturedAt: at, FilterVersion: archive.FilterVersion, Adapter: archive.AdapterInfo{Name: "synthetic"}}
	metadataBytes, err := json.Marshal(initialMetadata)
	if err != nil {
		t.Fatal(err)
	}
	p := PendingPublication{MetadataKey: metadataKey, MetadataBytes: metadataBytes, Bundle: archive.SourceBundle{SchemaVersion: archive.SourceSchemaVersion, ArchiveSessionID: id, NativeSessionID: reg.NativeSessionID, ProjectID: reg.ProjectID, Capture: archive.SourceCapture{Harness: reg.Harness, AdapterName: "synthetic", FilterVersion: archive.FilterVersion, CapturedAt: at}, NativeRecords: []map[string]any{{"content": strings.Repeat("x", 96<<10)}}}, ReadyAt: at, Attempted: true, SkillEvidence: string(cfg.EffectiveSkillEvidence())}
	source, err := archive.BuildCompressedSource(p.Bundle)
	if err != nil {
		t.Fatal(err)
	}
	p.SourceBytes, p.SourceSHA256 = source.Bytes, source.SHA256
	p.SourceKey, err = archive.SourceObjectKey(p.Bundle, source.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	var metadata archive.Metadata
	if err = json.Unmarshal(p.MetadataBytes, &metadata); err != nil {
		t.Fatal(err)
	}
	metadata.FilterVersion = p.Bundle.Capture.FilterVersion
	metadata.SourceBundle = p.SourceReference()
	p.MetadataBytes, err = json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := s.SaveImportStage(t.Context(), reg, p.Bundle, cfg.EffectiveSkillEvidence())
	if err != nil {
		t.Fatal(err)
	}
	reg.StagedImportSHA256 = digest
	if _, err = s.RegisterReservedSession(agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: reg.NativeSessionID}, reg.ArchiveSessionID, func(string) archive.SessionRegistration { return reg }); err != nil {
		t.Fatal(err)
	}
	if request {
		if err = s.SaveRequest(reg.ArchiveSessionID, "backfill", p.ReadyAt); err != nil {
			t.Fatal(err)
		}
		req, _, err := s.LoadRequest(reg.ArchiveSessionID)
		if err != nil {
			t.Fatal(err)
		}
		p.RequestToken = req.Token
	}
	p, err = s.FreezeStagedSelection(t.Context(), reg.ArchiveSessionID, p, ReadyPredecessorAbsent, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SavePending(reg.ArchiveSessionID, p); err != nil {
		t.Fatal(err)
	}
	guard, err := local.LockCollectorGuard(s.Home())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { guard.Release() })
	return s, p, guard
}

func stagedSelectionFixture(t *testing.T, request bool) (*Store, PendingPublication, *local.CollectorGuard) {
	t.Helper()
	s, p, guard := stagedPreparedFixture(t, request)
	published, err := s.LoadPublishedState(p.Bundle.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := published.SaveReadyPublication(p, p.ReadyAt); err != nil {
		t.Fatal(err)
	}
	return s, p, guard
}

// Controlled valid detached-snapshot seam isolates release regressions from
// the independent SameAsBundle save bug in frozen 2e. Same source in both arms.
func stagedDetachedFixture(t *testing.T) (*Store, PendingPublication, *local.CollectorGuard) {
	t.Helper()
	s, p, guard := stagedPreparedFixture(t, true)
	published, err := s.LoadPublishedState(p.Bundle.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	next, err := published.nextBudgeted(p.Bundle, p.ReadyAt, CacheStatusPublished, "", p.MetadataBytes, nil, ptrStagedSource(p))
	if err != nil {
		t.Fatal(err)
	}
	next.LastPublished.SameAsBundle = false
	next.LastPublished.Bundle = p.Bundle
	next.LastPublished.StagedSelection = p.StagedSelection
	next.PublicationVersion = 3
	if err := published.write(next); err != nil {
		t.Fatal(err)
	}
	return s, p, guard
}

type stagedReadbackFixture struct {
	values    map[string][]byte
	corrupt   bool
	reads     int
	afterRead func(int) error
}

func (r *stagedReadbackFixture) GetLimited(ctx context.Context, key string, limit int64) ([]byte, error) {
	r.reads++
	if r.afterRead != nil {
		if err := r.afterRead(r.reads); err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b, ok := r.values[key]
	if !ok || int64(len(b)) > limit {
		return nil, errors.New("synthetic readback unavailable")
	}
	if r.corrupt {
		return []byte("wrong synthetic bytes"), nil
	}
	return append([]byte(nil), b...), nil
}

func stagedRemote(p PendingPublication) *stagedReadbackFixture {
	return &stagedReadbackFixture{values: map[string][]byte{p.MetadataKey: p.MetadataBytes, p.SourceKey: p.SourceBytes}}
}

func TestStagedSelectionExactReleaseAndRestart(t *testing.T) {
	s, p, guard := stagedSelectionFixture(t, true)
	id := p.Bundle.ArchiveSessionID
	if err := s.RemoveImportStage(id); !errors.Is(err, ErrReadySelectionFrozen) {
		t.Fatalf("generic cleanup bypass: %v", err)
	}
	remote := stagedRemote(p)
	if err := s.VerifyAndBeginStagedRelease(t.Context(), id, remote, guard); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.LoadRequest(id); err != nil || found {
		t.Fatalf("covered request: %v %v", found, err)
	}
	if _, found, err := s.LoadPending(id); err != nil || found {
		t.Fatalf("pending: %v %v", found, err)
	}
	reg, _, err := s.LoadRegistration(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.LoadImportStage(t.Context(), reg); err != nil || found {
		t.Fatalf("stage: %v %v", found, err)
	}
	reads := remote.reads
	if err := OpenReadOnly(s.Home()).VerifyAndBeginStagedRelease(t.Context(), id, remote, guard); err != nil {
		t.Fatal(err)
	}
	if remote.reads != reads {
		t.Fatal("completed release replayed remote reads")
	}
}

func TestStagedSelectionReadbackAndNewerTokenRetainEvidence(t *testing.T) {
	for _, mode := range []string{"wrong-readback", "newer-token", "missing-guard"} {
		t.Run(mode, func(t *testing.T) {
			s, p, guard := stagedSelectionFixture(t, true)
			id := p.Bundle.ArchiveSessionID
			remote := stagedRemote(p)
			if mode == "wrong-readback" {
				remote.corrupt = true
			}
			if mode == "newer-token" {
				if err := s.SaveRequest(id, "hook", p.ReadyAt); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "missing-guard" {
				guard = nil
			}
			before, err := os.ReadFile(s.requestPath(id))
			if err != nil {
				t.Fatal(err)
			}
			if err = s.VerifyAndBeginStagedRelease(t.Context(), id, remote, guard); err == nil {
				t.Fatal("unsupported release accepted")
			}
			after, err := os.ReadFile(s.requestPath(id))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("request changed", err)
			}
			reg, _, err := s.LoadRegistration(id)
			if err != nil {
				t.Fatal(err)
			}
			if _, found, err := s.LoadImportStage(t.Context(), reg); err != nil || !found {
				t.Fatalf("stage lost: %v %v", found, err)
			}
		})
	}
}

func TestStagedSelectionMandatoryCodecAndClosedAuthority(t *testing.T) {
	_, p, _ := stagedSelectionFixture(t, false)
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(string) string{
		func(s string) string { return strings.Replace(s, `"journal_version":3`, `"journal_version":2`, 1) },
		func(s string) string {
			return strings.Replace(s, `"stage_sha256":`, `"stage_sha256":null,"stage_sha256":`, 1)
		},
		func(s string) string { return strings.Replace(s, `"had_request":false`, `"had_request":null`, 1) },
		func(s string) string { return strings.Replace(s, `"stage_sha256":`, `"Stage_SHA256":`, 1) },
	} {
		var decoded PendingPublication
		if err := json.Unmarshal([]byte(mutate(string(raw))), &decoded); err == nil {
			t.Fatal("damaged staged authority accepted")
		}
	}
}

func TestStagedSelectionLegacyExactRemovalCannotBypassReceipt(t *testing.T) {
	s, p, _ := stagedSelectionFixture(t, true)
	if err := s.RemoveExactLegacyPending(p.Bundle.ArchiveSessionID, p); err == nil {
		t.Fatal("legacy exact removal accepted typed3")
	}
	if _, found, err := s.LoadPending(p.Bundle.ArchiveSessionID); err != nil || !found {
		t.Fatalf("typed pending lost: %v %v", found, err)
	}
}

func TestStagedFloorPreservesOrdinarySelection2(t *testing.T) {
	s, p := readySelectionFixture(t)
	cfg, _, err := config.Load(s.Home())
	if err != nil {
		t.Fatal(err)
	}
	cfg.ImportStagingProtection = true
	cfg.StagedSelectionProtection = true
	if err = config.Save(s.Home(), cfg); err != nil {
		t.Fatal(err)
	}
	if err = s.SavePending(p.Bundle.ArchiveSessionID, p); err != nil {
		t.Fatal(err)
	}
	loaded, found, err := s.LoadPending(p.Bundle.ArchiveSessionID)
	if err != nil || !found || loaded.Selection == nil || loaded.JournalVersion != 2 || *loaded.Selection != *p.Selection {
		t.Fatal("ordinary pending2 changed", err)
	}
	published, err := s.LoadPublishedState(p.Bundle.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err = published.SaveReadyPublication(p, p.ReadyAt); err != nil {
		t.Fatal(err)
	}
	loadedPublished, err := OpenReadOnly(s.Home()).LoadPublishedState(p.Bundle.ArchiveSessionID)
	if err != nil || loadedPublished.state.PublicationVersion != 2 || loadedPublished.state.LastPublished.Selection == nil || *loadedPublished.state.LastPublished.Selection != *p.Selection {
		t.Fatal("ordinary published2 changed", err)
	}
}

func TestStagedReleaseRechecksTokenAfterVerifiedReadback(t *testing.T) {
	s, p, guard := stagedSelectionFixture(t, true)
	id := p.Bundle.ArchiveSessionID
	remote := stagedRemote(p)
	remote.afterRead = func(n int) error {
		if n == 3 {
			return s.SaveRequest(id, "hook", p.ReadyAt)
		}
		return nil
	}
	if err := s.VerifyAndBeginStagedRelease(t.Context(), id, remote, guard); !errors.Is(err, ErrReadySelectionFrozen) {
		t.Fatalf("new final token: %v", err)
	}
	request, found, err := s.LoadRequest(id)
	if err != nil || !found || request.Token == p.RequestToken {
		t.Fatal("new token not preserved", err)
	}
	reg, _, err := s.LoadRegistration(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.LoadImportStage(t.Context(), reg); err != nil || !found {
		t.Fatal("stage removed before final token check", err)
	}
}

type stagedCatalogRefusalFixture struct{ *stagedReadbackFixture }

func (*stagedCatalogRefusalFixture) CatalogMetadataAuthority() bool { return true }

func TestStagedReleaseRefusesCatalogBeforeAnyRead(t *testing.T) {
	s, p, guard := stagedSelectionFixture(t, true)
	remote := &stagedCatalogRefusalFixture{stagedRemote(p)}
	if err := s.VerifyAndBeginStagedRelease(t.Context(), p.Bundle.ArchiveSessionID, remote, guard); !errors.Is(err, ErrReadySelectionFrozen) {
		t.Fatalf("catalog accepted: %v", err)
	}
	if remote.reads != 0 {
		t.Fatal("catalog refusal performed remote reads")
	}
	if _, found, err := s.LoadPending(p.Bundle.ArchiveSessionID); err != nil || !found {
		t.Fatal("catalog refusal lost pending", err)
	}
}

func ptrStagedSource(p PendingPublication) *archive.SourceReference {
	ref := p.SourceReference()
	return &ref
}
