package state

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

func TestNativeImportInitialStageQuotaHandoffAllowsLegacyWriter(t *testing.T) {
	t.Parallel()
	f := newNativeImportPrepareFixture(t)
	prepared, err := f.prepare(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := prepared.Close(); err != nil {
			t.Error(err)
		}
	}()
	observed := false
	f.native.store.onWriteSync = func() {
		if observed {
			return
		}
		observed = true
		// This runs after complete stage file sync, before source/temp verification.
		// A legacy writer takes hooks→quota while the new writer owns neither lock.
		err := config.WithDurableStorage(f.native.store.home, func(g config.DurableStorageGuard) (err error) {
			q, err := f.native.store.openDurableQuota(g)
			if err != nil {
				return err
			}
			defer func() { err = errors.Join(err, q.Close()) }()
			dir, err := q.home.Root.Open(filepath.Dir(importStageRelative(prepared.reg.ArchiveSessionID)))
			if err != nil {
				return err
			}
			defer func() { err = errors.Join(err, dir.Close()) }()
			entries, err := dir.ReadDir(2)
			if err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			if len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), ".pending-") {
				return errors.New("complete owned stage temp missing at quota handoff")
			}
			info, err := entries[0].Info()
			if err != nil {
				return err
			}
			usage, err := q.usageContext(t.Context())
			if err != nil {
				return err
			}
			if usage.physical < info.Size() || usage.charged < 2*info.Size()+durableControlBytes {
				return errors.New("owned pending temp not fully charged")
			}
			return nil
		})
		if err != nil {
			t.Error("legacy hooks-to-quota writer blocked or misaccounted", err)
		}
	}
	owner, err := f.native.store.CommitNativeImport(t.Context(), prepared)
	f.native.store.onWriteSync = nil
	if err != nil || !observed {
		t.Fatal("initial stage handoff failed", observed, err)
	}
	if _, err := os.Stat(filepath.Join(f.native.store.home, importStageRelative(owner.ArchiveSessionID))); err != nil {
		t.Fatal(err)
	}
	if unlock, err := local.NamedLockWait(f.native.store.home, "hooks.lock", 0); err != nil {
		t.Fatal("policy lock leaked", err)
	} else {
		unlock()
	}
}

func TestNativeImportInitialStageHandoffRefusesChangedControlAndForeignTemp(t *testing.T) {
	for _, mode := range []string{"undone", "canceled", "foreign-temp"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			f := newNativeImportPrepareFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			p, err := f.prepare(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = p.Close() }()
			called := false
			f.native.store.onWriteSync = func() {
				if called {
					return
				}
				called = true
				switch mode {
				case "undone":
					markActualNativeImportUndone(t, f.native.store, p.reg)
				case "canceled":
					cancel()
				case "foreign-temp":
					path := filepath.Join(f.native.store.home, filepath.Dir(importStageRelative(p.reg.ArchiveSessionID)), ".pending-foreign")
					if err := os.WriteFile(path, []byte("foreign"), 0600); err != nil {
						t.Error(err)
					}
				}
			}
			_, err = f.native.store.CommitNativeImport(ctx, p)
			f.native.store.onWriteSync = nil
			if err == nil || !called {
				t.Fatal("stale handoff accepted", called, err)
			}
			if _, err := os.Stat(filepath.Join(f.native.store.home, importStageRelative(p.reg.ArchiveSessionID))); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("refusal selected stage", err)
			}
			if _, found, err := f.native.store.LoadRegistration(p.reg.ArchiveSessionID); err != nil || found {
				t.Fatal("refusal registered owner", found, err)
			}
			if mode == "foreign-temp" {
				raw, err := os.ReadFile(filepath.Join(f.native.store.home, filepath.Dir(importStageRelative(p.reg.ArchiveSessionID)), ".pending-foreign"))
				if err != nil || string(raw) != "foreign" {
					t.Fatal("foreign temp changed", err)
				}
			}
		})
	}
}
