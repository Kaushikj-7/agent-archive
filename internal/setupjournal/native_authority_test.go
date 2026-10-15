package setupjournal

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/fileapply"
	"github.com/wangjohn/agent-archive/internal/local"
)

func nativeJournalFixture(t *testing.T) (Journal, []byte, []byte) {
	t.Helper()
	cfg := config.Config{RetentionDays: 90, NativeRoots: []archive.NativeRoot{{Agent: "synthetic", Configured: "/before", Canonical: "/before", Revision: "one"}}}
	before, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.NativeRoots[0] = archive.NativeRoot{Agent: "synthetic", Configured: "/after", Canonical: "/after", Revision: "two"}
	after, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return Journal{FilesOnly: true, Changes: []fileapply.Change{{Path: filepath.Join(t.TempDir(), "config.json"), Before: before, After: after, Existed: true, Mode: 0600}}}, before, after
}

func TestNativeJournalWireRefusesOldRecoveryAndPreservesSemantics(t *testing.T) {
	j, _, _ := nativeJournalFixture(t)
	for _, filesOnly := range []bool{false, true} {
		j.FilesOnly = filesOnly
		raw, err := json.Marshal(j)
		if err != nil {
			t.Fatal(err)
		}
		var old struct {
			FilesOnly bool `json:"files_only"`
		}
		if json.Unmarshal(raw, &old) == nil {
			t.Fatal("old recovery silently skipped native fence")
		}
		var restored Journal
		if err = json.Unmarshal(raw, &restored); err != nil || restored.FilesOnly != filesOnly || !restored.NativeAuthorityProtection {
			t.Fatal("journal recovery semantics changed", err)
		}
		for _, mutate := range []func([]byte) []byte{
			func(raw []byte) []byte { return bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":2`), 1) },
			func(raw []byte) []byte {
				return bytes.Replace(raw, []byte(`"native_authority_protection":true`), []byte(`"native_authority_protection":false`), 1)
			},
			func(raw []byte) []byte {
				return bytes.Replace(raw, []byte(`"files_only":{"version":1,"writer":"native-authority-v16","value":true}`), []byte(`"files_only":true`), 1)
			},
		} {
			changed := mutate(raw)
			if bytes.Equal(changed, raw) {
				continue
			}
			if json.Unmarshal(changed, &restored) == nil {
				t.Fatal("unsupported recovery fence accepted")
			}
		}
	}
}

func TestPriorSetupCannotRecoverNativeAuthorityJournal(t *testing.T) {
	binary := os.Getenv("AGENT_ARCHIVE_SOURCE_OLD_BINARY")
	if binary == "" {
		t.Skip("requires exact isolated prior source build; not published acceptance")
	}
	root := t.TempDir()
	userHome := filepath.Join(root, "user")
	home := filepath.Join(root, "archive")
	bin := filepath.Join(root, "bin")
	temp := filepath.Join(root, "tmp")
	for _, path := range []string{userHome, home, bin, temp} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, tool := range []string{"launchctl", "systemctl", "security"} {
		if err := os.WriteFile(filepath.Join(bin, tool), []byte("#!/bin/sh\n: > \"$HOME/unexpected-service\"\nexit 99\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	j, before, after := nativeJournalFixture(t)
	j.Changes[0].Path = filepath.Join(home, "config.json")
	if err := os.WriteFile(j.Changes[0].Path, after, 0600); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(before, after) {
		t.Fatal("inert test mutation")
	}
	if err := local.Write(JournalPath(home), j); err != nil {
		t.Fatal(err)
	}
	journalBefore, err := os.ReadFile(JournalPath(home))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "setup", "--yes")
	cmd.Dir = userHome
	cmd.Env = []string{"HOME=" + userHome, "AGENT_ARCHIVE_HOME=" + home, "TMPDIR=" + temp, "PATH=" + bin, "NO_COLOR=1", "AWS_EC2_METADATA_DISABLED=true"}
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "cannot unmarshal object") || !strings.Contains(string(out), "bool") {
		t.Fatal("prior setup did not reject typed recovery fence", string(out), err)
	}
	cfgAfter, err := os.ReadFile(j.Changes[0].Path)
	if err != nil || !bytes.Equal(after, cfgAfter) {
		t.Fatal("prior setup rolled back native authority", err)
	}
	journalAfter, err := os.ReadFile(JournalPath(home))
	if err != nil || !bytes.Equal(journalBefore, journalAfter) {
		t.Fatal("prior setup rewrote recovery authority", err)
	}
	if _, err := os.Stat(filepath.Join(userHome, "unexpected-service")); !os.IsNotExist(err) {
		t.Fatal("prior setup touched machine services")
	}
}
