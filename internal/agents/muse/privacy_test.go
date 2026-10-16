package muse

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestPrivacyChangesReachHandoffAndSurviveNativeLoss(t *testing.T) {
	t.Parallel()
	raw := readSynthetic(t, "synthetic-exec-conversation.jsonl")
	raw = bytes.Replace(raw, []byte("Explain the synthetic parser."), []byte("<system-reminder>HIDDEN_SENTINEL</system-reminder>API key: sk-proj-abcdefghijklmnopqrstuvwxyz1234567890"), 1)
	raw = bytes.Replace(raw, []byte("It validates bounded native records and keeps own visible conversation."), []byte(strings.Repeat("x", 70000)+"VISIBLE_TAIL"), 1)
	out, err := filterBytes(t, raw, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	retained := bytes.Join(out.Records, nil)
	for _, sentinel := range []string{"HIDDEN_SENTINEL", "abcdefghijklmnopqrstuvwxyz1234567890", "VISIBLE_TAIL"} {
		if bytes.Contains(retained, []byte(sentinel)) {
			t.Fatalf("unsafe or unbounded text retained: %s", sentinel)
		}
	}
	want := map[string]int{"content_truncated": 8, "hidden_instruction_omitted": 6, "sensitive_content_redacted": 6}
	for code, record := range want {
		found := false
		for _, gap := range out.Gaps {
			if gap.Code == code {
				found = gap.Record == record && gap.Detail == privacyGapDetail(code)
			}
		}
		if !found {
			t.Fatalf("missing positioned provenance %s: %+v", code, out.Gaps)
		}
	}
	b := bundleFor(t, out) // The native source has already been removed.
	a, err := (Parser{}).Parse(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	h, err := archive.BuildHandoffWithAnalysis(b, a, nil, archive.HandoffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	for code := range want {
		if !bytes.Contains(encoded, []byte(code)) {
			t.Fatalf("handoff omitted %s", code)
		}
	}
	for range 2 {
		next, err := (Filter{}).Refilter(t.Context(), b, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(next.Records, out.Records) || !reflect.DeepEqual(next.Gaps, out.Gaps) {
			t.Fatal("native-loss refilter lost or duplicated privacy provenance")
		}
		b = bundleFor(t, next)
	}
}

func TestRetainedPrivacyRefreshCanonicalizesOnlyKnownProvenance(t *testing.T) {
	t.Parallel()
	out, err := filterBytes(t, readSynthetic(t, "synthetic-exec-conversation.jsonl"), agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	b := bundleFor(t, out)
	for _, text := range []string{
		strings.Repeat("x", 70000) + "VISIBLE_TAIL",
		"API key: sk-proj-abcdefghijklmnopqrstuvwxyz1234567890",
		"<system-reminder>HIDDEN_SENTINEL</system-reminder>Visible text",
	} {
		b.NativeRecords[7]["fields"].(map[string]any)["text"] = text
		if _, err := (Parser{}).Parse(t.Context(), b); err == nil {
			t.Fatal("parser silently changed unrefreshed retained evidence")
		}
	}
	b.NativeRecords[7]["fields"].(map[string]any)["text"] = strings.Repeat("x", 70000) + "VISIBLE_TAIL"
	b.Capture.Gaps = []archive.CaptureGap{
		{Code: "content_truncated", Record: 8, Detail: "PRIVATE_UNTRUSTED_DETAIL"},
		{Code: "content_truncated", Record: 9, Detail: "duplicate"},
		{Code: "sensitive_content_redacted", Record: 0, Detail: "bad position"},
		{Code: "hidden_instruction_omitted", Record: 1000, Detail: "outside evidence"},
		{Code: "PRIVATE_UNTRUSTED_CODE", Record: 1, Detail: "private"},
	}
	next, err := (Filter{}).Refilter(t.Context(), b, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, gap := range next.Gaps {
		if gap.Code == "content_truncated" {
			found++
			if gap.Record != 8 || gap.Detail != "content truncated" {
				t.Fatalf("uncanonical provenance: %+v", gap)
			}
		}
		if strings.Contains(gap.Code, "PRIVATE") || strings.Contains(gap.Detail, "PRIVATE") || gap.Code == "hidden_instruction_omitted" || gap.Code == "sensitive_content_redacted" {
			t.Fatalf("untrusted provenance retained: %+v", gap)
		}
	}
	if found != 1 {
		t.Fatalf("gap count %d", found)
	}
	if _, err := (Parser{}).Parse(t.Context(), bundleFor(t, next)); err != nil {
		t.Fatalf("refreshed evidence refused: %v", err)
	}
}
