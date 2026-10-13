package backfill

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/cursor"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func settledCursorFixture(t *testing.T) (Registration, Candidate, config.Config, string) {
	t.Helper()
	r, c, cfg := stagingFixture(t)
	cfg.ImportedHarnesses = []string{"cursor"}
	cfg.DurableStorageProtection = true
	cfg.PublicationCompositionProtection = true
	cfg.ImportStagingProtection = true
	cfg.StagedSelectionProtection = true
	cfg.SettledCursorSelectionProtection = true
	if err := config.Save(r.Home, cfg); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := config.Load(r.Home)
	if err != nil {
		t.Fatal(err)
	}
	c.Harness = "cursor"
	c.NativeSessionID = "settled-selected"
	c.TranscriptPath = ""
	c.SourceKind = archive.SourceKindCursorSQLite
	c.SourceKey = c.NativeSessionID
	c.StartedAtSource = archive.StartedAtSourceCursorComposer
	database := filepath.Join(t.TempDir(), "state.vscdb")
	rows := chatRows(c.NativeSessionID, map[string]any{"createdAt": millis(c.StartedAt), "workspaceIdentifier": map[string]any{"id": "synthetic-workspace", "uri": "file://" + c.ProjectRoot}}, strings.Repeat("selected question ", 4096), "selected answer")
	writeCursorDB(t, database, false, rows)
	catalog := readCursorDatabase(t.Context(), testSources, database, cursorstore.Options{})
	if !catalog.Checked || len(catalog.Chats) != 1 {
		t.Fatalf("actual settled planning: %+v", catalog)
	}
	c.reviewedCursor = &catalog.Chats[0]
	r.CursorDatabase = database
	r.Context = t.Context()
	if _, _, err = r.Store.EnsureArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.ID("cursor"), NativeID: c.NativeSessionID}); err != nil {
		t.Fatal(err)
	}
	return r, c, cfg, database
}

