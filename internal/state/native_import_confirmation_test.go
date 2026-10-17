package state

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

// This arranges reviewed intent only, without opening any source or creating
// an owner, reservation or stage. Source proof qualification belongs to prepare.
func nativeImportConfirmationFixture(t *testing.T) (*Store, archive.NativeImportBatchControl) {
	t.Helper()
	f := newNativeAdmissionFixture(t)
	cfg, _, err := config.Load(f.store.Home())
	if err != nil {
		t.Fatal(err)
	}
	cfg.NativeImportProtection, cfg.DurableStorageProtection = true, true
	cfg.PublicationCompositionProtection, cfg.ImportStagingProtection, cfg.StagedSelectionProtection = true, true, true
	cfg.ImportedHarnesses = []string{"muse"}
	if err := config.Save(f.store.Home(), cfg); err != nil {
		t.Fatal(err)
	}
	fingerprint, err := archive.NativeRootFingerprint(f.candidate.Root)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	scope := archive.NativeImportBatchScope{BatchID: "2026-10-17-1", StartedAtUTC: at.Format(time.RFC3339Nano), DestinationID: cfg.DestinationID(), Filters: archive.ImportBatchFilters{Harnesses: []string{"muse"}, ProjectIDs: []string{}}, ProjectIDs: []string{f.candidate.ProjectID}, Selected: []archive.NativeImportBatchSelection{{Kind: archive.NativeImportOrdinaryRoot, NativeID: "session", OriginalTUTC: at.AddDate(-1, 0, 0).Format(time.RFC3339Nano), ProjectID: f.candidate.ProjectID, RootFingerprint: fingerprint, RootRevision: f.candidate.Root.Revision, BindingSHA256: strings.Repeat("b", 64), Source: archive.NativeCheckpoint{Version: 1, Domain: "muse-ordinary", Algorithm: "sha256", Boundary: 1, SHA256: strings.Repeat("c", 64)}}}}
	digest, err := archive.NativeImportBatchScopeDigest(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	control := archive.NativeImportBatchControl{ID: scope.BatchID, StartedAt: at, Filters: scope.Filters, DestinationID: scope.DestinationID, NativeImport: &archive.NativeImportBatchConfirmation{Contract: "muse-historical-import-v1", Instance: strings.Repeat("a", 32), ScopeSHA256: digest, Scope: scope}}
	if err := local.Write(filepath.Join(f.store.Home(), "imports", control.ID+".json"), control); err != nil {
		t.Fatal(err)
	}
	// The configured native directory can disappear: confirmation reads config
	// and the selected control only, never native files or live capture windows.
	if err := os.RemoveAll(f.candidate.Root.Canonical); err != nil {
		t.Fatal(err)
	}
	return f.store, control
}

func TestNativeImportConfirmationHandleLifetimeAndStoreBinding(t *testing.T) {
	t.Parallel()
	s, control := nativeImportConfirmationFixture(t)
	budget := agentapi.NewNativeReadBudget(8 << 20)
	scoped, closeScope := s.WithReadBudget(t.Context(), budget)
	defer closeScope()
	h, err := scoped.OpenConfirmedNativeImportBatch(t.Context(), control.ID)
	if err != nil {
		t.Fatal(err)
	}
	if budget.Available() == 8<<20 {
		t.Fatal("retained control was not charged")
	}
	if err := h.checkCurrent(t.Context(), scoped); err != nil {
		t.Fatal(err)
	}
	if err := h.checkCurrent(t.Context(), s); err == nil {
		t.Fatal("different Store accepted")
	}
	copied := ConfirmedNativeImportBatch{store: h.store, self: h, closeRead: h.closeRead}
	if err := copied.checkCurrent(t.Context(), scoped); err == nil {
		t.Fatal("copied handle accepted")
	}
	if err := copied.Close(); err == nil {
		t.Fatal("copied handle closed original lifetime")
	}
	if err := h.checkCurrent(t.Context(), scoped); err != nil {
		t.Fatal("copy affected original", err)
	}
	var zero ConfirmedNativeImportBatch
	if err := zero.checkCurrent(t.Context(), scoped); err == nil {
		t.Fatal("zero handle accepted")
	}
	release, err := h.borrow(scoped)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.checkOpen(scoped); err == nil {
		t.Fatal("Close did not expire borrowed handle")
	}
	if budget.Available() == 8<<20 {
		t.Fatal("in-flight borrowed input released early")
	}
	release()
	release()
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if budget.Available() != 8<<20 {
		t.Fatal("closed handle leaked budget", budget.Available())
	}
	if err := h.checkCurrent(t.Context(), scoped); err == nil {
		t.Fatal("closed handle accepted")
	}
}

func TestNativeImportConfirmationRechecksCurrentControl(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"missing", "future", "instance", "undone", "completed", "progress"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			s, control := nativeImportConfirmationFixture(t)
			h, err := s.OpenConfirmedNativeImportBatch(t.Context(), control.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := h.Close(); err != nil {
					t.Error(err)
				}
			}()
			path := filepath.Join(s.Home(), "imports", control.ID+".json")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "future":
				if err := os.WriteFile(path, bytes.Replace(raw, []byte("muse-historical-import-v1"), []byte("muse-historical-import-v2"), 1), 0600); err != nil {
					t.Fatal(err)
				}
			case "instance":
				control.NativeImport.Instance = strings.Repeat("f", 32)
			case "undone":
				at := time.Now().UTC()
				control.UndoneAt = &at
			case "completed":
				at := time.Now().UTC()
				control.CompletedAt = &at
			case "progress":
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(raw, &fields); err != nil {
					t.Fatal(err)
				}
				fields["sessions"] = json.RawMessage(`["independent-progress"]`)
				raw, err = json.Marshal(fields)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "instance" || mode == "undone" || mode == "completed" {
				if err := local.Write(path, control); err != nil {
					t.Fatal(err)
				}
			}
			err = h.checkCurrent(t.Context(), s)
			if (err == nil) != (mode == "progress") {
				t.Fatal("current control eligibility", err)
			}
		})
	}
}

