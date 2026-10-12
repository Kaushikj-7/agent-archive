package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// This opt-in acceptance check executes the actual published v0.1.1 writer,
// not a source build or a decoder model. Extended macOS supplies its asset.
func publishedWriterBinary(t *testing.T) string {
	t.Helper()
	binary := os.Getenv("AGENT_ARCHIVE_OLD_BINARY")
	if binary == "" {
		t.Skip("requires the checksum-pinned published Darwin v0.1.1 binary")
	}
	if runtime.GOOS != "darwin" || !filepath.IsAbs(binary) {
		t.Fatal("published acceptance requires a native Darwin absolute executable")
	}
	raw, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	want := ""
	switch runtime.GOARCH {
	case "amd64":
		want = "c8fff68b623a7e0143503adce2d83c6494d7efa55fa0724eb4f76ae54c66255e"
	case "arm64":
		want = "09f08430627cc0c867afd41ce7eb2c3e61859b640dfb2994323577847eee06e6"
	default:
		t.Fatal("unsupported published Darwin architecture")
	}
	if hex.EncodeToString(digest[:]) != want {
		t.Fatal("published binary checksum differs from pinned v0.1.1 asset")
	}
	return binary
}

func publishedPauseFixture(t *testing.T, binary string) (string, func(string) ([]byte, error)) {
	t.Helper()
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	userHome, temp, bin := filepath.Join(root, "home"), filepath.Join(root, "tmp"), filepath.Join(root, "bin")
	for _, dir := range []string{userHome, temp, bin} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	// Any unexpected scheduler or Keychain invocation fails and leaves evidence.
	marker := filepath.Join(userHome, "unexpected-service")
	for _, name := range []string{"launchctl", "systemctl", "security"} {
		stub := "#!/bin/sh\n: > \"$HOME/unexpected-service\"\nexit 99\n"
		if err := os.WriteFile(filepath.Join(bin, name), []byte(stub), 0700); err != nil {
			t.Fatal(err)
		}
	}
	pause := func(home string) ([]byte, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, "pause")
		cmd.Dir = userHome
		cmd.Env = []string{"HOME=" + userHome, "AGENT_ARCHIVE_HOME=" + home, "TMPDIR=" + temp, "PATH=" + bin, "NO_COLOR=1", "AWS_EC2_METADATA_DISABLED=true"}
		out, err := cmd.CombinedOutput()
		if ctx.Err() != nil {
			t.Fatalf("published pause exceeded deadline: %v", ctx.Err())
		}
		if _, e := os.Stat(marker); !os.IsNotExist(e) {
			t.Fatal("published pause invoked a scheduler or Keychain command")
		}
		return out, err
	}
	return root, pause
}

func TestPublishedWriterRefusesProtectedConfiguration(t *testing.T) {
	t.Parallel()
	assertOldWriterRefusesProtectedConfiguration(t, publishedWriterBinary(t))
}

func TestSourceBuiltWriterRefusesProtectedConfiguration(t *testing.T) {
	binary := os.Getenv("AGENT_ARCHIVE_SOURCE_OLD_BINARY")
	if binary == "" {
		t.Skip("requires isolated source-built prior writer")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("source-built writer must be an absolute executable")
	}
	assertOldWriterRefusesProtectedConfiguration(t, binary)
}

