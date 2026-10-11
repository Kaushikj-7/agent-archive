package codex

import (
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// A desktop session whose only retained user text is open-page context has
// no prompt title; the row shows Codex's own thread name, never the wrapper.
func TestOpenPageOnlySessionShowsThreadName(t *testing.T) {
	t.Parallel()
	_, request := labelFixture(t)
	bundle := request.Bundle
	bundle.Capture.FilterVersion = "15"
	bundle.NativeRecords = append(bundle.NativeRecords[:1], map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": `<external_codex_apps_open_page>{"page_id":null}</external_codex_apps_open_page>`}}}})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reference := archive.SourceReference{Key: "k", SHA256: "0000000000000000000000000000000000000000000000000000000000000000"}
	derive := func(b archive.SourceBundle) archive.Metadata {
		t.Helper()
		analysis, err := (Parser{}).Parse(t.Context(), b)
		m, err := archive.BuildMetadataWithAnalysis(b, analysis, err, "machine", at, at, reference, archive.ParserInfo{Version: (Parser{}).Version()})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	if m := derive(bundle); m.Title != "" || archive.DisplayTitle(m) != "" {
		t.Fatalf("unnamed: title %q display %q", m.Title, archive.DisplayTitle(m))
	}
	label := archive.SessionLabel{NativeID: labelTestID, State: archive.SessionLabelPresent, Name: "Investigate upload flake", Source: archive.SessionLabelIndex, Contract: LabelContract}
	bundle.SupplementalEvidence = []archive.SupplementalEvidence{label.Evidence(at, "codex")}
	if m := derive(bundle); m.Title != "" || archive.DisplayTitle(m) != label.Name {
		t.Fatalf("named: title %q display %q", m.Title, archive.DisplayTitle(m))
	}
}