func TestNativeImportConfirmationScopeRechecksConfiguration(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"paused", "root", "project", "destination", "flag"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			s, control := nativeImportConfirmationFixture(t)
			h, err := s.OpenConfirmedNativeImportBatch(t.Context(), control.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := h.Close(); err != nil {
					t.Error(err)
				}
			}()
			cfg, _, err := config.Load(s.Home())
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "paused":
				cfg.Paused = true
			case "root":
				path := filepath.Join(s.Home(), "config.json")
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				changed := bytes.ReplaceAll(raw, []byte(`"revision": "one"`), []byte(`"revision": "changed"`))
				changed = bytes.ReplaceAll(changed, []byte(`"revision":"one"`), []byte(`"revision":"changed"`))
				if bytes.Equal(changed, raw) {
					t.Fatal("inert root revision mutation")
				}
				if err := os.WriteFile(path, changed, 0600); err != nil {
					t.Fatal(err)
				}
				if err := h.checkCurrent(t.Context(), s); err == nil {
					t.Fatal("changed root accepted")
				}
				return
			case "project":
				cfg.Archive.Projects[0].Included = false
			case "destination":
				cfg.Storage.Bucket += "-changed"
			case "flag":
				cfg.NativeImportProtection = false
			}
			// Deliberately model a stale writer without weakening config.Save sticky
			// protections. The consuming predicate must independently refuse it.
			if err := local.Write(filepath.Join(s.Home(), "config.json"), cfg); err != nil {
				t.Fatal(err)
			}
			if err := h.checkCurrent(t.Context(), s); err == nil {
				t.Fatal("changed current policy accepted")
			}
		})
	}
}
