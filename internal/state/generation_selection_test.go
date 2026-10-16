package state

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

func TestGenerationRecoveryRefusesChangedPriorCommittedSelection(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"unchanged", "metadata", "source-reference"} {
		t.Run(change, func(t *testing.T) {
			s, reg, at := generationFixture(t)
			p, err := s.LoadPublishedState(reg.ArchiveSessionID)
			if err != nil {
				t.Fatal(err)
			}
			bundle, _, _ := p.LastPublished()
			bundle.Capture.AdapterName = "test"
			packed, err := archive.BuildCompressedSource(bundle)
			if err != nil {
				t.Fatal(err)
			}
			key, err := archive.SourceObjectKey(bundle, packed.SHA256)
			if err != nil {
				t.Fatal(err)
			}
			metadata := archive.Metadata{SchemaVersion: archive.MetadataSchemaVersion, SessionID: reg.ArchiveSessionID, NativeSessionID: reg.NativeSessionID, ProjectID: reg.ProjectID, Harness: reg.Harness, CapturedAt: bundle.Capture.CapturedAt, SourceBundle: archive.SourceReference{Key: key, SHA256: packed.SHA256, CompressedBytes: len(packed.Bytes)}}
			raw, err := json.Marshal(metadata)
			if err != nil {
				t.Fatal(err)
			}
			if err := p.Save(bundle, reg.RegisteredAt, CacheStatusPublished, raw); err != nil {
				t.Fatal(err)
			}
			if err := p.SaveBlocked(bundle, reg.RegisteredAt, BlockedReasonTranscriptRewritten); err != nil {
				t.Fatal(err)
			}
			stop := errors.New("stop after journal")
			s.onIndexStep = func(step string) error {
				if step == "generation-journal" {
					return stop
				}
				return nil
			}
			if _, err := s.BeginGenerationRecovery(reg.ArchiveSessionID, at, generationBuilder(at)); !errors.Is(err, stop) {
				t.Fatal(err)
			}
			if change != "unchanged" {
				if change == "metadata" {
					metadata.MetadataDerivedAt = at.Add(time.Hour)
				} else {
					metadata.SourceBundle.SHA256 = strings.Repeat("a", 64)
					metadata.SourceBundle.Key = "sessions/" + reg.Harness.Name + "/" + reg.ArchiveSessionID + "/source." + metadata.SourceBundle.SHA256 + ".jsonl.gz"
					if _, err := metadata.SourceReferences(); err != nil {
						t.Fatal(err)
					}
				}
				raw, err = json.Marshal(metadata)
				if err != nil {
					t.Fatal(err)
				}
				if err := p.CacheMetadata(raw); err != nil {
					t.Fatal(err)
				}
			}
			s.onIndexStep = nil
			err = s.ResumeGenerationRecoveries(t.Context())
			if change != "unchanged" {
				if !errors.Is(err, ErrSessionIndexRecoveryRequired) {
					t.Fatalf("routing changed after predecessor mutation: %v", err)
				}
				current, found, e := s.LoadRegistration(reg.ArchiveSessionID)
				if e != nil || !found || current.CaptureFrozen {
					t.Fatalf("changed predecessor froze routing: %v %v", found, e)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestGenerationRecoveryPriorWriterCompatibility(t *testing.T) {
	// Sequential: one run-owned prior executable exercises real process boundaries.
	executable := os.Getenv("AA_GENERATION_OLD_READER_EXECUTABLE")
	if executable == "" {
		t.Skip("run-owned pinned prior reader executable not supplied")
	}
	for _, phase := range []string{"journal", "complete", "legacy-journal"} {
		t.Run(phase, func(t *testing.T) {
			s, reg, at := generationFixture(t)
			probe := func(mode string) {
				t.Helper()
				ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, executable, "-test.run=^TestGenerationOldReaderProbe$", "-test.count=1")
				cmd.Env = append(os.Environ(), "AA_GENERATION_PROBE_HOME="+s.Home(), "AA_GENERATION_PROBE_MODE="+mode, "AA_GENERATION_PROBE_ID="+reg.ArchiveSessionID, "AA_GENERATION_PROBE_NATIVE="+reg.NativeSessionID, "AA_GENERATION_PROBE_AT="+at.Format(time.RFC3339Nano))
				output, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("prior binary %s: %v %s", mode, err, output)
				}
			}
			if phase == "legacy-journal" {
				probe("create-journal")
				if err := s.ResumeGenerationRecoveries(t.Context()); err != nil {
					t.Fatal(err)
				}
				receipt, found, err := s.loadGenerationRecovery(t.Context(), reg.ArchiveSessionID)
				if err != nil || !found || receipt.Version != 1 {
					t.Fatalf("old journal promoted: %#v %v %v", receipt, found, err)
				}
				probe("accept-lookup")
				return
			}
			if phase == "journal" {
				s.onIndexStep = func(step string) error {
					if step == "generation-journal" {
						return errors.New("interrupt before fence")
					}
					return nil
				}
			}
			_, err := s.BeginGenerationRecovery(reg.ArchiveSessionID, at, generationBuilder(at))
			if phase == "journal" {
				if err == nil {
					t.Fatal("missing interruption")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			before := settledProtocolInventory(t, s.Home())
			probe("refuse-lookup")
			probe("refuse-resume")
			if after := settledProtocolInventory(t, s.Home()); !reflect.DeepEqual(before, after) {
				t.Fatal("prior binary changed generation2 state")
			}
		})
	}
}

func TestGenerationRecoveryRefusesIncompleteVersionTwoAuthority(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"prior_metadata_sha256", "prior_source_set_sha256", "future-version", "downgraded-version"} {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			s, reg, at := generationFixture(t)
			s.onIndexStep = func(step string) error {
				if step == "generation-journal" {
					return errors.New("stop before fence")
				}
				return nil
			}
			if _, err := s.BeginGenerationRecovery(reg.ArchiveSessionID, at, generationBuilder(at)); err == nil {
				t.Fatal("missing interruption")
			}
			path := s.generationRecoveryPath(reg.ArchiveSessionID)
			var wire map[string]any
			if err := local.Read(path, &wire); err != nil {
				t.Fatal(err)
			}
			switch field {
			case "future-version":
				wire["version"] = 3
			case "downgraded-version":
				wire["version"] = 1
			default:
				delete(wire, field)
			}
			if err := local.Write(path, wire); err != nil {
				t.Fatal(err)
			}
			s.onIndexStep = nil
			before := settledProtocolInventory(t, s.Home())
			if err := s.ResumeGenerationRecoveries(t.Context()); !errors.Is(err, ErrSessionIndexRecoveryRequired) {
				t.Fatalf("invalid generation authority replayed: %v", err)
			}
			if _, found, err := s.GenerationSuccessor(reg.ArchiveSessionID); !errors.Is(err, ErrSessionIndexRecoveryRequired) || found {
				t.Fatalf("invalid successor routed: %v %v", found, err)
			}
			if after := settledProtocolInventory(t, s.Home()); !reflect.DeepEqual(before, after) {
				t.Fatal("invalid authority changed routing")
			}
		})
	}
}
