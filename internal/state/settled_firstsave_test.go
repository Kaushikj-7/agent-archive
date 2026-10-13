package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/cursor"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/nativefacts"
)

type settledStateCatalogHost struct{ db *sql.DB }

func (h settledStateCatalogHost) Query(ctx context.Context, query string, visit func(agentapi.DatabaseRecord) error) (err error) {
	rows, err := h.db.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var record agentapi.DatabaseRecord
		if err = rows.Scan(&record.Key, &record.Value); err != nil {
			return err
		}
		if err = visit(record); err != nil {
			return err
		}
	}
	return rows.Err()
}

func settledStateCaptureFixture(t *testing.T) (*Store, archive.SessionRegistration, *nativefacts.CaptureRequest, *cursor.SettledImportEvidence, *agentapi.NativeReadBudget) {
	t.Helper()
	s := newTestStore(t)
	cfg, _, err := config.Load(s.Home())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Archive.Enabled = true
	cfg.ImportedHarnesses = []string{"cursor"}
	cfg.DurableStorageProtection = true
	cfg.PublicationCompositionProtection = true
	cfg.ImportStagingProtection = true
	cfg.StagedSelectionProtection = true
	cfg.SettledCursorSelectionProtection = true
	if err = config.Save(s.Home(), cfg); err != nil {
		t.Fatal(err)
	}
	cfg, _, err = config.Load(s.Home())
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reg := registration(t)
	reg.Harness = archive.Harness{Name: archive.HarnessCursor}
	reg.NativeSessionID = "settled-state-selected"
	reg.SourceKind = archive.SourceKindCursorSQLite
	reg.SourceKey = reg.NativeSessionID
	reg.TranscriptPath = ""
	reg.SessionStartedAt = at
	reg.RegisteredAt = at.Add(time.Hour)
	reg.AdmittedAt = reg.RegisteredAt
	reg.Origin = archive.SessionOriginImport
	reg.ImportBatch = archive.NewImportBatch("settled-state-batch")
	reg.DestinationID = cfg.DestinationID()
	reg.ArchiveSessionID, _, err = s.EnsureArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.Cursor, NativeID: reg.NativeSessionID})
	if err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(t.TempDir(), "state.vscdb")
	db, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{`CREATE TABLE ItemTable (key TEXT UNIQUE, value BLOB)`, `CREATE TABLE cursorDiskKV (key TEXT UNIQUE, value BLOB)`} {
		if _, err = db.ExecContext(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
	composer := map[string]any{"_v": 18, "composerId": reg.NativeSessionID, "createdAt": at.UnixMilli(), "lastUpdatedAt": at.Add(time.Second).UnixMilli(), "workspaceIdentifier": map[string]any{"id": "workspace", "uri": "file://" + reg.ProjectRoot}, "status": "completed", "fullConversationHeadersOnly": []map[string]any{{"bubbleId": "question", "type": 1}, {"bubbleId": "answer", "type": 2}}}
	values := map[string]any{"composerData:" + reg.NativeSessionID: composer, "bubbleId:" + reg.NativeSessionID + ":question": map[string]any{"_v": 3, "bubbleId": "question", "type": 1, "text": "question", "createdAt": at.UnixMilli()}, "bubbleId:" + reg.NativeSessionID + ":answer": map[string]any{"_v": 3, "bubbleId": "answer", "type": 2, "text": "answer", "createdAt": at.Add(time.Second).UnixMilli()}}
	for key, value := range values {
		raw, e := json.Marshal(value)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = db.ExecContext(t.Context(), `INSERT INTO cursorDiskKV (key,value) VALUES (?,?)`, key, raw); e != nil {
			t.Fatal(e)
		}
	}
	catalog, err := (cursor.DatabaseCatalogInspector{}).InspectCatalog(t.Context(), settledStateCatalogHost{db: db})
	if err != nil || len(catalog.Chats) != 1 {
		t.Fatal(catalog, err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	budget := agentapi.NewNativeReadBudget(128 << 20)
	request, err := nativefacts.FreezeCaptureRequest(t.Context(), reg, catalog.Chats[0], database, reg.AdmittedAt, nativefacts.CapturePolicy{FilterVersion: archive.FilterVersion, SkillEvidence: nativefacts.CaptureSkillMode(cfg.EffectiveSkillEvidence())}, budget)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(request.Close)
	evidence, err := cursor.CaptureSettledRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := evidence.Close(); err != nil {
			t.Error(err)
		}
	})
	return s, reg, request, evidence, budget
}

func TestSettledFirstSaveRechecksAfterSyncedPreparation(t *testing.T) {
	s, reg, request, evidence, budget := settledStateCaptureFixture(t)
	read, closeRead := s.WithReadBudget(t.Context(), budget)
	defer closeRead()
	hookRan := false
	var ordinaryRaw, requestRaw []byte
	read.onWriteSync = func() {
		read.onWriteSync = nil
		hookRan = true
		ordinary := reg
		ordinary.Origin = archive.SessionOriginHook
		ordinary.ImportBatch = archive.NewImportBatch("")
		if _, err := s.RegisterReservedSession(agentmeta.SessionKey{Agent: agentmeta.Cursor, NativeID: reg.NativeSessionID}, reg.ArchiveSessionID, func(string) archive.SessionRegistration { return ordinary }); err != nil {
			t.Fatal(err)
		}
		if err := s.SaveRequest(reg.ArchiveSessionID, "hook", reg.AdmittedAt); err != nil {
			t.Fatal(err)
		}
		var err error
		ordinaryRaw, err = os.ReadFile(s.registrationPath(reg.ArchiveSessionID))
		if err != nil {
			t.Fatal(err)
		}
		requestRaw, err = os.ReadFile(s.requestPath(reg.ArchiveSessionID))
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := read.SaveSettledCursorImportStage(t.Context(), reg, request, evidence); !errors.Is(err, ErrReadySelectionFrozen) {
		t.Fatalf("overtaken first-save accepted: %v", err)
	}
	if !hookRan {
		t.Fatal("actual synced-preparation boundary not reached")
	}
	after, err := os.ReadFile(s.registrationPath(reg.ArchiveSessionID))
	if err != nil || string(after) != string(ordinaryRaw) {
		t.Fatalf("overtaken registration changed: %v", err)
	}
	after, err = os.ReadFile(s.requestPath(reg.ArchiveSessionID))
	if err != nil || string(after) != string(requestRaw) {
		t.Fatalf("overtaken request changed: %v", err)
	}
	if _, err = os.Lstat(filepath.Join(s.home, importStageRelative(reg.ArchiveSessionID))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("overtaken first-save left stage: %v", err)
	}
}

func TestSettledCheckedRenameKeepsRequestLock(t *testing.T) {
	s, reg, _, _, _ := settledStateCaptureFixture(t)
	cfg, _, err := config.Load(s.Home())
	if err != nil {
		t.Fatal(err)
	}
	prepared := make(chan struct{})
	completed := make(chan error, 1)
	ordinary := reg
	ordinary.Origin = archive.SessionOriginHook
	ordinary.ImportBatch = archive.NewImportBatch("")
	writer := *s
	writer.onWriteSync = func() { writer.onWriteSync = nil; close(prepared) }
	renameObserved := false
	err = config.WithImportStaging(s.Home(), func(g config.DurableStorageGuard) error {
		value := struct {
			Boundary string `json:"boundary"`
		}{Boundary: "rename-only lock control"}
		return s.writeDurableGuardChecked(t.Context(), g, importStageRelative(reg.ArchiveSessionID), value, func(rename func() error) error {
			return s.commitFreshSettledStage(t.Context(), g, reg, cfg.EffectiveSkillEvidence(), func() error {
				go func() {
					_, err := writer.RegisterReservedSession(agentmeta.SessionKey{Agent: agentmeta.Cursor, NativeID: reg.NativeSessionID}, reg.ArchiveSessionID, func(string) archive.SessionRegistration { return ordinary })
					completed <- err
				}()
				select {
				case <-prepared:
				case <-time.After(5 * time.Second):
					return errors.New("ordinary writer did not prepare outside request lock")
				}
				if _, found, err := s.LoadRegistration(reg.ArchiveSessionID); err != nil || found {
					return errors.Join(errors.New("ordinary writer committed between freshness check and rename"), err)
				}
				renameObserved = true
				return rename()
			})
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !renameObserved {
		t.Fatal("checked rename boundary not reached")
	}
	select {
	case err = <-completed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ordinary writer blocked after rename lock release")
	}
	if _, found, err := s.LoadRegistration(reg.ArchiveSessionID); err != nil || !found {
		t.Fatal(found, err)
	}
}
