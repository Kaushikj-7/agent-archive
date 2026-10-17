package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestNativeImportUnsupportedReadersNeverQuarantineOrRepairOwner(t *testing.T) {
	for _, mutation := range []struct {
		name   string
		change func([]byte) []byte
	}{
		{"future", func(raw []byte) []byte {
			return bytes.Replace(raw, []byte(`"contract":"muse-historical-import-v1"`), []byte(`"contract":"future"`), 1)
		}},
		{"nested_type", func(raw []byte) []byte {
			return bytes.Replace(raw, []byte(`"batch_id":"2026-10-17-1"`), []byte(`"batch_id":[]`), 1)
		}},
		{"null", func(raw []byte) []byte {
			return bytes.Replace(raw, []byte(`"native_import":{`), []byte(`"native_import":null,"ignored":{`), 1)
		}},
		{"truncated", func(raw []byte) []byte { return raw[:len(`{"native_import":{`)] }},
		{"escaped_truncated", func(raw []byte) []byte { return []byte(`{"native_\u0069mport":`) }},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			f, owner := historicalNativeOwnerFixture(t)
			path := f.store.registrationPath(owner.ArchiveSessionID)
			original, err := json.Marshal(owner)
			if err != nil {
				t.Fatal(err)
			}
			raw := mutation.change(original)
			if bytes.Equal(raw, original) {
				t.Fatal("inert mutation")
			}
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			key := agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: owner.NativeSessionID}
			indexPath := qualifiedSessionIndexPath(f.store.home, key)
			index, err := os.ReadFile(indexPath)
			if err != nil {
				t.Fatal(err)
			}
			assertUnsupportedNativeOwnerReads(t, f.store, owner, key)
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, raw) {
				t.Fatal("protected owner changed", err)
			}
			after, err = os.ReadFile(indexPath)
			if err != nil || !bytes.Equal(after, index) {
				t.Fatal("protected index changed", err)
			}
			if len(f.store.QuarantinedFiles()) != 0 {
				t.Fatal("protected receipt quarantined")
			}
			f.assertReleased(t)
		})
	}
}

func assertUnsupportedNativeOwnerReads(t *testing.T, s *Store, owner archive.SessionRegistration, key agentmeta.SessionKey) {
	t.Helper()
	check := func(err error) {
		t.Helper()
		if !errors.Is(err, archive.ErrUnsupportedNativeImport) || IsUndecodable(err) || isCorruptJSON(err) {
			t.Fatal("receipt became repairable", err)
		}
	}
	_, _, err := s.LoadRegistration(owner.ArchiveSessionID)
	check(err)
	_, err = s.LoadRegistrations()
	check(err)
	regs, issues, err := s.ForCollectorPass().ScanRegistrations()
	if err != nil || len(regs) != 0 {
		t.Fatal(regs, err)
	}
	check(issues[owner.ArchiveSessionID])
	_, _, err = s.ArchiveSessionID(key)
	check(err)
	check(s.recoverRegistrationOwners(key, []string{owner.ArchiveSessionID}))
	check(s.SaveRegistration(owner))
	_, err = s.UpdateRegistration(owner.ArchiveSessionID, func(*archive.SessionRegistration) error { t.Fatal("unsupported owner reached mutation"); return nil })
	check(err)
	scoped, closeRead := s.WithReadBudget(t.Context(), agentapi.NewNativeReadBudget(1<<20))
	_, _, err = scoped.LoadRegistration(owner.ArchiveSessionID)
	check(err)
	var budgeted archive.SessionRegistration
	check(scoped.readBudgeted(s.registrationPath(owner.ArchiveSessionID), &budgeted, true))
	closeRead()
	entries, err := os.ReadDir(filepath.Join(s.home, "registrations"))
	if err != nil {
		t.Fatal(err)
	}
	reader := s.newRecoveryRegistrationReader()
	results := reader.readChunk(entries)
	reader.close()
	check(results[0].err)
	var buffer bytes.Buffer
	var reg archive.SessionRegistration
	check(readRecoveryRegistration(s.registrationPath(owner.ArchiveSessionID), &buffer, &reg))
}
