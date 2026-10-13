package backfill

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
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