func TestSettledCursorPublicImportPublishesAfterNativeDeletion(t *testing.T) {
	r, c, cfg, database := settledCursorFixture(t)
	result, err := r.Run([]Candidate{c})
	if err != nil || len(result.Sessions) != 1 {
		t.Fatalf("actual Cursor admission: %+v %v", result, err)
	}
	id := result.Sessions[0]
	read, closeRead := r.Store.WithReadBudget(t.Context(), agentapi.NewNativeReadBudget(128<<20))
	reg, found, err := read.LoadRegistration(id)
	if err != nil || !found {
		t.Fatal(reg, found, err)
	}
	stage, found, err := read.LoadImportStage(t.Context(), reg)
	if err != nil || !found || stage.Version != 3 || stage.CursorFacts == nil {
		t.Fatal(stage.Version, found, err)
	}
	closeRead()
	if err = os.Remove(database); err != nil {
		t.Fatal(err)
	}
	guard, err := local.LockCollectorGuard(r.Home)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Release()
	remote := storagetest.NewMemoryStore()
	reopened := state.OpenReadOnly(r.Home)
	got, err := collector.Run(t.Context(), reopened, remote, collector.Options{CollectorGuard: guard, Sources: testSources, Parsers: testSources, MachineID: "synthetic", Now: func() time.Time { return r.AdmittedAt.Add(time.Minute) }, AcceptSession: cfg.AcceptSession})
	defer got.CloseStagedResults()
	if err != nil || len(got.Errors) != 0 || len(got.Published) != 1 {
		t.Fatalf("retained Cursor restart: %+v %v", got, err)
	}
	summary, found, err := reopened.LoadPublishedSummary(id)
	if err != nil || !found || !summary.Published {
		t.Fatal(summary, found, err)
	}
	raw, err := os.ReadFile(filepath.Join(r.Home, "published", id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || string(fields["publication_version"]) != "4" {
		t.Fatal("mandatory published4 missing")
	}
	if !bytes.Contains(raw, []byte(`"cursor_commit4"`)) || bytes.Contains(raw, []byte(`"staged_commit3"`)) {
		t.Fatal("Cursor authority used older codec")
	}
	label, _, err := reopened.LoadLabelPublication(id, 8<<20)
	if err != nil || label == nil {
		t.Fatalf("published4 label route: %v", err)
	}
	_, pending, err := reopened.LoadPending(id)
	if err != nil || pending {
		t.Fatal("exact release did not retire pending", pending, err)
	}
}

func TestSettledCursorAdmissionPressureKeepsAuthority(t *testing.T) {
	r, c, _, _ := settledCursorFixture(t)
	r.ReadBudget = agentapi.NewNativeReadBudget(512)
	before := settledStoreInventory(t, r.Home)
	if _, err := r.Run([]Candidate{c}); !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatalf("captured-owner refusal: %v", err)
	}
	after := settledStoreInventory(t, r.Home)
	if len(after) != len(before) {
		t.Fatal("refusal created authority files", after)
	}
	for name, raw := range before {
		if !bytes.Equal(after[name], raw) {
			t.Fatalf("pressure changed %s", name)
		}
	}
	regs, err := r.Store.LoadRegistrations()
	if err != nil || len(regs) != 0 {
		t.Fatal("refused native read admitted owner", regs, err)
	}
	if r.ReadBudget.Available() != 512 {
		t.Fatal("failed capture leaked its original owner")
	}
}

func TestSettledCursorReparentingAfterReviewRefusesAdmission(t *testing.T) {
	r, c, _, database := settledCursorFixture(t)
	writer := openCursorWriter(t, database, false)
	insertCursorRows(t, writer, chatRows("new-parent", map[string]any{"subagentComposerIds": []string{c.NativeSessionID}}, "parent question", "parent answer"))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	before := settledStoreInventory(t, r.Home)
	if _, err := r.Run([]Candidate{c}); err == nil {
		t.Fatal("fresh same-TX parent relationship was ignored")
	}
	after := settledStoreInventory(t, r.Home)
	if len(after) != len(before) {
		t.Fatal("refusal created authority files", after)
	}
	for name, raw := range before {
		if !bytes.Equal(after[name], raw) {
			t.Fatalf("routing refusal changed %s", name)
		}
	}
	regs, err := r.Store.LoadRegistrations()
	if err != nil || len(regs) != 0 {
		t.Fatal(regs, err)
	}
}

func settledStoreInventory(t *testing.T, home string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	err := filepath.WalkDir(home, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		name, err := filepath.Rel(home, path)
		if err != nil {
			return err
		}
		if strings.HasSuffix(name, ".lock") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[name] = raw
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestSettledCursorMalformedGlobalAliasesRefuseAdmission(t *testing.T) {
	for _, raw := range []string{
		`{"_V":18,"composerId":"foreign-parent","fullConversationHeadersOnly":[],"subagentComposerIds":["settled-selected"]}`,
		`{"_v":18,"composerId":"foreign-parent","fullConversationHeadersOnly":[],"SubagentComposerIds":["settled-selected"]}`,
		`{"_v":18,"composerId":5,"fullConversationHeadersOnly":[],"subagentComposerIds":[]}`,
		`{"_v":18,"composerId":"foreign-parent","fullConversationHeadersOnly":[],"workspaceIdentifier":{"id":5}}`,
		`{"_v":18,"composerId":"foreign-parent","fullConversationHeadersOnly":[],"workspaceIdentifier":null}`,
		`{"_v":18,"composerId":"foreign-parent","fullConversationHeadersOnly":[],"workspaceIdentifier":{"uri":{"Path":"/project"}}}`,
		`{"_v":18,"composerId":"foreign-parent","fullConversationHeadersOnly":[],"workspaceIdentifier":{"uri":{"scheme":null,"path":"/project"}}}`,
		`{"_v":18,"composerId":"foreign-parent","fullConversationHeadersOnly":[],"workspaceIdentifier":{"uri":{"authority":5,"path":"/project"}}}`,
	} {
		t.Run(raw, func(t *testing.T) {
			r, c, _, database := settledCursorFixture(t)
			writer := openCursorWriter(t, database, false)
			insertCursorRows(t, writer, map[string]any{"composerData:foreign-parent": raw})
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			before := settledStoreInventory(t, r.Home)
			if _, err := r.Run([]Candidate{c}); err == nil {
				t.Fatal("global malformed/aliased authority admitted selected owner")
			}
			after := settledStoreInventory(t, r.Home)
			if len(before) != len(after) {
				t.Fatal("global refusal created authorities")
			}
			for name, body := range before {
				if !bytes.Equal(body, after[name]) {
					t.Fatalf("global refusal changed %s", name)
				}
			}
		})
	}
}

func TestSettledCursorFirstSaveRefusesOccupiedSlots(t *testing.T) {
	for _, directory := range []string{"registrations", "requests", "pending", "published"} {
		t.Run(directory, func(t *testing.T) {
			r, c, _, _ := settledCursorFixture(t)
			id, found, err := r.Store.ArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.Cursor, NativeID: c.NativeSessionID})
			if err != nil || !found {
				t.Fatal(found, err)
			}
			reg := r.registration(c, id, "")
			budget := agentapi.NewNativeReadBudget(128 << 20)
			evidence, err := cursor.CaptureSettledImport(t.Context(), r.CursorDatabase, reg, *c.reviewedCursor, r.AdmittedAt, budget)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := evidence.Close(); err != nil {
					t.Error(err)
				}
			}()
			if directory == "registrations" {
				ordinary := reg
				ordinary.Origin = archive.SessionOriginHook
				ordinary.ImportBatch = archive.NewImportBatch("")
				if _, err = r.Store.RegisterReservedSession(agentmeta.SessionKey{Agent: agentmeta.Cursor, NativeID: ordinary.NativeSessionID}, id, func(string) archive.SessionRegistration { return ordinary }); err != nil {
					t.Fatal(err)
				}
			} else {
				// Standalone journal evidence may be orphaned/malformed. Its presence
				// is not absence and must not authorize a new first-stage selection.
				if err = local.WriteBytes(filepath.Join(r.Home, directory, id+".json"), []byte(`{"unknown":"retained authority"}`)); err != nil {
					t.Fatal(err)
				}
			}
			before := settledStoreInventory(t, r.Home)
			store, closeStore := r.Store.WithReadBudget(t.Context(), budget)
			defer closeStore()
			if _, err = store.SaveSettledCursorImportStage(t.Context(), reg, evidence); !errors.Is(err, state.ErrReadySelectionFrozen) {
				t.Fatalf("occupied first-save accepted: %v", err)
			}
			after := settledStoreInventory(t, r.Home)
			if len(after) != len(before) {
				t.Fatal("occupied refusal created authority files", after)
			}
			for path, raw := range before {
				if !bytes.Equal(raw, after[path]) {
					t.Fatalf("occupied refusal changed %s", path)
				}
			}
			if _, err = os.Lstat(filepath.Join(r.Home, "sessions", id, "import-stage", "stage.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("first-save left stage: %v", err)
			}
		})
	}
}

func TestSettledCursorExactStageRetryAfterAdmission(t *testing.T) {
	r, c, _, _ := settledCursorFixture(t)
	id, found, err := r.Store.ArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.Cursor, NativeID: c.NativeSessionID})
	if err != nil || !found {
		t.Fatal(found, err)
	}
	reg := r.registration(c, id, "")
	budget := agentapi.NewNativeReadBudget(128 << 20)
	evidence, err := cursor.CaptureSettledImport(t.Context(), r.CursorDatabase, reg, *c.reviewedCursor, r.AdmittedAt, budget)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := evidence.Close(); err != nil {
			t.Error(err)
		}
	}()
	store, closeStore := r.Store.WithReadBudget(t.Context(), budget)
	defer closeStore()
	digest, err := store.SaveSettledCursorImportStage(t.Context(), reg, evidence)
	if err != nil {
		t.Fatal(err)
	}
	admitted := reg
	admitted.StagedImportSHA256 = digest
	if _, err = r.Store.RegisterReservedSession(agentmeta.SessionKey{Agent: agentmeta.Cursor, NativeID: reg.NativeSessionID}, id, func(string) archive.SessionRegistration { return admitted }); err != nil {
		t.Fatal(err)
	}
	if err = r.Store.SaveRequest(id, "backfill", r.AdmittedAt); err != nil {
		t.Fatal(err)
	}
	before := settledStoreInventory(t, r.Home)
	retry, err := store.SaveSettledCursorImportStage(t.Context(), reg, evidence)
	if err != nil || retry != digest {
		t.Fatalf("exact owned retry: %s %v", retry, err)
	}
	after := settledStoreInventory(t, r.Home)
	if len(after) != len(before) {
		t.Fatal("exact retry changed authority inventory")
	}
	for path, raw := range before {
		if !bytes.Equal(raw, after[path]) {
			t.Fatalf("exact retry changed %s", path)
		}
	}
}

