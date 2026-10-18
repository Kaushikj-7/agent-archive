package collector

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/muse"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/evidence"
	"github.com/wangjohn/agent-archive/internal/state"
)

func TestSkillContentCollectorRestoresBeforeActivity(t *testing.T) {
	scan, reader, _ := nativeCollectorFixture(t)
	body := []byte("---\nname: declared-label\n---\nAn ordinary skill.\n")
	root := t.TempDir()
	dir := filepath.Join(root, "installed-origin")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), body, 0600); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join("..", "agents", "muse", "testdata", "synthetic-skill-read.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.ReplaceAll(raw, []byte("/work/synthetic-muse"), []byte(scan.reg.ProjectRoot))
	h := sha256.Sum256(body)
	raw = bytes.ReplaceAll(raw, []byte("sha256:"+strings.Repeat("a", 64)), []byte("sha256:"+hex.EncodeToString(h[:])))
	if !bytes.Contains(raw, []byte(hex.EncodeToString(h[:]))) {
		t.Fatal("native fixture hash precondition")
	}
	if err = os.WriteFile(reader.ref.Path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	scan.published, err = scan.local.LoadPublishedState(scan.id())
	if err != nil {
		t.Fatal(err)
	}
	calls, releases := 0, 0
	scan.opts.SupplementalEvidence = func(archive.SessionRegistration, time.Time) ([]archive.SupplementalEvidence, error) {
		t.Fatal("legacy callback doubled observation")
		return nil, nil
	}
	scan.opts.SupplementalSkillContent = func(ctx context.Context, reg archive.SessionRegistration, at time.Time, b *agentapi.NativeReadBudget) (agentapi.SkillContentObservation, func(), error) {
		calls++
		out, close, err := evidence.ObserveSkillsWithContent(ctx, evidence.SkillOptions{Harness: "muse", Mode: config.SkillEvidenceBody, ObservedAt: at, Locations: []agentapi.SkillRoot{{Path: root, Scope: "user_muse_config", UserBoundary: root}}}, b)
		return out, func() { releases++; close() }, err
	}
	read := func() sourceRead {
		t.Helper()
		mark := len(scan.retainedReleases)
		out, observed, err := reader.Filter(t.Context(), muse.Filter{}, 64<<20)
		if err != nil {
			t.Fatal(err)
		}
		if observed.skillContent == nil {
			t.Fatal("attested ordinary operation did not carry companion")
		}
		return sourceRead{adapter: muse.Filter{}, filtered: out, observed: observed, filterLease: scan.nativeFilterLease(mark)}
	}
	first := read()
	candidate, _, err := scan.buildRead(&first)
	if err != nil {
		t.Fatal(err)
	}
	analysis, err := (muse.Parser{}).Parse(t.Context(), candidate)
	if err != nil || len(analysis.View.NativeSkillUses) != 1 || analysis.View.NativeSkillUses[0].SHA256 != hex.EncodeToString(h[:]) || calls != 1 || releases != 1 {
		t.Fatal("actual collector active-gate join", err, calls, releases)
	}
	if err = scan.published.Save(candidate, scan.now, state.CacheStatusPublished); err != nil {
		t.Fatal(err)
	}
	if err = scan.closeNativeLeases(); err != nil {
		t.Fatal(err)
	}
	scan.releaseRetained()
	scan.published, err = scan.local.LoadPublishedState(scan.id())
	if err != nil {
		t.Fatal(err)
	}
	candidate, _, _, _ = scan.published.Cached()
	// A later skill edit alone cannot initialize an observer or change the
	// already-qualified native content fact during an unchanged source scan.
	if err = os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("different current content"), 0600); err != nil {
		t.Fatal(err)
	}
	second := read()
	unchanged, _, err := scan.buildRead(&second)
	if err != nil || calls != 1 || releases != 1 || !(muse.Filter{}).EvidenceExtends(candidate, unchanged) || !(muse.Filter{}).EvidenceExtends(unchanged, candidate) {
		t.Fatal("unchanged tick manufactured skill activity", err, calls, releases)
	}
	if !reflect.DeepEqual(candidate.NativeRecords, unchanged.NativeRecords) {
		t.Fatal("previous safe proof was not restored before activity")
	}
	if err = scan.closeNativeLeases(); err != nil {
		t.Fatal(err)
	}
	scan.releaseRetained()
	// The same immutable event with a contradictory private native digest must
	// refuse; a fresh observation cannot repair or replace the prior proof.
	raw = bytes.ReplaceAll(raw, []byte(hex.EncodeToString(h[:])), []byte(strings.Repeat("b", 64)))
	if err = os.WriteFile(reader.ref.Path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	third := read()
	if _, _, err = scan.buildRead(&third); err == nil || calls != 1 {
		t.Fatal("contradiction queried filesystem or accepted replacement")
	}
	prior, _, _, _ := scan.published.Cached()
	if !reflect.DeepEqual(prior.NativeRecords, candidate.NativeRecords) {
		t.Fatal("contradiction replaced prior publication")
	}
}

func TestSkillContentCallbackNoneAndImport(t *testing.T) {
	for _, mode := range []config.SkillEvidence{config.SkillEvidenceNone, config.SkillEvidenceMetadata} {
		scan, _, _ := nativeCollectorFixture(t)
		scan.opts.SkillEvidence = mode
		called := 0
		scan.opts.SupplementalSkillContent = func(context.Context, archive.SessionRegistration, time.Time, *agentapi.NativeReadBudget) (agentapi.SkillContentObservation, func(), error) {
			called++
			return agentapi.SkillContentObservation{}, func() {}, nil
		}
		if mode == config.SkillEvidenceMetadata {
			scan.reg.Origin = archive.SessionOriginImport
		}
		_, release, err := scan.observeSkillContent()
		if release != nil {
			release()
		}
		if err != nil || called != 0 {
			t.Fatal("none/import initialized content observation", err)
		}
	}
}

func TestSkillContentCollectorErrorAndCancelReleaseView(t *testing.T) {
	for _, cancelDuringObservation := range []bool{false, true} {
		scan, reader, _ := nativeCollectorFixture(t)
		var err error
		scan.published, err = scan.local.LoadPublishedState(scan.id())
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		scan.ctx = ctx
		released := 0
		refusal := errors.New("synthetic observation refusal")
		scan.opts.SupplementalSkillContent = func(context.Context, archive.SessionRegistration, time.Time, *agentapi.NativeReadBudget) (agentapi.SkillContentObservation, func(), error) {
			if cancelDuringObservation {
				cancel()
				return agentapi.SkillContentObservation{}, func() { released++ }, nil
			}
			return agentapi.SkillContentObservation{}, func() { released++ }, refusal
		}
		mark := len(scan.retainedReleases)
		out, observed, err := reader.Filter(t.Context(), muse.Filter{}, 64<<20)
		if err != nil {
			t.Fatal(err)
		}
		read := sourceRead{adapter: muse.Filter{}, filtered: out, observed: observed, filterLease: scan.nativeFilterLease(mark)}
		candidate, _, err := scan.buildRead(&read)
		want := refusal
		if cancelDuringObservation {
			want = context.Canceled
		}
		if !errors.Is(err, want) || released != 1 || len(candidate.NativeRecords) != 0 {
			t.Fatal("error/cancel retained a borrowed view or partial candidate", err, released)
		}
		cancel()
		if err = scan.closeNativeLeases(); err != nil {
			t.Fatal(err)
		}
		scan.releaseRetained()
		if used, _ := scan.readBudget().Charged(); used != 0 {
			t.Fatal("failed active gate leaked source/output loan")
		}
	}
}

func TestSkillContentCollectorPreservesHashlessPublishedPrefix(t *testing.T) {
	scan, reader, _ := nativeCollectorFixture(t)
	root := t.TempDir()
	body := []byte("---\nname: declared-label\n---\nAn ordinary later-available skill.\n")
	h := sha256.Sum256(body)
	raw, err := os.ReadFile(filepath.Join("..", "agents", "muse", "testdata", "synthetic-skill-read.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.ReplaceAll(raw, []byte("/work/synthetic-muse"), []byte(scan.reg.ProjectRoot))
	raw = bytes.ReplaceAll(raw, []byte("sha256:"+strings.Repeat("a", 64)), []byte("sha256:"+hex.EncodeToString(h[:])))
	if err = os.WriteFile(reader.ref.Path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	scan.published, err = scan.local.LoadPublishedState(scan.id())
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	scan.opts.SupplementalSkillContent = func(ctx context.Context, _ archive.SessionRegistration, at time.Time, b *agentapi.NativeReadBudget) (agentapi.SkillContentObservation, func(), error) {
		calls++
		return evidence.ObserveSkillsWithContent(ctx, evidence.SkillOptions{Harness: "muse", Mode: config.SkillEvidenceBody, ObservedAt: at, Locations: []agentapi.SkillRoot{{Path: root, Scope: "user_muse_config", UserBoundary: root}}}, b)
	}
	read := func() sourceRead {
		t.Helper()
		mark := len(scan.retainedReleases)
		out, observed, err := reader.Filter(t.Context(), muse.Filter{}, 64<<20)
		if err != nil {
			t.Fatal(err)
		}
		return sourceRead{adapter: muse.Filter{}, filtered: out, observed: observed, filterLease: scan.nativeFilterLease(mark)}
	}
	save := func(b archive.SourceBundle) archive.SourceBundle {
		t.Helper()
		if err := scan.published.Save(b, scan.now, state.CacheStatusPublished); err != nil {
			t.Fatal(err)
		}
		if err := scan.closeNativeLeases(); err != nil {
			t.Fatal(err)
		}
		scan.releaseRetained()
		var err error
		scan.published, err = scan.local.LoadPublishedState(scan.id())
		if err != nil {
			t.Fatal(err)
		}
		cached, _, _, _ := scan.published.Cached()
		return cached
	}
	first := read()
	baseline, _, err := scan.buildRead(&first)
	if err != nil {
		t.Fatal(err)
	}
	a, err := (muse.Parser{}).Parse(t.Context(), baseline)
	if err != nil || len(a.View.NativeSkillUses) != 1 || a.View.NativeSkillUses[0].SHA256 != "" || calls != 1 {
		t.Fatal("hashless durable baseline precondition", err, calls)
	}
	baseline = save(baseline)
	dir := filepath.Join(root, "installed-origin")
	if err = os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "SKILL.md"), body, 0600); err != nil {
		t.Fatal(err)
	}
	unchanged := read()
	quiet, _, err := scan.buildRead(&unchanged)
	if err != nil || calls != 1 || !reflect.DeepEqual(baseline.NativeRecords, quiet.NativeRecords) {
		t.Fatal("file appearance initialized unchanged tick", err, calls)
	}
	if err = scan.closeNativeLeases(); err != nil {
		t.Fatal(err)
	}
	scan.releaseRetained()

	// A real explicit request may observe the now-available file, but cannot
	// upgrade the earlier published event or trigger the rewrite guard.
	scan.req.Token = "synthetic-refresh"
	requested := read()
	refreshed, supplemental, err := scan.buildRead(&requested)
	if err != nil {
		t.Fatal(err)
	}
	_, blocked, err := scan.guard(t.Context(), requested, refreshed, supplemental)
	if err != nil || blocked || calls != 2 || !reflect.DeepEqual(baseline.NativeRecords, refreshed.NativeRecords) {
		t.Fatal("hashless baseline was retroactively enriched", err, blocked, calls)
	}
	baseline = save(refreshed)
	scan.req.Token = ""

	// Append a genuine assistant turn and a NEW read event in the same run.
	// Fixture mutation uses the existing exact native shapes and owned IDs.
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	decode := func(line []byte) map[string]any {
		t.Helper()
		var m map[string]any
		d := json.NewDecoder(bytes.NewReader(line))
		d.UseNumber()
		if err := d.Decode(&m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	turn := decode(lines[len(lines)-3])
	if turn["payload_type"] != "runtime.session" {
		t.Fatal("assistant template type")
	}
	tp := turn["payload"].(map[string]any)
	te := tp["event"].(map[string]any)
	if te["kind"] != "assistant_message_committed" {
		t.Fatal("assistant template kind")
	}
	turn["id"], turn["sequence"] = "cccccccc-cccc-4ccc-8ccc-000000000011", json.Number("11")
	tp["source_run_record_id"], tp["source_run_record_sequence"] = "dddddddd-dddd-4ddd-8ddd-000000000004", json.Number("4")
	te["message_id"], te["response_id"], te["text"] = "synthetic-new-message", "synthetic-new-response", "A genuine appended assistant turn."
	event := decode(lines[len(lines)-1])
	ep := event["payload"].(map[string]any)
	ee := ep["event"].(map[string]any)
	if ee["kind"] != "skill_read_observed" {
		t.Fatal("read template kind")
	}
	event["id"], event["sequence"] = "cccccccc-cccc-4ccc-8ccc-000000000012", json.Number("12")
	ep["source_run_record_id"], ep["source_run_record_sequence"] = "dddddddd-dddd-4ddd-8ddd-000000000005", json.Number("5")
	ee["observed_at_sequence"] = json.Number("5")
	for _, row := range []map[string]any{turn, event} {
		encoded, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		raw = append(raw, append(encoded, '\n')...)
	}
	if err = os.WriteFile(reader.ref.Path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	appended := read()
	candidate, supplemental, err := scan.buildRead(&appended)
	if err != nil {
		t.Fatal(err)
	}
	_, blocked, err = scan.guard(t.Context(), appended, candidate, supplemental)
	if err != nil || blocked || calls != 3 || !(muse.Filter{}).EvidenceExtends(baseline, candidate) {
		t.Fatal("genuine append was blocked by content proof", err, blocked, calls)
	}
	a, err = (muse.Parser{}).Parse(t.Context(), candidate)
	if err != nil || len(a.View.NativeSkillUses) != 2 || a.View.NativeSkillUses[0].SHA256 != "" || a.View.NativeSkillUses[1].SHA256 != hex.EncodeToString(h[:]) {
		t.Fatal("old/new skill events did not retain distinct proof authority", err)
	}
	if !reflect.DeepEqual(baseline.NativeRecords, candidate.NativeRecords[:len(baseline.NativeRecords)]) {
		t.Fatal("published prefix changed during genuine append")
	}
	if err = scan.closeNativeLeases(); err != nil {
		t.Fatal(err)
	}
	scan.releaseRetained()
	if used, _ := scan.readBudget().Charged(); used != 0 {
		t.Fatal("hashless prefix regression leaked loan")
	}
}
