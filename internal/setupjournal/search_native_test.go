package setupjournal

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/fileapply"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/scheduler"
	"github.com/wangjohn/agent-archive/internal/scheduler/launchd"
)

// Value marshaling must retain recovery fences even for an unaddressable journal.
var _ json.Marshaler = Journal{}

func combinedNativeSearchJournal(t *testing.T) (Journal, []byte, []byte) {
	t.Helper()
	j, before, after := nativeJournalFixture(t)
	protect := func(raw []byte) []byte {
		var cfg config.Config
		must(t, json.Unmarshal(raw, &cfg))
		must(t, config.SetSearchIndexing(&cfg, true))
		result, err := json.Marshal(cfg)
		must(t, err)
		return result
	}
	before, after = protect(before), protect(after)
	j.Changes[0].Before, j.Changes[0].After = before, after
	j.Jobs = []ManagedJob{{Role: SearchRole, Backend: "launchd", Ref: "com.agent-archive.search", Definition: "/synthetic/Library/LaunchAgents/com.agent-archive.search.plist", Intent: Keep}}
	return j, before, after
}

func TestManagedNativeJournalRoundtripPreservesBothFencesAndRoleOrder(t *testing.T) {
	for _, searchFirst := range []bool{false, true} {
		for _, filesOnly := range []bool{false, true} {
			j, _, _ := combinedNativeSearchJournal(t)
			collector := ManagedJob{Role: CollectorRole, Backend: "launchd", Ref: "com.agent-archive.collector", Definition: "/synthetic/Library/LaunchAgents/com.agent-archive.collector.plist", Intent: Keep}
			if searchFirst {
				j.Jobs = append(j.Jobs, collector)
			} else {
				j.Jobs = append([]ManagedJob{collector}, j.Jobs...)
			}
			j.FilesOnly = filesOnly
			raw, err := json.Marshal(j)
			must(t, err)
			var loaded Journal
			must(t, json.Unmarshal(raw, &loaded))
			if loaded.FilesOnly != filesOnly || !loaded.NativeAuthorityProtection || len(loaded.Jobs) != 2 || loaded.Jobs[0].Role != j.Jobs[0].Role || !bytes.Equal(loaded.Changes[0].Before, j.Changes[0].Before) {
				t.Fatal("composed journal lost ownership or rollback bytes", string(raw))
			}
			// A native-only predecessor knows files_only as an object but still knows
			// changes as an array; a search-only predecessor knows files_only as bool.
			var nativeOnly struct {
				Changes   []fileapply.Change `json:"changes"`
				FilesOnly json.RawMessage    `json:"files_only"`
			}
			var searchOnly struct {
				Changes   json.RawMessage `json:"changes"`
				FilesOnly bool            `json:"files_only"`
			}
			var legacy frozenLegacyJournal
			if json.Unmarshal(raw, &nativeOnly) == nil || json.Unmarshal(raw, &searchOnly) == nil || json.Unmarshal(raw, &legacy) == nil {
				t.Fatal("a predecessor accepted combined recovery authority")
			}
			roundtrip, err := json.Marshal(loaded)
			must(t, err)
			if !bytes.Equal(raw, roundtrip) {
				t.Fatal("second encoding lost journal authority")
			}
		}
	}
}

func TestManagedNativeJournalRejectsMalformedAuthorityBeforeRecovery(t *testing.T) {
	j, _, _ := combinedNativeSearchJournal(t)
	raw, err := json.Marshal(j)
	must(t, err)
	valid := string(raw)
	for _, mutated := range []string{
		strings.Replace(valid, `"schema_version":2`, `"schema_version":3`, 1),
		strings.Replace(valid, `"native_authority_protection":true`, `"native_authority_protection":false`, 1),
		strings.Replace(valid, `"native_authority_protection":true,`, ``, 1),
		strings.Replace(valid, `"files_only":{"version":1,"writer":"native-authority-v16","value":true}`, `"files_only":true`, 1),
		strings.Replace(valid, `"writer":"native-authority-v16"`, `"writer":"future"`, 1),
		strings.Replace(valid, `"files_only":`, `"FILES_ONLY":null,"files_only":`, 1),
		strings.Replace(valid, `"role":"search"`, `"ROLE":"search","role":"search"`, 1),
		strings.Replace(valid, `"schema_version":2`, `"schema_version":2,"SCHEMA_VERSION":2`, 1),
		strings.Replace(valid, `"schema_version":2`, `"schema_version":2,"schema_version":2`, 1),
		strings.Replace(valid, `"intent":"keep"`, `"intent":"future"`, 1),
	} {
		if mutated == valid {
			t.Fatal("inert mutation")
		}
		home := t.TempDir()
		must(t, os.WriteFile(JournalPath(home), []byte(mutated), 0600))
		err := Recover(home, func(string) (scheduler.Scheduler, error) {
			t.Fatal("malformed authority reached manager")
			return nil, nil
		}, func() (func(), error) { t.Fatal("malformed authority took locks"); return func() {}, nil })
		if err == nil || !TransactionPending(home) {
			t.Fatal("malformed recovery was admitted", mutated)
		}
	}
}

func TestManagedNativeJournalRecoveryRestoresCombinedConfigWithoutManagerMutation(t *testing.T) {
	home := local.CanonicalPath(t.TempDir())
	j, before, after := combinedNativeSearchJournal(t)
	j.Changes[0].Path = filepath.Join(home, "config.json")
	backend := launchd.Scheduler{}
	ref := backend.Ref(scheduler.Installation{DataHome: home, Role: scheduler.Search})
	j.Jobs[0].Ref = string(ref)
	j.Jobs[0].Definition = filepath.Join(t.TempDir(), "Library", "LaunchAgents", string(ref)+".plist")
	must(t, local.WriteBytes(j.Changes[0].Path, after))
	must(t, local.Write(JournalPath(home), j))
	model := &managedBackendModel{Scheduler: backend, loaded: map[scheduler.Ref]bool{}}
	must(t, Recover(home, func(string) (scheduler.Scheduler, error) { return model, nil }, noLock))
	restored, err := os.ReadFile(j.Changes[0].Path)
	must(t, err)
	if !bytes.Equal(restored, before) || TransactionPending(home) || len(model.loaded) != 0 {
		t.Fatal("combined rollback changed manager or lost config")
	}
	var cfg config.Config
	must(t, json.Unmarshal(restored, &cfg))
	if !cfg.NativeAuthorityProtection || !cfg.SearchLifecycleProtection || !cfg.SearchIndexing || cfg.NativeRoots[0].Revision != "one" {
		t.Fatal("rollback lost composed authority", cfg)
	}
	// Recovery remains idempotent once its journal has been removed.
	must(t, Recover(home, func(string) (scheduler.Scheduler, error) {
		t.Fatal("completed recovery reached manager")
		return nil, nil
	}, noLock))
}