func assertOldWriterRefusesProtectedConfiguration(t *testing.T, binary string) {
	t.Helper()
	root, pause := publishedPauseFixture(t, binary)
	control := filepath.Join(root, "control")
	if err := os.Mkdir(control, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := publishedWriterFiniteControl()
	if err := Save(control, cfg); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(control, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if out, err := pause(control); err != nil {
		t.Fatalf("published scalar-schema control failed: %v %s", err, out)
	}
	after, err := os.ReadFile(filepath.Join(control, "config.json"))
	if err != nil || bytes.Equal(before, after) {
		t.Fatal("published control did not rewrite the intended archive root", err)
	}
	var paused struct {
		Paused bool `json:"paused"`
	}
	if err := json.Unmarshal(after, &paused); err != nil || !paused.Paused {
		t.Fatal("published control did not persist paused=true", err)
	}
	for _, fixture := range publishedWriterProtectedFixtures() {
		home := filepath.Join(root, fixture.name)
		if err := os.Mkdir(home, 0700); err != nil {
			t.Fatal(err)
		}
		cfg = fixture.cfg
		if err := Save(home, cfg); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(filepath.Join(home, "config.json"))
		if err != nil {
			t.Fatal(err)
		}
		out, err := pause(home)
		if err == nil || !strings.Contains(string(out), "cannot unmarshal object") || !strings.Contains(string(out), "schema_version") || !strings.Contains(string(out), "type int") || !strings.Contains(string(out), filepath.Join(home, "config.json")) {
			t.Fatalf("published writer did not reject protected schema in intended root (%s): %v %s", fixture.name, err, out)
		}
		after, err := os.ReadFile(filepath.Join(home, "config.json"))
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("published writer changed protected config (%s): %v", fixture.name, err)
		}
	}
}

func publishedWriterFiniteControl() Config {
	return Config{RetentionDays: DefaultRetentionDays, MachineID: "synthetic-old-writer", Archive: archive.Config{Enabled: true}}
}

type publishedWriterProtectedFixture struct {
	name   string
	cfg    Config
	writer configWriter
}

func publishedWriterProtectedFixtures() []publishedWriterProtectedFixture {
	disabled, enabled := publishedWriterFiniteControl(), publishedWriterFiniteControl()
	disabled.Discovery = &DiscoveryConfig{Enabled: false}
	enabled.Discovery = &DiscoveryConfig{Enabled: true}
	forever := publishedWriterFiniteControl()
	forever.RetentionDays = 0
	stagedFinite, stagedForever := publishedWriterFiniteControl(), forever
	stagedFinite.ImportStagingProtection = true
	stagedForever.ImportStagingProtection = true
	historical, _ := historicalPolicyFixture()
	return []publishedWriterProtectedFixture{
		{name: "historical-projects", cfg: historical, writer: historicalProjectWriter},
		{name: "consent-disabled-finite", cfg: disabled, writer: discoveryWriter},
		{name: "consent-enabled-finite", cfg: enabled, writer: discoveryWriter},
		{name: "forever", cfg: forever, writer: retentionWriter},
		{name: "staging-finite", cfg: stagedFinite, writer: importStagingWriter},
		{name: "staging-forever", cfg: stagedForever, writer: importStagingWriter},
	}
}

func TestPublishedWriterFixturesKeepFiniteControlAndProtectedPromises(t *testing.T) {
	control := t.TempDir()
	if err := Save(control, publishedWriterFiniteControl()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(control, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var legacy struct {
		SchemaVersion int `json:"schema_version"`
		RetentionDays int `json:"retention_days"`
	}
	if err := json.Unmarshal(raw, &legacy); err != nil || legacy.RetentionDays != DefaultRetentionDays {
		t.Fatalf("finite control must remain scalar-schema: %s %v", raw, err)
	}
	loaded, _, err := Load(control)
	if err != nil || loaded.RetentionProtection || loaded.ImportStagingProtection || loaded.Discovery != nil {
		t.Fatalf("finite control gained protection: %+v %v", loaded, err)
	}
	for _, fixture := range publishedWriterProtectedFixtures() {
		t.Run(fixture.name, func(t *testing.T) {
			home := t.TempDir()
			if err := Save(home, fixture.cfg); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(filepath.Join(home, "config.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(raw, &legacy); err == nil {
				t.Fatal("protected fixture accepted by scalar schema decoder")
			}
			var fenced struct {
				SchemaVersion writerVersion `json:"schema_version"`
			}
			if err := json.Unmarshal(raw, &fenced); err != nil || fenced.SchemaVersion.Writer != fixture.writer {
				t.Fatalf("wrong protection: %s %v", raw, err)
			}
			loaded, _, err := Load(home)
			if err != nil || loaded.RetentionDays != fixture.cfg.RetentionDays || loaded.ImportStagingProtection != fixture.cfg.ImportStagingProtection {
				t.Fatalf("protected fixture lost retention/staging: %+v %v", loaded, err)
			}
		})
	}
}
