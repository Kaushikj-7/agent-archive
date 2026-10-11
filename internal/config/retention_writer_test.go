package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// This opt-in probe executes a protected pre-forever writer. The scalar-schema
// published writer probe has a different contract and remains separate.
func TestPriorRetentionWriterRefusesForeverBeforeMutation(t *testing.T) {
	binary := os.Getenv("AGENT_ARCHIVE_PRE_FOREVER_BINARY")
	if binary == "" {
		t.Skip("requires an isolated protected pre-forever writer binary")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("pre-forever writer must be an absolute executable")
	}
	raw, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("pre-forever binary SHA-256: %x", sha256.Sum256(raw))
	root, pause := publishedPauseFixture(t, binary)
	finite := publishedWriterFiniteControl()
	discovery := finite
	discovery.Discovery = &DiscoveryConfig{Enabled: false}
	for name, cfg := range map[string]Config{"finite": finite, "discovery-finite": discovery} {
		home := filepath.Join(root, name)
		if err := Save(home, cfg); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(filepath.Join(home, "config.json"))
		if err != nil {
			t.Fatal(err)
		}
		var original Config
		if err := json.Unmarshal(before, &original); err != nil {
			t.Fatal(err)
		}
		outside := retentionProbeTree(t, root)
		for path := range outside {
			if path == home || strings.HasPrefix(path, home+string(filepath.Separator)) {
				delete(outside, path)
			}
		}
		if out, err := pause(home); err != nil {
			t.Fatalf("supported %s control failed: %v %s", name, err, out)
		}
		after, err := os.ReadFile(filepath.Join(home, "config.json"))
		var saved Config
		if err != nil || bytes.Equal(before, after) || json.Unmarshal(after, &saved) != nil || !saved.Paused || saved.RetentionDays != cfg.RetentionDays || saved.MachineID != cfg.MachineID || !reflect.DeepEqual(saved.Discovery, cfg.Discovery) {
			t.Fatalf("%s control did not pause the intended configuration: %v", name, err)
		}
		saved.Paused, saved.PauseGeneration = original.Paused, original.PauseGeneration
		if !reflect.DeepEqual(original, saved) {
			t.Fatalf("%s control changed settings beyond its pause transition", name)
		}
		afterOutside := retentionProbeTree(t, root)
		for path := range afterOutside {
			if path == home || strings.HasPrefix(path, home+string(filepath.Separator)) {
				delete(afterOutside, path)
			}
		}
		if !reflect.DeepEqual(outside, afterOutside) {
			t.Fatalf("%s control changed another fixture or disposable user home", name)
		}
	}
	forever, sticky, staged := finite, finite, finite
	forever.RetentionDays = 0
	sticky.RetentionProtection = true
	staged.RetentionDays, staged.ImportStagingProtection = 0, true
	for name, cfg := range map[string]Config{"forever": forever, "sticky-finite": sticky, "staged-forever": staged} {
		home := filepath.Join(root, name)
		if err := Save(home, cfg); err != nil {
			t.Fatal(err)
		}
		before := retentionProbeTree(t, root)
		out, err := pause(home)
		message := string(out)
		if err == nil || !strings.Contains(message, "configuration requires a supported writer fence") || !strings.Contains(message, filepath.Join(home, "config.json")) {
			t.Fatalf("pre-forever writer did not refuse %s in the intended home: %v %s", name, err, out)
		}
		if !reflect.DeepEqual(before, retentionProbeTree(t, root)) {
			t.Fatalf("refused %s changed disposable configuration, home or service state", name)
		}
		t.Logf("%s: refused without mutation", name)
	}
}

type retentionProbeEntry struct {
	Mode fs.FileMode
	Data string
}

func retentionProbeTree(t *testing.T, root string) map[string]retentionProbeEntry {
	t.Helper()
	entries := make(map[string]retentionProbeEntry)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		var data []byte
		if info.Mode().IsRegular() {
			data, err = os.ReadFile(path)
			if err != nil {
				return err
			}
		}
		entries[path] = retentionProbeEntry{Mode: info.Mode(), Data: string(data)}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}
