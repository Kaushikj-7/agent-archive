package muse

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/evidence"
	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

func skillContentNative(t *testing.T, body string) []byte {
	t.Helper()
	h := sha256.Sum256([]byte(body))
	return nativeSkillReads(t, 1, func(m map[string]any) {
		object(object(m["payload"])["event"])["evidence_hash"] = "sha256:" + hex.EncodeToString(h[:])
	})
}

func skillContentOperation(t *testing.T, raw []byte, budget *agentapi.NativeReadBudget) (archive.FilteredTranscript, agentapi.SkillContentJoin, func()) {
	t.Helper()
	out, join, releaseObservation, err := (Filter{}).FilterSkillContentLeased(t.Context(), agentapi.NativeInput{File: memoryInput{bytes.NewReader(raw)}}, agentapi.FilterContext{}, budget)
	if err != nil {
		releaseObservation()
		t.Fatal(err)
	}
	t.Cleanup(releaseObservation)
	return out, join, releaseObservation
}

func TestSkillContentFullPipeline(t *testing.T) {
	t.Parallel()
	body := "---\nname: declared-label\n---\nAn ordinary independently named skill.\n"
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "installed-origin"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "installed-origin", "SKILL.md"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	budget := agentapi.NewNativeReadBudget(8 << 20)
	raw := skillContentNative(t, body)
	_, join, releaseObservation := skillContentOperation(t, raw, budget)
	if _, err := join.Restore(t.Context(), archive.SourceBundle{}); err != nil {
		t.Fatal(err)
	}
	observed, release, err := evidence.ObserveSkillsWithContent(t.Context(), evidence.SkillOptions{Harness: "muse", Mode: config.SkillEvidenceBody, ObservedAt: time.Unix(2000000000, 0), Locations: []agentapi.SkillRoot{{Path: root, Scope: "user_muse_config", UserBoundary: root}}}, budget)
	if err != nil {
		t.Fatal(err)
	}
	out, err := join.Finalize(t.Context(), observed.Digests)
	release()
	if err != nil {
		t.Fatal("safe content failed actual observer join", err)
	}
	golden.Check(t, filepath.Join("testdata", "synthetic-skill-content.golden.jsonl"), append(bytes.Join(out.Records, []byte("\n")), '\n'))
	wantHash := sha256.Sum256([]byte(body))
	want := []archive.SkillUse{{Name: "pr10-local", SHA256: hex.EncodeToString(wantHash[:]), Evidence: archive.SkillUseEvidenceNativeInvocation}}
	b := bundleFor(t, out)
	b.SupplementalEvidence = observed.Evidence
	for range 2 {
		a, err := (Parser{}).Parse(t.Context(), b)
		if err != nil || !reflect.DeepEqual(a.View.NativeSkillUses, want) {
			t.Fatal("safe content lost native identity", err)
		}
		m, err := archive.BuildMetadataWithAnalysis(b, a, nil, "synthetic", time.Unix(2000000000, 0), time.Unix(2000000100, 0), archive.SourceReference{Key: "sessions/muse/synthetic/source.jsonl.gz", SHA256: strings.Repeat("b", 64), CompressedBytes: 1}, archive.ParserInfo{Name: "muse", Version: (Parser{}).Version()})
		if err != nil || !reflect.DeepEqual(m.SkillsUsed, want) || len(m.SkillsAvailable) != 1 || m.SkillsAvailable[0].Name != "declared-label" || m.SkillsAvailable[0].Coverage != archive.SkillCoverageInstalledOnly {
			t.Fatal("content join invented an inventory alias or eligibility", err)
		}
		next, err := (Filter{}).Refilter(t.Context(), b, time.Time{})
		if err != nil || !reflect.DeepEqual(out.Records, next.Records) {
			t.Fatal("safe content refilter drift", err)
		}
		b = bundleFor(t, next)
		b.SupplementalEvidence = observed.Evidence
	}
	if bytes.Contains(bytes.Join(out.Records, nil), []byte("evidence_hash")) {
		t.Fatal("native original-hash field retained")
	}
	releaseObservation()
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("pipeline content loan leaked")
	}
}

