package collector

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestSourceInspectionPreservesPostScanSizeAndLateRefusalPrecedence(t *testing.T) {
	rows := strings.Repeat(`{"type":"message","content":"synthetic visible text"}`+"\n", 12)
	const limit = 400
	for _, tail := range []string{"", `{"type":"session_meta","payload":{"id":"x","history_mode":"unsupported"}}` + "\n"} {
		path := writeTranscript(t, t.TempDir(), "ordinary.jsonl", rows+tail)
		_, filter, ok := testSources.LookupSources("codex")
		if !ok {
			t.Fatal("codex unavailable")
		}
		reg := archive.SessionRegistration{Harness: archive.Harness{Name: "codex"}, TranscriptPath: path, SessionStartedAt: time.Time{}}
		reader, _ := newSourceReader(reg, Options{Sources: testSources})
		r := reader.(providerReader)
		_, _, want := r.Filter(t.Context(), filter, limit)
		var summary *agentapi.ImportSummary
		_, _, got := r.filter(t.Context(), filter, limit, &summary)
		if got == nil || want == nil || got.Error() != want.Error() {
			t.Fatalf("summary error %v retained error %v", got, want)
		}
		if tail == "" && !errors.Is(got, errTranscriptTooLarge) {
			t.Fatalf("lost final output limit: %v", got)
		}
		if tail != "" && errors.Is(got, errTranscriptTooLarge) {
			t.Fatalf("output limit concealed late metadata refusal: %v", got)
		}
	}
}