func TestSettledCursorGlobalRosterBoundControls(t *testing.T) {
	for _, count := range []int{64, 65} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			r, c, _, database := settledCursorFixture(t)
			ids := make([]string, count)
			for i := range ids {
				ids[i] = fmt.Sprintf("foreign-child-%02d", i)
			}
			raw, err := json.Marshal(map[string]any{"_v": 18, "composerId": "foreign-parent", "fullConversationHeadersOnly": []any{}, "subagentComposerIds": ids})
			if err != nil {
				t.Fatal(err)
			}
			writer := openCursorWriter(t, database, false)
			insertCursorRows(t, writer, map[string]any{"composerData:foreign-parent": string(raw)})
			if err = writer.Close(); err != nil {
				t.Fatal(err)
			}
			before := settledStoreInventory(t, r.Home)
			result, err := r.Run([]Candidate{c})
			if count == 64 {
				if err != nil || len(result.Sessions) != 1 {
					t.Fatalf("exact64 global roster control: %+v %v", result, err)
				}
				return
			}
			if err == nil {
				t.Fatal("oversize global roster admitted owner")
			}
			after := settledStoreInventory(t, r.Home)
			if len(after) != len(before) {
				t.Fatal("oversize roster created authority files")
			}
			for path, raw := range before {
				if !bytes.Equal(raw, after[path]) {
					t.Fatalf("oversize roster changed %s", path)
				}
			}
		})
	}
}

