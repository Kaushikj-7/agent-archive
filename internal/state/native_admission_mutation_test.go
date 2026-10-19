package state

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
)

func TestPreparedNativeAdmissionSourceChangesBeforeEachOwnerCAS(t *testing.T) {
	t.Parallel()
	mutations := []struct {
		name   string
		change func(*testing.T, string)
	}{
		{name: "append", change: func(t *testing.T, path string) {
			t.Helper()
			file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			_, writeErr := file.WriteString("\n")
			closeErr := file.Close()
			if writeErr != nil || closeErr != nil {
				t.Fatal(writeErr, closeErr)
			}
		}},
		{name: "replacement", change: func(t *testing.T, path string) {
			t.Helper()
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			replacement := filepath.Join(filepath.Dir(path), "replacement.jsonl")
			if err := os.WriteFile(replacement, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(replacement, path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "same_stamp_rewrite", change: func(t *testing.T, path string) {
			t.Helper()
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			changed := bytes.Replace(raw, []byte("synthetic parser"), []byte("synthetic parSer"), 1)
			if bytes.Equal(raw, changed) || len(raw) != len(changed) {
				t.Fatal("rewrite fixture did not preserve size")
			}
			if err := os.WriteFile(path, changed, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, mutation := range mutations {
		for _, registrationStage := range []bool{false, true} {
			name := mutation.name + "/reservation"
			if registrationStage {
				name = mutation.name + "/registration"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				fixture := newNativeAdmissionFixture(t)
				prepared, err := fixture.prepare(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				called := false
				mutate := func() {
					if !called {
						called = true
						mutation.change(t, fixture.path)
					}
				}
				if registrationStage {
					fixture.store.onWriteSync = mutate
				} else {
					fixture.store.onIndexSync = mutate
				}
				if _, err := fixture.store.CommitNativeAdmission(t.Context(), prepared); err == nil {
					t.Fatal("changed source committed owner")
				}
				if !called {
					t.Fatal("stage mutation not exercised")
				}
				if _, found, err := fixture.store.LoadRegistration(prepared.reg.ArchiveSessionID); err != nil || found {
					t.Fatal("changed source exposed owner", found, err)
				}
				key := agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: fixture.candidate.Intent.NativeSessionID}
				entry, found, err := fixture.store.readQualifiedIndex(key)
				if err != nil || found != registrationStage || found && !supportedNativeReservation(entry.NativeReservation) {
					t.Fatal("wrong reservation boundary", entry, found, err)
				}
				if err := prepared.Close(); err != nil {
					t.Fatal(err)
				}
				fixture.assertReleased(t)
			})
		}
	}
}
