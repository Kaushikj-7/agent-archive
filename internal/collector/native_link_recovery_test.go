package collector

import (
	"bytes"
	"encoding/json"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/codex"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestNativeLinkRequestFailureRetriesAfterRestart(t *testing.T) {
	local := newTestStore(t)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	home := t.TempDir()
	parent := nativeLinkRegistration("parent", "00000000-0000-0000-0000-000000000001", "", home, at)
	child := nativeLinkRegistration("child", "00000000-0000-0000-0000-000000000002", parent.NativeSessionID, home, at)
	for _, reg := range []archive.SessionRegistration{parent, child} {
		if err := local.SaveRegistration(reg); err != nil {
			t.Fatal(err)
		}
	}
	blocked := filepath.Join(local.Home(), "requests", "child.json")
	if err := os.Mkdir(blocked, 0700); err != nil {
		t.Fatal(err)
	}
	p := &pass{local: local, registrations: []archive.SessionRegistration{child, parent}, now: at}
	p.reconcileNativeLinks()
	if p.result.NativeLinkPendingReasons["relationship_state_retry"] == 0 {
		t.Fatal("request failure was not recorded")
	}
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	reopened, err := state.Open(local.Home())
	if err != nil {
		t.Fatal(err)
	}
	regs, err := reopened.LoadRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	restarted := &pass{local: reopened, registrations: regs, now: at.Add(time.Minute)}
	restarted.reconcileNativeLinks()
	if _, found, err := local.LoadRequest(child.ArchiveSessionID); err != nil || !found {
		t.Fatalf("link publication request lost across restart: found=%v err=%v", found, err)
	}
}

func TestNativeLateParentLinkRefiltersRetainedHistory(t *testing.T) {
	scan, p := privacyJournal(t)
	var originals []archive.SourceBundle
	for _, input := range p.History.Inputs {
		raw, err := scan.local.ReadPendingSource(scan.id(), state.PendingSource{Reference: input.Reference, Name: input.Reference.SHA256 + ".gz"})
		if err != nil {
			t.Fatal(err)
		}
		bundle, err := archive.ReadSourceBundle(bytes.NewReader(raw), archive.DecodeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		originals = append(originals, bundle)
	}
	if len(originals) < 3 {
		t.Fatal("control requires all three actual retained revision inputs")
	}
	for _, original := range originals {
		original.NativeChild = true
		original.ParentSessionID = ""
		original.NativeRecords[len(original.NativeRecords)-1] = map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "visible DB_PASSWORD=" + plantedSecret}}}}
		scan.reg.NativeChild = true
		scan.reg.ParentSessionID = "late-parent"
		out, err := refilterBundleWithBudget(t, t.Context(), scan.reg, codex.Filter{}, original, agentapi.NewNativeReadBudget(16<<20))
		if err != nil {
			t.Fatal("late link blocked retained privacy maintenance", err)
		}
		if !out.Capture.CapturedAt.Equal(original.Capture.CapturedAt) || out.History == nil || out.ParentSessionID != scan.reg.ParentSessionID || !out.NativeChild {
			t.Fatal("late link changed retained ownership or capture age")
		}
		encoded, err := json.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(encoded, []byte(plantedSecret)) || !bytes.Contains(encoded, []byte("visible")) || !reflect.DeepEqual(out.Ordinals, original.Ordinals) || !reflect.DeepEqual(out.History.Spans, original.History.Spans) {
			t.Fatal("retained privacy maintenance lost redaction or raw history boundaries")
		}
		conflicted := original
		conflicted.ParentSessionID = "different-parent"
		if _, err := refilterBundle(t, t.Context(), scan.reg, codex.Filter{}, conflicted); err == nil {
			t.Fatal("conflicting parent accepted")
		}
		unproven := original
		unproven.NativeChild = false
		if _, err := refilterBundle(t, t.Context(), scan.reg, codex.Filter{}, unproven); err == nil {
			t.Fatal("unproven historical child relaxed parent identity")
		}
	}
}

func TestNativeParentRepairDoesNotHideOwnEvidenceOrConflictingLinks(t *testing.T) {
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	old := archive.SourceBundle{NativeChild: true, Capture: archive.SourceCapture{CapturedAt: at, Gaps: []archive.CaptureGap{{Code: "native_parent_link_pending"}}}, NativeRecords: []map[string]any{{"type": "own-event"}}}
	linked := old
	linked.ParentSessionID = "parent"
	linked.Capture.Gaps = nil
	if same, err := bundleChangeIsLinkOnly(old, linked); err != nil || !same {
		t.Fatal("parent repair treated as own activity", err)
	}
	if same, err := bundleChangeIsLinkOnly(linked, old); err != nil || same {
		t.Fatal("resolved parent disappearance hidden", err)
	}
	changed := linked
	changed.NativeRecords = []map[string]any{{"type": "new-own-event"}}
	if same, err := bundleChangeIsLinkOnly(old, changed); err != nil || same {
		t.Fatal("own activity hidden", err)
	}
	conflict := linked
	conflict.ParentSessionID = "different-parent"
	if same, err := bundleChangeIsLinkOnly(linked, conflict); err != nil || same {
		t.Fatal("parent conflict hidden", err)
	}
	changed = linked
	changed.NativeChild = false
	if same, err := bundleChangeIsLinkOnly(old, changed); err != nil || same {
		t.Fatal("ownership change hidden", err)
	}
}

func TestNativeFrozenParentConflictPreservesJournal(t *testing.T) {
	scan, p := privacyJournal(t)
	scan.reg.NativeChild, scan.reg.ParentSessionID = true, "conflicting-parent"
	p.Bundle.NativeChild, p.Bundle.ParentSessionID = true, "original-parent"
	var metadata archive.Metadata
	if err := json.Unmarshal(p.MetadataBytes, &metadata); err != nil {
		t.Fatal(err)
	}
	before := snapshotMtimes(t, scan.local.Home())
	if err := scan.prepareHistoryInput(&p, &metadata, p.History.Inputs[0]); err == nil {
		t.Fatal("different known frozen native parent accepted")
	}
	if !reflect.DeepEqual(before, snapshotMtimes(t, scan.local.Home())) {
		t.Fatal("conflicting frozen parent changed journal or stages")
	}
}
