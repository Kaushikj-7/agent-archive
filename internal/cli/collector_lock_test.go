package cli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/local"
)

func TestCollectorGuardSharesLockAndExpiresOnRelease(t *testing.T) {
	t.Parallel()
	for _, needsGuard := range []bool{false, true} {
		name := "ordinary"
		if needsGuard {
			name = "guarded"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
			guard, unlock, err := lockCollectorGuard(home, "sync", at, needsGuard)
			must(t, err)
			t.Cleanup(func() {
				if unlock != nil {
					unlock()
				}
			})
			if (guard != nil) != needsGuard {
				t.Fatalf("typed ownership=%v, want %v", guard != nil, needsGuard)
			}
			if guard != nil {
				origin, err := guard.Origin()
				if err != nil || origin == "" {
					t.Fatalf("actual held guard has no origin: %v", err)
				}
				guardedHome, err := guard.Home()
				canonicalHome, pathErr := filepath.EvalSymlinks(home)
				if err != nil || pathErr != nil || guardedHome != canonicalHome {
					t.Fatalf("guard owns another home: %s %v %v", guardedHome, err, pathErr)
				}
			}
			before, found := readCollectorLockRecord(home)
			if !found || before.Holder != "sync" || !before.Since.Equal(at) {
				t.Fatal("holder record missing")
			}
			for _, contenderNeedsGuard := range []bool{false, true} {
				other, release, err := lockCollectorGuard(home, "contender", at, contenderNeedsGuard)
				if release != nil {
					release()
				}
				if !errors.Is(err, local.ErrBusy) || other != nil || release != nil {
					t.Fatalf("contender escaped mutual exclusion: %v", err)
				}
			}
			if release, err := local.Lock(home); !errors.Is(err, local.ErrBusy) {
				if release != nil {
					release()
				}
				t.Fatalf("ordinary lock escaped the guard: %v", err)
			}
			if after, found := readCollectorLockRecord(home); !found || before != after {
				t.Fatal("busy contender changed the held record")
			}
			unlock()
			unlock = nil
			if guard != nil {
				if _, err := guard.Origin(); !errors.Is(err, local.ErrBusy) {
					t.Fatalf("released guard still grants authority: %v", err)
				}
			} else if _, err := os.Stat(filepath.Join(home, "collector-principal.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("ordinary legacy lock created typed ownership: %v", err)
			}
			assertCollectorUnlocked(t, home)
		})
	}
}

func TestCollectorGuardAcquisitionFailureReleasesActualLock(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	must(t, local.Write(filepath.Join(home, "collector-principal.json"), "invalid-principal"))
	guard, release, err := lockCollectorGuard(home, "sync", time.Now(), true)
	if release != nil {
		release()
	}
	if err == nil || guard != nil || release != nil {
		t.Fatalf("invalid durable principal granted ownership: %v", err)
	}
	assertCollectorUnlocked(t, home)
}