func TestSkillContentSecretGuessesAndMarkerStayHashless(t *testing.T) {
	t.Parallel()
	for _, body := range []string{"password=SyntheticCredential42", "password=WrongSyntheticCredential42", "password=[REDACTED]", "ordinary prose [REDACTED]"} {
		t.Run(body, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			dir := filepath.Join(root, "one")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			out, release, err := evidence.ObserveSkillsWithContent(t.Context(), evidence.SkillOptions{Harness: "muse", ObservedAt: time.Unix(2000000000, 0), Locations: []agentapi.SkillRoot{{Path: root, Scope: "project_agents", ProjectBoundary: root}}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			if len(out.Digests) != 0 {
				t.Fatal("private original digest comparison admitted")
			}
			_, join, _ := skillContentOperation(t, skillContentNative(t, "password=SyntheticCredential42"), nil)
			if _, err = join.Restore(t.Context(), archive.SourceBundle{}); err != nil {
				t.Fatal(err)
			}
			filtered, err := join.Finalize(t.Context(), out.Digests)
			if err != nil {
				t.Fatal(err)
			}
			a, err := (Parser{}).Parse(t.Context(), bundleFor(t, filtered))
			if err != nil || len(a.View.NativeSkillUses) != 1 || a.View.NativeSkillUses[0].SHA256 != "" {
				t.Fatal("secret equality oracle exposed", err)
			}
		})
	}
}

func TestSkillContentRestoreAndContradiction(t *testing.T) {
	t.Parallel()
	body := "ordinary content\n"
	h := agentapi.SkillContentDigest(sha256.Sum256([]byte(body)))
	_, join, _ := skillContentOperation(t, skillContentNative(t, body), nil)
	if _, err := join.Restore(t.Context(), archive.SourceBundle{}); err != nil {
		t.Fatal(err)
	}
	out, err := join.Finalize(t.Context(), []agentapi.SkillContentDigest{h, h})
	if err != nil {
		t.Fatal(err)
	}
	prior := bundleFor(t, out)
	_, again, _ := skillContentOperation(t, skillContentNative(t, body), nil)
	restored, err := again.Restore(t.Context(), prior)
	if err != nil || !reflect.DeepEqual(out.Records, restored.Records) {
		t.Fatal("unchanged tick lost prior proof", err)
	}
	final, err := again.Finalize(t.Context(), nil)
	if err != nil || !(Filter{}).EvidenceExtends(prior, bundleFor(t, final)) {
		t.Fatal("missing current file changed prior proof", err)
	}
	_, changed, _ := skillContentOperation(t, skillContentNative(t, "different content\n"), nil)
	if _, err := changed.Restore(t.Context(), prior); err == nil {
		t.Fatal("same-event native hash contradiction accepted")
	}
	if len(changed.(*skillContentJoin).pending) != 0 {
		t.Fatal("contradiction retained private hash")
	}
}

func TestSkillContentCancelBudgetAndAttestation(t *testing.T) {
	t.Parallel()
	for _, cancelNow := range []bool{false, true} {
		budget := agentapi.NewNativeReadBudget(8 << 20)
		_, join, releaseObservation := skillContentOperation(t, skillContentNative(t, "ordinary"), budget)
		restored, err := join.Restore(t.Context(), archive.SourceBundle{})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		block := budget.Reservation()
		n := int64(0)
		if cancelNow {
			cancel()
		} else {
			n = budget.Available()
			if !block.Reserve(n) {
				t.Fatal("test pressure setup")
			}
		}
		h := agentapi.SkillContentDigest(sha256.Sum256([]byte("ordinary")))
		out, err := join.Finalize(ctx, []agentapi.SkillContentDigest{h})
		cancel()
		block.Release(n)
		if cancelNow {
			if !errors.Is(err, context.Canceled) {
				t.Fatal("cancellation downgraded", err)
			}
		} else if err != nil || !reflect.DeepEqual(out, restored) || budget.ScopeExceeded() || budget.ScopePressured() {
			t.Fatal("optional capacity changed restored output", err)
		}
		if len(join.(*skillContentJoin).pending) != 0 {
			t.Fatal("terminal finalization kept private state")
		}
		releaseObservation()
		releaseObservation()
		if used, _ := budget.Charged(); used != 0 {
			t.Fatal("operation lease leaked")
		}
		if _, err := join.Finalize(t.Context(), nil); !errors.Is(err, agentapi.ErrClosed) {
			t.Fatal("closed operation reused")
		}
	}
	type decorated struct{ Filter }
	if (Filter{}).SkillContentJoinFor(decorated{}) {
		t.Fatal("decorator bypassed")
	}
	legacy, err := filterBytes(t, skillContentNative(t, "ordinary"), agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	a, err := (Parser{}).Parse(t.Context(), bundleFor(t, legacy))
	if err != nil || a.View.NativeSkillUses[0].SHA256 != "" {
		t.Fatal("ordinary filter gained observer authority")
	}
}

func TestSkillContentClosedRetainedAndNativeSeparation(t *testing.T) {
	t.Parallel()
	h := agentapi.SkillContentDigest(sha256.Sum256([]byte("ordinary")))
	_, join, _ := skillContentOperation(t, skillContentNative(t, "ordinary"), nil)
	if _, err := join.Restore(t.Context(), archive.SourceBundle{}); err != nil {
		t.Fatal(err)
	}
	out, err := join.Finalize(t.Context(), []agentapi.SkillContentDigest{h})
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(map[string]any){
		func(f map[string]any) { delete(f, "hash_evidence") },
		func(f map[string]any) { delete(f, "sha256") },
		func(f map[string]any) { f["hash_evidence"] = "future" },
		func(f map[string]any) { f["sha256"] = strings.Repeat("A", 64) },
		func(f map[string]any) { f["sha256"] = nil },
		func(f map[string]any) { f["extra"] = true },
	} {
		b := bundleFor(t, out)
		change(object(b.NativeRecords[len(b.NativeRecords)-1]["fields"]))
		if _, err := (Parser{}).Parse(t.Context(), b); err == nil {
			t.Fatal("unsupported safe producer shape parsed")
		}
		if _, err := (Filter{}).Refilter(t.Context(), b, time.Time{}); err == nil {
			t.Fatal("unsupported safe producer shape refiltered")
		}
	}
	raw := nativeSkillReads(t, 1, func(m map[string]any) { object(object(m["payload"])["event"])["hash_evidence"] = skillContentEvidence })
	if _, err := filterBytes(t, raw, agentapi.ReadLimits{}); err == nil {
		t.Fatal("native input asserted safe producer marker")
	}
	// Deliberately pin the adopted producer trust boundary. A coherent retained
	// canonical replacement is not authenticated by a marker or compressed SHA.
	b := bundleFor(t, out)
	object(b.NativeRecords[len(b.NativeRecords)-1]["fields"])["sha256"] = strings.Repeat("b", 64)
	if _, err := (Parser{}).Parse(t.Context(), b); err != nil {
		t.Fatal("producer trust was silently replaced by invented authentication", err)
	}
}

func TestSkillContentOldEightFieldReaderRefusesHashVariant(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("testdata", "filter31-skill-fields-before-content.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	id := doc.(map[string]any)["$id"].(string)
	if err = c.AddResource(id, doc); err != nil {
		t.Fatal(err)
	}
	schema, err := c.Compile(id)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := filterBytes(t, skillContentNative(t, "ordinary"), agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	decode := func(raw []byte) map[string]any {
		t.Helper()
		var r map[string]any
		if err := json.Unmarshal(raw, &r); err != nil {
			t.Fatal(err)
		}
		return object(r["fields"])
	}
	if err = schema.Validate(decode(legacy.Records[len(legacy.Records)-1])); err != nil {
		t.Fatal("old branch lost legacy support", err)
	}
	_, join, _ := skillContentOperation(t, skillContentNative(t, "ordinary"), nil)
	if _, err = join.Restore(t.Context(), archive.SourceBundle{}); err != nil {
		t.Fatal(err)
	}
	h := agentapi.SkillContentDigest(sha256.Sum256([]byte("ordinary")))
	out, err := join.Finalize(t.Context(), []agentapi.SkillContentDigest{h})
	if err != nil {
		t.Fatal(err)
	}
	if schema.Validate(decode(out.Records[len(out.Records)-1])) == nil {
		t.Fatal("old exact eight-field reader accepted new safe hash")
	}
}

// Exercise the outer fieldShape -> validFields gate as well as the event's
// own validator: each independently enforces the same exact closed variant.
func TestSkillContentDispatchUsesExactVariant(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		content bool
		marker  bool
		extra   bool
		want    bool
	}{
		{name: "old8", want: true},
		{name: "safe10", content: true, marker: true, want: true},
		{name: "hash_without_marker", content: true},
		{name: "marker_without_hash", marker: true},
		{name: "extra_old", extra: true},
		{name: "extra_safe", content: true, marker: true, extra: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := map[string]any{"kind": "run", "event_kind": "skill_read_observed", "run_id": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "source_run_record_id": "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "source_run_record_sequence": json.Number("1"), "skill_id": "installed-origin", "evidence_kind": "read_skill_tool", "observed_at_sequence": json.Number("1")}
			if tc.content {
				f["sha256"] = strings.Repeat("a", 64)
			}
			if tc.marker {
				f["hash_evidence"] = skillContentEvidence
			}
			if tc.extra {
				f["extra"] = "future"
			}
			r := retained{Type: payloadRuntimeSession, PayloadSchema: 1, Fields: f}
			if got := validFields(r); got != tc.want {
				t.Fatalf("outer exact-variant dispatch = %v, want %v", got, tc.want)
			}
			if got := skillReadFields(r); got != tc.want {
				t.Fatalf("event exact-variant predicate = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSkillContentOptionalFinalizePressure(t *testing.T) {
	t.Parallel()
	for _, room := range []int64{0, 4096} {
		t.Run(fmt.Sprint(room), func(t *testing.T) {
			t.Parallel()
			budget := agentapi.NewNativeReadBudget(8 << 20)
			end := budget.BeginScope(8 << 20)
			defer end()
			_, join, release := skillContentOperation(t, skillContentNative(t, "ordinary"), budget)
			restored, err := join.Restore(t.Context(), archive.SourceBundle{})
			if err != nil {
				t.Fatal(err)
			}
			blocker := budget.Reservation()
			n := blocker.Available() - room
			if n < 0 || !blocker.Reserve(n) {
				t.Fatal("pressure setup")
			}
			h := agentapi.SkillContentDigest(sha256.Sum256([]byte("ordinary")))
			out, err := join.Finalize(t.Context(), []agentapi.SkillContentDigest{h})
			if err != nil || !reflect.DeepEqual(out, restored) || len(join.(*skillContentJoin).pending) != 0 {
				t.Fatal("optional proof partially changed restored output", err)
			}
			if budget.ScopeExceeded() || budget.ScopePressured() {
				t.Fatal("optional finalization set refusal flags")
			}
			parsed, err := (Parser{}).Parse(t.Context(), bundleFor(t, out))
			if err != nil || len(parsed.View.NativeSkillUses) != 1 || parsed.View.NativeSkillUses[0].SHA256 != "" {
				t.Fatal("capacity fallback lost positive hashless invocation", err)
			}
			blocker.Release(n)
			release()
			if used, _ := budget.Charged(); used != 0 {
				t.Fatal("optional finalization charge leaked")
			}
		})
	}
}

func TestSkillContentRequiredRestoreAndOutputRemainStrict(t *testing.T) {
	t.Parallel()
	h := agentapi.SkillContentDigest(sha256.Sum256([]byte("ordinary")))
	_, initial, _ := skillContentOperation(t, skillContentNative(t, "ordinary"), nil)
	if _, err := initial.Restore(t.Context(), archive.SourceBundle{}); err != nil {
		t.Fatal(err)
	}
	enriched, err := initial.Finalize(t.Context(), []agentapi.SkillContentDigest{h})
	if err != nil {
		t.Fatal(err)
	}
	prior := bundleFor(t, enriched)
	budget := agentapi.NewNativeReadBudget(8 << 20)
	_, required, release := skillContentOperation(t, skillContentNative(t, "ordinary"), budget)
	blocker := budget.Reservation()
	n := blocker.Available()
	if !blocker.Reserve(n) {
		t.Fatal("required pressure setup")
	}
	if _, err := required.Restore(t.Context(), prior); !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatal("required published proof silently dropped", err)
	}
	blocker.Release(n)
	release()
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("required refusal leaked")
	}
	_, limited, _ := skillContentOperation(t, skillContentNative(t, "ordinary"), nil)
	restored, err := limited.Restore(t.Context(), archive.SourceBundle{})
	if err != nil {
		t.Fatal(err)
	}
	limited.(*skillContentJoin).limits.FilteredBytes = int64(restored.Boundary.RetainedBytes)
	if _, err := limited.Finalize(t.Context(), []agentapi.SkillContentDigest{h}); !errors.Is(err, archive.ErrRecordTooLarge) {
		t.Fatal("output ceiling downgraded to hashless success", err)
	}
}

func TestSkillContentPressurePreservesRestoredProof(t *testing.T) {
	t.Parallel()
	h := agentapi.SkillContentDigest(sha256.Sum256([]byte("ordinary")))
	_, first, _ := skillContentOperation(t, skillContentNative(t, "ordinary"), nil)
	if _, err := first.Restore(t.Context(), archive.SourceBundle{}); err != nil {
		t.Fatal(err)
	}
	old, err := first.Finalize(t.Context(), []agentapi.SkillContentDigest{h})
	if err != nil {
		t.Fatal(err)
	}
	prior := bundleFor(t, old)
	raw := nativeSkillReads(t, 2, func(m map[string]any) {
		object(object(m["payload"])["event"])["evidence_hash"] = "sha256:" + hex.EncodeToString(h[:])
	})
	budget := agentapi.NewNativeReadBudget(8 << 20)
	_, join, release := skillContentOperation(t, raw, budget)
	restored, err := join.Restore(t.Context(), prior)
	if err != nil {
		t.Fatal(err)
	}
	blocker := budget.Reservation()
	n := blocker.Available()
	if !blocker.Reserve(n) {
		t.Fatal("pressure setup")
	}
	out, err := join.Finalize(t.Context(), []agentapi.SkillContentDigest{h})
	if err != nil || !reflect.DeepEqual(out, restored) || !(Filter{}).EvidenceExtends(prior, bundleFor(t, out)) {
		t.Fatal("optional pressure changed published prefix", err)
	}
	parsed, err := (Parser{}).Parse(t.Context(), bundleFor(t, out))
	if err != nil || len(parsed.View.NativeSkillUses) != 2 || parsed.View.NativeSkillUses[0].SHA256 == "" || parsed.View.NativeSkillUses[1].SHA256 != "" {
		t.Fatal("restored proof lost or new proof partially added", err)
	}
	blocker.Release(n)
	release()
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("restored fallback leaked")
	}
}
