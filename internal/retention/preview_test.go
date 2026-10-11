package retention

import (
	"encoding/json"
	"github.com/wangjohn/agent-archive/internal/archive"
	"strings"
	"testing"
	"time"
)

func TestRetentionPreviewUsesPreservedMeaningfulAge(t *testing.T) {
	local := newTestStore(t)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reg := registration("session-1", "")
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	p, err := local.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	b := archive.SourceBundle{SchemaVersion: archive.SourceSchemaVersion, ArchiveSessionID: reg.ArchiveSessionID, NativeSessionID: "11111111-1111-4111-8111-111111111111", ProjectID: reg.ProjectID, Capture: archive.SourceCapture{Harness: reg.Harness, CapturedAt: at}}
	ref := func(d string) archive.SourceReference {
		return archive.SourceReference{Key: "sessions/codex/session-1/source." + strings.Repeat(d, 64) + ".jsonl.gz", SHA256: strings.Repeat(d, 64), CompressedBytes: 10}
	}
	active, old := ref("a"), ref("b")
	recent := at.Add(80 * 24 * time.Hour)
	m := archive.Metadata{SchemaVersion: archive.HistoryMetadataSchemaVersion, SessionID: b.ArchiveSessionID, NativeSessionID: b.NativeSessionID, ProjectID: b.ProjectID, Harness: reg.Harness, CapturedAt: at, SourceBundle: active, History: &archive.RevisionHistory{CurrentRevision: b.NativeSessionID, Preserved: []archive.RevisionReference{{RevisionID: "22222222-2222-4222-8222-222222222222", Source: old, CapturedAt: recent}}}}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.SavePublication(b, at, active, raw); err != nil {
		t.Fatal(err)
	}
	preview, err := NewExpiryPreview(local, at.Add(100*24*time.Hour), func(archive.SessionRegistration) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		age     time.Duration
		expired bool
	}{{30 * 24 * time.Hour, false}, {20 * 24 * time.Hour, true}, {0, false}} {
		expired, err := preview.SessionExpired(reg, tc.age)
		if err != nil || expired != tc.expired {
			t.Fatalf("age %v expired %v error %v", tc.age, expired, err)
		}
	}
}
