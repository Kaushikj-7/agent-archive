package cli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/local"
)

func TestCollectorLockGuardRoutePreservesExclusionAndLifetime(t *testing.T) {
	t.Parallel()
	for _, guarded := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "typed"}[guarded], func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			guard, release, err := lockCollectorGuard(home, "sync", time.Now(), guarded)
			if err != nil {
				t.Fatal(err)
			}
			held := true
			defer func() {
				if held {
					release()
				}
			}()
			if unlock, err := local.Lock(home); !errors.Is(err, local.ErrBusy) {
				if err == nil {
					unlock()
				}
				t.Fatal("collector lock exclusion lost", err)
			}
			if guarded {
				if guard == nil {
					t.Fatal("typed route lost actual lock authority")
				}
				if _, err := guard.Origin(); err != nil {
					t.Fatal("held guard has no actual origin", err)
				}
			} else if guard != nil {
				t.Fatal("ordinary legacy lock gained guard effects")
			} else if _, err := os.Lstat(filepath.Join(home, "collector-principal.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("ordinary legacy lock changed principal files", err)
			}
			release()
			held = false
			if guarded {
				if _, err := guard.Origin(); err == nil {
					t.Fatal("released guard retained authority")
				}
			}
			unlock, err := local.Lock(home)
			if err != nil {
				t.Fatal("collector lock not released", err)
			}
			unlock()
		})
	}
}
