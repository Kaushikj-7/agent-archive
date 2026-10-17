package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/destination"
	"github.com/wangjohn/agent-archive/internal/fileapply"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
)

var museReplayWriterFixtureDirectory = flag.String("muse-replay-writer-fixture-directory", "", "export synthetic replay protection fixtures to an explicit acceptance directory")

// Export is opt-in. The ordinary test uses TempDir; execution of a pinned older
// executable belongs to a separate bounded qualification packet and lease.
func TestMuseReplayProtectedWriterFixtures(t *testing.T) {
	directory := *museReplayWriterFixtureDirectory
	if directory == "" {
		directory = t.TempDir()
	} else if !filepath.IsAbs(directory) {
		t.Fatal("absolute fixture directory required")
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	project := filepath.Join(directory, "native-project")
	nativeHome := filepath.Join(directory, "native-home")
	nativeRoot := archive.NativeRoot{Agent: "muse", Configured: nativeHome, Canonical: filepath.Join(nativeHome, "sessions"), Revision: "fixture-root"}
	cfg := config.Config{MachineID: "00000000000000000000000000000001", RetentionDays: 90, Harnesses: []string{"muse"}, Storage: destination.Config{Provider: destination.ProviderS3, Bucket: "synthetic-old-writer", Prefix: "fixture", AWSProfile: "synthetic"}, DestinationSince: at.Add(-time.Hour), NativeRoots: []archive.NativeRoot{nativeRoot}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: project, ProjectID: archive.ProjectID(project), Included: true, ActivatedAt: at.Add(-time.Hour)}}}}
	active, err := config.ReconcileMuseCapture(cfg, config.Config{}, at)
	must(t, err)
	serialize := func(v any) []byte {
		raw, err := json.MarshalIndent(v, "", "  ")
		must(t, err)
		return append(raw, '\n')
	}
	ordinary := serialize(active)
	active.NativeReplayProtection = true
	strong := serialize(active)
	sticky := active
	sticky.Archive.Enabled = false
	sticky, err = config.ReconcileMuseCapture(sticky, active, at.Add(time.Hour))
	must(t, err)
	fixtures := map[string][]byte{"config-ordinary-admission.json": ordinary, "config-replay-active.json": strong, "config-replay-sticky.json": serialize(sticky)}
	for _, managed := range []bool{false, true} {
		form := "legacy"
		if managed {
			form = "managed"
		}
		for _, transition := range []struct {
			name          string
			before, after []byte
		}{{"strong-before", strong, ordinary}, {"strong-after", ordinary, strong}} {
			target := filepath.Join(directory, "owned-"+form+"-"+transition.name, "config.json")
			journal := setupjournal.Journal{FilesOnly: true, Changes: []fileapply.Change{{Path: target, Before: transition.before, After: transition.after, Existed: true, Mode: 0600}}}
			if managed {
				journal.Jobs = []setupjournal.ManagedJob{{Role: setupjournal.SearchRole, Backend: "launchd", Ref: "com.agent-archive.synthetic-replay", Definition: filepath.Join(directory, "owned-jobs", "synthetic.plist"), Intent: setupjournal.Keep}}
			}
			encoded := serialize(journal)
			var decoded setupjournal.Journal
			must(t, json.Unmarshal(encoded, &decoded))
			if !decoded.NativeReplayProtection || !decoded.NativeAdmissionProtection {
				t.Fatal("authentic serializer dropped replay floor")
			}
			fixtures["journal-"+form+"-"+transition.name+".json"] = encoded
		}
	}
	fingerprint, err := archive.NativeRootFingerprint(nativeRoot)
	must(t, err)
	intent := agentapi.AdmissionIntent{Version: 2, Harness: "muse", NativeMode: agentapi.NativeStartupCandidate, NativeSessionID: "opaque-root", ProjectRoot: project, ObservedCwd: project, DestinationID: active.DestinationID(), ObservedAt: at, Event: "SessionStart", NativeAdmission: &archive.NativeAdmission{Contract: "muse-root-admission", HookContract: "muse-root-hooks-v1", RootFingerprint: fingerprint, PolicyFingerprint: active.MuseCapture.PolicyFingerprint, WindowGeneration: active.MuseCapture.Generation}}
	for _, marker := range []struct {
		name  string
		value *archive.Replay
	}{{"ordinary", nil}, {"marker", &archive.Replay{}}, {"identifier", &archive.Replay{RunID: "synthetic-old-writer-run"}}} {
		intent.Replay = marker.value
		raw := serialize(intent)
		var decoded agentapi.AdmissionIntent
		must(t, json.Unmarshal(raw, &decoded))
		if !museReplayEqual(decoded.Replay, marker.value) {
			t.Fatal("queue serializer changed normalized marker")
		}
		fixtures["queue-v2-"+marker.name+".json"] = raw
	}
	manifest := map[string]struct {
		Bytes  int    `json:"bytes"`
		SHA256 string `json:"sha256"`
	}{}
	for name, raw := range fixtures {
		path := filepath.Join(directory, name)
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		must(t, err)
		_, writeErr := file.Write(raw)
		closeErr := file.Close()
		must(t, writeErr)
		must(t, closeErr)
		digest := sha256.Sum256(raw)
		manifest[name] = struct {
			Bytes  int    `json:"bytes"`
			SHA256 string `json:"sha256"`
		}{len(raw), hex.EncodeToString(digest[:])}
	}
	manifestBytes := serialize(manifest)
	must(t, os.WriteFile(filepath.Join(directory, "manifest.json"), manifestBytes, 0600))
}
