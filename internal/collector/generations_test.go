package collector

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestGenerationRecoveryRequiresObservedNativeIdentity(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		harness    string
		transcript string
		refuse     bool
	}{
		{"codex matching metadata", "codex", `{"type":"session_meta","payload":{"id":"native-1"}}` + "\n" + codexTranscript, false},
		{"codex different metadata", "codex", `{"type":"session_meta","payload":{"id":"different"}}` + "\n" + codexTranscript, true},
		{"codex redacted metadata", "codex", `{"type":"session_meta","payload":{"id":"sk-abcdefghijklmnopqrstuv"}}` + "\n" + codexTranscript, true},
		{"codex blank hook identity", "codex", codexTranscript, false},
		{"claude different owner", "claude", `{"type":"user","sessionId":"different","message":{"role":"user","content":"current"}}`, true},
		{"claude matching owner", "claude", `{"type":"user","sessionId":"native-1","message":{"role":"user","content":"current"}}`, false},
		{"claude copied history", "claude", `{"type":"user","sessionId":"copied","message":{"role":"user","content":"earlier"}}
{"type":"user","sessionId":"native-1","message":{"role":"user","content":"current"}}`, false},
		{"claude blank hook identity", "claude", `{"type":"user","message":{"role":"user","content":"current"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg := registration(t, writeTranscript(t, t.TempDir(), "native.jsonl", tc.transcript))
			reg.Harness.Name = tc.harness
			at := reg.RegisteredAt.Add(time.Hour)
			build, closePreview, err := PrepareGenerationRecovery(t.Context(), reg, at, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", RepoKey: func(string) string { return "" }})
			defer closePreview()
			if tc.refuse {
				if err == nil || build != nil {
					t.Fatal("different observed native identity admitted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			_, pending, err := build(reg, "successor")
			if err != nil || pending.Bundle.NativeSessionID != reg.NativeSessionID || pending.Bundle.PreviousGenerationID != reg.ArchiveSessionID || pending.Bundle.Capture.FilterVersion != archive.FilterVersion {
				t.Fatalf("legitimate recovery changed identity or privacy: %#v %v", pending.Bundle, err)
			}
		})
	}
}

func TestGenerationRecoveryPreservesMismatchAndCapturesNewActivity(t *testing.T) {
	t.Parallel()
	for _, mismatch := range []bool{false, true} {
		t.Run(map[bool]string{false: "compaction", true: "filter-mismatch"}[mismatch], func(t *testing.T) {
			t.Parallel()
			s, cloud := newTestStore(t), storagetest.NewMemoryStore()
			at := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
			dir := publishCodexSession(t, s, cloud, at)
			writeTranscript(t, dir, "codex.jsonl", truncatedCodexTranscript)
			opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return at.Add(time.Hour) }, RepoKey: func(string) string { return "" }}
			if result, err := Run(context.Background(), s, cloud, opts); err != nil || len(result.Errors) != 0 {
				t.Fatalf("block: %#v %v", result, err)
			}
			current := truncatedCodexTranscript
			if mismatch {
				editRetainedRecords(t, s, func(records []map[string]any) []map[string]any {
					for _, record := range records {
						if payload, ok := record["payload"].(map[string]any); ok {
							payload["content"] = "earlier filtered output"
						}
					}
					return records
				})
				simulateFilterUpgrade(t, s)
				current = codexTranscript + "\n" + `{"type":"response_item","id":"m2","payload":{"type":"message","role":"assistant","content":"current recovered activity"}}`
				writeTranscript(t, dir, "codex.jsonl", current)
				opts.Now = func() time.Time { return at.Add(2 * time.Hour) }
				if result, err := Run(context.Background(), s, cloud, opts); err != nil || len(result.Errors) != 0 {
					t.Fatalf("refilter: %#v %v", result, err)
				}
			}
			old := fetchMetadata(t, cloud, "codex", "session-1")
			reg, _, err := s.LoadRegistration("session-1")
			if err != nil {
				t.Fatal(err)
			}
			recoveryAt := at.Add(3 * time.Hour)
			builder, closePreview, err := PrepareGenerationRecovery(context.Background(), reg, recoveryAt, opts)
			defer closePreview()
			if err != nil {
				t.Fatal(err)
			}
			next, err := s.BeginGenerationRecovery(reg.ArchiveSessionID, recoveryAt, builder)
			if err != nil {
				t.Fatal(err)
			}
			opts.Now = func() time.Time { return recoveryAt.Add(time.Minute) }
			result, err := Run(context.Background(), s, cloud, opts)
			if err != nil || len(result.Errors) != 0 {
				t.Fatalf("new generation publish: %#v %v", result, err)
			}
			newMetadata := fetchMetadata(t, cloud, "codex", next)
			if newMetadata.PreviousGenerationID != "session-1" || newMetadata.ParentSessionID != "" || !newMetadata.CapturedAt.Equal(recoveryAt) {
				t.Fatalf("new metadata %#v", newMetadata)
			}
			if currentOld := fetchMetadata(t, cloud, "codex", "session-1"); currentOld.SourceBundle != old.SourceBundle || !currentOld.CapturedAt.Equal(old.CapturedAt) {
				t.Fatal("original history or age replaced")
			}
			if successor := fetchBundle(t, cloud, newMetadata); successor.PreviousGenerationID != "session-1" {
				t.Fatal("source lost generation relation")
			}
			active, found, err := s.ArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: reg.NativeSessionID})
			if err != nil || !found || active != next {
				t.Fatalf("route %s %v", active, err)
			}
			// An unchanged pass cannot repeatedly restamp the generation's retention.
			opts.Now = func() time.Time { return recoveryAt.Add(2 * time.Hour) }
			if result, err := Run(context.Background(), s, cloud, opts); err != nil || len(result.Errors) != 0 || len(result.Published) != 0 {
				t.Fatalf("unchanged generation: %#v %v", result, err)
			}
			appended := current + "\n" + `{"type":"response_item","id":"later","payload":{"type":"message","role":"assistant","content":"future exactly once"}}`
			writeTranscript(t, dir, "codex.jsonl", appended)
			if err := s.SaveRequest(next, "stop", recoveryAt.Add(3*time.Hour)); err != nil {
				t.Fatal(err)
			}
			opts.Now = func() time.Time { return recoveryAt.Add(3 * time.Hour) }
			result, err = Run(context.Background(), s, cloud, opts)
			if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 || result.Published[0] != next {
				t.Fatalf("continued activity %#v %v", result, err)
			}
			if text := recordsText(fetchBundle(t, cloud, fetchMetadata(t, cloud, "codex", next))); strings.Count(text, "future exactly once") != 1 {
				t.Fatalf("activity %s", text)
			}
			if fetchMetadata(t, cloud, "codex", "session-1").SourceBundle != old.SourceBundle {
				t.Fatal("new activity overwrote historical source")
			}
			opts.ParserVersion = "recovery-provenance-refresh"
			if result, err := Run(context.Background(), s, cloud, opts); err != nil || len(result.Errors) != 0 {
				t.Fatalf("metadata refresh: %#v %v", result, err)
			}
			refreshed := fetchMetadata(t, cloud, "codex", next)
			if refreshed.PreviousGenerationID != "session-1" || fetchBundle(t, cloud, refreshed).PreviousGenerationID != "session-1" {
				t.Fatal("metadata refresh lost generation provenance")
			}
		})
	}
}

func TestFrozenGenerationPrivacyAndParserMaintenanceNeverReadsNative(t *testing.T) {
	t.Parallel()
	s, cloud := newTestStore(t), storagetest.NewMemoryStore()
	at := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	dir := publishCodexSession(t, s, cloud, at)
	writeTranscript(t, dir, "codex.jsonl", truncatedCodexTranscript)
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return at.Add(time.Hour) }, RepoKey: func(string) string { return "" }}
	if _, err := Run(context.Background(), s, cloud, opts); err != nil {
		t.Fatal(err)
	}
	reg, _, _ := s.LoadRegistration("session-1")
	builder, closePreview, err := PrepareGenerationRecovery(context.Background(), reg, at.Add(2*time.Hour), opts)
	defer closePreview()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginGenerationRecovery("session-1", at.Add(2*time.Hour), builder); err != nil {
		t.Fatal(err)
	}
	// Corrupt current input would fail if any frozen maintenance opened it.
	if err := os.WriteFile(reg.TranscriptPath, []byte("not native JSON"), 0600); err != nil {
		t.Fatal(err)
	}
	editRetainedRecords(t, s, plantSecret)
	simulateFilterUpgrade(t, s)
	frozen, _, _ := s.LoadRegistration("session-1")
	published, err := s.LoadPublishedState("session-1")
	if err != nil {
		t.Fatal(err)
	}
	opts.SkillEvidence = config.SkillEvidenceNone
	scan := newSessionScan(context.Background(), s, cloud, frozen, state.Request{}, published, at.Add(3*time.Hour), opts)
	outcome, err := scan.run(scan.ctx)
	if err != nil || outcome != outcomePublished {
		t.Fatalf("privacy maintenance %v %v", outcome, err)
	}
	assertRefilteredSnapshot(t, cloud, at)
	opts.ParserVersion = "generation-parser-test"
	published, err = s.LoadPublishedState("session-1")
	if err != nil {
		t.Fatal(err)
	}
	scan = newSessionScan(context.Background(), s, cloud, frozen, state.Request{}, published, at.Add(4*time.Hour), opts)
	outcome, err = scan.run(scan.ctx)
	if err != nil || outcome != outcomePublished {
		t.Fatalf("parser maintenance %v %v", outcome, err)
	}
	m := fetchMetadata(t, cloud, "codex", "session-1")
	if !m.CapturedAt.Equal(at) || m.Parser.Version != opts.ParserVersion {
		t.Fatalf("age/parser changed: %#v", m)
	}
}
