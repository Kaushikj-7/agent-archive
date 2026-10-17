package config

import (
	"errors"
	"github.com/wangjohn/agent-archive/internal/local"
	"os"
	"path/filepath"
	"testing"
)

func TestPublicationCompositionFinalOwnedCloseErrorFollowsCallbackEffects(t *testing.T) {
	for _, callbackError := range []bool{false, true} {
		t.Run(map[bool]string{false: "successful-callback", true: "failed-callback"}[callbackError], func(t *testing.T) {
			home := durableTestHome(t)
			if err := Save(home, Config{MachineID: "synthetic"}); err != nil {
				t.Fatal(err)
			}
			closeFailure := errors.New("synthetic final owned Root.Close error after real close")
			callbackFailure := errors.New("synthetic callback error after effect")
			closes := 0
			var retained DurableStorageGuard
			err := withPublicationStorageClosing(home, true, func(g DurableStorageGuard) error {
				retained = g
				held, err := g.RootedHome(home)
				if err != nil {
					return err
				}
				if err = held.Root.WriteFile("synthetic-callback-effect", []byte("exact effect"), 0600); err != nil {
					return err
				}
				if callbackError {
					return callbackFailure
				}
				return nil
			}, func(held *local.RootedHome) error { closes++; return errors.Join(held.Close(), closeFailure) })
			if !errors.Is(err, closeFailure) || errors.Is(err, callbackFailure) != callbackError || closes != 1 {
				t.Fatal("final close error lost", err, closes)
			}
			raw, err := os.ReadFile(filepath.Join(home, "synthetic-callback-effect"))
			if err != nil || string(raw) != "exact effect" {
				t.Fatal("completed callback effect misrepresented", string(raw), err)
			}
			if err = retained.CheckHome(home); err == nil {
				t.Fatal("closed guard survived")
			}
			// Default production close stays the actual owned close and is successful.
			if err = WithPublicationComposition(home, func(g PublicationCompositionGuard) error { _, err := g.Storage(home); return err }); err != nil {
				t.Fatal("default close", err)
			}
		})
	}
}
