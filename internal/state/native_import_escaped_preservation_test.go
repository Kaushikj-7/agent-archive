package state

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

func TestNativeImportEscapedSelectorScanPreservesDamagedOwner(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string][]byte{
		"escaped":       []byte(`{"ignored":"unterminated,"native_\u0069mport":{}}`),
		"escaped-alias": []byte(`{"ignored":"unterminated,"NATIVE_\u0049MPORT":{}}`),
		"fully-escaped": []byte(`{"ignored":"unterminated,"\u006e\u0061\u0074\u0069\u0076\u0065\u005f\u0069\u006d\u0070\u006f\u0072\u0074":{}}`),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, owner := historicalNativeOwnerFixture(t)
			cfg, _, err := config.Load(f.store.Home())
			if err != nil {
				t.Fatal(err)
			}
			cfg.NativeImportProtection = true
			if err := config.Save(f.store.Home(), cfg); err != nil {
				t.Fatal(err)
			}
			if err := f.store.SaveRequest(owner.ArchiveSessionID, "later-independent-request", time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			key := agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: owner.NativeSessionID}
			indexPath := qualifiedSessionIndexPath(f.store.Home(), key)
			index, err := os.ReadFile(indexPath)
			if err != nil {
				t.Fatal(err)
			}
			requestPath := filepath.Join(f.store.Home(), "requests", owner.ArchiveSessionID+".json")
			request, err := os.ReadFile(requestPath)
			if err != nil {
				t.Fatal(err)
			}
			path := f.store.registrationPath(owner.ArchiveSessionID)
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			// Start at the actual census. An ordinary syntax error here used to move
			// this protected record aside before a later ownership reader could refuse.
			regs, issues, scanErr := f.store.ForCollectorPass().ScanRegistrations()
			if scanErr != nil || len(regs) != 0 {
				t.Fatal("unexpected scan result", regs, scanErr)
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, raw) {
				t.Fatal("escaped protected owner was quarantined or changed", err)
			}
			if !errors.Is(issues[owner.ArchiveSessionID], archive.ErrUnsupportedNativeImport) {
				t.Fatal("scan lost protected-selector refusal", issues)
			}
			assertUnsupportedNativeOwnerReads(t, f.store, owner, key)
			if err := f.store.CheckSessionUndo(owner); !errors.Is(err, archive.ErrUnsupportedNativeImport) {
				t.Fatal("undo lost actual malformed authority", err)
			}
			if _, err := f.store.PrepareSessionDeletion(owner, RemovalReasonUndo, nil, time.Now().UTC(), ""); !errors.Is(err, archive.ErrUnsupportedNativeImport) {
				t.Fatal("deletion prepared malformed protected owner", err)
			}
			for path, want := range map[string][]byte{path: raw, indexPath: index, requestPath: request} {
				got, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(got, want) {
					t.Fatal("refusal changed owner/index/independent request", path, err)
				}
			}
			entries, err := os.ReadDir(filepath.Join(f.store.Home(), "registrations"))
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || len(f.store.QuarantinedFiles()) != 0 {
				t.Fatal("protected escaped record produced quarantine", entries)
			}
			f.assertReleased(t)
		})
	}
}
