package collector

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
