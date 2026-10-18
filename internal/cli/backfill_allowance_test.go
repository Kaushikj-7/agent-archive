package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
)

// Simulate a classified native read outcome at the existing provider boundary;
// discovery, preparation, plan decisions and command rendering remain real.
type backfillRefusalProvider struct {
	agentapi.SourceProvider
	err error
}

func (p backfillRefusalProvider) OpenPass(ctx context.Context, env agentapi.SourceEnvironment) (agentapi.SourcePass, error) {
	pass, err := p.SourceProvider.OpenPass(ctx, env)
	if err != nil {
		return nil, err
	}
	return backfillRefusalPass{pass, p.err}, nil
}

type backfillRefusalPass struct {
	agentapi.SourcePass
	err error
}

func (p backfillRefusalPass) Read(ctx context.Context, ref agentapi.SourceRef, limits agentapi.ReadLimits) (agentapi.SourceSnapshot, error) {
	if p.err != nil && strings.HasPrefix(filepath.Base(ref.Path), "rollout-11111111-") {
		return nil, fmt.Errorf("synthetic private locator: %w", errors.Join(p.err, archive.ErrRelatedHistory))
	}
	return p.SourcePass.Read(ctx, ref, limits)
}

func TestBackfillDryRunDistinguishesLookupAllowanceFromSourceRefusals(t *testing.T) {
	private := errors.New("synthetic private native ID and transcript text")
	for _, tc := range []struct {
		name   string
		err    error
		detail backfill.DiagnosticDetail
		action backfill.DiagnosticAction
		text   string
	}{
		{name: "small complete history"},
		{"spent lookup allowance", agentapi.WithCause(agentapi.Wrap(agentapi.Limit, private), agentapi.CauseLookupAllowanceSpent), backfill.DetailLookupAllowanceSpent, backfill.ActionRetry, "This run's allowance for finding Codex rollouts"},
		{"native inventory no progress", agentapi.WithCause(agentapi.Wrap(agentapi.Unavailable, private), agentapi.CauseLookupNoProgress), backfill.DetailLookupNoProgress, backfill.ActionRetry, "Native inventory made no progress"},
		{"native I/O timeout", agentapi.WithCause(agentapi.Wrap(agentapi.Unavailable, private), agentapi.CauseLookupIOTimeout), backfill.DetailLookupIOTimeout, backfill.ActionRetry, "A native lookup I/O operation timed out"},
		{"source bytes", agentapi.RawLimit(65, 64, nil), backfill.DetailSourceSizeLimit, backfill.ActionAwaitSupport, "source exceeds the supported size limit"},
		{"source record", agentapi.Wrap(agentapi.Limit, archive.ErrRecordTooLarge), backfill.DetailSourceRecordLimit, backfill.ActionAwaitSupport, "source record exceeds the supported size limit"},
		{"history graph", agentapi.WithCause(agentapi.Wrap(agentapi.Limit, private), agentapi.CauseRolloutHistoryDepthLimit), backfill.DetailRolloutHistoryDepthLimit, backfill.ActionAwaitSupport, "Rollout history depth limit"},
		{"missing evidence", agentapi.WithCause(agentapi.Wrap(agentapi.Unavailable, private), agentapi.CauseHistoryBaseUnavailable), backfill.DetailHistoryBaseUnavailable, backfill.ActionRetry, "A Codex rollout this thread continues from was not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, bucket := newImportFixture(t)
			const parent = "22222222-2222-4222-8222-222222222222"
			for i := range 3 {
				id := fmt.Sprintf("11111111-1111-4111-8111-%012d", i)
				f.nativeIDs = append(f.nativeIDs, id)
				header, err := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{
					"id": id, "session_id": parent, "parent_thread_id": parent,
					"cwd": filepath.Join(f.userHome, "agent-archive"), "timestamp": "2026-09-20T17:00:00Z",
					"cli_version": "dev", "originator": "codex_cli_rs", "subagent_history_start_ordinal": 1,
					"source": map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": parent, "depth": 1}}},
				}})
				must(t, err)
				body := fmt.Sprintf("\n"+`{"type":"event_msg","payload":{"type":"task_started","turn_id":%q,"started_at":"2026-09-20T17:00:00Z"}}`+"\n"+`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Synthetic own prompt"}]}}`+"\n", id)
				f.write(t, filepath.Join(".codex", "sessions", "rollout-"+id+".jsonl"), string(header)+body)
			}
			var bindings []builtin.Integration
			for _, d := range productionAgents.Catalog().All() {
				binding, ok := productionAgents.Lookup(string(d.ID))
				if !ok {
					t.Fatal("missing built-in binding")
				}
				binding.Descriptor = agentmeta.Descriptor{ID: d.ID}
				if d.ID == agentmeta.Codex {
					binding.Sources = backfillRefusalProvider{binding.Sources, tc.err}
				}
				bindings = append(bindings, binding)
			}
			var err error
			f.env.Agents, err = builtin.New(agentmeta.Builtins(), bindings)
			must(t, err)
			f.env = withTrace(f.env, "1")
			before := snapshotAll(t, f, bucket)
			for _, structured := range []bool{false, true} {
				args := []string{"--dry-run", "--harness", "codex"}
				if structured {
					args = append(args, "--json")
				}
				out, diagnostics, code := f.importRun(t, nil, false, args...)
				if code != 0 {
					t.Fatal(code, diagnostics)
				}
				before.check(t, f, bucket)
				f.checkPrivate(t, "plan", out)
				f.checkPrivate(t, "trace", diagnostics)
				if strings.Contains(diagnostics, f.root) {
					t.Fatal("trace exposed source path")
				}
				for _, output := range []string{out, diagnostics} {
					for _, secret := range []string{parent, "Synthetic own prompt", private.Error(), "synthetic private locator"} {
						if strings.Contains(output, secret) {
							t.Fatalf("output leaked %q", secret)
						}
					}
				}
				for _, want := range []string{"backfill native preparation", "codex_lookup_scopes 1", "backfill_native_preparation_attempts 3", "backfill_native_inspection_attempts 3"} {
					if !strings.Contains(diagnostics, want) {
						t.Errorf("trace lacks %q: %s", want, diagnostics)
					}
				}
				if tc.detail == backfill.DetailLookupAllowanceSpent {
					if !strings.Contains(diagnostics, "backfill_lookup_unfinished_candidates 3") {
						t.Fatalf("unfinished candidates not counted: %s", diagnostics)
					}
				} else if strings.Contains(diagnostics, "backfill_lookup_unfinished_candidates") {
					t.Fatalf("source refusal counted as scheduling exhaustion: %s", diagnostics)
				}
				if structured {
					var plan struct {
						Projects []struct {
							Sessions map[string]int `json:"sessions"`
						} `json:"projects"`
						Diagnostics []backfill.DiagnosticSummary `json:"diagnostics"`
					}
					must(t, json.Unmarshal([]byte(out), &plan))
					selected := 0
					for _, project := range plan.Projects {
						selected += project.Sessions["codex"]
					}
					wantSelected := 1 // The ordinary source is unaffected.
					if tc.err == nil {
						wantSelected = 4
					}
					if selected != wantSelected {
						t.Fatalf("selected %d, want %d: %s", selected, wantSelected, out)
					}
					if tc.err != nil && (len(plan.Diagnostics) != 1 || plan.Diagnostics[0].Detail != tc.detail || plan.Diagnostics[0].Action != tc.action || plan.Diagnostics[0].Skip != backfill.SkipRelatedHistory || plan.Diagnostics[0].Count != 3) {
						t.Fatalf("lost native classification: %s", out)
					}
					if tc.err == nil && len(plan.Diagnostics) != 0 {
						t.Fatalf("complete histories refused: %s", out)
					}
				} else if tc.err != nil && !strings.Contains(out, tc.text) {
					t.Fatalf("missing human diagnostic: %s", out)
				}
			}
		})
	}
}