func TestSettledCursorSelectedNullRoutingKeepsAuthority(t *testing.T) {
	r, c, _, database := settledCursorFixture(t)
	writer := openCursorWriter(t, database, false)
	rows := chatRows(c.NativeSessionID, map[string]any{"createdAt": millis(c.StartedAt)}, "question", "answer")
	var composer map[string]any
	if err := json.Unmarshal([]byte(rows["composerData:"+c.NativeSessionID].(string)), &composer); err != nil {
		t.Fatal(err)
	}
	composer["workspaceIdentifier"] = nil
	raw, err := json.Marshal(composer)
	if err != nil {
		t.Fatal(err)
	}
	rows["composerData:"+c.NativeSessionID] = string(raw)
	insertCursorRows(t, writer, rows)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	before := settledStoreInventory(t, r.Home)
	if _, err := r.Run([]Candidate{c}); err == nil {
		t.Fatal("selected null routing admitted owner")
	}
	after := settledStoreInventory(t, r.Home)
	if len(after) != len(before) {
		t.Fatal("null routing created authority files")
	}
	for path, raw := range before {
		if !bytes.Equal(raw, after[path]) {
			t.Fatalf("null routing changed %s", path)
		}
	}
}

func TestSettledCursorFirstSaveRefusesStaleCommittedIndex(t *testing.T) {
	r, c, _, _ := settledCursorFixture(t)
	id, found, err := r.Store.ArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.Cursor, NativeID: c.NativeSessionID})
	if err != nil || !found {
		t.Fatal(found, err)
	}
	reg := r.registration(c, id, "")
	budget := agentapi.NewNativeReadBudget(128 << 20)
	evidence, err := cursor.CaptureSettledImport(t.Context(), r.CursorDatabase, reg, *c.reviewedCursor, r.AdmittedAt, budget)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := evidence.Close(); err != nil {
			t.Error(err)
		}
	}()
	ordinary := reg
	ordinary.Origin = archive.SessionOriginHook
	ordinary.ImportBatch = archive.NewImportBatch("")
	if _, err = r.Store.RegisterReservedSession(agentmeta.SessionKey{Agent: agentmeta.Cursor, NativeID: reg.NativeSessionID}, id, func(string) archive.SessionRegistration { return ordinary }); err != nil {
		t.Fatal(err)
	}
	// This negative crash/file-loss setup keeps the real committed index intact.
	// It is not a fresh positive owner or a registration-promotion shortcut.
	if err = os.Remove(filepath.Join(r.Home, "registrations", id+".json")); err != nil {
		t.Fatal(err)
	}
	before := settledStoreInventory(t, r.Home)
	store, closeStore := r.Store.WithReadBudget(t.Context(), budget)
	defer closeStore()
	if _, err = store.SaveSettledCursorImportStage(t.Context(), reg, evidence); !errors.Is(err, state.ErrReadySelectionFrozen) {
		t.Fatalf("stale committed index revived: %v", err)
	}
	after := settledStoreInventory(t, r.Home)
	if len(after) != len(before) {
		t.Fatal("stale index refusal created authorities")
	}
	for path, raw := range before {
		if !bytes.Equal(raw, after[path]) {
			t.Fatalf("stale refusal changed %s", path)
		}
	}
}
