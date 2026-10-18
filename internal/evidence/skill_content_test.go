package evidence

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

func TestSkillContentSameReadSafeSubset(t *testing.T) {
	t.Parallel()
	for _, mode := range []config.SkillEvidence{config.SkillEvidenceBody, config.SkillEvidenceMetadata, config.SkillEvidenceNone} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			ordinary := "---\nname: declared-label\n---\n" + strings.Repeat("ordinary content\n", 1500)
			for name, body := range map[string]string{"native-id": ordinary, "duplicate": ordinary, "secret-correct": "password=SyntheticCredential42", "secret-incorrect": "password=WrongSyntheticCredential42", "literal-marker": "password=[REDACTED]"} {
				dir := filepath.Join(root, name)
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			budget := agentapi.NewNativeReadBudget(1 << 20)
			options := SkillOptions{Harness: "muse", Mode: mode, ObservedAt: time.Unix(2000000000, 0), Locations: []agentapi.SkillRoot{{Path: root, Scope: "project_agents", ProjectBoundary: root}}}
			out, close, err := ObserveSkillsWithContent(t.Context(), options, budget)
			if err != nil {
				t.Fatal(err)
			}
			if mode == config.SkillEvidenceNone {
				if len(out.Evidence) != 0 || len(out.Digests) != 0 {
					t.Fatal("none observed content")
				}
			} else {
				want := []agentapi.SkillContentDigest{agentapi.SkillContentDigest(sha256.Sum256([]byte(ordinary)))}
				if !reflect.DeepEqual(out.Digests, want) {
					t.Fatal("safe subset confused raw, redacted, marker or truncated hashes")
				}
				for _, item := range out.Evidence {
					if item.Kind == archive.EvidenceKindSkillSnapshot && item.Payload["name"] == "declared-label" && len(item.Payload["snapshot"].(string)) > maxSnapshotBodyBytes {
						t.Fatal("body cap raised")
					}
				}
			}
			close()
			close()
			if used, _ := budget.Charged(); used != 0 {
				t.Fatal("safe companion pass loan leaked")
			}
		})
	}
}

func TestSkillContentCancellationAndBudgetDiscard(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "one"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "one", "SKILL.md"), []byte("ordinary content"), 0600); err != nil {
		t.Fatal(err)
	}
	options := SkillOptions{Harness: "muse", ObservedAt: time.Unix(2000000000, 0), Locations: []agentapi.SkillRoot{{Path: root, Scope: "user_agents", UserBoundary: root}}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, c := range []struct {
		ctx   context.Context
		limit int64
		want  error
	}{{ctx, 1 << 20, context.Canceled}, {t.Context(), 1, agentapi.ErrReadBudget}} {
		budget := agentapi.NewNativeReadBudget(c.limit)
		out, close, err := ObserveSkillsWithContent(c.ctx, options, budget)
		close()
		if !errors.Is(err, c.want) || len(out.Digests) != 0 {
			t.Fatal("partial content proof returned", err)
		}
		if used, _ := budget.Charged(); used != 0 {
			t.Fatal("failed observation leaked")
		}
	}
}
