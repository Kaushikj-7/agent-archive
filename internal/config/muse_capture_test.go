package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/destination"
)

func museWindowFixture(at time.Time) Config {
	return Config{RetentionDays: 90, MachineID: "synthetic", Harnesses: []string{"muse"}, Storage: destination.Config{Provider: destination.ProviderS3, Bucket: "synthetic", Prefix: "fixture"}, DestinationSince: at.Add(-time.Hour), NativeRoots: []archive.NativeRoot{{Agent: "muse", Configured: "/muse", Canonical: "/muse/sessions", Revision: "one"}}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: "/project", ProjectID: archive.ProjectID("/project"), Included: true, ActivatedAt: at.Add(-time.Hour)}}}}
}

func TestMuseCaptureIssuanceFloorSurvivesInactiveWindows(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	first, err := ReconcileMuseCapture(museWindowFixture(at), Config{}, at)
	if err != nil {
		t.Fatal(err)
	}
	for _, pause := range []bool{false, true} {
		t.Run(map[bool]string{false: "disable", true: "pause"}[pause], func(t *testing.T) {
			t.Parallel()
			off := first
			if pause {
				off.Paused = true
				off.PauseGeneration = "paused"
			} else {
				off.Archive.Enabled = false
			}
			off, err := ReconcileMuseCapture(off, first, at.Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(off)
			if err != nil {
				t.Fatal(err)
			}
			if off.MuseCapture != nil || !off.MuseCaptureLastIssuedAt.Equal(at) {
				t.Fatal("inactive window lost issuance floor")
			}
			if err := json.Unmarshal(raw, &off); err != nil {
				t.Fatal(err)
			}
			for _, nextTime := range []time.Time{time.Time{}, at.Add(-time.Second), at} {
				on := off
				on.Paused = false
				on.Archive.Enabled = true
				on.PauseGeneration = "resumed"
				if _, err := ReconcileMuseCapture(on, off, nextTime); err == nil {
					t.Fatalf("backward/equal issuance accepted: %v", nextTime)
				}
			}
			on := off
			on.Paused = false
			on.Archive.Enabled = true
			on.PauseGeneration = "resumed"
			on, err = ReconcileMuseCapture(on, off, at.Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if on.MuseCapture.Generation == first.MuseCapture.Generation || !on.MuseCapture.Since.Equal(at.Add(time.Hour)) || !on.MuseCaptureLastIssuedAt.Equal(on.MuseCapture.Since) {
				t.Fatal("forward resume did not rotate")
			}
		})
	}
	unrelated := first
	unrelated.MachineName = "renamed"
	unrelated, err = ReconcileMuseCapture(unrelated, first, at.Add(time.Hour))
	if err != nil || *unrelated.MuseCapture != *first.MuseCapture || !unrelated.MuseCaptureLastIssuedAt.Equal(at) {
		t.Fatal("unrelated save renewed authority", err)
	}
}

func TestMuseCaptureProtectedSelectorsAndFloorAreClosed(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	cfg, err := ReconcileMuseCapture(museWindowFixture(at), Config{}, at)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, bad := range []string{
		strings.Replace(text, `"admission":"muse-root-admission"`, `"admission":"future"`, 1),
		strings.Replace(text, `"muse_capture_last_issued_at":"2026-10-16T00:00:00Z",`, "", 1),
		strings.Replace(text, `"muse_capture_last_issued_at":"2026-10-16T00:00:00Z"`, `"muse_capture_last_issued_at":null`, 1),
		strings.Replace(text, `"muse_capture_last_issued_at":"2026-10-16T00:00:00Z"`, `"muse_capture_last_issued_at":"2026-10-16T00:00:01Z"`, 1),
		strings.Replace(text, `"muse_capture":{`, `"muse_capture":{"future":true,`, 1),
		strings.Replace(text, `"muse_capture_last_issued_at":`, `"MUSE_CAPTURE_LAST_ISSUED_AT":`, 1),
	} {
		if bad == text {
			t.Fatal("mutation did not change fixture")
		}
		var next Config
		if json.Unmarshal([]byte(bad), &next) == nil {
			t.Fatalf("future/ambiguous authority accepted: %s", bad)
		}
	}
}
