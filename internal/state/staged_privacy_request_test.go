package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/agents/retainedprivacy"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

func TestStagedPrivacyRequestUsesActualImmutableStageWithoutChangingOriginalAuthority(t *testing.T) {
	s, pending, _ := stagedPreparedFixture(t, true, []map[string]any{
		{"type": "session_meta", "payload": map[string]any{"id": "native-owned"}},
		{"type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "synthetic retained prompt"}}}},
	})
	id := pending.Bundle.ArchiveSessionID
	before := stagedAuthorityBytes(t, s, id)
	reg, _, err := s.LoadRegistration(id)
	if err != nil {
		t.Fatal(err)
	}
	stage, _, err := s.LoadImportStage(t.Context(), reg)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, err := config.Load(s.Home())
	if err != nil {
		t.Fatal(err)
	}
	cfg.SkillEvidence = config.SkillEvidenceNone
	if err := config.Save(s.Home(), cfg); err != nil {
		t.Fatal(err)
	}
	budget := agentapi.NewNativeReadBudget(32 << 20)
	defer budget.BeginScope(16 << 20)()
	read, closeRead := s.WithReadBudget(t.Context(), budget)
	defer closeRead()
	_, filter, _ := builtin.NewBuiltins().LookupSources("codex")
	request, err := read.FreezeStagedPrivacyRequest(t.Context(), id, filter.Version())
	if err != nil {
		t.Fatal(err)
	}
	defer request.Close()
	proof, err := builtin.NewBuiltins().TransformRetainedPrivacy(request)
	if err != nil {
		t.Fatal(err)
	}
	defer proof.Close()
	err = proof.WithVerifiedTransform(t.Context(), request, budget, func(_ context.Context, output archive.SourceBundle, _ archive.CompressedSource, facts retainedprivacy.Facts) error {
		if facts.StageSHA256 != stage.SHA256 || facts.OwnerSHA256 == "" || facts.CoveredToken != pending.RequestToken || !facts.HadRequest || facts.OriginalPolicy.SkillEvidence != string(stage.SkillEvidence) || facts.TargetPolicy.SkillEvidence != "none" || output.Capture.FilterVersion != archive.FilterVersion || !output.Capture.CapturedAt.Equal(stage.Bundle.Capture.CapturedAt) {
			t.Fatal("stage/request/policy/capture facts changed", facts)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	assertStagedAuthorityBytes(t, s, id, before)
}

type stagedPrivacyFault string

const (
	stagedPrivacyMissing   stagedPrivacyFault = "missing"
	stagedPrivacyAltered   stagedPrivacyFault = "altered"
	stagedPrivacyCancelled stagedPrivacyFault = "cancelled"
	stagedPrivacyUnscoped  stagedPrivacyFault = "unscoped"
	stagedPrivacyNewer     stagedPrivacyFault = "newer-request"
)

func TestStagedPrivacyRequestRefusesMissingOrAlteredStageAndOriginalContextCancellation(t *testing.T) {
	for _, mode := range []stagedPrivacyFault{stagedPrivacyMissing, stagedPrivacyAltered, stagedPrivacyCancelled, stagedPrivacyUnscoped, stagedPrivacyNewer} {
		t.Run(string(mode), func(t *testing.T) {
			s, pending, _ := stagedPreparedFixture(t, true)
			id := pending.Bundle.ArchiveSessionID
			path := filepath.Join(s.Home(), importStageRelative(id))
			switch mode {
			case stagedPrivacyNewer:
				if err := s.SaveRequest(id, "newer", time.Now()); err != nil {
					t.Fatal(err)
				}
			case stagedPrivacyMissing:
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case stagedPrivacyAltered:
				if err := os.WriteFile(path, []byte(`{"version":2,"typed_stage":true}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			before := stagedAuthorityBytes(t, s, id)
			budget := agentapi.NewNativeReadBudget(32 << 20)
			if mode != stagedPrivacyUnscoped {
				defer budget.BeginScope(16 << 20)()
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			read, closeRead := s.WithReadBudget(ctx, budget)
			defer closeRead()
			if mode == stagedPrivacyCancelled {
				cancel()
			}
			_, filter, _ := builtin.NewBuiltins().LookupSources("codex")
			request, err := read.FreezeStagedPrivacyRequest(context.Background(), id, filter.Version())
			if err == nil || request != nil {
				t.Fatal("invalid stage/lifetime minted a request", err)
			}
			if mode == stagedPrivacyCancelled && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			assertStagedAuthorityBytes(t, s, id, before)
		})
	}
}
